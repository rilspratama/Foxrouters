package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/singleflight"
)

// refreshSF collapses concurrent Refresh() for the same account email.
var refreshSF singleflight.Group

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
	db           *DBStore
}

func (a *GrokAccount) IsExpired() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return time.Now().After(a.expiresAt.Add(-REFRESH_BUFFER))
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
	// Snapshot credentials under lock — do not hold lock across network I/O.
	a.mu.Lock()
	email := a.Email
	rt := a.RefreshToken
	a.mu.Unlock()

	log.Printf("[grok-refresh] %s refreshing...", email)
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
	// Limit response body to 1MB — OAuth token responses are ~3-5KB,
	// 1MB is 200x headroom. Prevents memory exhaustion from malformed responses.
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

	log.Printf("[grok-refresh] %s ok, expires %ds", email, expIn)
	// Persist outside lock (singleflight guarantees one writer per email)
	if a.db != nil {
		a.db.SaveGrokAccount(a)
		a.db.UpsertGrokAccount(a)
		a.db.LogRefresh(RefreshLog{
			Timestamp: time.Now(), AccountEmail: email, Provider: "grok",
			Success: true,
		})
		a.db.LogEvent(AccountEvent{
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
	db       *DBStore
}

func NewGrokAccountManager(db *DBStore) *GrokAccountManager {
	return &GrokAccountManager{accounts: make([]*GrokAccount, 0), db: db}
}

func (am *GrokAccountManager) Len() int {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return len(am.accounts)
}

// LoadFromRedis loads ALL grok accounts from Redis (single source of truth).
func (am *GrokAccountManager) LoadFromRedis() error {
	if am.db == nil || am.db.rdb == nil {
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
	log.Printf("[grok] loaded %d accounts from Redis", len(am.accounts))
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
		go bestAcc.Refresh() // singleflight inside Refresh
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

// reenableCooldowns lifts temp cooldowns past 10 minutes. Called by background
// worker — NOT from Next() — so request path stays O(k) with no Redis I/O.
func (am *GrokAccountManager) reenableCooldowns() {
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
			acc.db.SaveGrokAccount(acc)
		}
		log.Printf("[grok] re-enabled cooldown account %s", acc.Email)
	}
}

func reenableWorker(am *GrokAccountManager) {
	// Run once at start so recently-expired cooldowns recover quickly after restart
	am.reenableCooldowns()
	ticker := time.NewTicker(REENABLE_TICK)
	defer ticker.Stop()
	for range ticker.C {
		am.reenableCooldowns()
	}
}

// ============================================================================
// AUTO-REFRESH WORKER — pre-warm tokens concurrently, never block requests
// ============================================================================

func autoRefreshWorker(am *GrokAccountManager) {
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
							acc.db.SaveGrokAccount(acc)
						}
						log.Printf("[worker] %s revoked, disabled", acc.Email)
					} else {
						log.Printf("[worker] %s refresh error: %v", acc.Email, err)
					}
				}
			}(a)
		}
		wg.Wait()
	}
}

// ============================================================================
// MODEL ROUTING + GROK PROXY
// ============================================================================

// expandGrokAlias maps grok-4.5-{high,medium,low,auto,none} → reasoning_effort value.
// Returns (effort, true) if model is an alias, ("", false) otherwise.
// Mirrors 9router's grok-cli thinkingConfig: options ["low","medium","high","xhigh"], default "high".
func expandGrokAlias(model string) (string, bool) {
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

func isGrokModel(model string) bool {
	return strings.HasPrefix(model, "grok-")
}

func grokHeaders(token, accept, model string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", accept)
	// TUI-mimic headers (from grok-build source: inject_url_derived_headers)
	h.Set("X-XAI-Token-Auth", "xai-grok-cli")
	h.Set("x-authenticateresponse", "authenticate-response")
	h.Set("x-grok-client-version", GROK_CLIENT_VERSION)
	h.Set("x-grok-client-identifier", GROK_CLIENT_IDENTIFIER)
	h.Set("x-grok-client-mode", "tui")
	h.Set("User-Agent", fmt.Sprintf("grok-shell/%s (linux; x86_64)", GROK_CLIENT_VERSION))
	// Per-request TUI headers (GrokRequestHeaders from xai-grok-sampler)
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

func proxyGrok(c *gin.Context, body []byte, am *GrokAccountManager, clientStream bool, hc *HealthChecker, model string) {
	if !hc.grok.CanRequest() {
		hc.grok.RecordRequest(0, fmt.Errorf("circuit open"))
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
	total := am.Len() // O(1) — no GetAll copy

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
			log.Printf("[grok] attempt %d/%d %s network: %v", attempt+1, total, acc.Email, err)
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
				log.Printf("[grok] %s 403 PERMANENT BAN: %s", acc.Email, truncateLog(bodyStr, 200))
			} else {
				acc.disabledAt = time.Now()
				log.Printf("[grok] %s 403 cooldown: %s", acc.Email, truncateLog(bodyStr, 200))
			}
			acc.mu.Unlock()
			if acc.db != nil {
				acc.db.SaveGrokAccount(acc)
			}
			continue
		}

		if resp.StatusCode >= 500 {
			resp.Body.Close()
			hc.grok.RecordRequest(time.Since(reqStart), fmt.Errorf("upstream %d", resp.StatusCode))
			log.Printf("[grok] %s upstream %d", acc.Email, resp.StatusCode)
			continue
		}

		lastResp = resp
		lastAcc = acc
		break
	}

	if lastResp == nil {
		c.JSON(503, gin.H{"error": "all grok accounts on cooldown"})
		// Capture error for ClickHouse audit trail
		c.Set("error_msg", "all grok accounts on cooldown")
		errJSON, _ := json.Marshal(gin.H{"error": "all grok accounts on cooldown"})
		c.Set("response_body", json.RawMessage(errJSON))
		return
	}

	hc.grok.RecordRequest(time.Since(reqStart), nil)
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
				// Incremental line parse (carry partial lines across reads)
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
					// Single unmarshal with optional error field
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
		// Limit to 10MB — covers large completions but prevents OOM from runaway responses.
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
