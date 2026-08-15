// Package upstream — Alibaba Cloud Model Studio (DashScope) provider.
//
// Alibaba is a plain OpenAI-compatible upstream: Bearer sk-ws-* API key,
// POST https://dashscope-intl.aliyuncs.com/compatible-mode/v1/chat/completions.
// No session lifecycle, no OAuth device flow — just keys, round-robin,
// per-key usage tracking and circuit breaker.
package upstream

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"log/slog"

	"foxrouters/internal/db"
)

const (
	alibabaChatURL  = "https://dashscope-intl.aliyuncs.com/compatible-mode/v1/chat/completions"
	alibabaRedisNS  = "ali:key:"       // Redis hash prefix for keys
	alibabaCooldown = 60 * time.Second // 429 cooldown
)

// alibabaDisabled is set once at init from ALIBABA_DISABLED env gate.
var alibabaDisabled = os.Getenv("ALIBABA_DISABLED") == "1"

// AlibabaModelConfig maps a gateway ID (ali/…) to the upstream model name.
type AlibabaModelConfig struct {
	Gateway   string
	Upstream  string
	Reasoning bool // emits reasoning_content
}

var alibabaModelConfigs = []AlibabaModelConfig{
	{Gateway: "ali/qwen3.8-max", Upstream: "qwen3.8-max", Reasoning: true},
	{Gateway: "ali/qwen-turbo", Upstream: "qwen-turbo", Reasoning: false},
	{Gateway: "ali/qwen-plus", Upstream: "qwen-plus", Reasoning: false},
	{Gateway: "ali/deepseek-v4-flash", Upstream: "deepseek-v4-flash", Reasoning: true},
	{Gateway: "ali/glm-5.2", Upstream: "glm-5.2", Reasoning: true},
	{Gateway: "ali/kimi-k2.7-code", Upstream: "kimi-k2.7-code", Reasoning: true},
}

// IsAlibabaModel reports whether the gateway model id routes to Alibaba.
func IsAlibabaModel(model string) bool { return strings.HasPrefix(model, "ali/") }

// aliStripPrefix removes the "ali/" prefix → upstream model name.
func aliStripPrefix(model string) string { return strings.TrimPrefix(model, "ali/") }

// AlibabaModelConfigByGateway returns the config for a gateway model id
// (ali/…) or nil if unknown.
func AlibabaModelConfigByGateway(gatewayID string) *AlibabaModelConfig {
	for i := range alibabaModelConfigs {
		if alibabaModelConfigs[i].Gateway == gatewayID {
			return &alibabaModelConfigs[i]
		}
	}
	return nil
}

// AlibabaModelList exposes the full static catalog (for /v1/models).
func AlibabaModelList() []AlibabaModelConfig {
	return alibabaModelConfigs
}

// AlibabaKey is a single DashScope API key in the pool.
type AlibabaKey struct {
	mu sync.RWMutex

	Key            string // sk-ws-* (also the Redis hash name)
	Email          string
	Disabled       bool
	DisabledReason string
	DisabledAt     time.Time

	Requests  int64
	TokensIn  int64
	TokensOut int64
	Errors    int64
	LastError string

	CooldownUntil  time.Time
	CooldownReason string

	db *db.Store
}

// AlibabaKeyManager is the pool of DashScope keys (ordered slice, RR cursor).
type AlibabaKeyManager struct {
	keys []*AlibabaKey
	mu   sync.RWMutex
	idx  uint64
	db   *db.Store
}

func NewAlibabaKeyManager(store *db.Store) *AlibabaKeyManager {
	return &AlibabaKeyManager{db: store}
}

// ---- Redis persistence ------------------------------------------------

