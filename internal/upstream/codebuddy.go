package upstream

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/singleflight"

	"foxrouters/internal/db"
)

// ===========================================================================
// CODEBUDDY KEY MANAGER
// ===========================================================================

// CBCredType distinguishes API-key credentials from OAuth access tokens.
// Both hit the same chat endpoint; only the Authorization header and
// refresh lifecycle differ.
type CBCredType string

const (
	CBAuthAPIKey CBCredType = "api_key"
	CBAuthOAuth  CBCredType = "oauth"
)

type CBKey struct {
	Key          string     // API key (ck_*) OR email for OAuth (display + dedup key)
	CredType     CBCredType // "api_key" or "oauth"
	AccessToken  string     // OAuth only
	RefreshToken string     // OAuth only
	ExpiresAt    time.Time  // OAuth only
	Email        string     // OAuth only (same as Key)
	mu           sync.RWMutex
	disabled     bool
	disabledAt   time.Time
	creditsUsed  float64
	totalReqs    int64
	// Meter fields — populated by SyncCredits from /v2/billing/meter/get-user-resource.
	// creditLimit == 0 means "never synced"; CreditLimit() falls back to CB_CREDIT_LIMIT.
	creditLimit   float64
	creditsRemain float64
	packageName   string
	cycleEnd      string
	meterStatus   int
	meterSyncedAt time.Time
	db            *db.Store
	refreshSF     singleflight.Group // OAuth only — collapses concurrent Refresh()
	syncSF        singleflight.Group // collapses concurrent SyncCredits()
}

// NewCBKeyForTest returns a CBKey for whitebox tests.
func NewCBKeyForTest(key string, opts ...CBKeyOption) *CBKey {
	k := &CBKey{Key: key, CredType: CBAuthAPIKey}
	for _, o := range opts {
		o(k)
	}
	if k.CredType == "" {
		k.CredType = CBAuthAPIKey
	}
	return k
}

// CBKeyOption mutates a test CBKey.
type CBKeyOption func(*CBKey)

// WithCBDisabledCooldown marks the key disabled with a cooldown timestamp.
// Zero time = permanent disable.
func WithCBDisabledCooldown(at time.Time) CBKeyOption {
	return func(k *CBKey) { k.disabled = true; k.disabledAt = at }
}

// WithCBCredType sets the credential type for tests.
func WithCBCredType(t CBCredType) CBKeyOption {
	return func(k *CBKey) { k.CredType = t }
}

// WithCBOAuthTokens sets OAuth token fields for tests.
func WithCBOAuthTokens(access, refresh string, expiresAt time.Time) CBKeyOption {
	return func(k *CBKey) {
		k.CredType = CBAuthOAuth
		k.AccessToken = access
		k.RefreshToken = refresh
		k.ExpiresAt = expiresAt
		if k.Email == "" {
			k.Email = k.Key
		}
	}
}

// Stats returns credits used, total requests, and disabled flag.
func (k *CBKey) Stats() (float64, int64, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.creditsUsed, k.totalReqs, k.disabled
}

// CreditLimit returns the per-key credit limit from the meter API when synced,
// otherwise the package-level CB_CREDIT_LIMIT fallback.
func (k *CBKey) CreditLimit() float64 {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.creditLimit > 0 {
		return k.creditLimit
	}
	return CB_CREDIT_LIMIT
}

// IsDisabled returns the disabled flag (mutex-safe).
func (k *CBKey) IsDisabled() bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.disabled
}

// GetCredType returns the credential type (mutex-safe).
func (k *CBKey) GetCredType() CBCredType {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.CredType == "" {
		return CBAuthAPIKey
	}
	return k.CredType
}

// CBKeySnapshot is a mutex-safe copy of CBKey state for handlers/metrics.
type CBKeySnapshot struct {
	Key           string
	CredType      CBCredType
	Email         string
	ExpiresAt     time.Time
	CreditsUsed   float64
	CreditLimit   float64
	CreditsRemain float64
	PackageName   string
	CycleEnd      string
	MeterStatus   int
	MeterSyncedAt time.Time
	TotalReqs     int64
	Disabled      bool
	DisabledAt    time.Time
}

// Snapshot returns a mutex-safe copy of the key's current state.
func (k *CBKey) Snapshot() CBKeySnapshot {
	k.mu.RLock()
	defer k.mu.RUnlock()
	ct := k.CredType
	if ct == "" {
		ct = CBAuthAPIKey
	}
	limit := k.creditLimit
	if limit <= 0 {
		limit = CB_CREDIT_LIMIT
	}
	return CBKeySnapshot{
		Key:           k.Key,
		CredType:      ct,
		Email:         k.Email,
		ExpiresAt:     k.ExpiresAt,
		CreditsUsed:   k.creditsUsed,
		CreditLimit:   limit,
		CreditsRemain: k.creditsRemain,
		PackageName:   k.packageName,
		CycleEnd:      k.cycleEnd,
		MeterStatus:   k.meterStatus,
		MeterSyncedAt: k.meterSyncedAt,
		TotalReqs:     k.totalReqs,
		Disabled:      k.disabled,
		DisabledAt:    k.disabledAt,
	}
}

// DisplayID returns a log/dashboard-safe identifier: email for OAuth,
// masked API key for api_key. Never logs full tokens.
func (k *CBKey) DisplayID() string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.displayIDLocked()
}

func (k *CBKey) displayIDLocked() string {
	if k.CredType == CBAuthOAuth {
		if k.Email != "" {
			return k.Email
		}
		return k.Key
	}
	return maskCBKey(k.Key)
}

// maskCBKey masks a ck_* key (or any string) for logs/dashboard.
func maskCBKey(key string) string {
	if len(key) > 12 {
		return key[:8] + "..." + key[len(key)-4:]
	}
	return key
}

// AuthHeader returns the Authorization header value for this credential.
// API key: "Bearer ck_*"; OAuth: "Bearer <accessToken>".
func (k *CBKey) AuthHeader() string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.CredType == CBAuthOAuth {
		return "Bearer " + k.AccessToken
	}
	return "Bearer " + k.Key
}

