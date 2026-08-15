package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"foxrouters/internal/db"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type FreebuffAccount struct {
	Token         string    `json:"token"`
	UserID        string    `json:"user_id"`
	Email         string    `json:"email"`
	Disabled      bool      `json:"disabled"`
	DisabledAt    time.Time `json:"disabled_at"`
	Banned        bool      `json:"banned"`
	BannedAt      time.Time `json:"banned_at"`
	CooldownUntil time.Time `json:"cooldown_until"`
	QuotaRecent   float64   `json:"quota_recent"`
	QuotaLimit    float64   `json:"quota_limit"`
	QuotaSyncedAt time.Time `json:"quota_synced_at"`
	QuotaResetAt  time.Time `json:"quota_reset_at"`
	QuotaPeriod   string    `json:"quota_period"`
	// Access tier from GET /session `accessTier` ("" = unknown, "full", "limited", "blocked")
	Tier                string  `json:"tier"`
	CountryCode         string  `json:"country_code"`
	CountryBlockReason  string  `json:"country_block_reason"`
	EntitlementBase     float64 `json:"entitlement_base"`
	EntitlementReferral float64 `json:"entitlement_referral"`
	EntitlementStreak   float64 `json:"entitlement_streak"`
	// Per-model session quota snapshot (from rateLimitsByModel, key = upstream model name)
	QuotaByModel map[string]FbModelQuota `json:"quota_by_model"`
	// Hourly session tracking (in-memory, resets each hour)
	HourlySessionCount int       `json:"-"`
	HourlyWindowStart  time.Time `json:"-"`
	mu                 sync.Mutex
	db                 *db.Store
}

// FbModelQuota is one model's session-quota entry from GET /session rateLimitsByModel.
type FbModelQuota struct {
	Limit               float64 `json:"limit"`
	RecentCount         float64 `json:"recent_count"`
	ResetAt             string  `json:"reset_at,omitempty"`
	Period              string  `json:"period,omitempty"`
	EntitlementBase     float64 `json:"entitlement_base,omitempty"`
	EntitlementReferral float64 `json:"entitlement_referral,omitempty"`
	EntitlementStreak   float64 `json:"entitlement_streak,omitempty"`
}

// FreebuffAccountManager manages the Freebuff account pool.
type FreebuffAccountManager struct {
	accounts map[string]*FreebuffAccount
	mu       sync.RWMutex
	idx      uint64
	db       *db.Store
}

func NewFreebuffAccountManager(store *db.Store) *FreebuffAccountManager {
	return &FreebuffAccountManager{
		accounts: make(map[string]*FreebuffAccount),
		db:       store,
	}
}

func (am *FreebuffAccountManager) DB() *db.Store { return am.db }

func (am *FreebuffAccountManager) Len() int {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return len(am.accounts)
}

// GetAccount returns the account for a token, or nil if not found.
func (am *FreebuffAccountManager) GetAccount(token string) *FreebuffAccount {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.accounts[token]
}

// QuotaSummary returns (totalRecent, totalLimit, exhaustedCount) across all accounts.
func (am *FreebuffAccountManager) QuotaSummary() (totalRecent, totalLimit float64, exhausted int) {
	am.mu.RLock()
	defer am.mu.RUnlock()
	for _, acc := range am.accounts {
		acc.mu.Lock()
		totalRecent += acc.QuotaRecent
		totalLimit += acc.QuotaLimit
		if acc.QuotaLimit > 0 && acc.QuotaRecent >= acc.QuotaLimit {
			exhausted++
		}
		acc.mu.Unlock()
	}
	return
}

// IncrementSessionCount increments the hourly session counter for an account.
// Resets the counter if a new hour window has started.
func (am *FreebuffAccountManager) IncrementSessionCount(acc *FreebuffAccount) {
	acc.mu.Lock()
	defer acc.mu.Unlock()

	now := time.Now()
	hourStart := now.Truncate(time.Hour) // start of current hour

	if acc.HourlyWindowStart.IsZero() || acc.HourlyWindowStart.Before(hourStart) {
		acc.HourlyWindowStart = hourStart
		acc.HourlySessionCount = 0
	}
	acc.HourlySessionCount++
}

