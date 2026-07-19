// Package handlers — Anthropic Messages API adapter.
//
// POST /v1/messages endpoint that:
//   1. Parses Anthropic-format request (system field, content blocks, x-api-key)
//   2. Converts to OpenAI /v1/chat/completions format
//   3. Forwards through the existing proxy.ProxyRequest (Grok/CodeBuddy routing,
//      auth, rate-limit, metrics, ClickHouse audit — all reused)
//   4. Translates the OpenAI response (JSON or SSE) back to Anthropic format
//
// This lets Claude Code point at FoxRouters with ANTHROPIC_BASE_URL and get
// Grok / CodeBuddy behind the scenes.
package handlers

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"foxrouters/internal/auth"
	"foxrouters/internal/proxy"
	"foxrouters/internal/upstream"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// REQUEST TYPES (Anthropic Messages API)
// ============================================================================

// anthropicRequest mirrors https://docs.anthropic.com/en/api/messages.
// Content can be a plain string OR an array of content blocks — we keep it
// as json.RawMessage and decode on the fly.
type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        json.RawMessage    `json:"system,omitempty"` // string OR []block
	Messages      []anthropicMessage `json:"messages"`
	Stream        bool               `json:"stream,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Metadata      json.RawMessage    `json:"metadata,omitempty"`
	Tools         json.RawMessage    `json:"tools,omitempty"`      // pass-through — best effort
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentBlockIn: one block from an Anthropic content-array message.
type contentBlockIn struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// Vision / other block types are ignored for now (best-effort text extraction).
}

// ============================================================================
// RESPONSE TYPES (Anthropic Messages API)
// ============================================================================

type anthropicResponse struct {
	ID           string                    `json:"id"`
	Type         string                    `json:"type"`
	Role         string                    `json:"role"`
	Model        string                    `json:"model"`
	Content      []anthropicContentBlockOut `json:"content"`
	StopReason   string                    `json:"stop_reason"`
	StopSequence *string                   `json:"stop_sequence"`
	Usage        anthropicUsage            `json:"usage"`
}

type anthropicContentBlockOut struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ============================================================================
// MODEL MAPPING
// ============================================================================

// mapAnthropicModel translates a client-supplied Anthropic model name into a
// FoxRouters upstream model. Rules:
//   - Custom alias match → return the alias target (checked first so users
//     can override any of the built-in rules below).
//   - Explicit "cb/" or "grok-" prefix → passthrough (client escape hatch)
//   - Model containing "grok" → grok-4.5
//   - Anything else (claude-*) → ANTHROPIC_DEFAULT_MODEL env, or cb/claude-sonnet-4.6
func mapAnthropicModel(m string, reg *proxy.CustomRegistry) string {
	m = strings.TrimSpace(m)
	if m == "" {
		return defaultAnthropicUpstream()
	}
	// Custom alias takes precedence over any hardcoded rule.
	if reg != nil {
		resolved, _, _ := reg.Resolve(m)
		if resolved != m {
			return resolved
		}
	}
	if strings.HasPrefix(m, "cb/") || strings.HasPrefix(m, "grok-") {
		return m
	}
	lower := strings.ToLower(m)
	// Escape hatch: "...-grok" or "grok" in the name → route to Grok.
	if strings.Contains(lower, "grok") {
		return "grok-4.5"
	}
	return defaultAnthropicUpstream()
}

func defaultAnthropicUpstream() string {
	if v := strings.TrimSpace(os.Getenv("ANTHROPIC_DEFAULT_MODEL")); v != "" {
		return v
	}
	return "cb/claude-sonnet-4.6"
}

// ============================================================================
// REQUEST TRANSLATION (Anthropic → OpenAI)
// ============================================================================

// extractText returns the text content from either a plain-string content or
// an array of content blocks. Non-text blocks are skipped.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Fall back to array of blocks.
	var blocks []contentBlockIn
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// buildOpenAIBody translates Anthropic → OpenAI /v1/chat/completions payload.
func buildOpenAIBody(req *anthropicRequest, reg *proxy.CustomRegistry) ([]byte, string, error) {
	upstreamModel := mapAnthropicModel(req.Model, reg)

	msgs := make([]map[string]any, 0, len(req.Messages)+1)

	// system field (string or array of blocks) → leading system message.
	if len(req.System) > 0 {
		systxt := extractText(req.System)
		if strings.TrimSpace(systxt) != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": systxt})
		}
	}

	for _, m := range req.Messages {
		txt := extractText(m.Content)
		msgs = append(msgs, map[string]any{"role": m.Role, "content": txt})
	}

	out := map[string]any{
		"model":    upstreamModel,
		"messages": msgs,
		"stream":   req.Stream,
	}
	if req.MaxTokens > 0 {
		out["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}

	buf, err := json.Marshal(out)
	if err != nil {
		return nil, "", err
	}
	return buf, upstreamModel, nil
}

// mapStopReason: OpenAI finish_reason → Anthropic stop_reason.
func mapStopReason(openaiFinish string) string {
	switch openaiFinish {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "stop_sequence"
	case "tool_calls", "function_call":
		return "tool_use"
	case "":
		return "end_turn"
	}
	return "end_turn"
}

// genMsgID: Anthropic-style "msg_<hex>" identifier.
func genMsgID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "msg_" + hex.EncodeToString(b[:])
}

// ============================================================================
// RESPONSE INTERCEPTOR — buffered gin.ResponseWriter wrapper
// ============================================================================

// captureWriter buffers everything written by the downstream handler so we
// can rewrite it. Also silently absorbs Flush() calls (nothing hits the
// client until we're done translating).
type captureWriter struct {
	gin.ResponseWriter
	buf    bytes.Buffer
	status int
	header http.Header
}

func newCaptureWriter(orig gin.ResponseWriter) *captureWriter {
	return &captureWriter{
		ResponseWriter: orig,
		status:         200,
		header:         http.Header{},
	}
}

func (w *captureWriter) Header() http.Header       { return w.header }
func (w *captureWriter) WriteHeader(code int)      { w.status = code }
func (w *captureWriter) WriteHeaderNow()           {}
func (w *captureWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *captureWriter) WriteString(s string) (int, error) {
	return w.buf.WriteString(s)
}
func (w *captureWriter) Status() int   { return w.status }
func (w *captureWriter) Size() int     { return w.buf.Len() }
func (w *captureWriter) Written() bool { return w.buf.Len() > 0 }
func (w *captureWriter) Flush()        { /* swallow — we translate at the end */ }

// ============================================================================
// STREAMING PIPELINE — parse OpenAI SSE, emit Anthropic SSE in real time
// ============================================================================

// streamWriter parses OpenAI-format SSE chunks arriving via Write() and emits
// Anthropic-format SSE events to the real writer. It expects data lines of
// the form `data: {json}` and the terminator `data: [DONE]`.
type streamWriter struct {
	gin.ResponseWriter
	real       gin.ResponseWriter
	flusher    http.Flusher
	msgID      string
	model      string
	started    bool // message_start + content_block_start already sent
	stopped    bool // message_stop already sent
	finish     string
	inputToks  int
	outputToks int
	textBuf    strings.Builder
	carry      string // partial line from previous Write
	errBuf     []byte // upstream error body captured before streaming started
	// Headers set by the downstream proxy — we don't forward them; we set our own.
	sinkHeader http.Header
	statusCode int
}

func newStreamWriter(real gin.ResponseWriter, msgID, model string) *streamWriter {
	fl, _ := real.(http.Flusher)
	return &streamWriter{
		ResponseWriter: real,
		real:           real,
		flusher:        fl,
		msgID:          msgID,
		model:          model,
		sinkHeader:     http.Header{},
		statusCode:     200,
	}
}

func (w *streamWriter) Header() http.Header  { return w.sinkHeader }
func (w *streamWriter) Status() int          { return w.statusCode }
func (w *streamWriter) Size() int            { return -1 }
func (w *streamWriter) Written() bool        { return w.started }
func (w *streamWriter) WriteHeaderNow()      {}

func (w *streamWriter) WriteHeader(code int) {
	// Capture status but DON'T commit to real writer yet.
	// The outer handler will decide format (SSE vs JSON error) based on
	// whether streaming actually started. This prevents double WriteHeader
	// and lets us surface upstream error bodies cleanly.
	w.statusCode = code
}

// ensureStart emits message_start + content_block_start on the wire, once.
func (w *streamWriter) ensureStart() {
	if w.started {
		return
	}
	w.started = true
	// Set SSE headers on the real writer.
	h := w.real.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.real.WriteHeader(200)

	startMsg := map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            w.msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         w.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	}
	w.emitEvent("message_start", startMsg)

	blockStart := map[string]any{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]any{"type": "text", "text": ""},
	}
	w.emitEvent("content_block_start", blockStart)
}

// emitEvent writes one Anthropic SSE frame (event: <name>\ndata: <json>\n\n).
func (w *streamWriter) emitEvent(name string, data any) {
	buf, _ := json.Marshal(data)
	fmt.Fprintf(w.real, "event: %s\ndata: %s\n\n", name, buf)
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

func (w *streamWriter) Write(p []byte) (int, error) {
	// If upstream errored before streaming started, buffer the error body
	// so the outer handler can surface it to the client. Without this, the
	// line-splitter below would silently drop non-SSE bytes.
	if !w.started && w.statusCode >= 400 {
		w.errBuf = append(w.errBuf, p...)
		return len(p), nil
	}
	// Feed bytes through a line splitter; parse `data: {...}` frames.
	chunk := w.carry + string(p)
	lines := strings.Split(chunk, "\n")
	w.carry = lines[len(lines)-1]
	for _, ln := range lines[:len(lines)-1] {
		ln = strings.TrimRight(ln, "\r")
		w.processLine(ln)
	}
	return len(p), nil
}

// ErrBuffer returns bytes captured while in the pre-start error state.
// Returns nil once streaming has begun.
func (w *streamWriter) ErrBuffer() []byte { return w.errBuf }

// processLine parses a single OpenAI-format SSE line.
func (w *streamWriter) processLine(line string) {
	if !strings.HasPrefix(line, "data: ") && !strings.HasPrefix(line, "data:") {
		return
	}
	data := strings.TrimPrefix(strings.TrimPrefix(line, "data: "), "data:")
	data = strings.TrimSpace(data)
	if data == "" {
		return
	}
	if data == "[DONE]" {
		w.finalize()
		return
	}

	// Parse OpenAI chunk.
	var oc struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
				Role    string `json:"role"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &oc); err != nil {
		return
	}
	if oc.Usage != nil {
		w.inputToks = oc.Usage.PromptTokens
		w.outputToks = oc.Usage.CompletionTokens
	}
	if len(oc.Choices) == 0 {
		return
	}
	ch := oc.Choices[0]
	if ch.Delta.Content != "" {
		w.ensureStart()
		w.textBuf.WriteString(ch.Delta.Content)
		w.emitEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "text_delta", "text": ch.Delta.Content},
		})
	}
	if ch.FinishReason != nil && *ch.FinishReason != "" {
		w.finish = *ch.FinishReason
	}
}

