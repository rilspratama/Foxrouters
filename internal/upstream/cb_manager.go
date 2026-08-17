package upstream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"foxrouters/internal/db"
	"hash/fnv"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type CBKeyManager struct {
	keys []*CBKey
	mu   sync.RWMutex
	next int
	db   *db.Store

	// sticky sessions: sessionID → bound key (prompt-cache locality)
	sticky   map[string]*stickyBinding
	stickyMu sync.Mutex

	// cacheTemp: per-account cache hit rates by content prefix (sysHash).
	// Hybrid/content-hash selection prefers the warmest account.
	cacheTemp *cacheTemperature
}

type stickyBinding struct {
	key      *CBKey
	lastSeen time.Time
}

// stickyTTL is how long a session binding survives without traffic.
const stickyTTL = 30 * time.Minute

func NewCBKeyManager(store *db.Store) *CBKeyManager {
	km := &CBKeyManager{keys: make([]*CBKey, 0), db: store, sticky: make(map[string]*stickyBinding), cacheTemp: newCacheTemperature()}
	go km.stickyJanitor()
	return km
}

// RecordCacheHit feeds the cache-temperature map from a real response.
// hitPct is the upstream-reported cache hit % for (key, sysHash prefix).
func (km *CBKeyManager) RecordCacheHit(keyKey, sysHash string, hitPct float64) {
	if km.cacheTemp != nil {
		km.cacheTemp.record(keyKey, sysHash, hitPct)
	}
}

// SnapshotCacheTemp returns the per-account cache temperature map (debug).
func (km *CBKeyManager) SnapshotCacheTemp() map[string]map[string]CacheTempSnap {
	if km == nil || km.cacheTemp == nil {
		return nil
	}
	return km.cacheTemp.snapshot()
}

// stickyJanitor evicts idle bindings so the map doesn't grow unbounded.
// Also prunes the cache-temperature map (same 5m tick) so per-prefix entries
// don't accumulate forever as conversations churn.
func (km *CBKeyManager) stickyJanitor() {
	if km.sticky == nil {
		return
	}
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-stickyTTL)
		km.stickyMu.Lock()
		for sid, b := range km.sticky {
			if b.lastSeen.Before(cutoff) {
				delete(km.sticky, sid)
			}
		}
		km.stickyMu.Unlock()
		if km.cacheTemp != nil {
			km.cacheTemp.prune(cacheTempMaxAge)
		}
	}
}

// NextSticky returns the key bound to sessionID. If unbound, or the bound key
// got disabled (credits exhausted etc.), it binds the next round-robin key.
// All requests with the same sessionID hit the same upstream account →
// CodeBuddy prompt-cache stays hot instead of regenerating per request.
func (km *CBKeyManager) NextSticky(sessionID string) (*CBKey, error) {
	if sessionID == "" || km.sticky == nil {
		return km.Next()
	}
	km.stickyMu.Lock()
	if b, ok := km.sticky[sessionID]; ok {
		if !b.key.IsDisabled() {
			b.lastSeen = time.Now()
			km.stickyMu.Unlock()
			return b.key, nil
		}
		delete(km.sticky, sessionID) // bound key died — rebind below
	}
	km.stickyMu.Unlock()

	key, err := km.Next()
	if err != nil {
		return nil, err
	}
	km.stickyMu.Lock()
	km.sticky[sessionID] = &stickyBinding{key: key, lastSeen: time.Now()}
	km.stickyMu.Unlock()
	return key, nil
}

// UnbindSticky drops a session binding (e.g. after permanent disable so the
// next request rebinds to a fresh key instead of the dead one).
func (km *CBKeyManager) UnbindSticky(sessionID string, key *CBKey) {
	if sessionID == "" || km.sticky == nil {
		return
	}
	km.stickyMu.Lock()
	if b, ok := km.sticky[sessionID]; ok && b.key == key {
		delete(km.sticky, sessionID)
	}
	km.stickyMu.Unlock()
}

// StickyCount reports active session bindings (dashboard/debug).
func (km *CBKeyManager) StickyCount() int {
	km.stickyMu.Lock()
	defer km.stickyMu.Unlock()
	return len(km.sticky)
}