// LoadFromRedis restores keys persisted as HSET ali:key:<fullkey>.
func (am *AlibabaKeyManager) LoadFromRedis() error {
	am.mu.Lock()
	defer am.mu.Unlock()
	if am.db == nil {
		return nil
	}
	keys, err := am.db.Redis().Keys(aliCtx(am.db), alibabaRedisNS+"*").Result()
	if err != nil {
		return err
	}
	for _, k := range keys {
		fields, err := am.db.Redis().HGetAll(aliCtx(am.db), k).Result()
		if err != nil || len(fields) == 0 {
			continue
		}
		key := strings.TrimPrefix(k, alibabaRedisNS)
		acc := &AlibabaKey{Key: key, Email: fields["email"], db: am.db}
		acc.Disabled = fields["disabled"] == "1"
		acc.DisabledReason = fields["disabled_reason"]
		if ts, ok := fields["disabled_at"]; ok && ts != "" {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				acc.DisabledAt = t
			}
		}
		if v := fields["requests"]; v != "" {
			fmt.Sscanf(v, "%d", &acc.Requests)
		}
		if v := fields["tokens_in"]; v != "" {
			fmt.Sscanf(v, "%d", &acc.TokensIn)
		}
		if v := fields["tokens_out"]; v != "" {
			fmt.Sscanf(v, "%d", &acc.TokensOut)
		}
		if v := fields["errors"]; v != "" {
			fmt.Sscanf(v, "%d", &acc.Errors)
		}
		acc.LastError = fields["last_error"]
		if v := fields["cooldown_until"]; v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				acc.CooldownUntil = t
			}
		}
		am.keys = append(am.keys, acc)
	}
	sort.Slice(am.keys, func(i, j int) bool { return am.keys[i].Key < am.keys[j].Key })
	slog.Info("alibaba keys loaded from redis", "count", len(am.keys))
	return nil
}

// Save persists the key to Redis (fire-and-forget on error).
func (am *AlibabaKeyManager) Save(acc *AlibabaKey) {
	if am.db == nil {
		return
	}
	acc.mu.RLock()
	m := map[string]any{
		"email":           acc.Email,
		"disabled":        boolInt(acc.Disabled),
		"disabled_reason": acc.DisabledReason,
		"disabled_at":     timeToStr(acc.DisabledAt),
		"requests":        acc.Requests,
		"tokens_in":       acc.TokensIn,
		"tokens_out":      acc.TokensOut,
		"errors":          acc.Errors,
		"last_error":      acc.LastError,
		"cooldown_until":  timeToStr(acc.CooldownUntil),
		"cooldown_reason": acc.CooldownReason,
	}
	acc.mu.RUnlock()
	if err := am.db.Redis().HSet(aliCtx(am.db), alibabaRedisNS+acc.Key, m).Err(); err != nil {
		slog.Warn("alibaba save failed", "key", keyPrefix(acc.Key), "error", err)
	}
}

// ---- Pool ops ---------------------------------------------------------

// AddAccount inserts a key (by full sk-ws-* value). Returns added/total.
func (am *AlibabaKeyManager) AddAccount(key, email string) (added bool, total int, err error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return false, 0, fmt.Errorf("key is required")
	}
	am.mu.Lock()
	defer am.mu.Unlock()
	for _, k := range am.keys {
		if k.Key == key {
			// update email if empty, keep state
			if email != "" && k.Email == "" {
				k.Email = email
				am.saveNoLock(k)
			}
			return false, len(am.keys), nil
		}
	}
	acc := &AlibabaKey{Key: key, Email: email, db: am.db}
	am.keys = append(am.keys, acc)
	sort.Slice(am.keys, func(i, j int) bool { return am.keys[i].Key < am.keys[j].Key })
	am.saveNoLock(acc)
	return true, len(am.keys), nil
}

func (am *AlibabaKeyManager) saveNoLock(acc *AlibabaKey) {
	if am.db == nil {
		return
	}
	acc.mu.RLock()
	m := map[string]any{
		"email":           acc.Email,
		"disabled":        boolInt(acc.Disabled),
		"disabled_reason": acc.DisabledReason,
		"disabled_at":     timeToStr(acc.DisabledAt),
		"requests":        acc.Requests,
		"tokens_in":       acc.TokensIn,
		"tokens_out":      acc.TokensOut,
		"errors":          acc.Errors,
		"last_error":      acc.LastError,
		"cooldown_until":  timeToStr(acc.CooldownUntil),
		"cooldown_reason": acc.CooldownReason,
	}
	acc.mu.RUnlock()
	if err := am.db.Redis().HSet(aliCtx(am.db), alibabaRedisNS+acc.Key, m).Err(); err != nil {
		slog.Warn("alibaba save failed", "key", keyPrefix(acc.Key), "error", err)
	}
}

// RemoveAccount deletes a key from the pool + Redis.
func (am *AlibabaKeyManager) RemoveAccount(key string) error {
	am.mu.Lock()
	defer am.mu.Unlock()
	for i, k := range am.keys {
		if k.Key == key {
			am.keys = append(am.keys[:i], am.keys[i+1:]...)
			if am.db != nil {
				am.db.Redis().Del(aliCtx(am.db), alibabaRedisNS+key)
			}
			return nil
		}
	}
	return fmt.Errorf("key not found")
}