// LoadFromRedis loads all Freebuff accounts from Redis.
func (am *FreebuffAccountManager) LoadFromRedis() error {
	if am.db == nil || !am.db.Ready() {
		return fmt.Errorf("redis not available")
	}
	rdb := am.db.Redis()
	if rdb == nil {
		return fmt.Errorf("redis client nil")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	iter := rdb.Scan(ctx, 0, "fb:account:*", 100).Iterator()
	loaded := 0
	for iter.Next(ctx) {
		key := iter.Val()
		token := strings.TrimPrefix(key, "fb:account:")
		vals, err := rdb.HGetAll(ctx, key).Result()
		if err != nil {
			slog.Warn("fb HGetAll failed", "module", "freebuff", "token", token[:8]+"...", "error", err)
			continue
		}
		acc := &FreebuffAccount{
			Token:  token,
			UserID: vals["user_id"],
			Email:  vals["email"],
			db:     am.db,
		}
		if vals["disabled"] == "1" || vals["disabled"] == "true" {
			acc.Disabled = true
		}
		if vals["banned"] == "1" || vals["banned"] == "true" {
			acc.Banned = true
		}
		if v, ok := vals["banned_at"]; ok && v != "" && v != "0" {
			var ts int64
			fmt.Sscanf(v, "%d", &ts)
			if ts > 0 {
				acc.BannedAt = time.Unix(ts, 0)
			}
		}
		if v, ok := vals["cooldown_until"]; ok && v != "" && v != "0" {
			var ts int64
			fmt.Sscanf(v, "%d", &ts)
			if ts > 0 {
				acc.CooldownUntil = time.Unix(ts, 0)
			}
		}
		if v, ok := vals["quota_recent"]; ok && v != "" {
			fmt.Sscanf(v, "%f", &acc.QuotaRecent)
		}
		if v, ok := vals["quota_limit"]; ok && v != "" {
			fmt.Sscanf(v, "%f", &acc.QuotaLimit)
		}
		if v, ok := vals["quota_synced_at"]; ok && v != "" && v != "0" {
			var ts int64
			fmt.Sscanf(v, "%d", &ts)
			if ts > 0 {
				acc.QuotaSyncedAt = time.Unix(ts, 0)
			}
		}
		if v, ok := vals["quota_reset_at"]; ok && v != "" && v != "0" {
			var ts int64
			fmt.Sscanf(v, "%d", &ts)
			if ts > 0 {
				acc.QuotaResetAt = time.Unix(ts, 0)
			}
		}
		if v, ok := vals["quota_period"]; ok {
			acc.QuotaPeriod = v
		}
		acc.Tier = vals["tier"]
		acc.CountryCode = vals["country_code"]
		acc.CountryBlockReason = vals["country_block_reason"]
		if v, ok := vals["entitlement_base"]; ok && v != "" {
			fmt.Sscanf(v, "%f", &acc.EntitlementBase)
		}
		if v, ok := vals["entitlement_referral"]; ok && v != "" {
			fmt.Sscanf(v, "%f", &acc.EntitlementReferral)
		}
		if v, ok := vals["entitlement_streak"]; ok && v != "" {
			fmt.Sscanf(v, "%f", &acc.EntitlementStreak)
		}
		if v, ok := vals["quota_by_model"]; ok && v != "" {
			var qbm map[string]FbModelQuota
			if json.Unmarshal([]byte(v), &qbm) == nil && len(qbm) > 0 {
				acc.QuotaByModel = qbm
			}
		}
		am.mu.Lock()
		am.accounts[token] = acc
		am.mu.Unlock()
		loaded++
	}
	if err := iter.Err(); err != nil {
		return err
	}
	slog.Info("Freebuff accounts loaded", "module", "freebuff", "count", loaded)
	return nil
}

// MarkBanned marks an account permanently banned (persisted to Redis).
// Banned accounts are skipped by Next() but keep their entry so the ban is
// visible in the dashboard; they can be manually re-enabled via re-import.
func (am *FreebuffAccountManager) MarkBanned(token string) {
	am.mu.Lock()
	acc, ok := am.accounts[token]
	if !ok {
		am.mu.Unlock()
		return
	}
	acc.mu.Lock()
	if !acc.Banned {
		acc.Banned = true
		acc.BannedAt = time.Now()
		am.SaveAccount(acc)
	}
	acc.mu.Unlock()
	am.mu.Unlock()
}

// SaveAccount persists account state to Redis.
func (am *FreebuffAccountManager) SaveAccount(acc *FreebuffAccount) {
	if am.db == nil || !am.db.Ready() {
		return
	}
	rdb := am.db.Redis()
	if rdb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	disabledStr := "0"
	if acc.Disabled {
		disabledStr = "1"
	}
	cooldownTs := int64(0)
	if !acc.CooldownUntil.IsZero() {
		cooldownTs = acc.CooldownUntil.Unix()
	}
	bannedStr := "0"
	if acc.Banned {
		bannedStr = "1"
	}
	data := map[string]interface{}{
		"token":                acc.Token,
		"user_id":              acc.UserID,
		"email":                acc.Email,
		"disabled":             disabledStr,
		"disabled_at":          acc.DisabledAt.Unix(),
		"banned":               bannedStr,
		"banned_at":            acc.BannedAt.Unix(),
		"cooldown_until":       cooldownTs,
		"quota_recent":         acc.QuotaRecent,
		"quota_limit":          acc.QuotaLimit,
		"quota_synced_at":      acc.QuotaSyncedAt.Unix(),
		"quota_reset_at":       acc.QuotaResetAt.Unix(),
		"quota_period":         acc.QuotaPeriod,
		"tier":                 acc.Tier,
		"country_code":         acc.CountryCode,
		"country_block_reason": acc.CountryBlockReason,
		"entitlement_base":     acc.EntitlementBase,
		"entitlement_referral": acc.EntitlementReferral,
		"entitlement_streak":   acc.EntitlementStreak,
	}
	if qbmJSON, err := json.Marshal(acc.QuotaByModel); err == nil && qbmJSON != nil {
		data["quota_by_model"] = string(qbmJSON)
	}
	key := "fb:account:" + acc.Token
	if err := rdb.HSet(ctx, key, data).Err(); err != nil {
		slog.Warn("fb HSet failed", "module", "freebuff", "error", err)
	}
}

// AddAccount probes /api/v1/me for userID+email, then adds the account.
// If userID/email are already known (from OAuth poll), pass them to avoid re-probe.
func (am *FreebuffAccountManager) AddAccount(token string) (added bool, total int, err error) {
	return am.AddAccountWithInfo(token, "", "")
}

// AddAccountWithInfo adds an account with pre-known userID + email (from OAuth poll).
// Falls back to /api/v1/me probe if userID or email is empty.
func (am *FreebuffAccountManager) AddAccountWithInfo(token, userID, email string) (added bool, total int, err error) {
	am.mu.Lock()
	if existing, exists := am.accounts[token]; exists {
		// Update email/userID if they were empty and we now have them
		updated := false
		if existing.Email == "" && email != "" {
			existing.Email = email
			updated = true
		}
		if existing.UserID == "" && userID != "" {
			existing.UserID = userID
			updated = true
		}
		if updated {
			am.SaveAccount(existing)
		}
		am.mu.Unlock()
		return false, am.Len(), nil
	}
	am.mu.Unlock()

	// Only probe if we don't already have userID or email
	if userID == "" || email == "" {
		uid2, email2, err := fbProbeMe(token)
		if err != nil {
			return false, am.Len(), err
		}
		if userID == "" {
			userID = uid2
		}
		if email == "" {
			email = email2
		}
	}

	acc := &FreebuffAccount{
		Token:  token,
		UserID: userID,
		Email:  email,
		db:     am.db,
	}

	am.mu.Lock()
	am.accounts[token] = acc
	total = len(am.accounts)
	am.mu.Unlock()

	am.SaveAccount(acc)
	slog.Info("Freebuff account added", "module", "freebuff", "email", email, "user_id", userID)
	return true, total, nil
}

// RemoveAccount deletes an account from the pool + Redis.
func (am *FreebuffAccountManager) RemoveAccount(token string) error {
	am.mu.Lock()
	delete(am.accounts, token)
	am.mu.Unlock()

	if am.db != nil && am.db.Ready() {
		rdb := am.db.Redis()
		if rdb != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			rdb.Del(ctx, "fb:account:"+token)
		}
	}
	return nil
}

