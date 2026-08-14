package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
	Key            string     // API key (ck_*) OR email for OAuth (display + dedup key)
	CredType       CBCredType // "api_key" or "oauth"
	AccessToken    string     // OAuth only
	RefreshToken   string     // OAuth only
	ExpiresAt      time.Time  // OAuth only
	Email          string     // OAuth only (same as Key)
	mu             sync.RWMutex
	disabled       bool
	disabledAt     time.Time
	disabledReason string // reason for permanent disable (persisted to Redis)
	creditsUsed    float64
	totalReqs      int64
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
	Key            string
	CredType       CBCredType
	Email          string
	ExpiresAt      time.Time
	CreditsUsed    float64
	CreditLimit    float64
	CreditsRemain  float64
	PackageName    string
	CycleEnd       string
	MeterStatus    int
	MeterSyncedAt  time.Time
	TotalReqs      int64
	Disabled       bool
	DisabledAt     time.Time
	DisabledReason string
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
		Key:            k.Key,
		CredType:       ct,
		Email:          k.Email,
		ExpiresAt:      k.ExpiresAt,
		CreditsUsed:    k.creditsUsed,
		CreditLimit:    limit,
		CreditsRemain:  k.creditsRemain,
		PackageName:    k.packageName,
		CycleEnd:       k.cycleEnd,
		MeterStatus:    k.meterStatus,
		MeterSyncedAt:  k.meterSyncedAt,
		TotalReqs:      k.totalReqs,
		Disabled:       k.disabled,
		DisabledAt:     k.disabledAt,
		DisabledReason: k.disabledReason,
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

// UserID returns the X-User-Id value the current CLI sends:
// "anonymous_" + last 8 chars of the credential (API key or OAuth AT).
func (k *CBKey) UserID() string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	cred := k.Key
	if k.CredType == CBAuthOAuth {
		cred = k.AccessToken
	}
	cred = strings.TrimSpace(cred)
	if len(cred) > 8 {
		cred = cred[len(cred)-8:]
	}
	return "anonymous_" + cred
}