// ---------------------------------------------------------------------------
// Key selection modes (router-level strategy, runtime-configurable)
// ---------------------------------------------------------------------------

// CBSelectorMode picks how ProxyCodeBuddy chooses the upstream key.
type CBSelectorMode string

const (
	// SelectorRR — classic round-robin over enabled keys (no cache locality).
	SelectorRR CBSelectorMode = "rr"
	// SelectorSticky — session-id header binds conversation to one key until
	// it dies (per-conversation cache locality; different sessions may land on
	// different keys, re-caching the shared system prompt each time).
	SelectorSticky CBSelectorMode = "sticky"
	// SelectorContentHash — hash(model + first system message) deterministically
	// maps to one key: every session sharing the same system prompt lands on
	// the same account → giant shared prefix cached once.
	SelectorContentHash CBSelectorMode = "content-hash"
	// SelectorHybrid — content-hash picks a small bucket (~3 keys); session-id
	// sticks to one key inside the bucket. Dead keys rebind within the bucket,
	// keeping the shared system-prompt cache warm.
	SelectorHybrid CBSelectorMode = "hybrid"
)

// hybridBucketSize is how many enabled keys form one hybrid bucket.
const hybridBucketSize = 3

// selectorMode holds the active mode (atomic for lock-free hot-path reads).
var selectorMode atomic.Value // stores CBSelectorMode

func init() {
	m := CBSelectorMode(os.Getenv("CB_SELECTOR_MODE"))
	if !validSelectorMode(m) {
		m = SelectorSticky // default: sticky sessions (prompt-cache locality)
	}
	selectorMode.Store(m)
}

func validSelectorMode(m CBSelectorMode) bool {
	switch m {
	case SelectorRR, SelectorSticky, SelectorContentHash, SelectorHybrid:
		return true
	}
	return false
}

// GetSelectorMode returns the active CB key selection mode.
func GetSelectorMode() CBSelectorMode { return selectorMode.Load().(CBSelectorMode) }

// SetSelectorMode validates + stores the mode and persists it to Redis
// (cb:config hash) so restarts keep the operator's choice.
func SetSelectorMode(store *db.Store, m CBSelectorMode) error {
	if !validSelectorMode(m) {
		return fmt.Errorf("invalid selector mode %q (valid: rr|sticky|content-hash|hybrid)", m)
	}
	selectorMode.Store(m)
	if store != nil {
		if err := store.SetCBConfig("selector_mode", string(m)); err != nil {
			slog.Warn("selector mode persist failed", "module", "cb", "error", err)
		}
	}
	slog.Info("cb selector mode changed", "module", "cb", "mode", m)
	return nil
}

// LoadSelectorMode restores the persisted mode from Redis (called at startup).
func LoadSelectorMode(store *db.Store) {
	if store == nil {
		return
	}
	if v, err := store.GetCBConfig("selector_mode"); err == nil && validSelectorMode(CBSelectorMode(v)) {
		selectorMode.Store(CBSelectorMode(v))
		slog.Info("cb selector mode restored", "module", "cb", "mode", v)
	}
}

// NextForMode selects a key according to the active mode.
//   - sessionID:  from x-session-id/x-conversation-id/x-chat-id header (may be "")
//   - sysHash:    hash of model + first system message content (may be "")
//   - clientKey:  full gateway API key from auth context — the client identity
//     signal. Harnesses like Hermes don't send a session ID, so the key is the
//     only stable per-client marker: hybrid mode buckets BY KEY so all of one
//     client's conversations warm the same small account bucket.
func (km *CBKeyManager) NextForMode(mode CBSelectorMode, sessionID, sysHash, clientKey string) (*CBKey, error) {
	switch mode {
	case SelectorRR:
		return km.Next()
	case SelectorContentHash:
		return km.nextByHash(sysHash)
	case SelectorHybrid:
		return km.nextHybrid(sessionID, sysHash, clientKey)
	case SelectorSticky:
		fallthrough
	default:
		if sessionID != "" {
			return km.NextSticky(sessionID)
		}
		return km.Next()
	}
}

