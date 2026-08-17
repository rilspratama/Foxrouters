package upstream

import (
	"hash/fnv"
	"strconv"
	"sync"
	"time"
)

// cacheTemperature tracks per-account cache-hit rates for content prefixes
// (sysHash). Hybrid/content-hash selectors use it to prefer the account whose
// upstream prompt cache is warmest for the current prefix — the gateway
// equivalent of a harness pinning prompt_cache_key, except the gateway has
// cross-account visibility no single harness can have.
//
// Data comes from real responses: each 2xx records the cache hit % (from
// usage.prompt_cache_hit_tokens / prompt_tokens_details.cached_tokens) for
// (account, sysHash). Kept in-memory (hot path, bounded by account×prefix);
// not persisted across restarts — temperature rebuilds within minutes.
const (
	// cacheWarmThreshold: minimum EMA hit % for an account to be preferred.
	cacheWarmThreshold = 50.0
	// cacheTempMaxAge: prefix entries older than this are considered cold.
	cacheTempMaxAge = 30 * time.Minute
)

type cacheTemperature struct {
	mu    sync.Mutex
	byKey map[string]map[string]*cacheTempEntry // accountKey -> prefix -> entry
}

type cacheTempEntry struct {
	ema      float64 // exponential moving average of cache hit % (0..100)
	samples  int
	lastSeen time.Time
}

func newCacheTemperature() *cacheTemperature {
	return &cacheTemperature{byKey: make(map[string]map[string]*cacheTempEntry)}
}

// record updates the EMA for (accountKey, prefix). hitPct < 0 (unknown usage)
// is ignored. Alpha starts at 1.0 for the first sample and floors at 0.3 so a
// few observations still move the estimate, but single outliers cannot spike it.
// The prefix is stored as a short FNV hash — raw system prompts never live in
// this map (bounded memory, no prompt-content leakage via snapshot).
func (t *cacheTemperature) record(accountKey, prefix string, hitPct float64) {
	if hitPct < 0 || prefix == "" || accountKey == "" {
		return
	}
	key := prefixHash(prefix)
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.byKey[accountKey]
	if m == nil {
		m = make(map[string]*cacheTempEntry)
		t.byKey[accountKey] = m
	}
	e := m[key]
	if e == nil {
		m[key] = &cacheTempEntry{ema: hitPct, samples: 1, lastSeen: time.Now()}
		return
	}
	alpha := 1.0 / float64(e.samples+1)
	if alpha < 0.3 {
		alpha = 0.3
	}
	e.ema = (1-alpha)*e.ema + alpha*hitPct
	e.samples++
	e.lastSeen = time.Now()
}

// best returns the accountKey among candidates whose recorded cache for
// prefix is warmest, provided it is warm enough (ema >= warmThreshold) and
// fresh (seen within maxAge). ok=false when no candidate qualifies — the
// caller falls back to its deterministic hash pick.
func (t *cacheTemperature) best(candidates []string, prefix string, warmThreshold float64, maxAge time.Duration) (string, bool) {
	if len(candidates) == 0 || prefix == "" {
		return "", false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	key := prefixHash(prefix)
	bestKey, bestEma := "", -1.0
	for _, k := range candidates {
		m := t.byKey[k]
		if m == nil {
			continue
		}
		e := m[key]
		if e == nil || e.ema < warmThreshold {
			continue
		}
		if now.Sub(e.lastSeen) > maxAge {
			continue
		}
		if e.ema > bestEma {
			bestKey, bestEma = k, e.ema
		}
	}
	if bestKey == "" {
		return "", false
	}
	return bestKey, true
}

// dropAccount removes all temperature entries for an account (used when an
// account is removed from the pool — stale rows would linger until prune).
func (t *cacheTemperature) dropAccount(accountKey string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.byKey, accountKey)
}

// prune drops prefixes whose entries are older than maxAge, bounding memory
// growth when conversations end. Safe to call from a janitor goroutine.
func (t *cacheTemperature) prune(maxAge time.Duration) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, m := range t.byKey {
		for p, e := range m {
			if now.Sub(e.lastSeen) > maxAge {
				delete(m, p)
			}
		}
		if len(m) == 0 {
			delete(t.byKey, k)
		}
	}
}

// snapshot returns a copy of the temperature map for observability/debug.
// Prefix keys are already short hashes (raw system prompts never stored).
func (t *cacheTemperature) snapshot() map[string]map[string]CacheTempSnap {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]map[string]CacheTempSnap, len(t.byKey))
	for k, m := range t.byKey {
		pm := make(map[string]CacheTempSnap, len(m))
		for p, e := range m {
			pm[p] = CacheTempSnap{EMA: e.ema, Samples: e.samples, LastSeen: e.lastSeen}
		}
		out[k] = pm
	}
	return out
}

// CacheTempSnap is the serializable view of one (account, prefix) entry.
type CacheTempSnap struct {
	EMA      float64   `json:"ema"`
	Samples  int       `json:"samples"`
	LastSeen time.Time `json:"last_seen"`
}

// prefixHash maps a content prefix (model|first system message) to a short
// deterministic key — FNV-1a 64 in hex. Raw prompt text is never used as a
// map key or emitted in snapshots; collisions are harmless for this use
// (identical prefix always hashes identically, which is all matching needs).
func prefixHash(prefix string) string {
	h := fnv.New64a()
	h.Write([]byte(prefix))
	return strconv.FormatUint(h.Sum64(), 16)
}