// IsExpired reports whether an OAuth access token needs refresh.
// API keys never expire from the gateway's perspective.
func (k *CBKey) IsExpired() bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.CredType != CBAuthOAuth {
		return false
	}
	if k.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().After(k.ExpiresAt.Add(-REFRESH_BUFFER))
}

// toDTO returns a db.CBKeyDTO snapshot under RLock. Use before saveCBKey.
func (k *CBKey) toDTO() db.CBKeyDTO {
	k.mu.RLock()
	defer k.mu.RUnlock()
	credType := string(k.CredType)
	if credType == "" {
		credType = string(CBAuthAPIKey)
	}
	return db.CBKeyDTO{
		Key:           k.Key,
		CredType:      credType,
		AccessToken:   k.AccessToken,
		RefreshToken:  k.RefreshToken,
		ExpiresAt:     k.ExpiresAt,
		Email:         k.Email,
		CreditsUsed:   k.creditsUsed,
		TotalReqs:     k.totalReqs,
		Disabled:      k.disabled,
		DisabledAt:    k.disabledAt,
		CreditLimit:   k.creditLimit,
		CreditsRemain: k.creditsRemain,
		PackageName:   k.packageName,
		CycleEnd:      k.cycleEnd,
		MeterStatus:   k.meterStatus,
		MeterSyncedAt: k.meterSyncedAt,
	}
}

// EnsureValid refreshes an OAuth token if near expiry. API keys are a no-op.
func (k *CBKey) EnsureValid() error {
	if k.GetCredType() != CBAuthOAuth {
		return nil
	}
	if !k.IsExpired() {
		return nil
	}
	return k.Refresh()
}

// Refresh refreshes an OAuth access token via CB_OAUTH_REFRESH_URL.
// Concurrent calls for the same account are collapsed via singleflight.
// Mutex is NOT held during the HTTP round-trip (lock-split).
func (k *CBKey) Refresh() error {
	if k.GetCredType() != CBAuthOAuth {
		return nil
	}
	_, err, _ := k.refreshSF.Do(k.Key, func() (any, error) {
		return nil, k.refreshLocked()
	})
	return err
}

func (k *CBKey) refreshLocked() error {
	// Snapshot refresh material under lock — no network under mu.
	k.mu.Lock()
	email := k.Email
	if email == "" {
		email = k.Key
	}
	rt := k.RefreshToken
	k.mu.Unlock()

	if rt == "" {
		return fmt.Errorf("cb oauth refresh: empty refresh token for %s", email)
	}

	slog.Debug("refreshing oauth", "module", "cb-refresh", "email", email)

	req, err := http.NewRequest("POST", CB_OAUTH_REFRESH_URL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Refresh-Token", rt)
	req.Header.Set("X-Auth-Refresh-Source", "cli")

	client, proxyID := getClient(tokenRefreshClient, "codebuddy")
	resp, err := client.Do(req)
	if err != nil {
		markProxyResult(proxyID, err, 0)
		return err
	}
	markProxyResult(proxyID, nil, resp.StatusCode)
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("cb oauth refresh [%d]: %s", resp.StatusCode, truncateLog(string(body), 200))
	}

	// Response: {"code":0,"data":{"accessToken":"...","refreshToken":"...","expiresIn":31535929}}
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("cb oauth refresh parse: %w", err)
	}
	if envelope.Code != 0 {
		return fmt.Errorf("cb oauth refresh code=%d msg=%s", envelope.Code, envelope.Msg)
	}
	if envelope.Data.AccessToken == "" {
		return fmt.Errorf("cb oauth refresh: empty accessToken")
	}

	k.mu.Lock()
	k.AccessToken = envelope.Data.AccessToken
	if envelope.Data.RefreshToken != "" {
		k.RefreshToken = envelope.Data.RefreshToken
	}
	if envelope.Data.ExpiresIn > 0 {
		k.ExpiresAt = time.Now().Add(time.Duration(envelope.Data.ExpiresIn) * time.Second)
	} else {
		// Fallback: 365 days (documented CB OAuth TTL)
		k.ExpiresAt = time.Now().Add(365 * 24 * time.Hour)
	}
	expAt := k.ExpiresAt
	k.mu.Unlock()

	slog.Info("oauth refresh ok", "module", "cb-refresh", "email", email, "expires_at", expAt.Format(time.RFC3339))
	if k.db != nil {
		saveCBKey(k.db, k.toDTO())
		k.db.LogRefresh(db.RefreshLog{
			Timestamp:    time.Now(),
			AccountEmail: email,
			Provider:     "codebuddy",
			Success:      true,
		})
		k.db.LogEvent(db.AccountEvent{
			Timestamp: time.Now(),
			AccountID: email,
			Provider:  "codebuddy",
			EventType: "token_refreshed",
		})
	}
	return nil
}

// AddCredits accumulates credits and auto-disables when the limit is hit.
// Uses the per-key CreditLimit() (meter CapacitySizePrecise when synced,
// otherwise CB_CREDIT_LIMIT fallback). SSE-parsed credits still work as an
// interim signal until the next meter sync.
func (k *CBKey) AddCredits(c float64) {
	k.mu.Lock()
	k.creditsUsed += c
	k.totalReqs++
	// Prefer meter remain when available: if remain was set and used climbs
	// above limit, disable. creditLimit==0 → fallback constant.
	limit := k.creditLimit
	if limit <= 0 {
		limit = CB_CREDIT_LIMIT
	}
	// If meter remain is known, also update local remain estimate.
	if k.meterSyncedAt.After(time.Time{}) && k.creditLimit > 0 {
		k.creditsRemain = k.creditLimit - k.creditsUsed
		if k.creditsRemain < 0 {
			k.creditsRemain = 0
		}
	}
	if k.creditsUsed >= limit {
		k.disabled = true
		k.disabledAt = time.Time{} // permanent until reset
		slog.Warn("key disabled (credits used)",
			"module", "cb",
			"key", k.displayIDLocked(),
			"credits_used", k.creditsUsed,
			"credit_limit", limit)
	}
	k.mu.Unlock()
	if k.db != nil {
		saveCBKey(k.db, k.toDTO())
	}
}