// finalize emits content_block_stop + message_delta + message_stop.
func (w *streamWriter) finalize() {
	if w.stopped {
		return
	}
	w.ensureStart() // in case upstream sent no content, still emit shell
	w.stopped = true

	w.emitEvent("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": 0,
	})
	w.emitEvent("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   mapStopReason(w.finish),
			"stop_sequence": nil,
		},
		"usage": map[string]any{"output_tokens": w.outputToks},
	})
	w.emitEvent("message_stop", map[string]any{"type": "message_stop"})
}

func (w *streamWriter) Flush() {
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

// ============================================================================
// AUTH — Anthropic accepts x-api-key; we also accept Authorization: Bearer
// ============================================================================

// AnthropicAuthMiddleware normalises Anthropic's x-api-key header into the
// standard Authorization: Bearer form so the existing auth.AuthMiddleware
// (already installed on the router) can validate it. Runs BEFORE the main
// AuthMiddleware in the middleware chain.
func AnthropicAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Only touch /v1/messages — leave everything else alone.
		if c.Request.URL.Path != "/v1/messages" {
			c.Next()
			return
		}
		if c.GetHeader("Authorization") == "" {
			if k := c.GetHeader("x-api-key"); k != "" {
				c.Request.Header.Set("Authorization", "Bearer "+k)
			}
		}
		c.Next()
	}
}

