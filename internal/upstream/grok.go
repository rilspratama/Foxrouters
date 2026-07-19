package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/singleflight"

	"foxrouters/internal/db"
)

// refreshSF collapses concurrent Refresh() for the same account email.
var refreshSF singleflight.Group

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
	resp, err := tokenRefreshClient.Do(req)
	if err != nil {
		return err
	}
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
		saveGrokAccount(a.db, a)
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
}

func NewGrokAccountManager(store *db.Store) *GrokAccountManager {
	return &GrokAccountManager{accounts: make([]*GrokAccount, 0), db: store}
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
			saveGrokAccount(acc.db, acc)
		}
		slog.Info("re-enabled cooldown account", "module", "grok", "email", acc.Email)
	}
}

// ReenableWorker is the long-lived goroutine that lifts cooldowns.
func ReenableWorker(am *GrokAccountManager) {
	am.ReenableCooldowns()
	ticker := time.NewTicker(REENABLE_TICK)
	defer ticker.Stop()
	for range ticker.C {
		am.ReenableCooldowns()
	}
}

// AutoRefreshWorker pre-warms tokens concurrently before they expire.
func AutoRefreshWorker(am *GrokAccountManager) {
	ticker := time.NewTicker(PRE_WARM_TICK)
	defer ticker.Stop()
	for range ticker.C {
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
							saveGrokAccount(acc.db, acc)
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
				saveGrokAccount(am.db, existing)
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
		saveGrokAccount(am.db, acc)
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
		saveGrokAccount(am.db, acc)
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

func grokHeaders(token, accept, model string) http.Header {
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

// ProxyGrok forwards a chat/completions (or /v1/*) request to Grok, retrying
// per-account on 401/403/5xx.
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

	client := upstreamClient
	total := am.Len()

	var lastResp *http.Response
	var lastAcc *GrokAccount
	reqStart := time.Now()

	for attempt := 0; attempt < total; attempt++ {
		acc, err := am.Next()
		if err != nil {
			break
		}
		token := acc.GetAccessToken()
		headers := grokHeaders(token, accept, model)

		req, _ := http.NewRequest(c.Request.Method, upstreamURL, bytes.NewReader(body))
		req.Header = headers
		resp, err := client.Do(req)
		if err != nil {
			slog.Warn("network attempt failed", "module", "grok", "attempt", attempt+1, "total", total, "email", acc.Email, "error", err)
			continue
		}

		if resp.StatusCode == 401 {
			resp.Body.Close()
			if err := acc.Refresh(); err == nil {
				req, _ = http.NewRequest(c.Request.Method, upstreamURL, bytes.NewReader(body))
				req.Header = grokHeaders(acc.GetAccessToken(), accept, model)
				resp, err = client.Do(req)
				if err != nil {
					continue
				}
			} else {
				continue
			}
		}

		if resp.StatusCode == 403 {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			bodyStr := string(bodyBytes)
			acc.mu.Lock()
			acc.disabled = true
			if strings.Contains(bodyStr, "spending-limit") ||
				strings.Contains(bodyStr, "spending_limit") ||
				strings.Contains(bodyStr, "banned") ||
				strings.Contains(bodyStr, "suspended") ||
				strings.Contains(bodyStr, "permanently") {
				acc.disabledAt = time.Time{}
				slog.Warn("403 permanent ban", "module", "grok", "email", acc.Email, "body", truncateLog(bodyStr, 200))
			} else {
				acc.disabledAt = time.Now()
				slog.Warn("403 cooldown", "module", "grok", "email", acc.Email, "body", truncateLog(bodyStr, 200))
			}
			acc.mu.Unlock()
			if acc.db != nil {
				saveGrokAccount(acc.db, acc)
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

	for k, v := range lastResp.Header {
		if strings.EqualFold(k, "Content-Encoding") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, vv := range v {
			c.Writer.Header().Add(k, vv)
		}
	}
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

		var streamContent strings.Builder
		var streamTokensIn, streamTokensOut int
		var lineCarry string
		for {
			n, err := lastResp.Body.Read(buf)
			if n > 0 {
				c.Writer.Write(buf[:n])
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
				if pt, ok := usage["prompt_tokens"].(float64); ok {
					c.Set("tokens_in", int(pt))
				}
				if ct, ok := usage["completion_tokens"].(float64); ok {
					c.Set("tokens_out", int(ct))
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
			Content string `json:"content"`
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
