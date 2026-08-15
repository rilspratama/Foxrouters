package upstream

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

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
	// Router-side default output cap: if the client sent NO cap at all
	// (neither max_tokens nor max_completion_tokens), set a sane default.
	// Reasoning models (claude-opus-5, gpt-5.x) burn output tokens on
	// reasoning_content first — with no cap, upstream defaults can truncate
	// the visible answer to reasoning-only. 32768 leaves room for both.
	if _, ok := m["max_completion_tokens"]; !ok {
		m["max_completion_tokens"] = 32768
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
	// Content-item sanitation: CodeBuddy 400 11101 "missing type field in
	// content item at index N" when a content array contains an item without
	// a "type" key (e.g. Anthropic-style {text:"..."} blocks). Drop such
	// items so a malformed client body fails cleanly instead of cascading.
	if msgs, ok := m["messages"].([]any); ok {
		for _, msg := range msgs {
			mm, ok := msg.(map[string]any)
			if !ok {
				continue
			}
			content, ok := mm["content"].([]any)
			if !ok {
				continue
			}
			filtered := make([]any, 0, len(content))
			for _, item := range content {
				im, isObj := item.(map[string]any)
				if !isObj || im["type"] != nil {
					filtered = append(filtered, item)
				}
			}
			if len(filtered) != len(content) {
				mm["content"] = filtered
			}
		}
		m["messages"] = msgs
	}

	// Multi-turn tool-history collapse. claude-opus-5 handles ordinary
	// agent-loop transcripts fine — verified live 2026-08-14: an 18-msg /
	// 800KB / 27-tool / reasoning_effort:high request returns normal
	// tool_calls, not reasoning-only. The failure mode appears only on
	// degenerate transcripts (very deep history with malformed trailing
	// assistant turns, content arrays without a type field). Collapsing
	// early is harmful: it forces a premature final answer ("I was cut
	// off") after the first tool round. So collapse is a SAFETY NET for
	// deep histories only (>16 msgs = 8+ tool rounds). The official CLI
	// manages session state itself and sends lean per-turn messages;
	// mirroring that only kicks in where upstream would otherwise choke.
	// Gated to claude-opus-5 — opus-4.7-1m and other models handle
	// multi-turn tool history fine and must NOT be collapsed.
	if model, ok := m["model"].(string); ok && isOpus5Model(model) {
		if msgs, ok := m["messages"].([]any); ok && len(msgs) > 16 && hasToolHistory(msgs) {
			m["messages"] = collapseToolHistory(msgs)
			// Drop tools when collapsing: the transcript already carries the
			// tool results, and opus-5 with tools + collapsed history gets
			// stuck in tool-calling mode (0 content, stream never finishes).
			// Verified live: collapsed-only → 9775ch full answer; +tools → 0.
			delete(m, "tools")
			// M1: tool_choice / parallel_tool_calls reference the deleted tool
			// list — OpenAI-compatible upstreams 400 on tool_choice w/o tools.
			delete(m, "tool_choice")
			delete(m, "parallel_tool_calls")
			// Drop the nested reasoning object + output caps too: opus-5 goes
			// reasoning-only when reasoning_effort AND max_completion_tokens
			// are both present on a collapsed request (verified live:
			// effort+maxcomp → 374ch; effort-only → 23834ch). The CLI sends
			// neither cap — mirror that.
			delete(m, "reasoning")
			delete(m, "max_completion_tokens")
			// M2: legacy/Anthropic-style clients send `max_tokens` — same cap
			// the comment above says must be removed.
			delete(m, "max_tokens")
			slog.Debug("collapsed multi-turn tool history for opus-5",
				"module", "cb", "orig_msgs", len(msgs))
		}
	}

	// Normalize reasoning params. CodeBuddy upstream only accepts
	// `reasoning_effort` (flat string: low/medium/high/xhigh). Clients send
	// different shapes:
	//   Hermes/OpenRouter: extra_body.reasoning = {enabled:true, effort:"high"}
	//   DeepSeek: thinking = {type:"enabled"}
	//   Qwen/ZAI: enable_thinking = true
	// Translate all of them to reasoning_effort if not already set.
	if _, hasRE := m["reasoning_effort"]; !hasRE {
		if r, ok := m["reasoning"].(map[string]any); ok {
			if enabled, ok := r["enabled"].(bool); ok && !enabled {
				// explicitly disabled — skip
			} else if effort, ok := r["effort"].(string); ok && effort != "" {
				m["reasoning_effort"] = effort
			} else {
				m["reasoning_effort"] = "medium"
			}
		} else if t, ok := m["thinking"].(map[string]any); ok {
			if tp, ok := t["type"].(string); ok && tp == "enabled" {
				m["reasoning_effort"] = "medium"
			}
		} else if et, ok := m["enable_thinking"].(bool); ok && et {
			m["reasoning_effort"] = "medium"
		}
	}
	// Also check nested extra_body.reasoning (OpenAI SDK wraps extra_body fields)
	if eb, ok := m["extra_body"].(map[string]any); ok {
		if _, hasRE := m["reasoning_effort"]; !hasRE {
			if r, ok := eb["reasoning"].(map[string]any); ok {
				if enabled, ok := r["enabled"].(bool); ok && !enabled {
					// explicitly disabled — skip
				} else if effort, ok := r["effort"].(string); ok && effort != "" {
					m["reasoning_effort"] = effort
				} else {
					m["reasoning_effort"] = "medium"
				}
			}
		}
	}
	return json.Marshal(m)
}