// Len returns the pool size.
func (am *AlibabaKeyManager) Len() int {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return len(am.keys)
}

// Get returns the key by full value (nil if absent).
func (am *AlibabaKeyManager) Get(key string) *AlibabaKey {
	am.mu.RLock()
	defer am.mu.RUnlock()
	for _, k := range am.keys {
		if k.Key == key {
			return k
		}
	}
	return nil
}

// Next returns the next eligible key (not disabled, not in cooldown),
// round-robin from the cursor. Returns error when the pool has no eligible key.
func (am *AlibabaKeyManager) Next() (*AlibabaKey, error) {
	// Snapshot the pool once under RLock — a concurrent RemoveAccount
	// (mu.Lock) between two RLock windows could shrink the slice and make
	// am.keys[idx] panic (index out of range → whole gateway crash).
	am.mu.RLock()
	keys := append([]*AlibabaKey(nil), am.keys...)
	n := len(keys)
	am.mu.RUnlock()
	if n == 0 {
		return nil, fmt.Errorf("no alibaba keys")
	}
	start := int(atomic.LoadUint64(&am.idx) % uint64(n))
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		k := keys[idx]
		k.mu.RLock()
		ok := !k.Disabled && time.Now().After(k.CooldownUntil)
		k.mu.RUnlock()
		if ok {
			atomic.AddUint64(&am.idx, 1)
			return k, nil
		}
	}
	return nil, fmt.Errorf("no eligible alibaba keys (all disabled or in cooldown)")
}

// DisableKey permanently disables a key (rule: disable, never delete).
func (am *AlibabaKeyManager) DisableKey(key, reason string) error {
	acc := am.Get(key)
	if acc == nil {
		return fmt.Errorf("key not found")
	}
	acc.mu.Lock()
	acc.Disabled = true
	acc.DisabledReason = reason
	acc.DisabledAt = time.Now()
	acc.mu.Unlock()
	am.Save(acc)
	return nil
}

// EnableKey re-enables a disabled key.
func (am *AlibabaKeyManager) EnableKey(key string) error {
	acc := am.Get(key)
	if acc == nil {
		return fmt.Errorf("key not found")
	}
	acc.mu.Lock()
	acc.Disabled = false
	acc.DisabledReason = ""
	acc.DisabledAt = time.Time{}
	acc.CooldownUntil = time.Time{}
	acc.mu.Unlock()
	am.Save(acc)
	return nil
}

// RecordUsage accumulates per-key token/request counters and persists.
func (am *AlibabaKeyManager) RecordUsage(key string, tokIn, tokOut int64) {
	acc := am.Get(key)
	if acc == nil {
		return
	}
	acc.mu.Lock()
	acc.Requests++
	acc.TokensIn += tokIn
	acc.TokensOut += tokOut
	acc.mu.Unlock()
	am.Save(acc)
}

// RecordError bumps the error counter + last error message.
func (am *AlibabaKeyManager) RecordError(key, msg string) {
	acc := am.Get(key)
	if acc == nil {
		return
	}
	acc.mu.Lock()
	acc.Errors++
	if msg != "" {
		acc.LastError = truncateLog(msg, 200)
	}
	acc.mu.Unlock()
	am.Save(acc)
}

// SetCooldown puts a key into cooldown (429 / transient).
func (am *AlibabaKeyManager) SetCooldown(key, reason string, d time.Duration) {
	acc := am.Get(key)
	if acc == nil {
		return
	}
	acc.mu.Lock()
	acc.CooldownUntil = time.Now().Add(d)
	acc.CooldownReason = reason
	acc.mu.Unlock()
	am.Save(acc)
}

// ListAccounts returns DTOs for the dashboard. The full sk-ws-* secret is
// NEVER included — only a masked display string + an opaque key hash.
func (am *AlibabaKeyManager) ListAccounts() []map[string]any {
	am.mu.RLock()
	defer am.mu.RUnlock()
	out := make([]map[string]any, 0, len(am.keys))
	for _, k := range am.keys {
		k.mu.RLock()
		out = append(out, map[string]any{
			"key_masked":      keyPrefix(k.Key),
			"key_hash":        AliKeyHash(k.Key),
			"email":           k.Email,
			"disabled":        k.Disabled,
			"disabled_reason": k.DisabledReason,
			"disabled_at":     timeToStr(k.DisabledAt),
			"requests":        k.Requests,
			"tokens_in":       k.TokensIn,
			"tokens_out":      k.TokensOut,
			"errors":          k.Errors,
			"last_error":      k.LastError,
			"cooldown_until":  timeToStr(k.CooldownUntil),
			"cooldown_reason": k.CooldownReason,
		})
		k.mu.RUnlock()
	}
	return out
}

