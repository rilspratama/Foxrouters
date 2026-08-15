package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

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

// sseBufPool reuses read buffers for stream proxying.
var sseBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 4096)
		return &b
	},
}
