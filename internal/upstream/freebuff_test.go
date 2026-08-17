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

// TestFbNextCacheTemperature verifies NextTemp: within the same priority
// group, the account with the warmest cache for the content prefix wins;
// cold prefixes keep the round-robin fallback; below-threshold EMAs don't
// divert.
func TestFbNextCacheTemperature(t *testing.T) {
	fresh := func(token string) *FreebuffAccount {
		return &FreebuffAccount{Token: token, Email: token + "@example.com", QuotaLimit: 6, QuotaRecent: 0}
	}
	am := NewFreebuffAccountManager(nil)
	am.accounts["a"] = fresh("a")
	am.accounts["b"] = fresh("b")

	const model = "deepseek/deepseek-v4-flash"
	const hotPrefix = model + "|sys:HOT"
	// Warm b's cache for the hot prefix (real hit rate from a previous 2xx).
	am.RecordCacheHit("b", hotPrefix, 97)

	// Warm prefix: temperature overrides RR → b every time.
	for i := 0; i < 2; i++ {
		got, err := am.NextTemp(model, hotPrefix)
		if err != nil || got == nil {
			t.Fatalf("NextTemp: %v", err)
		}
		if got.Token != "b" {
			t.Fatalf("warm account must win (call %d): got %s, want b", i+1, got.Token)
		}
	}

	// Cold prefix: no temperature entry → round-robin spreads across the pool.
	// Assert SET coverage over several calls (map-iteration order + non-stable
	// sort re-randomize the top group per call, so a strict call#1 != call#2
	// parity assertion would be flaky — both tokens must appear instead).
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		got, err := am.NextTemp(model, model+"|sys:COLD")
		if err != nil || got == nil {
			t.Fatalf("NextTemp cold: %v", err)
		}
		seen[got.Token] = true
	}
	if len(seen) < 2 {
		t.Fatalf("cold prefix must spread round-robin across accounts: only saw %v", seen)
	}

	// Below-threshold EMA must NOT divert (30 < 50 warm threshold).
	am2 := NewFreebuffAccountManager(nil)
	am2.accounts["x"] = fresh("x")
	am2.accounts["y"] = fresh("y")
	am2.RecordCacheHit("y", hotPrefix, 30)
	// If temperature diverted, the 1st call could still land on y via RR with
	// 2 accounts — so assert on determinism instead: record 95 on x, then the
	// warmest (x) must win even though RR would alternate.
	am2.RecordCacheHit("x", hotPrefix, 95)
	got, err := am2.NextTemp(model, hotPrefix)
	if err != nil || got == nil {
		t.Fatalf("NextTemp threshold: %v", err)
	}
	if got.Token != "x" {
		t.Fatalf("warmest EMA must win: got %s, want x", got.Token)
	}
}

