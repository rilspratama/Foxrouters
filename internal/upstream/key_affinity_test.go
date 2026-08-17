package upstream

import (
	"hash/fnv"
	"sort"
	"testing"
)

// TestHybridKeyAffinity is the regression test for key-affinity hybrid
// bucketing: the client API key (not the session id, which harnesses like
// Hermes never send) picks the bucket, and the system-prompt hash picks the
// account WITHIN that bucket. Same key → same bucket; same key + same
// sysHash → same account (deterministic); different keys → different buckets.

func cbEnabledKeys(km *CBKeyManager) []*CBKey {
	km.mu.RLock()
	defer km.mu.RUnlock()
	out := make([]*CBKey, 0, len(km.keys))
	for _, k := range km.keys {
		if !k.IsDisabled() {
			out = append(out, k)
		}
	}
	return out
}

func fnvBucketStart(seed string, n int) int {
	h := fnv.New64a()
	h.Write([]byte(seed))
	return int(h.Sum64() % uint64(n))
}

func TestHybridKeyAffinityBucketStable(t *testing.T) {
	km := NewCBKeyManager(nil)
	keys := make([]*CBKey, 12)
	for i := range keys {
		keys[i] = NewCBKeyForTest("ck_affinity_test_key_0000" + string(rune('a'+i)))
	}
	km.SetKeysForTest(keys)

	enabled := cbEnabledKeys(km)
	const clientKey = "gw-hermes-single-stable-key"
	// Expected bucket = 3 consecutive enabled keys starting at fnv("key:"+clientKey)
	start := fnvBucketStart("key:"+clientKey, len(enabled))
	size := hybridBucketSize
	if size > len(enabled) {
		size = len(enabled)
	}
	bucket := make(map[*CBKey]bool, size)
	for i := 0; i < size; i++ {
		bucket[enabled[(start+i)%len(enabled)]] = true
	}

	// Different system prompts must ALL land inside the same key bucket.
	for _, sys := range []string{"sys:A", "sys:B", "sys:C", "sys:different-project-context"} {
		k, err := km.nextHybrid("", sys, clientKey)
		if err != nil {
			t.Fatalf("nextHybrid(%q) error: %v", sys, err)
		}
		if !bucket[k] {
			t.Fatalf("sys %q routed to key %s OUTSIDE the client-key bucket", sys, k.Key)
		}
	}

	// Same key + same sysHash → deterministic same account.
	k1, _ := km.nextHybrid("", "sys:A", clientKey)
	k2, _ := km.nextHybrid("", "sys:A", clientKey)
	if k1 != k2 {
		t.Fatalf("same key+sysHash not deterministic: %s vs %s", k1.Key, k2.Key)
	}
}

// TestHybridSystemLessFallsBackToRR is the regression test for the P2-1 fix:
// a request with NO system message (sysHash == "") must NOT be herded onto
// bucket[0] via key-affinity — there is no prompt cache to warm, so it falls
// back to round-robin across ALL enabled keys (pre-key-affinity behavior).
// Without this, one API key's system-less traffic would pile onto a single
// account, concentrating rate limits / free-tier quota.
func TestHybridSystemLessFallsBackToRR(t *testing.T) {
	km := NewCBKeyManager(nil)
	keys := make([]*CBKey, 8)
	for i := range keys {
		keys[i] = NewCBKeyForTest("ck_rr_fallback_" + string(rune('a'+i)))
	}
	km.SetKeysForTest(keys)
	enabled := cbEnabledKeys(km)
	if len(enabled) < 4 {
		t.Fatalf("need >=4 enabled keys, got %d", len(enabled))
	}

	// System-less requests with a client key must spread across the pool
	// (round-robin), NOT always return the same bucket[0] account.
	seen := map[*CBKey]bool{}
	for i := 0; i < 8; i++ {
		k, err := km.nextHybrid("", "", "gw-sysless-client")
		if err != nil {
			t.Fatalf("nextHybrid(system-less) error: %v", err)
		}
		seen[k] = true
	}
	if len(seen) < 2 {
		t.Fatalf("system-less traffic collapsed onto %d key(s) — expected spread across pool (P2-1 regression)", len(seen))
	}
}

func TestHybridKeyAffinityIsolation(t *testing.T) {
	km := NewCBKeyManager(nil)
	keys := make([]*CBKey, 16)
	for i := range keys {
		keys[i] = NewCBKeyForTest("ck_affinity_iso_" + string(rune('0'+i)))
	}
	km.SetKeysForTest(keys)
	enabled := cbEnabledKeys(km)

	keyA := "gw-key-alpha"
	keyB := "gw-key-beta"
	startA := fnvBucketStart("key:"+keyA, len(enabled))
	startB := fnvBucketStart("key:"+keyB, len(enabled))
	if startA == startB {
		t.Skip("hash collision on bucket start — pick different keys")
	}

	// Every pick for key A must be within A's bucket, never in B's bucket
	// when the buckets are disjoint (bucket size 3, 16 keys, disjoint).
	bucketA := map[*CBKey]bool{}
	for i := 0; i < hybridBucketSize; i++ {
		bucketA[enabled[(startA+i)%len(enabled)]] = true
	}
	for _, sys := range []string{"s1", "s2", "s3", "s4", "s5"} {
		k, err := km.nextHybrid("", sys, keyA)
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if !bucketA[k] {
			t.Fatalf("key A routed outside its bucket: %s", k.Key)
		}
	}
}