// cbChatHeaders applies the header set the current CodeBuddy CLI (2.134.x)
// sends on /chat/completions — stable subset (drops OTel + stainless noise).
// Conversation/Request IDs are per-request UUIDs (cache is content-addressed
// per account, so they're telemetry only, but keeping them matches the CLI).
func cbChatHeaders(req *http.Request, key *CBKey) {
	req.Header.Set("Authorization", key.AuthHeader())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("x-requested-with", "XMLHttpRequest")
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Type", "CLI")
	req.Header.Set("X-IDE-Name", "CLI")
	req.Header.Set("X-IDE-Version", "2.134.0")
	req.Header.Set("X-Private-Data", "false")
	req.Header.Set("x-codebuddy-request", "1")
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-User-Id", key.UserID())
	req.Header.Set("User-Agent", "CLI/2.134.0 CodeBuddy/2.134.0")
	req.Header.Set("X-Conversation-ID", uuid.NewString())
	req.Header.Set("X-Request-ID", uuid.NewString())
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
// When TokenRefreshDisabled=1 (dev), AccessToken/RefreshToken are zeroed
// so OAuth credentials never persist to dev Redis (leak prevention).
// API keys (Key field) are kept — they don't rotate and are needed for dev.
func (k *CBKey) toDTO() db.CBKeyDTO {
	k.mu.RLock()
	defer k.mu.RUnlock()
	credType := string(k.CredType)
	if credType == "" {
		credType = string(CBAuthAPIKey)
	}
	at, rt := k.AccessToken, k.RefreshToken
	if TokenRefreshDisabled {
		at, rt = "", ""
	}
	return db.CBKeyDTO{
		Key:            k.Key,
		CredType:       credType,
		AccessToken:    at,
		RefreshToken:   rt,
		ExpiresAt:      k.ExpiresAt,
		Email:          k.Email,
		CreditsUsed:    k.creditsUsed,
		TotalReqs:      k.totalReqs,
		Disabled:       k.disabled,
		DisabledAt:     k.disabledAt,
		DisabledReason: k.disabledReason,
		CreditLimit:    k.creditLimit,
		CreditsRemain:  k.creditsRemain,
		PackageName:    k.packageName,
		CycleEnd:       k.cycleEnd,
		MeterStatus:    k.meterStatus,
		MeterSyncedAt:  k.meterSyncedAt,
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
	if TokenRefreshDisabled {
		return fmt.Errorf("token refresh disabled (TOKEN_REFRESH_DISABLED=1)")
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
		// I1: local-credit exhaustion is meter-driven — auto-lift when a
		// later meter sync reports credits.
		k.disabledReason = cbMeterDisableReason
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
// On Status==3 (exhausted) or CycleCapacityRemainPrecise<=0 (cycle-accurate
// remain — CapacityRemain is the static subscription size and stays 100 for
// exhausted Free Plans) the key is permanently
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

	accounts, err := parseCBMeterAccounts(body)
	if err != nil {
		return err
	}

	// applyMeterAccounts takes the lock itself — no outer Lock/Unlock here.
	sum, disabledNow, reenabledNow := k.applyMeterAccounts(accounts)

	if k.db != nil {
		saveCBKey(k.db, k.toDTO())
	}
	if disabledNow {
		slog.Warn("key disabled (meter exhausted)",
			"module", "cb-meter",
			"key", display,
			"remain", sum.remain,
			"status", sum.status,
			"package", sum.primary)
	}
	if reenabledNow {
		slog.Info("key re-enabled (meter has credits)",
			"module", "cb-meter",
			"key", display,
			"remain", sum.remain,
			"package", sum.primary)
	}
	slog.Debug("credit sync ok",
		"module", "cb-meter",
		"key", display,
		"used", sum.used,
		"remain", sum.remain,
		"limit", sum.size,
		"status", sum.status)
	return nil
}

// meterSummary is the aggregated result of applyMeterAccounts.
type meterSummary struct {
	size    float64
	used    float64
	remain  float64
	status  int
	primary string
}

// applyMeterAccounts aggregates ALL meter plans (a key can hold Bonus Pack +
// Free Plan Subscription concurrently) and writes the result under k.mu.
// Returns the summary, whether this call newly made the key permanently
// disabled, and whether it lifted a previous permanent disable.
// Sums size/used/remain across plans; keeps the plan with the most remain as
// the display package; status = 3 only when EVERY plan is exhausted (an
// exhausted Bonus Pack must NOT kill a key that still has Free Plan credits
// left — disable only when TOTAL remain <= 0).
//
// Re-enable semantics: a meter sync that reports credits (remain > 0 and not
// all-exhausted) LIFTS a permanent disable. This is safe because SyncCredits
// fails earlier for auth-dead keys (401 / revoked refresh token hit the same
// Authorization header before this code runs), so only keys whose meter call
// succeeds get re-enabled. Truly exhausted keys still report remain <= 0 and
// stay disabled.
// cbMeterDisableReason marks disables caused by the meter (credit) sync
// worker. Operator-initiated disables (DisableKey) carry arbitrary reasons
// and must NEVER be auto-re-enabled by the worker.
const cbMeterDisableReason = "meter exhausted"

// cycleEndAfter reports whether a > b as RFC3339 timestamps, falling back to
// lexicographic comparison when either string fails to parse.
func cycleEndAfter(a, b string) bool {
	ta, ea := time.Parse(time.RFC3339, a)
	tb, eb := time.Parse(time.RFC3339, b)
	if ea == nil && eb == nil {
		return ta.After(tb)
	}
	return a > b
}

func (k *CBKey) applyMeterAccounts(accounts []cbMeterAccount) (meterSummary, bool, bool) {
	// H1: empty/partial meter payload must not panic or touch state.
	if len(accounts) == 0 {
		return meterSummary{}, false, false
	}
	var size, used, remain float64
	status := 0
	primary := accounts[0].PackageName
	maxRemain := -1.0
	cycleEnd := ""
	allExhausted := len(accounts) > 0
	for _, acc := range accounts {
		aSize := meterSize(acc)
		aUsed := meterUsed(acc)
		aRemain := meterRemain(acc)
		size += aSize
		used += aUsed
		remain += aRemain
		if acc.Status != 3 {
			allExhausted = false
		}
		if aRemain > maxRemain {
			maxRemain = aRemain
			primary = acc.PackageName
		}
		if cycleEndAfter(acc.CycleEndTime, cycleEnd) {
			cycleEnd = acc.CycleEndTime
		}
	}
	if allExhausted {
		status = 3
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	// M4: update the three credit fields atomically — a zero-size payload
	// (schema drift / partial response) must not overwrite good data with 0
	// nor leave a stale limit paired with a fresh remain.
	if size > 0 {
		k.creditsUsed = used
		k.creditLimit = size
		k.creditsRemain = remain
	}
	k.packageName = primary
	k.cycleEnd = cycleEnd
	k.meterStatus = status
	k.meterSyncedAt = time.Now()

	disabledNow := false
	reenabledNow := false
	// H2: only Status 3 is authoritative on its own. A zero-size payload must
	// not be read as exhaustion — require evidence (size > 0) for remain <= 0.
	exhausted := status == 3 || (size > 0 && remain <= 0)
	if exhausted {
		// Either not disabled, or only on cooldown — make permanent.
		if !k.disabled || !k.disabledAt.IsZero() {
			k.disabled = true
			k.disabledAt = time.Time{}
			k.disabledReason = cbMeterDisableReason // L3: explain the disable
			disabledNow = true
		}
	} else {
		// H3: meter says credits remain — lift a disable ONLY when it came
		// from the meter path (or is a cooldown with a non-zero timestamp).
		// Operator disables (arbitrary reason, zero timestamp) fail closed.
		if k.disabled && (k.disabledReason == cbMeterDisableReason || !k.disabledAt.IsZero()) {
			k.disabled = false
			k.disabledAt = time.Time{}
			k.disabledReason = ""
			reenabledNow = true
		}
	}
	return meterSummary{size: size, used: used, remain: remain, status: status, primary: primary}, disabledNow, reenabledNow
}

// cbMeterAccount is one entry from the meter API Accounts[] list. A key can
// hold MULTIPLE plans concurrently (e.g. Bonus Pack trial + Free Plan
// Subscription) — the gateway aggregates all of them.
//
// IMPORTANT field semantics (verified live 2026-08-14): CapacityRemain is
// the STATIC subscription capacity for CapacityType=4 (Free Plan) — it
// reports 100 even when the cycle quota is fully consumed. CycleCapacityRemain
// is the real remaining quota for the current cycle and is the value that
// drives 14018 "credits exhausted" upstream. Cycle* fields must be preferred.
type cbMeterAccount struct {
	PackageName                string `json:"PackageName"`
	CapacitySize               int    `json:"CapacitySize"`
	CapacityUsed               int    `json:"CapacityUsed"`
	CapacityRemain             int    `json:"CapacityRemain"`
	CapacitySizePrecise        string `json:"CapacitySizePrecise"`
	CapacityUsedPrecise        string `json:"CapacityUsedPrecise"`
	CapacityRemainPrecise      string `json:"CapacityRemainPrecise"`
	CycleCapacitySize          int    `json:"CycleCapacitySize"`
	CycleCapacityUsed          int    `json:"CycleCapacityUsed"`
	CycleCapacityRemain        int    `json:"CycleCapacityRemain"`
	CycleCapacitySizePrecise   string `json:"CycleCapacitySizePrecise"`
	CycleCapacityUsedPrecise   string `json:"CycleCapacityUsedPrecise"`
	CycleCapacityRemainPrecise string `json:"CycleCapacityRemainPrecise"`
	CycleStartTime             string `json:"CycleStartTime"`
	CycleEndTime               string `json:"CycleEndTime"`
	Status                     int    `json:"Status"`
}

// meterCyclePresent reports whether the API response carried CycleCapacity*
// fields. CycleCapacitySize is the reliable presence probe: real responses
// always carry size > 0 (100/250), while older/partial responses leave it 0.
// Distinguishing "cycle absent" from "cycle exhausted" is impossible from
// remain alone (both give 0) — size is the tiebreaker.
func meterCyclePresent(acc cbMeterAccount) bool {
	if acc.CycleCapacitySize != 0 {
		return true
	}
	// L5: a literal "0" string is NOT evidence of a populated Cycle block —
	// treat it as absent and fall back to the legacy Capacity* fields.
	s := strings.TrimSpace(acc.CycleCapacitySizePrecise)
	if s == "" {
		return false
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil && v == 0 {
		return false
	}
	return true
}

// meterRemain returns the cycle-accurate remaining credits. When Cycle* is
// present (the normal case), CycleCapacityRemain is authoritative and a 0
// means genuinely exhausted — it must NOT fall back to CapacityRemain, which
// stays 100 for exhausted Free Plan accounts. Falls back to legacy Capacity*
// only when the response carries no Cycle fields at all.
func meterRemain(acc cbMeterAccount) float64 {
	if meterCyclePresent(acc) {
		return parseFloatOr(acc.CycleCapacityRemainPrecise, float64(acc.CycleCapacityRemain))
	}
	return parseFloatOr(acc.CapacityRemainPrecise, float64(acc.CapacityRemain))
}

// meterUsed mirrors meterRemain for the used side.
func meterUsed(acc cbMeterAccount) float64 {
	if meterCyclePresent(acc) {
		return parseFloatOr(acc.CycleCapacityUsedPrecise, float64(acc.CycleCapacityUsed))
	}
	return parseFloatOr(acc.CapacityUsedPrecise, float64(acc.CapacityUsed))
}

// meterSize mirrors meterRemain for the plan size.
func meterSize(acc cbMeterAccount) float64 {
	if meterCyclePresent(acc) {
		return parseFloatOr(acc.CycleCapacitySizePrecise, float64(acc.CycleCapacitySize))
	}
	return parseFloatOr(acc.CapacitySizePrecise, float64(acc.CapacitySize))
}

// parseCBMeterAccounts extracts the full Accounts[] list from the nested meter
// response. Shape: {"code":0,"data":{"Response":{"Data":{"Accounts":[...]}}}}
func parseCBMeterAccounts(body []byte) ([]cbMeterAccount, error) {
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
		return nil, fmt.Errorf("cb credit sync parse: %w", err)
	}
	if envelope.Code != 0 {
		return nil, fmt.Errorf("cb credit sync code=%d msg=%s", envelope.Code, envelope.Msg)
	}
	if len(envelope.Data.Response.Data.Accounts) == 0 {
		return nil, fmt.Errorf("cb credit sync: empty Accounts")
	}
	return envelope.Data.Response.Data.Accounts, nil
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

	// sticky sessions: sessionID → bound key (prompt-cache locality)
	sticky   map[string]*stickyBinding
	stickyMu sync.Mutex
}

type stickyBinding struct {
	key      *CBKey
	lastSeen time.Time
}

// stickyTTL is how long a session binding survives without traffic.
const stickyTTL = 30 * time.Minute

func NewCBKeyManager(store *db.Store) *CBKeyManager {
	km := &CBKeyManager{keys: make([]*CBKey, 0), db: store, sticky: make(map[string]*stickyBinding)}
	go km.stickyJanitor()
	return km
}

// stickyJanitor evicts idle bindings so the map doesn't grow unbounded.
func (km *CBKeyManager) stickyJanitor() {
	if km.sticky == nil {
		return
	}
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-stickyTTL)
		km.stickyMu.Lock()
		for sid, b := range km.sticky {
			if b.lastSeen.Before(cutoff) {
				delete(km.sticky, sid)
			}
		}
		km.stickyMu.Unlock()
	}
}

// NextSticky returns the key bound to sessionID. If unbound, or the bound key
// got disabled (credits exhausted etc.), it binds the next round-robin key.
// All requests with the same sessionID hit the same upstream account →
// CodeBuddy prompt-cache stays hot instead of regenerating per request.
func (km *CBKeyManager) NextSticky(sessionID string) (*CBKey, error) {
	if sessionID == "" || km.sticky == nil {
		return km.Next()
	}
	km.stickyMu.Lock()
	if b, ok := km.sticky[sessionID]; ok {
		if !b.key.IsDisabled() {
			b.lastSeen = time.Now()
			km.stickyMu.Unlock()
			return b.key, nil
		}
		delete(km.sticky, sessionID) // bound key died — rebind below
	}
	km.stickyMu.Unlock()

	key, err := km.Next()
	if err != nil {
		return nil, err
	}
	km.stickyMu.Lock()
	km.sticky[sessionID] = &stickyBinding{key: key, lastSeen: time.Now()}
	km.stickyMu.Unlock()
	return key, nil
}

// UnbindSticky drops a session binding (e.g. after permanent disable so the
// next request rebinds to a fresh key instead of the dead one).
func (km *CBKeyManager) UnbindSticky(sessionID string, key *CBKey) {
	if sessionID == "" || km.sticky == nil {
		return
	}
	km.stickyMu.Lock()
	if b, ok := km.sticky[sessionID]; ok && b.key == key {
		delete(km.sticky, sessionID)
	}
	km.stickyMu.Unlock()
}

// StickyCount reports active session bindings (dashboard/debug).
func (km *CBKeyManager) StickyCount() int {
	km.stickyMu.Lock()
	defer km.stickyMu.Unlock()
	return len(km.sticky)
}

// ---------------------------------------------------------------------------
// Key selection modes (router-level strategy, runtime-configurable)
// ---------------------------------------------------------------------------

// CBSelectorMode picks how ProxyCodeBuddy chooses the upstream key.
type CBSelectorMode string

const (
	// SelectorRR — classic round-robin over enabled keys (no cache locality).
	SelectorRR CBSelectorMode = "rr"
	// SelectorSticky — session-id header binds conversation to one key until
	// it dies (per-conversation cache locality; different sessions may land on
	// different keys, re-caching the shared system prompt each time).
	SelectorSticky CBSelectorMode = "sticky"
	// SelectorContentHash — hash(model + first system message) deterministically
	// maps to one key: every session sharing the same system prompt lands on
	// the same account → giant shared prefix cached once.
	SelectorContentHash CBSelectorMode = "content-hash"
	// SelectorHybrid — content-hash picks a small bucket (~3 keys); session-id
	// sticks to one key inside the bucket. Dead keys rebind within the bucket,
	// keeping the shared system-prompt cache warm.
	SelectorHybrid CBSelectorMode = "hybrid"
)

// hybridBucketSize is how many enabled keys form one hybrid bucket.
const hybridBucketSize = 3

// selectorMode holds the active mode (atomic for lock-free hot-path reads).
var selectorMode atomic.Value // stores CBSelectorMode

func init() {
	m := CBSelectorMode(os.Getenv("CB_SELECTOR_MODE"))
	if !validSelectorMode(m) {
		m = SelectorSticky // default: sticky sessions (prompt-cache locality)
	}
	selectorMode.Store(m)
}

func validSelectorMode(m CBSelectorMode) bool {
	switch m {
	case SelectorRR, SelectorSticky, SelectorContentHash, SelectorHybrid:
		return true
	}
	return false
}

// GetSelectorMode returns the active CB key selection mode.
func GetSelectorMode() CBSelectorMode { return selectorMode.Load().(CBSelectorMode) }

// SetSelectorMode validates + stores the mode and persists it to Redis
// (cb:config hash) so restarts keep the operator's choice.
func SetSelectorMode(store *db.Store, m CBSelectorMode) error {
	if !validSelectorMode(m) {
		return fmt.Errorf("invalid selector mode %q (valid: rr|sticky|content-hash|hybrid)", m)
	}
	selectorMode.Store(m)
	if store != nil {
		if err := store.SetCBConfig("selector_mode", string(m)); err != nil {
			slog.Warn("selector mode persist failed", "module", "cb", "error", err)
		}
	}
	slog.Info("cb selector mode changed", "module", "cb", "mode", m)
	return nil
}

// LoadSelectorMode restores the persisted mode from Redis (called at startup).
func LoadSelectorMode(store *db.Store) {
	if store == nil {
		return
	}
	if v, err := store.GetCBConfig("selector_mode"); err == nil && validSelectorMode(CBSelectorMode(v)) {
		selectorMode.Store(CBSelectorMode(v))
		slog.Info("cb selector mode restored", "module", "cb", "mode", v)
	}
}

// NextForMode selects a key according to the active mode.
//   - sessionID: from x-session-id/x-conversation-id/x-chat-id header (may be "")
//   - sysHash:   hash of model + first system message content (may be "")
func (km *CBKeyManager) NextForMode(mode CBSelectorMode, sessionID, sysHash string) (*CBKey, error) {
	switch mode {
	case SelectorRR:
		return km.Next()
	case SelectorContentHash:
		return km.nextByHash(sysHash)
	case SelectorHybrid:
		return km.nextHybrid(sessionID, sysHash)
	case SelectorSticky:
		fallthrough
	default:
		if sessionID != "" {
			return km.NextSticky(sessionID)
		}
		return km.Next()
	}
}

// nextByHash: deterministic key from sysHash (FNV-1a over enabled keys).
// All sessions with the same system prompt land on the same account.
func (km *CBKeyManager) nextByHash(sysHash string) (*CBKey, error) {
	km.mu.RLock()
	enabled := make([]*CBKey, 0, len(km.keys))
	for _, k := range km.keys {
		if !k.IsDisabled() {
			enabled = append(enabled, k)
		}
	}
	km.mu.RUnlock()
	if len(enabled) == 0 {
		return nil, fmt.Errorf("all cb keys disabled")
	}
	if sysHash == "" {
		return km.Next()
	}
	h := fnv.New64a()
	h.Write([]byte(sysHash))
	return enabled[h.Sum64()%uint64(len(enabled))], nil
}

// nextHybrid: content-hash selects a bucket of hybridBucketSize enabled keys;
// session-id sticks to one key inside the bucket (rebinding within the bucket
// when the key dies → shared system-prompt cache stays warm).
func (km *CBKeyManager) nextHybrid(sessionID, sysHash string) (*CBKey, error) {
	km.mu.RLock()
	enabled := make([]*CBKey, 0, len(km.keys))
	for _, k := range km.keys {
		if !k.IsDisabled() {
			enabled = append(enabled, k)
		}
	}
	km.mu.RUnlock()
	if len(enabled) == 0 {
		return nil, fmt.Errorf("all cb keys disabled")
	}
	if sysHash == "" {
		if sessionID != "" {
			return km.NextSticky(sessionID)
		}
		return km.Next()
	}

	// Bucket = consecutive slice of enabled keys starting at hash position.
	h := fnv.New64a()
	h.Write([]byte(sysHash))
	start := int(h.Sum64() % uint64(len(enabled)))
	bucket := make([]*CBKey, 0, hybridBucketSize)
	for i := 0; i < hybridBucketSize && i < len(enabled); i++ {
		bucket = append(bucket, enabled[(start+i)%len(enabled)])
	}

	// Session binding within bucket (reuse sticky map — but only accept the
	// bound key if it is in THIS bucket; otherwise rebind into the bucket).
	if sessionID != "" {
		km.stickyMu.Lock()
		if b, ok := km.sticky[sessionID]; ok && !b.key.IsDisabled() {
			for _, k := range bucket {
				if k == b.key {
					b.lastSeen = time.Now()
					km.stickyMu.Unlock()
					return k, nil
				}
			}
		}
		delete(km.sticky, sessionID)
		// bind first bucket key (deterministic per session: hash sessionID)
		sh := fnv.New64a()
		sh.Write([]byte(sessionID))
		pick := bucket[sh.Sum64()%uint64(len(bucket))]
		km.sticky[sessionID] = &stickyBinding{key: pick, lastSeen: time.Now()}
		km.stickyMu.Unlock()
		return pick, nil
	}
	return bucket[0], nil
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
		// Build all keys into a local slice, then swap under lock — avoids
		// data race with hot-path readers (Next, ResolveKey, nextByHash…)
		// that hold km.mu.RLock while iterating km.keys.
		loaded := make([]*CBKey, 0, len(redisState))
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
				key.disabledReason = state["disabled_reason"]
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
				// I2: legacy rows (pre-provenance) carry empty reason + zero
				// timestamp — they were meter-driven disables. Tag them so the
				// H3 auto-lift restores the pre-upgrade self-healing behavior.
				// Operator disables always persisted a reason via DisableKey.
				if key.disabled && key.disabledReason == "" && key.disabledAt.IsZero() {
					key.disabledReason = cbMeterDisableReason
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
			loaded = append(loaded, key)
		}
		km.mu.Lock()
		km.keys = loaded
		km.mu.Unlock()
		slog.Info("loaded keys from Redis", "module", "cb", "count", len(loaded))
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
		// Snapshot mutable fields under k.mu to avoid race with
		// AddOAuthAccount which writes CredType/Email under k.mu only.
		k.mu.RLock()
		keyVal := k.Key
		ct := k.CredType
		email := k.Email
		k.mu.RUnlock()

		if keyVal == maskedOrFull {
			return keyVal
		}
		// OAuth: also match by Email field
		if ct == CBAuthOAuth && email == maskedOrFull {
			return keyVal
		}
		// Check masked form: first 8 + "..." + last 4 (API keys)
		if len(keyVal) > 12 {
			masked := keyVal[:8] + "..." + keyVal[len(keyVal)-4:]
			if masked == maskedOrFull {
				return keyVal
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
// Pass a context cancelled on SIGTERM for clean shutdown.
func ReenableCBWorker(ctx context.Context, km *CBKeyManager) {
	km.ReenableCooldowns()
	ticker := time.NewTicker(REENABLE_TICK)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			km.ReenableCooldowns()
		}
	}
}

// CBOAuthRefreshWorker pre-warms OAuth access tokens before they expire.
// Mirrors Grok's AutoRefreshWorker: every PRE_WARM_TICK, scan OAuth keys
// within PRE_WARM_WINDOW of expiry, refresh with MAX_CONCURRENT_REFRESH cap.
// Pass a context cancelled on SIGTERM for clean shutdown.
func CBOAuthRefreshWorker(ctx context.Context, km *CBKeyManager) {
	ticker := time.NewTicker(PRE_WARM_TICK)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
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
// Pass a context cancelled on SIGTERM for clean shutdown.
func CBCreditSyncWorker(ctx context.Context, km *CBKeyManager) {
	// Immediate first pass with small stagger so we don't stampede on boot.
	syncAllCBCredits(km, true)

	ticker := time.NewTicker(CB_CREDIT_SYNC_TICK)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			syncAllCBCredits(km, false)
		}
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
	// Router-side default output cap: if the client sent NO cap at all
	// (neither max_tokens nor max_completion_tokens), set a sane default.
	// Reasoning models (claude-opus-5, gpt-5.x) burn output tokens on
	// reasoning_content first — with no cap, upstream defaults can truncate
	// the visible answer to reasoning-only. 32768 leaves room for both.
	if _, ok := m["max_completion_tokens"]; !ok {
		m["max_completion_tokens"] = 32768
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
	// Content-item sanitation: CodeBuddy 400 11101 "missing type field in
	// content item at index N" when a content array contains an item without
	// a "type" key (e.g. Anthropic-style {text:"..."} blocks). Drop such
	// items so a malformed client body fails cleanly instead of cascading.
	if msgs, ok := m["messages"].([]any); ok {
		for _, msg := range msgs {
			mm, ok := msg.(map[string]any)
			if !ok {
				continue
			}
			content, ok := mm["content"].([]any)
			if !ok {
				continue
			}
			filtered := make([]any, 0, len(content))
			for _, item := range content {
				im, isObj := item.(map[string]any)
				if !isObj || im["type"] != nil {
					filtered = append(filtered, item)
				}
			}
			if len(filtered) != len(content) {
				mm["content"] = filtered
			}
		}
		m["messages"] = msgs
	}

	// Multi-turn tool-history collapse. claude-opus-5 handles ordinary
	// agent-loop transcripts fine — verified live 2026-08-14: an 18-msg /
	// 800KB / 27-tool / reasoning_effort:high request returns normal
	// tool_calls, not reasoning-only. The failure mode appears only on
	// degenerate transcripts (very deep history with malformed trailing
	// assistant turns, content arrays without a type field). Collapsing
	// early is harmful: it forces a premature final answer ("I was cut
	// off") after the first tool round. So collapse is a SAFETY NET for
	// deep histories only (>16 msgs = 8+ tool rounds). The official CLI
	// manages session state itself and sends lean per-turn messages;
	// mirroring that only kicks in where upstream would otherwise choke.
	// Gated to claude-opus-5 — opus-4.7-1m and other models handle
	// multi-turn tool history fine and must NOT be collapsed.
	if model, ok := m["model"].(string); ok && isOpus5Model(model) {
		if msgs, ok := m["messages"].([]any); ok && len(msgs) > 16 && hasToolHistory(msgs) {
			m["messages"] = collapseToolHistory(msgs)
			// Drop tools when collapsing: the transcript already carries the
			// tool results, and opus-5 with tools + collapsed history gets
			// stuck in tool-calling mode (0 content, stream never finishes).
			// Verified live: collapsed-only → 9775ch full answer; +tools → 0.
			delete(m, "tools")
			// M1: tool_choice / parallel_tool_calls reference the deleted tool
			// list — OpenAI-compatible upstreams 400 on tool_choice w/o tools.
			delete(m, "tool_choice")
			delete(m, "parallel_tool_calls")
			// Drop the nested reasoning object + output caps too: opus-5 goes
			// reasoning-only when reasoning_effort AND max_completion_tokens
			// are both present on a collapsed request (verified live:
			// effort+maxcomp → 374ch; effort-only → 23834ch). The CLI sends
			// neither cap — mirror that.
			delete(m, "reasoning")
			delete(m, "max_completion_tokens")
			// M2: legacy/Anthropic-style clients send `max_tokens` — same cap
			// the comment above says must be removed.
			delete(m, "max_tokens")
			slog.Debug("collapsed multi-turn tool history for opus-5",
				"module", "cb", "orig_msgs", len(msgs))
		}
	}

	// Normalize reasoning params. CodeBuddy upstream only accepts
	// `reasoning_effort` (flat string: low/medium/high/xhigh). Clients send
	// different shapes:
	//   Hermes/OpenRouter: extra_body.reasoning = {enabled:true, effort:"high"}
	//   DeepSeek: thinking = {type:"enabled"}
	//   Qwen/ZAI: enable_thinking = true
	// Translate all of them to reasoning_effort if not already set.
	if _, hasRE := m["reasoning_effort"]; !hasRE {
		if r, ok := m["reasoning"].(map[string]any); ok {
			if enabled, ok := r["enabled"].(bool); ok && !enabled {
				// explicitly disabled — skip
			} else if effort, ok := r["effort"].(string); ok && effort != "" {
				m["reasoning_effort"] = effort
			} else {
				m["reasoning_effort"] = "medium"
			}
		} else if t, ok := m["thinking"].(map[string]any); ok {
			if tp, ok := t["type"].(string); ok && tp == "enabled" {
				m["reasoning_effort"] = "medium"
			}
		} else if et, ok := m["enable_thinking"].(bool); ok && et {
			m["reasoning_effort"] = "medium"
		}
	}
	// Also check nested extra_body.reasoning (OpenAI SDK wraps extra_body fields)
	if eb, ok := m["extra_body"].(map[string]any); ok {
		if _, hasRE := m["reasoning_effort"]; !hasRE {
			if r, ok := eb["reasoning"].(map[string]any); ok {
				if enabled, ok := r["enabled"].(bool); ok && !enabled {
					// explicitly disabled — skip
				} else if effort, ok := r["effort"].(string); ok && effort != "" {
					m["reasoning_effort"] = effort
				} else {
					m["reasoning_effort"] = "medium"
				}
			}
		}
	}
	return json.Marshal(m)
}

// hasToolHistory reports whether a message list contains a multi-turn
// assistant/tool exchange (role=tool, or assistant with non-empty tool_calls).
// isOpus5Model reports whether the model is claude-opus-5 in any form the
// router may carry (cb/ prefix, -1m suffix, case variants). Exact-match
// gating silently skipped those forms, re-exposing the collapse bug.
func isOpus5Model(model string) bool {
	n := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "cb/")
	return strings.HasPrefix(n, "claude-opus-5")
}

func hasToolHistory(messages []any) bool {
	for _, msg := range messages {
		mm, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		if mm["role"] == "tool" {
			return true
		}
		if mm["role"] == "assistant" {
			if tcs, ok := mm["tool_calls"].([]any); ok && len(tcs) > 0 {
				return true
			}
		}
	}
	return false
}

// collapseToolHistory flattens a multi-turn transcript into a single user
// message, mirroring how the official CodeBuddy CLI manages conversation
// state (it never dumps full history in one request). claude-opus-5 turns
// reasoning-only on large multi-turn tool histories — collapsing restores a
// final answer.
//
// System messages are preserved as a separate system turn: CodeBuddy rejects
// requests without a system message (11101 "Parse message failed"), and
// cbTransform's system injection runs BEFORE this collapse in the pipeline.
func collapseToolHistory(messages []any) []any {
	var sysParts []string
	var rest []any
	for _, msg := range messages {
		mm, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		if mm["role"] == "system" {
			if c := renderContentText(mm["content"]); c != "" {
				sysParts = append(sysParts, c)
			}
			continue
		}
		rest = append(rest, msg)
	}
	out := make([]any, 0, 2)
	if len(sysParts) > 0 {
		out = append(out, map[string]any{"role": "system", "content": strings.Join(sysParts, "\n\n")})
	}
	var sb strings.Builder
	for _, msg := range rest {
		mm, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		switch role {
		case "user":
			if c := renderContentText(mm["content"]); c != "" {
				sb.WriteString("[User]\n")
				sb.WriteString(c)
				sb.WriteString("\n\n")
			}
		case "assistant":
			if tcs, ok := mm["tool_calls"].([]any); ok && len(tcs) > 0 {
				for _, tc := range tcs {
					tcm, ok := tc.(map[string]any)
					if !ok {
						continue
					}
					fn, _ := tcm["function"].(map[string]any)
					name, _ := fn["name"].(string)
					args, _ := fn["arguments"].(string)
					sb.WriteString("[Assistant tool call] ")
					sb.WriteString(name)
					sb.WriteString("(")
					sb.WriteString(args)
					sb.WriteString(")\n\n")
				}
			} else if c := renderContentText(mm["content"]); c != "" {
				sb.WriteString("[Assistant]\n")
				sb.WriteString(c)
				sb.WriteString("\n\n")
			}
		case "tool":
			if c := renderContentText(mm["content"]); c != "" {
				sb.WriteString("[Tool result]\n")
				sb.WriteString(c)
				sb.WriteString("\n\n")
			}
		}
	}
	sb.WriteString("Based on the full conversation above, give your final answer now.")
	out = append(out, map[string]any{"role": "user", "content": sb.String()})
	return out
}

// renderContentText renders a message content field to plain text: string
// passthrough, content arrays → joined text items, anything else → "".
func renderContentText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, item := range c {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := im["type"].(string); ok && t == "text" {
				if txt, ok := im["text"].(string); ok {
					sb.WriteString(txt)
					sb.WriteString("\n")
				}
			}
		}
		return sb.String()
	default:
		return ""
	}
}

// cbCollectStream: read SSE stream → return single JSON (for non-stream clients).
func cbCollectStream(resp *http.Response, model string, key *CBKey) gin.H {
	defer resp.Body.Close()
	var content, reasoning strings.Builder
	var finish string
	var usage map[string]any
	var credit float64
	// toolCalls accumulates streamed delta.tool_calls (keyed by index),
	// merging partial function.arguments chunks like the OpenAI SDK expects.
	toolCalls := map[int]*struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}{}
	var toolIndexOrder []int

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
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
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
			for _, tc := range chunk.Choices[0].Delta.ToolCalls {
				cur, ok := toolCalls[tc.Index]
				if !ok {
					cur = &struct {
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					}{}
					toolCalls[tc.Index] = cur
					toolIndexOrder = append(toolIndexOrder, tc.Index)
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				if tc.Type != "" {
					cur.Type = tc.Type
				}
				if tc.Function.Name != "" {
					cur.Function.Name = tc.Function.Name
				}
				cur.Function.Arguments += tc.Function.Arguments
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
	// Surface reasoning_content when the upstream emitted it (thinking enabled
	// via reasoning_effort/enable_thinking) — clients like Hermes display it.
	if r := reasoning.String(); r != "" {
		resp2["choices"].([]gin.H)[0]["message"].(gin.H)["reasoning_content"] = r
	}
	// Re-attach streamed tool_calls (OpenAI delta format) so non-stream clients
	// see the full tool call. Without this, finish_reason="tool_calls" arrives
	// with an empty message → clients treat it as an empty response.
	if len(toolCalls) > 0 {
		sort.Slice(toolIndexOrder, func(i, j int) bool { return toolIndexOrder[i] < toolIndexOrder[j] })
		tcs := make([]gin.H, 0, len(toolIndexOrder))
		for _, idx := range toolIndexOrder {
			tc := toolCalls[idx]
			tcs = append(tcs, gin.H{
				"id":       tc.ID,
				"type":     tc.Type,
				"function": gin.H{"name": tc.Function.Name, "arguments": tc.Function.Arguments},
			})
		}
		resp2["choices"].([]gin.H)[0]["message"].(gin.H)["tool_calls"] = tcs
	}
	if usage != nil {
		resp2["usage"] = usage
	}
	return resp2
}

// permanentDisable marks a key permanently disabled and persists via toDTO.
// The reason is persisted too (H3 provenance): meter/exhaustion disables must
// pass cbMeterDisableReason so the credit-sync worker can auto-lift them when
// the meter recovers; auth-failure reasons (401 etc.) fail closed forever.
func permanentDisable(key *CBKey, reason string) {
	key.mu.Lock()
	key.disabled = true
	key.disabledAt = time.Time{}
	key.disabledReason = reason
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

	// Sticky session: client may pin all requests in a conversation to the
	// same upstream key via header (prompt-cache locality). First header wins:
	// x-session-id, x-conversation-id, x-chat-id.
	sessionID := c.GetHeader("x-session-id")
	if sessionID == "" {
		sessionID = c.GetHeader("x-conversation-id")
	}
	if sessionID == "" {
		sessionID = c.GetHeader("x-chat-id")
	}
	if len(sessionID) > 128 {
		sessionID = sessionID[:128]
	}

	// sysHash: identifies the shared prompt prefix (model + first system
	// message) for content-hash / hybrid selection modes. Computed from the
	// already-transformed body so the prefix matches what upstream sees.
	sysHash := ""
	{
		var tm map[string]any
		if json.Unmarshal(transformed, &tm) == nil {
			if model, _ := tm["model"].(string); model != "" {
				sysHash = model
			}
			if msgs, ok := tm["messages"].([]any); ok && len(msgs) > 0 {
				if first, ok := msgs[0].(map[string]any); ok && first["role"] == "system" {
					if sc, ok := first["content"].(string); ok {
						sysHash += "|" + sc
					}
				}
			}
		}
	}
	mode := GetSelectorMode()

	var lastResp *http.Response
	var lastKey *CBKey
	reqStart := time.Now()

	// Mode-selected key gets the first attempts before falling back to RR.
	// RR mode: no preference — every attempt is plain round-robin.
	stickyAttempts := 0
	if mode != SelectorRR {
		stickyAttempts = 2
	}

	// Safety cap: don't walk the whole pool on key-level failures (401/403/429).
	// After 6 consecutive key failures report the last error instead — prevents
	// a full-pool disable storm and bounds per-request upstream calls.
	const maxKeyAttempts = 6
	var lastFailure string
	for attempt := 0; attempt < total+stickyAttempts; attempt++ {
		if attempt >= maxKeyAttempts && lastFailure != "" {
			c.JSON(503, gin.H{"error": "cb keys failing", "detail": lastFailure})
			c.Set("error_msg", lastFailure)
			errJSON, _ := json.Marshal(gin.H{"error": "cb keys failing", "detail": lastFailure})
			c.Set("response_body", json.RawMessage(errJSON))
			return
		}
		// C10: bail out early if the client cancelled — don't walk the
		// whole key list burning upstream calls for a dead request.
		if err := c.Request.Context().Err(); err != nil {
			slog.Debug("client cancelled before attempt", "module", "cb", "attempt", attempt+1, "error", err)
			return
		}
		var key *CBKey
		var err error
		if attempt < stickyAttempts {
			key, err = km.NextForMode(mode, sessionID, sysHash)
		} else {
			key, err = km.Next()
		}
		if err != nil {
			break
		}

		// OAuth: refresh if near-expiry before building the request.
		if err := key.EnsureValid(); err != nil {
			slog.Warn("ensure valid failed", "module", "cb", "key", key.DisplayID(), "error", err)
			// Fall through — try with existing token; 401 path may still refresh.
		}

		req, _ := http.NewRequestWithContext(c.Request.Context(), "POST", CB_UPSTREAM_URL, bytes.NewReader(transformed))
		cbChatHeaders(req, key)

		resp, err := client.Do(req)
		if err != nil {
			lastFailure = "network error: " + err.Error()
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
					lastFailure = "401 oauth refresh failed"
					permanentDisable(key, "401 oauth refresh failed: "+refreshErr.Error())
					km.UnbindSticky(sessionID, key)
					continue
				}
				// Rebuild request with fresh AT
				req, _ = http.NewRequestWithContext(c.Request.Context(), "POST", CB_UPSTREAM_URL, bytes.NewReader(transformed))
				cbChatHeaders(req, key)
				resp, err = client.Do(req)
				if err != nil {
					lastFailure = "network error after refresh: " + err.Error()
					markProxyResult(proxyID, err, 0)
					continue
				}
				markProxyResult(proxyID, nil, resp.StatusCode)
				if resp.StatusCode == 401 {
					resp.Body.Close()
					lastFailure = "401 after oauth refresh"
					permanentDisable(key, "401 after oauth refresh, permanent")
					km.UnbindSticky(sessionID, key)
					continue
				}
				// Fall through to process non-401 response below.
			} else {
				lastFailure = "401 unauthorized"
				permanentDisable(key, "401 unauthorized, permanent")
				km.UnbindSticky(sessionID, key)
				continue
			}
		}

		if resp.StatusCode == 429 {
			// Read body for 429 to distinguish trial-not-activated (14017) and
			// credits-exhausted (14018) — both permanent — from rate limiting.
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			bodyStr := string(bodyBytes)
			resp.Body.Close()
			lastFailure = "429: " + truncateLog(bodyStr, 120)
			switch {
			case strings.Contains(bodyStr, "14017"):
				lastFailure = "429 trial not activated (14017), permanent"
				permanentDisable(key, "429 trial not activated, permanent")
				km.UnbindSticky(sessionID, key)
			case strings.Contains(bodyStr, "14018") || strings.Contains(bodyStr, "Credits exhausted"):
				lastFailure = "credits exhausted (14018)"
				// I1: exhaustion is meter-driven — tag with the meter reason so
				// the key auto-lifts when a later sync reports credits.
				permanentDisable(key, cbMeterDisableReason)
				km.UnbindSticky(sessionID, key) // session rebinds to fresh key next request
			default:
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
				lastFailure = "403 request illegal (banned)"
				permanentDisable(key, "403 request illegal, banned, permanent")
				km.UnbindSticky(sessionID, key)
				continue
			}
			if strings.Contains(bodyStr, "14018") || strings.Contains(bodyStr, "Credits exhausted") {
				lastFailure = "credits exhausted"
				permanentDisable(key, "credits exhausted, code 14018")
				km.UnbindSticky(sessionID, key) // session rebinds to fresh key next request
				continue
			}
			// ALL 400s are client request errors (bad body, bad params, unknown
			// model, any 111xx code — past, present, future). NEVER a key
			// problem. Return directly: no disable, no retry on the next key.
			// Rotating on a request-side error cascades a single malformed
			// request into disabling the entire pool (the 11101 cascade).
			if resp.StatusCode == 400 {
				hc.CB.RecordRequest(time.Since(reqStart), fmt.Errorf("cb 400 client error"))
				c.JSON(400, gin.H{"error": "CodeBuddy rejected request", "detail": truncateLog(bodyStr, 500)})
				c.Set("error_msg", truncateLog(bodyStr, 500))
				errJSON, _ := json.Marshal(gin.H{"error": "CodeBuddy rejected request", "detail": truncateLog(bodyStr, 500)})
				c.Set("response_body", json.RawMessage(errJSON))
				return
			}
			// Any other 4xx (404, 422, 408, unknown codes…) is also a
			// client/endpoint error, not a key problem. Same rule: return it,
			// never rotate-and-disable the pool. Only 401/403/429 above touch
			// key state (auth/banned/rate — all genuinely key-specific).
			c.JSON(resp.StatusCode, gin.H{"error": "CodeBuddy request failed", "detail": truncateLog(bodyStr, 500)})
			c.Set("error_msg", truncateLog(bodyStr, 500))
			errJSON, _ := json.Marshal(gin.H{"error": "CodeBuddy request failed", "detail": truncateLog(bodyStr, 500)})
			c.Set("response_body", json.RawMessage(errJSON))
			return
		}

		if resp.StatusCode >= 500 {
			resp.Body.Close()
			lastFailure = fmt.Sprintf("upstream %d", resp.StatusCode)
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
		copyUpstreamHeaders(c.Writer.Header(), lastResp.Header)
		c.Writer.WriteHeader(lastResp.StatusCode)
		flusher, _ := c.Writer.(http.Flusher)
		scanner := bufio.NewScanner(lastResp.Body)
		scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
		// C6: same as grok — a client disconnect must stop the stream loop
		// promptly so we don't keep pulling from upstream forever.
		ctx := c.Request.Context()
		var streamContent strings.Builder
		var streamReasoning strings.Builder
		var streamTokensIn, streamTokensOut int
		var streamUsage map[string]any // last chunk's full usage (has cache fields)
		var streamFinish string        // real finish_reason from upstream (may be "tool_calls")
		// tool_calls accumulation keyed by index (mirrors cbCollectStream).
		var streamToolCalls = map[int]*struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}{}
		var streamToolOrder []int
		var streamChunks int
		for scanner.Scan() {
			streamChunks++
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
							// Keep the LAST non-nil usage (final chunk has real
							// token counts + cache hit fields). Intermediate
							// chunks may have all-zero usage.
							streamUsage = sc.Usage
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
							streamReasoning.WriteString(sc.Choices[0].Delta.ReasoningContent)
							if fr := sc.Choices[0].FinishReason; fr != "" {
								streamFinish = fr
							}
							for _, tc := range sc.Choices[0].Delta.ToolCalls {
								cur, ok := streamToolCalls[tc.Index]
								if !ok {
									cur = &struct {
										ID       string `json:"id"`
										Type     string `json:"type"`
										Function struct {
											Name      string `json:"name"`
											Arguments string `json:"arguments"`
										} `json:"function"`
									}{}
									streamToolCalls[tc.Index] = cur
									streamToolOrder = append(streamToolOrder, tc.Index)
								}
								if tc.ID != "" {
									cur.ID = tc.ID
								}
								if tc.Type != "" {
									cur.Type = tc.Type
								}
								if tc.Function.Name != "" {
									cur.Function.Name = tc.Function.Name
								}
								cur.Function.Arguments += tc.Function.Arguments
							}
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
		slog.Debug("sse loop: ended",
			"module", "cb",
			"chunks", streamChunks,
			"scanner_err", func() string {
				if scanner.Err() != nil {
					return scanner.Err().Error()
				}
				return ""
			}(),
			"ctx_err", func() string {
				if ctx.Err() != nil {
					return ctx.Err().Error()
				}
				return ""
			}(),
			"finish", streamFinish,
			"content_len", streamContent.Len())
		c.Set("output_text", truncateLog(streamContent.String(), 1000))
		c.Set("tokens_in", streamTokensIn)
		c.Set("tokens_out", streamTokensOut)
		// Build the stored response_body. Use the full upstream usage map (which
		// contains prompt_cache_hit_tokens etc.) instead of a minimal 3-field
		// reconstruction — extractCacheHitPct needs the cache fields.
		respUsage := streamUsage
		if respUsage == nil {
			respUsage = gin.H{
				"prompt_tokens":     streamTokensIn,
				"completion_tokens": streamTokensOut,
				"total_tokens":      streamTokensIn + streamTokensOut,
			}
		}
		msg := gin.H{"role": "assistant", "content": streamContent.String()}
		if r := streamReasoning.String(); r != "" {
			msg["reasoning_content"] = r
		}
		if len(streamToolOrder) > 0 {
			tcs := make([]gin.H, 0, len(streamToolOrder))
			for _, idx := range streamToolOrder {
				cur := streamToolCalls[idx]
				tcs = append(tcs, gin.H{
					"id":   cur.ID,
					"type": cur.Type,
					"function": gin.H{
						"name":      cur.Function.Name,
						"arguments": cur.Function.Arguments,
					},
				})
			}
			msg["tool_calls"] = tcs
		}
		finish := streamFinish
		if finish == "" {
			finish = "stop"
		}
		respJSON, _ := json.Marshal(gin.H{
			"choices": []gin.H{{
				"message":       msg,
				"finish_reason": finish,
			}},
			"usage":  respUsage,
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

// DisableKey permanently disables a key (disabledAt = zero time) and persists
// the state + reason to Redis via SaveCBKey, so the disable survives restarts
// and is never overwritten by the credit-sync worker. Returns false if the key
// is unknown. Used by POST /cb/keys/disable (dashboard + sweep tooling).
func (km *CBKeyManager) DisableKey(key, reason string) bool {
	km.mu.RLock()
	var found *CBKey
	for _, k := range km.keys {
		if k.Key == key {
			found = k
			break
		}
	}
	km.mu.RUnlock()
	if found == nil {
		return false
	}
	found.mu.Lock()
	found.disabled = true
	found.disabledAt = time.Time{} // permanent — never auto-reenabled
	found.disabledReason = reason
	found.mu.Unlock()
	if found.db != nil {
		saveCBKey(found.db, found.toDTO())
	} else {
		// M5: memory-only change — lost on restart. Make it visible.
		slog.Warn("disable persisted to memory only (no store)", "module", "cb", "key", maskCBKey(key))
	}
	slog.Info("disabled cb key", "module", "cb", "key", maskCBKey(key), "reason", reason)
	return true
}

// EnableKey re-enables a previously disabled key (clears disabled flag and
// reason, persists via SaveCBKey so the state is consistent in memory + Redis).
// Returns false if the key is unknown. Used by POST /cb/keys/enable.
func (km *CBKeyManager) EnableKey(key string) bool {
	km.mu.RLock()
	var found *CBKey
	for _, k := range km.keys {
		if k.Key == key {
			found = k
			break
		}
	}
	km.mu.RUnlock()
	if found == nil {
		return false
	}
	found.mu.Lock()
	found.disabled = false
	found.disabledAt = time.Time{}
	found.disabledReason = ""
	found.mu.Unlock()
	if found.db != nil {
		saveCBKey(found.db, found.toDTO())
	} else {
		// M5: memory-only change — lost on restart. Make it visible.
		slog.Warn("enable persisted to memory only (no store)", "module", "cb", "key", maskCBKey(key))
	}
	slog.Info("enabled cb key", "module", "cb", "key", maskCBKey(key))
	return true
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