// nextByHash: deterministic key from sysHash (FNV-1a over enabled keys).
// All sessions with the same system prompt land on the same account.
func (km *CBKeyManager) nextByHash(sysHash string) (*CBKey, error) {
	km.mu.RLock()
	enabled := make([]*CBKey, 0, len(km.keys))
	for _, k := range km.keys {
		if !k.IsDisabled() {
			enabled = append(enabled, k)
		}
	}
	km.mu.RUnlock()
	if len(enabled) == 0 {
		return nil, fmt.Errorf("all cb keys disabled")
	}
	if sysHash == "" {
		return km.Next()
	}
	h := fnv.New64a()
	h.Write([]byte(sysHash))
	return enabled[h.Sum64()%uint64(len(enabled))], nil
}

// nextHybrid: the client API key picks a bucket of hybridBucketSize enabled
// keys (key-affinity — one harness key keeps ALL its conversations in one
// bucket so the account-level prompt cache warms across sessions); session-id
// sticks to one key inside the bucket; without a session id the sysHash picks
// the key (shared system prompt → same account). Rebinding happens only inside
// the bucket → the shared system-prompt cache stays warm.
func (km *CBKeyManager) nextHybrid(sessionID, sysHash, clientKey string) (*CBKey, error) {
	km.mu.RLock()
	enabled := make([]*CBKey, 0, len(km.keys))
	for _, k := range km.keys {
		if !k.IsDisabled() {
			enabled = append(enabled, k)
		}
	}
	km.mu.RUnlock()
	if len(enabled) == 0 {
		return nil, fmt.Errorf("all cb keys disabled")
	}
	// No system message (sysHash == ""): nothing to warm in an upstream prompt
	// cache, so key-affinity bucketing buys nothing and would herd ALL
	// system-less traffic from one API key onto a single account (bucket[0]),
	// concentrating rate limits / free-tier quota. Fall back to RR/sticky like
	// the pre-key-affinity behavior. clientKey is deliberately ignored here —
	// auth always sets it, so the old `&& clientKey == ""` guard was dead code.
	if sysHash == "" {
		if sessionID != "" {
			return km.NextSticky(sessionID)
		}
		return km.Next()
	}

	// Bucket seed: the client API key wins (key-affinity). sysHash is the
	// defensive fallback (auth always sets client_key, so this is unreachable
	// in practice).
	bucketSeed := sysHash
	if clientKey != "" {
		bucketSeed = "key:" + clientKey
	}
	h := fnv.New64a()
	h.Write([]byte(bucketSeed))
	start := int(h.Sum64() % uint64(len(enabled)))
	bucket := make([]*CBKey, 0, hybridBucketSize)
	for i := 0; i < hybridBucketSize && i < len(enabled); i++ {
		bucket = append(bucket, enabled[(start+i)%len(enabled)])
	}

	// Session binding within bucket (reuse sticky map — but only accept the
	// bound key if it is in THIS bucket; otherwise rebind into the bucket).
	if sessionID != "" {
		km.stickyMu.Lock()
		if b, ok := km.sticky[sessionID]; ok && !b.key.IsDisabled() {
			for _, k := range bucket {
				if k == b.key {
					b.lastSeen = time.Now()
					km.stickyMu.Unlock()
					return k, nil
				}
			}
		}
		delete(km.sticky, sessionID)
		// bind first bucket key (deterministic per session: hash sessionID)
		sh := fnv.New64a()
		sh.Write([]byte(sessionID))
		pick := bucket[sh.Sum64()%uint64(len(bucket))]
		km.sticky[sessionID] = &stickyBinding{key: pick, lastSeen: time.Now()}
		km.stickyMu.Unlock()
		return pick, nil
	}

	// No session id: prefer the account in this bucket whose upstream prompt
	// cache is warmest for this prefix (cache-temperature-aware routing —
	// real hit rates recorded from previous responses). Falls back to the
	// deterministic sysHash pick when nothing is warm yet.
	if sysHash != "" && km.cacheTemp != nil {
		cands := make([]string, 0, len(bucket))
		for _, k := range bucket {
			cands = append(cands, k.Key)
		}
		if best, ok := km.cacheTemp.best(cands, sysHash, cacheWarmThreshold, cacheTempMaxAge); ok {
			for _, k := range bucket {
				if k.Key == best {
					return k, nil
				}
			}
		}
	}

	// No session id: pick deterministically by system-prompt hash inside the
	// key's bucket → all sessions sharing a system prompt share one account.
	if sysHash != "" {
		sh := fnv.New64a()
		sh.Write([]byte(sysHash))
		return bucket[sh.Sum64()%uint64(len(bucket))], nil
	}
	return bucket[0], nil
}

