package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"foxrouters/internal/db"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

var refreshSF singleflight.Group

// TokenRefreshDisabled gates ALL token refresh paths (background workers,
// on-demand EnsureValid, 401-retry refresh). Set TOKEN_REFRESH_DISABLED=1 in
// dev: dev Redis is seeded from prod, so any dev-side refresh rotates the RT
// upstream and invalidates prod's copy → invalid_grant → permanent disable.
var TokenRefreshDisabled = os.Getenv("TOKEN_REFRESH_DISABLED") == "1"

type GrokAccount struct {
	Email        string `json:"email"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token,omitempty"`
	ExpiresIn    int    `json:"expires_in"`
	Expired      string `json:"expired"`
	LastRefresh  string `json:"last_refresh"`
	Sub          string `json:"sub"`
	// Image generation (console.x.ai DPoP): SSO cookies from browser login.
	// Password is used for lazy pure-HTTP re-login when the cookie dies.
	SSO              string `json:"sso,omitempty"`
	SSORW            string `json:"sso_rw,omitempty"`
	Password         string `json:"-"`
	mu               sync.RWMutex
	expiresAt        time.Time
	disabled         bool
	disabledAt       time.Time
	db               *db.Store
	imgCooldownUntil time.Time // skip image-gen until this time (429 resource-exhausted)
	vidCooldownUntil time.Time // skip video-gen until this time (429 resource-exhausted)

	// Free-tier quota from GET /v1/usage (console.x.ai): chat 10, image 5, video 2
	imgQuotaUsed   int64
	imgQuotaLimit  int64
	vidQuotaUsed   int64
	vidQuotaLimit  int64
	chatQuotaUsed  int64
	chatQuotaLimit int64
	quotaSyncedAt  time.Time

	// Billing fields (populated by SyncBilling, persisted to Redis)
	billingSyncedAt time.Time
	periodStart     string // currentPeriod.start
	periodEnd       string // currentPeriod.end (weekly reset)
	periodType      string // "USAGE_PERIOD_TYPE_WEEKLY"
	onDemandCap     int64  // PAYG cap in cents (0 = not enabled / free tier)
	onDemandUsed    int64  // PAYG used in cents
	prepaidBalance  int64  // purchased credits in cents
	unifiedBilling  bool   // isUnifiedBillingUser

	// Per-account cumulative token usage (accumulated from response usage field)
	// Free-tier quota = 1,000,000 tokens (down from 2M as of Aug 2026).
	tokensUsed       int64     // cumulative total_tokens (prompt + completion + reasoning)
	promptTokens     int64     // cumulative prompt_tokens (includes cached)
	completionTokens int64     // cumulative completion_tokens
	usageResetAt     time.Time // when the weekly period last reset (from billing sync)
}

// NewGrokAccountForTest returns a bare GrokAccount for whitebox tests.
// Fields consumed by tests (expiresAt, disabled, disabledAt) are settable
// via functional options.
func NewGrokAccountForTest(email, access, refresh string, opts ...GrokAccountOption) *GrokAccount {
	a := &GrokAccount{Email: email, AccessToken: access, RefreshToken: refresh, expiresAt: time.Now().Add(time.Hour)}
	for _, o := range opts {
		o(a)
	}
	return a
}

// GrokAccountOption mutates a test-only GrokAccount.
type GrokAccountOption func(*GrokAccount)

// WithExpiresAt sets the internal expiry stamp.
func WithExpiresAt(t time.Time) GrokAccountOption { return func(a *GrokAccount) { a.expiresAt = t } }

// WithDisabledCooldown marks the account disabled with a timestamp (cooldown).
// Passing a zero time signals a permanent disable.
func WithDisabledCooldown(at time.Time) GrokAccountOption {
	return func(a *GrokAccount) { a.disabled = true; a.disabledAt = at }
}

func (a *GrokAccount) IsExpired() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return time.Now().After(a.expiresAt.Add(-REFRESH_BUFFER))
}