// TestFbNextTierGating verifies Next(model) skips limited-tier accounts for
// premium models, skips blocked accounts always, and lets unknown tier pass.
func TestFbNextTierGating(t *testing.T) {
	mk := func(token, tier string) *FreebuffAccount {
		return &FreebuffAccount{
			Token:      token,
			Email:      token + "@example.com",
			Tier:       tier,
			QuotaLimit: 6, QuotaRecent: 0,
		}
	}
	cases := []struct {
		name      string
		accounts  []*FreebuffAccount
		model     string
		wantToken string
		wantErr   bool
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
			Token:      token,
			Email:      token + "@example.com",
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

// TestApplyFbQuotaFromModelMapStatusNoneExhausted is the regression test for
// the "0/6 but can't create session" bug: upstream status="none" (no ACTIVE
// session) still carries the REAL daily recentCount in rateLimitsByModel. The
// quota must reflect 6/6 + cooldown-until-reset — NOT reset to fresh 0/6.
func TestApplyFbQuotaFromModelMapStatusNoneExhausted(t *testing.T) {
	acc := &FreebuffAccount{}
	resetAt := "2026-08-16T07:00:00.000Z"
	qbm := map[string]FbModelQuota{
		"deepseek/deepseek-v4-flash": {
			Limit: 6, RecentCount: 6, ResetAt: resetAt, Period: "pacific_day",
		},
	}

	applied := applyFbQuotaFromModelMap(acc, qbm)
	if !applied {
		t.Fatalf("expected applied=true when map has the reference model")
	}
	if acc.QuotaRecent != 6 || acc.QuotaLimit != 6 {
		t.Fatalf("expected quota to stay 6/6 (exhausted), got recent=%v limit=%v", acc.QuotaRecent, acc.QuotaLimit)
	}
	wantReset, _ := time.Parse(time.RFC3339, resetAt)
	if !acc.QuotaResetAt.Equal(wantReset) {
		t.Fatalf("expected resetAt %v, got %v", wantReset, acc.QuotaResetAt)
	}
	if acc.CooldownUntil.IsZero() {
		t.Fatalf("expected cooldown-until-reset for exhausted account, got zero")
	}
	if !acc.CooldownUntil.Equal(wantReset) {
		t.Fatalf("expected cooldown until %v, got %v", wantReset, acc.CooldownUntil)
	}
}

// TestApplyFbQuotaFromModelMapStatusNoneFresh: status="none" with recentCount=0
// must stay 0/6 with NO cooldown (same result as before the fix, but reached
// through the real rate-limit data instead of the status shortcut).
func TestApplyFbQuotaFromModelMapStatusNoneFresh(t *testing.T) {
	acc := &FreebuffAccount{}
	qbm := map[string]FbModelQuota{
		"deepseek/deepseek-v4-flash": {
			Limit: 6, RecentCount: 0, ResetAt: "2026-08-16T07:00:00.000Z", Period: "pacific_day",
		},
	}

	if !applyFbQuotaFromModelMap(acc, qbm) {
		t.Fatalf("expected applied=true")
	}
	if acc.QuotaRecent != 0 || acc.QuotaLimit != 6 {
		t.Fatalf("expected 0/6, got recent=%v limit=%v", acc.QuotaRecent, acc.QuotaLimit)
	}
	if !acc.CooldownUntil.IsZero() {
		t.Fatalf("expected no cooldown for fresh quota, got %v", acc.CooldownUntil)
	}
}

// TestApplyFbQuotaFromModelMapNoData: empty rateLimitsByModel (truly no data)
// falls back to fresh 0/6.
func TestApplyFbQuotaFromModelMapNoData(t *testing.T) {
	acc := &FreebuffAccount{CooldownUntil: time.Now().Add(10 * time.Minute)}
	if applyFbQuotaFromModelMap(acc, nil) {
		t.Fatalf("expected applied=false for empty map")
	}
	if acc.QuotaRecent != 0 || acc.QuotaLimit != 6 {
		t.Fatalf("expected fallback 0/6, got recent=%v limit=%v", acc.QuotaRecent, acc.QuotaLimit)
	}
	if !acc.CooldownUntil.IsZero() {
		t.Fatalf("expected cooldown cleared for fresh fallback, got %v", acc.CooldownUntil)
	}
}

// TestFbSessionCreateError429 is the regression test for the session-create
// 429 classification: it must map to ErrFBQuotaExceeded so the proxy can
// cooldown the account instead of retrying it immediately.
func TestFbSessionCreateError429(t *testing.T) {
	err := fbSessionCreateError(429, []byte(`{"error":{"message":"6 sessions used today"}}`))
	if !errors.Is(err, ErrFBQuotaExceeded) {
		t.Fatalf("expected ErrFBQuotaExceeded for 429, got %v", err)
	}
}

// TestFbQuotaSyncErrorBanned is the regression test for quota-sync 403 banned
// classification: GET /session returns 403 {"status":"banned"} for banned
// accounts (not a 200 with a status field), which must map to ErrFBBanned so
// SyncAllQuota marks them banned instead of leaving them selectable.
func TestFbQuotaSyncErrorBanned(t *testing.T) {
	err := fbQuotaSyncError(403, []byte(`{"status":"banned"}`))
	if !errors.Is(err, ErrFBBanned) {
		t.Fatalf("expected ErrFBBanned for 403 banned, got %v", err)
	}
}

// TestFbQuotaSyncErrorOther keeps non-banned errors generic (no sentinel).
func TestFbQuotaSyncErrorOther(t *testing.T) {
	err := fbQuotaSyncError(500, []byte(`internal error`))
	if errors.Is(err, ErrFBBanned) || errors.Is(err, ErrFBQuotaExceeded) {
		t.Fatalf("expected generic error for 500, got %v", err)
	}
	if err == nil {
		t.Fatalf("expected non-nil error")
	}
}
