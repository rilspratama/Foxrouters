package upstream

import (
	"encoding/json"
	"errors"
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

// TestFbNextSessionAware verifies session-aware account selection:
// live session match for the requested model > idle (no session) > mismatch
// (delete+recreate fallback). Mismatch accounts must only be picked when
// nothing better exists.
func TestFbNextSessionAware(t *testing.T) {
	fresh := func(token string) *FreebuffAccount {
		return &FreebuffAccount{
			Token:   token,
			Email:   token + "@example.com",
			QuotaLimit: 6, QuotaRecent: 0,
		}
	}
	future := time.Now().Add(30 * time.Minute)

	t.Run("session match beats idle", func(t *testing.T) {
		am := NewFreebuffAccountManager(nil)
		am.accounts["a"] = fresh("a")
		am.accounts["b"] = fresh("b")
		am.storeSession("a", "deepseek/deepseek-v4-flash",
			&FreebuffSession{InstanceID: "s1", Model: "deepseek/deepseek-v4-flash", ExpiresAt: future})
		for i := 0; i < 10; i++ {
			got, err := am.Next("deepseek/deepseek-v4-flash")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Token != "a" {
				t.Fatalf("iter %d: got %s, want a (session match)", i, got.Token)
			}
		}
	})

	t.Run("idle beats mismatch", func(t *testing.T) {
		am := NewFreebuffAccountManager(nil)
		am.accounts["a"] = fresh("a") // has flash session
		am.accounts["b"] = fresh("b") // idle
		am.accounts["c"] = fresh("c") // idle
		am.storeSession("a", "deepseek/deepseek-v4-flash",
			&FreebuffSession{InstanceID: "s1", Model: "deepseek/deepseek-v4-flash", ExpiresAt: future})
		for i := 0; i < 20; i++ {
			got, err := am.Next("mimo/mimo-v2.5") // different model
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Token == "a" {
				t.Fatalf("iter %d: picked a (mismatch) while idle accounts exist", i)
			}
		}
	})

	t.Run("mismatch fallback when nothing better", func(t *testing.T) {
		am := NewFreebuffAccountManager(nil)
		am.accounts["a"] = fresh("a")
		am.accounts["b"] = fresh("b")
		am.storeSession("a", "deepseek/deepseek-v4-flash",
			&FreebuffSession{InstanceID: "s1", Model: "deepseek/deepseek-v4-flash", ExpiresAt: future})
		am.storeSession("b", "deepseek/deepseek-v4-flash",
			&FreebuffSession{InstanceID: "s2", Model: "deepseek/deepseek-v4-flash", ExpiresAt: future})
		seen := map[string]bool{}
		for i := 0; i < 20; i++ {
			got, err := am.Next("mimo/mimo-v2.5")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			seen[got.Token] = true
		}
		if !seen["a"] || !seen["b"] {
			t.Fatalf("expected both mismatch accounts to be served, got %v", seen)
		}
	})

	t.Run("expired session treated as idle", func(t *testing.T) {
		am := NewFreebuffAccountManager(nil)
		am.accounts["a"] = fresh("a") // expired flash session
		am.accounts["b"] = fresh("b")
		am.storeSession("a", "deepseek/deepseek-v4-flash",
			&FreebuffSession{InstanceID: "s1", Model: "deepseek/deepseek-v4-flash", ExpiresAt: time.Now().Add(-1 * time.Minute)})
		for i := 0; i < 10; i++ {
			got, err := am.Next("deepseek/deepseek-v4-flash")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// expired "a" is idle — b may also be picked by RR, but "a" must
			// not be picked as a session match (it should behave as idle).
			if got.Token != "a" && got.Token != "b" {
				t.Fatalf("iter %d: unexpected account %s", i, got.Token)
			}
		}
	})
}

// TestFbQuotaByModelJSONRoundTrip verifies the per-model quota map survives
// JSON marshal/unmarshal (the Redis persistence format).
func TestFbQuotaByModelJSONRoundTrip(t *testing.T) {
	acc := &FreebuffAccount{
		Token: "tok-qbm",
		Tier:  "limited",
		QuotaByModel: map[string]FbModelQuota{
			"deepseek/deepseek-v4-flash": {Limit: 6, RecentCount: 4, Period: "pacific_day", ResetAt: "2026-08-11T07:00:00Z", EntitlementBase: 6},
			"mimo/mimo-v2.5":             {Limit: 6, RecentCount: 4, Period: "pacific_day"},
			"z-ai/glm-5.2":               {Limit: 0, RecentCount: 0, Period: "pacific_day"},
		},
	}
	raw, err := json.Marshal(acc.QuotaByModel)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]FbModelQuota
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 models, got %d", len(got))
	}
	flash := got["deepseek/deepseek-v4-flash"]
	if flash.Limit != 6 || flash.RecentCount != 4 || flash.EntitlementBase != 6 {
		t.Fatalf("flash quota mismatch: %+v", flash)
	}
	if got["z-ai/glm-5.2"].Limit != 0 {
		t.Fatalf("glm should have zero limit: %+v", got["z-ai/glm-5.2"])
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

// TestFbSessionCreateErrorBanned verifies 403 {"status":"banned"} is classified
// as ErrFBBanned while other non-200 responses are not.
func TestFbSessionCreateErrorBanned(t *testing.T) {
	if err := fbSessionCreateError(403, []byte(`{"status":"banned"}`)); !errors.Is(err, ErrFBBanned) {
		t.Fatalf("403 banned body: got %v, want ErrFBBanned", err)
	}
	// Other statuses/bodies must NOT be classified as banned.
	for name, tc := range map[string]struct {
		code int
		body []byte
	}{
		"403 other body": {403, []byte(`{"error":"forbidden"}`)},
		"403 empty":      {403, nil},
		"429 quota":      {429, []byte(`{"model":"x","entitlementBreakdown":{"base":6}}`)},
		"500":            {500, []byte(`internal error`)},
	} {
		if err := fbSessionCreateError(tc.code, tc.body); errors.Is(err, ErrFBBanned) {
			t.Fatalf("%s: unexpected ErrFBBanned (code=%d body=%s)", name, tc.code, tc.body)
		}
	}
}

// TestFbBannedLifecycle verifies MarkBanned persists + surfaces in ListAccounts
// and that Next() skips banned accounts.
func TestFbBannedLifecycle(t *testing.T) {
	am := NewFreebuffAccountManager(nil)
	am.accounts["account-token-aaaa"] = &FreebuffAccount{Token: "account-token-aaaa", Email: "a@example.com", QuotaLimit: 6, QuotaRecent: 0}
	am.accounts["account-token-bbbb"] = &FreebuffAccount{Token: "account-token-bbbb", Email: "b@example.com", QuotaLimit: 6, QuotaRecent: 0}

	am.MarkBanned("account-token-aaaa")

	// Next must never return the banned account.
	for i := 0; i < 10; i++ {
		got, err := am.Next("deepseek/deepseek-v4-flash")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Token == "account-token-aaaa" {
			t.Fatalf("iter %d: Next returned banned account a", i)
		}
	}

	// ListAccounts surfaces the ban.
	for _, e := range am.ListAccounts() {
		if e["email"] == "a@example.com" {
			if e["status"] != "banned" || e["banned"] != true {
				t.Fatalf("expected banned status for a, got status=%v banned=%v", e["status"], e["banned"])
			}
			if bannedAt, _ := e["banned_at"].(time.Time); bannedAt.IsZero() {
				t.Fatalf("expected banned_at to be set")
			}
		}
	}

	// MarkBanned is idempotent (no panic, second call fine).
	am.MarkBanned("account-token-aaaa")

	// Banned account can be restored by clearing the flag (manual re-enable).
	am.mu.RLock()
	accA := am.accounts["account-token-aaaa"]
	am.mu.RUnlock()
	accA.mu.Lock()
	accA.Banned = false
	accA.mu.Unlock()
	// Round-robin means the FIRST call may hit the other account — the restored
	// one must be selectable within a few iterations.
	restored := false
	for i := 0; i < 10; i++ {
		got, err := am.Next("deepseek/deepseek-v4-flash")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Token == "account-token-aaaa" {
			restored = true
			break
		}
	}
	if !restored {
		t.Fatalf("expected restored account a to be selectable again")
	}
}