// GrokAccountSnapshot is a value copy of a GrokAccount for read-only use
// (handlers, metrics). Fields mirror what the /accounts response needs.
type GrokAccountSnapshot struct {
	Email        string
	Sub          string
	AccessToken  string
	RefreshToken string
	IDToken      string
	Expired      string
	ExpiresIn    int
	ExpiresAt    time.Time
	LastRefresh  string
	Disabled     bool
	DisabledAt   time.Time
	// TokenStatus is a convenience: "active" | "banned" | "cooldown" | "expired"
	TokenStatus string
	// Image-gen (SSO) fields — SSO/SSORW/Password are json:"-" (never serialized)
	SSO              string    `json:"-"`
	SSORW            string    `json:"-"`
	Password         string    `json:"-"`
	ImgCooldownUntil time.Time `json:"img_cooldown_until"`
	VidCooldownUntil time.Time `json:"vid_cooldown_until"`
	// Free-tier quota (GET /v1/usage): image 5, video 2, chat 10
	ImgQuotaUsed   int64     `json:"img_quota_used"`
	ImgQuotaLimit  int64     `json:"img_quota_limit"`
	VidQuotaUsed   int64     `json:"vid_quota_used"`
	VidQuotaLimit  int64     `json:"vid_quota_limit"`
	ChatQuotaUsed  int64     `json:"chat_quota_used"`
	ChatQuotaLimit int64     `json:"chat_quota_limit"`
	QuotaSyncedAt  time.Time `json:"quota_synced_at"`

	// Billing fields
	BillingSyncedAt time.Time
	PeriodStart     string
	PeriodEnd       string
	PeriodType      string
	OnDemandCap     int64
	OnDemandUsed    int64
	PrepaidBalance  int64
	UnifiedBilling  bool

	// Per-account cumulative token usage
	TokensUsed       int64
	PromptTokens     int64
	CompletionTokens int64
	UsageResetAt     time.Time
}

// Snapshot returns a mutex-safe copy of the account's current state.
func (a *GrokAccount) Snapshot() GrokAccountSnapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	s := GrokAccountSnapshot{
		Email:            a.Email,
		Sub:              a.Sub,
		AccessToken:      a.AccessToken,
		RefreshToken:     a.RefreshToken,
		IDToken:          a.IDToken,
		Expired:          a.Expired,
		ExpiresIn:        a.ExpiresIn,
		ExpiresAt:        a.expiresAt,
		LastRefresh:      a.LastRefresh,
		Disabled:         a.disabled,
		DisabledAt:       a.disabledAt,
		SSO:              a.SSO,
		SSORW:            a.SSORW,
		Password:         a.Password,
		ImgCooldownUntil: a.imgCooldownUntil,
		VidCooldownUntil: a.vidCooldownUntil,
		ImgQuotaUsed:     a.imgQuotaUsed,
		ImgQuotaLimit:    a.imgQuotaLimit,
		VidQuotaUsed:     a.vidQuotaUsed,
		VidQuotaLimit:    a.vidQuotaLimit,
		ChatQuotaUsed:    a.chatQuotaUsed,
		ChatQuotaLimit:   a.chatQuotaLimit,
		QuotaSyncedAt:    a.quotaSyncedAt,

		BillingSyncedAt: a.billingSyncedAt,
		PeriodStart:     a.periodStart,
		PeriodEnd:       a.periodEnd,
		PeriodType:      a.periodType,
		OnDemandCap:     a.onDemandCap,
		OnDemandUsed:    a.onDemandUsed,
		PrepaidBalance:  a.prepaidBalance,
		UnifiedBilling:  a.unifiedBilling,

		TokensUsed:       a.tokensUsed,
		PromptTokens:     a.promptTokens,
		CompletionTokens: a.completionTokens,
		UsageResetAt:     a.usageResetAt,
	}
	switch {
	case a.disabled && a.disabledAt.IsZero():
		s.TokenStatus = "banned"
	case a.disabled:
		s.TokenStatus = "cooldown"
	case time.Now().After(a.expiresAt.Add(-REFRESH_BUFFER)):
		s.TokenStatus = "expired"
	default:
		s.TokenStatus = "active"
	}
	return s
}

