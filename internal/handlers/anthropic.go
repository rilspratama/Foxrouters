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
	Thinking      json.RawMessage    `json:"thinking,omitempty"`   // {type:"enabled", budget_tokens:N} or {type:"adaptive"}
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentBlockIn: one block from an Anthropic content-array message.
// Supports text, tool_use (assistant messages), and tool_result (user messages).
type contentBlockIn struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// tool_use fields (appear in assistant messages)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result fields (appear in user messages)
	ToolUseID     string          `json:"tool_use_id,omitempty"`
	ToolContent   json.RawMessage `json:"content,omitempty"`
	IsError       bool            `json:"is_error,omitempty"`
}

// ============================================================================
// RESPONSE TYPES (Anthropic Messages API)
// ============================================================================

type anthropicResponse struct {
	ID           string                     `json:"id"`
	Type         string                     `json:"type"`
	Role         string                     `json:"role"`
	Model        string                     `json:"model"`
	Content      []anthropicContentBlockOut `json:"content"`
	StopReason   string                     `json:"stop_reason"`
	StopSequence *string                    `json:"stop_sequence"`
	Usage        anthropicUsage             `json:"usage"`
}

// anthropicContentBlockOut is emitted in Anthropic responses. Supports
// thinking, text, and tool_use blocks. Fields are omitted based on Type.
type anthropicContentBlockOut struct {
	Type string `json:"type"` // "thinking", "text", or "tool_use"
	// text-block fields
	Text string `json:"text,omitempty"`
	// thinking-block fields
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	// tool_use-block fields
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
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
	// Explicit prefixes → passthrough.
	if strings.HasPrefix(m, "cb/") || strings.HasPrefix(m, "grok-") {
		return m
	}
	lower := strings.ToLower(m)
	// Escape hatch: "...-grok" or "grok" in the name → route to Grok.
	if strings.Contains(lower, "grok") {
		return "grok-4.5"
	}
	// Bare model names (glm-5.2, gpt-5.5, kimi-k3, etc.) — passthrough.
	// The proxy's proxyRequest handles routing: non-grok-* → CodeBuddy.
	// Only fall through to default if it looks like a Claude model name
	// (claude-*) that needs translation to a CodeBuddy upstream.
	if !strings.HasPrefix(lower, "claude-") {
		return m
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

// extractToolResultContent flattens Anthropic tool_result content into a
// string suitable for OpenAI's role:tool message. Anthropic tool_result
// content can be a string or an array of {type:"text", text:"..."} blocks.
func extractToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
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
	// Last-ditch: marshal the raw JSON back to string.
	return string(raw)
}

// translateMessages converts an Anthropic messages array to OpenAI messages,
// handling text, tool_use (assistant → tool_calls), and tool_result
// (user → role:tool). One Anthropic message may fan out into multiple OpenAI
// messages (e.g. a user message with several tool_result blocks becomes
// several role:tool messages).
func translateMessages(msgs []anthropicMessage) []map[string]any {
	out := make([]map[string]any, 0, len(msgs)+1)
	for _, m := range msgs {
		// Try string content first — most common shape.
		var plain string
		if err := json.Unmarshal(m.Content, &plain); err == nil {
			out = append(out, map[string]any{"role": m.Role, "content": plain})
			continue
		}
		// Array of content blocks.
		var blocks []contentBlockIn
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			// Fallback: stringify.
			out = append(out, map[string]any{"role": m.Role, "content": extractText(m.Content)})
			continue
		}

		if m.Role == "user" {
			// Split into a text portion + any tool_result blocks.
			var textParts []string
			var toolResults []map[string]any
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if b.Text != "" {
						textParts = append(textParts, b.Text)
					}
				case "tool_result":
					tr := map[string]any{
						"role":         "tool",
						"tool_call_id": b.ToolUseID,
						"content":      extractToolResultContent(b.ToolContent),
					}
					toolResults = append(toolResults, tr)
				}
			}
			if len(textParts) > 0 {
				out = append(out, map[string]any{"role": "user", "content": strings.Join(textParts, "\n")})
			}
			out = append(out, toolResults...)
			continue
		}

		if m.Role == "assistant" {
			// Text goes in content; tool_use blocks become tool_calls.
			var textParts []string
			var toolCalls []map[string]any
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if b.Text != "" {
						textParts = append(textParts, b.Text)
					}
				case "tool_use":
					// input is a JSON object; OpenAI wants function.arguments as a JSON string.
					args := string(b.Input)
					if args == "" {
						args = "{}"
					}
					toolCalls = append(toolCalls, map[string]any{
						"id":   b.ID,
						"type": "function",
						"function": map[string]any{
							"name":      b.Name,
							"arguments": args,
						},
					})
				}
			}
			asst := map[string]any{"role": "assistant"}
			if len(textParts) > 0 {
				asst["content"] = strings.Join(textParts, "\n")
			} else {
				asst["content"] = nil
			}
			if len(toolCalls) > 0 {
				asst["tool_calls"] = toolCalls
			}
			out = append(out, asst)
			continue
		}

		// Unknown role — best-effort text.
		out = append(out, map[string]any{"role": m.Role, "content": extractText(m.Content)})
	}
	return out
}