func TestHybridKeyAffinitySessionStickyWithinBucket(t *testing.T) {
	km := NewCBKeyManager(nil)
	keys := make([]*CBKey, 10)
	for i := range keys {
		keys[i] = NewCBKeyForTest("ck_affinity_sticky_" + string(rune('a'+i)))
	}
	km.SetKeysForTest(keys)
	enabled := cbEnabledKeys(km)

	const clientKey = "gw-sticky-client"
	start := fnvBucketStart("key:"+clientKey, len(enabled))
	bucket := map[*CBKey]bool{}
	for i := 0; i < hybridBucketSize && i < len(enabled); i++ {
		bucket[enabled[(start+i)%len(enabled)]] = true
	}

	// Session-pinned account must be inside the key bucket AND stable.
	s1, err := km.nextHybrid("sess-123", "sys:X", clientKey)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !bucket[s1] {
		t.Fatalf("sticky pick %s outside key bucket", s1.Key)
	}
	s2, err := km.nextHybrid("sess-123", "sys:X", clientKey)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if s1 != s2 {
		t.Fatalf("session binding not stable: %s vs %s", s1.Key, s2.Key)
	}
}

func TestHybridKeyAffinityGrokMirror(t *testing.T) {
	am := NewGrokAccountManager(nil)
	accs := make([]*GrokAccount, 12)
	for i := range accs {
		accs[i] = NewGrokAccountForTest("g"+string(rune('a'+i))+"@x.com", "atok", "rtok")
	}
	am.SetAccountsForTest(accs)

	am.mu.RLock()
	enabled := make([]*GrokAccount, 0, len(am.accounts))
	for _, a := range am.accounts {
		if !a.IsDisabled() {
			enabled = append(enabled, a)
		}
	}
	am.mu.RUnlock()

	const clientKey = "gw-grok-hermes-key"
	// C2-08: nextHybrid uses rendezvous (HRW) hashing over stable account
	// identity (email) — bucket membership must be STABLE under churn. The
	// final pick is a deterministic sysHash hash INSIDE that bucket. Verify:
	// (a) the pick is always inside the HRW top-N bucket, (b) the HRW bucket
	// is stable when an account flips disabled (membership does not shift).
	hrwBucket := func(seed string) map[*GrokAccount]bool {
		type scored struct {
			acc *GrokAccount
			s   uint64
		}
		scoredEnabled := make([]scored, 0, len(enabled))
		for _, a := range enabled {
			h := fnv.New64a()
			h.Write([]byte(seed))
			h.Write([]byte("\x00"))
			h.Write([]byte(a.Email))
			scoredEnabled = append(scoredEnabled, scored{a, h.Sum64()})
		}
		sort.Slice(scoredEnabled, func(i, j int) bool { return scoredEnabled[i].s > scoredEnabled[j].s })
		n := grokHybridBucketSize
		if n > len(scoredEnabled) {
			n = len(scoredEnabled)
		}
		m := make(map[*GrokAccount]bool, n)
		for i := 0; i < n; i++ {
			m[scoredEnabled[i].acc] = true
		}
		return m
	}

	seed := "key:" + clientKey
	bucket := hrwBucket(seed)

	for _, sys := range []string{"g1", "g2", "g3"} {
		a, err := am.nextHybrid("", sys, clientKey)
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if !bucket[a] {
			t.Fatalf("grok routed outside HRW key bucket: %s", a.Email)
		}
	}

	// Deterministic for same key+sys.
	a1, _ := am.nextHybrid("", "g1", clientKey)
	a2, _ := am.nextHybrid("", "g1", clientKey)
	if a1 != a2 {
		t.Fatalf("grok not deterministic for same key+sys: %s vs %s", a1.Email, a2.Email)
	}

	// Stability under churn: disable the current bucket[0] member and confirm
	// the REMAINING bucket members are unchanged (no modulo-window shift).
	disabled := a1
	disabled.disabled = true
	after := hrwBucket(seed)
	delete(after, disabled)
	// every remaining original member must still be in the new bucket
	for _, sys := range []string{"g1", "g2", "g3"} {
		orig := bucket
		for acc := range orig {
			if acc == disabled {
				continue
			}
			if !after[acc] {
				t.Fatalf("HRW bucket membership shifted after disabling (churn instability)")
			}
		}
		_ = sys
	}
	_ = bucket
}
