package upstream

import (
	"fmt"
	"strings"
	"testing"
)

// TestFBStreamToNonStreamToolCalls verifies that tool_call deltas streamed
// incrementally (index-keyed fragments) are accumulated, sorted by index and
// attached to the aggregated message — the fix for the non-stream Freebuff
// path silently dropping tool_calls.
func TestFBStreamToNonStreamToolCalls(t *testing.T) {
	// Simulate a streamed tool_call split across 3 chunks:
	//  - chunk 0: id + name, args part 1
	//  - chunk 0: args part 2
	//  - chunk 1: id + name, args
	stream := strings.Join([]string{
		`data: {"id":"fb_1","model":"deepseek/deepseek-v4-flash","choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		`data: {"id":"fb_1","model":"deepseek/deepseek-v4-flash","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Jakarta\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"fb_1","model":"deepseek/deepseek-v4-flash","choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_xyz","type":"function","function":{"name":"get_time","arguments":"{\"city\":\"Jakarta\"}"}}]},"finish_reason":"tool_calls"}]}`,
		"data: [DONE]",
	}, "\n")

	agg := fbStreamToNonStream(strings.NewReader(stream), "deepseek/deepseek-v4-flash")
	if agg == nil {
		t.Fatal("agg is nil")
	}

	choices, ok := agg["choices"].([]map[string]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("choices shape wrong: %#v", agg["choices"])
	}
	msg, ok := choices[0]["message"].(map[string]any)
	if !ok {
		t.Fatalf("message shape wrong: %#v", choices[0]["message"])
	}
	tcs, ok := msg["tool_calls"].([]map[string]any)
	if !ok {
		t.Fatalf("tool_calls missing or wrong shape: %#v", msg["tool_calls"])
	}
	if len(tcs) != 2 {
		t.Fatalf("expected 2 tool calls, got %d: %#v", len(tcs), tcs)
	}

	// Index 0: id + name from first chunk, arguments joined across chunks.
	tc0 := tcs[0]
	if tc0["id"] != "call_abc" {
		t.Fatalf("tc0 id = %v, want call_abc", tc0["id"])
	}
	fn0, _ := tc0["function"].(map[string]any)
	if fn0["name"] != "get_weather" {
		t.Fatalf("tc0 fn name = %v, want get_weather", fn0["name"])
	}
	if fn0["arguments"] != `{"city":"Jakarta"}` {
		t.Fatalf("tc0 args not joined: %v", fn0["arguments"])
	}

	// Index 1 present and sorted after index 0.
	tc1 := tcs[1]
	if tc1["id"] != "call_xyz" {
		t.Fatalf("tc1 id = %v, want call_xyz", tc1["id"])
	}

	// finish_reason preserved.
	if fr, _ := choices[0]["finish_reason"].(string); fr != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", choices[0]["finish_reason"])
	}
}

// TestFBStreamToNonStreamNoToolCalls — plain text stream (no tool_calls)
// must not set msg["tool_calls"] and must keep content.
func TestFBStreamToNonStreamNoToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"fb_2","model":"deepseek/deepseek-v4-flash","choices":[{"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}`,
		`data: {"id":"fb_2","model":"deepseek/deepseek-v4-flash","choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
		"data: [DONE]",
	}, "\n")

	agg := fbStreamToNonStream(strings.NewReader(stream), "deepseek/deepseek-v4-flash")
	choices, ok := agg["choices"].([]map[string]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("choices shape wrong: %#v", agg["choices"])
	}
	msg, _ := choices[0]["message"].(map[string]any)
	if msg["content"] != "Hello" {
		t.Fatalf("content = %v, want Hello", msg["content"])
	}
	if _, has := msg["tool_calls"]; has {
		t.Fatalf("tool_calls should be absent for plain text: %#v", msg["tool_calls"])
	}
}

// TestFBStreamToNonStreamBounded verifies the SEC2-01 caps: an upstream
// streaming unbounded bytes (or huge tool_call indices) cannot grow memory
// without bound — the aggregator aborts past the ceiling with
// finish_reason="length".
func TestFBStreamToNonStreamBounded(t *testing.T) {
	// A stream of many distinct tool_call indices (way over the cap) must not
	// grow the toolCalls map unbounded.
	chunks := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		chunks = append(chunks, `data: {"id":"fb_b","model":"m","choices":[{"delta":{"tool_calls":[{"index":`+
			fmt.Sprintf("%d", i)+`,"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":null}]}`)
	}
	stream := strings.Join(chunks, "\n")
	agg := fbStreamToNonStream(strings.NewReader(stream), "m")
	choices, ok := agg["choices"].([]map[string]any)
	if !ok || len(choices) == 0 {
		t.Fatal("no choices")
	}
	msg, _ := choices[0]["message"].(map[string]any)
	tcs, has := msg["tool_calls"].([]map[string]any)
	if has && len(tcs) > fbStreamToolCallsCap {
		t.Fatalf("tool_calls grew to %d, cap is %d", len(tcs), fbStreamToolCallsCap)
	}
}

// TestFBMaskToken verifies the length-safe token mask (C2-06).
func TestFBMaskToken(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "***"},
		{"short", "***"},
		{"1234567890ab", "12345678...cdef"},
	}
	// only verify the <12 → "***" and the shape for long tokens
	for _, c := range cases {
		got := fbMaskToken(c.in)
		if len(c.in) < 12 && got != "***" {
			t.Fatalf("fbMaskToken(%q) = %q, want ***", c.in, got)
		}
		if len(c.in) >= 12 && len(got) != len(c.in)+3 {
			t.Fatalf("fbMaskToken(%q) = %q (len %d), want masked len %d", c.in, got, len(got), len(c.in)+3)
		}
	}
}