// hasToolHistory reports whether a message list contains a multi-turn
// assistant/tool exchange (role=tool, or assistant with non-empty tool_calls).
// isOpus5Model reports whether the model is claude-opus-5 in any form the
// router may carry (cb/ prefix, -1m suffix, case variants). Exact-match
// gating silently skipped those forms, re-exposing the collapse bug.
func isOpus5Model(model string) bool {
	n := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "cb/")
	return strings.HasPrefix(n, "claude-opus-5")
}

func hasToolHistory(messages []any) bool {
	for _, msg := range messages {
		mm, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		if mm["role"] == "tool" {
			return true
		}
		if mm["role"] == "assistant" {
			if tcs, ok := mm["tool_calls"].([]any); ok && len(tcs) > 0 {
				return true
			}
		}
	}
	return false
}

// collapseToolHistory flattens a multi-turn transcript into a single user
// message, mirroring how the official CodeBuddy CLI manages conversation
// state (it never dumps full history in one request). claude-opus-5 turns
// reasoning-only on large multi-turn tool histories — collapsing restores a
// final answer.
//
// System messages are preserved as a separate system turn: CodeBuddy rejects
// requests without a system message (11101 "Parse message failed"), and
// cbTransform's system injection runs BEFORE this collapse in the pipeline.
func collapseToolHistory(messages []any) []any {
	var sysParts []string
	var rest []any
	for _, msg := range messages {
		mm, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		if mm["role"] == "system" {
			if c := renderContentText(mm["content"]); c != "" {
				sysParts = append(sysParts, c)
			}
			continue
		}
		rest = append(rest, msg)
	}
	out := make([]any, 0, 2)
	if len(sysParts) > 0 {
		out = append(out, map[string]any{"role": "system", "content": strings.Join(sysParts, "\n\n")})
	}
	var sb strings.Builder
	for _, msg := range rest {
		mm, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		switch role {
		case "user":
			if c := renderContentText(mm["content"]); c != "" {
				sb.WriteString("[User]\n")
				sb.WriteString(c)
				sb.WriteString("\n\n")
			}
		case "assistant":
			if tcs, ok := mm["tool_calls"].([]any); ok && len(tcs) > 0 {
				for _, tc := range tcs {
					tcm, ok := tc.(map[string]any)
					if !ok {
						continue
					}
					fn, _ := tcm["function"].(map[string]any)
					name, _ := fn["name"].(string)
					args, _ := fn["arguments"].(string)
					sb.WriteString("[Assistant tool call] ")
					sb.WriteString(name)
					sb.WriteString("(")
					sb.WriteString(args)
					sb.WriteString(")\n\n")
				}
			} else if c := renderContentText(mm["content"]); c != "" {
				sb.WriteString("[Assistant]\n")
				sb.WriteString(c)
				sb.WriteString("\n\n")
			}
		case "tool":
			if c := renderContentText(mm["content"]); c != "" {
				sb.WriteString("[Tool result]\n")
				sb.WriteString(c)
				sb.WriteString("\n\n")
			}
		}
	}
	sb.WriteString("Based on the full conversation above, give your final answer now.")
	out = append(out, map[string]any{"role": "user", "content": sb.String()})
	return out
}