// ActiveKeyCount returns the number of keys available for routing right now
// (not permanently disabled, not in cooldown). Used to scale the per-model
// free-tier limit: quota is 1M PER KEY, so N active keys = N × limit.
func (am *AlibabaKeyManager) ActiveKeyCount() int {
	am.mu.RLock()
	defer am.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, k := range am.keys {
		k.mu.RLock()
		if !k.Disabled && !k.CooldownUntil.After(now) {
			n++
		}
		k.mu.RUnlock()
	}
	return n
}

// QuotaSummary aggregates pool counters for the health endpoint.
func (am *AlibabaKeyManager) QuotaSummary() (totalReq, totalErr int64, active int) {
	am.mu.RLock()
	defer am.mu.RUnlock()
	for _, k := range am.keys {
		k.mu.RLock()
		totalReq += k.Requests
		totalErr += k.Errors
		if !k.Disabled {
			active++
		}
		k.mu.RUnlock()
	}
	return
}

// ---- helpers ----------------------------------------------------------

// AliKeyHash returns a stable opaque ID for a key (SHA-256, 24 hex chars).
// Used in DTOs/APIs so the full sk-ws-* secret never leaves the server.
func AliKeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:12])
}

// FindKeyByHash resolves an opaque key hash back to the full key, or ""
// if no key matches.
func (am *AlibabaKeyManager) FindKeyByHash(hash string) string {
	if hash == "" {
		return ""
	}
	am.mu.RLock()
	defer am.mu.RUnlock()
	for _, k := range am.keys {
		if AliKeyHash(k.Key) == hash {
			return k.Key
		}
	}
	return ""
}