// SyncCredits fetches live credit usage from the CodeBuddy meter API and
// updates local state. Works for both API keys (ck_*) and OAuth access tokens
// via Authorization: Bearer. Concurrent calls for the same key are collapsed
// via singleflight. Never holds mu during the network round-trip.
//
// On Status==3 (exhausted) or CapacityRemainPrecise<=0 the key is permanently
// disabled. Permanent disables are never auto-reenabled even if the meter later
// reports remain>0 (operator must re-import / re-enable manually).
func (k *CBKey) SyncCredits() error {
	_, err, _ := k.syncSF.Do(k.Key, func() (any, error) {
		return nil, k.syncCreditsLocked()
	})
	return err
}

func (k *CBKey) syncCreditsLocked() error {
	// OAuth: ensure access token is fresh before the meter call.
	if k.GetCredType() == CBAuthOAuth {
		if err := k.EnsureValid(); err != nil {
			return fmt.Errorf("cb credit sync ensure valid: %w", err)
		}
	}

	auth := k.AuthHeader()
	display := k.DisplayID()

	req, err := http.NewRequest("POST", CB_CREDIT_METER_URL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)

	client, proxyID := getClient(tokenRefreshClient, "codebuddy")
	resp, err := client.Do(req)
	if err != nil {
		markProxyResult(proxyID, err, 0)
		return fmt.Errorf("cb credit sync: %w", err)
	}
	markProxyResult(proxyID, nil, resp.StatusCode)
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("cb credit sync [%d]: %s", resp.StatusCode, truncateLog(string(body), 200))
	}

	account, err := parseCBMeterAccount(body)
	if err != nil {
		return err
	}

	// Parse precise fields first, fall back to int fields.
	size := parseFloatOr(account.CapacitySizePrecise, float64(account.CapacitySize))
	used := parseFloatOr(account.CapacityUsedPrecise, float64(account.CapacityUsed))
	remain := parseFloatOr(account.CapacityRemainPrecise, float64(account.CapacityRemain))

	k.mu.Lock()
	k.creditsUsed = used
	if size > 0 {
		k.creditLimit = size
	}
	k.creditsRemain = remain
	k.packageName = account.PackageName
	k.cycleEnd = account.CycleEndTime
	k.meterStatus = account.Status
	k.meterSyncedAt = time.Now()

	// Exhausted: permanent disable. Do NOT auto-reenable permanent disables
	// even if meter later reports remain>0 (safe).
	if account.Status == 3 || remain <= 0 {
		if !k.disabled || !k.disabledAt.IsZero() {
			// Either not disabled, or only on cooldown — make permanent.
			k.disabled = true
			k.disabledAt = time.Time{}
			slog.Warn("key disabled (meter exhausted)",
				"module", "cb-meter",
				"key", k.displayIDLocked(),
				"remain", remain,
				"status", account.Status,
				"package", account.PackageName)
		}
	}
	k.mu.Unlock()

	if k.db != nil {
		saveCBKey(k.db, k.toDTO())
	}
	slog.Debug("credit sync ok",
		"module", "cb-meter",
		"key", display,
		"used", used,
		"remain", remain,
		"limit", size,
		"status", account.Status)
	return nil
}

// cbMeterAccount is the first Accounts[] entry from the meter API.
type cbMeterAccount struct {
	PackageName           string `json:"PackageName"`
	CapacitySize          int    `json:"CapacitySize"`
	CapacityUsed          int    `json:"CapacityUsed"`
	CapacityRemain        int    `json:"CapacityRemain"`
	CapacitySizePrecise   string `json:"CapacitySizePrecise"`
	CapacityUsedPrecise   string `json:"CapacityUsedPrecise"`
	CapacityRemainPrecise string `json:"CapacityRemainPrecise"`
	CycleStartTime        string `json:"CycleStartTime"`
	CycleEndTime          string `json:"CycleEndTime"`
	Status                int    `json:"Status"`
}

