package upstream

import (
	"net/http"
	"testing"
	"time"
)

// TestFbSessionCacheMemoryRoundTrip verifies storeSession → cachedSession
// round-trip through the in-memory L1 layer (nil db → Redis skipped).
func TestFbSessionCacheMemoryRoundTrip(t *testing.T) {
	am := NewFreebuffAccountManager(nil)
	token, model := "tok-test-1", "deepseek/deepseek-v4-flash"
	sess := &FreebuffSession{
		InstanceID:  "inst-abc",
		Model:       model,
		ExpiresAt:   time.Now().Add(30 * time.Minute),
		RemainingMs: 3_600_000,
	}
	am.storeSession(token, model, sess)

	got := am.cachedSession(token, model)
	if got == nil {
		t.Fatalf("cachedSession returned nil after store")
	}
	if got.InstanceID != "inst-abc" || got.Model != model {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// different model must NOT hit the same cache entry
	if other := am.cachedSession(token, "mimo/mimo-v2.5"); other != nil {
		t.Fatalf("cachedSession for another model unexpectedly hit: %+v", other)
	}
}

// TestFbSessionCacheExpired verifies an expired session is not returned and
// does not come back from the memory layer after expiry.
func TestFbSessionCacheExpired(t *testing.T) {
	am := NewFreebuffAccountManager(nil)
	token, model := "tok-test-2", "deepseek/deepseek-v4-flash"
	am.storeSession(token, model, &FreebuffSession{
		InstanceID: "inst-expired",
		Model:      model,
		ExpiresAt:  time.Now().Add(30 * time.Second), // below MIN_REMAINING (60s)
	})
	if got := am.cachedSession(token, model); got != nil {
		t.Fatalf("expected nil for near-expired session, got %+v", got)
	}
}

// TestFbRunCacheMemoryRoundTrip verifies storeRun → cachedRun round-trip and
// TTL-based expiry on the memory path.
func TestFbRunCacheMemoryRoundTrip(t *testing.T) {
	am := NewFreebuffAccountManager(nil)
	token, agent := "tok-test-3", "base2-free-deepseek-flash"
	am.storeRun(token, agent, "run-xyz")

	if runID, ok := am.cachedRun(token, agent); !ok || runID != "run-xyz" {
		t.Fatalf("cachedRun round-trip failed: runID=%q ok=%v", runID, ok)
	}

	// Different agent must not hit
	if _, ok := am.cachedRun(token, "base2-free-mimo"); ok {
		t.Fatalf("cachedRun for another agent unexpectedly hit")
	}

	// Manually age the entry past TTL → must miss
	fbRunCache.Lock()
	fbRunCache.m[fbRunKeyPrefix+token+":"+agent] = fbRunEntry{
		RunID:     "run-xyz",
		CreatedAt: time.Now().Add(-FREEBUFF_RUN_CACHE_TTL - time.Minute),
	}
	fbRunCache.Unlock()
	if _, ok := am.cachedRun(token, agent); ok {
		t.Fatalf("cachedRun returned stale entry past TTL")
	}
}

// TestFbDeleteSessionClearsCache verifies fbDeleteSession clears the in-memory
// cache for a token even when the upstream DELETE fails (network error).
func TestFbDeleteSessionClearsCache(t *testing.T) {
	am := NewFreebuffAccountManager(nil)
	token := "tok-test-4"
	am.storeSession(token, "deepseek/deepseek-v4-flash", &FreebuffSession{
		InstanceID: "inst-a", Model: "deepseek/deepseek-v4-flash",
		ExpiresAt: time.Now().Add(30 * time.Minute),
	})
	am.storeSession(token, "mimo/mimo-v2.5", &FreebuffSession{
		InstanceID: "inst-b", Model: "mimo/mimo-v2.5",
		ExpiresAt: time.Now().Add(30 * time.Minute),
	})
	// other token must survive
	am.storeSession("tok-other", "deepseek/deepseek-v4-flash", &FreebuffSession{
		InstanceID: "inst-c", Model: "deepseek/deepseek-v4-flash",
		ExpiresAt: time.Now().Add(30 * time.Minute),
	})

	// client pointing at a closed port → Do() errors, but cache must still clear
	badClient := &http.Client{Timeout: 200 * time.Millisecond}
	am.fbDeleteSession(badClient, token)

	for _, model := range []string{"deepseek/deepseek-v4-flash", "mimo/mimo-v2.5"} {
		if got := am.cachedSession(token, model); got != nil {
			t.Fatalf("session cache for %s not cleared after fbDeleteSession: %+v", model, got)
		}
	}
	if got := am.cachedSession("tok-other", "deepseek/deepseek-v4-flash"); got == nil {
		t.Fatalf("unrelated token cache entry was cleared")
	}
}

// TestFbNextTierGating verifies Next(model) skips limited-tier accounts for
// premium models, skips blocked accounts always, and lets unknown tier pass.
func TestFbNextTierGating(t *testing.T) {
	mk := func(token, tier string) *FreebuffAccount {
		return &FreebuffAccount{
			Token:   token,
			Email:   token + "@example.com",
			Tier:    tier,
			QuotaLimit: 6, QuotaRecent: 0,
		}
	}
	cases := []struct {
		name        string
		accounts    []*FreebuffAccount
		model       string
		wantToken   string
		wantErr     bool
	}{
		{"limited + premium → no eligible", []*FreebuffAccount{mk("a", "limited")}, "deepseek/deepseek-v4-pro", "", true},
		{"limited + standard → picked", []*FreebuffAccount{mk("a", "limited")}, "deepseek/deepseek-v4-flash", "a", false},
		{"full + premium → picked", []*FreebuffAccount{mk("a", "full")}, "deepseek/deepseek-v4-pro", "a", false},
		{"unknown tier + premium → picked (pass-through)", []*FreebuffAccount{mk("a", "")}, "openai/gpt-5.6-luna", "a", false},
		{"blocked + standard → no eligible", []*FreebuffAccount{mk("a", "blocked")}, "deepseek/deepseek-v4-flash", "", true},
		{"mixed: limited+full → full wins premium", []*FreebuffAccount{mk("a", "limited"), mk("b", "full")}, "minimax/minimax-m3", "b", false},
		{"mixed: standard mimo on limited", []*FreebuffAccount{mk("a", "limited"), mk("b", "full")}, "mimo/mimo-v2.5", "", false}, // either ok
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			am := NewFreebuffAccountManager(nil)
			for _, acc := range tc.accounts {
				am.accounts[acc.Token] = acc
			}
			got, err := am.Next(tc.model)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got account %s", got.Token)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantToken != "" && got.Token != tc.wantToken {
				t.Fatalf("got %s, want %s", got.Token, tc.wantToken)
			}
			if tc.wantToken == "" && got == nil {
				t.Fatalf("expected an account")
			}
		})
	}
}

// TestFbRedisNilSafe verifies cache helpers never panic when db is nil and
// fall back to memory-only behavior.
func TestFbRedisNilSafe(t *testing.T) {
	am := NewFreebuffAccountManager(nil)
	if am.redis() != nil {
		t.Fatalf("redis() should return nil for nil db")
	}
	if got := am.cachedSession("tok", "model"); got != nil {
		t.Fatalf("expected nil session")
	}
	if _, ok := am.cachedRun("tok", "agent"); ok {
		t.Fatalf("expected run miss")
	}
	// store with nil db must not panic
	am.storeSession("tok", "model", &FreebuffSession{InstanceID: "i", Model: "model", ExpiresAt: time.Now().Add(time.Hour)})
	am.storeRun("tok", "agent", "run")
}