// toDTO returns a db.GrokAccountDTO snapshot of the account under RLock.
// Use this before calling saveGrokAccount — it guarantees the persisted
// payload is a consistent snapshot, never a partial mid-write mix.
// When TokenRefreshDisabled=1 (dev), AccessToken/RefreshToken/IDToken are
// zeroed so credentials never persist to dev Redis (leak prevention).
func (a *GrokAccount) toDTO() db.GrokAccountDTO {
	a.mu.RLock()
	defer a.mu.RUnlock()
	at, rt, idt := a.AccessToken, a.RefreshToken, a.IDToken
	if TokenRefreshDisabled {
		at, rt, idt = "", "", ""
	}
	return db.GrokAccountDTO{
		Email:            a.Email,
		AccessToken:      at,
		RefreshToken:     rt,
		IDToken:          idt,
		ExpiresAt:        a.expiresAt,
		ExpiresIn:        a.ExpiresIn,
		Expired:          a.Expired,
		LastRefresh:      a.LastRefresh,
		Sub:              a.Sub,
		Disabled:         a.disabled,
		DisabledAt:       a.disabledAt,
		SSO:              a.SSO,
		SSORW:            a.SSORW,
		Password:         a.Password,
		ImgCooldownUntil: a.imgCooldownUntil,
		VidCooldownUntil: a.vidCooldownUntil,
		ImgQuotaUsed:     a.imgQuotaUsed,
		ImgQuotaLimit:    a.imgQuotaLimit,
		VidQuotaUsed:     a.vidQuotaUsed,
		VidQuotaLimit:    a.vidQuotaLimit,
		ChatQuotaUsed:    a.chatQuotaUsed,
		ChatQuotaLimit:   a.chatQuotaLimit,
		QuotaSyncedAt:    a.quotaSyncedAt,

		BillingSyncedAt: a.billingSyncedAt,
		PeriodStart:     a.periodStart,
		PeriodEnd:       a.periodEnd,
		PeriodType:      a.periodType,
		OnDemandCap:     a.onDemandCap,
		OnDemandUsed:    a.onDemandUsed,
		PrepaidBalance:  a.prepaidBalance,
		UnifiedBilling:  a.unifiedBilling,

		TokensUsed:       a.tokensUsed,
		PromptTokens:     a.promptTokens,
		CompletionTokens: a.completionTokens,
		UsageResetAt:     a.usageResetAt,
	}
}

func (a *GrokAccount) IsDisabled() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.disabled
}

func (a *GrokAccount) GetAccessToken() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}

// Refresh refreshes the access token. Concurrent calls for the same email
// are collapsed via singleflight. Mutex is NOT held during the HTTP round-trip.
func (a *GrokAccount) Refresh() error {
	if TokenRefreshDisabled {
		return fmt.Errorf("token refresh disabled (TOKEN_REFRESH_DISABLED=1)")
	}
	_, err, _ := refreshSF.Do(a.Email, func() (any, error) {
		return nil, a.refreshLocked()
	})
	return err
}

func (a *GrokAccount) refreshLocked() error {
	a.mu.Lock()
	email := a.Email
	rt := a.RefreshToken
	a.mu.Unlock()

	slog.Debug("refreshing", "module", "grok-refresh", "email", email)
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {XAI_CLIENT_ID},
		"refresh_token": {rt},
	}
	req, err := http.NewRequest("POST", XAI_TOKEN_URL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	client, proxyID := getClient(tokenRefreshClient, "grok")
	resp, err := client.Do(req)
	if err != nil {
		markProxyResult(proxyID, err, 0)
		return err
	}
	markProxyResult(proxyID, nil, resp.StatusCode)
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("refresh [%d]: %s", resp.StatusCode, string(body))
	}
	var tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tokens); err != nil {
		return err
	}

	a.mu.Lock()
	a.AccessToken = tokens.AccessToken
	if tokens.RefreshToken != "" {
		a.RefreshToken = tokens.RefreshToken
	}
	if tokens.IDToken != "" {
		a.IDToken = tokens.IDToken
	}
	if tokens.ExpiresIn > 0 {
		a.ExpiresIn = tokens.ExpiresIn
	}
	a.expiresAt = time.Now().Add(time.Duration(a.ExpiresIn) * time.Second)
	a.LastRefresh = time.Now().Format(time.RFC3339)
	a.Expired = a.expiresAt.Format(time.RFC3339)
	expIn := a.ExpiresIn
	a.mu.Unlock()

	slog.Info("refresh ok", "module", "grok-refresh", "email", email, "expires_in_s", expIn)
	if a.db != nil {
		saveGrokAccount(a.db, a.toDTO())
		a.db.LogRefresh(db.RefreshLog{
			Timestamp: time.Now(), AccountEmail: email, Provider: "grok",
			Success: true,
		})
		a.db.LogEvent(db.AccountEvent{
			Timestamp: time.Now(), AccountID: email, Provider: "grok",
			EventType: "token_refreshed",
		})
	}
	return nil
}