// renderContentText renders a message content field to plain text: string
// passthrough, content arrays → joined text items, anything else → "".
func renderContentText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, item := range c {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := im["type"].(string); ok && t == "text" {
				if txt, ok := im["text"].(string); ok {
					sb.WriteString(txt)
					sb.WriteString("\n")
				}
			}
		}
		return sb.String()
	default:
		return ""
	}
}

// cbCollectStream: read SSE stream → return single JSON (for non-stream clients).
func cbCollectStream(resp *http.Response, model string, key *CBKey) gin.H {
	defer resp.Body.Close()
	var content, reasoning strings.Builder
	var finish string
	var usage map[string]any
	var credit float64
	// toolCalls accumulates streamed delta.tool_calls (keyed by index),
	// merging partial function.arguments chunks like the OpenAI SDK expects.
	toolCalls := map[int]*struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}{}
	var toolIndexOrder []int

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
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			content.WriteString(chunk.Choices[0].Delta.Content)
			reasoning.WriteString(chunk.Choices[0].Delta.ReasoningContent)
			if chunk.Choices[0].FinishReason != "" {
				finish = chunk.Choices[0].FinishReason
			}
			for _, tc := range chunk.Choices[0].Delta.ToolCalls {
				cur, ok := toolCalls[tc.Index]
				if !ok {
					cur = &struct {
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					}{}
					toolCalls[tc.Index] = cur
					toolIndexOrder = append(toolIndexOrder, tc.Index)
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
	// Surface reasoning_content when the upstream emitted it (thinking enabled
	// via reasoning_effort/enable_thinking) — clients like Hermes display it.
	if r := reasoning.String(); r != "" {
		resp2["choices"].([]gin.H)[0]["message"].(gin.H)["reasoning_content"] = r
	}
	// Re-attach streamed tool_calls (OpenAI delta format) so non-stream clients
	// see the full tool call. Without this, finish_reason="tool_calls" arrives
	// with an empty message → clients treat it as an empty response.
	if len(toolCalls) > 0 {
		sort.Slice(toolIndexOrder, func(i, j int) bool { return toolIndexOrder[i] < toolIndexOrder[j] })
		tcs := make([]gin.H, 0, len(toolIndexOrder))
		for _, idx := range toolIndexOrder {
			tc := toolCalls[idx]
			tcs = append(tcs, gin.H{
				"id":       tc.ID,
				"type":     tc.Type,
				"function": gin.H{"name": tc.Function.Name, "arguments": tc.Function.Arguments},
			})
		}
		resp2["choices"].([]gin.H)[0]["message"].(gin.H)["tool_calls"] = tcs
	}
	if usage != nil {
		resp2["usage"] = usage
	}
	return resp2
}

// permanentDisable marks a key permanently disabled and persists via toDTO.
// The reason is persisted too (H3 provenance): meter/exhaustion disables must
// pass cbMeterDisableReason so the credit-sync worker can auto-lift them when
// the meter recovers; auth-failure reasons (401 etc.) fail closed forever.
func permanentDisable(key *CBKey, reason string) {
	key.mu.Lock()
	key.disabled = true
	key.disabledAt = time.Time{}
	key.disabledReason = reason
	key.mu.Unlock()
	if key.db != nil {
		saveCBKey(key.db, key.toDTO())
	}
	slog.Warn("key disabled", "module", "cb", "key", key.DisplayID(), "reason", reason)
}

// cooldownDisable marks a key with a temp cooldown and persists.
func cooldownDisable(key *CBKey, reason string) {
	key.mu.Lock()
	key.disabled = true
	key.disabledAt = time.Now()
	key.mu.Unlock()
	if key.db != nil {
		saveCBKey(key.db, key.toDTO())
	}
	slog.Warn("key disabled", "module", "cb", "key", key.DisplayID(), "reason", reason)
}
