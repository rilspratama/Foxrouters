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

	// Round-robin between them.
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		e, err := pool.Next()
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
	if _, err := pool.Next(); err == nil {
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