// fbIsPremiumModel reports whether the upstream model is a premium (full-mode
// only) model — i.e. NOT deepseek-v4-flash / mimo-v2.5.
func fbIsPremiumModel(upstreamModel string) bool {
	models := GetFBModels()
	for i := range models {
		if models[i].Upstream == upstreamModel {
			return models[i].FullMode
		}
	}
	return false
}

// Next returns the next available account (quota-aware, skip disabled + cooldown + exhausted).
// Prefers accounts with lowest QuotaRecent (most remaining quota).
// model: requested upstream model — accounts on "limited" tier are skipped for
// premium (full-mode-only) models, and "blocked" accounts are always skipped.
// Tier "" (unknown, not yet synced) passes through.
func (am *FreebuffAccountManager) Next(model string) (*FreebuffAccount, error) {
	am.mu.RLock()
	defer am.mu.RUnlock()

	if len(am.accounts) == 0 {
		return nil, fmt.Errorf("no freebuff accounts available")
	}

	premiumModel := fbIsPremiumModel(model)

	// Build list of eligible accounts (not disabled, not on cooldown, quota not exhausted,
	// tier-compatible with the requested model)
	var eligible []*FreebuffAccount
	var earliestReset time.Time
	for _, acc := range am.accounts {
		acc.mu.Lock()
		disabled := acc.Disabled
		banned := acc.Banned
		cooldown := !acc.CooldownUntil.IsZero() && time.Now().Before(acc.CooldownUntil)
		quotaExhausted := acc.QuotaLimit > 0 && acc.QuotaRecent >= acc.QuotaLimit
		tierBlocked := acc.Tier == "blocked" || (acc.Tier == "limited" && premiumModel)
		if !acc.QuotaResetAt.IsZero() && (earliestReset.IsZero() || acc.QuotaResetAt.Before(earliestReset)) {
			earliestReset = acc.QuotaResetAt
		}
		acc.mu.Unlock()
		if !disabled && !banned && !cooldown && !quotaExhausted && !tierBlocked {
			eligible = append(eligible, acc)
		}
	}

	if len(eligible) == 0 {
		if premiumModel {
			return nil, fmt.Errorf("all freebuff accounts on limited tier — premium model %q unavailable without full-access accounts", model)
		}
		if !earliestReset.IsZero() {
			return nil, fmt.Errorf("all freebuff accounts quota exhausted, resets at %s", earliestReset.UTC().Format(time.RFC3339))
		}
		return nil, fmt.Errorf("all freebuff accounts on cooldown or disabled")
	}

	// Sort by QuotaRecent ascending (least used = most remaining quota)
	// Snapshot quota values under lock, then sort without holding locks (avoid deadlock in comparator)
	type fbCandidate struct {
		acc      *FreebuffAccount
		recent   float64
		priority int // 0 = live cached session for requested model, 1 = idle (no session), 2 = session on other model
	}

	candidates := make([]fbCandidate, 0, len(eligible))
	for _, acc := range eligible {
		acc.mu.Lock()
		recent := acc.QuotaRecent
		acc.mu.Unlock()
		priority := 2
		if am.hasCachedSessionFor(acc.Token, model) {
			priority = 0
		} else if !am.hasAnyCachedSession(acc.Token) {
			priority = 1
		}
		candidates = append(candidates, fbCandidate{acc: acc, recent: recent, priority: priority})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority < candidates[j].priority
		}
		return candidates[i].recent < candidates[j].recent
	})

	// Pick the best priority group (session match > idle > mismatch), round-robin within it.
	bestPriority := candidates[0].priority
	var top []*FreebuffAccount
	for _, c := range candidates {
		if c.priority != bestPriority {
			break
		}
		top = append(top, c.acc)
	}

	idx := atomic.AddUint64(&am.idx, 1)
	return top[idx%uint64(len(top))], nil
}

