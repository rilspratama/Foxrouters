package upstream

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"foxrouters/internal/db"
)

// ============================================================================
// CONSTANTS
// ============================================================================

const (
	FREEBUFF_API_BASE     = "https://www.codebuff.com"
	FREEBUFF_CHAT_PATH    = "/api/v1/chat/completions"
	FREEBUFF_SESSION_PATH = "/api/v1/freebuff/session"
	FREEBUFF_RUN_PATH     = "/api/v1/agent-runs"
	FREEBUFF_ME_PATH      = "/api/v1/me"
	FREEBUFF_STREAK_PATH  = "/api/v1/freebuff/streak"
	FREEBUFF_ADS_PATH     = "/api/v1/ads"

	FREEBUFF_BUFFY_PREFIX = "You are Buffy, the strategic coding assistant."

	// Timeouts
	FREEBUFF_UPSTREAM_TIMEOUT  = 25 * time.Second
	FREEBUFF_SESSION_TIMEOUT   = 12 * time.Second
	FREEBUFF_NONSTREAM_TIMEOUT = 50 * time.Second

	// Session cache: reuse if >60s remaining
	FREEBUFF_SESSION_MIN_REMAINING = 60 * time.Second

	// Run cache: reuse runId for 10 minutes
	FREEBUFF_RUN_CACHE_TTL = 10 * time.Minute
)

// FreebuffModels maps gateway model IDs (fb/ prefix) to upstream config.
type FreebuffModelConfig struct {
	GatewayID  string // e.g. "fb/deepseek-v4-flash"
	Upstream   string // e.g. "deepseek/deepseek-v4-flash"
	Agent      string // e.g. "base2-free-deepseek-flash"
	Reasoning  bool
	FullMode   bool // true = only available in full access mode (US/EU exit)
}

var FreebuffModels = []FreebuffModelConfig{
	{"fb/deepseek-v4-flash", "deepseek/deepseek-v4-flash", "base2-free-deepseek-flash", true, false},
	{"fb/mimo-v2.5", "mimo/mimo-v2.5", "base2-free-mimo", false, false},
	{"fb/deepseek-v4-pro", "deepseek/deepseek-v4-pro", "base2-free-deepseek", true, true},
	{"fb/minimax-m3", "minimax/minimax-m3", "base2-free-minimax-m3", false, true},
	{"fb/gpt-5.6-luna", "openai/gpt-5.6-luna", "base2-free-luna", false, true},
	{"fb/glm-5.2", "z-ai/glm-5.2", "base2-free-glm", true, true},
}

// IsFreebuffModel returns true if the model routes to the Freebuff upstream.
func IsFreebuffModel(model string) bool {
	return strings.HasPrefix(model, "fb/")
}

// fbStripPrefix removes the "fb/" prefix → upstream model name.
func fbStripPrefix(model string) string {
	return strings.TrimPrefix(model, "fb/")
}

// fbModelConfig looks up the model config by gateway ID.
func fbModelConfig(gatewayID string) *FreebuffModelConfig {
	for i := range FreebuffModels {
		if FreebuffModels[i].GatewayID == gatewayID {
			return &FreebuffModels[i]
		}
	}
	// Default to deepseek-v4-flash
	return &FreebuffModels[0]
}

// ============================================================================
// ACCOUNT + MANAGER
// ============================================================================

// FreebuffAccount holds one Freebuff auth token + metadata.
type FreebuffAccount struct {
	Token         string    `json:"token"`
	UserID        string    `json:"user_id"`
	Email         string    `json:"email"`
	Disabled      bool      `json:"disabled"`
	DisabledAt    time.Time `json:"disabled_at"`
	CooldownUntil time.Time `json:"cooldown_until"`
	QuotaRecent   float64   `json:"quota_recent"`
	QuotaLimit    float64   `json:"quota_limit"`
	QuotaSyncedAt time.Time `json:"quota_synced_at"`
	QuotaResetAt  time.Time `json:"quota_reset_at"`
	QuotaPeriod   string    `json:"quota_period"`
	// Hourly session tracking (in-memory, resets each hour)
	HourlySessionCount int       `json:"-"`
	HourlyWindowStart  time.Time `json:"-"`
	mu                 sync.Mutex
	db                 *db.Store
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
	data := map[string]interface{}{
		"token":           acc.Token,
		"user_id":         acc.UserID,
		"email":           acc.Email,
		"disabled":        disabledStr,
		"disabled_at":     acc.DisabledAt.Unix(),
		"cooldown_until":  cooldownTs,
		"quota_recent":    acc.QuotaRecent,
		"quota_limit":     acc.QuotaLimit,
		"quota_synced_at": acc.QuotaSyncedAt.Unix(),
		"quota_reset_at":  acc.QuotaResetAt.Unix(),
		"quota_period":    acc.QuotaPeriod,
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

// Next returns the next available account (quota-aware, skip disabled + cooldown + exhausted).
// Prefers accounts with lowest QuotaRecent (most remaining quota).
func (am *FreebuffAccountManager) Next() (*FreebuffAccount, error) {
	am.mu.RLock()
	defer am.mu.RUnlock()

	if len(am.accounts) == 0 {
		return nil, fmt.Errorf("no freebuff accounts available")
	}

	// Build list of eligible accounts (not disabled, not on cooldown, quota not exhausted)
	var eligible []*FreebuffAccount
	var earliestReset time.Time
	for _, acc := range am.accounts {
		acc.mu.Lock()
		disabled := acc.Disabled
		cooldown := !acc.CooldownUntil.IsZero() && time.Now().Before(acc.CooldownUntil)
		quotaExhausted := acc.QuotaLimit > 0 && acc.QuotaRecent >= acc.QuotaLimit
		if !acc.QuotaResetAt.IsZero() && (earliestReset.IsZero() || acc.QuotaResetAt.Before(earliestReset)) {
			earliestReset = acc.QuotaResetAt
		}
		acc.mu.Unlock()
		if !disabled && !cooldown && !quotaExhausted {
			eligible = append(eligible, acc)
		}
	}

	if len(eligible) == 0 {
		if !earliestReset.IsZero() {
			return nil, fmt.Errorf("all freebuff accounts quota exhausted, resets at %s", earliestReset.UTC().Format(time.RFC3339))
		}
		return nil, fmt.Errorf("all freebuff accounts on cooldown or disabled")
	}

	// Sort by QuotaRecent ascending (least used = most remaining quota)
	// Snapshot quota values under lock, then sort without holding locks (avoid deadlock in comparator)
	type accQuota struct {
		acc    *FreebuffAccount
		recent float64
	}
	snapshots := make([]accQuota, len(eligible))
	for i, acc := range eligible {
		acc.mu.Lock()
		snapshots[i] = accQuota{acc: acc, recent: acc.QuotaRecent}
		acc.mu.Unlock()
	}
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].recent < snapshots[j].recent
	})
	// Rebuild eligible from sorted snapshots
	for i, s := range snapshots {
		eligible[i] = s.acc
	}

	// Round-robin among the top candidates with same lowest quota
	// (or just pick the first = least used)
	idx := atomic.AddUint64(&am.idx, 1)
	return eligible[idx%uint64(len(eligible))], nil
}

