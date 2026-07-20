package main

import (
	"testing"

	"foxrouters/internal/proxy"
)

// TestProxyPool_CRUD_NoDB exercises the in-memory paths (nil store) so we
// know the round-robin, validation, and toggle logic works without Redis.
func TestProxyPool_CRUD_NoDB(t *testing.T) {
	pool := proxy.NewProxyPool(nil)
	if pool.Len() != 0 {
		t.Fatalf("expected empty pool, got %d", pool.Len())
	}

	// Add two proxies.
	a, err := pool.Add(proxy.ProxyEntry{Protocol: "http", Host: "a.example.com", Port: 8080, Label: "a", Enabled: true})
	if err != nil {
		t.Fatalf("Add a: %v", err)
	}
	b, err := pool.Add(proxy.ProxyEntry{Protocol: "socks5", Host: "b.example.com", Port: 1080, Label: "b", Enabled: true})
	if err != nil {
		t.Fatalf("Add b: %v", err)
	}
	if pool.Len() != 2 || pool.EnabledLen() != 2 {
		t.Fatalf("expected 2/2, got %d/%d", pool.Len(), pool.EnabledLen())
	}

	// Round-robin between them for the "grok" scope. Both entries default
	// to ["all"] so both are eligible.
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		e, err := pool.Next("grok")
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		seen[e.ID]++
	}
	if seen[a.ID] < 1 || seen[b.ID] < 1 {
		t.Fatalf("round-robin did not visit both entries: %+v", seen)
	}

	// Toggle disables one.
	enabled, err := pool.Toggle(a.ID)
	if err != nil || enabled {
		t.Fatalf("Toggle a: enabled=%v err=%v", enabled, err)
	}
	if pool.EnabledLen() != 1 {
		t.Fatalf("expected 1 enabled after toggle, got %d", pool.EnabledLen())
	}

	// MarkFailed on b five times → auto-disabled.
	for i := 0; i < 5; i++ {
		pool.MarkFailed(b.ID)
	}
	if pool.EnabledLen() != 0 {
		t.Fatalf("expected 0 enabled after fail threshold, got %d", pool.EnabledLen())
	}
	if _, err := pool.Next("grok"); err == nil {
		t.Fatalf("expected Next to fail when no proxies enabled")
	}

	// Delete a.
	if err := pool.Delete(a.ID); err != nil {
		t.Fatalf("Delete a: %v", err)
	}
	if pool.Len() != 1 {
		t.Fatalf("expected 1 remaining, got %d", pool.Len())
	}
}

func TestProxyPool_Validation(t *testing.T) {
	pool := proxy.NewProxyPool(nil)
	cases := []struct {
		name  string
		entry proxy.ProxyEntry
	}{
		{"bad protocol", proxy.ProxyEntry{Protocol: "ftp", Host: "x", Port: 1}},
		{"empty host", proxy.ProxyEntry{Protocol: "http", Host: "", Port: 1}},
		{"port too low", proxy.ProxyEntry{Protocol: "http", Host: "x", Port: 0}},
		{"port too high", proxy.ProxyEntry{Protocol: "http", Host: "x", Port: 70000}},
		{"host with space", proxy.ProxyEntry{Protocol: "http", Host: "bad host", Port: 80}},
		{"bad upstream", proxy.ProxyEntry{Protocol: "http", Host: "x", Port: 80, Upstreams: []string{"gemini"}}},
	}
	for _, tc := range cases {
		if _, err := pool.Add(tc.entry); err == nil {
			t.Errorf("%s: expected validation error, got none", tc.name)
		}
	}
}

