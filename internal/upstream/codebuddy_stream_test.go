package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestCBTransformTrailingAssistant verifies cbTransform appends a synthetic
// user turn when the last message is role=assistant. CodeBuddy rejects
// assistant-terminated histories with 11133 invalid_parameter_value.
func TestCBTransformTrailingAssistant(t *testing.T) {
	// Trailing assistant turns are now forwarded AS-IS (mirror cb2api).
	// The old sanitize ('[response interrupted]' + synthetic '[continue]')
	// made CodeBuddy cut tool-calling streams at exactly 25s — removed.
	msgs := []map[string]any{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "hi"},
		{"role": "assistant", "content": "done"},
	}
	body, _ := json.Marshal(map[string]any{"model": "claude-opus-5", "messages": msgs})
	out, err := cbTransform(body)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	all := m["messages"].([]any)
	if len(all) != len(msgs) {
		t.Fatalf("len = %d, want %d (messages must not be modified)", len(all), len(msgs))
	}
	last := all[len(all)-1].(map[string]any)
	if last["role"] != "assistant" || last["content"] != "done" {
		t.Fatalf("last = %v, want assistant 'done' unchanged", last)
	}
	// User-terminated history must NOT be modified.
	msgsU := append(msgs, map[string]any{"role": "user", "content": "next"})
	bodyU, _ := json.Marshal(map[string]any{"model": "claude-opus-5", "messages": msgsU})
	outU, _ := cbTransform(bodyU)
	var mU map[string]any
	json.Unmarshal(outU, &mU)
	if n := len(mU["messages"].([]any)); n != len(msgsU) {
		t.Fatalf("user-terminated len = %d, want %d", n, len(msgsU))
	}
}

// TestCBCollectStreamToolCalls verifies that streamed delta.tool_calls chunks
// (OpenAI format) are merged and re-attached to the rebuilt non-stream
// response. Regression: Hermes subagents got finish_reason="tool_calls" with
// an empty message because cbCollectStream dropped tool_calls → "empty
// response" failures.
func TestCBCollectStreamToolCalls(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"/etc/hostname\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"usage":{"prompt_tokens":10,"completion_tokens":5,"credit":0.01}}`,
		`data: [DONE]`,
	}, "\n")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse))
	}))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	got := cbCollectStream(resp, "claude-opus-5", nil)
	b, _ := json.Marshal(got)
	t.Logf("result: %s", b)

	choices, ok := got["choices"].([]gin.H)
	if !ok || len(choices) == 0 {
		t.Fatalf("no choices: %v", got)
	}
	msg, ok := choices[0]["message"].(gin.H)
	if !ok {
		t.Fatalf("no message: %v", choices[0])
	}
	if fr := choices[0]["finish_reason"]; fr != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", fr)
	}
	tcs, ok := msg["tool_calls"].([]gin.H)
	if !ok || len(tcs) != 1 {
		t.Fatalf("tool_calls missing/wrong: %#v", msg["tool_calls"])
	}
	tc := tcs[0]
	if tc["id"] != "call_abc" {
		t.Fatalf("id = %v", tc["id"])
	}
	fn, _ := tc["function"].(gin.H)
	if fn["name"] != "read_file" {
		t.Fatalf("fn name = %v", fn["name"])
	}
	wantArgs := `{"path":"/etc/hostname"}`
	if fn["arguments"] != wantArgs {
		t.Fatalf("arguments = %v, want %v", fn["arguments"], wantArgs)
	}
}
