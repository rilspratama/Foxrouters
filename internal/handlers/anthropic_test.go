// Anthropic adapter unit tests — request/response translation only.
// These are pure functions, so they can be exercised without spinning up
// upstream managers or a Gin router.
package handlers

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildOpenAIBody_ToolsAndToolChoice(t *testing.T) {
	req := &anthropicRequest{
		Model:     "cb/claude-sonnet-4.6",
		MaxTokens: 100,
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`"what's the weather in SF?"`)},
		},
		Tools: json.RawMessage(`[
			{"name":"get_weather","description":"Get current weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}
		]`),
		ToolChoice: json.RawMessage(`{"type":"any"}`),
	}
	body, _, err := buildOpenAIBody(req, nil)
	if err != nil {
		t.Fatalf("buildOpenAIBody error: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tools, ok := out["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("expected 1 tool in body; got %v", out["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("tool.type = %v; want 'function'", tool["type"])
	}
	fn := tool["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("tool.function.name = %v; want get_weather", fn["name"])
	}
	if _, ok := fn["parameters"]; !ok {
		t.Errorf("tool.function.parameters missing (input_schema wasn't translated)")
	}
	// tool_choice: "any" → "required"
	if out["tool_choice"] != "required" {
		t.Errorf("tool_choice = %v; want 'required'", out["tool_choice"])
	}
}

func TestTranslateToolChoice_SpecificTool(t *testing.T) {
	got := translateToolChoice(json.RawMessage(`{"type":"tool","name":"get_weather"}`))
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("expected map; got %T (%v)", got, got)
	}
	if m["type"] != "function" {
		t.Errorf("type = %v; want function", m["type"])
	}
	fn := m["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("function.name = %v", fn["name"])
	}
}

func TestTranslateMessages_ToolResultAndToolUse(t *testing.T) {
	msgs := []anthropicMessage{
		{Role: "user", Content: json.RawMessage(`"weather in SF?"`)},
		{Role: "assistant", Content: json.RawMessage(`[
			{"type":"text","text":"Let me check."},
			{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}
		]`)},
		{Role: "user", Content: json.RawMessage(`[
			{"type":"tool_result","tool_use_id":"toolu_1","content":"72F sunny"}
		]`)},
	}
	out := translateMessages(msgs)
	if len(out) < 3 {
		t.Fatalf("expected >=3 output messages; got %d: %v", len(out), out)
	}
	// out[0]: user text
	if out[0]["role"] != "user" || out[0]["content"] != "weather in SF?" {
		t.Errorf("msg[0] = %v", out[0])
	}
	// out[1]: assistant with tool_calls
	asst := out[1]
	if asst["role"] != "assistant" {
		t.Fatalf("msg[1].role = %v", asst["role"])
	}
	if asst["content"] != "Let me check." {
		t.Errorf("msg[1].content = %v", asst["content"])
	}
	tcs, ok := asst["tool_calls"].([]map[string]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("msg[1].tool_calls = %v (type %T)", asst["tool_calls"], asst["tool_calls"])
	}
	if tcs[0]["id"] != "toolu_1" {
		t.Errorf("tool_call.id = %v", tcs[0]["id"])
	}
	fn := tcs[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("tool_call.function.name = %v", fn["name"])
	}
	if !strings.Contains(fn["arguments"].(string), "SF") {
		t.Errorf("tool_call.function.arguments should carry input JSON; got %v", fn["arguments"])
	}
	// out[2]: role=tool
	tr := out[2]
	if tr["role"] != "tool" {
		t.Fatalf("msg[2].role = %v", tr["role"])
	}
	if tr["tool_call_id"] != "toolu_1" {
		t.Errorf("tool_call_id = %v", tr["tool_call_id"])
	}
	if tr["content"] != "72F sunny" {
		t.Errorf("tool_result content = %v", tr["content"])
	}
}

func TestExtractFromCapturedBody_ToolCalls_JSON(t *testing.T) {
	body := []byte(`{
		"choices":[{
			"message":{
				"content":"",
				"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]
			},
			"finish_reason":"tool_calls"
		}]
	}`)
	text, finish, _, tcs := extractFromCapturedBody(body)
	if text != "" {
		t.Errorf("text = %q; want empty", text)
	}
	if finish != "tool_calls" {
		t.Errorf("finish = %v", finish)
	}
	if len(tcs) != 1 {
		t.Fatalf("tool_calls len = %d", len(tcs))
	}
	if tcs[0].ID != "call_abc" || tcs[0].Function.Name != "get_weather" {
		t.Errorf("unexpected tool_call: %+v", tcs[0])
	}
	if !strings.Contains(tcs[0].Function.Arguments, "SF") {
		t.Errorf("arguments = %q", tcs[0].Function.Arguments)
	}
}

func TestExtractFromCapturedBody_ToolCalls_SSE(t *testing.T) {
	// Simulated OpenAI-format SSE stream with tool_calls arriving piecewise.
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","content":""}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_xyz","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"SF\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	_, finish, _, tcs := extractFromCapturedBody([]byte(sse))
	if finish != "tool_calls" {
		t.Errorf("finish = %v", finish)
	}
	if len(tcs) != 1 {
		t.Fatalf("tool_calls len = %d (%+v)", len(tcs), tcs)
	}
	if tcs[0].ID != "call_xyz" {
		t.Errorf("id = %v", tcs[0].ID)
	}
	if tcs[0].Function.Name != "get_weather" {
		t.Errorf("name = %v", tcs[0].Function.Name)
	}
	want := `{"city":"SF"}`
	if tcs[0].Function.Arguments != want {
		t.Errorf("arguments = %q; want %q", tcs[0].Function.Arguments, want)
	}
}

func TestMapStopReason_ToolUse(t *testing.T) {
	if got := mapStopReason("tool_calls"); got != "tool_use" {
		t.Errorf("tool_calls → %v; want tool_use", got)
	}
}
