package db

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBodyStringCap verifies oversized request/response bodies are bounded to
// logBodyCap with a truncation marker, while normal bodies pass through
// untouched. Prod incident 2026-08-17: multi-MB request_body rows bloated the
// DB to 21GB and blocked /history/recent readers past busy_timeout.
func TestBodyStringCap(t *testing.T) {
	orig := logBodyCap
	defer func() { logBodyCap = orig }()
	logBodyCap = 64

	// Empty → empty.
	if got := bodyString(nil); got != "" {
		t.Fatalf("nil body = %q, want empty", got)
	}
	if got := bodyString(json.RawMessage{}); got != "" {
		t.Fatalf("empty body = %q, want empty", got)
	}

	// Small body → verbatim.
	small := json.RawMessage(`{"a":1}`)
	if got := bodyString(small); got != string(small) {
		t.Fatalf("small body mangled: %q", got)
	}

	// Oversized JSON object → capped + marker.
	big := json.RawMessage(`{"payload":"` + strings.Repeat("x", 100) + `"}`)
	got := bodyString(big)
	if len(got) > 64+len(" ...(truncated)") {
		t.Fatalf("big body not capped: len %d", len(got))
	}
	if !strings.Contains(got, "...(truncated)") {
		t.Fatalf("big body missing truncation marker: %q", got)
	}

	// Oversized non-object → capped, no marker needed but still bounded.
	bigPlain := json.RawMessage(strings.Repeat("y", 200))
	got2 := bodyString(bigPlain)
	if len(got2) > 64 {
		t.Fatalf("plain big body not capped: len %d", len(got2))
	}
}
