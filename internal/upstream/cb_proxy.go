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

	"github.com/gin-gonic/gin"
)

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

	client, proxyID := getClient(upstreamClient, "codebuddy")
	total := km.Len()

	// Sticky session: client may pin all requests in a conversation to the
	// same upstream key via header (prompt-cache locality). First header wins:
	// x-session-id, x-conversation-id, x-chat-id.
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
	// message) for content-hash / hybrid selection modes. Computed from the
	// already-transformed body so the prefix matches what upstream sees.
	sysHash := ""
	{
		var tm map[string]any
		if json.Unmarshal(transformed, &tm) == nil {
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
	mode := GetSelectorMode()

	var lastResp *http.Response
	var lastKey *CBKey
	reqStart := time.Now()

	// Mode-selected key gets the first attempts before falling back to RR.
	// RR mode: no preference — every attempt is plain round-robin.
	stickyAttempts := 0
	if mode != SelectorRR {
		stickyAttempts = 2
	}

	// Safety cap: don't walk the whole pool on key-level failures (401/403/429).
	// After 6 consecutive key failures report the last error instead — prevents
	// a full-pool disable storm and bounds per-request upstream calls.
	const maxKeyAttempts = 6
	var lastFailure string
	for attempt := 0; attempt < total+stickyAttempts; attempt++ {
		if attempt >= maxKeyAttempts && lastFailure != "" {
			c.JSON(503, gin.H{"error": "cb keys failing", "detail": lastFailure})
			c.Set("error_msg", lastFailure)
			errJSON, _ := json.Marshal(gin.H{"error": "cb keys failing", "detail": lastFailure})
			c.Set("response_body", json.RawMessage(errJSON))
			return
		}
		// C10: bail out early if the client cancelled — don't walk the
		// whole key list burning upstream calls for a dead request.
		if err := c.Request.Context().Err(); err != nil {
			slog.Debug("client cancelled before attempt", "module", "cb", "attempt", attempt+1, "error", err)
			return
		}
		var key *CBKey
		var err error
		if attempt < stickyAttempts {
			key, err = km.NextForMode(mode, sessionID, sysHash)
		} else {
			key, err = km.Next()
		}
		if err != nil {
			break
		}

		// OAuth: refresh if near-expiry before building the request.
		if err := key.EnsureValid(); err != nil {
			slog.Warn("ensure valid failed", "module", "cb", "key", key.DisplayID(), "error", err)
			// Fall through — try with existing token; 401 path may still refresh.
		}

		req, _ := http.NewRequestWithContext(c.Request.Context(), "POST", CB_UPSTREAM_URL, bytes.NewReader(transformed))
		cbChatHeaders(req, key)

		resp, err := client.Do(req)
		if err != nil {
			lastFailure = "network error: " + err.Error()
			markProxyResult(proxyID, err, 0)
			continue
		}
		markProxyResult(proxyID, nil, resp.StatusCode)

		if resp.StatusCode == 401 {
			resp.Body.Close()
			// OAuth: try one refresh + retry; API key: permanent disable.
			if key.GetCredType() == CBAuthOAuth {
				refreshErr := key.Refresh()
				if refreshErr != nil {
					lastFailure = "401 oauth refresh failed"
					permanentDisable(key, "401 oauth refresh failed: "+refreshErr.Error())
					km.UnbindSticky(sessionID, key)
					continue
				}
				// Rebuild request with fresh AT
				req, _ = http.NewRequestWithContext(c.Request.Context(), "POST", CB_UPSTREAM_URL, bytes.NewReader(transformed))
				cbChatHeaders(req, key)
				resp, err = client.Do(req)
				if err != nil {
					lastFailure = "network error after refresh: " + err.Error()
					markProxyResult(proxyID, err, 0)
					continue
				}
				markProxyResult(proxyID, nil, resp.StatusCode)
				if resp.StatusCode == 401 {
					resp.Body.Close()
					lastFailure = "401 after oauth refresh"
					permanentDisable(key, "401 after oauth refresh, permanent")
					km.UnbindSticky(sessionID, key)
					continue
				}
				// Fall through to process non-401 response below.
			} else {
				lastFailure = "401 unauthorized"
				permanentDisable(key, "401 unauthorized, permanent")
				km.UnbindSticky(sessionID, key)
				continue
			}
		}

		if resp.StatusCode == 429 {
			// Read body for 429 to distinguish trial-not-activated (14017) and
			// credits-exhausted (14018) — both permanent — from rate limiting.
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			bodyStr := string(bodyBytes)
			resp.Body.Close()
			lastFailure = "429: " + truncateLog(bodyStr, 120)
			switch {
			case strings.Contains(bodyStr, "14017"):
				lastFailure = "429 trial not activated (14017), permanent"
				permanentDisable(key, "429 trial not activated, permanent")
				km.UnbindSticky(sessionID, key)
			case strings.Contains(bodyStr, "14018") || strings.Contains(bodyStr, "Credits exhausted"):
				lastFailure = "credits exhausted (14018)"
				// I1: exhaustion is meter-driven — tag with the meter reason so
				// the key auto-lifts when a later sync reports credits.
				permanentDisable(key, cbMeterDisableReason)
				km.UnbindSticky(sessionID, key) // session rebinds to fresh key next request
			default:
				cooldownDisable(key, "429 rate limited, cooldown 10m")
			}
			continue
		}

		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			bodyStr := string(bodyBytes)
			// 403 with code 11140 = "request illegal" (banned/flagged key) → permanent disable
			if resp.StatusCode == 403 && strings.Contains(bodyStr, "11140") {
				lastFailure = "403 request illegal (banned)"
				permanentDisable(key, "403 request illegal, banned, permanent")
				km.UnbindSticky(sessionID, key)
				continue
			}
			if strings.Contains(bodyStr, "14018") || strings.Contains(bodyStr, "Credits exhausted") {
				lastFailure = "credits exhausted"
				permanentDisable(key, "credits exhausted, code 14018")
				km.UnbindSticky(sessionID, key) // session rebinds to fresh key next request
				continue
			}
			// ALL 400s are client request errors (bad body, bad params, unknown
			// model, any 111xx code — past, present, future). NEVER a key
			// problem. Return directly: no disable, no retry on the next key.
			// Rotating on a request-side error cascades a single malformed
			// request into disabling the entire pool (the 11101 cascade).
			if resp.StatusCode == 400 {
				hc.CB.RecordRequest(time.Since(reqStart), fmt.Errorf("cb 400 client error"))
				c.JSON(400, gin.H{"error": "CodeBuddy rejected request", "detail": truncateLog(bodyStr, 500)})
				c.Set("error_msg", truncateLog(bodyStr, 500))
				errJSON, _ := json.Marshal(gin.H{"error": "CodeBuddy rejected request", "detail": truncateLog(bodyStr, 500)})
				c.Set("response_body", json.RawMessage(errJSON))
				return
			}
			// Any other 4xx (404, 422, 408, unknown codes…) is also a
			// client/endpoint error, not a key problem. Same rule: return it,
			// never rotate-and-disable the pool. Only 401/403/429 above touch
			// key state (auth/banned/rate — all genuinely key-specific).
			c.JSON(resp.StatusCode, gin.H{"error": "CodeBuddy request failed", "detail": truncateLog(bodyStr, 500)})
			c.Set("error_msg", truncateLog(bodyStr, 500))
			errJSON, _ := json.Marshal(gin.H{"error": "CodeBuddy request failed", "detail": truncateLog(bodyStr, 500)})
			c.Set("response_body", json.RawMessage(errJSON))
			return
		}

		if resp.StatusCode >= 500 {
			resp.Body.Close()
			lastFailure = fmt.Sprintf("upstream %d", resp.StatusCode)
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
	// upstream_account: email for OAuth, masked key for API key
	c.Set("upstream_account", lastKey.DisplayID())

	if clientStream {
		defer lastResp.Body.Close()
		copyUpstreamHeaders(c.Writer.Header(), lastResp.Header)
		c.Writer.WriteHeader(lastResp.StatusCode)
		flusher, _ := c.Writer.(http.Flusher)
		scanner := bufio.NewScanner(lastResp.Body)
		scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
		// C6: same as grok — a client disconnect must stop the stream loop
		// promptly so we don't keep pulling from upstream forever.
		ctx := c.Request.Context()
		var streamContent strings.Builder
		var streamReasoning strings.Builder
		var streamTokensIn, streamTokensOut int
		var streamUsage map[string]any // last chunk's full usage (has cache fields)
		var streamFinish string        // real finish_reason from upstream (may be "tool_calls")
		// tool_calls accumulation keyed by index (mirrors cbCollectStream).
		var streamToolCalls = map[int]*struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}{}
		var streamToolOrder []int
		var streamChunks int
		for scanner.Scan() {
			streamChunks++
			if err := ctx.Err(); err != nil {
				slog.Debug("sse loop: client cancelled", "module", "cb", "error", err)
				lastResp.Body.Close()
				break
			}
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
								permanentDisable(lastKey, "credits exhausted in stream")
							} else if strings.Contains(errStr, "14017") {
								permanentDisable(lastKey, "trial not activated in stream, permanent")
							}
						}
						if sc.Usage != nil {
							// Keep the LAST non-nil usage (final chunk has real
							// token counts + cache hit fields). Intermediate
							// chunks may have all-zero usage.
							streamUsage = sc.Usage
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
							streamReasoning.WriteString(sc.Choices[0].Delta.ReasoningContent)
							if fr := sc.Choices[0].FinishReason; fr != "" {
								streamFinish = fr
							}
							for _, tc := range sc.Choices[0].Delta.ToolCalls {
								cur, ok := streamToolCalls[tc.Index]
								if !ok {
									cur = &struct {
										ID       string `json:"id"`
										Type     string `json:"type"`
										Function struct {
											Name      string `json:"name"`
											Arguments string `json:"arguments"`
										} `json:"function"`
									}{}
									streamToolCalls[tc.Index] = cur
									streamToolOrder = append(streamToolOrder, tc.Index)
								}
								if tc.ID != "" {
									cur.ID = tc.ID
								}
								if tc.Type != "" {
									cur.Type = tc.Type
								}
								if tc.Function.Name != "" {
									cur.Function.Name = tc.Function.Name
								}
								cur.Function.Arguments += tc.Function.Arguments
							}
						}
					}
				}
			}
			if _, werr := fmt.Fprintf(c.Writer, "%s\n", line); werr != nil {
				slog.Debug("sse loop: write to client failed", "module", "cb", "error", werr)
				lastResp.Body.Close()
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		slog.Debug("sse loop: ended",
			"module", "cb",
			"chunks", streamChunks,
			"scanner_err", func() string {
				if scanner.Err() != nil {
					return scanner.Err().Error()
				}
				return ""
			}(),
			"ctx_err", func() string {
				if ctx.Err() != nil {
					return ctx.Err().Error()
				}
				return ""
			}(),
			"finish", streamFinish,
			"content_len", streamContent.Len())
		c.Set("output_text", truncateLog(streamContent.String(), 1000))
		c.Set("tokens_in", streamTokensIn)
		c.Set("tokens_out", streamTokensOut)
		// Build the stored response_body. Use the full upstream usage map (which
		// contains prompt_cache_hit_tokens etc.) instead of a minimal 3-field
		// reconstruction — extractCacheHitPct needs the cache fields.
		respUsage := streamUsage
		if respUsage == nil {
			respUsage = gin.H{
				"prompt_tokens":     streamTokensIn,
				"completion_tokens": streamTokensOut,
				"total_tokens":      streamTokensIn + streamTokensOut,
			}
		}
		msg := gin.H{"role": "assistant", "content": streamContent.String()}
		if r := streamReasoning.String(); r != "" {
			msg["reasoning_content"] = r
		}
		if len(streamToolOrder) > 0 {
			tcs := make([]gin.H, 0, len(streamToolOrder))
			for _, idx := range streamToolOrder {
				cur := streamToolCalls[idx]
				tcs = append(tcs, gin.H{
					"id":   cur.ID,
					"type": cur.Type,
					"function": gin.H{
						"name":      cur.Function.Name,
						"arguments": cur.Function.Arguments,
					},
				})
			}
			msg["tool_calls"] = tcs
		}
		finish := streamFinish
		if finish == "" {
			finish = "stop"
		}
		respJSON, _ := json.Marshal(gin.H{
			"choices": []gin.H{{
				"message":       msg,
				"finish_reason": finish,
			}},
			"usage":  respUsage,
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