// ListAccounts returns a snapshot of all accounts for the API/dashboard.
func (am *FreebuffAccountManager) ListAccounts() []map[string]any {
	am.mu.RLock()
	defer am.mu.RUnlock()

	result := make([]map[string]any, 0, len(am.accounts))
	for _, acc := range am.accounts {
		acc.mu.Lock()
		status := "active"
		if acc.Disabled {
			status = "disabled"
		} else if !acc.CooldownUntil.IsZero() && time.Now().Before(acc.CooldownUntil) {
			status = "cooldown"
		}
		entry := map[string]any{
			"token":           acc.Token[:8] + "..." + acc.Token[len(acc.Token)-4:],
			"token_full":      acc.Token,
			"user_id":         acc.UserID,
			"email":           acc.Email,
			"status":          status,
			"disabled":        acc.Disabled,
			"cooldown_until":  acc.CooldownUntil,
			"quota_recent":    acc.QuotaRecent,
			"quota_limit":     acc.QuotaLimit,
			"quota_reset_at":     acc.QuotaResetAt,
			"quota_period":       acc.QuotaPeriod,
			"quota_synced_at":    acc.QuotaSyncedAt,
			"hourly_session_count": acc.HourlySessionCount,
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

// SyncQuota syncs quota info for a single account from GET /api/v1/freebuff/session.
func (am *FreebuffAccountManager) SyncQuota(acc *FreebuffAccount) error {
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", FREEBUFF_API_BASE+FREEBUFF_SESSION_PATH, nil)
	req.Header.Set("Authorization", "Bearer "+acc.Token)
	req.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("quota sync: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("quota sync [%d]: %s", resp.StatusCode, truncateLog(string(body), 200))
	}

	var data struct {
		RateLimitsByModel map[string]struct {
			Limit       float64 `json:"limit"`
			RecentCount float64 `json:"recentCount"`
			ResetAt     string  `json:"resetAt"`
			Period      string  `json:"period"`
			WindowHours int     `json:"windowHours"`
		} `json:"rateLimitsByModel"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return fmt.Errorf("quota sync parse: %w", err)
	}

	// Use deepseek-v4-flash as the reference model (available in all tiers)
	rl, ok := data.RateLimitsByModel["deepseek/deepseek-v4-flash"]
	if !ok {
		// No rate limit info — account might not have a session yet
		acc.mu.Lock()
		acc.QuotaSyncedAt = time.Now()
		acc.mu.Unlock()
		am.SaveAccount(acc)
		return nil
	}

	acc.mu.Lock()
	acc.QuotaRecent = rl.RecentCount
	acc.QuotaLimit = rl.Limit
	acc.QuotaPeriod = rl.Period
	acc.QuotaSyncedAt = time.Now()

	// Parse resetAt
	if rl.ResetAt != "" {
		if resetTime, err := time.Parse(time.RFC3339, rl.ResetAt); err == nil {
			acc.QuotaResetAt = resetTime
			// Auto-cooldown if quota exhausted
			if rl.Limit > 0 && rl.RecentCount >= rl.Limit {
				acc.CooldownUntil = resetTime
			}
		}
	}
	acc.mu.Unlock()

	am.SaveAccount(acc)
	slog.Info("fb quota synced", "module", "freebuff", "token", acc.Token[:8]+"...", "recent", rl.RecentCount, "limit", rl.Limit, "reset", rl.ResetAt)
	return nil
}

// SyncAllQuota syncs quota for all accounts.
func (am *FreebuffAccountManager) SyncAllQuota() {
	am.mu.RLock()
	var accounts []*FreebuffAccount
	for _, acc := range am.accounts {
		accounts = append(accounts, acc)
	}
	am.mu.RUnlock()

	synced := 0
	for _, acc := range accounts {
		if err := am.SyncQuota(acc); err != nil {
			slog.Warn("fb quota sync failed", "module", "freebuff", "token", acc.Token[:8]+"...", "error", err)
		} else {
			synced++
		}
	}
	slog.Info("fb quota sync complete", "module", "freebuff", "synced", synced, "total", len(accounts))
}

// FbQuotaSyncWorker periodically syncs quota for all Freebuff accounts (every 5 min).
func FbQuotaSyncWorker(ctx context.Context, am *FreebuffAccountManager) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	// Initial sync after 10s startup delay
	time.Sleep(10 * time.Second)
	am.SyncAllQuota()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			am.SyncAllQuota()
		}
	}
}

// ============================================================================
// SESSION + RUN CACHE (in-memory, ephemeral)
// ============================================================================

type FreebuffSession struct {
	InstanceID  string
	Model       string
	ExpiresAt   time.Time
	RemainingMs int64
}

var fbSessionCache = struct {
	sync.Mutex
	m map[string]*FreebuffSession // key: "token:model"
}{m: make(map[string]*FreebuffSession)}

var fbRunCache = struct {
	sync.Mutex
	m map[string]fbRunEntry // key: "token:agent"
}{m: make(map[string]fbRunEntry)}

type fbRunEntry struct {
	RunID     string
	CreatedAt time.Time
}

// fbGetOrCreateSession returns an active session, creating one if needed.
func fbGetOrCreateSession(client *http.Client, token, userID, model string) (*FreebuffSession, error) {
	cacheKey := token + ":" + model

	// Check cache
	fbSessionCache.Lock()
	cached, ok := fbSessionCache.m[cacheKey]
	fbSessionCache.Unlock()
	if ok && time.Until(cached.ExpiresAt) > FREEBUFF_SESSION_MIN_REMAINING {
		return cached, nil
	}

	// GET current session (0 cost — doesn't create one)
	sess, err := fbGetSession(client, token)
	if err == nil && sess != nil && sess.Model == model && time.Until(sess.ExpiresAt) > FREEBUFF_SESSION_MIN_REMAINING {
		fbSessionCache.Lock()
		fbSessionCache.m[cacheKey] = sess
		fbSessionCache.Unlock()
		return sess, nil
	}

	// If model mismatch, delete old session
	if sess != nil && sess.Model != model {
		fbDeleteSession(client, token)
	}

	// Fire ads + streak (best-effort)
	go fbFireAdsAndStreak(client, token)

	// POST new session
	sess, err = fbCreateSession(client, token, model)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	fbSessionCache.Lock()
	fbSessionCache.m[cacheKey] = sess
	fbSessionCache.Unlock()
	return sess, nil
}

// fbGetSession does GET /api/v1/freebuff/session (0 cost).
func fbGetSession(client *http.Client, token string) (*FreebuffSession, error) {
	req, _ := http.NewRequest("GET", FREEBUFF_API_BASE+FREEBUFF_SESSION_PATH, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET session: %d", resp.StatusCode)
	}
	var data struct {
		Status      string `json:"status"`
		InstanceID  string `json:"instanceId"`
		Model       string `json:"model"`
		ExpiresAt   string `json:"expiresAt"`
		RemainingMs int64  `json:"remainingMs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	if data.Status != "active" {
		return nil, fmt.Errorf("session not active: %s", data.Status)
	}
	expiresAt, _ := time.Parse(time.RFC3339, data.ExpiresAt)
	return &FreebuffSession{
		InstanceID:  data.InstanceID,
		Model:       data.Model,
		ExpiresAt:   expiresAt,
		RemainingMs: data.RemainingMs,
	}, nil
}

// fbCreateSession does POST /api/v1/freebuff/session.
func fbCreateSession(client *http.Client, token, model string) (*FreebuffSession, error) {
	body, _ := json.Marshal(map[string]string{"model": model})
	req, _ := http.NewRequest("POST", FREEBUFF_API_BASE+FREEBUFF_SESSION_PATH, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-freebuff-model", model)
	req.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("POST session: %d %s", resp.StatusCode, string(respBody))
	}

	var data struct {
		Status      string `json:"status"`
		InstanceID  string `json:"instanceId"`
		Model       string `json:"model"`
		ExpiresAt   string `json:"expiresAt"`
		RemainingMs int64  `json:"remainingMs"`
	}
	if err := json.Unmarshal(respBody, &data); err != nil {
		return nil, err
	}

	// Handle queued status — poll
	if data.Status == "queued" && data.InstanceID != "" {
		for i := 0; i < 8; i++ {
			time.Sleep(1500 * time.Millisecond)
			sess, err := fbGetSessionWithInstance(client, token, data.InstanceID)
			if err == nil && sess != nil {
				return sess, nil
			}
		}
		return nil, fmt.Errorf("session stayed queued")
	}

	if data.Status != "active" || data.InstanceID == "" {
		return nil, fmt.Errorf("session not active: %s", data.Status)
	}

	expiresAt, _ := time.Parse(time.RFC3339, data.ExpiresAt)
	return &FreebuffSession{
		InstanceID:  data.InstanceID,
		Model:       data.Model,
		ExpiresAt:   expiresAt,
		RemainingMs: data.RemainingMs,
	}, nil
}

func fbGetSessionWithInstance(client *http.Client, token, instanceID string) (*FreebuffSession, error) {
	req, _ := http.NewRequest("GET", FREEBUFF_API_BASE+FREEBUFF_SESSION_PATH, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-freebuff-instance-id", instanceID)
	req.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET session: %d", resp.StatusCode)
	}
	var data struct {
		Status      string `json:"status"`
		InstanceID  string `json:"instanceId"`
		Model       string `json:"model"`
		ExpiresAt   string `json:"expiresAt"`
		RemainingMs int64  `json:"remainingMs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	if data.Status != "active" {
		return nil, fmt.Errorf("not active: %s", data.Status)
	}
	expiresAt, _ := time.Parse(time.RFC3339, data.ExpiresAt)
	return &FreebuffSession{
		InstanceID:  data.InstanceID,
		Model:       data.Model,
		ExpiresAt:   expiresAt,
		RemainingMs: data.RemainingMs,
	}, nil
}

func fbDeleteSession(client *http.Client, token string) {
	req, _ := http.NewRequest("DELETE", FREEBUFF_API_BASE+FREEBUFF_SESSION_PATH, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()

	// Clear all cached sessions for this token
	fbSessionCache.Lock()
	for k := range fbSessionCache.m {
		if strings.HasPrefix(k, token+":") {
			delete(fbSessionCache.m, k)
		}
	}
	fbSessionCache.Unlock()
}

// fbFireAdsAndStreak simulates the official CLI behavior (best-effort, don't block).
func fbFireAdsAndStreak(client *http.Client, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Ads impression
	adsBody, _ := json.Marshal(map[string]any{
		"provider":  "gravity",
		"sessionId": uuid.New().String(),
		"surface":   "waiting_room",
		"device": map[string]string{
			"os":       "linux",
			"timezone": "UTC",
			"locale":   "en-US",
		},
		"userAgent": "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0 Safari/537.36",
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", FREEBUFF_API_BASE+FREEBUFF_ADS_PATH, bytes.NewReader(adsBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
	}

	// Streak check-in
	req2, _ := http.NewRequestWithContext(ctx, "GET", FREEBUFF_API_BASE+FREEBUFF_STREAK_PATH, nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")
	if resp, err := client.Do(req2); err == nil {
		resp.Body.Close()
	}
}

// fbGetOrCreateRun returns a cached runId or creates a new one.
func fbGetOrCreateRun(client *http.Client, token, agentId string) (string, error) {
	cacheKey := token + ":" + agentId

	// Check cache
	fbRunCache.Lock()
	cached, ok := fbRunCache.m[cacheKey]
	fbRunCache.Unlock()
	if ok && time.Since(cached.CreatedAt) < FREEBUFF_RUN_CACHE_TTL {
		return cached.RunID, nil
	}

	// POST /api/v1/agent-runs
	body, _ := json.Marshal(map[string]any{
		"action":         "START",
		"agentId":        agentId,
		"ancestorRunIds": []string{},
	})
	req, _ := http.NewRequest("POST", FREEBUFF_API_BASE+FREEBUFF_RUN_PATH, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("start run: %d %s", resp.StatusCode, string(respBody))
	}

	var data struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(respBody, &data); err != nil {
		return "", err
	}
	if data.RunID == "" {
		return "", fmt.Errorf("no runId in response")
	}

	fbRunCache.Lock()
	fbRunCache.m[cacheKey] = fbRunEntry{RunID: data.RunID, CreatedAt: time.Now()}
	fbRunCache.Unlock()
	return data.RunID, nil
}

// fbProbeMe does GET /api/v1/me to verify token + get userId + email.
// ============================================================================
// DEVICE FLOW (Login URL generation + polling)
// ============================================================================

// FreebuffDeviceStart is the result of starting a device/login OAuth flow.
type FreebuffDeviceStart struct {
	State           string `json:"state"`
	AuthURL         string `json:"auth_url"`
	FingerprintID   string `json:"fingerprint_id"`
	FingerprintHash string `json:"fingerprint_hash"`
	ExpiresAt       int64  `json:"expires_at"`
}

// FreebuffDevicePoll is the result of one poll for tokens.
type FreebuffDevicePoll struct {
	Status      string `json:"status"` // pending | ready | error
	AuthToken   string `json:"auth_token,omitempty"`
	UserID      string `json:"user_id,omitempty"`
	Email       string `json:"email,omitempty"`
	Error       string `json:"error,omitempty"`
}

// fbStartDeviceAuth requests a fresh login URL from Freebuff.
// POST /api/auth/cli/code {fingerprintId} → {loginUrl, fingerprintHash, expiresAt}
func fbStartDeviceAuth() (*FreebuffDeviceStart, error) {
	// Generate random fingerprint ID
	fpID := "gw-" + fbRandomString(12)

	body, _ := json.Marshal(map[string]string{"fingerprintId": fpID})
	req, _ := http.NewRequest("POST", FREEBUFF_API_BASE+"/api/auth/cli/code", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")

	client := &http.Client{Timeout: FREEBUFF_SESSION_TIMEOUT}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fb device start: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fb device start [%d]: %s", resp.StatusCode, truncateLog(string(respBody), 200))
	}

	var data struct {
		FingerprintID   string `json:"fingerprintId"`
		FingerprintHash string `json:"fingerprintHash"`
		LoginURL        string `json:"loginUrl"`
		ExpiresAt       int64  `json:"expiresAt"`
	}
	if err := json.Unmarshal(respBody, &data); err != nil {
		return nil, fmt.Errorf("fb device start parse: %w", err)
	}
	if data.LoginURL == "" {
		return nil, fmt.Errorf("fb device start: missing loginUrl")
	}

	// Extract auth_code from loginUrl to use as state
	state := data.FingerprintID
	if parsed, err := parseAuthCode(data.LoginURL); err == nil && parsed != "" {
		state = parsed
	}

	slog.Info("fb device start ok", "module", "freebuff", "fingerprint", fpID[:16])
	return &FreebuffDeviceStart{
		State:           state,
		AuthURL:         data.LoginURL,
		FingerprintID:   data.FingerprintID,
		FingerprintHash: data.FingerprintHash,
		ExpiresAt:       data.ExpiresAt,
	}, nil
}

// fbPollDeviceAuth checks whether the user has completed browser login.
// GET /api/auth/cli/status?fingerprintId=...&fingerprintHash=...&expiresAt=...
func fbPollDeviceAuth(fingerprintID, fingerprintHash string, expiresAt int64) (*FreebuffDevicePoll, error) {
	params := url.Values{}
	params.Set("fingerprintId", fingerprintID)
	params.Set("fingerprintHash", fingerprintHash)
	params.Set("expiresAt", fmt.Sprintf("%d", expiresAt))

	req, _ := http.NewRequest("GET", FREEBUFF_API_BASE+"/api/auth/cli/status?"+params.Encode(), nil)
	req.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")

	client := &http.Client{Timeout: FREEBUFF_SESSION_TIMEOUT}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fb device poll: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	// Non-200 = pending
	if resp.StatusCode != 200 {
		return &FreebuffDevicePoll{Status: "pending"}, nil
	}

	var data struct {
		Message string `json:"message"`
		User    struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Email     string `json:"email"`
			AuthToken string `json:"authToken"`
		} `json:"user"`
		// Fallbacks for flat response shape
		AuthToken string `json:"authToken"`
		Token     string `json:"token"`
		UserID    string `json:"userId"`
		UID       string `json:"uid"`
		Email     string `json:"email"`
	}
	if err := json.Unmarshal(respBody, &data); err != nil {
		return &FreebuffDevicePoll{Status: "pending"}, nil
	}

	// Check for success — token can be in user.authToken (nested) or root authToken
	token := data.User.AuthToken
	if token == "" {
		token = data.AuthToken
	}
	if token == "" {
		token = data.Token
	}

	if data.Message == "Authentication successful!" && token != "" {
		// Use user fields from response, fall back to /api/v1/me probe
		userID := data.User.ID
		email := data.User.Email
		if userID == "" || email == "" {
			uid2, email2, _ := fbProbeMe(token)
			if userID == "" {
				userID = uid2
			}
			if email == "" {
				email = email2
			}
		}

		slog.Info("fb device poll ready", "module", "freebuff", "fingerprint", fingerprintID[:16], "user_id", userID)
		return &FreebuffDevicePoll{
			Status:    "ready",
			AuthToken: token,
			UserID:    userID,
			Email:     email,
		}, nil
	}

	// Still pending
	return &FreebuffDevicePoll{Status: "pending"}, nil
}

// StartFreebuffDeviceAuth is the exported wrapper for handlers.
func StartFreebuffDeviceAuth() (*FreebuffDeviceStart, error) {
	return fbStartDeviceAuth()
}

// PollFreebuffDeviceAuth is the exported wrapper for handlers.
func PollFreebuffDeviceAuth(fingerprintID, fingerprintHash string, expiresAt int64) (*FreebuffDevicePoll, error) {
	return fbPollDeviceAuth(fingerprintID, fingerprintHash, expiresAt)
}

// parseAuthCode extracts the auth_code query param from a login URL.
func parseAuthCode(loginURL string) (string, error) {
	parsed, err := url.Parse(loginURL)
	if err != nil {
		return "", err
	}
	return parsed.Query().Get("auth_code"), nil
}

func fbProbeMe(token string) (userID, email string, err error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", FREEBUFF_API_BASE+FREEBUFF_ME_PATH, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("probe /me: %d", resp.StatusCode)
	}
	var data struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", "", err
	}
	return data.ID, data.Email, nil
}

// ============================================================================
// BODY TRANSFORMATION
// ============================================================================

// fbTransform modifies the request body for Freebuff upstream.
func fbTransform(bodyMap map[string]any, upstreamModel, instanceID, runID string) []byte {
	// 1. Prepend Buffy system prompt if needed
	fbEnsureBuffyPrefix(bodyMap)

	// 2. Inject end_turn tool if tools present
	fbInjectEndTurnTool(bodyMap)

	// 3. Set upstream model (strip fb/ prefix already done by caller)
	bodyMap["model"] = upstreamModel

	// 4. Always stream upstream
	bodyMap["stream"] = true

	// 5. Provider data_collection deny
	bodyMap["provider"] = map[string]string{"data_collection": "deny"}

	// 6. Stop sequence
	bodyMap["stop"] = []string{`"cb_easp"`}

	// 7. Default max_tokens to 384K (DeepSeek V4 Flash max output) if client didn't set it.
	// Reasoning models burn tokens for reasoning first — small max_tokens causes
	// finish_reason=length with empty content.
	// Then clamp: prompt_tokens + max_tokens must not exceed 1M (model context limit).
	const fbMaxContext = 1048576 // 1M tokens (DeepSeek V4 Flash)
	if _, has := bodyMap["max_tokens"]; !has {
		if _, has2 := bodyMap["max_completion_tokens"]; !has2 {
			bodyMap["max_tokens"] = 393216 // 384K default
		}
	}
	// Clamp max_tokens so prompt + completion ≤ 1M
	// max_tokens can be float64 (from client JSON) or int (set by us above)
	var mtVal int
	switch v := bodyMap["max_tokens"].(type) {
	case float64:
		mtVal = int(v)
	case int:
		mtVal = v
	case json.Number:
		n, _ := v.Int64()
		mtVal = int(n)
	default:
		mtVal = 0
	}
	if mtVal > 0 {
		// Rough prompt token estimate: count message content chars / 4
		estimatedPromptTokens := 0
		if msgs, ok := bodyMap["messages"].([]any); ok {
			for _, m := range msgs {
				if mm, ok := m.(map[string]any); ok {
					if c, ok := mm["content"].(string); ok {
						estimatedPromptTokens += len(c) / 4
					}
				}
			}
		}
		maxAllowed := fbMaxContext - estimatedPromptTokens
		if maxAllowed < 1000 {
			maxAllowed = 1000
		}
		if mtVal > maxAllowed {
			bodyMap["max_tokens"] = maxAllowed
		}
	}

	// 7. codebuff_metadata
	clientID := "wf-" + fbRandomString(8)
	traceID := uuid.New().String()
	bodyMap["codebuff_metadata"] = map[string]string{
		"run_id":                runID,
		"client_id":             clientID,
		"cost_mode":             "free",
		"freebuff_instance_id":  instanceID,
		"trace_session_id":      traceID,
	}

	// Remove fields that Freebuff upstream doesn't understand
	delete(bodyMap, "reasoning")
	delete(bodyMap, "extra_body")

	out, _ := json.Marshal(bodyMap)
	return out
}

// fbEnsureBuffyPrefix ensures the first system message starts with the Buffy prefix.
func fbEnsureBuffyPrefix(bodyMap map[string]any) {
	msgs, ok := bodyMap["messages"].([]any)
	if !ok || len(msgs) == 0 {
		// No messages — prepend a system message
		bodyMap["messages"] = []any{
			map[string]any{"role": "system", "content": FREEBUFF_BUFFY_PREFIX},
		}
		return
	}

	// Check first message
	first, ok := msgs[0].(map[string]any)
	if !ok {
		return
	}

	role, _ := first["role"].(string)
	if role != "system" {
		// Prepend a new system message
		newMsgs := append([]any{
			map[string]any{"role": "system", "content": FREEBUFF_BUFFY_PREFIX},
		}, msgs...)
		bodyMap["messages"] = newMsgs
		return
	}

	// System message exists — check content
	content, ok := first["content"].(string)
	if !ok {
		// Content is array (multimodal) — prepend a separate system message
		newMsgs := append([]any{
			map[string]any{"role": "system", "content": FREEBUFF_BUFFY_PREFIX},
		}, msgs...)
		bodyMap["messages"] = newMsgs
		return
	}

	if !strings.HasPrefix(content, FREEBUFF_BUFFY_PREFIX) {
		// Prepend Buffy prefix to existing content
		first["content"] = FREEBUFF_BUFFY_PREFIX + "\n\n" + content
		msgs[0] = first
		bodyMap["messages"] = msgs
	}
}

// fbInjectEndTurnTool injects a dummy end_turn tool to bypass foreign client detection.
func fbInjectEndTurnTool(bodyMap map[string]any) {
	tools, ok := bodyMap["tools"].([]any)
	if !ok || len(tools) == 0 {
		return
	}

	// Check if end_turn already present
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tm["function"].(map[string]any)
		if !ok {
			continue
		}
		if name, _ := fn["name"].(string); name == "end_turn" {
			return // already present
		}
	}

	// Inject end_turn
	endTurn := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "end_turn",
			"description": "Signal the end of the current task.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
	bodyMap["tools"] = append(tools, endTurn)
}

func fbRandomString(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ============================================================================
// PROXY FREEBUFF
// ============================================================================

// fbIsDisabled checks if FREEBUFF_DISABLED env is set.
var fbDisabled = os.Getenv("FREEBUFF_DISABLED") == "1"

// ProxyFreebuff handles chat completions for the Freebuff upstream.
func ProxyFreebuff(c *gin.Context, body []byte, bodyMap map[string]any, am *FreebuffAccountManager, clientStream bool, hc *HealthChecker) {
	if fbDisabled || am == nil || am.Len() == 0 {
		c.JSON(503, gin.H{"error": "freebuff upstream not available"})
		c.Set("error_msg", "freebuff not available")
		errJSON, _ := json.Marshal(gin.H{"error": "freebuff upstream not available"})
		c.Set("response_body", json.RawMessage(errJSON))
		return
	}

	// Circuit breaker
	if hc != nil && hc.FB != nil && !hc.FB.CanRequest() {
		hc.FB.RecordRequest(0, fmt.Errorf("circuit open"))
		c.JSON(503, gin.H{"error": "freebuff upstream circuit breaker open"})
		c.Set("error_msg", "fb circuit breaker open")
		errJSON, _ := json.Marshal(gin.H{"error": "freebuff circuit breaker open"})
		c.Set("response_body", json.RawMessage(errJSON))
		return
	}

	// Resolve model config
	gatewayModel, _ := bodyMap["model"].(string)
	mc := fbModelConfig(gatewayModel)
	upstreamModel := mc.Upstream
	agentID := mc.Agent

	total := am.Len()
	reqStart := time.Now()

	// Get HTTP client (through proxy pool if configured)
	client, proxyID := getClient(upstreamClient, "freebuff")

	var lastErr string

	for attempt := 0; attempt < total; attempt++ {
		if err := c.Request.Context().Err(); err != nil {
			slog.Debug("client cancelled", "module", "freebuff", "attempt", attempt+1)
			return
		}

		acc, err := am.Next()
		if err != nil {
			break
		}

		// Per-account serialization — Freebuff upstream breaks if concurrent >1
		acc.mu.Lock()

		// 1. Get/create session
		sess, err := fbGetOrCreateSession(client, acc.Token, acc.UserID, upstreamModel)
		if err != nil {
			acc.mu.Unlock()
			lastErr = fmt.Sprintf("session: %v", err)
			slog.Warn("fb session failed", "module", "freebuff", "attempt", attempt+1, "error", err)
			continue
		}

		// 2. Get/create run
		runID, err := fbGetOrCreateRun(client, acc.Token, agentID)
		if err != nil {
			acc.mu.Unlock()
			lastErr = fmt.Sprintf("run: %v", err)
			slog.Warn("fb run failed", "module", "freebuff", "attempt", attempt+1, "error", err)
			continue
		}

		// 3. Transform body
		// Deep copy bodyMap to avoid mutation across attempts
		bodyCopy := make(map[string]any)
		bodyBytes, _ := json.Marshal(bodyMap)
		json.Unmarshal(bodyBytes, &bodyCopy)

		transformedBody := fbTransform(bodyCopy, upstreamModel, sess.InstanceID, runID)

		// 4. Build request
		chatReq, _ := http.NewRequestWithContext(
			c.Request.Context(),
			"POST",
			FREEBUFF_API_BASE+FREEBUFF_CHAT_PATH,
			bytes.NewReader(transformedBody),
		)
		chatReq.Header.Set("Authorization", "Bearer "+acc.Token)
		chatReq.Header.Set("Content-Type", "application/json")
		chatReq.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")
		chatReq.Header.Set("x-freebuff-instance-id", sess.InstanceID)
		if acc.UserID != "" {
			chatReq.Header.Set("x-freebuff-acting-user-id", acc.UserID)
		}

		// 5. Send request
		timeout := FREEBUFF_UPSTREAM_TIMEOUT
		if !clientStream {
			timeout = FREEBUFF_NONSTREAM_TIMEOUT
		}
		chatClient := &http.Client{Timeout: timeout}
		if client != nil {
			chatClient = client
			chatClient.Timeout = timeout
		}

		resp, err := chatClient.Do(chatReq)
		if err != nil {
			acc.mu.Unlock()
			markProxyResult(proxyID, err, 0)
			lastErr = fmt.Sprintf("network: %v", err)
			slog.Warn("fb network failed", "module", "freebuff", "attempt", attempt+1, "error", err)
			continue
		}
		markProxyResult(proxyID, nil, resp.StatusCode)

		// 6. Handle 428 (stale session) — recreate + retry once
		if resp.StatusCode == 428 {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if strings.Contains(string(respBody), "waiting_room_required") {
				slog.Debug("fb 428 stale session, recreating", "module", "freebuff", "attempt", attempt+1)
				fbDeleteSession(client, acc.Token)
				sess, err = fbGetOrCreateSession(client, acc.Token, acc.UserID, upstreamModel)
				if err != nil {
					acc.mu.Unlock()
					lastErr = fmt.Sprintf("session recreate: %v", err)
					continue
				}
				// Retry with new session
				bodyCopy2 := make(map[string]any)
				json.Unmarshal(bodyBytes, &bodyCopy2)
				transformedBody = fbTransform(bodyCopy2, upstreamModel, sess.InstanceID, runID)
				chatReq, _ = http.NewRequestWithContext(c.Request.Context(), "POST", FREEBUFF_API_BASE+FREEBUFF_CHAT_PATH, bytes.NewReader(transformedBody))
				chatReq.Header.Set("Authorization", "Bearer "+acc.Token)
				chatReq.Header.Set("Content-Type", "application/json")
				chatReq.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")
				chatReq.Header.Set("x-freebuff-instance-id", sess.InstanceID)
				if acc.UserID != "" {
					chatReq.Header.Set("x-freebuff-acting-user-id", acc.UserID)
				}
				resp, err = chatClient.Do(chatReq)
				if err != nil {
					acc.mu.Unlock()
					lastErr = fmt.Sprintf("network retry: %v", err)
					continue
				}
			} else {
				acc.mu.Unlock()
				lastErr = fmt.Sprintf("428: %s", truncateLog(string(respBody), 200))
				continue
			}
		}

		// 7. Handle 429 (rate limited) — cooldown account
		if resp.StatusCode == 429 {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			cooldown := fbParseCooldown(string(respBody))
			acc.CooldownUntil = time.Now().Add(cooldown)
			am.SaveAccount(acc)
			acc.mu.Unlock()
			lastErr = fmt.Sprintf("429: %s", truncateLog(string(respBody), 200))
			slog.Warn("fb rate limited, cooldown", "module", "freebuff", "cooldown", cooldown, "body", truncateLog(string(respBody), 200))
			continue
		}

		// 8. Handle 5xx — try next account
		if resp.StatusCode >= 500 {
			resp.Body.Close()
			acc.mu.Unlock()
			lastErr = fmt.Sprintf("upstream %d", resp.StatusCode)
			slog.Warn("fb upstream error", "module", "freebuff", "status", resp.StatusCode)
			continue
		}

		// 9. Success — stream passthrough or aggregate
		if hc != nil && hc.FB != nil {
			hc.FB.RecordRequest(time.Since(reqStart), nil)
		}
		c.Set("upstream_account", acc.Email)
		acc.mu.Unlock()

		defer resp.Body.Close()

		if clientStream {
			// Stream passthrough with usage capture
			copyUpstreamHeaders(c.Writer.Header(), resp.Header)
			c.Writer.WriteHeader(resp.StatusCode)
			flusher, _ := c.Writer.(http.Flusher)
			reader := bufio.NewReader(resp.Body)

			var streamUsage map[string]any
			var streamBuf strings.Builder
			for {
				line, err := reader.ReadString('\n')
				if line != "" {
					c.Writer.Write([]byte(line))
					if flusher != nil {
						flusher.Flush()
					}
					// Capture for response_body + usage extraction
					streamBuf.WriteString(line)
					trimmed := strings.TrimSpace(line)
					if strings.HasPrefix(trimmed, "data: ") {
						data := strings.TrimPrefix(trimmed, "data: ")
						if data != "[DONE]" && data != "" {
							var chunk map[string]any
							if json.Unmarshal([]byte(data), &chunk) == nil {
								// Unwrap envelope
								if inner, ok := chunk["data"].(map[string]any); ok {
									if _, hasChoices := inner["choices"]; hasChoices {
										chunk = inner
									}
								}
								if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
									streamUsage = u
								}
							}
						}
					}
				}
				if err != nil {
					break
				}
			}

			// Set tokens + response_body for logging
			if streamUsage != nil {
				if pt, ok := streamUsage["prompt_tokens"].(float64); ok {
					c.Set("tokens_in", int(pt))
				}
				if ct, ok := streamUsage["completion_tokens"].(float64); ok {
					c.Set("tokens_out", int(ct))
				}
				// Store stream body for cache_hit_pct extraction
				c.Set("response_body", json.RawMessage(streamBuf.String()))
			}

			// Increment hourly session counter
			am.IncrementSessionCount(acc)
		} else {
			// Aggregate SSE → non-stream
			agg := fbStreamToNonStream(resp.Body, upstreamModel)
			// Set tokens + response_body for logging
			if usage, ok := agg["usage"].(map[string]any); ok {
				if pt, ok := usage["prompt_tokens"].(float64); ok {
					c.Set("tokens_in", int(pt))
				}
				if ct, ok := usage["completion_tokens"].(float64); ok {
					c.Set("tokens_out", int(ct))
				}
			}
			if aggBytes, err := json.Marshal(agg); err == nil {
				c.Set("response_body", json.RawMessage(aggBytes))
			}
			// Set output_text for dashboard preview
			if msg, ok := agg["choices"].([]any); ok && len(msg) > 0 {
				if choice, ok := msg[0].(map[string]any); ok {
					if m, ok := choice["message"].(map[string]any); ok {
						if content, ok := m["content"].(string); ok {
							c.Set("output_text", content)
						}
					}
				}
			}
			c.JSON(200, agg)

			// Increment hourly session counter
			am.IncrementSessionCount(acc)
		}
		return
	}

	// All accounts failed
	if hc != nil && hc.FB != nil {
		hc.FB.RecordRequest(time.Since(reqStart), fmt.Errorf("all accounts failed"))
	}
	c.JSON(503, gin.H{"error": "all freebuff accounts failed: " + lastErr})
	c.Set("error_msg", "fb all accounts failed: "+lastErr)
	errJSON, _ := json.Marshal(gin.H{"error": "all freebuff accounts failed"})
	c.Set("response_body", json.RawMessage(errJSON))
}

// fbStreamToNonStream aggregates upstream SSE into a single OpenAI response.
func fbStreamToNonStream(body io.Reader, model string) map[string]any {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var content, reasoning, finishReason, respID, respModel string
	var usage map[string]any

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" || data == "" {
			continue
		}

		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}

		// Unwrap {data: {...}} envelope
		if inner, ok := chunk["data"].(map[string]any); ok {
			if _, hasChoices := inner["choices"]; hasChoices {
				chunk = inner
			}
		}

		if id, ok := chunk["id"].(string); ok && id != "" {
			respID = id
		}
		if m, ok := chunk["model"].(string); ok && m != "" {
			respModel = m
		}
		if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
			usage = u
		}

		choices, ok := chunk["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		choice, ok := choices[0].(map[string]any)
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		if c, ok := delta["content"].(string); ok {
			content += c
		}
		if r, ok := delta["reasoning_content"].(string); ok {
			reasoning += r
		}
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			finishReason = fr
		}
	}

	if finishReason == "" {
		finishReason = "stop"
	}
	if respID == "" {
		respID = "fb_" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	if respModel == "" {
		respModel = model
	}
	if usage == nil {
		usage = map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}

	msg := map[string]any{"role": "assistant"}
	if content != "" {
		msg["content"] = content
	} else if reasoning != "" {
		msg["content"] = reasoning
		msg["reasoning_used_as_content"] = true
	} else {
		msg["content"] = ""
	}
	if reasoning != "" && content != "" {
		msg["reasoning_content"] = reasoning
	}

	return map[string]any{
		"id":      respID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   respModel,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       msg,
				"finish_reason": finishReason,
				"logprobs":      nil,
			},
		},
		"usage": usage,
	}
}

// fbParseCooldown extracts cooldown duration from 429 response body.
func fbParseCooldown(body string) time.Duration {
	// Try JSON retryAfterMs
	var data struct {
		Error struct {
			RetryAfterMs int64 `json:"retryAfterMs"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &data) == nil && data.Error.RetryAfterMs > 0 {
		return time.Duration(data.Error.RetryAfterMs) * time.Millisecond
	}

	// Try "try again in Xh Ym Zs" text
	lower := strings.ToLower(body)
	if strings.Contains(lower, "try again in") {
		// Parse hours, minutes, seconds
		h, m, s := 0, 0, 0
		if idx := strings.Index(lower, "try again in"); idx >= 0 {
			rest := lower[idx+len("try again in"):]
			fmt.Sscanf(rest, "%dh %dm %ds", &h, &m, &s)
			if h == 0 && m == 0 && s == 0 {
				fmt.Sscanf(rest, "%dh", &h)
				if h == 0 {
					fmt.Sscanf(rest, "%dm", &m)
				}
			}
			if h > 0 || m > 0 || s > 0 {
				return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second
			}
		}
	}

	// Default 60s
	return 60 * time.Second
}

// fbMaskToken masks a token for display.
func fbMaskToken(token string) string {
	if len(token) <= 12 {
		return token
	}
	return token[:8] + "..." + token[len(token)-4:]
}

// ============================================================================
// UTIL (avoid importing math/big for crypto/rand)
// ============================================================================

var _ = big.NewInt // keep import if needed elsewhere

