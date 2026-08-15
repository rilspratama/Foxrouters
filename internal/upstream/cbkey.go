package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"foxrouters/internal/db"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"
)

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
