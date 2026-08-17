package upstream

import (
	"strings"
	"testing"
)

// TestFreebuffAPIBaseAtomic ensures the runtime-overridable base URL works:
// default from env, SetFreebuffAPIBase changes it, empty set is ignored.
func TestFreebuffAPIBaseAtomic(t *testing.T) {
	orig := FreebuffAPIBase()
	defer SetFreebuffAPIBase(orig) // restore for other tests

	// Default must be a valid https base.
	if orig == "" {
		t.Fatal("default FreebuffAPIBase must not be empty")
	}

	// Set overrides.
	if err := SetFreebuffAPIBase("https://1.1.1.1"); err != nil {
		t.Fatalf("valid set rejected: %v", err)
	}
	if got := FreebuffAPIBase(); got != "https://1.1.1.1" {
		t.Fatalf("after set, got %q, want https://relay.example.com", got)
	}

	// Trailing slash trimmed (single slash only).
	if err := SetFreebuffAPIBase("https://1.1.1.1/"); err != nil {
		t.Fatalf("trailing-slash set rejected: %v", err)
	}
	if got := FreebuffAPIBase(); got != "https://1.1.1.1" {
		t.Fatalf("trailing slash not trimmed: %q", got)
	}

	// Empty / whitespace set is rejected (keeps previous).
	if err := SetFreebuffAPIBase("   "); err == nil {
		t.Fatal("empty set should be rejected")
	}
	if got := FreebuffAPIBase(); got != "https://1.1.1.1" {
		t.Fatalf("empty set should keep previous, got %q", got)
	}
}

// TestFreebuffAPIBaseValidation covers the single-choke-point rules:
// https-only, public host, bare origin, length bound, no userinfo.
func TestFreebuffAPIBaseValidation(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		valid bool
	}{
		{"https ok", "https://1.1.1.1", true},
		{"https with path rejected", "https://1.1.1.1/some/path", false},
		{"https with query rejected", "https://relay.example.com?x=1", false},
		{"userinfo rejected", "https://user:pass@relay.example.com", false},
		{"http rejected by default", "http://1.1.1.1", false},
		{"bare https:// rejected", "https://", false},
		{"ftp rejected", "ftp://relay.example.com", false},
		{"empty rejected", "", false},
		{"whitespace rejected", "   ", false},
		{"loopback literal rejected", "https://127.0.0.1:8000", false},
		{"localhost rejected", "https://localhost:8000", false},
		{"private literal rejected", "https://10.0.0.5", false},
		{"too long rejected", "https://1.1.1.1/" + strings.Repeat("a", 300), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NormalizeFreebuffAPIBase(c.in)
			if c.valid && err != nil {
				t.Fatalf("expected valid, got error: %v", err)
			}
			if !c.valid && err == nil {
				t.Fatalf("expected rejection, got nil error")
			}
		})
	}
}

// TestFreebuffAPIBaseLoadPersisted verifies LoadFreebuffAPIBase prefers a
// persisted Redis value over the env default, and skips invalid persisted
// values (falling back to env/default) instead of poisoning the gateway.
type fakeFBConfigStore struct {
	v string
}

func (f *fakeFBConfigStore) GetFBConfig(field string) (string, error) {
	if field == "api_base" {
		return f.v, nil
	}
	return "", nil
}

func TestFreebuffAPIBaseLoadPersisted(t *testing.T) {
	orig := FreebuffAPIBase()
	defer SetFreebuffAPIBase(orig)

	// Empty persisted → keeps current.
	LoadFreebuffAPIBase(&fakeFBConfigStore{v: ""})
	if got := FreebuffAPIBase(); got != orig {
		t.Fatalf("empty persisted changed base to %q (want %q)", got, orig)
	}

	// Non-empty persisted → wins.
	LoadFreebuffAPIBase(&fakeFBConfigStore{v: "https://1.1.1.1"})
	if got := FreebuffAPIBase(); got != "https://1.1.1.1" {
		t.Fatalf("persisted override not applied: got %q", got)
	}

	// Invalid persisted (http://) → skipped, keeps current (no poisoning).
	LoadFreebuffAPIBase(&fakeFBConfigStore{v: "http://evil.example.com"})
	if got := FreebuffAPIBase(); got != "https://1.1.1.1" {
		t.Fatalf("invalid persisted should be skipped, got %q", got)
	}

	// nil store → no panic, no change.
	LoadFreebuffAPIBase(nil)
	if got := FreebuffAPIBase(); got != "https://1.1.1.1" {
		t.Fatalf("nil store changed base to %q", got)
	}
}