func TestProxyPool_PasswordMasking(t *testing.T) {
	pool := proxy.NewProxyPool(nil)
	created, err := pool.Add(proxy.ProxyEntry{
		Protocol: "http", Host: "p.example.com", Port: 8080,
		Username: "u", Password: "secret", Enabled: true,
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if created.Password != "secret" {
		t.Fatalf("Add response should reveal raw password, got %q", created.Password)
	}
	list := pool.List()
	if len(list) != 1 {
		t.Fatalf("list len: %d", len(list))
	}
	if list[0].Password != "***" {
		t.Fatalf("List should mask password, got %q", list[0].Password)
	}
}

// TestProxyPool_UpstreamScoping verifies Next(upstream) filters by the
// Upstreams field: "all" matches everything, specific scopes only match
// their own upstream.
func TestProxyPool_UpstreamScoping(t *testing.T) {
	pool := proxy.NewProxyPool(nil)

	all, err := pool.Add(proxy.ProxyEntry{
		Protocol: "http", Host: "all.example.com", Port: 8080,
		Enabled: true, Upstreams: []string{"all"},
	})
	if err != nil {
		t.Fatalf("Add all: %v", err)
	}
	grokOnly, err := pool.Add(proxy.ProxyEntry{
		Protocol: "http", Host: "grok.example.com", Port: 8080,
		Enabled: true, Upstreams: []string{"grok"},
	})
	if err != nil {
		t.Fatalf("Add grok-only: %v", err)
	}
	cbOnly, err := pool.Add(proxy.ProxyEntry{
		Protocol: "http", Host: "cb.example.com", Port: 8080,
		Enabled: true, Upstreams: []string{"codebuddy"},
	})
	if err != nil {
		t.Fatalf("Add cb-only: %v", err)
	}

	// Next("grok") should only return `all` or `grokOnly`, never `cbOnly`.
	grokEligible := map[string]bool{all.ID: true, grokOnly.ID: true}
	for i := 0; i < 30; i++ {
		e, err := pool.Next("grok")
		if err != nil {
			t.Fatalf("Next(grok) iter %d: %v", i, err)
		}
		if !grokEligible[e.ID] {
			t.Fatalf("Next(grok) returned non-grok-scoped proxy: id=%s host=%s", e.ID, e.Host)
		}
	}

	// Next("codebuddy") should only return `all` or `cbOnly`, never `grokOnly`.
	cbEligible := map[string]bool{all.ID: true, cbOnly.ID: true}
	for i := 0; i < 30; i++ {
		e, err := pool.Next("codebuddy")
		if err != nil {
			t.Fatalf("Next(codebuddy) iter %d: %v", i, err)
		}
		if !cbEligible[e.ID] {
			t.Fatalf("Next(codebuddy) returned non-cb-scoped proxy: id=%s host=%s", e.ID, e.Host)
		}
	}

	// Confirm both eligible sets get hit across enough iterations
	// (round-robin should visit both in each scope).
	grokSeen := map[string]int{}
	for i := 0; i < 30; i++ {
		e, _ := pool.Next("grok")
		if e != nil {
			grokSeen[e.ID]++
		}
	}
	if grokSeen[all.ID] < 1 || grokSeen[grokOnly.ID] < 1 {
		t.Fatalf("RR did not visit both grok-eligible proxies: %+v", grokSeen)
	}
}

// TestProxyPool_ScopingNoMatch verifies Next returns an error when no
// enabled proxy matches the requested scope.
func TestProxyPool_ScopingNoMatch(t *testing.T) {
	pool := proxy.NewProxyPool(nil)
	_, err := pool.Add(proxy.ProxyEntry{
		Protocol: "http", Host: "g.example.com", Port: 8080,
		Enabled: true, Upstreams: []string{"grok"},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	// grok-only proxy is enabled — codebuddy scope should find no candidate.
	if _, err := pool.Next("codebuddy"); err == nil {
		t.Fatalf("expected Next(codebuddy) to fail (no cb-scoped proxies)")
	}
	// grok scope should work.
	if _, err := pool.Next("grok"); err != nil {
		t.Fatalf("Next(grok) unexpected error: %v", err)
	}
}

// TestProxyPool_EmptyUpstreamsDefaultsToAll verifies backward compat:
// entries added without an Upstreams list default to ["all"].
func TestProxyPool_EmptyUpstreamsDefaultsToAll(t *testing.T) {
	pool := proxy.NewProxyPool(nil)
	created, err := pool.Add(proxy.ProxyEntry{
		Protocol: "http", Host: "legacy.example.com", Port: 8080, Enabled: true,
		// Upstreams intentionally left nil.
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(created.Upstreams) != 1 || created.Upstreams[0] != "all" {
		t.Fatalf("expected Upstreams=[all] by default, got %+v", created.Upstreams)
	}
	// Both scopes should find this proxy.
	for _, up := range []string{"grok", "codebuddy"} {
		e, err := pool.Next(up)
		if err != nil {
			t.Fatalf("Next(%s): %v", up, err)
		}
		if e.ID != created.ID {
			t.Fatalf("Next(%s) returned unexpected id: %s", up, e.ID)
		}
	}
}

// TestProxyPool_UpstreamsNormalisation verifies dedupe + "all" collapse
// happens on Add.
func TestProxyPool_UpstreamsNormalisation(t *testing.T) {
	pool := proxy.NewProxyPool(nil)
	// "all" + "grok" should collapse to just ["all"].
	e1, err := pool.Add(proxy.ProxyEntry{
		Protocol: "http", Host: "a.example.com", Port: 80, Enabled: true,
		Upstreams: []string{"all", "grok"},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(e1.Upstreams) != 1 || e1.Upstreams[0] != "all" {
		t.Fatalf("expected [all] after collapse, got %+v", e1.Upstreams)
	}
	// Duplicates should dedupe.
	e2, err := pool.Add(proxy.ProxyEntry{
		Protocol: "http", Host: "b.example.com", Port: 80, Enabled: true,
		Upstreams: []string{"grok", "grok", "codebuddy"},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(e2.Upstreams) != 2 {
		t.Fatalf("expected dedupe to 2 entries, got %+v", e2.Upstreams)
	}
}