func (a *GrokAccount) EnsureValid() error {
	if !a.IsExpired() {
		return nil
	}
	return a.Refresh()
}

// GROK ACCOUNT MANAGER

type GrokAccountManager struct {
	accounts []*GrokAccount
	mu       sync.RWMutex
	next     int
	db       *db.Store

	// sticky sessions: sessionID → bound account (prompt-cache locality)
	sticky   map[string]*grokStickyBinding
	stickyMu sync.Mutex
}

// grokStickyBinding records which account a session is pinned to.
type grokStickyBinding struct {
	acc      *GrokAccount
	lastSeen time.Time
}

// grokStickyTTL is how long a session binding survives without traffic.
const grokStickyTTL = 30 * time.Minute

func NewGrokAccountManager(store *db.Store) *GrokAccountManager {
	am := &GrokAccountManager{accounts: make([]*GrokAccount, 0), db: store, sticky: make(map[string]*grokStickyBinding)}
	go am.stickyJanitor()
	return am
}

// DB returns the persistence handle (nil in test builds).
func (am *GrokAccountManager) DB() *db.Store { return am.db }

// SetAccountsForTest replaces the internal slice. Whitebox tests only.
func (am *GrokAccountManager) SetAccountsForTest(accts []*GrokAccount) {
	am.mu.Lock()
	defer am.mu.Unlock()
	am.accounts = accts
}

// AddAccount appends an account. The account gets its db handle wired.
// Callers should provide accounts constructed via NewGrokAccountForTest
// (or freshly built structs in-package).
func (am *GrokAccountManager) AddAccount(acc *GrokAccount) {
	am.mu.Lock()
	defer am.mu.Unlock()
	if am.db != nil && acc.db == nil {
		acc.db = am.db
	}
	am.accounts = append(am.accounts, acc)
}

func (am *GrokAccountManager) Len() int {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return len(am.accounts)
}