func keyPrefix(key string) string {
	if len(key) > 12 {
		return key[:12] + "…"
	}
	return key
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func timeToStr(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// keep unused imports happy if build tags change the file
var _ = json.Marshal
var _ = http.StatusOK
var _ = gin.H{}

// ProxyAlibaba proxies an OpenAI-compatible request to DashScope.
// Model routing: gateway id ali/… → upstream model name (strip prefix).
// Error handling mirrors the CodeBuddy policy: 400s passthrough (no rotate,
// no disable), 401 permanent disable, 429 cooldown + rotate, 5xx rotate.
func ProxyAlibaba(c *gin.Context, body []byte, bodyMap map[string]any, am *AlibabaKeyManager, clientStream bool, hc *HealthChecker) {
	if alibabaDisabled || am == nil || am.Len() == 0 {
		c.JSON(503, gin.H{"error": "alibaba upstream not available"})
		c.Set("error_msg", "alibaba not available")
		errJSON, _ := json.Marshal(gin.H{"error": "alibaba upstream not available"})
		c.Set("response_body", json.RawMessage(errJSON))
		return
	}
	if hc != nil && hc.Ali != nil && !hc.Ali.CanRequest() {
		hc.Ali.RecordRequest(0, fmt.Errorf("circuit open"))
		c.JSON(503, gin.H{"error": "alibaba upstream circuit breaker open"})
		c.Set("error_msg", "ali circuit breaker open")
		errJSON, _ := json.Marshal(gin.H{"error": "alibaba circuit breaker open"})
		c.Set("response_body", json.RawMessage(errJSON))
		return
	}

	// Resolve the upstream model name.
	gatewayModel, _ := bodyMap["model"].(string)
	upstreamModel := aliStripPrefix(gatewayModel)
	if upstreamModel != gatewayModel {
		// Rewrite body so the upstream receives the bare model name.
		bodyMap["model"] = upstreamModel
		if b, err := json.Marshal(bodyMap); err == nil {
			body = b
		}
	}

	total := am.Len()
	client, _ := getClient(upstreamClient, "alibaba")

	var lastErr string
	for attempt := 0; attempt < total; attempt++ {
		if err := c.Request.Context().Err(); err != nil {
			return
		}
		acc, err := am.Next()
		if err != nil {
			lastErr = fmt.Sprintf("next: %v", err)
			slog.Warn("ali no eligible key", "module", "alibaba", "model", upstreamModel, "error", err)
			break
		}

		// Fresh body per attempt (retry safety).
		bodyBytes := body
		if attempt > 0 {
			re, _ := json.Marshal(bodyMap)
			bodyBytes = re
		}

		req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, alibabaChatURL, strings.NewReader(string(bodyBytes)))
		if err != nil {
			am.RecordError(acc.Key, "req build: "+err.Error())
			continue
		}
		req.Header.Set("Authorization", "Bearer "+acc.Key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "foxrouters/1.6.13")

		resp, err := client.Do(req)
		if err != nil {
			am.RecordError(acc.Key, err.Error())
			lastErr = err.Error()
			if hc != nil && hc.Ali != nil {
				hc.Ali.RecordRequest(0, err)
			}
			continue
		}

		switch {
		case resp.StatusCode == 401:
			// Key invalid/revoked — permanent disable.
			bodyErr := readErrBody(resp)
			am.DisableKey(acc.Key, "401 "+truncateLog(bodyErr, 120))
			slog.Warn("ali key 401, disabling", "module", "alibaba", "key", keyPrefix(acc.Key), "err", bodyErr)
			resp.Body.Close()
			lastErr = "401 unauthorized"
			continue

		case resp.StatusCode == 429:
			// Rate limit — cooldown then rotate.
			bodyErr := readErrBody(resp)
			am.SetCooldown(acc.Key, "429 "+truncateLog(bodyErr, 120), alibabaCooldown)
			slog.Warn("ali key rate limited", "module", "alibaba", "key", keyPrefix(acc.Key), "err", bodyErr)
			resp.Body.Close()
			lastErr = "429 rate limited"
			if hc != nil && hc.Ali != nil {
				hc.Ali.RecordRequest(0, fmt.Errorf("429"))
			}
			continue

		case resp.StatusCode >= 400 && resp.StatusCode < 500:
			// Client error — passthrough, no rotate, no disable.
			bodyErr := readErrBody(resp)
			if resp.StatusCode == 403 && strings.Contains(bodyErr, "AccessDenied.Unpurchased") {
				// Key has no free-tier entitlement for this model — permanent
				// disable so RR never picks it again (rotate to next key).
				am.DisableKey(acc.Key, "403 AccessDenied.Unpurchased")
				slog.Warn("ali key AccessDenied.Unpurchased, disabling", "module", "alibaba", "key", keyPrefix(acc.Key))
				resp.Body.Close()
				lastErr = "403 AccessDenied.Unpurchased"
				continue
			}
			am.RecordError(acc.Key, fmt.Sprintf("%d %s", resp.StatusCode, truncateLog(bodyErr, 120)))
			c.Data(resp.StatusCode, "application/json", []byte(bodyErr))
			c.Set("error_msg", bodyErr)
			c.Set("response_body", json.RawMessage(bodyErr))
			resp.Body.Close()
			return // pass the 4xx straight back to the client

		case resp.StatusCode >= 500:
			// Upstream error — rotate to next key.
			bodyErr := readErrBody(resp)
			am.RecordError(acc.Key, fmt.Sprintf("%d %s", resp.StatusCode, truncateLog(bodyErr, 120)))
			lastErr = bodyErr
			if hc != nil && hc.Ali != nil {
				hc.Ali.RecordRequest(0, fmt.Errorf("upstream %d", resp.StatusCode))
			}
			resp.Body.Close()
			continue
		}

		// Success — stream or aggregate.
		if clientStream || isStreamRequest(bodyBytes) {
			aliStreamPassthrough(c, resp, acc, am, upstreamModel)
		} else {
			aliNonStream(c, resp, acc, am, upstreamModel)
		}
		if hc != nil && hc.Ali != nil {
			hc.Ali.RecordRequest(1, nil)
		}
		return
	}

	if c.Writer.Written() {
		return
	}
	c.JSON(503, gin.H{"error": "all alibaba keys failed", "detail": truncateLog(lastErr, 200)})
	c.Set("error_msg", "all alibaba keys failed: "+truncateLog(lastErr, 200))
	errJSON, _ := json.Marshal(gin.H{"error": "all alibaba keys failed"})
	c.Set("response_body", json.RawMessage(errJSON))
}

// readErrBody drains a response body into a bounded string.
func readErrBody(resp *http.Response) string {
	b := make([]byte, 0, 512)
	tmp := make([]byte, 512)
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			b = append(b, tmp[:n]...)
			if len(b) > 4096 {
				break
			}
		}
		if err != nil {
			break
		}
	}
	return string(b)
}

func isStreamRequest(body []byte) bool {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	b, _ := m["stream"].(bool)
	return b
}