// SetKeysForTest replaces the internal slice. Whitebox tests only.
func (km *CBKeyManager) SetKeysForTest(keys []*CBKey) {
	km.mu.Lock()
	defer km.mu.Unlock()
	km.keys = keys
}

// LoadFromRedis loads all CB keys from Redis (single source of truth).
// If Redis is empty (fresh deploy), falls back to file/env as bootstrap seed,
// then persists those keys to Redis so subsequent starts are file-independent.
// Existing entries without cred_type default to api_key (backward compatible).
func (km *CBKeyManager) LoadFromRedis() error {
	redisState, err := km.db.LoadCBKeys()
	if err != nil {
		return fmt.Errorf("cb keys load: %w", err)
	}

	if len(redisState) > 0 {
		// Build all keys into a local slice, then swap under lock — avoids
		// data race with hot-path readers (Next, ResolveKey, nextByHash…)
		// that hold km.mu.RLock while iterating km.keys.
		loaded := make([]*CBKey, 0, len(redisState))
		for apiKey, state := range redisState {
			key := &CBKey{Key: apiKey, db: km.db, CredType: CBAuthAPIKey}
			// cred_type defaults to api_key for legacy entries
			if ct := state["cred_type"]; ct == string(CBAuthOAuth) {
				key.CredType = CBAuthOAuth
				key.AccessToken = state["access_token"]
				key.RefreshToken = state["refresh_token"]
				key.Email = state["email"]
				if key.Email == "" {
					key.Email = apiKey // Key field is email for OAuth
				}
				if v := state["expires_at"]; v != "" {
					if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
						key.ExpiresAt = time.Unix(n, 0)
					}
				}
			}
			if cu, err := strconv.ParseFloat(state["credits_used"], 64); err == nil {
				key.creditsUsed = cu
			}
			if tr, err := strconv.ParseInt(state["total_requests"], 10, 64); err == nil {
				key.totalReqs = tr
			}
			if state["disabled"] == "true" || state["disabled"] == "1" {
				key.disabled = true
				key.disabledReason = state["disabled_reason"]
				if v := state["disabled_at"]; v != "" {
					if n, err := strconv.ParseInt(v, 10, 64); err == nil {
						if n <= 0 {
							key.disabledAt = time.Time{}
						} else {
							key.disabledAt = time.Unix(n, 0)
						}
					} else {
						key.disabledAt = time.Time{}
					}
				} else {
					key.disabledAt = time.Time{}
				}
				// I2: legacy rows (pre-provenance) carry empty reason + zero
				// timestamp — they were meter-driven disables. Tag them so the
				// H3 auto-lift restores the pre-upgrade self-healing behavior.
				// Operator disables always persisted a reason via DisableKey.
				if key.disabled && key.disabledReason == "" && key.disabledAt.IsZero() {
					key.disabledReason = cbMeterDisableReason
				}
			}
			// Meter fields (optional — missing = never synced, fallback CB_CREDIT_LIMIT)
			if v := state["credit_limit"]; v != "" {
				if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
					key.creditLimit = f
				}
			}
			if v := state["credits_remain"]; v != "" {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					key.creditsRemain = f
				}
			}
			key.packageName = state["package_name"]
			key.cycleEnd = state["cycle_end"]
			if v := state["meter_status"]; v != "" {
				if n, err := strconv.Atoi(v); err == nil {
					key.meterStatus = n
				}
			}
			if v := state["meter_synced_at"]; v != "" {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
					key.meterSyncedAt = time.Unix(n, 0)
				}
			}
			loaded = append(loaded, key)
		}
		km.mu.Lock()
		km.keys = loaded
		km.mu.Unlock()
		slog.Info("loaded keys from Redis", "module", "cb", "count", len(loaded))
		return nil
	}

	// Bootstrap from file/env (first run only)
	keysStr := os.Getenv("CB_API_KEYS")
	if keysStr == "" {
		keysStr = os.Getenv("CB_API_KEY")
	}
	if keysStr == "" {
		if v := os.Getenv("CB_KEY_FILE"); v != "" {
			if data, err := os.ReadFile(v); err == nil {
				keysStr = strings.TrimSpace(string(data))
			}
		} else {
			if data, err := os.ReadFile("./codebuddy-key.txt"); err == nil {
				keysStr = strings.TrimSpace(string(data))
			}
		}
	}
	if keysStr == "" {
		slog.Warn("no API keys found (Redis empty, no file/env bootstrap)", "module", "cb")
		return nil
	}

	seedCount := 0
	for _, k := range strings.FieldsFunc(keysStr, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	}) {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		key := &CBKey{Key: k, CredType: CBAuthAPIKey, db: km.db}
		km.keys = append(km.keys, key)
		if km.db != nil {
			saveCBKey(km.db, key.toDTO())
		}
		seedCount++
	}
	slog.Info("bootstrapped keys from file/env → Redis (first run)", "module", "cb", "count", seedCount)
	return nil
}