// translateTools converts Anthropic tools array to OpenAI tools array.
// Anthropic: [{name, description, input_schema}]
// OpenAI:    [{type:"function", function:{name, description, parameters}}]
func translateTools(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var atools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		InputSchema json.RawMessage `json:"input_schema,omitempty"`
	}
	if err := json.Unmarshal(raw, &atools); err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(atools))
	for _, t := range atools {
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if len(t.InputSchema) > 0 {
			fn["parameters"] = json.RawMessage(t.InputSchema)
		} else {
			// OpenAI wants an object schema; empty ok.
			fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// translateToolChoice converts Anthropic tool_choice to OpenAI tool_choice.
// Anthropic shapes:
//   {"type":"auto"}                          → "auto"
//   {"type":"any"}                           → "required"
//   {"type":"none"}                          → "none"
//   {"type":"tool", "name":"get_weather"}    → {"type":"function","function":{"name":"get_weather"}}
// Also accepts a bare string ("auto"/"any"/"none").
func translateToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "any":
			return "required"
		case "auto", "none":
			return s
		}
		return "auto"
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name,omitempty"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	switch obj.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if obj.Name == "" {
			return "auto"
		}
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": obj.Name},
		}
	}
	return "auto"
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

	msgs = append(msgs, translateMessages(req.Messages)...)

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
	if tools := translateTools(req.Tools); len(tools) > 0 {
		out["tools"] = tools
	}
	if tc := translateToolChoice(req.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}

	// Translate Anthropic thinking config → OpenAI reasoning_effort.
	// Anthropic: {"type":"enabled","budget_tokens":N} or {"type":"adaptive"}
	// OpenAI:    "reasoning_effort":"high" (or medium/low)
	if len(req.Thinking) > 0 {
		var think struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		}
		if json.Unmarshal(req.Thinking, &think) == nil {
			switch {
			case think.Type == "enabled" && think.BudgetTokens >= 16000:
				out["reasoning_effort"] = "high"
			case think.Type == "enabled" && think.BudgetTokens >= 8000:
				out["reasoning_effort"] = "medium"
			case think.Type == "enabled":
				out["reasoning_effort"] = "low"
			case think.Type == "adaptive":
				out["reasoning_effort"] = "high"
			}
		}
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
	real          gin.ResponseWriter
	flusher       http.Flusher
	msgID         string
	model         string
	started       bool // message_start already sent
	thinkingOpen  bool // content_block_start for thinking (index 0) already sent
	thinkingClosed bool // content_block_stop for thinking emitted
	textOpen      bool // content_block_start for text already sent
	textClosed    bool // content_block_stop for text emitted
	textBlockIdx  int  // 0 when no thinking, 1 when thinking present
	stopped       bool // message_stop already sent
	finish        string
	inputToks     int
	outputToks    int
	textBuf       strings.Builder
	carry         string // partial line from previous Write
	errBuf        []byte // upstream error body captured before streaming started
	// Tool-use tracking. OpenAI streams tool_calls with an `index` field
	// (0-based within tool_calls array). We map each OpenAI tool_call index
	// to a distinct Anthropic content_block index (starting after the text
	// block). We buffer arguments per tool so we can emit input_json_delta
	// as they stream in.
	toolBlocks  map[int]*streamToolBlock
	nextBlockIdx int // next Anthropic content-block index to assign
	// Headers set by the downstream proxy — we don't forward them; we set our own.
	sinkHeader http.Header
	statusCode int
}

// streamToolBlock tracks one in-progress tool_use block.
type streamToolBlock struct {
	anthropicIdx int    // content_block index in the Anthropic stream
	id           string // tool_use id
	name         string // function name
	started      bool   // content_block_start emitted
	closed       bool   // content_block_stop emitted
}