// aliStreamPassthrough relays the SSE stream and captures usage from the last
// chunk so logging middleware records tokens + cache_hit_pct.
func aliStreamPassthrough(c *gin.Context, resp *http.Response, acc *AlibabaKey, am *AlibabaKeyManager, model string) {
	defer resp.Body.Close()
	c.Header("Content-Type", "text/event-stream")
	c.Writer.WriteHeader(resp.StatusCode)

	reader := bufio.NewReader(resp.Body)
	var streamBuf strings.Builder
	var streamUsage map[string]any
	var gotChunk bool

	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			gotChunk = true
			if _, werr := c.Writer.WriteString(line); werr != nil {
				break
			}
			c.Writer.Flush()
			streamBuf.WriteString(line)
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "data: ") {
				data := strings.TrimPrefix(trimmed, "data: ")
				if data != "[DONE]" && data != "" {
					var chunk map[string]any
					if json.Unmarshal([]byte(data), &chunk) == nil {
						if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
							streamUsage = u
						}
					}
				}
			}
		}
		if err != nil {
			break
		}
	}

	var tokIn, tokOut int64
	if streamUsage != nil {
		if v, ok := streamUsage["prompt_tokens"].(float64); ok {
			tokIn = int64(v)
		}
		if v, ok := streamUsage["completion_tokens"].(float64); ok {
			tokOut = int64(v)
		}
	}
	am.RecordUsage(acc.Key, tokIn, tokOut)
	am.RecordUsageModel(model, tokIn, tokOut)
	c.Set("tokens_in", int(tokIn))
	c.Set("tokens_out", int(tokOut))
	if gotChunk && streamBuf.Len() > 0 {
		c.Set("response_body", json.RawMessage(streamBuf.String()))
	}
}

// aliNonStream forwards the JSON response and captures usage.
func aliNonStream(c *gin.Context, resp *http.Response, acc *AlibabaKey, am *AlibabaKeyManager, model string) {
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		am.RecordError(acc.Key, "read: "+err.Error())
		c.JSON(502, gin.H{"error": "alibaba read failed"})
		return
	}
	c.Data(resp.StatusCode, "application/json", bodyBytes)

	var tokIn, tokOut int64
	var parsed map[string]any
	if json.Unmarshal(bodyBytes, &parsed) == nil {
		if u, ok := parsed["usage"].(map[string]any); ok {
			if v, ok := u["prompt_tokens"].(float64); ok {
				tokIn = int64(v)
			}
			if v, ok := u["completion_tokens"].(float64); ok {
				tokOut = int64(v)
			}
		}
	}
	am.RecordUsage(acc.Key, tokIn, tokOut)
	am.RecordUsageModel(model, tokIn, tokOut)
	c.Set("tokens_in", int(tokIn))
	c.Set("tokens_out", int(tokOut))
	c.Set("response_body", json.RawMessage(bodyBytes))
	// output_text for dashboard preview
	if parsed != nil {
		if choices, ok := parsed["choices"].([]any); ok && len(choices) > 0 {
			if first, ok := choices[0].(map[string]any); ok {
				if msg, ok := first["message"].(map[string]any); ok {
					if content, ok := msg["content"].(string); ok && content != "" {
						c.Set("output_text", truncateLog(content, 500))
					}
				}
			}
		}
	}
}

// aliCtx returns a short-lived context for a Redis call (the db.Store exposes
// no public context; a 3s bound keeps slow Redis from stalling the pool).
func aliCtx(s *db.Store) context.Context {
	if s == nil {
		return context.Background()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = cancel // Redis commands complete in ms; cancel is best-effort
	return ctx
}

// TestAlibabaKey probes a single DashScope key with a minimal chat request
// (qwen3.8-max "Say OK", 16 max tokens). Returns ok + model/usage on success.
func TestAlibabaKey(key string) (map[string]any, error) {
	payload := []byte(`{"model":"qwen3.8-max","messages":[{"role":"user","content":"Say OK"}],"max_tokens":16}`)
	req, err := http.NewRequest(http.MethodPost, alibabaChatURL, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "foxrouters/1.6.13")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := upstreamClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, truncateLog(string(bodyBytes), 200))
	}
	var parsed map[string]any
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		return nil, err
	}
	content := ""
	if choices, ok := parsed["choices"].([]any); ok && len(choices) > 0 {
		if first, ok := choices[0].(map[string]any); ok {
			if msg, ok := first["message"].(map[string]any); ok {
				content, _ = msg["content"].(string)
			}
		}
	}
	return map[string]any{"ok": true, "model": "qwen3.8-max", "reply": truncateLog(content, 120)}, nil
}