// LoadFromRedis loads ALL grok accounts from Redis (single source of truth).
func (am *GrokAccountManager) LoadFromRedis() error {
	if am.db == nil || !am.db.Ready() {
		return fmt.Errorf("redis not available")
	}
	redisState, err := am.db.LoadGrokAccounts()
	if err != nil {
		return err
	}
	am.mu.Lock()
	defer am.mu.Unlock()
	am.accounts = am.accounts[:0]
	for email, state := range redisState {
		if state["access_token"] == "" || state["refresh_token"] == "" {
			continue
		}
		acc := &GrokAccount{
			Email:        email,
			AccessToken:  state["access_token"],
			RefreshToken: state["refresh_token"],
			IDToken:      state["id_token"],
			Expired:      state["expired"],
			LastRefresh:  state["last_refresh"],
			Sub:          state["sub"],
			SSO:          state["sso"],
			SSORW:        state["sso_rw"],
			Password:     state["password"],
			db:           am.db,
		}
		if v := state["expires_in"]; v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				acc.ExpiresIn = n
			}
		}
		if v := state["expired"]; v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				acc.expiresAt = t
			} else {
				acc.expiresAt = time.Now()
			}
		} else if acc.ExpiresIn > 0 {
			acc.expiresAt = time.Now().Add(time.Duration(acc.ExpiresIn) * time.Second)
		} else {
			acc.expiresAt = time.Now()
		}
		if v := state["disabled"]; v == "true" || v == "1" {
			acc.disabled = true
			if v := state["disabled_at"]; v != "" {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					if n <= 0 {
						acc.disabledAt = time.Time{}
					} else {
						acc.disabledAt = time.Unix(n, 0)
					}
				}
			}
		}
		// Image-gen cooldown (429 resource-exhausted marker)
		if v := state["img_cooldown_until"]; v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				acc.imgCooldownUntil = time.Unix(n, 0)
			}
		}
		// Video-gen cooldown (429 resource-exhausted marker)
		if v := state["vid_cooldown_until"]; v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				acc.vidCooldownUntil = time.Unix(n, 0)
			}
		}
		// Free-tier quota (GET /v1/usage)
		if n, err := strconv.ParseInt(state["img_quota_used"], 10, 64); err == nil {
			acc.imgQuotaUsed = n
		}
		if n, err := strconv.ParseInt(state["img_quota_limit"], 10, 64); err == nil {
			acc.imgQuotaLimit = n
		}
		if n, err := strconv.ParseInt(state["vid_quota_used"], 10, 64); err == nil {
			acc.vidQuotaUsed = n
		}
		if n, err := strconv.ParseInt(state["vid_quota_limit"], 10, 64); err == nil {
			acc.vidQuotaLimit = n
		}
		if n, err := strconv.ParseInt(state["chat_quota_used"], 10, 64); err == nil {
			acc.chatQuotaUsed = n
		}
		if n, err := strconv.ParseInt(state["chat_quota_limit"], 10, 64); err == nil {
			acc.chatQuotaLimit = n
		}
		if v := state["quota_synced_at"]; v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				acc.quotaSyncedAt = time.Unix(n, 0)
			}
		}
		// Billing fields
		if v := state["billing_synced_at"]; v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				acc.billingSyncedAt = time.Unix(n, 0)
			}
		}
		acc.periodStart = state["period_start"]
		acc.periodEnd = state["period_end"]
		acc.periodType = state["period_type"]
		if v := state["on_demand_cap"]; v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				acc.onDemandCap = n
			}
		}
		if v := state["on_demand_used"]; v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				acc.onDemandUsed = n
			}
		}
		if v := state["prepaid_balance"]; v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				acc.prepaidBalance = n
			}
		}
		if v := state["unified_billing"]; v == "true" || v == "1" {
			acc.unifiedBilling = true
		}
		// Per-account token usage
		if v := state["tokens_used"]; v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				acc.tokensUsed = n
			}
		}
		if v := state["prompt_tokens"]; v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				acc.promptTokens = n
			}
		}
		if v := state["completion_tokens"]; v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				acc.completionTokens = n
			}
		}
		if v := state["usage_reset_at"]; v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				acc.usageResetAt = time.Unix(n, 0)
			}
		}
		am.accounts = append(am.accounts, acc)
	}
	slog.Info("loaded accounts from Redis", "module", "grok", "count", len(am.accounts))
	return nil
}

// Next returns the next healthy account. O(k) round-robin — no full-pool
// re-enable scan (that runs in reenableWorker). No Redis I/O on hot path.
func (am *GrokAccountManager) Next() (*GrokAccount, error) {
	am.mu.Lock()
	defer am.mu.Unlock()
	if len(am.accounts) == 0 {
		return nil, fmt.Errorf("no grok accounts")
	}
	now := time.Now()

	// Pass 1: valid token outside refresh window
	for i := 0; i < len(am.accounts); i++ {
		idx := (am.next + i) % len(am.accounts)
		acc := am.accounts[idx]
		acc.mu.RLock()
		if acc.disabled {
			acc.mu.RUnlock()
			continue
		}
		if now.Before(acc.expiresAt.Add(-REFRESH_BUFFER)) {
			acc.mu.RUnlock()
			am.next = (idx + 1) % len(am.accounts)
			return acc, nil
		}
		acc.mu.RUnlock()
	}

	// Pass 2: near-expiry — pick best remaining, async singleflight refresh
	var bestAcc *GrokAccount
	var bestExpiry time.Time
	for _, acc := range am.accounts {
		acc.mu.RLock()
		if acc.disabled {
			acc.mu.RUnlock()
			continue
		}
		if bestAcc == nil || acc.expiresAt.After(bestExpiry) {
			bestAcc = acc
			bestExpiry = acc.expiresAt
		}
		acc.mu.RUnlock()
	}
	if bestAcc != nil {
		go bestAcc.Refresh()
		am.next = (am.next + 1) % len(am.accounts)
		return bestAcc, nil
	}
	return nil, fmt.Errorf("all grok accounts disabled")
}