func (km *CBKeyManager) Next() (*CBKey, error) {
	km.mu.Lock()
	defer km.mu.Unlock()
	if len(km.keys) == 0 {
		return nil, fmt.Errorf("no cb keys")
	}
	for i := 0; i < len(km.keys); i++ {
		idx := (km.next + i) % len(km.keys)
		key := km.keys[idx]
		key.mu.Lock()
		if key.disabled {
			key.mu.Unlock()
			continue
		}
		key.mu.Unlock()
		km.next = (idx + 1) % len(km.keys)
		return key, nil
	}
	return nil, fmt.Errorf("all cb keys disabled")
}

func (km *CBKeyManager) Len() int {
	km.mu.RLock()
	defer km.mu.RUnlock()
	return len(km.keys)
}

func (km *CBKeyManager) GetAll() []*CBKey {
	km.mu.RLock()
	defer km.mu.RUnlock()
	r := make([]*CBKey, len(km.keys))
	copy(r, km.keys)
	return r
}

// ResolveKey resolves a masked key (e.g. "ck_abcde...wxyz"), full key, or
// OAuth email to the full Key field string. Returns empty string if not found.
func (km *CBKeyManager) ResolveKey(maskedOrFull string) string {
	km.mu.RLock()
	defer km.mu.RUnlock()
	for _, k := range km.keys {
		// Snapshot mutable fields under k.mu to avoid race with
		// AddOAuthAccount which writes CredType/Email under k.mu only.
		k.mu.RLock()
		keyVal := k.Key
		ct := k.CredType
		email := k.Email
		k.mu.RUnlock()

		if keyVal == maskedOrFull {
			return keyVal
		}
		// OAuth: also match by Email field
		if ct == CBAuthOAuth && email == maskedOrFull {
			return keyVal
		}
		// Check masked form: first 8 + "..." + last 4 (API keys)
		if len(keyVal) > 12 {
			masked := keyVal[:8] + "..." + keyVal[len(keyVal)-4:]
			if masked == maskedOrFull {
				return keyVal
			}
		}
	}
	return ""
}

// AddKey hot-imports a CodeBuddy API key into the runtime pool + Redis.
func (km *CBKeyManager) AddKey(apiKey string) (added bool, total int) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return false, km.Len()
	}
	km.mu.Lock()
	for _, existing := range km.keys {
		if existing.Key == apiKey {
			n := len(km.keys)
			km.mu.Unlock()
			return false, n
		}
	}
	key := &CBKey{Key: apiKey, CredType: CBAuthAPIKey, db: km.db}
	km.keys = append(km.keys, key)
	total = len(km.keys)
	km.mu.Unlock()
	if km.db != nil {
		saveCBKey(km.db, key.toDTO())
	}
	return true, total
}

