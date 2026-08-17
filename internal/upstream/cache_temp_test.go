package upstream

import (
	"fmt"
	"hash/fnv"
	"sort"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// cacheTemperature unit tests
// ---------------------------------------------------------------------------

func TestCacheTempRecordAndBest(t *testing.T) {
	tm := newCacheTemperature()
	tm.record("k1", "sys:A", 90)
	tm.record("k2", "sys:A", 40) // below warm threshold
	tm.record("k3", "sys:B", 80) // different prefix

	best, ok := tm.best([]string{"k1", "k2", "k3"}, "sys:A", cacheWarmThreshold, time.Hour)
	if !ok || best != "k1" {
		t.Fatalf("expected warmest k1, got %q ok=%v", best, ok)
	}
	// cold prefix → no pick (caller falls back to deterministic hash)
	if _, ok := tm.best([]string{"k1", "k2", "k3"}, "sys:COLD", cacheWarmThreshold, time.Hour); ok {
		t.Fatalf("cold prefix must not route via temperature")
	}
}

func TestCacheTempBelowThreshold(t *testing.T) {
	tm := newCacheTemperature()
	tm.record("k1", "sys:A", 30)
	if _, ok := tm.best([]string{"k1"}, "sys:A", cacheWarmThreshold, time.Hour); ok {
		t.Fatalf("ema 30 below threshold 50 must not qualify")
	}
}

func TestCacheTempEMA(t *testing.T) {
	tm := newCacheTemperature()
	tm.record("k1", "sys:A", 100)
	tm.record("k1", "sys:A", 0)
	// alpha=0.5 after 2 samples → ema = 50 (still qualifies at 50)
	if best, ok := tm.best([]string{"k1"}, "sys:A", cacheWarmThreshold, time.Hour); !ok || best != "k1" {
		t.Fatalf("ema 50 should qualify, got ok=%v best=%q", ok, best)
	}
	tm.record("k1", "sys:A", 0)
	// alpha=1/3 → ema = (2/3)*50 = 33.3 → below threshold now
	if _, ok := tm.best([]string{"k1"}, "sys:A", cacheWarmThreshold, time.Hour); ok {
		t.Fatalf("ema 33.3 must not qualify")
	}
}

func TestCacheTempStale(t *testing.T) {
	tm := newCacheTemperature()
	tm.record("k1", "sys:A", 95)
	// whitebox: age the entry beyond maxAge
	tm.mu.Lock()
	tm.byKey["k1"][prefixHash("sys:A")].lastSeen = time.Now().Add(-2 * cacheTempMaxAge)
	tm.mu.Unlock()
	if _, ok := tm.best([]string{"k1"}, "sys:A", cacheWarmThreshold, cacheTempMaxAge); ok {
		t.Fatalf("stale entry must not qualify")
	}
}

func TestCacheTempPrune(t *testing.T) {
	tm := newCacheTemperature()
	tm.record("k1", "sys:A", 95)
	tm.record("k1", "sys:NEW", 95)
	tm.mu.Lock()
	tm.byKey["k1"][prefixHash("sys:A")].lastSeen = time.Now().Add(-2 * cacheTempMaxAge)
	tm.mu.Unlock()
	tm.prune(cacheTempMaxAge)
	tm.mu.Lock()
	_, hasA := tm.byKey["k1"][prefixHash("sys:A")]
	_, hasNew := tm.byKey["k1"][prefixHash("sys:NEW")]
	tm.mu.Unlock()
	if hasA {
		t.Fatalf("prune must drop stale prefix")
	}
	if !hasNew {
		t.Fatalf("prune must keep fresh prefix")
	}
}

// TestCacheTempConcurrent exercises record/best/prune/snapshot concurrently
// (run under -race). Covers the audit gap: no test previously poked the map
// from multiple goroutines at once.
func TestCacheTempConcurrent(t *testing.T) {
	tm := newCacheTemperature()
	done := make(chan struct{})
	for g := 0; g < 8; g++ {
		go func(g int) {
			for i := 0; i < 200; i++ {
				prefix := fmt.Sprintf("sys:g%d:%d", g, i%7)
				tm.record("k"+string(rune('a'+g%4)), prefix, float64(i%100))
				if i%20 == 0 {
					tm.best([]string{"ka", "kb", "kc", "kd"}, prefix, cacheWarmThreshold, time.Hour)
				}
				if i%50 == 0 {
					tm.prune(time.Minute)
					tm.snapshot()
				}
			}
			done <- struct{}{}
		}(g)
	}
	for g := 0; g < 8; g++ {
		<-done
	}
}

// ---------------------------------------------------------------------------
// Selector integration: hybrid picks the warmest account inside the key bucket
// ---------------------------------------------------------------------------

func TestHybridCacheTemperatureRouting(t *testing.T) {
	km := NewCBKeyManager(nil)
	keys := make([]*CBKey, 12)
	for i := range keys {
		keys[i] = NewCBKeyForTest("ck_tmp_000000000" + string(rune('a'+i)))
	}
	km.SetKeysForTest(keys)
	enabled := cbEnabledKeys(km)

	const clientKey = "gw-cache-temp-client"
	start := fnvBucketStart("key:"+clientKey, len(enabled))
	size := hybridBucketSize
	if size > len(enabled) {
		size = len(enabled)
	}
	bucket := make([]*CBKey, 0, size)
	for i := 0; i < size; i++ {
		bucket = append(bucket, enabled[(start+i)%len(enabled)])
	}
	if len(bucket) < 2 {
		t.Skip("bucket too small for this test")
	}

	// deterministic fnv pick for "sys:HOT" inside the bucket
	sh := fnv.New64a()
	sh.Write([]byte("sys:HOT"))
	det := bucket[sh.Sum64()%uint64(len(bucket))]
	var warmKey *CBKey
	for _, k := range bucket {
		if k != det {
			warmKey = k
			break
		}
	}

	// Warm a different bucket key for "sys:HOT" → hybrid must prefer it.
	km.RecordCacheHit(warmKey.Key, "sys:HOT", 97)
	got, err := km.nextHybrid("", "sys:HOT", clientKey)
	if err != nil {
		t.Fatalf("nextHybrid error: %v", err)
	}
	if got != warmKey {
		t.Fatalf("expected warm key %s, got %s (deterministic pick was %s)", warmKey.Key, got.Key, det.Key)
	}

	// Unknown prefix: no temperature → deterministic fnv pick.
	sh2 := fnv.New64a()
	sh2.Write([]byte("sys:COLD"))
	wantCold := bucket[sh2.Sum64()%uint64(len(bucket))]
	coldKey, err := km.nextHybrid("", "sys:COLD", clientKey)
	if err != nil {
		t.Fatalf("nextHybrid error: %v", err)
	}
	if coldKey != wantCold {
		t.Fatalf("cold prefix must fall back to deterministic pick: got %s want %s", coldKey.Key, wantCold.Key)
	}

	// Session binding still wins over temperature (sticky is more specific).
	s1, err := km.nextHybrid("sess-777", "sys:HOT", clientKey)
	if err != nil {
		t.Fatalf("nextHybrid error: %v", err)
	}
	s2, err := km.nextHybrid("sess-777", "sys:HOT", clientKey)
	if err != nil {
		t.Fatalf("nextHybrid error: %v", err)
	}
	if s1 != s2 {
		t.Fatalf("session binding must stay stable under temperature routing: %s vs %s", s1.Key, s2.Key)
	}
}

func TestHybridCacheTemperatureGrokMirror(t *testing.T) {
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

	const clientKey = "gw-grok-temp-client"
	// C2-08: nextHybrid uses rendezvous (HRW) hashing — the hybrid bucket is
	// the top-N HRW accounts. Compute it the same way the impl does so the
	// cache-temp warm-pick assertion targets a member of that bucket.
	type scored struct {
		acc *GrokAccount
		s   uint64
	}
	scoredEnabled := make([]scored, 0, len(enabled))
	for _, acc := range enabled {
		h := fnv.New64a()
		h.Write([]byte("key:" + clientKey))
		h.Write([]byte("\x00"))
		h.Write([]byte(acc.Email))
		scoredEnabled = append(scoredEnabled, scored{acc, h.Sum64()})
	}
	sort.Slice(scoredEnabled, func(i, j int) bool { return scoredEnabled[i].s > scoredEnabled[j].s })
	size := grokHybridBucketSize
	if size > len(scoredEnabled) {
		size = len(scoredEnabled)
	}
	bucket := make([]*GrokAccount, 0, size)
	for i := 0; i < size; i++ {
		bucket = append(bucket, scoredEnabled[i].acc)
	}
	if len(bucket) < 2 {
		t.Skip("bucket too small for this test")
	}

	sh := fnv.New64a()
	sh.Write([]byte("sys:GROK-HOT"))
	det := bucket[sh.Sum64()%uint64(len(bucket))]
	var warmAcc *GrokAccount
	for _, a := range bucket {
		if a != det {
			warmAcc = a
			break
		}
	}

	am.RecordCacheHit(warmAcc.Email, "sys:GROK-HOT", 95)
	got, err := am.nextHybrid("", "sys:GROK-HOT", clientKey)
	if err != nil {
		t.Fatalf("grok nextHybrid error: %v", err)
	}
	if got != warmAcc {
		t.Fatalf("grok: expected warm account %s, got %s", warmAcc.Email, got.Email)
	}
}