func (am *GrokAccountManager) GetAll() []*GrokAccount {
	am.mu.RLock()
	defer am.mu.RUnlock()
	r := make([]*GrokAccount, len(am.accounts))
	copy(r, am.accounts)
	return r
}

// ReenableCooldowns lifts temp cooldowns past 10 minutes. Called by
// background worker only — request path stays O(k) with no Redis I/O.
func (am *GrokAccountManager) ReenableCooldowns() {
	accounts := am.GetAll()
	now := time.Now()
	var reenabled []*GrokAccount
	for _, acc := range accounts {
		acc.mu.Lock()
		if acc.disabled && !acc.disabledAt.IsZero() && now.Sub(acc.disabledAt) > 10*time.Minute {
			acc.disabled = false
			reenabled = append(reenabled, acc)
		}
		acc.mu.Unlock()
	}
	for _, acc := range reenabled {
		if acc.db != nil {
			saveGrokAccount(acc.db, acc.toDTO())
		}
		slog.Info("re-enabled cooldown account", "module", "grok", "email", acc.Email)
	}
}

// ---------------------------------------------------------------------------
// Sticky session binding (prompt-cache locality)
// ---------------------------------------------------------------------------

// stickyJanitor evicts idle bindings so the map doesn't grow unbounded.
func (am *GrokAccountManager) stickyJanitor() {
	if am.sticky == nil {
		return
	}
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-grokStickyTTL)
		am.stickyMu.Lock()
		for sid, b := range am.sticky {
			if b.lastSeen.Before(cutoff) {
				delete(am.sticky, sid)
			}
		}
		am.stickyMu.Unlock()
	}
}

// NextSticky returns the account bound to sessionID. If unbound, or the bound
// account got disabled, it binds the next round-robin account. All requests
// with the same sessionID hit the same upstream account → Grok prompt-cache
// stays hot instead of regenerating per request.
func (am *GrokAccountManager) NextSticky(sessionID string) (*GrokAccount, error) {
	if sessionID == "" || am.sticky == nil {
		return am.Next()
	}
	am.stickyMu.Lock()
	if b, ok := am.sticky[sessionID]; ok {
		if !b.acc.IsDisabled() {
			b.lastSeen = time.Now()
			am.stickyMu.Unlock()
			return b.acc, nil
		}
		delete(am.sticky, sessionID) // bound account died — rebind below
	}
	am.stickyMu.Unlock()

	acc, err := am.Next()
	if err != nil {
		return nil, err
	}
	am.stickyMu.Lock()
	am.sticky[sessionID] = &grokStickyBinding{acc: acc, lastSeen: time.Now()}
	am.stickyMu.Unlock()
	return acc, nil
}

// UnbindSticky drops a session binding (e.g. after permanent disable so the
// next request rebinds to a fresh account instead of the dead one).
func (am *GrokAccountManager) UnbindSticky(sessionID string, acc *GrokAccount) {
	if sessionID == "" || am.sticky == nil {
		return
	}
	am.stickyMu.Lock()
	if b, ok := am.sticky[sessionID]; ok && b.acc == acc {
		delete(am.sticky, sessionID)
	}
	am.stickyMu.Unlock()
}

// StickyCount reports active session bindings (dashboard/debug).
func (am *GrokAccountManager) StickyCount() int {
	am.stickyMu.Lock()
	defer am.stickyMu.Unlock()
	return len(am.sticky)
}