// AddOAuthAccount hot-imports a CodeBuddy OAuth account (dedup by email).
// Key field = email for OAuth entries.
func (km *CBKeyManager) AddOAuthAccount(email, accessToken, refreshToken string, expiresAt time.Time) (added bool, total int) {
	email = strings.TrimSpace(email)
	accessToken = strings.TrimSpace(accessToken)
	refreshToken = strings.TrimSpace(refreshToken)
	if email == "" || accessToken == "" || refreshToken == "" {
		return false, km.Len()
	}

	// Eager refresh: if the supplied AT is already expired (or within the
	// 10-minute refresh buffer), try refreshing via RT now so the account
	// is in a usable state before it enters the pool.  We perform this
	// BEFORE acquiring km.mu to avoid blocking the hot path; if refresh
	// succeeds we use the fresh AT/RT, otherwise we fall through and store
	// the supplied tokens as-is (the 401 path / pre-warm worker can retry
	// later, and permanent disable handles truly dead RTs).
	if expiresAt.Before(time.Now().Add(REFRESH_BUFFER)) {
		probe := &CBKey{
			Key:          email,
			CredType:     CBAuthOAuth,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			ExpiresAt:    expiresAt,
			Email:        email,
		}
		if refreshedAt, err := tryEagerRefresh(probe); err == nil && !refreshedAt.IsZero() {
			accessToken = probe.AccessToken
			refreshToken = probe.RefreshToken
			expiresAt = refreshedAt
			slog.Info("oauth eager refresh ok", "module", "cb", "email", email)
		} else if err != nil {
			slog.Warn("oauth eager refresh failed, storing as-is", "module", "cb", "email", email, "error", err)
		}
	}

	km.mu.Lock()
	for _, existing := range km.keys {
		if existing.Key == email || (existing.CredType == CBAuthOAuth && existing.Email == email) {
			// Update tokens on existing OAuth entry
			existing.mu.Lock()
			existing.CredType = CBAuthOAuth
			existing.AccessToken = accessToken
			existing.RefreshToken = refreshToken
			existing.ExpiresAt = expiresAt
			existing.Email = email
			existing.disabled = false
			existing.disabledAt = time.Time{}
			existing.mu.Unlock()
			n := len(km.keys)
			km.mu.Unlock()
			if km.db != nil {
				saveCBKey(km.db, existing.toDTO())
			}
			return false, n
		}
	}
	key := &CBKey{
		Key:          email,
		CredType:     CBAuthOAuth,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
		Email:        email,
		db:           km.db,
	}
	km.keys = append(km.keys, key)
	total = len(km.keys)
	km.mu.Unlock()
	if km.db != nil {
		saveCBKey(km.db, key.toDTO())
	}
	return true, total
}

// tryEagerRefresh performs a one-shot OAuth token refresh on a probe CBKey
// (not yet in the pool). On success the probe's token fields are updated in
// place and the new ExpiresAt is returned. Network round-trip runs WITHOUT
// the pool mutex held.
func tryEagerRefresh(probe *CBKey) (time.Time, error) {
	if probe.GetCredType() != CBAuthOAuth {
		return time.Time{}, nil
	}
	if err := probe.Refresh(); err != nil {
		return time.Time{}, err
	}
	probe.mu.RLock()
	at := probe.AccessToken
	rt := probe.RefreshToken
	exp := probe.ExpiresAt
	probe.mu.RUnlock()
	if at == "" || rt == "" {
		return time.Time{}, fmt.Errorf("eager refresh returned empty token")
	}
	return exp, nil
}

