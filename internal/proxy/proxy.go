// Package proxy wires the HTTP entrypoint (/v1/chat/completions, /v1/models)
// to the correct upstream — Grok or CodeBuddy — and emits Prometheus metrics
// + async ClickHouse audit rows for every proxied call.
//
// Dependencies:
//   - internal/upstream  (isGrokModel, expandGrokAlias, proxyGrok, proxyCodeBuddy, MAX_REQUEST_BODY)
//   - internal/db        (RequestLog DTO, Store.LogRequest)
//   - internal/auth      (Manager.Get / IsModelAllowed / IncrementTokens / IncrementRequests)
//   - internal/metrics   (RequestsTotal, RequestDuration)
package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"foxrouters/internal/auth"
	"foxrouters/internal/db"
	"foxrouters/internal/metrics"
	"foxrouters/internal/upstream"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// UTILITY FUNCTIONS
// ============================================================================

// extractInputText: extract last user message from request body for logging.
// Truncates to 500 chars to avoid bloating the DB.
func extractInputText(bodyMap map[string]any) string {
	msgs, ok := bodyMap["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return ""
	}
	// Find last user message
	for i := len(msgs) - 1; i >= 0; i-- {
		msg, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "user" {
			continue
		}
		content := msg["content"]
		switch v := content.(type) {
		case string:
			return upstream.TruncateLog(v, 500)
		case []any:
			// Array of content parts (vision etc.) — extract text parts
			var parts []string
			for _, p := range v {
				pm, ok := p.(map[string]any)
				if !ok {
					continue
				}
				if pt, _ := pm["type"].(string); pt == "text" {
					if txt, ok := pm["text"].(string); ok {
						parts = append(parts, txt)
					}
				}
			}
			if len(parts) > 0 {
				return upstream.TruncateLog(strings.Join(parts, " "), 500)
			}
		}
	}
	return ""
}