// ReenableWorker is the long-lived goroutine that lifts cooldowns.
// Pass a context cancelled on SIGTERM for clean shutdown.
func ReenableWorker(ctx context.Context, am *GrokAccountManager) {
	am.ReenableCooldowns()
	ticker := time.NewTicker(REENABLE_TICK)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			am.ReenableCooldowns()
		}
	}
}

// AutoRefreshWorker pre-warms tokens concurrently before they expire.
// Pass a context cancelled on SIGTERM for clean shutdown.
func AutoRefreshWorker(ctx context.Context, am *GrokAccountManager) {
	ticker := time.NewTicker(PRE_WARM_TICK)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		accounts := am.GetAll()
		var wg sync.WaitGroup
		sem := make(chan struct{}, MAX_CONCURRENT_REFRESH)

		for _, a := range accounts {
			a.mu.RLock()
			perm := a.disabled && a.disabledAt.IsZero()
			needsRefresh := !perm && time.Now().After(a.expiresAt.Add(-PRE_WARM_WINDOW))
			a.mu.RUnlock()

			if !needsRefresh {
				continue
			}

			wg.Add(1)
			sem <- struct{}{}
			go func(acc *GrokAccount) {
				defer wg.Done()
				defer func() { <-sem }()

				if err := acc.Refresh(); err != nil {
					if strings.Contains(err.Error(), "invalid_grant") {
						acc.mu.Lock()
						acc.disabled = true
						acc.disabledAt = time.Time{}
						acc.mu.Unlock()
						if acc.db != nil {
							saveGrokAccount(acc.db, acc.toDTO())
						}
						slog.Warn("account revoked, disabled", "module", "grok-worker", "email", acc.Email)
					} else {
						slog.Warn("refresh error", "module", "grok-worker", "email", acc.Email, "error", err)
					}
				}
			}(a)
		}
		wg.Wait()
	}
}

func (am *GrokAccountManager) ImportAccountRaw(email, accessToken, refreshToken, idToken string, expiresIn int) (*GrokAccount, bool) {
	am.mu.Lock()
	defer am.mu.Unlock()
	for _, existing := range am.accounts {
		if existing.Email == email {
			return existing, false
		}
	}
	acc := &GrokAccount{
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		IDToken:      idToken,
		ExpiresIn:    expiresIn,
		expiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second),
		db:           am.db,
	}
	acc.Expired = acc.expiresAt.Format(time.RFC3339)
	am.accounts = append(am.accounts, acc)
	return acc, true
}

// UpsertAccount inserts or updates a Grok account by email. Fields are set
// under the account mutex; Redis persistence runs after unlock. Returns the
// full new pool size and whether the account was newly created.
func (am *GrokAccountManager) UpsertAccount(email, accessToken, refreshToken, idToken, sub string, expiresIn int) (created bool, total int, acc *GrokAccount) {
	if expiresIn == 0 {
		expiresIn = 21600 // default 6h
	}
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)
	expired := expiresAt.Format(time.RFC3339)
	lastRefresh := time.Now().Format(time.RFC3339)

	am.mu.Lock()
	// existing?
	for _, existing := range am.accounts {
		if existing.Email == email {
			existing.mu.Lock()
			existing.AccessToken = accessToken
			existing.RefreshToken = refreshToken
			existing.IDToken = idToken
			existing.ExpiresIn = expiresIn
			existing.Expired = expired
			existing.LastRefresh = lastRefresh
			existing.Sub = sub
			existing.expiresAt = expiresAt
			existing.disabled = false
			existing.disabledAt = time.Time{}
			existing.mu.Unlock()
			total = len(am.accounts)
			am.mu.Unlock()
			if am.db != nil {
				saveGrokAccount(am.db, existing.toDTO())
			}
			return false, total, existing
		}
	}
	acc = &GrokAccount{
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		IDToken:      idToken,
		ExpiresIn:    expiresIn,
		Expired:      expired,
		LastRefresh:  lastRefresh,
		Sub:          sub,
		expiresAt:    expiresAt,
		db:           am.db,
	}
	am.accounts = append(am.accounts, acc)
	total = len(am.accounts)
	am.mu.Unlock()
	if am.db != nil {
		saveGrokAccount(am.db, acc.toDTO())
	}
	return true, total, acc
}