// hasCachedSessionFor reports whether the account has a live cached session for
// the given model. In-memory L1 only — cheap, no Redis per account. Redis L2 is
// warmed back into L1 by cachedSession() on actual use, so a cold L1 after a
// restart just degrades to "idle" (priority 1), which fbGetOrCreateSession still
// resolves correctly via GET /session (0 cost).
func (am *FreebuffAccountManager) hasCachedSessionFor(token, model string) bool {
	cacheKey := fbSessionKeyPrefix + token + ":" + model
	fbSessionCache.Lock()
	cached, ok := fbSessionCache.m[cacheKey]
	fbSessionCache.Unlock()
	return ok && time.Until(cached.ExpiresAt) > FREEBUFF_SESSION_MIN_REMAINING
}

// hasAnyCachedSession reports whether the account has at least one live cached
// session (any model). Expired entries are treated as idle.
func (am *FreebuffAccountManager) hasAnyCachedSession(token string) bool {
	prefix := fbSessionKeyPrefix + token + ":"
	fbSessionCache.Lock()
	defer fbSessionCache.Unlock()
	for k, v := range fbSessionCache.m {
		if strings.HasPrefix(k, prefix) && time.Until(v.ExpiresAt) > FREEBUFF_SESSION_MIN_REMAINING {
			return true
		}
	}
	return false
}