// toInt safely converts interface{} from c.Get() to int.
func toInt(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// toString safely converts interface{} from c.Get() to string.
func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// ============================================================================
// MAIN HANDLER
// ============================================================================

// ProxyRequest routes /v1/chat/completions to Grok or CodeBuddy based on the
// requested model, expands Grok aliases, enforces per-key model whitelists,
// records Prometheus metrics, updates per-key token quotas, and emits an
// async RequestLog to ClickHouse for chat completions only.
//
// The optional `registry` argument (may be nil) resolves runtime-configured
// custom models + aliases (see internal/proxy/custom.go). Aliases are
// rewritten in-body before routing; custom models override the default
// grok-* / cb/* prefix routing.
//
// The optional `combos` argument (may be nil) resolves "combo/<name>"
// virtual models into concrete backend models. See internal/proxy/combo.go.
// Fallback combos retry on 5xx by buffering the upstream response through a
// httptest-style recorder and only flushing to the real writer on success
// or list exhaustion.
func ProxyRequest(grokAM *upstream.GrokAccountManager, cbKM *upstream.CBKeyManager, fbAM *upstream.FreebuffAccountManager, hc *upstream.HealthChecker, authMgr *auth.Manager, registry *CustomRegistry, combos *ComboRegistry) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path

		// /v1/models — local
		if path == "/v1/models" || path == "/models" {
			models := []gin.H{
				// CodeBuddy — GPT
				{"id": "gpt-5.6-sol", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "gpt-5.6-terra", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "gpt-5.6-luna", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "gpt-5.5", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "gpt-5.4", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "gpt-4.1", "object": "model", "owned_by": "codebuddy", "reasoning": false},
				{"id": "gpt-5.3-codex", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				// CodeBuddy — Claude
				{"id": "claude-opus-4.7-1m", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "claude-opus-4.6", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "claude-sonnet-4.6", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				// CodeBuddy — Gemini
				{"id": "gemini-3.1-pro", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "gemini-3.5-flash", "object": "model", "owned_by": "codebuddy", "reasoning": false},
				// CodeBuddy — GLM
				{"id": "glm-5.2", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "glm-5.1", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "glm-5.0", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				// CodeBuddy — DeepSeek
				{"id": "deepseek-v3", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				// CodeBuddy — Kimi
				{"id": "kimi-k2.5", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "kimi-k3", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				// CodeBuddy — Default
				{"id": "default-model", "object": "model", "owned_by": "codebuddy", "reasoning": true},
			}
			// Grok models — dynamic from upstream /v1/models when refreshed,
			// static fallback otherwise (aliases are reasoning_effort variants).
			if gEntries := upstream.GetGrokModels(); len(gEntries) > 0 {
				for _, g := range gEntries {
					models = append(models, gin.H{"id": g.ID, "object": "model", "owned_by": "xai", "reasoning": true})
					for _, e := range g.ReasoningEfforts {
						if e != "" {
							models = append(models, gin.H{"id": g.ID + "-" + e, "object": "model", "owned_by": "xai", "reasoning": true})
						}
					}
					if g.ID == "grok-4.5" {
						// Gateway-internal aliases (not in upstream efforts list)
						models = append(models,
							gin.H{"id": "grok-4.5-auto", "object": "model", "owned_by": "xai", "reasoning": true},
							gin.H{"id": "grok-4.5-none", "object": "model", "owned_by": "xai", "reasoning": true})
					}
				}
			} else {
				models = append(models,
					gin.H{"id": "grok-4.5", "object": "model", "owned_by": "xai", "reasoning": true},
					gin.H{"id": "grok-4.5-high", "object": "model", "owned_by": "xai", "reasoning": true},
					gin.H{"id": "grok-4.5-medium", "object": "model", "owned_by": "xai", "reasoning": true},
					gin.H{"id": "grok-4.5-low", "object": "model", "owned_by": "xai", "reasoning": true},
					gin.H{"id": "grok-4.5-xhigh", "object": "model", "owned_by": "xai", "reasoning": true},
					gin.H{"id": "grok-4.5-auto", "object": "model", "owned_by": "xai", "reasoning": true},
					gin.H{"id": "grok-4.5-none", "object": "model", "owned_by": "xai", "reasoning": true},
					gin.H{"id": "grok-4", "object": "model", "owned_by": "xai", "reasoning": true},
					gin.H{"id": "grok-4-fast-reasoning", "object": "model", "owned_by": "xai", "reasoning": true},
					gin.H{"id": "grok-code-fast-1", "object": "model", "owned_by": "xai", "reasoning": true})
			}
			// Freebuff models — dynamic from the model registry
			// (fetched from freebuff-models.json), static fallback otherwise.
			for _, m := range upstream.GetFBModels() {
				models = append(models, gin.H{"id": m.GatewayID, "object": "model", "owned_by": "freebuff", "reasoning": m.Reasoning})
			}
			// Backward-compat aliases: cb/<model> → same model, same upstream.
			// Routing treats non-grok-* as CodeBuddy, so both shapes work.
			cbAliases := []gin.H{
				{"id": "cb/gpt-5.6-sol", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "cb/gpt-5.6-terra", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "cb/gpt-5.6-luna", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "cb/gpt-5.5", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "cb/gpt-5.4", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "cb/gpt-4.1", "object": "model", "owned_by": "codebuddy", "reasoning": false},
				{"id": "cb/gpt-5.3-codex", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "cb/claude-opus-4.7-1m", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "cb/claude-opus-4.6", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "cb/claude-sonnet-4.6", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "cb/gemini-3.1-pro", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "cb/gemini-3.5-flash", "object": "model", "owned_by": "codebuddy", "reasoning": false},
				{"id": "cb/glm-5.2", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "cb/glm-5.1", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "cb/glm-5.0", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "cb/deepseek-v3", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "cb/kimi-k2.5", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "cb/kimi-k3", "object": "model", "owned_by": "codebuddy", "reasoning": true},
				{"id": "cb/default-model", "object": "model", "owned_by": "codebuddy", "reasoning": true},
			}
			models = append(models, cbAliases...)
			// Append runtime-registered custom models.
			if registry != nil {
				for _, entry := range registry.ListModels() {
					models = append(models, gin.H{"id": entry.ID, "object": "model", "owned_by": entry.OwnedBy})
				}
			}
			// Append runtime-registered combos (v1.4.0).
			if combos != nil {
				for _, c := range combos.ListCombos() {
					models = append(models, gin.H{"id": "combo/" + c.Name, "object": "model", "owned_by": "foxrouters"})
				}
			}
			// Anthropic clients (Claude Code, anthropic-sdk-*, etc.) probe
			// GET /v1/models and expect Anthropic-shaped entries. Detect them
			// by well-known request headers and enrich each entry in-place
			// with {type, display_name, created_at}. OpenAI fields stay too
			// so the same response works for both client families.
			if isAnthropicClient(c) {
				for i := range models {
					id, _ := models[i]["id"].(string)
					models[i]["type"] = "model"
					models[i]["display_name"] = displayNameForModel(id)
					models[i]["created_at"] = "2025-01-01T00:00:00Z"
				}
			}
			c.JSON(200, gin.H{"object": "list", "data": models})
			return
		}

		// Only handle chat completions
		if path != "/v1/chat/completions" && path != "/chat/completions" {
			c.JSON(404, gin.H{"error": "not found: " + path})
			return
		}

		// Cap request body to prevent OOM / DoS via multi-GB uploads.
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, upstream.MAX_REQUEST_BODY)
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			// MaxBytesReader returns *http.MaxBytesError when limit exceeded
			if _, ok := err.(*http.MaxBytesError); ok {
				c.JSON(413, gin.H{"error": "request body too large", "limit_bytes": upstream.MAX_REQUEST_BODY})
				return
			}
			c.JSON(400, gin.H{"error": "read body failed"})
			return
		}

		var bodyMap map[string]any
		if err := json.Unmarshal(body, &bodyMap); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON"})
			return
		}

		// Rewrite agent identity strings before forwarding to upstream.
		rewriteAgentIdentity(bodyMap)
		body, _ = json.Marshal(bodyMap)

		model, _ := bodyMap["model"].(string)
		if model == "" {
			model = "grok-4.5"
			bodyMap["model"] = model
			body, _ = json.Marshal(bodyMap)
		}

		// Custom alias + custom model resolution (runtime-configured).
		// 1) Alias rewrite (single hop): "my-claude" → "cb/claude-sonnet-4.6"
		// 2) Custom-model lookup: if resolved id is registered, override the
		//    routing upstream + the model_name that goes over the wire.
		var customUpstream, customModelName string
		if registry != nil {
			resolved, up, mn := registry.Resolve(model)
			if resolved != model {
				model = resolved
				bodyMap["model"] = model
				body, _ = json.Marshal(bodyMap)
			}
			customUpstream = up
			customModelName = mn
		}

		// Combo resolution (v1.4.0). Runs after custom alias but before
		// default grok/cb prefix routing. On a hit we rewrite the model in
		// the request body to the concrete backend model and, for fallback
		// strategy, remember the combo name so the dispatch loop below can
		// walk to the next model on 5xx.
		var comboName string
		var comboStrategy string
		if combos != nil {
			if next, ok := combos.Resolve(model); ok {
				// Remember for fallback retry chain.
				if strings.HasPrefix(model, "combo/") {
					comboName = strings.TrimPrefix(model, "combo/")
					if cb, ok2 := combos.GetCombo(comboName); ok2 {
						comboStrategy = cb.Strategy
					}
				}
				model = next
				bodyMap["model"] = model
				body, _ = json.Marshal(bodyMap)
				// Re-run custom alias resolution against the concrete model
				// so a combo pointing at "my-claude" still resolves through
				// the alias table (single hop).
				if registry != nil {
					resolved, up, mn := registry.Resolve(model)
					if resolved != model {
						model = resolved
						bodyMap["model"] = model
						body, _ = json.Marshal(bodyMap)
					}
					customUpstream = up
					customModelName = mn
				}
			}
		}

		// Model alias expansion: grok-4.5-{high,medium,low,auto,none} → grok-4.5 + reasoning_effort
		// Mirrors 9router's grok-cli provider (upstreamModelId + thinking level).
		// Skip when a custom model has already routed us — the custom model_name
		// is authoritative in that case.
		if customUpstream == "" {
			if effort, ok := upstream.ExpandGrokAlias(model); ok {
				model = "grok-4.5"
				bodyMap["model"] = model
				// Only set reasoning_effort if client didn't specify one already
				if _, has := bodyMap["reasoning_effort"]; !has {
					if _, has2 := bodyMap["reasoning"]; !has2 {
						bodyMap["reasoning_effort"] = effort
					}
				}
				body, _ = json.Marshal(bodyMap)
			}
		}

		// Per-key model whitelist check.
		// If the key has allowed_models set, reject models not on the list.
		// Supports glob: "grok-*", "cb/*", or exact "cb/gpt-5.5".
		fullKey := c.GetString("client_key")
		if fullKey != "" && authMgr != nil {
			if info, ok := authMgr.Get(fullKey); ok {
				if !info.IsModelAllowed(model) {
					c.JSON(403, gin.H{
						"error": "model not allowed for this API key",
						"model": model,
						"hint":  "this key is restricted to specific models — contact the gateway operator",
					})
					c.Set("error_msg", "model not allowed: "+model)
					errJSON, _ := json.Marshal(gin.H{"error": "model not allowed", "model": model})
					c.Set("response_body", json.RawMessage(errJSON))
					return
				}
			}
		}

		clientStream := false
		if s, ok := bodyMap["stream"].(bool); ok && s {
			clientStream = true
		}
		if c.GetHeader("Accept") == "text/event-stream" {
			clientStream = true
		}

		startTime := time.Now()
		upstreamName := "codebuddy"

		// dispatch runs the routing switch for a single (model, body). It
		// mutates upstreamName so the metrics/logging block below sees the
		// last-tried upstream. Extracted into a closure so fallback combos
		// can invoke it in a loop with different models.
		dispatch := func(m string, b []byte, bm map[string]any) {
			switch customUpstream {
			case "grok":
				upstreamName = "grok"
				effectiveModel := m
				if customModelName != "" {
					effectiveModel = customModelName
					bm["model"] = effectiveModel
					b, _ = json.Marshal(bm)
				}
				upstream.ProxyGrok(c, b, grokAM, clientStream, hc, effectiveModel)
			case "codebuddy":
				upstreamName = "codebuddy"
				if customModelName != "" {
					// cbTransform will TrimPrefix("cb/") on this — prepend so the
					// upstream sees exactly customModelName.
					bm["model"] = "cb/" + customModelName
					b, _ = json.Marshal(bm)
				}
				upstream.ProxyCodeBuddy(c, b, bm, cbKM, clientStream, hc)
			default:
				if upstream.IsFreebuffModel(m) {
					upstreamName = "freebuff"
					upstream.ProxyFreebuff(c, b, bm, fbAM, clientStream, hc)
				} else if upstream.IsGrokModel(m) {
					upstreamName = "grok"
					upstream.ProxyGrok(c, b, grokAM, clientStream, hc, m)
				} else {
					upstreamName = "codebuddy"
					upstream.ProxyCodeBuddy(c, b, bm, cbKM, clientStream, hc)
				}
			}
		}

		// Fallback combo retry chain:
		//   For non-streaming requests we wrap c.Writer in a buffering
		//   recorder, invoke dispatch, then inspect status. On 5xx (or
		//   circuit-open 503) we peel model resolution + try the next entry.
		//   4xx bails out immediately — client errors don't get retried.
		//
		//   Streaming (SSE) requests skip retry: once the first byte hits
		//   the wire we cannot un-send it. They use the head model only.
		if comboName != "" && comboStrategy == "fallback" && !clientStream {
			// Snapshot the concrete-model chain from the combo.
			cb, _ := combos.GetCombo(comboName)
			// Walk models[0..N-1]; model already holds models[0] (resolved
			// earlier). On retry we resolve alias + custom-model again for
			// each candidate so combos of aliases keep working.
			var lastRecorder *bufferedWriter
			for _, candidate := range cb.Models {
				// Re-run custom alias + custom-model on this candidate.
				candModel := candidate
				candCustomUp := ""
				candCustomMN := ""
				if registry != nil {
					r, up, mn := registry.Resolve(candModel)
					candModel = r
					candCustomUp = up
					candCustomMN = mn
				}
				// Re-expand grok alias when no custom routing hit.
				if candCustomUp == "" {
					if effort, ok := upstream.ExpandGrokAlias(candModel); ok {
						candModel = "grok-4.5"
						bodyMap["model"] = candModel
						if _, has := bodyMap["reasoning_effort"]; !has {
							if _, has2 := bodyMap["reasoning"]; !has2 {
								bodyMap["reasoning_effort"] = effort
							}
						}
					}
				}
				bodyMap["model"] = candModel
				candBody, _ := json.Marshal(bodyMap)

				// Buffer the upstream response so we can decide to retry.
				bw := newBufferedWriter(c.Writer)
				origWriter := c.Writer
				c.Writer = bw

				// Temporarily swap custom-upstream vars for this attempt so
				// the closure routes correctly.
				savedUp, savedMN := customUpstream, customModelName
				customUpstream = candCustomUp
				customModelName = candCustomMN
				dispatch(candModel, candBody, bodyMap)
				customUpstream, customModelName = savedUp, savedMN

				c.Writer = origWriter
				model = candModel // track last-tried for logging

				// 2xx / 3xx / 4xx — final. 4xx isn't retried (client error).
				if bw.status < 500 {
					lastRecorder = bw
					break
				}
				lastRecorder = bw
				// else continue to next candidate
			}
			// Flush whichever recorder held the terminal response.
			if lastRecorder != nil {
				lastRecorder.flush()
			}
		} else {
			// Routing decision:
			//   1. Custom model → routes to its declared upstream. If a ModelName
			//      override is set, we rewrite bodyMap[model] to that name so the
			//      upstream sees the "real" model. cbTransform strips the cb/
			//      prefix, so for CodeBuddy we prepend one to keep its stripCBPrefix
			//      happy; grok upstream sees the model_name as-is.
			//   2. Fall through to prefix routing (grok-* vs cb/*).
			dispatch(model, body, bodyMap)
		}

		// Record Prometheus metrics for this proxied request. Bucket status by
		// 3-digit HTTP code (200, 429, 500). Duration in seconds for the
		// standard histogram buckets. Cheap: label lookups + atomic increments.
		elapsed := time.Since(startTime).Seconds()
		status := strconv.Itoa(c.Writer.Status())
		metrics.RequestsTotal.WithLabelValues(upstreamName, status).Inc()
		metrics.RequestDuration.WithLabelValues(upstreamName).Observe(elapsed)

		// Per-key token quota tracking
		fullKey = c.GetString("client_key")
		if fullKey != "" && authMgr != nil {
			tokensIn, _ := c.Get("tokens_in")
			tokensOut, _ := c.Get("tokens_out")
			totalTokens := int64(toInt(tokensIn) + toInt(tokensOut))
			if totalTokens > 0 {
				authMgr.IncrementTokens(fullKey, totalTokens)
			} else {
				authMgr.IncrementRequests(fullKey)
			}
		}

		// Async log to ClickHouse — only for chat completion endpoint,
		// not for probes to /v1/models, /health, /props, etc.
		if grokAM.DB() != nil && path == "/v1/chat/completions" {
			inputText := extractInputText(bodyMap)
			outputText, _ := c.Get("output_text")
			tokensIn, _ := c.Get("tokens_in")
			tokensOut, _ := c.Get("tokens_out")
			responseBody, _ := c.Get("response_body")

			// Full request/response JSON stored in ClickHouse (ZSTD) — unlimited.
			rl := db.RequestLog{
				Timestamp:  startTime,
				RequestID:  c.GetString("request_id"),
				ClientKey:  c.GetString("client_key_masked"),
				Model:      model,
				Upstream:   upstreamName,
				AccountID:  c.GetString("upstream_account"),
				StatusCode: c.Writer.Status(),
				LatencyMs:  int(time.Since(startTime).Milliseconds()),
				TokensIn:   toInt(tokensIn),
				TokensOut:  toInt(tokensOut),
				InputText:  inputText,
				OutputText: toString(outputText),
			}
			// Capture error message for non-2xx responses (audit trail)
			if errMsg, exists := c.Get("error_msg"); exists {
				rl.ErrorMsg = toString(errMsg)
			}
			if len(body) > 0 {
				rl.RequestBody = json.RawMessage(body)
			}
			if rb, ok := responseBody.(json.RawMessage); ok && len(rb) > 0 {
				rl.ResponseBody = rb
			} else if errMsg, exists := c.Get("response_body"); exists {
				// Fallback: error branches set response_body directly
				if rb, ok := errMsg.(json.RawMessage); ok && len(rb) > 0 {
					rl.ResponseBody = rb
				}
			}
			grokAM.DB().LogRequest(rl)
		}
	}
}

// rewriteAgentIdentity replaces agent identity strings in message content
// before forwarding to upstream. Walks all messages (system/user/assistant)
// and applies string replacement to both plain-string and array-of-blocks
// content formats, plus tool_result nested blocks and tool descriptions.
// Mirrors etteum-pool's sanitizeRequest.
func rewriteAgentIdentity(bm map[string]any) {
	if msgs, ok := bm["messages"].([]any); ok {
		for _, msg := range msgs {
			m, ok := msg.(map[string]any)
			if !ok {
				continue
			}
			rewriteMessageContent(m)
		}
	}
	// Sanitize tool definitions: tools[].function.description
	if tools, ok := bm["tools"].([]any); ok {
		for _, tool := range tools {
			t, ok := tool.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := t["function"].(map[string]any)
			if !ok {
				continue
			}
			if desc, ok := fn["description"].(string); ok {
				fn["description"] = rewriteAgentStrings(desc)
			}
		}
	}
}

// rewriteMessageContent rewrites the "content" field of a single message.
// Handles: plain string, array of content blocks (text), and tool_result
// blocks whose content is either a string or an array of text blocks.
func rewriteMessageContent(m map[string]any) {
	switch v := m["content"].(type) {
	case string:
		m["content"] = rewriteAgentStrings(v)
	case []any:
		for _, block := range v {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			switch t := b["type"].(type) {
			case string:
				if t == "tool_result" {
					rewriteToolResultContent(b)
					continue
				}
			}
			if text, ok := b["text"].(string); ok {
				b["text"] = rewriteAgentStrings(text)
			}
		}
	}
}

// rewriteToolResultContent sanitizes a tool_result content block, which can
// carry either a plain string or an array of text blocks.
func rewriteToolResultContent(b map[string]any) {
	switch v := b["content"].(type) {
	case string:
		b["content"] = rewriteAgentStrings(v)
	case []any:
		for _, inner := range v {
			ib, ok := inner.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := ib["text"].(string); ok {
				ib["text"] = rewriteAgentStrings(text)
			}
		}
	}
}

// rewriteAgentStrings applies the full sanitization pipeline to a text string.
// Delegates to ApplyFilters (the pudidil filter rule set).
func rewriteAgentStrings(s string) string {
	return ApplyFilters(s)
}
