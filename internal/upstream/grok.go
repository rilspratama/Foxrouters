package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/singleflight"

	"foxrouters/internal/db"
)

// refreshSF collapses concurrent Refresh() for the same account email.
var refreshSF singleflight.Group

// billingSF collapses concurrent SyncBilling() for the same account email.
var billingSF singleflight.Group

// GrokAccount holds one OAuth session against auth.x.ai.
type GrokAccount struct {
	Email        string `json:"email"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token,omitempty"`
	ExpiresIn    int    `json:"expires_in"`
	Expired      string `json:"expired"`
	LastRefresh  string `json:"last_refresh"`
	Sub          string `json:"sub"`
	mu           sync.RWMutex
	expiresAt    time.Time
	disabled     bool
	disabledAt   time.Time
	db           *db.Store

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
	tokensUsed   int64 // cumulative total_tokens (prompt + completion + reasoning)
	promptTokens int64 // cumulative prompt_tokens (includes cached)
	completionTokens int64 // cumulative completion_tokens
	usageResetAt time.Time // when the weekly period last reset (from billing sync)
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
		Email:        a.Email,
		Sub:          a.Sub,
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		IDToken:      a.IDToken,
		Expired:      a.Expired,
		ExpiresIn:    a.ExpiresIn,
		ExpiresAt:    a.expiresAt,
		LastRefresh:  a.LastRefresh,
		Disabled:     a.disabled,
		DisabledAt:   a.disabledAt,

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
func (a *GrokAccount) toDTO() db.GrokAccountDTO {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return db.GrokAccountDTO{
		Email:        a.Email,
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		IDToken:      a.IDToken,
		ExpiresAt:    a.expiresAt,
		ExpiresIn:    a.ExpiresIn,
		Expired:      a.Expired,
		LastRefresh:  a.LastRefresh,
		Sub:          a.Sub,
		Disabled:     a.disabled,
		DisabledAt:   a.disabledAt,

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

// ============================================================================
// GROK ACCOUNT MANAGER
// ============================================================================

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

// ─── Billing sync ────────────────────────────────────────────────────────────

// grokBillingResponse is the JSON shape from GET /v1/billing?format=credits.
type grokBillingResponse struct {
	Config struct {
		CurrentPeriod struct {
			Type  string `json:"type"`
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"currentPeriod"`
		OnDemandCap       *grokCent `json:"onDemandCap"`
		OnDemandUsed      *grokCent `json:"onDemandUsed"`
		PrepaidBalance    *grokCent `json:"prepaidBalance"`
		IsUnifiedBilling  bool      `json:"isUnifiedBillingUser"`
		BillingPeriodEnd  string    `json:"billingPeriodEnd"`
	} `json:"config"`
}

// grokCent is the { "val": <cents> } wrapper used by the billing API.
type grokCent struct {
	Val int64 `json:"val"`
}

// SyncBilling fetches the current billing/usage state from cli-chat-proxy
// and persists it to the account + Redis. Singleflight per email.
func (a *GrokAccount) SyncBilling() error {
	_, err, _ := billingSF.Do(a.Email, func() (any, error) {
		return nil, a.syncBillingLocked()
	})
	return err
}

func (a *GrokAccount) syncBillingLocked() error {
	// Ensure token is fresh before the billing call.
	if err := a.EnsureValid(); err != nil {
		return fmt.Errorf("grok billing ensure valid: %w", err)
	}

	token := a.GetAccessToken()

	req, err := http.NewRequest("GET", GROK_BILLING_URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("x-authenticateresponse", "authenticate-response")
	req.Header.Set("x-grok-client-version", GROK_CLIENT_VERSION)
	req.Header.Set("x-grok-client-identifier", GROK_CLIENT_IDENTIFIER)
	req.Header.Set("x-grok-client-mode", "tui")
	req.Header.Set("User-Agent", fmt.Sprintf("grok-shell/%s (linux; x86_64)", GROK_CLIENT_VERSION))
	if a.Sub != "" {
		req.Header.Set("x-userid", a.Sub)
	}
	if a.Email != "" {
		req.Header.Set("x-email", a.Email)
	}

	client, proxyID := getClient(upstreamClient, "grok")
	resp, err := client.Do(req)
	if err != nil {
		markProxyResult(proxyID, err, 0)
		return fmt.Errorf("grok billing: %w", err)
	}
	markProxyResult(proxyID, nil, resp.StatusCode)
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("grok billing [%d]: %s", resp.StatusCode, truncateLog(string(body), 200))
	}

	var billing grokBillingResponse
	if err := json.Unmarshal(body, &billing); err != nil {
		return fmt.Errorf("grok billing parse: %w", err)
	}

	var cap, used, prepaid int64
	if billing.Config.OnDemandCap != nil {
		cap = billing.Config.OnDemandCap.Val
	}
	if billing.Config.OnDemandUsed != nil {
		used = billing.Config.OnDemandUsed.Val
	}
	if billing.Config.PrepaidBalance != nil {
		prepaid = billing.Config.PrepaidBalance.Val
	}

	// Detect weekly period rollover — if periodEnd changed, reset usage counters
	newPeriodEnd := billing.Config.CurrentPeriod.End
	a.mu.Lock()
	periodChanged := newPeriodEnd != "" && a.periodEnd != "" && newPeriodEnd != a.periodEnd
	a.billingSyncedAt = time.Now()
	a.periodStart = billing.Config.CurrentPeriod.Start
	a.periodEnd = newPeriodEnd
	a.periodType = billing.Config.CurrentPeriod.Type
	a.onDemandCap = cap
	a.onDemandUsed = used
	a.prepaidBalance = prepaid
	a.unifiedBilling = billing.Config.IsUnifiedBilling
	if periodChanged {
		a.tokensUsed = 0
		a.promptTokens = 0
		a.completionTokens = 0
		a.usageResetAt = time.Now()
	}
	a.mu.Unlock()

	if periodChanged {
		slog.Info("grok billing period rolled over, usage reset",
			"module", "grok-billing",
			"email", a.Email,
			"old_period_end", a.periodEnd,
			"new_period_end", newPeriodEnd)
	}

	if a.db != nil {
		saveGrokAccount(a.db, a.toDTO())
	}
	slog.Debug("grok billing sync ok",
		"module", "grok-billing",
		"email", a.Email,
		"period_end", a.periodEnd,
		"on_demand_cap", cap,
		"on_demand_used", used,
		"prepaid", prepaid,
		"unified", billing.Config.IsUnifiedBilling)
	return nil
}

// GROK_FREE_TIER_QUOTA is the free-tier weekly token quota (1M as of Aug 2026,
// down from 2M). Used for dashboard display only — enforcement is server-side.
const GROK_FREE_TIER_QUOTA = 1_000_000

// RecordUsage accumulates per-account token usage from a completed request's
// usage fields. Called after ProxyGrok finishes reading the upstream response.
// Non-blocking: persists to Redis best-effort. Does NOT return an error —
// usage tracking is telemetry, not critical path.
func (a *GrokAccount) RecordUsage(promptTokens, completionTokens int) {
	if promptTokens <= 0 && completionTokens <= 0 {
		return
	}
	total := int64(promptTokens + completionTokens)
	a.mu.Lock()
	a.promptTokens += int64(promptTokens)
	a.completionTokens += int64(completionTokens)
	a.tokensUsed += total
	a.mu.Unlock()

	// Persist best-effort (fire-and-forget — don't block the response path).
	if a.db != nil {
		go saveGrokAccount(a.db, a.toDTO())
	}
}

// ResetUsage zeroes the per-account usage counters. Called by SyncBilling
// when it detects a new billing period (periodEnd changed).
func (a *GrokAccount) ResetUsage(resetAt time.Time) {
	a.mu.Lock()
	a.tokensUsed = 0
	a.promptTokens = 0
	a.completionTokens = 0
	a.usageResetAt = resetAt
	a.mu.Unlock()
	if a.db != nil {
		go saveGrokAccount(a.db, a.toDTO())
	}
}

// GrokBillingSyncWorker periodically syncs billing data for all non-banned
// Grok accounts. Mirrors CBCreditSyncWorker. Pass a context cancelled on
// SIGTERM for clean shutdown.
func GrokBillingSyncWorker(ctx context.Context, am *GrokAccountManager) {
	syncAllGrokBilling(am, true)

	ticker := time.NewTicker(GROK_BILLING_SYNC_TICK)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			syncAllGrokBilling(am, false)
		}
	}
}

func syncAllGrokBilling(am *GrokAccountManager, stagger bool) {
	accounts := am.GetAll()
	var wg sync.WaitGroup
	sem := make(chan struct{}, GROK_BILLING_SYNC_CONCURRENCY)
	idx := 0
	for _, a := range accounts {
		a.mu.RLock()
		perm := a.disabled && a.disabledAt.IsZero() // banned — skip
		a.mu.RUnlock()
		if perm {
			continue
		}

		if stagger && idx > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		idx++

		wg.Add(1)
		sem <- struct{}{}
		go func(acc *GrokAccount) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := acc.SyncBilling(); err != nil {
				slog.Warn("grok billing sync error",
					"module", "grok-billing",
					"email", acc.Email,
					"error", err)
			}
		}(a)
	}
	wg.Wait()
}

// ImportAccountRaw adds an account with raw token material (used by /accounts/import).
// db handle is inherited from the manager. Returns the created account and true
// if new, existing account and false if the email already exists (caller may
// choose to update fields on the returned pointer).
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

// ============================================================================
// MODEL ROUTING + GROK PROXY
// ============================================================================

// ExpandGrokAlias maps grok-4.5-{high,medium,low,auto,none,xhigh} → reasoning_effort.
func ExpandGrokAlias(model string) (string, bool) {
	switch model {
	case "grok-4.5-high", "grok-4.5-xhigh":
		return "high", true
	case "grok-4.5-medium":
		return "medium", true
	case "grok-4.5-low":
		return "low", true
	case "grok-4.5-auto":
		return "auto", true
	case "grok-4.5-none":
		return "none", true
	default:
		return "", false
	}
}

// IsGrokModel returns true if the model routes to the Grok upstream.
func IsGrokModel(model string) bool {
	return strings.HasPrefix(model, "grok-")
}

func grokHeaders(token, accept, model, userID, email string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", accept)
	h.Set("X-XAI-Token-Auth", "xai-grok-cli")
	h.Set("x-authenticateresponse", "authenticate-response")
	h.Set("x-grok-client-version", GROK_CLIENT_VERSION)
	h.Set("x-grok-client-identifier", GROK_CLIENT_IDENTIFIER)
	h.Set("x-grok-client-mode", "tui")
	h.Set("User-Agent", fmt.Sprintf("grok-shell/%s (linux; x86_64)", GROK_CLIENT_VERSION))
	if userID != "" {
		h.Set("x-userid", userID)
	}
	if email != "" {
		h.Set("x-email", email)
	}
	convID := fmt.Sprintf("conv-%d", time.Now().UnixNano())
	reqID := fmt.Sprintf("req-%d", time.Now().UnixNano())
	sessionID := fmt.Sprintf("sess-%d", time.Now().UnixNano())
	agentID := "agent-shell"
	h.Set("x-grok-conv-id", convID)
	h.Set("x-grok-req-id", reqID)
	h.Set("x-grok-model-override", model)
	h.Set("x-grok-session-id", sessionID)
	h.Set("x-grok-agent-id", agentID)
	return h
}

// ---------------------------------------------------------------------------
// Account selection modes (router-level strategy, runtime-configurable)
// ---------------------------------------------------------------------------

// GrokSelectorMode picks how ProxyGrok chooses the upstream account.
type GrokSelectorMode string

const (
	// GrokSelectorRR — classic round-robin over enabled accounts (no cache locality).
	GrokSelectorRR GrokSelectorMode = "rr"
	// GrokSelectorSticky — session-id header binds conversation to one account
	// until it dies (per-conversation cache locality; different sessions may
	// land on different accounts, re-caching the shared system prompt each time).
	GrokSelectorSticky GrokSelectorMode = "sticky"
	// GrokSelectorContentHash — hash(model + first system message) deterministically
	// maps to one account: every session sharing the same system prompt lands on
	// the same account → giant shared prefix cached once.
	GrokSelectorContentHash GrokSelectorMode = "content-hash"
	// GrokSelectorHybrid — content-hash picks a small bucket (~3 accounts);
	// session-id sticks to one account inside the bucket. Dead accounts rebind
	// within the bucket, keeping the shared system-prompt cache warm.
	GrokSelectorHybrid GrokSelectorMode = "hybrid"
)

// grokHybridBucketSize is how many enabled accounts form one hybrid bucket.
const grokHybridBucketSize = 3

// grokSelectorMode holds the active mode (atomic for lock-free hot-path reads).
var grokSelectorMode atomic.Value // stores GrokSelectorMode

func init() {
	m := GrokSelectorMode(os.Getenv("GROK_SELECTOR_MODE"))
	if !validGrokSelectorMode(m) {
		m = GrokSelectorSticky // default: sticky sessions (prompt-cache locality)
	}
	grokSelectorMode.Store(m)
}

func validGrokSelectorMode(m GrokSelectorMode) bool {
	switch m {
	case GrokSelectorRR, GrokSelectorSticky, GrokSelectorContentHash, GrokSelectorHybrid:
		return true
	}
	return false
}

// GetGrokSelectorMode returns the active Grok account selection mode.
func GetGrokSelectorMode() GrokSelectorMode { return grokSelectorMode.Load().(GrokSelectorMode) }

// SetGrokSelectorMode validates + stores the mode and persists it to Redis
// (grok:config hash) so restarts keep the operator's choice.
func SetGrokSelectorMode(store *db.Store, m GrokSelectorMode) error {
	if !validGrokSelectorMode(m) {
		return fmt.Errorf("invalid selector mode %q (valid: rr|sticky|content-hash|hybrid)", m)
	}
	grokSelectorMode.Store(m)
	if store != nil {
		if err := store.SetGrokConfig("selector_mode", string(m)); err != nil {
			slog.Warn("selector mode persist failed", "module", "grok", "error", err)
		}
	}
	slog.Info("grok selector mode changed", "module", "grok", "mode", m)
	return nil
}

// LoadGrokSelectorMode restores the persisted mode from Redis (called at startup).
func LoadGrokSelectorMode(store *db.Store) {
	if store == nil {
		return
	}
	if v, err := store.GetGrokConfig("selector_mode"); err == nil && validGrokSelectorMode(GrokSelectorMode(v)) {
		grokSelectorMode.Store(GrokSelectorMode(v))
		slog.Info("grok selector mode restored", "module", "grok", "mode", v)
	}
}

// NextForMode selects an account according to the active mode.
//   - sessionID: from x-session-id/x-conversation-id/x-chat-id header (may be "")
//   - sysHash:   hash of model + first system message content (may be "")
func (am *GrokAccountManager) NextForMode(mode GrokSelectorMode, sessionID, sysHash string) (*GrokAccount, error) {
	switch mode {
	case GrokSelectorRR:
		return am.Next()
	case GrokSelectorContentHash:
		return am.nextByHash(sysHash)
	case GrokSelectorHybrid:
		return am.nextHybrid(sessionID, sysHash)
	case GrokSelectorSticky:
		fallthrough
	default:
		if sessionID != "" {
			return am.NextSticky(sessionID)
		}
		return am.Next()
	}
}

// nextByHash: deterministic account from sysHash (FNV-1a over enabled accounts).
// All sessions with the same system prompt land on the same account.
func (am *GrokAccountManager) nextByHash(sysHash string) (*GrokAccount, error) {
	am.mu.RLock()
	enabled := make([]*GrokAccount, 0, len(am.accounts))
	for _, acc := range am.accounts {
		if !acc.IsDisabled() {
			enabled = append(enabled, acc)
		}
	}
	am.mu.RUnlock()
	if len(enabled) == 0 {
		return nil, fmt.Errorf("all grok accounts disabled")
	}
	if sysHash == "" {
		return am.Next()
	}
	h := fnv.New64a()
	h.Write([]byte(sysHash))
	return enabled[h.Sum64()%uint64(len(enabled))], nil
}

// nextHybrid: content-hash selects a bucket of grokHybridBucketSize enabled
// accounts; session-id sticks to one account inside the bucket (rebinding
// within the bucket when the account dies → shared system-prompt cache stays
// warm).
func (am *GrokAccountManager) nextHybrid(sessionID, sysHash string) (*GrokAccount, error) {
	am.mu.RLock()
	enabled := make([]*GrokAccount, 0, len(am.accounts))
	for _, acc := range am.accounts {
		if !acc.IsDisabled() {
			enabled = append(enabled, acc)
		}
	}
	am.mu.RUnlock()
	if len(enabled) == 0 {
		return nil, fmt.Errorf("all grok accounts disabled")
	}
	if sysHash == "" {
		if sessionID != "" {
			return am.NextSticky(sessionID)
		}
		return am.Next()
	}

	// Bucket = consecutive slice of enabled accounts starting at hash position.
	h := fnv.New64a()
	h.Write([]byte(sysHash))
	start := int(h.Sum64() % uint64(len(enabled)))
	bucket := make([]*GrokAccount, 0, grokHybridBucketSize)
	for i := 0; i < grokHybridBucketSize && i < len(enabled); i++ {
		bucket = append(bucket, enabled[(start+i)%len(enabled)])
	}

	// Session binding within bucket (reuse sticky map — but only accept the
	// bound account if it is in THIS bucket; otherwise rebind into the bucket).
	if sessionID != "" {
		am.stickyMu.Lock()
		if b, ok := am.sticky[sessionID]; ok && !b.acc.IsDisabled() {
			for _, acc := range bucket {
				if acc == b.acc {
					b.lastSeen = time.Now()
					am.stickyMu.Unlock()
					return acc, nil
				}
			}
		}
		delete(am.sticky, sessionID)
		// bind first bucket account (deterministic per session: hash sessionID)
		sh := fnv.New64a()
		sh.Write([]byte(sessionID))
		pick := bucket[sh.Sum64()%uint64(len(bucket))]
		am.sticky[sessionID] = &grokStickyBinding{acc: pick, lastSeen: time.Now()}
		am.stickyMu.Unlock()
		return pick, nil
	}
	return bucket[0], nil
}

// ProxyGrok forwards a chat/completions (or /v1/*) request to Grok, retrying
// per-account on 401/402/403/5xx.
func ProxyGrok(c *gin.Context, body []byte, am *GrokAccountManager, clientStream bool, hc *HealthChecker, model string) {
	if !hc.Grok.CanRequest() {
		hc.Grok.RecordRequest(0, fmt.Errorf("circuit open"))
		c.JSON(503, gin.H{"error": "grok upstream circuit breaker open"})
		c.Set("error_msg", "grok circuit breaker open")
		errJSON, _ := json.Marshal(gin.H{"error": "grok upstream circuit breaker open"})
		c.Set("response_body", json.RawMessage(errJSON))
		return
	}

	path := c.Request.URL.Path
	upstreamPath := strings.TrimPrefix(path, "/v1")
	upstreamURL := XAI_UPSTREAM_URL + upstreamPath
	if c.Request.URL.RawQuery != "" {
		upstreamURL += "?" + c.Request.URL.RawQuery
	}

	accept := "application/json"
	if clientStream {
		accept = "text/event-stream"
	}

	client, proxyID := getClient(upstreamClient, "grok")
	total := am.Len()

	// Sticky session: client may pin all requests in a conversation to the
	// same upstream account via header (prompt-cache locality). First header
	// wins: x-session-id, x-conversation-id, x-chat-id.
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
	// message) for content-hash / hybrid selection modes.
	sysHash := ""
	{
		var tm map[string]any
		if json.Unmarshal(body, &tm) == nil {
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
	mode := GetGrokSelectorMode()

	var lastResp *http.Response
	var lastAcc *GrokAccount
	reqStart := time.Now()

	// Mode-selected account gets the first attempts before falling back to RR.
	// RR mode: no preference — every attempt is plain round-robin.
	stickyAttempts := 0
	if mode != GrokSelectorRR {
		stickyAttempts = 2
	}

	for attempt := 0; attempt < total+stickyAttempts; attempt++ {
		// C10: bail out if the client already went away so we don't burn
		// upstream tokens walking the account list for a dead request.
		if err := c.Request.Context().Err(); err != nil {
			slog.Debug("client cancelled before attempt", "module", "grok", "attempt", attempt+1, "error", err)
			return
		}
		var acc *GrokAccount
		var err error
		if attempt < stickyAttempts {
			acc, err = am.NextForMode(mode, sessionID, sysHash)
		} else {
			acc, err = am.Next()
		}
		if err != nil {
			break
		}
		token := acc.GetAccessToken()
		headers := grokHeaders(token, accept, model, acc.Sub, acc.Email)

		req, _ := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, upstreamURL, bytes.NewReader(body))
		req.Header = headers
		resp, err := client.Do(req)
		if err != nil {
			markProxyResult(proxyID, err, 0)
			slog.Warn("network attempt failed", "module", "grok", "attempt", attempt+1, "total", total, "email", acc.Email, "error", err)
			continue
		}
		markProxyResult(proxyID, nil, resp.StatusCode)

		if resp.StatusCode == 401 {
			resp.Body.Close()
			refreshErr := acc.Refresh()
			if refreshErr != nil {
				// C8: refresh_token itself was revoked → permanent disable,
				// matching the pre-warm worker's invalid_grant handling.
				if strings.Contains(refreshErr.Error(), "invalid_grant") {
					acc.mu.Lock()
					acc.disabled = true
					acc.disabledAt = time.Time{}
					acc.mu.Unlock()
					if acc.db != nil {
						saveGrokAccount(acc.db, acc.toDTO())
					}
					am.UnbindSticky(sessionID, acc)
					slog.Warn("401 refresh invalid_grant, permanent disable", "module", "grok", "email", acc.Email)
				} else {
					slog.Warn("401 refresh failed", "module", "grok", "email", acc.Email, "error", refreshErr)
				}
				continue
			}
			req, _ = http.NewRequestWithContext(c.Request.Context(), c.Request.Method, upstreamURL, bytes.NewReader(body))
			req.Header = grokHeaders(acc.GetAccessToken(), accept, model, acc.Sub, acc.Email)
			resp, err = client.Do(req)
			if err != nil {
				markProxyResult(proxyID, err, 0)
				continue
			}
			markProxyResult(proxyID, nil, resp.StatusCode)
			// C8: second 401 after a successful refresh means the token
			// pair is stale in a way refresh can't fix (server-side
			// revocation, wrong client_id, etc). Disable permanently so
			// we don't loop the same account forever and never return
			// a stale 401 to the client.
			if resp.StatusCode == 401 {
				resp.Body.Close()
				acc.mu.Lock()
				acc.disabled = true
				acc.disabledAt = time.Time{}
				acc.mu.Unlock()
				if acc.db != nil {
					saveGrokAccount(acc.db, acc.toDTO())
				}
				am.UnbindSticky(sessionID, acc)
				slog.Warn("401 after refresh, permanent disable", "module", "grok", "email", acc.Email)
				continue
			}
		}

		// 429 = rate limited. Cooldown the account (not permanent) and rotate.
		// Parse Retry-After header for backoff (Grok sends this on 429).
		if resp.StatusCode == 429 {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			bodyStr := string(bodyBytes)
			retryAfter := resp.Header.Get("Retry-After")
			cooldownDur := GROK_RATE_LIMIT_COOLDOWN // default 60s
			if retryAfter != "" {
				if secs, err := strconv.Atoi(retryAfter); err == nil && secs > 0 && secs < 600 {
					cooldownDur = time.Duration(secs) * time.Second
				}
			}
			acc.mu.Lock()
			acc.disabled = true
			acc.disabledAt = time.Now().Add(cooldownDur)
			acc.mu.Unlock()
			if acc.db != nil {
				saveGrokAccount(acc.db, acc.toDTO())
			}
			slog.Warn("upstream rate limited, cooldown",
				"module", "grok",
				"email", acc.Email,
				"status", 429,
				"retry_after", retryAfter,
				"cooldown_secs", int(cooldownDur.Seconds()),
				"body", truncateLog(bodyStr, 200))
			continue
		}

		// 400 = bad request (invalid model, context_length_exceeded, image error,
		// encrypted_content). NOT an account problem — pass through to client
		// immediately without disabling or retrying.
		if resp.StatusCode == 400 {
			lastResp = resp
			lastAcc = acc
			slog.Debug("upstream 400 bad request, pass through",
				"module", "grok",
				"email", acc.Email,
				"status", 400)
			break
		}

		// 402 = payment required (xAI free-tier spending-limit / personal-team-blocked)
		// 403 = forbidden (ban / temp block). Both must rotate to next account —
		// otherwise a single exhausted free account short-circuits the whole RR pool.
		if resp.StatusCode == 402 || resp.StatusCode == 403 {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			bodyStr := string(bodyBytes)
			acc.mu.Lock()
			acc.disabled = true
			// spending-limit arrives as 402 (code personal-team-blocked:spending-limit)
			// or historically as 403 — treat both as permanent disable so Next() skips them.
			if resp.StatusCode == 402 ||
				strings.Contains(bodyStr, "spending-limit") ||
				strings.Contains(bodyStr, "spending_limit") ||
				strings.Contains(bodyStr, "personal-team-blocked") ||
				strings.Contains(bodyStr, "banned") ||
				strings.Contains(bodyStr, "suspended") ||
				strings.Contains(bodyStr, "permanently") {
				acc.disabledAt = time.Time{}
				am.UnbindSticky(sessionID, acc)
				slog.Warn("upstream permanent disable", "module", "grok", "status", resp.StatusCode, "email", acc.Email, "body", truncateLog(bodyStr, 200))
			} else {
				acc.disabledAt = time.Now()
				slog.Warn("upstream cooldown", "module", "grok", "status", resp.StatusCode, "email", acc.Email, "body", truncateLog(bodyStr, 200))
			}
			acc.mu.Unlock()
			if acc.db != nil {
				saveGrokAccount(acc.db, acc.toDTO())
			}
			continue
		}

		if resp.StatusCode >= 500 {
			resp.Body.Close()
			hc.Grok.RecordRequest(time.Since(reqStart), fmt.Errorf("upstream %d", resp.StatusCode))
			slog.Warn("upstream error", "module", "grok", "email", acc.Email, "status", resp.StatusCode)
			continue
		}

		lastResp = resp
		lastAcc = acc
		break
	}

	if lastResp == nil {
		c.JSON(503, gin.H{"error": "all grok accounts on cooldown"})
		c.Set("error_msg", "all grok accounts on cooldown")
		errJSON, _ := json.Marshal(gin.H{"error": "all grok accounts on cooldown"})
		c.Set("response_body", json.RawMessage(errJSON))
		return
	}

	hc.Grok.RecordRequest(time.Since(reqStart), nil)
	c.Set("upstream_account", lastAcc.Email)

	defer lastResp.Body.Close()

	copyUpstreamHeaders(c.Writer.Header(), lastResp.Header)
	c.Writer.WriteHeader(lastResp.StatusCode)

	if strings.Contains(lastResp.Header.Get("Content-Type"), "text/event-stream") {
		flusher, _ := c.Writer.(http.Flusher)
		bufPtr := sseBufPool.Get().(*[]byte)
		buf := *bufPtr
		if cap(buf) < 4096 {
			buf = make([]byte, 4096)
		} else {
			buf = buf[:4096]
		}
		defer func() {
			*bufPtr = buf[:0]
			sseBufPool.Put(bufPtr)
		}()

		// C6: honour client cancellation. Without this, a disconnected
		// client keeps the upstream stream burning tokens for up to the
		// full 300s read timeout. We poll ctx.Err() at the top of every
		// iteration and stop copying on Writer error (client TCP dead).
		ctx := c.Request.Context()

		var streamContent strings.Builder
		var streamTokensIn, streamTokensOut int
		var lineCarry string
		for {
			if err := ctx.Err(); err != nil {
				slog.Debug("sse loop: client cancelled", "module", "grok", "error", err)
				lastResp.Body.Close()
				break
			}
			n, err := lastResp.Body.Read(buf)
			if n > 0 {
				if _, werr := c.Writer.Write(buf[:n]); werr != nil {
					slog.Debug("sse loop: write to client failed", "module", "grok", "error", werr)
					lastResp.Body.Close()
					break
				}
				if flusher != nil {
					flusher.Flush()
				}
				chunk := lineCarry + string(buf[:n])
				parts := strings.Split(chunk, "\n")
				lineCarry = parts[len(parts)-1]
				for _, line := range parts[:len(parts)-1] {
					line = strings.TrimSpace(line)
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					data := strings.TrimPrefix(line, "data: ")
					if data == "[DONE]" || data == "" {
						continue
					}
					var sc sseChunk
					if json.Unmarshal([]byte(data), &sc) != nil {
						continue
					}
					if len(sc.Choices) > 0 {
						streamContent.WriteString(sc.Choices[0].Delta.Content)
					}
					if sc.Usage != nil {
						if pt, ok := sc.Usage["prompt_tokens"].(float64); ok {
							streamTokensIn = int(pt)
						}
						if ct, ok := sc.Usage["completion_tokens"].(float64); ok {
							streamTokensOut = int(ct)
						}
					}
				}
			}
			if err != nil {
				break
			}
		}
		c.Set("output_text", truncateLog(streamContent.String(), 1000))
		c.Set("tokens_in", streamTokensIn)
		c.Set("tokens_out", streamTokensOut)
		// Accumulate per-account usage (telemetry, non-blocking)
		if lastAcc != nil {
			lastAcc.RecordUsage(streamTokensIn, streamTokensOut)
		}
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
			"model":  model,
			"stream": true,
		})
		c.Set("response_body", json.RawMessage(respJSON))
	} else {
		bodyBytes, _ := io.ReadAll(io.LimitReader(lastResp.Body, 10<<20))
		c.Writer.Write(bodyBytes)
		var result map[string]any
		if json.Unmarshal(bodyBytes, &result) == nil {
			c.Set("response_body", json.RawMessage(bodyBytes))
			if choices, ok := result["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					if msg, ok := choice["message"].(map[string]any); ok {
						if content, ok := msg["content"].(string); ok {
							c.Set("output_text", truncateLog(content, 1000))
						}
					}
				}
			}
			if usage, ok := result["usage"].(map[string]any); ok {
				pt, _ := usage["prompt_tokens"].(float64)
				ct, _ := usage["completion_tokens"].(float64)
				c.Set("tokens_in", int(pt))
				c.Set("tokens_out", int(ct))
				// Accumulate per-account usage (telemetry, non-blocking)
				if lastAcc != nil {
					lastAcc.RecordUsage(int(pt), int(ct))
				}
			}
		}
	}
}

// sseChunk is a shared SSE parse target (single unmarshal).
type sseChunk struct {
	Error   any `json:"error"`
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage map[string]any `json:"usage"`
}

// sseBufPool reuses read buffers for stream proxying.
var sseBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 4096)
		return &b
	},
}

// CleanupDisabled removes all permanently disabled grok accounts (disabledAt is zero time).
// Returns the count of removed accounts. Does NOT affect cooldown accounts (disabledAt set).
// Note: permanently disabled accounts are the same ones reported as token_status="banned".
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