// ReenableCooldowns lifts temp cooldowns past 10 minutes (background only).
func (km *CBKeyManager) ReenableCooldowns() {
	keys := km.GetAll()
	now := time.Now()
	var reenabled []*CBKey
	for _, key := range keys {
		key.mu.Lock()
		if key.disabled && !key.disabledAt.IsZero() && now.Sub(key.disabledAt) > 10*time.Minute {
			key.disabled = false
			reenabled = append(reenabled, key)
		}
		key.mu.Unlock()
	}
	for _, key := range reenabled {
		if key.db != nil {
			saveCBKey(key.db, key.toDTO())
		}
		slog.Info("re-enabled cooldown key", "module", "cb", "key", key.DisplayID())
	}
}

// ReenableCBWorker is the long-lived goroutine that lifts cooldowns.
// Pass a context cancelled on SIGTERM for clean shutdown.
func ReenableCBWorker(ctx context.Context, km *CBKeyManager) {
	km.ReenableCooldowns()
	ticker := time.NewTicker(REENABLE_TICK)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			km.ReenableCooldowns()
		}
	}
}

// CBOAuthRefreshWorker pre-warms OAuth access tokens before they expire.
// Mirrors Grok's AutoRefreshWorker: every PRE_WARM_TICK, scan OAuth keys
// within PRE_WARM_WINDOW of expiry, refresh with MAX_CONCURRENT_REFRESH cap.
// Pass a context cancelled on SIGTERM for clean shutdown.
func CBOAuthRefreshWorker(ctx context.Context, km *CBKeyManager) {
	ticker := time.NewTicker(PRE_WARM_TICK)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		keys := km.GetAll()
		var wg sync.WaitGroup
		sem := make(chan struct{}, MAX_CONCURRENT_REFRESH)

		for _, k := range keys {
			k.mu.RLock()
			isOAuth := k.CredType == CBAuthOAuth
			perm := k.disabled && k.disabledAt.IsZero()
			needsRefresh := isOAuth && !perm && !k.ExpiresAt.IsZero() &&
				time.Now().After(k.ExpiresAt.Add(-PRE_WARM_WINDOW))
			email := k.Email
			if email == "" {
				email = k.Key
			}
			k.mu.RUnlock()

			if !needsRefresh {
				continue
			}

			wg.Add(1)
			sem <- struct{}{}
			go func(key *CBKey, email string) {
				defer wg.Done()
				defer func() { <-sem }()

				if err := key.Refresh(); err != nil {
					slog.Warn("oauth pre-warm refresh error", "module", "cb-worker", "email", email, "error", err)
				}
			}(k, email)
		}
		wg.Wait()
	}
}

// CBCreditSyncWorker periodically syncs credit usage from the CodeBuddy meter
// API for all non-permanently-disabled keys. Runs once immediately at start
// (with small stagger), then every CB_CREDIT_SYNC_TICK with concurrency
// CB_CREDIT_SYNC_CONCURRENCY.
// Pass a context cancelled on SIGTERM for clean shutdown.
func CBCreditSyncWorker(ctx context.Context, km *CBKeyManager) {
	// Immediate first pass with small stagger so we don't stampede on boot.
	syncAllCBCredits(km, true)

	ticker := time.NewTicker(CB_CREDIT_SYNC_TICK)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			syncAllCBCredits(km, false)
		}
	}
}

