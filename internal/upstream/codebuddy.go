package upstream

import (
	"bufio"
	"bytes"
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

	"foxrouters/internal/db"
)

// ============================================================================
// CODEBUDDY KEY MANAGER
// ============================================================================

type CBKey struct {
	Key         string
	mu          sync.RWMutex
	disabled    bool
	disabledAt  time.Time
	creditsUsed float64
	totalReqs   int64
	db          *db.Store
}

// NewCBKeyForTest returns a CBKey for whitebox tests.
func NewCBKeyForTest(key string, opts ...CBKeyOption) *CBKey {
	k := &CBKey{Key: key}
	for _, o := range opts {
		o(k)
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

// Stats returns credits used, total requests, and disabled flag.
func (k *CBKey) Stats() (float64, int64, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.creditsUsed, k.totalReqs, k.disabled
}

// IsDisabled returns the disabled flag (mutex-safe).
func (k *CBKey) IsDisabled() bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.disabled
}

// AddCredits accumulates credits and auto-disables when the limit is hit.
func (k *CBKey) AddCredits(c float64) {
	k.mu.Lock()
	k.creditsUsed += c
	k.totalReqs++
	if k.creditsUsed >= CB_CREDIT_LIMIT {
		k.disabled = true
		k.disabledAt = time.Time{} // permanent until reset
		slog.Warn("key disabled (credits used)",
			"module", "cb",
			"key", k.Key[:8]+"..."+k.Key[len(k.Key)-4:],
			"credits_used", k.creditsUsed,
			"credit_limit", CB_CREDIT_LIMIT)
	}
	key := k.Key
	credits := k.creditsUsed
	reqs := k.totalReqs
	disabled := k.disabled
	disabledAt := k.disabledAt
	k.mu.Unlock()
	if k.db != nil {
		saveCBKey(k.db, key, credits, reqs, disabled, disabledAt)
	}
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
func (km *CBKeyManager) LoadFromRedis() error {
	redisState, err := km.db.LoadCBKeys()
	if err != nil {
		return fmt.Errorf("cb keys load: %w", err)
	}

	if len(redisState) > 0 {
		for apiKey, state := range redisState {
			key := &CBKey{Key: apiKey, db: km.db}
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
		key := &CBKey{Key: k, db: km.db}
		km.keys = append(km.keys, key)
		if km.db != nil {
			saveCBKey(km.db, k, 0, 0, false, time.Time{})
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
	key := &CBKey{Key: apiKey, db: km.db}
	km.keys = append(km.keys, key)
	total = len(km.keys)
	km.mu.Unlock()
	if km.db != nil {
		saveCBKey(km.db, key.Key, key.creditsUsed, key.totalReqs, key.disabled, key.disabledAt)
	}
	return true, total
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
			saveCBKey(key.db, key.Key, key.creditsUsed, key.totalReqs, key.disabled, key.disabledAt)
		}
		slog.Info("re-enabled cooldown key", "module", "cb", "key", key.Key[:8]+"..."+key.Key[len(key.Key)-4:])
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

// ============================================================================
// CODEBUDDY PROXY
// ============================================================================

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

	client := upstreamClient
	total := km.Len()

	var lastResp *http.Response
	var lastKey *CBKey
	reqStart := time.Now()

	for attempt := 0; attempt < total; attempt++ {
		key, err := km.Next()
		if err != nil {
			break
		}
		req, _ := http.NewRequest("POST", CB_UPSTREAM_URL, bytes.NewReader(transformed))
		req.Header.Set("Authorization", "Bearer "+key.Key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")

		resp, err := client.Do(req)
		if err != nil {
			continue
		}

		if resp.StatusCode == 401 || resp.StatusCode == 429 {
			resp.Body.Close()
			key.mu.Lock()
			key.disabled = true
			if resp.StatusCode == 401 {
				key.disabledAt = time.Time{}
			} else {
				key.disabledAt = time.Now()
			}
			key.mu.Unlock()
			if key.db != nil {
				saveCBKey(key.db, key.Key, key.creditsUsed, key.totalReqs, key.disabled, key.disabledAt)
			}
			if resp.StatusCode == 401 {
				slog.Warn("key disabled (401 unauthorized, permanent)", "module", "cb", "key", key.Key[:8]+"..."+key.Key[len(key.Key)-4:])
			} else {
				slog.Warn("key disabled (429 rate limited, cooldown 10m)", "module", "cb", "key", key.Key[:8]+"..."+key.Key[len(key.Key)-4:])
			}
			continue
		}

		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			bodyStr := string(bodyBytes)
			if strings.Contains(bodyStr, "14018") || strings.Contains(bodyStr, "Credits exhausted") {
				key.mu.Lock()
				key.disabled = true
				key.disabledAt = time.Time{}
				key.mu.Unlock()
				if key.db != nil {
					saveCBKey(key.db, key.Key, key.creditsUsed, key.totalReqs, key.disabled, key.disabledAt)
				}
				slog.Warn("key disabled (credits exhausted, code 14018)", "module", "cb", "key", key.Key[:8]+"..."+key.Key[len(key.Key)-4:])
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
				c.JSON(400, gin.H{"error": "model not available on CodeBuddy", "detail": truncateLog(bodyStr, 500)})
				c.Set("error_msg", truncateLog(bodyStr, 500))
				errJSON, _ := json.Marshal(gin.H{"error": "model not available on CodeBuddy", "detail": truncateLog(bodyStr, 500)})
				c.Set("response_body", json.RawMessage(errJSON))
				return
			}
			key.mu.Lock()
			key.disabled = true
			key.disabledAt = time.Now()
			key.mu.Unlock()
			if key.db != nil {
				saveCBKey(key.db, key.Key, key.creditsUsed, key.totalReqs, key.disabled, key.disabledAt)
			}
			slog.Warn("key disabled (4xx)", "module", "cb", "key", key.Key[:8]+"..."+key.Key[len(key.Key)-4:], "status", resp.StatusCode, "body", truncateLog(bodyStr, 200))
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
	c.Set("upstream_account", lastKey.Key[:8]+"..."+lastKey.Key[len(lastKey.Key)-4:])

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
		var streamContent strings.Builder
		var streamTokensIn, streamTokensOut int
		for scanner.Scan() {
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
								lastKey.mu.Lock()
								lastKey.disabled = true
								lastKey.disabledAt = time.Time{}
								lastKey.mu.Unlock()
								if lastKey.db != nil {
									saveCBKey(lastKey.db, lastKey.Key, lastKey.creditsUsed, lastKey.totalReqs, lastKey.disabled, lastKey.disabledAt)
								}
								slog.Warn("key disabled (credits exhausted in stream)", "module", "cb", "key", lastKey.Key[:8]+"..."+lastKey.Key[len(lastKey.Key)-4:])
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
			fmt.Fprintf(c.Writer, "%s\n", line)
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

// DeleteKey removes a CodeBuddy key by its key string.
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
			slog.Info("deleted cb key", "module", "cb", "key", key[:8]+"..."+key[len(key)-4:])
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