// ListAccounts returns a snapshot of all accounts for the API/dashboard.
func (am *FreebuffAccountManager) ListAccounts() []map[string]any {
	am.mu.RLock()
	defer am.mu.RUnlock()

	result := make([]map[string]any, 0, len(am.accounts))
	for _, acc := range am.accounts {
		acc.mu.Lock()
		status := "active"
		if acc.Banned {
			status = "banned"
		} else if acc.Disabled {
			status = "disabled"
		} else if !acc.CooldownUntil.IsZero() && time.Now().Before(acc.CooldownUntil) {
			status = "cooldown"
		}
		entry := map[string]any{
			"token":                acc.Token[:8] + "..." + acc.Token[len(acc.Token)-4:],
			"token_full":           acc.Token,
			"user_id":              acc.UserID,
			"email":                acc.Email,
			"status":               status,
			"disabled":             acc.Disabled,
			"banned":               acc.Banned,
			"banned_at":            acc.BannedAt,
			"cooldown_until":       acc.CooldownUntil,
			"quota_recent":         acc.QuotaRecent,
			"quota_limit":          acc.QuotaLimit,
			"quota_reset_at":       acc.QuotaResetAt,
			"quota_period":         acc.QuotaPeriod,
			"quota_synced_at":      acc.QuotaSyncedAt,
			"hourly_session_count": acc.HourlySessionCount,
			"tier":                 acc.Tier,
			"country_code":         acc.CountryCode,
			"country_block_reason": acc.CountryBlockReason,
			"entitlement_base":     acc.EntitlementBase,
			"entitlement_referral": acc.EntitlementReferral,
			"entitlement_streak":   acc.EntitlementStreak,
			"quota_by_model":       acc.QuotaByModel,
		}
		acc.mu.Unlock()
		result = append(result, entry)
	}
	return result
}

// ProbeAll does a health check on all accounts (GET /api/v1/me, 0 cost).
func (am *FreebuffAccountManager) ProbeAll() {
	am.mu.RLock()
	var accounts []*FreebuffAccount
	for _, acc := range am.accounts {
		accounts = append(accounts, acc)
	}
	am.mu.RUnlock()

	for _, acc := range accounts {
		userID, email, err := fbProbeMe(acc.Token)
		if err != nil {
			acc.mu.Lock()
			acc.Disabled = true
			acc.DisabledAt = time.Now()
			acc.mu.Unlock()
			am.SaveAccount(acc)
			slog.Warn("fb probe failed, disabling", "module", "freebuff", "token", acc.Token[:8]+"...", "error", err)
		} else if userID != acc.UserID || email != acc.Email {
			acc.mu.Lock()
			acc.UserID = userID
			acc.Email = email
			acc.Disabled = false
			acc.mu.Unlock()
			am.SaveAccount(acc)
		}
	}
}