// ============================================================================
// MAIN HANDLER
// ============================================================================

// HandleMessages implements POST /v1/messages (Anthropic Messages API).
// It reuses proxy.ProxyRequest for the actual upstream call — routing, auth
// (Bearer/x-api-key), rate limiting, metrics, and ClickHouse audit all
// continue to work unchanged; we just translate request/response formats.
func HandleMessages(grokAM *upstream.GrokAccountManager, cbKM *upstream.CBKeyManager, hc *upstream.HealthChecker, authMgr *auth.Manager, reg *proxy.CustomRegistry) gin.HandlerFunc {
	inner := proxy.ProxyRequest(grokAM, cbKM, hc, authMgr, reg)

	return func(c *gin.Context) {
		// Cap request body — same limit as chat/completions.
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, upstream.MAX_REQUEST_BODY)
		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			if _, ok := err.(*http.MaxBytesError); ok {
				c.JSON(413, gin.H{"type": "error", "error": gin.H{"type": "request_too_large", "message": "request body too large"}})
				return
			}
			c.JSON(400, gin.H{"type": "error", "error": gin.H{"type": "invalid_request_error", "message": "read body failed"}})
			return
		}

		var req anthropicRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			c.JSON(400, gin.H{"type": "error", "error": gin.H{"type": "invalid_request_error", "message": "invalid JSON: " + err.Error()}})
			return
		}
		if len(req.Messages) == 0 {
			c.JSON(400, gin.H{"type": "error", "error": gin.H{"type": "invalid_request_error", "message": "messages is required"}})
			return
		}
		if req.MaxTokens == 0 {
			// Anthropic makes this required; be permissive and default it.
			req.MaxTokens = 4096
		}

		// Translate → OpenAI format.
		openaiBody, upstreamModel, err := buildOpenAIBody(&req, reg)
		if err != nil {
			c.JSON(500, gin.H{"type": "error", "error": gin.H{"type": "api_error", "message": "translate failed: " + err.Error()}})
			return
		}

		clientModelName := req.Model
		if clientModelName == "" {
			clientModelName = upstreamModel
		}
		msgID := genMsgID()

		// Rewire the request so ProxyRequest thinks it's a normal chat/completions call.
		c.Request.Method = "POST"
		c.Request.URL.Path = "/v1/chat/completions"
		c.Request.Body = io.NopCloser(bytes.NewReader(openaiBody))
		c.Request.ContentLength = int64(len(openaiBody))
		c.Request.Header.Set("Content-Type", "application/json")

		// Streaming path: hook writer, let ProxyRequest pump SSE through us.
		if req.Stream {
			sw := newStreamWriter(c.Writer, msgID, clientModelName)
			origWriter := c.Writer
			c.Writer = sw
			defer func() { c.Writer = origWriter }()

			inner(c)

			// Flush any tail line still sitting in the carry buffer.
			if sw.carry != "" {
				sw.processLine(strings.TrimRight(sw.carry, "\r"))
				sw.carry = ""
			}
			// Guarantee terminal events even if upstream didn't emit [DONE].
			if sw.started && !sw.stopped {
				sw.finalize()
			}
			// If we never started (upstream errored before streaming), emit
			// a compact JSON error using whatever the proxy captured.
			if !sw.started {
				status := sw.statusCode
				if status < 400 {
					status = 502
				}
				// Prefer the error body we buffered directly from the upstream
				// response (P2 fix: streamWriter.Write now buffers non-SSE bytes
				// when !started && statusCode>=400, instead of dropping them).
				var bodyBytes []byte
				if len(sw.errBuf) > 0 {
					bodyBytes = sw.errBuf
				} else if rb, ok := c.Get("response_body"); ok {
					if rm, ok := rb.(json.RawMessage); ok {
						bodyBytes = []byte(rm)
					}
				}
				// P3 fix: parse + extract human-readable message instead of
				// embedding raw JSON string (avoids double-escaped output).
				var raw any
				if len(bodyBytes) > 0 {
					_ = json.Unmarshal(bodyBytes, &raw)
				}
				msg := extractUpstreamErrorMessage(raw, bodyBytes)
				c.Writer = origWriter
				c.Writer.Header().Set("Content-Type", "application/json")
				c.Writer.WriteHeader(status)
				out, _ := json.Marshal(map[string]any{
					"type":  "error",
					"error": map[string]any{"type": "api_error", "message": msg},
				})
				c.Writer.Write(out)
			}
			return
		}

		// Non-streaming path: buffer response, translate, then flush.
		cap := newCaptureWriter(c.Writer)
		origWriter := c.Writer
		c.Writer = cap
		inner(c)
		c.Writer = origWriter

		// ProxyRequest might have proxied SSE even though we asked for JSON
		// (CodeBuddy is stream-only upstream; the transform normally handles
		// the reduction, but be defensive: prefer c.Get("output_text") +
		// c.Get("tokens_in/out") which the proxy populates in BOTH modes).
		var (
			outputText string
			inputToks  int
			outputToks int
			finish     string
			upstreamErr = cap.status >= 400
		)
		if v, ok := c.Get("output_text"); ok {
			outputText, _ = v.(string)
		}
		if v, ok := c.Get("tokens_in"); ok {
			inputToks = anyToInt(v)
		}
		if v, ok := c.Get("tokens_out"); ok {
			outputToks = anyToInt(v)
		}

		// Parse the captured body: for a normal JSON response we can read
		// choices[0].message.content directly (more reliable than the
		// truncated output_text set by the proxy). For SSE, fall back to
		// scanning `data:` lines.
		bodyBytes := cap.buf.Bytes()
		if upstreamErr {
			// Parse upstream error body and extract a human-readable message
			// (P3 fix: avoid double-escaped JSON-in-JSON-in-JSON error envelope).
			// Anthropic spec: error.message should be a short string.
			status := cap.status
			c.Writer.Header().Set("Content-Type", "application/json")
			c.Writer.WriteHeader(status)
			var raw any
			_ = json.Unmarshal(bodyBytes, &raw)
			msg := extractUpstreamErrorMessage(raw, bodyBytes)
			out, _ := json.Marshal(map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": msg},
			})
			c.Writer.Write(out)
			return
		}

		if outputText == "" || strings.HasSuffix(outputText, "…") {
			// Try structured parse of the captured body for a full-fidelity string.
			text, finReason := extractFromCapturedBody(bodyBytes)
			if text != "" {
				outputText = text
			}
			if finish == "" {
				finish = finReason
			}
		}

		resp := anthropicResponse{
			ID:      msgID,
			Type:    "message",
			Role:    "assistant",
			Model:   clientModelName,
			Content: []anthropicContentBlockOut{{Type: "text", Text: outputText}},
			StopReason: mapStopReason(finish),
			Usage: anthropicUsage{
				InputTokens:  inputToks,
				OutputTokens: outputToks,
			},
		}
		out, _ := json.Marshal(resp)
		c.Writer.Header().Set("Content-Type", "application/json")
		c.Writer.WriteHeader(200)
		c.Writer.Write(out)
	}
}