func newStreamWriter(real gin.ResponseWriter, msgID, model string) *streamWriter {
	fl, _ := real.(http.Flusher)
	return &streamWriter{
		ResponseWriter: real,
		real:            real,
		flusher:         fl,
		msgID:           msgID,
		model:           model,
		toolBlocks:      make(map[int]*streamToolBlock),
		textBlockIdx:    0, // no thinking yet → text gets index 0
		nextBlockIdx:    1, // tools start at 1
		sinkHeader:      http.Header{},
		statusCode:      200,
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

// ensureStart emits message_start on the wire, once. The text content_block
// is opened lazily by ensureTextBlock() when the first text delta arrives;
// tool_use blocks are opened by ensureToolBlock().
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
}

// ensureThinkingBlock emits content_block_start for the thinking block (index 0)
// exactly once, right before the first thinking_delta. Once thinking is opened,
// the text block shifts to index 1 and tool blocks shift accordingly.
func (w *streamWriter) ensureThinkingBlock() {
	if w.thinkingOpen {
		return
	}
	w.thinkingOpen = true
	// Thinking always gets index 0. Text and tools shift up by 1.
	w.textBlockIdx = 1
	w.nextBlockIdx = 2
	w.emitEvent("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]any{
			"type":      "thinking",
			"thinking":  "",
			"signature": "",
		},
	})
}

// closeThinkingBlock emits content_block_stop for the thinking block if open
// and not already closed. Idempotent.
func (w *streamWriter) closeThinkingBlock() {
	if !w.thinkingOpen || w.thinkingClosed {
		return
	}
	w.thinkingClosed = true
	w.emitEvent("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": 0,
	})
}

// ensureTextBlock emits content_block_start for the text block
// exactly once, right before the first text_delta. The index is 0 when no
// thinking block was opened, or 1 when thinking precedes it.
func (w *streamWriter) ensureTextBlock() {
	if w.textOpen {
		return
	}
	// Before opening the text block, close the thinking block if it's open.
	w.closeThinkingBlock()
	w.textOpen = true
	w.emitEvent("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         w.textBlockIdx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

// closeTextBlock emits content_block_stop for the text block if it was opened
// and not already closed. Idempotent.
func (w *streamWriter) closeTextBlock() {
	if !w.textOpen || w.textClosed {
		return
	}
	w.textClosed = true
	w.emitEvent("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": w.textBlockIdx,
	})
}

// ensureToolBlock returns (and lazily creates) the streamToolBlock for the
// given OpenAI tool_call index. Emits content_block_start on first sight.
// id/name may arrive on the first delta or piecemeal — we track whichever
// arrives first and open the block once we have a name (OpenAI always sends
// name+id in the first delta of a tool_call).
func (w *streamWriter) ensureToolBlock(openaiIdx int, id, name string) *streamToolBlock {
	tb, ok := w.toolBlocks[openaiIdx]
	if !ok {
		tb = &streamToolBlock{anthropicIdx: w.nextBlockIdx}
		w.nextBlockIdx++
		w.toolBlocks[openaiIdx] = tb
	}
	if id != "" && tb.id == "" {
		tb.id = id
	}
	if name != "" && tb.name == "" {
		tb.name = name
	}
	if !tb.started && tb.name != "" {
		// Before opening a tool block, ensure the text block is closed
		// (Anthropic requires strictly ordered content blocks).
		w.closeTextBlock()
		tb.started = true
		w.emitEvent("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": tb.anthropicIdx,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    tb.id,
				"name":  tb.name,
				"input": map[string]any{},
			},
		})
	}
	return tb
}

// closeToolBlock emits content_block_stop for a tool block. Idempotent.
func (w *streamWriter) closeToolBlock(tb *streamToolBlock) {
	if tb == nil || !tb.started || tb.closed {
		return
	}
	tb.closed = true
	w.emitEvent("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": tb.anthropicIdx,
	})
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
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Role             string `json:"role"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id,omitempty"`
					Type     string `json:"type,omitempty"`
					Function struct {
						Name      string `json:"name,omitempty"`
						Arguments string `json:"arguments,omitempty"`
					} `json:"function"`
				} `json:"tool_calls,omitempty"`
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
	// Reasoning/thinking deltas — emitted as thinking_delta events.
	// CodeBuddy/GLM/DeepSeek/Kimi all use reasoning_content in streaming deltas.
	if ch.Delta.ReasoningContent != "" {
		w.ensureStart()
		w.ensureThinkingBlock()
		w.emitEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0, // thinking is always index 0
			"delta": map[string]any{"type": "thinking_delta", "thinking": ch.Delta.ReasoningContent},
		})
	}
	if ch.Delta.Content != "" {
		w.ensureStart()
		w.ensureTextBlock()
		w.textBuf.WriteString(ch.Delta.Content)
		w.emitEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": w.textBlockIdx,
			"delta": map[string]any{"type": "text_delta", "text": ch.Delta.Content},
		})
	}
	// Tool-call deltas. OpenAI streams these piecewise: the first delta
	// carries id/name, subsequent deltas carry function.arguments chunks.
	for _, tc := range ch.Delta.ToolCalls {
		w.ensureStart()
		tb := w.ensureToolBlock(tc.Index, tc.ID, tc.Function.Name)
		if tc.Function.Arguments != "" && tb.started {
			w.emitEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": tb.anthropicIdx,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": tc.Function.Arguments,
				},
			})
		}
	}
	if ch.FinishReason != nil && *ch.FinishReason != "" {
		w.finish = *ch.FinishReason
	}
}

