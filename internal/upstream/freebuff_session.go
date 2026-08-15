package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func (am *FreebuffAccountManager) SyncQuota(acc *FreebuffAccount) error {
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", FREEBUFF_API_BASE+FREEBUFF_SESSION_PATH, nil)
	req.Header.Set("Authorization", "Bearer "+acc.Token)
	req.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")
	// Force the FULL quota snapshot (including zero-limit models like GLM
	// referral) — without this the server may return a compact response.
	req.Header.Set("x-freebuff-include-unused-rate-limits", "1")
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
		Status             string `json:"status"`
		AccessTier         string `json:"accessTier"` // "full" | "limited" | "blocked"
		CountryCode        string `json:"countryCode"`
		CountryBlockReason string `json:"countryBlockReason"`
		RateLimitsByModel  map[string]struct {
			Limit                float64 `json:"limit"`
			RecentCount          float64 `json:"recentCount"`
			ResetAt              string  `json:"resetAt"`
			Period               string  `json:"period"`
			WindowHours          int     `json:"windowHours"`
			EntitlementBreakdown struct {
				Base     float64 `json:"base"`
				Referral float64 `json:"referral"`
				Streak   float64 `json:"streak"`
			} `json:"entitlementBreakdown"`
		} `json:"rateLimitsByModel"`
		RateLimit struct {
			EntitlementBreakdown struct {
				Base     float64 `json:"base"`
				Referral float64 `json:"referral"`
				Streak   float64 `json:"streak"`
			} `json:"entitlementBreakdown"`
		} `json:"rateLimit"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return fmt.Errorf("quota sync parse: %w", err)
	}

	// Banned account — upstream reports status:"banned" in GET /session.
	if data.Status == "banned" {
		return ErrFBBanned
	}

	// Always persist tier/country info (present even without an active session)
	acc.mu.Lock()
	if data.AccessTier != "" {
		acc.Tier = data.AccessTier
	}
	acc.CountryCode = data.CountryCode
	acc.CountryBlockReason = data.CountryBlockReason
	acc.EntitlementBase = data.RateLimit.EntitlementBreakdown.Base
	acc.EntitlementReferral = data.RateLimit.EntitlementBreakdown.Referral
	acc.EntitlementStreak = data.RateLimit.EntitlementBreakdown.Streak

	// Per-model quota snapshot (full map incl. zero-limit models like GLM referral)
	if len(data.RateLimitsByModel) > 0 {
		qbm := make(map[string]FbModelQuota, len(data.RateLimitsByModel))
		for name, rl := range data.RateLimitsByModel {
			qbm[name] = FbModelQuota{
				Limit:               rl.Limit,
				RecentCount:         rl.RecentCount,
				ResetAt:             rl.ResetAt,
				Period:              rl.Period,
				EntitlementBase:     rl.EntitlementBreakdown.Base,
				EntitlementReferral: rl.EntitlementBreakdown.Referral,
				EntitlementStreak:   rl.EntitlementBreakdown.Streak,
			}
		}
		acc.QuotaByModel = qbm
	}

	// Use deepseek-v4-flash as the reference model (available in all tiers)
	rl, ok := data.RateLimitsByModel["deepseek/deepseek-v4-flash"]
	if !ok || data.Status == "none" {
		// No active session — account is fresh, full quota available (0/6)
		acc.QuotaRecent = 0
		acc.QuotaLimit = 6 // default free tier limit
		acc.QuotaPeriod = "pacific_day"
		acc.QuotaSyncedAt = time.Now()
		acc.CooldownUntil = time.Time{} // not exhausted
		acc.mu.Unlock()
		am.SaveAccount(acc)
		slog.Debug("fb quota: no session, fresh account (0/6)", "module", "freebuff", "token", acc.Token[:8]+"...", "tier", acc.Tier)
		return nil
	}

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
	slog.Info("fb quota synced", "module", "freebuff", "token", acc.Token[:8]+"...",
		"recent", rl.RecentCount, "limit", rl.Limit, "reset", rl.ResetAt,
		"tier", data.AccessTier, "country", data.CountryCode)
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
			if errors.Is(err, ErrFBBanned) {
				am.MarkBanned(acc.Token)
				slog.Warn("fb account banned (quota sync)", "module", "freebuff", "token", acc.Token[:8]+"...")
				continue
			}
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

// FreebuffStreakInterval returns the streak check-in interval.
// Default 24h; env FREEBUFF_STREAK_INTERVAL overrides (min 1h).
func FreebuffStreakInterval() time.Duration {
	if v := os.Getenv("FREEBUFF_STREAK_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= time.Hour {
			return d
		}
	}
	return 24 * time.Hour
}

// FBStreakWorker periodically fires ads + streak check-in for ALL Freebuff
// accounts so daily streaks don't break between sessions. First run happens
// shortly after startup (after a short delay so startup burst settles),
// then every `interval`. Best-effort: failures are logged, never block.
func FBStreakWorker(ctx context.Context, am *FreebuffAccountManager, interval time.Duration) {
	if interval < time.Hour {
		interval = 24 * time.Hour
	}
	time.Sleep(20 * time.Second)
	am.StreakCheckinOnce()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			am.StreakCheckinOnce()
		}
	}
}

// StreakCheckinOnce fires ads + streak check-in for every Freebuff account
// with a token. Returns (checked, failed). Concurrency 3; 10s client timeout.
func (am *FreebuffAccountManager) StreakCheckinOnce() (int, int) {
	am.mu.RLock()
	var accounts []*FreebuffAccount
	for _, acc := range am.accounts {
		accounts = append(accounts, acc)
	}
	am.mu.RUnlock()

	client := &http.Client{Timeout: 10 * time.Second}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	var mu sync.Mutex
	checked, failed := 0, 0
	for _, acc := range accounts {
		acc.mu.Lock()
		token := acc.Token
		acc.mu.Unlock()
		if token == "" {
			continue
		}
		wg.Add(1)
		go func(tok string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := fbFireAdsAndStreak(client, tok); err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				slog.Warn("fb streak check-in failed", "module", "fb-streak", "token", tok[:8]+"...", "error", err)
				return
			}
			mu.Lock()
			checked++
			mu.Unlock()
			slog.Info("fb streak check-in ok", "module", "fb-streak", "token", tok[:8]+"...")
		}(token)
	}
	wg.Wait()
	if len(accounts) > 0 {
		slog.Info("fb streak check-in done", "module", "fb-streak", "checked", checked, "failed", failed, "total", len(accounts))
	}
	return checked, failed
}

// SESSION + RUN CACHE (in-memory, ephemeral)

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

// redis returns the underlying Redis client (nil-safe). Session/run cache is
// persisted to Redis so it survives gateway restarts; the in-memory maps act
// as a fast L1 layer.
func (am *FreebuffAccountManager) redis() *redis.Client {
	if am == nil || am.db == nil || !am.db.Ready() {
		return nil
	}
	return am.db.Redis()
}

// cachedSession checks the in-memory L1 cache first, then Redis (L2).
// Returns nil when no reusable session is found.
func (am *FreebuffAccountManager) cachedSession(token, model string) *FreebuffSession {
	cacheKey := fbSessionKeyPrefix + token + ":" + model

	fbSessionCache.Lock()
	cached, ok := fbSessionCache.m[cacheKey]
	fbSessionCache.Unlock()
	if ok && time.Until(cached.ExpiresAt) > FREEBUFF_SESSION_MIN_REMAINING {
		return cached
	}

	rdb := am.redis()
	if rdb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		raw, err := rdb.Get(ctx, cacheKey).Result()
		if err == nil && raw != "" {
			var sess FreebuffSession
			if json.Unmarshal([]byte(raw), &sess) == nil && sess.InstanceID != "" &&
				time.Until(sess.ExpiresAt) > FREEBUFF_SESSION_MIN_REMAINING {
				fbSessionCache.Lock()
				fbSessionCache.m[cacheKey] = &sess
				fbSessionCache.Unlock()
				return &sess
			}
		}
	}
	return nil
}

// storeSession writes the session to both the in-memory cache and Redis
// (Redis TTL = session expiry, so the key auto-dies when the session does).
func (am *FreebuffAccountManager) storeSession(token, model string, sess *FreebuffSession) {
	cacheKey := fbSessionKeyPrefix + token + ":" + model

	fbSessionCache.Lock()
	fbSessionCache.m[cacheKey] = sess
	fbSessionCache.Unlock()

	rdb := am.redis()
	if rdb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	raw, err := json.Marshal(sess)
	if err != nil {
		return
	}
	ttl := time.Until(sess.ExpiresAt)
	if ttl < time.Second {
		ttl = time.Second
	}
	if err := rdb.Set(ctx, cacheKey, raw, ttl).Err(); err != nil {
		slog.Warn("fb session cache set failed", "module", "freebuff", "error", err)
	}
}

// cachedRun checks the in-memory run cache, then Redis (L2).
func (am *FreebuffAccountManager) cachedRun(token, agentID string) (string, bool) {
	cacheKey := fbRunKeyPrefix + token + ":" + agentID

	fbRunCache.Lock()
	cached, ok := fbRunCache.m[cacheKey]
	fbRunCache.Unlock()
	if ok && time.Since(cached.CreatedAt) < FREEBUFF_RUN_CACHE_TTL {
		return cached.RunID, true
	}

	rdb := am.redis()
	if rdb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		raw, err := rdb.Get(ctx, cacheKey).Result()
		if err == nil && raw != "" {
			var entry fbRunEntry
			if json.Unmarshal([]byte(raw), &entry) == nil && entry.RunID != "" &&
				time.Since(entry.CreatedAt) < FREEBUFF_RUN_CACHE_TTL {
				fbRunCache.Lock()
				fbRunCache.m[cacheKey] = entry
				fbRunCache.Unlock()
				return entry.RunID, true
			}
		}
	}
	return "", false
}

// storeRun writes the run to both in-memory and Redis (TTL = run cache TTL).
func (am *FreebuffAccountManager) storeRun(token, agentID, runID string) {
	cacheKey := fbRunKeyPrefix + token + ":" + agentID
	entry := fbRunEntry{RunID: runID, CreatedAt: time.Now()}

	fbRunCache.Lock()
	fbRunCache.m[cacheKey] = entry
	fbRunCache.Unlock()

	rdb := am.redis()
	if rdb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	raw, _ := json.Marshal(entry)
	if err := rdb.Set(ctx, cacheKey, raw, FREEBUFF_RUN_CACHE_TTL).Err(); err != nil {
		slog.Warn("fb run cache set failed", "module", "freebuff", "error", err)
	}
}

// fbSessionCreateError classifies a non-200 POST /session response.
// 403 {"status":"banned"} → ErrFBBanned (permanent); everything else → generic.
func fbSessionCreateError(statusCode int, respBody []byte) error {
	if statusCode == 403 && strings.Contains(string(respBody), `"banned"`) {
		return ErrFBBanned
	}
	return fmt.Errorf("POST session: %d %s", statusCode, string(respBody))
}

// fbGetOrCreateSession returns an active session, creating one if needed.
// Session cache is L1 in-memory + L2 Redis (persistent across restarts).
func (am *FreebuffAccountManager) fbGetOrCreateSession(client *http.Client, token, userID, model string) (*FreebuffSession, error) {
	// Check cache (L1 memory, then L2 Redis)
	if cached := am.cachedSession(token, model); cached != nil {
		return cached, nil
	}

	// GET current session (0 cost — doesn't create one)
	sess, err := fbGetSession(client, token)
	if err == nil && sess != nil && sess.Model == model && time.Until(sess.ExpiresAt) > FREEBUFF_SESSION_MIN_REMAINING {
		am.storeSession(token, model, sess)
		return sess, nil
	}

	// If model mismatch, delete old session
	if sess != nil && sess.Model != model {
		am.fbDeleteSession(client, token)
	}

	// Fire ads + streak (best-effort)
	go func() {
		_ = fbFireAdsAndStreak(client, token)
	}()

	// POST new session
	sess, err = fbCreateSession(client, token, model)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	am.storeSession(token, model, sess)
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
		if data.Status == "banned" {
			return nil, ErrFBBanned
		}
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
		return nil, fbSessionCreateError(resp.StatusCode, respBody)
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

func (am *FreebuffAccountManager) fbDeleteSession(client *http.Client, token string) {
	// Clear all cached sessions for this token FIRST (memory + Redis) so a
	// stale cache entry can never survive — even if the upstream DELETE fails.
	fbSessionCache.Lock()
	for k := range fbSessionCache.m {
		if strings.HasPrefix(k, fbSessionKeyPrefix+token+":") {
			delete(fbSessionCache.m, k)
		}
	}
	fbSessionCache.Unlock()

	rdb := am.redis()
	if rdb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		keys, err := rdb.Keys(ctx, fbSessionKeyPrefix+token+":*").Result()
		if err == nil && len(keys) > 0 {
			if derr := rdb.Del(ctx, keys...).Err(); derr != nil {
				slog.Warn("fb session cache del failed", "module", "freebuff", "error", derr)
			}
		}
	}

	// Best-effort upstream DELETE
	req, _ := http.NewRequest("DELETE", FREEBUFF_API_BASE+FREEBUFF_SESSION_PATH, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
	}
}

// fbFireAdsAndStreak simulates the official CLI behavior (best-effort, don't block).
// Returns the first error encountered (nil when both calls succeed or are skipped).
func fbFireAdsAndStreak(client *http.Client, token string) error {
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
	if resp, err := client.Do(req); err != nil {
		return fmt.Errorf("ads: %w", err)
	} else {
		resp.Body.Close()
	}

	// Streak check-in
	req2, _ := http.NewRequestWithContext(ctx, "GET", FREEBUFF_API_BASE+FREEBUFF_STREAK_PATH, nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("User-Agent", "Freebuff-CLI/0.0.142")
	if resp, err := client.Do(req2); err != nil {
		return fmt.Errorf("streak: %w", err)
	} else {
		resp.Body.Close()
	}
	return nil
}

// fbGetOrCreateRun returns a cached runId or creates a new one.
// Run cache is L1 in-memory + L2 Redis (TTL 10min).
func (am *FreebuffAccountManager) fbGetOrCreateRun(client *http.Client, token, agentId string) (string, error) {
	// Check cache (L1 memory, then L2 Redis)
	if runID, ok := am.cachedRun(token, agentId); ok {
		return runID, nil
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

	am.storeRun(token, agentId, data.RunID)
	return data.RunID, nil
}
