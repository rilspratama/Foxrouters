package upstream

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type FreebuffDeviceStart struct {
	State           string `json:"state"`
	AuthURL         string `json:"auth_url"`
	FingerprintID   string `json:"fingerprint_id"`
	FingerprintHash string `json:"fingerprint_hash"`
	ExpiresAt       int64  `json:"expires_at"`
}

// FreebuffDevicePoll is the result of one poll for tokens.
type FreebuffDevicePoll struct {
	Status    string `json:"status"` // pending | ready | error
	AuthToken string `json:"auth_token,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	Email     string `json:"email,omitempty"`
	Error     string `json:"error,omitempty"`
}

// fbStartDeviceAuth requests a fresh login URL from Freebuff.
// POST /api/auth/cli/code {fingerprintId} → {loginUrl, fingerprintHash, expiresAt}
func fbStartDeviceAuth() (*FreebuffDeviceStart, error) {
	// Generate random fingerprint ID
	fpID := "gw-" + fbRandomString(12)

	body, _ := json.Marshal(map[string]string{"fingerprintId": fpID})
	req, _ := http.NewRequest("POST", FREEBUFF_API_BASE+"/api/auth/cli/code", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")

	client := &http.Client{Timeout: FREEBUFF_SESSION_TIMEOUT}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fb device start: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fb device start [%d]: %s", resp.StatusCode, truncateLog(string(respBody), 200))
	}

	var data struct {
		FingerprintID   string `json:"fingerprintId"`
		FingerprintHash string `json:"fingerprintHash"`
		LoginURL        string `json:"loginUrl"`
		ExpiresAt       int64  `json:"expiresAt"`
	}
	if err := json.Unmarshal(respBody, &data); err != nil {
		return nil, fmt.Errorf("fb device start parse: %w", err)
	}
	if data.LoginURL == "" {
		return nil, fmt.Errorf("fb device start: missing loginUrl")
	}

	// Extract auth_code from loginUrl to use as state
	state := data.FingerprintID
	if parsed, err := parseAuthCode(data.LoginURL); err == nil && parsed != "" {
		state = parsed
	}

	slog.Info("fb device start ok", "module", "freebuff", "fingerprint", fpID[:16])
	return &FreebuffDeviceStart{
		State:           state,
		AuthURL:         data.LoginURL,
		FingerprintID:   data.FingerprintID,
		FingerprintHash: data.FingerprintHash,
		ExpiresAt:       data.ExpiresAt,
	}, nil
}

// fbPollDeviceAuth checks whether the user has completed browser login.
// GET /api/auth/cli/status?fingerprintId=...&fingerprintHash=...&expiresAt=...
func fbPollDeviceAuth(fingerprintID, fingerprintHash string, expiresAt int64) (*FreebuffDevicePoll, error) {
	params := url.Values{}
	params.Set("fingerprintId", fingerprintID)
	params.Set("fingerprintHash", fingerprintHash)
	params.Set("expiresAt", fmt.Sprintf("%d", expiresAt))

	req, _ := http.NewRequest("GET", FREEBUFF_API_BASE+"/api/auth/cli/status?"+params.Encode(), nil)
	req.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")

	client := &http.Client{Timeout: FREEBUFF_SESSION_TIMEOUT}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fb device poll: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	// Non-200 = pending
	if resp.StatusCode != 200 {
		return &FreebuffDevicePoll{Status: "pending"}, nil
	}

	var data struct {
		Message string `json:"message"`
		User    struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Email     string `json:"email"`
			AuthToken string `json:"authToken"`
		} `json:"user"`
		// Fallbacks for flat response shape
		AuthToken string `json:"authToken"`
		Token     string `json:"token"`
		UserID    string `json:"userId"`
		UID       string `json:"uid"`
		Email     string `json:"email"`
	}
	if err := json.Unmarshal(respBody, &data); err != nil {
		return &FreebuffDevicePoll{Status: "pending"}, nil
	}

	// Check for success — token can be in user.authToken (nested) or root authToken
	token := data.User.AuthToken
	if token == "" {
		token = data.AuthToken
	}
	if token == "" {
		token = data.Token
	}

	if data.Message == "Authentication successful!" && token != "" {
		// Use user fields from response, fall back to /api/v1/me probe
		userID := data.User.ID
		email := data.User.Email
		if userID == "" || email == "" {
			uid2, email2, _ := fbProbeMe(token)
			if userID == "" {
				userID = uid2
			}
			if email == "" {
				email = email2
			}
		}

		slog.Info("fb device poll ready", "module", "freebuff", "fingerprint", fingerprintID[:16], "user_id", userID)
		return &FreebuffDevicePoll{
			Status:    "ready",
			AuthToken: token,
			UserID:    userID,
			Email:     email,
		}, nil
	}

	// Still pending
	return &FreebuffDevicePoll{Status: "pending"}, nil
}

// StartFreebuffDeviceAuth is the exported wrapper for handlers.
func StartFreebuffDeviceAuth() (*FreebuffDeviceStart, error) {
	return fbStartDeviceAuth()
}

// PollFreebuffDeviceAuth is the exported wrapper for handlers.
func PollFreebuffDeviceAuth(fingerprintID, fingerprintHash string, expiresAt int64) (*FreebuffDevicePoll, error) {
	return fbPollDeviceAuth(fingerprintID, fingerprintHash, expiresAt)
}

// parseAuthCode extracts the auth_code query param from a login URL.
func parseAuthCode(loginURL string) (string, error) {
	parsed, err := url.Parse(loginURL)
	if err != nil {
		return "", err
	}
	return parsed.Query().Get("auth_code"), nil
}

func fbProbeMe(token string) (userID, email string, err error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", FREEBUFF_API_BASE+FREEBUFF_ME_PATH, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("probe /me: %d", resp.StatusCode)
	}
	var data struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", "", err
	}
	return data.ID, data.Email, nil
}

// TestFreebuffAccount probes a Freebuff token directly against the chat
// upstream (deepseek/deepseek-v4-flash — the always-available limited-tier
// model). Session + run are created through the manager's cache so the probe
// also warms the gateway's session/run chain. Disabled accounts are still
// probed so operators can verify recovery. Network I/O happens outside any
// account mutex.
func (am *FreebuffAccountManager) TestFreebuffAccount(acc *FreebuffAccount) CredentialProbeResult {
	res := CredentialProbeResult{Model: "fb/deepseek-v4-flash"}
	if acc == nil {
		res.Error = "fb account is nil"
		return res
	}
	acc.mu.Lock()
	token := acc.Token
	userID := acc.UserID
	email := acc.Email
	acc.mu.Unlock()
	res.Email = email
	if token == "" {
		res.Error = "fb account has no token"
		return res
	}

	const (
		upstreamModel = "deepseek/deepseek-v4-flash"
		agentID       = "base2-free-deepseek-flash"
	)
	client := &http.Client{Timeout: 90 * time.Second}
	start := time.Now()

	// Session (cached, 1hr TTL) + run chain (cached, 10min)
	sess, err := am.fbGetOrCreateSession(client, token, userID, upstreamModel)
	if err != nil {
		res.Error = "session: " + err.Error()
		return res
	}
	runID, err := am.fbGetOrCreateRun(client, token, agentID)
	if err != nil {
		res.Error = "run: " + err.Error()
		return res
	}

	body := map[string]any{
		"model": upstreamModel,
		"messages": []map[string]string{
			{"role": "system", "content": FREEBUFF_BUFFY_PREFIX},
			{"role": "user", "content": "Say OK"},
		},
		"max_tokens": 512,
		"codebuff_metadata": map[string]any{
			"freebuff_instance_id": sess.InstanceID,
			"run_id":               runID,
			"cost_mode":            "free",
		},
	}
	payload := fbTransform(body, upstreamModel, sess.InstanceID, runID)

	req, err := http.NewRequest("POST", FREEBUFF_API_BASE+FREEBUFF_CHAT_PATH, bytes.NewReader(payload))
	if err != nil {
		res.Error = "request build: " + err.Error()
		return res
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")
	req.Header.Set("x-freebuff-instance-id", sess.InstanceID)
	if userID != "" {
		req.Header.Set("x-freebuff-acting-user-id", userID)
	}

	resp, err := client.Do(req)
	res.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = "network: " + err.Error()
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		res.Error = fmt.Sprintf("upstream %d: %s", resp.StatusCode, truncateLog(string(bodyBytes), 160))
		return res
	}
	content, err := fbParseSSEContent(resp.Body)
	if err != nil {
		res.Error = "stream parse: " + err.Error()
		return res
	}
	if strings.TrimSpace(content) == "" {
		res.Error = "empty response"
		return res
	}
	res.Content = truncateLog(content, 160)
	res.OK = true
	return res
}

// fbParseSSEContent joins delta.content pieces from an SSE chat stream.
func fbParseSSEContent(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var content strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			content.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if err := sc.Err(); err != nil {
		return content.String(), err
	}
	return content.String(), nil
}

// BODY TRANSFORMATION

// fbTransform modifies the request body for Freebuff upstream.
func fbTransform(bodyMap map[string]any, upstreamModel, instanceID, runID string) []byte {
	// 1. Prepend Buffy system prompt if needed
	fbEnsureBuffyPrefix(bodyMap)

	// 2. Inject end_turn tool if tools present
	fbInjectEndTurnTool(bodyMap)

	// 3. Set upstream model (strip fb/ prefix already done by caller)
	bodyMap["model"] = upstreamModel

	// 4. Always stream upstream
	bodyMap["stream"] = true

	// 5. Provider data_collection deny
	bodyMap["provider"] = map[string]string{"data_collection": "deny"}

	// 6. Stop sequence
	bodyMap["stop"] = []string{`"cb_easp"`}

	// 7. Default max_tokens to 384K (DeepSeek V4 Flash max output) if client didn't set it.
	// Reasoning models burn tokens for reasoning first — small max_tokens causes
	// finish_reason=length with empty content.
	// Then clamp: prompt_tokens + max_tokens must not exceed 1M (model context limit).
	const fbMaxContext = 1048576 // 1M tokens (DeepSeek V4 Flash)
	if _, has := bodyMap["max_tokens"]; !has {
		if _, has2 := bodyMap["max_completion_tokens"]; !has2 {
			bodyMap["max_tokens"] = 393216 // 384K default
		}
	}
	// Clamp max_tokens so prompt + completion ≤ 1M
	// max_tokens can be float64 (from client JSON) or int (set by us above)
	var mtVal int
	switch v := bodyMap["max_tokens"].(type) {
	case float64:
		mtVal = int(v)
	case int:
		mtVal = v
	case json.Number:
		n, _ := v.Int64()
		mtVal = int(n)
	default:
		mtVal = 0
	}
	if mtVal > 0 {
		// Rough prompt token estimate: count message content chars / 4
		estimatedPromptTokens := 0
		if msgs, ok := bodyMap["messages"].([]any); ok {
			for _, m := range msgs {
				if mm, ok := m.(map[string]any); ok {
					if c, ok := mm["content"].(string); ok {
						estimatedPromptTokens += len(c) / 4
					}
				}
			}
		}
		maxAllowed := fbMaxContext - estimatedPromptTokens
		if maxAllowed < 1000 {
			maxAllowed = 1000
		}
		if mtVal > maxAllowed {
			bodyMap["max_tokens"] = maxAllowed
		}
	}

	// 7. codebuff_metadata
	clientID := "wf-" + fbRandomString(8)
	traceID := uuid.New().String()
	bodyMap["codebuff_metadata"] = map[string]string{
		"run_id":               runID,
		"client_id":            clientID,
		"cost_mode":            "free",
		"freebuff_instance_id": instanceID,
		"trace_session_id":     traceID,
	}

	// Remove fields that Freebuff upstream doesn't understand
	delete(bodyMap, "reasoning")
	delete(bodyMap, "extra_body")

	out, _ := json.Marshal(bodyMap)
	return out
}

// fbEnsureBuffyPrefix ensures the first system message starts with the Buffy prefix.
func fbEnsureBuffyPrefix(bodyMap map[string]any) {
	msgs, ok := bodyMap["messages"].([]any)
	if !ok || len(msgs) == 0 {
		// No messages — prepend a system message
		bodyMap["messages"] = []any{
			map[string]any{"role": "system", "content": FREEBUFF_BUFFY_PREFIX},
		}
		return
	}

	// Check first message
	first, ok := msgs[0].(map[string]any)
	if !ok {
		return
	}

	role, _ := first["role"].(string)
	if role != "system" {
		// Prepend a new system message
		newMsgs := append([]any{
			map[string]any{"role": "system", "content": FREEBUFF_BUFFY_PREFIX},
		}, msgs...)
		bodyMap["messages"] = newMsgs
		return
	}

	// System message exists — check content
	content, ok := first["content"].(string)
	if !ok {
		// Content is array (multimodal) — prepend a separate system message
		newMsgs := append([]any{
			map[string]any{"role": "system", "content": FREEBUFF_BUFFY_PREFIX},
		}, msgs...)
		bodyMap["messages"] = newMsgs
		return
	}

	if !strings.HasPrefix(content, FREEBUFF_BUFFY_PREFIX) {
		// Prepend Buffy prefix to existing content
		first["content"] = FREEBUFF_BUFFY_PREFIX + "\n\n" + content
		msgs[0] = first
		bodyMap["messages"] = msgs
	}
}

// fbInjectEndTurnTool injects a dummy end_turn tool to bypass foreign client detection.
func fbInjectEndTurnTool(bodyMap map[string]any) {
	tools, ok := bodyMap["tools"].([]any)
	if !ok || len(tools) == 0 {
		return
	}

	// Check if end_turn already present
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tm["function"].(map[string]any)
		if !ok {
			continue
		}
		if name, _ := fn["name"].(string); name == "end_turn" {
			return // already present
		}
	}

	// Inject end_turn
	endTurn := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "end_turn",
			"description": "Signal the end of the current task.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
	bodyMap["tools"] = append(tools, endTurn)
}

func fbRandomString(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// PROXY FREEBUFF

// fbIsDisabled checks if FREEBUFF_DISABLED env is set.
var fbDisabled = os.Getenv("FREEBUFF_DISABLED") == "1"

// ProxyFreebuff handles chat completions for the Freebuff upstream.
func ProxyFreebuff(c *gin.Context, body []byte, bodyMap map[string]any, am *FreebuffAccountManager, clientStream bool, hc *HealthChecker) {
	if fbDisabled || am == nil || am.Len() == 0 {
		c.JSON(503, gin.H{"error": "freebuff upstream not available"})
		c.Set("error_msg", "freebuff not available")
		errJSON, _ := json.Marshal(gin.H{"error": "freebuff upstream not available"})
		c.Set("response_body", json.RawMessage(errJSON))
		return
	}

	// Circuit breaker
	if hc != nil && hc.FB != nil && !hc.FB.CanRequest() {
		hc.FB.RecordRequest(0, fmt.Errorf("circuit open"))
		c.JSON(503, gin.H{"error": "freebuff upstream circuit breaker open"})
		c.Set("error_msg", "fb circuit breaker open")
		errJSON, _ := json.Marshal(gin.H{"error": "freebuff circuit breaker open"})
		c.Set("response_body", json.RawMessage(errJSON))
		return
	}

	// Resolve model config
	gatewayModel, _ := bodyMap["model"].(string)
	mc := fbModelConfig(gatewayModel)
	upstreamModel := mc.Upstream
	agentID := mc.Agent

	total := am.Len()
	reqStart := time.Now()

	// Get HTTP client (through proxy pool if configured)
	client, proxyID := getClient(upstreamClient, "freebuff")

	var lastErr string

	for attempt := 0; attempt < total; attempt++ {
		if err := c.Request.Context().Err(); err != nil {
			slog.Debug("client cancelled", "module", "freebuff", "attempt", attempt+1)
			return
		}

		acc, err := am.Next(upstreamModel)
		if err != nil {
			lastErr = fmt.Sprintf("next: %v", err)
			slog.Warn("fb no eligible account", "module", "freebuff", "model", upstreamModel, "error", err)
			break
		}

		// Per-account serialization — Freebuff upstream breaks if concurrent >1
		acc.mu.Lock()

		// 1. Get/create session
		sess, err := am.fbGetOrCreateSession(client, acc.Token, acc.UserID, upstreamModel)
		if err != nil {
			if errors.Is(err, ErrFBBanned) {
				bannedToken := acc.Token
				lastErr = fmt.Sprintf("banned: %v", err)
				acc.mu.Unlock()
				am.MarkBanned(bannedToken)
				slog.Warn("fb account banned", "module", "freebuff", "token", bannedToken[:8]+"...")
				continue
			}
			acc.mu.Unlock()
			lastErr = fmt.Sprintf("session: %v", err)
			slog.Warn("fb session failed", "module", "freebuff", "attempt", attempt+1, "error", err)
			continue
		}

		// 2. Get/create run
		runID, err := am.fbGetOrCreateRun(client, acc.Token, agentID)
		if err != nil {
			acc.mu.Unlock()
			lastErr = fmt.Sprintf("run: %v", err)
			slog.Warn("fb run failed", "module", "freebuff", "attempt", attempt+1, "error", err)
			continue
		}

		// 3. Transform body
		// Deep copy bodyMap to avoid mutation across attempts
		bodyCopy := make(map[string]any)
		bodyBytes, _ := json.Marshal(bodyMap)
		json.Unmarshal(bodyBytes, &bodyCopy)

		transformedBody := fbTransform(bodyCopy, upstreamModel, sess.InstanceID, runID)

		// 4. Build request
		chatReq, _ := http.NewRequestWithContext(
			c.Request.Context(),
			"POST",
			FREEBUFF_API_BASE+FREEBUFF_CHAT_PATH,
			bytes.NewReader(transformedBody),
		)
		chatReq.Header.Set("Authorization", "Bearer "+acc.Token)
		chatReq.Header.Set("Content-Type", "application/json")
		chatReq.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")
		chatReq.Header.Set("x-freebuff-instance-id", sess.InstanceID)
		if acc.UserID != "" {
			chatReq.Header.Set("x-freebuff-acting-user-id", acc.UserID)
		}

		// 5. Send request
		timeout := FREEBUFF_UPSTREAM_TIMEOUT
		if !clientStream {
			timeout = FREEBUFF_NONSTREAM_TIMEOUT
		}
		chatClient := &http.Client{Timeout: timeout}
		if client != nil {
			// Clone the (possibly proxy) transport WITHOUT mutating the shared
			// default client. Setting .Timeout on the shared *http.Client would
			// permanently cap ALL CodeBuddy/Grok upstream streams at 25s.
			chatClient.Transport = client.Transport
		}

		resp, err := chatClient.Do(chatReq)
		if err != nil {
			acc.mu.Unlock()
			markProxyResult(proxyID, err, 0)
			lastErr = fmt.Sprintf("network: %v", err)
			slog.Warn("fb network failed", "module", "freebuff", "attempt", attempt+1, "error", err)
			continue
		}
		markProxyResult(proxyID, nil, resp.StatusCode)

		// 6. Handle 428 (stale session) — recreate + retry once
		if resp.StatusCode == 428 {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if strings.Contains(string(respBody), "waiting_room_required") {
				slog.Debug("fb 428 stale session, recreating", "module", "freebuff", "attempt", attempt+1)
				am.fbDeleteSession(client, acc.Token)
				sess, err = am.fbGetOrCreateSession(client, acc.Token, acc.UserID, upstreamModel)
				if err != nil {
					acc.mu.Unlock()
					lastErr = fmt.Sprintf("session recreate: %v", err)
					continue
				}
				// Retry with new session
				bodyCopy2 := make(map[string]any)
				json.Unmarshal(bodyBytes, &bodyCopy2)
				transformedBody = fbTransform(bodyCopy2, upstreamModel, sess.InstanceID, runID)
				chatReq, _ = http.NewRequestWithContext(c.Request.Context(), "POST", FREEBUFF_API_BASE+FREEBUFF_CHAT_PATH, bytes.NewReader(transformedBody))
				chatReq.Header.Set("Authorization", "Bearer "+acc.Token)
				chatReq.Header.Set("Content-Type", "application/json")
				chatReq.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")
				chatReq.Header.Set("x-freebuff-instance-id", sess.InstanceID)
				if acc.UserID != "" {
					chatReq.Header.Set("x-freebuff-acting-user-id", acc.UserID)
				}
				resp, err = chatClient.Do(chatReq)
				if err != nil {
					acc.mu.Unlock()
					lastErr = fmt.Sprintf("network retry: %v", err)
					continue
				}
			} else {
				acc.mu.Unlock()
				lastErr = fmt.Sprintf("428: %s", truncateLog(string(respBody), 200))
				continue
			}
		}

		// 7. Handle 429 (rate limited) — cooldown account
		if resp.StatusCode == 429 {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			cooldown := fbParseCooldown(string(respBody))
			acc.CooldownUntil = time.Now().Add(cooldown)
			am.SaveAccount(acc)
			acc.mu.Unlock()
			lastErr = fmt.Sprintf("429: %s", truncateLog(string(respBody), 200))
			slog.Warn("fb rate limited, cooldown", "module", "freebuff", "cooldown", cooldown, "body", truncateLog(string(respBody), 200))
			continue
		}

		// 8. Handle 403 (transient empty response) — retry next account, no cooldown
		// Freebuff occasionally returns 403 with valid JSON but empty content + 0 tokens.
		// The same account succeeds on the next request, so this is transient — retry.
		if resp.StatusCode == 403 {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			acc.mu.Unlock()
			lastErr = fmt.Sprintf("403: %s", truncateLog(string(respBody), 200))
			slog.Warn("fb transient 403, retrying", "module", "freebuff", "account", acc.Email, "body", truncateLog(string(respBody), 200))
			continue
		}

		// 9. Handle 5xx — try next account
		if resp.StatusCode >= 500 {
			resp.Body.Close()
			acc.mu.Unlock()
			lastErr = fmt.Sprintf("upstream %d", resp.StatusCode)
			slog.Warn("fb upstream error", "module", "freebuff", "status", resp.StatusCode)
			continue
		}

		// 10. Success — stream passthrough or aggregate
		if hc != nil && hc.FB != nil {
			hc.FB.RecordRequest(time.Since(reqStart), nil)
		}
		c.Set("upstream_account", acc.Email)
		acc.mu.Unlock()

		defer resp.Body.Close()

		if clientStream {
			// Stream passthrough with usage capture
			copyUpstreamHeaders(c.Writer.Header(), resp.Header)
			c.Writer.WriteHeader(resp.StatusCode)
			flusher, _ := c.Writer.(http.Flusher)
			reader := bufio.NewReader(resp.Body)

			var streamUsage map[string]any
			var streamBuf strings.Builder
			for {
				line, err := reader.ReadString('\n')
				if line != "" {
					c.Writer.Write([]byte(line))
					if flusher != nil {
						flusher.Flush()
					}
					// Capture for response_body + usage extraction
					streamBuf.WriteString(line)
					trimmed := strings.TrimSpace(line)
					if strings.HasPrefix(trimmed, "data: ") {
						data := strings.TrimPrefix(trimmed, "data: ")
						if data != "[DONE]" && data != "" {
							var chunk map[string]any
							if json.Unmarshal([]byte(data), &chunk) == nil {
								// Unwrap envelope
								if inner, ok := chunk["data"].(map[string]any); ok {
									if _, hasChoices := inner["choices"]; hasChoices {
										chunk = inner
									}
								}
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

			// Set tokens + response_body for logging.
			// Store an AGGREGATED JSON object (same shape as the non-stream
			// path) instead of the raw SSE text — raw `data: {...}` lines are
			// not valid JSON and break history-detail marshaling (RawMessage
			// MarshalJSON error → empty response in the dashboard).
			if streamUsage != nil {
				if pt, ok := streamUsage["prompt_tokens"].(float64); ok {
					c.Set("tokens_in", int(pt))
				}
				if ct, ok := streamUsage["completion_tokens"].(float64); ok {
					c.Set("tokens_out", int(ct))
				}
			}
			if agg := fbStreamToNonStream(strings.NewReader(streamBuf.String()), upstreamModel); agg != nil {
				if aggBytes, err := json.Marshal(agg); err == nil {
					c.Set("response_body", json.RawMessage(aggBytes))
				}
				// output_text for dashboard preview (stream clients don't get
				// it through the SSE passthrough).
				setFBOutputText(c, agg)
			}

			// Increment hourly session counter
			am.IncrementSessionCount(acc)
		} else {
			// Aggregate SSE → non-stream
			agg := fbStreamToNonStream(resp.Body, upstreamModel)
			// Set tokens + response_body for logging
			if usage, ok := agg["usage"].(map[string]any); ok {
				if pt, ok := usage["prompt_tokens"].(float64); ok {
					c.Set("tokens_in", int(pt))
				}
				if ct, ok := usage["completion_tokens"].(float64); ok {
					c.Set("tokens_out", int(ct))
				}
			}
			if aggBytes, err := json.Marshal(agg); err == nil {
				c.Set("response_body", json.RawMessage(aggBytes))
			}
			// Set output_text for dashboard preview
			setFBOutputText(c, agg)
			c.JSON(200, agg)

			// Increment hourly session counter
			am.IncrementSessionCount(acc)
		}
		return
	}

	// All accounts failed
	if hc != nil && hc.FB != nil {
		hc.FB.RecordRequest(time.Since(reqStart), fmt.Errorf("all accounts failed"))
	}
	c.JSON(503, gin.H{"error": "all freebuff accounts failed: " + lastErr})
	c.Set("error_msg", "fb all accounts failed: "+lastErr)
	errJSON, _ := json.Marshal(gin.H{"error": "all freebuff accounts failed"})
	c.Set("response_body", json.RawMessage(errJSON))
}

// setFBOutputText extracts choices[0].message.content from the aggregated
// response and stores it as c output_text for the dashboard preview.
// Handles BOTH choices shapes ([]map[string]any — built by fbStreamToNonStream —
// and []any — JSON round-trip). Previously only []any was asserted, which
// silently dropped output_text for every Freebuff request (preview empty).
func setFBOutputText(c *gin.Context, agg map[string]any) {
	if content, ok := fbExtractContent(agg); ok {
		c.Set("output_text", content)
	}
}

func fbExtractContent(agg map[string]any) (string, bool) {
	var first map[string]any
	switch ch := agg["choices"].(type) {
	case []map[string]any:
		if len(ch) > 0 {
			first = ch[0]
		}
	case []any:
		if len(ch) > 0 {
			if m, ok := ch[0].(map[string]any); ok {
				first = m
			}
		}
	}
	if first == nil {
		return "", false
	}
	if msg, ok := first["message"].(map[string]any); ok {
		if content, ok := msg["content"].(string); ok {
			return content, true
		}
	}
	return "", false
}

func fbStreamToNonStream(body io.Reader, model string) map[string]any {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var content, reasoning, finishReason, respID, respModel string
	var usage map[string]any

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" || data == "" {
			continue
		}

		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}

		// Unwrap {data: {...}} envelope
		if inner, ok := chunk["data"].(map[string]any); ok {
			if _, hasChoices := inner["choices"]; hasChoices {
				chunk = inner
			}
		}

		if id, ok := chunk["id"].(string); ok && id != "" {
			respID = id
		}
		if m, ok := chunk["model"].(string); ok && m != "" {
			respModel = m
		}
		if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
			usage = u
		}

		choices, ok := chunk["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		choice, ok := choices[0].(map[string]any)
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		if c, ok := delta["content"].(string); ok {
			content += c
		}
		if r, ok := delta["reasoning_content"].(string); ok {
			reasoning += r
		}
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			finishReason = fr
		}
	}

	if finishReason == "" {
		finishReason = "stop"
	}
	if respID == "" {
		respID = "fb_" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	if respModel == "" {
		respModel = model
	}
	if usage == nil {
		usage = map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}

	msg := map[string]any{"role": "assistant"}
	if content != "" {
		msg["content"] = content
	} else if reasoning != "" {
		msg["content"] = reasoning
		msg["reasoning_used_as_content"] = true
	} else {
		msg["content"] = ""
	}
	if reasoning != "" && content != "" {
		msg["reasoning_content"] = reasoning
	}

	return map[string]any{
		"id":      respID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   respModel,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       msg,
				"finish_reason": finishReason,
				"logprobs":      nil,
			},
		},
		"usage": usage,
	}
}

// fbParseCooldown extracts cooldown duration from 429 response body.
func fbParseCooldown(body string) time.Duration {
	// Try JSON retryAfterMs
	var data struct {
		Error struct {
			RetryAfterMs int64 `json:"retryAfterMs"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &data) == nil && data.Error.RetryAfterMs > 0 {
		return time.Duration(data.Error.RetryAfterMs) * time.Millisecond
	}

	// Try "try again in Xh Ym Zs" text
	lower := strings.ToLower(body)
	if strings.Contains(lower, "try again in") {
		// Parse hours, minutes, seconds
		h, m, s := 0, 0, 0
		if idx := strings.Index(lower, "try again in"); idx >= 0 {
			rest := lower[idx+len("try again in"):]
			fmt.Sscanf(rest, "%dh %dm %ds", &h, &m, &s)
			if h == 0 && m == 0 && s == 0 {
				fmt.Sscanf(rest, "%dh", &h)
				if h == 0 {
					fmt.Sscanf(rest, "%dm", &m)
				}
			}
			if h > 0 || m > 0 || s > 0 {
				return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second
			}
		}
	}

	// Default 60s
	return 60 * time.Second
}

// fbMaskToken masks a token for display.
func fbMaskToken(token string) string {
	if len(token) <= 12 {
		return token
	}
	return token[:8] + "..." + token[len(token)-4:]
}