// finalize emits content_block_stop for every open block + message_delta + message_stop.
func (w *streamWriter) finalize() {
	if w.stopped {
		return
	}
	w.ensureStart() // in case upstream sent no content, still emit shell
	// If nothing at all was emitted (no thinking, no text, no tools), open an
	// empty text block so the client sees a valid single-block message.
	if !w.thinkingOpen && !w.textOpen && len(w.toolBlocks) == 0 {
		w.ensureTextBlock()
	}
	w.stopped = true

	// Close blocks in Anthropic order: thinking (index 0) → text → tools.
	w.closeThinkingBlock()
	w.closeTextBlock()
	// Iterate tool blocks in ascending anthropicIdx order for deterministic output.
	// (Range over map is unordered — collect + sort by index.)
	if len(w.toolBlocks) > 0 {
		ordered := make([]*streamToolBlock, 0, len(w.toolBlocks))
		for _, tb := range w.toolBlocks {
			ordered = append(ordered, tb)
		}
		// Simple insertion sort — expected tiny N.
		for i := 1; i < len(ordered); i++ {
			for j := i; j > 0 && ordered[j-1].anthropicIdx > ordered[j].anthropicIdx; j-- {
				ordered[j-1], ordered[j] = ordered[j], ordered[j-1]
			}
		}
		for _, tb := range ordered {
			w.closeToolBlock(tb)
		}
	}
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
//
// Applies to any /v1/* path — Anthropic clients call GET /v1/models,
// POST /v1/messages, etc., all with x-api-key.
func AnthropicAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Only touch /v1/* — leave everything else alone.
		if !strings.HasPrefix(c.Request.URL.Path, "/v1/") {
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
func HandleMessages(grokAM *upstream.GrokAccountManager, cbKM *upstream.CBKeyManager, hc *upstream.HealthChecker, authMgr *auth.Manager, reg *proxy.CustomRegistry, combos *proxy.ComboRegistry) gin.HandlerFunc {
	inner := proxy.ProxyRequest(grokAM, cbKM, hc, authMgr, reg, combos)

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
			outputText  string
			inputToks   int
			outputToks  int
			finish      string
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

		// Prefer the proxy's stored response_body (set via c.Set in both
		// stream and non-stream paths) — it has full-fidelity JSON. Fall
		// back to the captured writer buffer.
		var bodyBytes []byte
		if rb, ok := c.Get("response_body"); ok {
			if rm, ok := rb.(json.RawMessage); ok {
				bodyBytes = []byte(rm)
			}
		}
		if len(bodyBytes) == 0 {
			bodyBytes = cap.buf.Bytes()
		}

		// Upstream error — surface to client as Anthropic error envelope.
		if cap.status >= 400 {
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

		parsedText, parsedFinish, reasoningText, toolCalls := extractFromCapturedBody(bodyBytes)
		if outputText == "" || (strings.HasSuffix(outputText, "…") && parsedText != "") {
			outputText = parsedText
		}
		if finish == "" {
			finish = parsedFinish
		}

		// Build content blocks in Anthropic order: thinking → text → tool_use.
		blocks := make([]anthropicContentBlockOut, 0, 1+len(toolCalls))
		if reasoningText != "" {
			blocks = append(blocks, anthropicContentBlockOut{
				Type:      "thinking",
				Thinking:  reasoningText,
				Signature: "",
			})
		}
		if outputText != "" {
			blocks = append(blocks, anthropicContentBlockOut{Type: "text", Text: outputText})
		}
		for _, tc := range toolCalls {
			// arguments is a JSON string; keep it as RawMessage so it emits
			// as an object (not a string) in the Anthropic response.
			var input json.RawMessage
			args := strings.TrimSpace(tc.Function.Arguments)
			if args == "" {
				input = json.RawMessage("{}")
			} else if json.Valid([]byte(args)) {
				input = json.RawMessage(args)
			} else {
				// Malformed args from upstream — pass through as a JSON string.
				b, _ := json.Marshal(args)
				input = json.RawMessage(b)
			}
			blocks = append(blocks, anthropicContentBlockOut{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: input,
			})
		}
		// Fallback: ensure at least one content block so the response is
		// syntactically valid.
		if len(blocks) == 0 {
			blocks = append(blocks, anthropicContentBlockOut{Type: "text", Text: ""})
		}
		// If a tool call was produced but the upstream didn't emit finish_reason,
		// map it to tool_use.
		if len(toolCalls) > 0 && finish == "" {
			finish = "tool_calls"
		}

		resp := anthropicResponse{
			ID:         msgID,
			Type:       "message",
			Role:       "assistant",
			Model:      clientModelName,
			Content:    blocks,
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

// openAIToolCallOut mirrors OpenAI's tool_call shape in a non-stream response.
// Used by extractFromCapturedBody.
type openAIToolCallOut struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// extractFromCapturedBody parses either a JSON chat.completion body OR a
// buffered SSE stream and returns (text, finish_reason, tool_calls).
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

func extractFromCapturedBody(b []byte) (text, finishReason, reasoning string, toolCalls []openAIToolCallOut) {
	if len(b) == 0 {
		return "", "", "", nil
	}
	trimmed := bytes.TrimSpace(b)
	// JSON?
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var r struct {
			Choices []struct {
				Message struct {
					Content          string              `json:"content"`
					ReasoningContent string              `json:"reasoning_content"`
					ToolCalls        []openAIToolCallOut `json:"tool_calls,omitempty"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(trimmed, &r); err == nil && len(r.Choices) > 0 {
			return r.Choices[0].Message.Content,
				r.Choices[0].FinishReason,
				r.Choices[0].Message.ReasoningContent,
				r.Choices[0].Message.ToolCalls
		}
	}
	// SSE stream — scan `data: {...}` lines. Tool calls in SSE arrive as
	// piecewise Delta.ToolCalls[i].{id,name,function.arguments}; reassemble.
	type tcAcc struct {
		id, name string
		args     strings.Builder
	}
	tcMap := map[int]*tcAcc{}
	var tcOrder []int
	var sb strings.Builder
	var rcb strings.Builder
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
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id,omitempty"`
						Function struct {
							Name      string `json:"name,omitempty"`
							Arguments string `json:"arguments,omitempty"`
						} `json:"function"`
					} `json:"tool_calls,omitempty"`
				} `json:"delta"`
				Message *struct {
					Content          string              `json:"content"`
					ReasoningContent string              `json:"reasoning_content"`
					ToolCalls        []openAIToolCallOut `json:"tool_calls,omitempty"`
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
		if oc.Choices[0].Delta.ReasoningContent != "" {
			rcb.WriteString(oc.Choices[0].Delta.ReasoningContent)
		}
		if oc.Choices[0].Delta.Content != "" {
			sb.WriteString(oc.Choices[0].Delta.Content)
		} else if oc.Choices[0].Message != nil {
			sb.WriteString(oc.Choices[0].Message.Content)
			if oc.Choices[0].Message.ReasoningContent != "" {
				rcb.WriteString(oc.Choices[0].Message.ReasoningContent)
			}
			// A full message block may also carry final tool_calls.
			for _, tc := range oc.Choices[0].Message.ToolCalls {
				idx := len(tcOrder)
				acc := &tcAcc{id: tc.ID, name: tc.Function.Name}
				acc.args.WriteString(tc.Function.Arguments)
				tcMap[idx] = acc
				tcOrder = append(tcOrder, idx)
			}
		}
		for _, tc := range oc.Choices[0].Delta.ToolCalls {
			acc, ok := tcMap[tc.Index]
			if !ok {
				acc = &tcAcc{}
				tcMap[tc.Index] = acc
				tcOrder = append(tcOrder, tc.Index)
			}
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if tc.Function.Name != "" {
				acc.name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				acc.args.WriteString(tc.Function.Arguments)
			}
		}
		if oc.Choices[0].FinishReason != nil {
			finish = *oc.Choices[0].FinishReason
		}
	}
	var tcs []openAIToolCallOut
	for _, idx := range tcOrder {
		acc := tcMap[idx]
		if acc == nil {
			continue
		}
		var t openAIToolCallOut
		t.ID = acc.id
		t.Type = "function"
		t.Function.Name = acc.name
		t.Function.Arguments = acc.args.String()
		tcs = append(tcs, t)
	}
	return sb.String(), finish, rcb.String(), tcs
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