// syncAllCBCredits walks the pool and SyncCredits() each non-permanent-disabled
// key. stagger=true adds a small per-key delay on the first boot pass.
func syncAllCBCredits(km *CBKeyManager, stagger bool) {
	keys := km.GetAll()
	var wg sync.WaitGroup
	sem := make(chan struct{}, CB_CREDIT_SYNC_CONCURRENCY)
	idx := 0
	for _, k := range keys {
		k.mu.RLock()
		perm := k.disabled && k.disabledAt.IsZero()
		display := k.displayIDLocked()
		k.mu.RUnlock()
		if perm {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		delay := time.Duration(0)
		if stagger {
			delay = time.Duration(idx%CB_CREDIT_SYNC_CONCURRENCY) * 200 * time.Millisecond
		}
		idx++
		go func(key *CBKey, display string, delay time.Duration) {
			defer wg.Done()
			defer func() { <-sem }()
			if delay > 0 {
				time.Sleep(delay)
			}
			if err := key.SyncCredits(); err != nil {
				slog.Warn("credit sync error", "module", "cb-meter", "key", display, "error", err)
			}
		}(k, display, delay)
	}
	wg.Wait()
}

func (km *CBKeyManager) DeleteKey(key string) bool {
	km.mu.Lock()
	for i, k := range km.keys {
		if k.Key == key {
			km.keys = append(km.keys[:i], km.keys[i+1:]...)
			km.mu.Unlock()
			if km.db != nil {
				km.db.DeleteCBKey(key)
			}
			slog.Info("deleted cb key", "module", "cb", "key", maskCBKey(key))
			return true
		}
	}
	km.mu.Unlock()
	return false
}

// DisableKey permanently disables a key (disabledAt = zero time) and persists
// the state + reason to Redis via SaveCBKey, so the disable survives restarts
// and is never overwritten by the credit-sync worker. Returns false if the key
// is unknown. Used by POST /cb/keys/disable (dashboard + sweep tooling).
func (km *CBKeyManager) DisableKey(key, reason string) bool {
	km.mu.RLock()
	var found *CBKey
	for _, k := range km.keys {
		if k.Key == key {
			found = k
			break
		}
	}
	km.mu.RUnlock()
	if found == nil {
		return false
	}
	found.mu.Lock()
	found.disabled = true
	found.disabledAt = time.Time{} // permanent — never auto-reenabled
	found.disabledReason = reason
	found.mu.Unlock()
	if found.db != nil {
		saveCBKey(found.db, found.toDTO())
	} else {
		// M5: memory-only change — lost on restart. Make it visible.
		slog.Warn("disable persisted to memory only (no store)", "module", "cb", "key", maskCBKey(key))
	}
	slog.Info("disabled cb key", "module", "cb", "key", maskCBKey(key), "reason", reason)
	return true
}

// EnableKey re-enables a previously disabled key (clears disabled flag and
// reason, persists via SaveCBKey so the state is consistent in memory + Redis).
// Returns false if the key is unknown. Used by POST /cb/keys/enable.
func (km *CBKeyManager) EnableKey(key string) bool {
	km.mu.RLock()
	var found *CBKey
	for _, k := range km.keys {
		if k.Key == key {
			found = k
			break
		}
	}
	km.mu.RUnlock()
	if found == nil {
		return false
	}
	found.mu.Lock()
	found.disabled = false
	found.disabledAt = time.Time{}
	found.disabledReason = ""
	found.mu.Unlock()
	if found.db != nil {
		saveCBKey(found.db, found.toDTO())
	} else {
		// M5: memory-only change — lost on restart. Make it visible.
		slog.Warn("enable persisted to memory only (no store)", "module", "cb", "key", maskCBKey(key))
	}
	slog.Info("enabled cb key", "module", "cb", "key", maskCBKey(key))
	return true
}

// CleanupDisabled removes all permanently disabled keys (disabledAt is zero time).
// Returns the count of removed keys. Does NOT affect cooldown keys (disabledAt set).
func (km *CBKeyManager) CleanupDisabled() int {
	km.mu.Lock()
	var removed int
	var kept []*CBKey
	for _, k := range km.keys {
		k.mu.RLock()
		permDisabled := k.disabled && k.disabledAt.IsZero()
		k.mu.RUnlock()
		if permDisabled {
			removed++
			if km.db != nil {
				km.db.DeleteCBKey(k.Key)
			}
		} else {
			kept = append(kept, k)
		}
	}
	km.keys = kept
	km.mu.Unlock()
	if removed > 0 {
		slog.Info("cleanup disabled cb keys", "module", "cb", "removed", removed, "remaining", km.Len())
	}
	return removed
}

// parseJWTExp extracts the exp claim from a JWT without verifying the signature.
// Returns zero time if the token is not a JWT or has no exp.
func parseJWTExp(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	// JWT payload is base64url without padding
	payload := parts[1]
	// Add padding if needed
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		// Try raw std encoding without padding variants
		raw, err = base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Time{}
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

// ParseJWTExp is exported for handlers that need to derive expires_at from an AT.
func ParseJWTExp(token string) time.Time { return parseJWTExp(token) }