// SetSSO stores the console.x.ai SSO cookies for an account (image generation).
// Empty sso clears them (e.g. dead cookie).
func (am *GrokAccountManager) SetSSO(email, sso, ssoRW string) {
	am.mu.RLock()
	var acc *GrokAccount
	for _, a := range am.accounts {
		if a.Email == email {
			acc = a
			break
		}
	}
	am.mu.RUnlock()
	if acc == nil {
		return
	}
	acc.mu.Lock()
	acc.SSO, acc.SSORW = sso, ssoRW
	acc.mu.Unlock()
	if am.db != nil {
		saveGrokAccount(am.db, acc.toDTO())
	}
}

// SetPassword stores the console.x.ai login password for an account (used by
// lazy pure-HTTP re-login when the SSO cookie dies). Never serialized.
func (am *GrokAccountManager) SetPassword(email, pwd string) {
	am.mu.RLock()
	var acc *GrokAccount
	for _, a := range am.accounts {
		if a.Email == email {
			acc = a
			break
		}
	}
	am.mu.RUnlock()
	if acc == nil {
		return
	}
	acc.mu.Lock()
	acc.Password = pwd
	acc.mu.Unlock()
	if am.db != nil {
		saveGrokAccount(am.db, acc.toDTO())
	}
}

// GetSnapshot returns a value snapshot for an account by email (nil when
// missing). Used by the image-gen handler after a lazy SSO refresh.
func (am *GrokAccountManager) GetSnapshot(email string) *GrokAccountSnapshot {
	am.mu.RLock()
	var acc *GrokAccount
	for _, a := range am.accounts {
		if a.Email == email {
			acc = a
			break
		}
	}
	am.mu.RUnlock()
	if acc == nil {
		return nil
	}
	s := acc.Snapshot()
	return &s
}

// DeleteAccount removes an account by email from memory + Redis.
func (am *GrokAccountManager) DeleteAccount(email string) bool {
	am.mu.Lock()
	defer am.mu.Unlock()
	for i, a := range am.accounts {
		if a.Email == email {
			am.accounts = append(am.accounts[:i], am.accounts[i+1:]...)
			if am.db != nil {
				am.db.DeleteGrokAccount(email)
			}
			return true
		}
	}
	return false
}

// UpdateExisting persists mutations made to an existing GrokAccount pointer.
// The caller mutated fields (tokens, expiry) under acc.mu already.
func (am *GrokAccountManager) UpdateExisting(acc *GrokAccount) {
	if am.db != nil {
		saveGrokAccount(am.db, acc.toDTO())
	}
}

func (am *GrokAccountManager) CleanupDisabled() int {
	return am.cleanupBy(func(a *GrokAccount) bool {
		a.mu.RLock()
		defer a.mu.RUnlock()
		// permanently disabled == banned (disabledAt zero)
		return a.disabled && a.disabledAt.IsZero()
	}, "disabled")
}

// CleanupBanned removes all banned grok accounts (token_status == "banned").
// In this codebase banned ≡ permanently disabled (disabled && disabledAt.IsZero()).
// Cooldown accounts are preserved.
func (am *GrokAccountManager) CleanupBanned() int {
	return am.cleanupBy(func(a *GrokAccount) bool {
		a.mu.RLock()
		defer a.mu.RUnlock()
		return a.disabled && a.disabledAt.IsZero()
	}, "banned")
}

// cleanupBy removes accounts matching pred. Returns removed count.
func (am *GrokAccountManager) cleanupBy(pred func(*GrokAccount) bool, label string) int {
	am.mu.Lock()
	var removed int
	var kept []*GrokAccount
	for _, a := range am.accounts {
		if pred(a) {
			removed++
			if am.db != nil {
				am.db.DeleteGrokAccount(a.Email)
			}
		} else {
			kept = append(kept, a)
		}
	}
	am.accounts = kept
	am.mu.Unlock()
	if removed > 0 {
		slog.Info("cleanup grok accounts", "module", "grok", "kind", label, "removed", removed, "remaining", am.Len())
	}
	return removed
}