// parseCBMeterAccount extracts Accounts[0] from the nested meter response.
// Shape: {"code":0,"data":{"Response":{"Data":{"Accounts":[...]}}}}
func parseCBMeterAccount(body []byte) (cbMeterAccount, error) {
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			Response struct {
				Data struct {
					Accounts []cbMeterAccount `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return cbMeterAccount{}, fmt.Errorf("cb credit sync parse: %w", err)
	}
	if envelope.Code != 0 {
		return cbMeterAccount{}, fmt.Errorf("cb credit sync code=%d msg=%s", envelope.Code, envelope.Msg)
	}
	if len(envelope.Data.Response.Data.Accounts) == 0 {
		return cbMeterAccount{}, fmt.Errorf("cb credit sync: empty Accounts")
	}
	return envelope.Data.Response.Data.Accounts[0], nil
}

// parseFloatOr parses s as float64; on failure returns fallback.
func parseFloatOr(s string, fallback float64) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fallback
	}
	return v
}

type CBKeyManager struct {
	keys []*CBKey
	mu   sync.RWMutex
	next int
	db   *db.Store
}

func NewCBKeyManager(store *db.Store) *CBKeyManager {
	return &CBKeyManager{keys: make([]*CBKey, 0), db: store}
}

// SetKeysForTest replaces the internal slice. Whitebox tests only.
func (km *CBKeyManager) SetKeysForTest(keys []*CBKey) {
	km.mu.Lock()
	defer km.mu.Unlock()
	km.keys = keys
}

// LoadFromRedis loads all CB keys from Redis (single source of truth).
// If Redis is empty (fresh deploy), falls back to file/env as bootstrap seed,
// then persists those keys to Redis so subsequent starts are file-independent.
// Existing entries without cred_type default to api_key (backward compatible).
func (km *CBKeyManager) LoadFromRedis() error {
	redisState, err := km.db.LoadCBKeys()
	if err != nil {
		return fmt.Errorf("cb keys load: %w", err)
	}

	if len(redisState) > 0 {
		for apiKey, state := range redisState {
			key := &CBKey{Key: apiKey, db: km.db, CredType: CBAuthAPIKey}
			// cred_type defaults to api_key for legacy entries
			if ct := state["cred_type"]; ct == string(CBAuthOAuth) {
				key.CredType = CBAuthOAuth
				key.AccessToken = state["access_token"]
				key.RefreshToken = state["refresh_token"]
				key.Email = state["email"]
				if key.Email == "" {
					key.Email = apiKey // Key field is email for OAuth
				}
				if v := state["expires_at"]; v != "" {
					if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
						key.ExpiresAt = time.Unix(n, 0)
					}
				}
			}
			if cu, err := strconv.ParseFloat(state["credits_used"], 64); err == nil {
				key.creditsUsed = cu
			}
			if tr, err := strconv.ParseInt(state["total_requests"], 10, 64); err == nil {
				key.totalReqs = tr
			}
			if state["disabled"] == "true" || state["disabled"] == "1" {
				key.disabled = true
				if v := state["disabled_at"]; v != "" {
					if n, err := strconv.ParseInt(v, 10, 64); err == nil {
						if n <= 0 {
							key.disabledAt = time.Time{}
						} else {
							key.disabledAt = time.Unix(n, 0)
						}
					} else {
						key.disabledAt = time.Time{}
					}
				} else {
					key.disabledAt = time.Time{}
				}
			}
			// Meter fields (optional — missing = never synced, fallback CB_CREDIT_LIMIT)
			if v := state["credit_limit"]; v != "" {
				if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
					key.creditLimit = f
				}
			}
			if v := state["credits_remain"]; v != "" {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					key.creditsRemain = f
				}
			}
			key.packageName = state["package_name"]
			key.cycleEnd = state["cycle_end"]
			if v := state["meter_status"]; v != "" {
				if n, err := strconv.Atoi(v); err == nil {
					key.meterStatus = n
				}
			}
			if v := state["meter_synced_at"]; v != "" {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
					key.meterSyncedAt = time.Unix(n, 0)
				}
			}
			km.keys = append(km.keys, key)
		}
		slog.Info("loaded keys from Redis", "module", "cb", "count", len(km.keys))
		return nil
	}

	// Bootstrap from file/env (first run only)
	keysStr := os.Getenv("CB_API_KEYS")
	if keysStr == "" {
		keysStr = os.Getenv("CB_API_KEY")
	}
	if keysStr == "" {
		if v := os.Getenv("CB_KEY_FILE"); v != "" {
			if data, err := os.ReadFile(v); err == nil {
				keysStr = strings.TrimSpace(string(data))
			}
		} else {
			if data, err := os.ReadFile("./codebuddy-key.txt"); err == nil {
				keysStr = strings.TrimSpace(string(data))
			}
		}
	}
	if keysStr == "" {
		slog.Warn("no API keys found (Redis empty, no file/env bootstrap)", "module", "cb")
		return nil
	}

	seedCount := 0
	for _, k := range strings.FieldsFunc(keysStr, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	}) {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		key := &CBKey{Key: k, CredType: CBAuthAPIKey, db: km.db}
		km.keys = append(km.keys, key)
		if km.db != nil {
			saveCBKey(km.db, key.toDTO())
		}
		seedCount++
	}
	slog.Info("bootstrapped keys from file/env → Redis (first run)", "module", "cb", "count", seedCount)
	return nil
}

func (km *CBKeyManager) Next() (*CBKey, error) {
	km.mu.Lock()
	defer km.mu.Unlock()
	if len(km.keys) == 0 {
		return nil, fmt.Errorf("no cb keys")
	}
	for i := 0; i < len(km.keys); i++ {
		idx := (km.next + i) % len(km.keys)
		key := km.keys[idx]
		key.mu.Lock()
		if key.disabled {
			key.mu.Unlock()
			continue
		}
		key.mu.Unlock()
		km.next = (idx + 1) % len(km.keys)
		return key, nil
	}
	return nil, fmt.Errorf("all cb keys disabled")
}

func (km *CBKeyManager) Len() int {
	km.mu.RLock()
	defer km.mu.RUnlock()
	return len(km.keys)
}

func (km *CBKeyManager) GetAll() []*CBKey {
	km.mu.RLock()
	defer km.mu.RUnlock()
	r := make([]*CBKey, len(km.keys))
	copy(r, km.keys)
	return r
}

// ResolveKey resolves a masked key (e.g. "ck_abcde...wxyz"), full key, or
// OAuth email to the full Key field string. Returns empty string if not found.
func (km *CBKeyManager) ResolveKey(maskedOrFull string) string {
	km.mu.RLock()
	defer km.mu.RUnlock()
	for _, k := range km.keys {
		if k.Key == maskedOrFull {
			return k.Key
		}
		// OAuth: also match by Email field
		if k.CredType == CBAuthOAuth && k.Email == maskedOrFull {
			return k.Key
		}
		// Check masked form: first 8 + "..." + last 4 (API keys)
		if len(k.Key) > 12 {
			masked := k.Key[:8] + "..." + k.Key[len(k.Key)-4:]
			if masked == maskedOrFull {
				return k.Key
			}
		}
	}
	return ""
}

// AddKey hot-imports a CodeBuddy API key into the runtime pool + Redis.
func (km *CBKeyManager) AddKey(apiKey string) (added bool, total int) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return false, km.Len()
	}
	km.mu.Lock()
	for _, existing := range km.keys {
		if existing.Key == apiKey {
			n := len(km.keys)
			km.mu.Unlock()
			return false, n
		}
	}
	key := &CBKey{Key: apiKey, CredType: CBAuthAPIKey, db: km.db}
	km.keys = append(km.keys, key)
	total = len(km.keys)
	km.mu.Unlock()
	if km.db != nil {
		saveCBKey(km.db, key.toDTO())
	}
	return true, total
}

// AddOAuthAccount hot-imports a CodeBuddy OAuth account (dedup by email).
// Key field = email for OAuth entries.
func (km *CBKeyManager) AddOAuthAccount(email, accessToken, refreshToken string, expiresAt time.Time) (added bool, total int) {
	email = strings.TrimSpace(email)
	accessToken = strings.TrimSpace(accessToken)
	refreshToken = strings.TrimSpace(refreshToken)
	if email == "" || accessToken == "" || refreshToken == "" {
		return false, km.Len()
	}

	// Eager refresh: if the supplied AT is already expired (or within the
	// 10-minute refresh buffer), try refreshing via RT now so the account
	// is in a usable state before it enters the pool.  We perform this
	// BEFORE acquiring km.mu to avoid blocking the hot path; if refresh
	// succeeds we use the fresh AT/RT, otherwise we fall through and store
	// the supplied tokens as-is (the 401 path / pre-warm worker can retry
	// later, and permanent disable handles truly dead RTs).
	if expiresAt.Before(time.Now().Add(REFRESH_BUFFER)) {
		probe := &CBKey{
			Key:          email,
			CredType:     CBAuthOAuth,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			ExpiresAt:    expiresAt,
			Email:        email,
		}
		if refreshedAt, err := tryEagerRefresh(probe); err == nil && !refreshedAt.IsZero() {
			accessToken = probe.AccessToken
			refreshToken = probe.RefreshToken
			expiresAt = refreshedAt
			slog.Info("oauth eager refresh ok", "module", "cb", "email", email)
		} else if err != nil {
			slog.Warn("oauth eager refresh failed, storing as-is", "module", "cb", "email", email, "error", err)
		}
	}

	km.mu.Lock()
	for _, existing := range km.keys {
		if existing.Key == email || (existing.CredType == CBAuthOAuth && existing.Email == email) {
			// Update tokens on existing OAuth entry
			existing.mu.Lock()
			existing.CredType = CBAuthOAuth
			existing.AccessToken = accessToken
			existing.RefreshToken = refreshToken
			existing.ExpiresAt = expiresAt
			existing.Email = email
			existing.disabled = false
			existing.disabledAt = time.Time{}
			existing.mu.Unlock()
			n := len(km.keys)
			km.mu.Unlock()
			if km.db != nil {
				saveCBKey(km.db, existing.toDTO())
			}
			return false, n
		}
	}
	key := &CBKey{
		Key:          email,
		CredType:     CBAuthOAuth,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
		Email:        email,
		db:           km.db,
	}
	km.keys = append(km.keys, key)
	total = len(km.keys)
	km.mu.Unlock()
	if km.db != nil {
		saveCBKey(km.db, key.toDTO())
	}
	return true, total
}

// tryEagerRefresh performs a one-shot OAuth token refresh on a probe CBKey
// (not yet in the pool). On success the probe's token fields are updated in
// place and the new ExpiresAt is returned. Network round-trip runs WITHOUT
// the pool mutex held.
func tryEagerRefresh(probe *CBKey) (time.Time, error) {
	if probe.GetCredType() != CBAuthOAuth {
		return time.Time{}, nil
	}
	if err := probe.Refresh(); err != nil {
		return time.Time{}, err
	}
	probe.mu.RLock()
	at := probe.AccessToken
	rt := probe.RefreshToken
	exp := probe.ExpiresAt
	probe.mu.RUnlock()
	if at == "" || rt == "" {
		return time.Time{}, fmt.Errorf("eager refresh returned empty token")
	}
	return exp, nil
}

// ReenableCooldowns lifts temp cooldowns past 10 minutes (background only).
func (km *CBKeyManager) ReenableCooldowns() {
	keys := km.GetAll()
	now := time.Now()
	var reenabled []*CBKey
	for _, key := range keys {
		key.mu.Lock()
		if key.disabled && !key.disabledAt.IsZero() && now.Sub(key.disabledAt) > 10*time.Minute {
			key.disabled = false
			reenabled = append(reenabled, key)
		}
		key.mu.Unlock()
	}
	for _, key := range reenabled {
		if key.db != nil {
			saveCBKey(key.db, key.toDTO())
		}
		slog.Info("re-enabled cooldown key", "module", "cb", "key", key.DisplayID())
	}
}

// ReenableCBWorker is the long-lived goroutine that lifts cooldowns.
func ReenableCBWorker(km *CBKeyManager) {
	km.ReenableCooldowns()
	ticker := time.NewTicker(REENABLE_TICK)
	defer ticker.Stop()
	for range ticker.C {
		km.ReenableCooldowns()
	}
}

// CBOAuthRefreshWorker pre-warms OAuth access tokens before they expire.
// Mirrors Grok's AutoRefreshWorker: every PRE_WARM_TICK, scan OAuth keys
// within PRE_WARM_WINDOW of expiry, refresh with MAX_CONCURRENT_REFRESH cap.
func CBOAuthRefreshWorker(km *CBKeyManager) {
	ticker := time.NewTicker(PRE_WARM_TICK)
	defer ticker.Stop()
	for range ticker.C {
		keys := km.GetAll()
		var wg sync.WaitGroup
		sem := make(chan struct{}, MAX_CONCURRENT_REFRESH)

		for _, k := range keys {
			k.mu.RLock()
			isOAuth := k.CredType == CBAuthOAuth
			perm := k.disabled && k.disabledAt.IsZero()
			needsRefresh := isOAuth && !perm && !k.ExpiresAt.IsZero() &&
				time.Now().After(k.ExpiresAt.Add(-PRE_WARM_WINDOW))
			email := k.Email
			if email == "" {
				email = k.Key
			}
			k.mu.RUnlock()

			if !needsRefresh {
				continue
			}

			wg.Add(1)
			sem <- struct{}{}
			go func(key *CBKey, email string) {
				defer wg.Done()
				defer func() { <-sem }()

				if err := key.Refresh(); err != nil {
					slog.Warn("oauth pre-warm refresh error", "module", "cb-worker", "email", email, "error", err)
				}
			}(k, email)
		}
		wg.Wait()
	}
}

// CBCreditSyncWorker periodically syncs credit usage from the CodeBuddy meter
// API for all non-permanently-disabled keys. Runs once immediately at start
// (with small stagger), then every CB_CREDIT_SYNC_TICK with concurrency
// CB_CREDIT_SYNC_CONCURRENCY.
func CBCreditSyncWorker(km *CBKeyManager) {
	// Immediate first pass with small stagger so we don't stampede on boot.
	syncAllCBCredits(km, true)

	ticker := time.NewTicker(CB_CREDIT_SYNC_TICK)
	defer ticker.Stop()
	for range ticker.C {
		syncAllCBCredits(km, false)
	}
}

// syncAllCBCredits walks the pool and SyncCredits() each non-permanent-disabled
// key. stagger=true adds a small per-key delay on the first boot pass.
func syncAllCBCredits(km *CBKeyManager, stagger bool) {
	keys := km.GetAll()
	var wg sync.WaitGroup
	sem := make(chan struct{}, CB_CREDIT_SYNC_CONCURRENCY)
	idx := 0
	for _, k := range keys {
		k.mu.RLock()
		perm := k.disabled && k.disabledAt.IsZero()
		display := k.displayIDLocked()
		k.mu.RUnlock()
		if perm {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		delay := time.Duration(0)
		if stagger {
			delay = time.Duration(idx%CB_CREDIT_SYNC_CONCURRENCY) * 200 * time.Millisecond
		}
		idx++
		go func(key *CBKey, display string, delay time.Duration) {
			defer wg.Done()
			defer func() { <-sem }()
			if delay > 0 {
				time.Sleep(delay)
			}
			if err := key.SyncCredits(); err != nil {
				slog.Warn("credit sync error", "module", "cb-meter", "key", display, "error", err)
			}
		}(k, display, delay)
	}
	wg.Wait()
}

// ===========================================================================
// CODEBUDDY PROXY
// ===========================================================================

func stripCBPrefix(model string) string {
	return strings.TrimPrefix(model, "cb/")
}

// cbTransform: force stream:true, inject system message, strip cb/ prefix.
// Also converts max_tokens → max_completion_tokens (CB uses the latter).
func cbTransform(body []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	m["stream"] = true
	if model, ok := m["model"].(string); ok {
		m["model"] = stripCBPrefix(model)
	}
	if mt, ok := m["max_tokens"]; ok {
		if _, exists := m["max_completion_tokens"]; !exists {
			m["max_completion_tokens"] = mt
		}
		delete(m, "max_tokens")
	}
	msgs, ok := m["messages"].([]any)
	if !ok || len(msgs) == 0 {
		m["messages"] = []any{
			map[string]any{"role": "system", "content": CB_DEFAULT_SYSTEM},
			map[string]any{"role": "user", "content": "Hello"},
		}
	} else {
		first, ok := msgs[0].(map[string]any)
		if !ok || first["role"] != "system" {
			sys := map[string]any{"role": "system", "content": CB_DEFAULT_SYSTEM}
			m["messages"] = append([]any{sys}, msgs...)
		}
	}
	return json.Marshal(m)
}

// cbCollectStream: read SSE stream → return single JSON (for non-stream clients).
func cbCollectStream(resp *http.Response, model string, key *CBKey) gin.H {
	defer resp.Body.Close()
	var content, reasoning strings.Builder
	var finish string
	var usage map[string]any
	var credit float64

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			content.WriteString(chunk.Choices[0].Delta.Content)
			reasoning.WriteString(chunk.Choices[0].Delta.ReasoningContent)
			if chunk.Choices[0].FinishReason != "" {
				finish = chunk.Choices[0].FinishReason
			}
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
			if c, ok := chunk.Usage["credit"].(float64); ok && c > 0 {
				credit = c
			}
		}
	}
	if finish == "" {
		finish = "stop"
	}
	if credit > 0 && key != nil {
		key.AddCredits(credit)
	}
	resp2 := gin.H{
		"id":      "chatcmpl-" + time.Now().Format("20060102150405"),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []gin.H{{
			"index":         0,
			"message":       gin.H{"role": "assistant", "content": content.String()},
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		resp2["usage"] = usage
	}
	return resp2
}

// permanentDisable marks a key permanently disabled and persists via toDTO.
func permanentDisable(key *CBKey, reason string) {
	key.mu.Lock()
	key.disabled = true
	key.disabledAt = time.Time{}
	key.mu.Unlock()
	if key.db != nil {
		saveCBKey(key.db, key.toDTO())
	}
	slog.Warn("key disabled", "module", "cb", "key", key.DisplayID(), "reason", reason)
}

// cooldownDisable marks a key with a temp cooldown and persists.
func cooldownDisable(key *CBKey, reason string) {
	key.mu.Lock()
	key.disabled = true
	key.disabledAt = time.Now()
	key.mu.Unlock()
	if key.db != nil {
		saveCBKey(key.db, key.toDTO())
	}
	slog.Warn("key disabled", "module", "cb", "key", key.DisplayID(), "reason", reason)
}

// ProxyCodeBuddy forwards a chat/completions request to CodeBuddy.
func ProxyCodeBuddy(c *gin.Context, body []byte, bodyMap map[string]any, km *CBKeyManager, clientStream bool, hc *HealthChecker) {
	if !hc.CB.CanRequest() {
		hc.CB.RecordRequest(0, fmt.Errorf("circuit open"))
		c.JSON(503, gin.H{"error": "codebuddy upstream circuit breaker open"})
		c.Set("error_msg", "cb circuit breaker open")
		errJSON, _ := json.Marshal(gin.H{"error": "codebuddy upstream circuit breaker open"})
		c.Set("response_body", json.RawMessage(errJSON))
		return
	}

	originalModel, _ := bodyMap["model"].(string)

	transformed, err := cbTransform(body)
	if err != nil {
		c.JSON(400, gin.H{"error": fmt.Sprintf("transform: %v", err)})
		return
	}

	client, proxyID := getClient(upstreamClient, "codebuddy")
	total := km.Len()

	var lastResp *http.Response
	var lastKey *CBKey
	reqStart := time.Now()

	for attempt := 0; attempt < total; attempt++ {
		// C10: bail out early if the client cancelled — don't walk the
		// whole key list burning upstream calls for a dead request.
		if err := c.Request.Context().Err(); err != nil {
			slog.Debug("client cancelled before attempt", "module", "cb", "attempt", attempt+1, "error", err)
			return
		}
		key, err := km.Next()
		if err != nil {
			break
		}

		// OAuth: refresh if near-expiry before building the request.
		if err := key.EnsureValid(); err != nil {
			slog.Warn("ensure valid failed", "module", "cb", "key", key.DisplayID(), "error", err)
			// Fall through — try with existing token; 401 path may still refresh.
		}

		req, _ := http.NewRequestWithContext(c.Request.Context(), "POST", CB_UPSTREAM_URL, bytes.NewReader(transformed))
		req.Header.Set("Authorization", key.AuthHeader())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")

		resp, err := client.Do(req)
		if err != nil {
			markProxyResult(proxyID, err, 0)
			continue
		}
		markProxyResult(proxyID, nil, resp.StatusCode)

		if resp.StatusCode == 401 {
			resp.Body.Close()
			// OAuth: try one refresh + retry; API key: permanent disable.
			if key.GetCredType() == CBAuthOAuth {
				refreshErr := key.Refresh()
				if refreshErr != nil {
					permanentDisable(key, "401 oauth refresh failed: "+refreshErr.Error())
					continue
				}
				// Rebuild request with fresh AT
				req, _ = http.NewRequestWithContext(c.Request.Context(), "POST", CB_UPSTREAM_URL, bytes.NewReader(transformed))
				req.Header.Set("Authorization", key.AuthHeader())
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "text/event-stream")
				resp, err = client.Do(req)
				if err != nil {
					markProxyResult(proxyID, err, 0)
					continue
				}
				markProxyResult(proxyID, nil, resp.StatusCode)
				if resp.StatusCode == 401 {
					resp.Body.Close()
					permanentDisable(key, "401 after oauth refresh, permanent")
					continue
				}
				// Fall through to process non-401 response below.
			} else {
				permanentDisable(key, "401 unauthorized, permanent")
				continue
			}
		}

		if resp.StatusCode == 429 {
			// Read body for 429 to distinguish trial-not-activated (14017) from rate limit.
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			bodyStr := string(bodyBytes)
			resp.Body.Close()
			if strings.Contains(bodyStr, "14017") {
				permanentDisable(key, "429 trial not activated, permanent")
			} else {
				cooldownDisable(key, "429 rate limited, cooldown 10m")
			}
			continue
		}

		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			bodyStr := string(bodyBytes)
			// 403 with code 11140 = "request illegal" (banned/flagged key) → permanent disable
			if resp.StatusCode == 403 && strings.Contains(bodyStr, "11140") {
				permanentDisable(key, "403 request illegal, banned, permanent")
				continue
			}
			if strings.Contains(bodyStr, "14018") || strings.Contains(bodyStr, "Credits exhausted") {
				permanentDisable(key, "credits exhausted, code 14018")
				continue
			}
			if resp.StatusCode == 400 && (strings.Contains(bodyStr, "11133") || strings.Contains(bodyStr, "Invalid request parameters")) {
				hc.CB.RecordRequest(time.Since(reqStart), fmt.Errorf("cb 400 invalid params"))
				c.JSON(400, gin.H{"error": "CodeBuddy rejected request parameters", "detail": truncateLog(bodyStr, 500)})
				c.Set("error_msg", truncateLog(bodyStr, 500))
				errJSON, _ := json.Marshal(gin.H{"error": "CodeBuddy rejected request parameters", "detail": truncateLog(bodyStr, 500)})
				c.Set("response_body", json.RawMessage(errJSON))
				return
			}
			if resp.StatusCode == 400 && (strings.Contains(bodyStr, "11102") ||
				strings.Contains(bodyStr, "service info not found") ||
				strings.Contains(bodyStr, "model [") && strings.Contains(bodyStr, "] service info not found")) {
				hc.CB.RecordRequest(time.Since(reqStart), fmt.Errorf("cb 400 model not found"))
				// P3 #7: Don't leak upstream body (contains requestId + internal tracing).
				// Return generic message only.
				c.JSON(400, gin.H{"error": "model not available on CodeBuddy"})
				c.Set("error_msg", "model not available on CodeBuddy")
				errJSON, _ := json.Marshal(gin.H{"error": "model not available on CodeBuddy"})
				c.Set("response_body", json.RawMessage(errJSON))
				return
			}
			cooldownDisable(key, fmt.Sprintf("4xx status=%d body=%s", resp.StatusCode, truncateLog(bodyStr, 200)))
			continue
		}

		if resp.StatusCode >= 500 {
			resp.Body.Close()
			hc.CB.RecordRequest(time.Since(reqStart), fmt.Errorf("upstream %d", resp.StatusCode))
			continue
		}

		lastResp = resp
		lastKey = key
		break
	}

	if lastResp == nil {
		c.JSON(503, gin.H{"error": "all cb keys disabled"})
		c.Set("error_msg", "all cb keys disabled")
		errJSON, _ := json.Marshal(gin.H{"error": "all cb keys disabled"})
		c.Set("response_body", json.RawMessage(errJSON))
		return
	}

	hc.CB.RecordRequest(time.Since(reqStart), nil)
	// upstream_account: email for OAuth, masked key for API key
	c.Set("upstream_account", lastKey.DisplayID())

	if clientStream {
		defer lastResp.Body.Close()
		for k, v := range lastResp.Header {
			if strings.EqualFold(k, "Content-Encoding") || strings.EqualFold(k, "Content-Length") {
				continue
			}
			for _, vv := range v {
				c.Writer.Header().Add(k, vv)
			}
		}
		c.Writer.WriteHeader(lastResp.StatusCode)
		flusher, _ := c.Writer.(http.Flusher)
		scanner := bufio.NewScanner(lastResp.Body)
		scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
		// C6: same as grok — a client disconnect must stop the stream loop
		// promptly so we don't keep pulling from upstream forever.
		ctx := c.Request.Context()
		var streamContent strings.Builder
		var streamTokensIn, streamTokensOut int
		for scanner.Scan() {
			if err := ctx.Err(); err != nil {
				slog.Debug("sse loop: client cancelled", "module", "cb", "error", err)
				lastResp.Body.Close()
				break
			}
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				data := strings.TrimPrefix(line, "data: ")
				if data != "[DONE]" && data != "" {
					var sc sseChunk
					if json.Unmarshal([]byte(data), &sc) == nil {
						if sc.Error != nil {
							errBytes, _ := json.Marshal(sc.Error)
							errStr := string(errBytes)
							if strings.Contains(errStr, "14018") || strings.Contains(errStr, "Credits exhausted") {
								permanentDisable(lastKey, "credits exhausted in stream")
							} else if strings.Contains(errStr, "14017") {
								permanentDisable(lastKey, "trial not activated in stream, permanent")
							}
						}
						if sc.Usage != nil {
							if cr, ok := sc.Usage["credit"].(float64); ok && cr > 0 {
								lastKey.AddCredits(cr)
							}
							if pt, ok := sc.Usage["prompt_tokens"].(float64); ok {
								streamTokensIn = int(pt)
							}
							if ct, ok := sc.Usage["completion_tokens"].(float64); ok {
								streamTokensOut = int(ct)
							}
						}
						if len(sc.Choices) > 0 {
							streamContent.WriteString(sc.Choices[0].Delta.Content)
						}
					}
				}
			}
			if _, werr := fmt.Fprintf(c.Writer, "%s\n", line); werr != nil {
				slog.Debug("sse loop: write to client failed", "module", "cb", "error", werr)
				lastResp.Body.Close()
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		c.Set("output_text", truncateLog(streamContent.String(), 1000))
		c.Set("tokens_in", streamTokensIn)
		c.Set("tokens_out", streamTokensOut)
		respJSON, _ := json.Marshal(gin.H{
			"choices": []gin.H{{
				"message":       gin.H{"role": "assistant", "content": streamContent.String()},
				"finish_reason": "stop",
			}},
			"usage": gin.H{
				"prompt_tokens":     streamTokensIn,
				"completion_tokens": streamTokensOut,
				"total_tokens":      streamTokensIn + streamTokensOut,
			},
			"model":  originalModel,
			"stream": true,
		})
		c.Set("response_body", json.RawMessage(respJSON))
	} else {
		result := cbCollectStream(lastResp, originalModel, lastKey)
		c.JSON(200, result)
		if respBytes, err := json.Marshal(result); err == nil {
			c.Set("response_body", json.RawMessage(respBytes))
		}
		if choices, ok := result["choices"].([]gin.H); ok && len(choices) > 0 {
			if msg, ok := choices[0]["message"].(gin.H); ok {
				if content, ok := msg["content"].(string); ok {
					c.Set("output_text", truncateLog(content, 1000))
				}
			}
		}
		if usage, ok := result["usage"].(map[string]any); ok {
			if pt, ok := usage["prompt_tokens"].(float64); ok {
				c.Set("tokens_in", int(pt))
			}
			if ct, ok := usage["completion_tokens"].(float64); ok {
				c.Set("tokens_out", int(ct))
			}
		}
	}
}

// DeleteKey removes a CodeBuddy key by its key string (API key or OAuth email).
// Returns true if the key was found and removed.
func (km *CBKeyManager) DeleteKey(key string) bool {
	km.mu.Lock()
	for i, k := range km.keys {
		if k.Key == key {
			km.keys = append(km.keys[:i], km.keys[i+1:]...)
			km.mu.Unlock()
			if km.db != nil {
				km.db.DeleteCBKey(key)
			}
			slog.Info("deleted cb key", "module", "cb", "key", maskCBKey(key))
			return true
		}
	}
	km.mu.Unlock()
	return false
}

// CleanupDisabled removes all permanently disabled keys (disabledAt is zero time).
// Returns the count of removed keys. Does NOT affect cooldown keys (disabledAt set).
func (km *CBKeyManager) CleanupDisabled() int {
	km.mu.Lock()
	var removed int
	var kept []*CBKey
	for _, k := range km.keys {
		k.mu.RLock()
		permDisabled := k.disabled && k.disabledAt.IsZero()
		k.mu.RUnlock()
		if permDisabled {
			removed++
			if km.db != nil {
				km.db.DeleteCBKey(k.Key)
			}
		} else {
			kept = append(kept, k)
		}
	}
	km.keys = kept
	km.mu.Unlock()
	if removed > 0 {
		slog.Info("cleanup disabled cb keys", "module", "cb", "removed", removed, "remaining", km.Len())
	}
	return removed
}

// parseJWTExp extracts the exp claim from a JWT without verifying the signature.
// Returns zero time if the token is not a JWT or has no exp.
func parseJWTExp(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	// JWT payload is base64url without padding
	payload := parts[1]
	// Add padding if needed
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		// Try raw std encoding without padding variants
		raw, err = base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Time{}
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

// ParseJWTExp is exported for handlers that need to derive expires_at from an AT.
func ParseJWTExp(token string) time.Time { return parseJWTExp(token) }
