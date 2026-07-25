package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// CredentialProbeResult is the operator-facing outcome of a direct upstream
// credential probe (not gateway RR routing).
type CredentialProbeResult struct {
	OK          bool    `json:"ok"`
	Status      int     `json:"status"`
	LatencyMs   int64   `json:"latency_ms"`
	Model       string  `json:"model"`
	Content     string  `json:"content,omitempty"`
	Credit      float64 `json:"credit,omitempty"`
	Error       string  `json:"error,omitempty"`
	CredType    string  `json:"cred_type,omitempty"`
	Email       string  `json:"email,omitempty"`
	TokenStatus string  `json:"token_status,omitempty"`
}

// GetByKey resolves a full key, masked key, or OAuth email to the live *CBKey.
// Returns nil when not found.
func (km *CBKeyManager) GetByKey(maskedOrFull string) *CBKey {
	full := km.ResolveKey(maskedOrFull)
	if full == "" {
		return nil
	}
	for _, k := range km.GetAll() {
		if k.Key == full {
			return k
		}
	}
	return nil
}

// GetByEmail finds a Grok account by email. Returns nil when not found.
func (am *GrokAccountManager) GetByEmail(email string) *GrokAccount {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil
	}
	for _, a := range am.GetAll() {
		if a.Email == email {
			return a
		}
	}
	return nil
}

// TestCBKey probes a CodeBuddy credential directly against the chat upstream.
// Disabled keys are still tested so operators can verify recovery.
// Network I/O happens outside any account mutex.
func TestCBKey(key *CBKey) CredentialProbeResult {
	const model = "gpt-5.5"
	res := CredentialProbeResult{Model: model}
	if key == nil {
		res.Error = "cb key is nil"
		return res
	}

	snap := key.Snapshot()
	res.CredType = string(snap.CredType)
	if res.CredType == "" {
		res.CredType = string(CBAuthAPIKey)
	}
	if snap.CredType == CBAuthOAuth {
		res.Email = snap.Email
		if res.Email == "" {
			res.Email = snap.Key
		}
	}

	// OAuth: refresh if near-expiry first (no network under mutex — EnsureValid
	// already lock-splits). Failures still fall through so operators see the
	// real chat status after a bad refresh.
	if err := key.EnsureValid(); err != nil {
		slog.Warn("cb credential probe ensure valid failed",
			"module", "cb-probe", "key", key.DisplayID(), "error", err)
		// Keep going — chat may still work or yield a clearer 401.
	}

	body := map[string]any{
		"model": model,
		"stream": true,
		"messages": []map[string]string{
			{"role": "system", "content": CB_DEFAULT_SYSTEM},
			{"role": "user", "content": "Say OK"},
		},
		"max_completion_tokens": 16,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		res.Error = "marshal: " + err.Error()
		return res
	}

	req, err := http.NewRequest("POST", CB_UPSTREAM_URL, bytes.NewReader(payload))
	if err != nil {
		res.Error = "request build: " + err.Error()
		return res
	}
	req.Header.Set("Authorization", key.AuthHeader())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	// Prefer healthCheckClient (30s) so a hung probe can't pin a dashboard
	// request for the full 300s upstream timeout. Fall back to tokenRefreshClient
	// when health client is nil (tests may swap clients).
	base := healthCheckClient
	if base == nil {
		base = tokenRefreshClient
	}
	client, proxyID := getClient(base, "codebuddy")
	// Cap probe timeout at 45s regardless of inherited client timeout.
	if client.Timeout == 0 || client.Timeout > 45*time.Second {
		cloned := *client
		cloned.Timeout = 45 * time.Second
		client = &cloned
	}

	start := time.Now()
	resp, err := client.Do(req)
	res.LatencyMs = time.Since(start).Milliseconds()
	markProxyResult(proxyID, err, func() int {
		if err != nil || resp == nil {
			return 0
		}
		return resp.StatusCode
	}())
	if err != nil {
		res.Error = err.Error()
		slog.Info("cb credential probe network error",
			"module", "cb-probe", "key", key.DisplayID(), "error", err, "latency_ms", res.LatencyMs)
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := strings.TrimSpace(string(errBody))
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		res.Error = truncateLog(msg, 240)
		slog.Info("cb credential probe failed",
			"module", "cb-probe", "key", key.DisplayID(),
			"status", resp.StatusCode, "latency_ms", res.LatencyMs)
		return res
	}

	content, credit := parseCBProbeStream(resp.Body)
	res.Content = truncateLog(content, 200)
	res.Credit = credit
	res.OK = true
	slog.Info("cb credential probe ok",
		"module", "cb-probe", "key", key.DisplayID(),
		"status", resp.StatusCode, "latency_ms", res.LatencyMs,
		"credit", credit, "content_len", len(content))
	return res
}

