package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCollapseToolHistory(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "You are a reviewer."},
		map[string]any{"role": "user", "content": "Review this file"},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"function": map[string]any{"name": "read_file", "arguments": `{"path":"a.go"}`}},
		}},
		map[string]any{"role": "tool", "content": "func main() {}"},
		map[string]any{"role": "assistant", "content": "Found it"},
	}
	if !hasToolHistory(msgs) {
		t.Fatal("expected tool history detected")
	}
	out := collapseToolHistory(msgs)
	if len(out) != 2 {
		t.Fatalf("expected 2 messages (system+user), got %d", len(out))
	}
	m := out[len(out)-1].(map[string]any)
	if m["role"] != "user" {
		t.Fatalf("expected user role, got %v", m["role"])
	}
	s, _ := m["content"].(string)
	for _, want := range []string{"[User]", "[Assistant tool call] read_file", "[Tool result]", "final answer"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in collapsed content", want)
		}
	}
	if strings.Contains(s, "[System]") {
		t.Fatalf("system should NOT be embedded in user content, got [System] in: %.100s", s)
	}
	sys := out[0].(map[string]any)
	if sys["role"] != "system" {
		t.Fatalf("expected first message to be system, got %v", sys["role"])
	}
	sysc, _ := sys["content"].(string)
	if !strings.Contains(sysc, "You are a reviewer.") {
		t.Fatalf("system content lost: %q", sysc)
	}
	// no tool history → NOT collapsed (plain passthrough)
	plain := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "hello"},
	}
	if hasToolHistory(plain) {
		t.Fatal("plain conversation should not have tool history")
	}
}

// TestCbTransformCollapseDropsTools: full transform on an opus-5 body with
// DEEP multi-turn tool history (>16 msgs) must collapse AND drop tools
// (else opus-5 gets stuck in tool-calling mode — verified live 0 content
// with tools present). Shallow histories (<16 msgs) must NOT collapse —
// opus-5 handles ordinary agent loops fine, early collapse forces a
// premature "I was cut off" final answer (verified live 2026-08-14).
func TestCbTransformCollapseDropsTools(t *testing.T) {
	deep := []string{
		`{"role":"system","content":"sys"}`,
		`{"role":"user","content":"review this"}`,
	}
	for i := 0; i < 7; i++ {
		deep = append(deep,
			`{"role":"assistant","content":"","tool_calls":[{"function":{"name":"read_file","arguments":"{}"}}]}`,
			`{"role":"tool","content":"func main(){}"}`,
		)
	}
	deep = append(deep, `{"role":"assistant","content":"done"}`, `{"role":"user","content":"final"}`)
	// 2 + 14 + 2 = 18 msgs > 16 → collapse expected
	body := []byte(`{"model":"claude-opus-5","messages":[` + strings.Join(deep, ",") + `],"tools":[{"type":"function","function":{"name":"read_file","description":"r","parameters":{"type":"object","properties":{}}}}]}`)
	out, err := cbTransform(body)
	if err != nil {
		t.Fatalf("cbTransform: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, has := m["tools"]; has {
		t.Fatal("tools should be dropped after collapse")
	}
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected [system, user], got %d msgs", len(msgs))
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatal("first message should be system")
	}
	if msgs[1].(map[string]any)["role"] != "user" {
		t.Fatal("second message should be user")
	}
	// shallow history (6 msgs, 1 tool round) → NO collapse, tools preserved
	shallow := []byte(`{"model":"claude-opus-5","messages":[
		{"role":"system","content":"sys"},
		{"role":"user","content":"review this"},
		{"role":"assistant","content":"","tool_calls":[{"function":{"name":"read_file","arguments":"{}"}}]},
		{"role":"tool","content":"func main(){}"},
		{"role":"assistant","content":"done"},
		{"role":"user","content":"final"}
	],"tools":[{"type":"function","function":{"name":"read_file","description":"r","parameters":{"type":"object","properties":{}}}}]}`)
	outS, err := cbTransform(shallow)
	if err != nil {
		t.Fatalf("cbTransform shallow: %v", err)
	}
	var mS map[string]any
	json.Unmarshal(outS, &mS)
	if _, has := mS["tools"]; !has {
		t.Fatal("tools should be preserved for shallow history (no collapse)")
	}
	mSgs, _ := mS["messages"].([]any)
	if len(mSgs) != 6 {
		t.Fatalf("shallow history should pass through un-collapsed, got %d msgs", len(mSgs))
	}
	// non-opus-5 model: NO collapse, tools preserved (even deep)
	body2 := []byte(`{"model":"claude-opus-4.7-1m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"","tool_calls":[{"function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","content":"r"}
	],"tools":[{"type":"function","function":{"name":"f"}}]}`)
	out2, err := cbTransform(body2)
	if err != nil {
		t.Fatalf("cbTransform2: %v", err)
	}
	var m2 map[string]any
	json.Unmarshal(out2, &m2)
	if _, has := m2["tools"]; !has {
		t.Fatal("tools should be preserved for non-opus-5 models")
	}
}

func TestRenderContentText(t *testing.T) {
	if got := renderContentText("plain"); got != "plain" {
		t.Fatalf("string case: %q", got)
	}
	arr := []any{
		map[string]any{"type": "text", "text": "hello "},
		map[string]any{"type": "text", "text": "world"},
		map[string]any{"type": "image_url"},
	}
	if got := renderContentText(arr); got != "hello \nworld\n" {
		t.Fatalf("array case: %q", got)
	}
	if got := renderContentText(123); got != "" {
		t.Fatalf("other case: %q", got)
	}
}