// extractFromCapturedBody parses either a JSON chat.completion body OR a
// buffered SSE stream and returns (text, finish_reason).
// extractUpstreamErrorMessage walks common upstream error JSON shapes and
// returns a short human-readable message. Falls back to the raw body if it
// isn't JSON or no known field is found. Avoids embedding JSON strings
// inside JSON strings (the old behaviour caused triple-escaped output).
func extractUpstreamErrorMessage(raw any, bodyBytes []byte) string {
	if m, ok := raw.(map[string]any); ok {
		// Common fields in order of preference.
		for _, k := range []string{"message", "msg", "error", "detail", "detail.message", "error.message"} {
			if v, ok := m[k]; ok {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
				// Nested map: e.g. {"error": {"message": "..."}}
				if sub, ok := v.(map[string]any); ok {
					if s, ok := sub["message"].(string); ok && s != "" {
						return s
					}
				}
			}
		}
	}
	// Non-JSON or no known field — return the raw body (at least it's a string,
	// not a JSON-encoded object embedded in a string field).
	s := strings.TrimSpace(string(bodyBytes))
	if s == "" {
		return "upstream error"
	}
	return s
}

func extractFromCapturedBody(b []byte) (string, string) {
	if len(b) == 0 {
		return "", ""
	}
	trimmed := bytes.TrimSpace(b)
	// JSON?
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var r struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(trimmed, &r); err == nil && len(r.Choices) > 0 {
			return r.Choices[0].Message.Content, r.Choices[0].FinishReason
		}
	}
	// SSE stream — scan `data: {...}` lines.
	var sb strings.Builder
	var finish string
	scanner := bufio.NewScanner(bytes.NewReader(b))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var oc struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				Message *struct {
					Content string `json:"content"`
				} `json:"message,omitempty"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &oc) != nil {
			continue
		}
		if len(oc.Choices) == 0 {
			continue
		}
		if oc.Choices[0].Delta.Content != "" {
			sb.WriteString(oc.Choices[0].Delta.Content)
		} else if oc.Choices[0].Message != nil {
			sb.WriteString(oc.Choices[0].Message.Content)
		}
		if oc.Choices[0].FinishReason != nil {
			finish = *oc.Choices[0].FinishReason
		}
	}
	return sb.String(), finish
}

func anyToInt(v any) int {
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