// parseCBProbeStream reads a CB SSE chat response and returns the first
// content deltas concatenated plus usage.credit when present.
func parseCBProbeStream(r io.Reader) (content string, credit float64) {
	var b strings.Builder
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" || data == "" {
			if data == "[DONE]" {
				break
			}
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
			Error any            `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			// Surface stream-level errors as content so operators see them;
			// credit still returned if present on a later usage frame.
			if errBytes, err := json.Marshal(chunk.Error); err == nil {
				if b.Len() == 0 {
					b.WriteString(string(errBytes))
				}
			}
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			b.WriteString(chunk.Choices[0].Delta.Content)
		}
		if chunk.Usage != nil {
			if c, ok := chunk.Usage["credit"].(float64); ok {
				credit = c
			}
		}
	}
	return b.String(), credit
}

// TestGrokAccount probes a Grok account directly against cli-chat-proxy.
// Disabled accounts are still tested so operators can verify recovery.
func TestGrokAccount(acc *GrokAccount) CredentialProbeResult {
	const model = "grok-4.5"
	res := CredentialProbeResult{Model: model}
	if acc == nil {
		res.Error = "grok account is nil"
		return res
	}

	snap := acc.Snapshot()
	res.Email = snap.Email
	res.TokenStatus = snap.TokenStatus

	if err := acc.EnsureValid(); err != nil {
		slog.Warn("grok credential probe ensure valid failed",
			"module", "grok-probe", "email", acc.Email, "error", err)
		// Fall through with existing AT so operators still get a status code.
	}
	// Refresh snapshot after possible EnsureValid.
	snap = acc.Snapshot()
	res.TokenStatus = snap.TokenStatus

	token := acc.GetAccessToken()
	if token == "" {
		res.Error = "empty access token"
		return res
	}

	body := map[string]any{
		"model":  model,
		"stream": false,
		"messages": []map[string]string{
			{"role": "user", "content": "Say OK"},
		},
		"max_tokens": 16,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		res.Error = "marshal: " + err.Error()
		return res
	}

	req, err := http.NewRequest("POST", XAI_UPSTREAM_URL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		res.Error = "request build: " + err.Error()
		return res
	}
	// Minimal required grok-shell headers (mirrors health check + grokHeaders).
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-grok-client-version", GROK_CLIENT_VERSION)
	req.Header.Set("x-grok-client-identifier", GROK_CLIENT_IDENTIFIER)
	req.Header.Set("User-Agent", fmt.Sprintf("grok-shell/%s (linux; x86_64)", GROK_CLIENT_VERSION))

	base := healthCheckClient
	if base == nil {
		base = tokenRefreshClient
	}
	client, proxyID := getClient(base, "grok")
	if client.Timeout == 0 || client.Timeout > 45*time.Second {
		cloned := *client
		cloned.Timeout = 45 * time.Second
		client = &cloned
	}

	start := time.Now()
	resp, err := client.Do(req)
	res.LatencyMs = time.Since(start).Milliseconds()
	markProxyResult(proxyID, err, func() int {
		if err != nil || resp == nil {
			return 0
		}
		return resp.StatusCode
	}())
	if err != nil {
		res.Error = err.Error()
		slog.Info("grok credential probe network error",
			"module", "grok-probe", "email", acc.Email, "error", err, "latency_ms", res.LatencyMs)
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(respBody))
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		res.Error = truncateLog(msg, 240)
		slog.Info("grok credential probe failed",
			"module", "grok-probe", "email", acc.Email,
			"status", resp.StatusCode, "latency_ms", res.LatencyMs)
		return res
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		res.Error = "parse response: " + err.Error()
		return res
	}
	if parsed.Error != nil {
		if errBytes, err := json.Marshal(parsed.Error); err == nil {
			res.Error = truncateLog(string(errBytes), 240)
		} else {
			res.Error = "upstream error object"
		}
		return res
	}
	if len(parsed.Choices) > 0 {
		res.Content = truncateLog(parsed.Choices[0].Message.Content, 200)
	}
	res.OK = true
	slog.Info("grok credential probe ok",
		"module", "grok-probe", "email", acc.Email,
		"status", resp.StatusCode, "latency_ms", res.LatencyMs,
		"content_len", len(res.Content))
	return res
}
