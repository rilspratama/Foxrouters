// Package db is the Redis (hot state) + ClickHouse (persistent history)
// integration layer for FoxRouters.
//
//   Redis: account tokens, CB key credits, rate limit buckets — sub-ms reads.
//   ClickHouse: request_logs, token_refresh_history, account_events — full-body history.
//
// This package deliberately does NOT depend on the concrete domain types
// (GrokAccount, GatewayKeyInfo, CBKey). Instead it defines a small set of
// DTOs — GrokAccountDTO, GatewayKeyDTO, CBKeyDTO — that callers convert
// to/from before persistence. That keeps the import graph one-directional
// (auth/upstream → db) and avoids any circular-import trap.
package db

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis key prefixes (exported so upstream packages can compose their own
// keys/scan patterns if they need to).
const (
	RK_GROK_ACCOUNT = "grok:account:" // HASH: access_token, refresh_token, expires_at, disabled, etc.
	RK_CB_KEY       = "cb:key:"       // HASH: credits_used, total_requests, disabled
	RK_GATEWAY_KEY  = "gw:key:"       // HASH: name, total_requests
	RK_RATE_LIMIT   = "rate:"         // STRING: token bucket state per client key

	// Custom-model / alias registry (v1.3.0). Both are Redis HASHes at a
	// single well-known key — field = id, value = JSON config or target.
	RK_CUSTOM_MODELS  = "custom_models"  // HASH field=model_id value=CustomModel JSON
	RK_CUSTOM_ALIASES = "custom_aliases" // HASH field=alias      value=target model_id
	RK_COMBOS         = "combos"         // HASH field=combo_name value=Combo JSON (v1.4.0)
	RK_COMBO_COUNTER  = "combo:counter:" // STRING prefix, atomic INCR for round-robin

	// Proxy pool (v1.5.0) — dashboard-managed HTTP/SOCKS5 proxies for upstream calls.
	RK_PROXY         = "fr:proxy:"         // HASH prefix: fr:proxy:<id> (fields: protocol, host, port, …)
	RK_PROXY_ENABLED = "fr:proxy:enabled"  // SET of enabled proxy IDs (fast round-robin selection)
	RK_PROXY_RR      = "fr:proxy:rr"       // STRING atomic INCR for round-robin index

	LOG_BUFFER_SIZE    = 10000
	LOG_FLUSH_INTERVAL = 2 * time.Second
)

// maskRedisKey masks the credential body of a Redis key so full API keys never
// leak into slog output. Given e.g. "cb:key:ck_abcd1234...wxyz", returns
// "cb:key:ck_abcd1...wxyz". Preserves the prefix so operators can still tell
// which pool the failure came from.
func maskRedisKey(rk string) string {
	// find the last ':' — everything after it is the credential body
	i := strings.LastIndex(rk, ":")
	if i < 0 || i == len(rk)-1 {
		return rk
	}
	prefix, body := rk[:i+1], rk[i+1:]
	if len(body) > 12 {
		return prefix + body[:8] + "..." + body[len(body)-4:]
	}
	return prefix + "***"
}

// envOr returns os.Getenv(key) if set, otherwise def.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// redisConfig resolves Redis connection from env:
// REDIS_ADDR (default 127.0.0.1:6379), REDIS_PASSWORD, REDIS_DB (default 0).
func redisConfig() (addr, password string, database int) {
	addr = envOr("REDIS_ADDR", "127.0.0.1:6379")
	password = envOr("REDIS_PASSWORD", "")
	database = 0
	if v := os.Getenv("REDIS_DB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			database = n
		}
	}
	return
}

// clickhouseAddr returns host:port for native protocol (default 127.0.0.1:9000).
func clickhouseAddr() string { return envOr("CLICKHOUSE_ADDR", "127.0.0.1:9000") }

func clickhouseDatabase() string { return envOr("CLICKHOUSE_DB", "gateway") }

// NewLogStore selects the log backend based on LOG_BACKEND:
//
//	LOG_BACKEND=sqlite      (default) → local file DB via modernc.org/sqlite
//	LOG_BACKEND=clickhouse            → external ClickHouse (opt-in)
//
// Unknown values fall through to sqlite so a typo can't take the gateway
// down. Exposed at package scope so tests can substitute a fake LogStore.
func NewLogStore() (LogStore, error) {
	backend := strings.ToLower(strings.TrimSpace(envOr("LOG_BACKEND", "sqlite")))
	switch backend {
	case "clickhouse", "ch":
		return newClickhouseStore()
	case "sqlite", "sqlite3", "":
		return newSqliteStore()
	default:
		slog.Warn("unknown LOG_BACKEND, defaulting to sqlite", "module", "db", "value", backend)
		return newSqliteStore()
	}
}

// ============================================================================
// STORE
// ============================================================================

// Store is the unified Redis + LogStore manager. It owns the async batching
// pipeline that feeds any LogStore backend (ClickHouse or SQLite) without
// blocking the request path.
type Store struct {
	rdb      *redis.Client
	logStore LogStore

	done chan struct{}
	wg   sync.WaitGroup

	reqLogCh  chan RequestLog
	refreshCh chan RefreshLog
	eventCh   chan AccountEvent
}

// ============================================================================
// DTOs — the persistence-friendly shape of domain types
// ============================================================================

// GrokAccountDTO carries the subset of a GrokAccount that lives in Redis.
type GrokAccountDTO struct {
	Email        string
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresAt    time.Time
	ExpiresIn    int
	Expired      string
	LastRefresh  string
	Sub          string
	Disabled     bool
	DisabledAt   time.Time
}

// GatewayKeyDTO carries the persisted shape of an auth key.
type GatewayKeyDTO struct {
	Key           string
	Name          string
	Role          string
	AllowedModels []string
	RPM           int
	Burst         int
	TokenQuota    int64
	TokensUsed    int64
	Requests      int64
	CreatedAt     time.Time
	Disabled      bool
}

// CBKeyDTO is the persisted shape of a CodeBuddy pool key.
// CredType is "api_key" (default) or "oauth". OAuth fields are only
// meaningful when CredType == "oauth".
type CBKeyDTO struct {
	Key          string
	CredType     string // "api_key" | "oauth"
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Email        string
	CreditsUsed  float64
	TotalReqs    int64
	Disabled     bool
	DisabledAt   time.Time
}

// NewStore initializes Redis + the selected LogStore backend, ensures schema,
// and spawns the async log consumers. Call Close() to flush + tear down.
func NewStore() (*Store, error) {
	rAddr, rPass, rDB := redisConfig()
	rdb := redis.NewClient(&redis.Options{
		Addr:         rAddr,
		Password:     rPass,
		DB:           rDB,
		PoolSize:     20,
		MinIdleConns: 5,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}

	logStore, err := NewLogStore()
	if err != nil {
		return nil, fmt.Errorf("log store init: %w", err)
	}

	s := &Store{
		rdb:       rdb,
		logStore:  logStore,
		done:      make(chan struct{}),
		reqLogCh:  make(chan RequestLog, LOG_BUFFER_SIZE),
		refreshCh: make(chan RefreshLog, LOG_BUFFER_SIZE),
		eventCh:   make(chan AccountEvent, LOG_BUFFER_SIZE),
	}

	// EnsureSchema uses a fresh ctx (30s) because SQLite may take a moment
	// on first-run PRAGMA setup with WAL journal creation.
	schemaCtx, schemaCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := s.logStore.EnsureSchema(schemaCtx); err != nil {
		slog.Warn("ensure schema failed", "module", "db-log", "backend", s.logStore.Kind(), "error", err)
	}
	schemaCancel()

	s.wg.Add(3)
	go s.consumeRequestLogs()
	go s.consumeRefreshLogs()
	go s.consumeAccountEvents()

	slog.Info("Redis + log store connected", "module", "db", "redis", rAddr, "log_backend", s.logStore.Kind())
	return s, nil
}

// Ready returns true if the store's Redis connection is initialized.
// Used by callers who want a fast pre-check before attempting a query.
func (s *Store) Ready() bool { return s != nil && s.rdb != nil }

// Redis exposes the underlying *redis.Client so peer packages (e.g.
// internal/tunnel) can persist their own state without dragging every
// operation through the Store. Returns nil when the store is not
// initialised.
func (s *Store) Redis() *redis.Client {
	if s == nil {
		return nil
	}
	return s.rdb
}

// DeleteGrokAccount removes the Redis HASH for a grok account by email.
func (s *Store) DeleteGrokAccount(email string) {
	if s == nil || s.rdb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := s.rdb.Del(ctx, RK_GROK_ACCOUNT+email).Err(); err != nil {
		slog.Warn("DEL grok account failed", "module", "db-redis", "email", email, "error", err)
	}
}

// DeleteCBKey removes the Redis HASH for a codebuddy key.
func (s *Store) DeleteCBKey(key string) {
	if s == nil || s.rdb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := s.rdb.Del(ctx, RK_CB_KEY+key).Err(); err != nil {
		slog.Warn("DEL cb key failed", "module", "db-redis", "key", maskRedisKey(RK_CB_KEY+key), "error", err)
	}
}

// DeleteGatewayKey removes the Redis HASH for a gateway API key.
func (s *Store) DeleteGatewayKey(key string) {
	if s == nil || s.rdb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := s.rdb.Del(ctx, RK_GATEWAY_KEY+key).Err(); err != nil {
		slog.Warn("DEL gateway key failed", "module", "db-redis", "key", maskRedisKey(RK_GATEWAY_KEY+key), "error", err)
	}
}

// ============================================================================
// REDIS — Grok accounts
// ============================================================================

// SaveGrokAccount writes account state to Redis (non-blocking, best-effort).
func (s *Store) SaveGrokAccount(dto GrokAccountDTO) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	data := map[string]interface{}{
		"email":         dto.Email,
		"access_token":  dto.AccessToken,
		"refresh_token": dto.RefreshToken,
		"id_token":      dto.IDToken,
		"expires_at":    dto.ExpiresAt.Unix(),
		"expires_in":    dto.ExpiresIn,
		"expired":       dto.Expired,
		"last_refresh":  dto.LastRefresh,
		"sub":           dto.Sub,
		"disabled":      dto.Disabled,
		"disabled_at":   dto.DisabledAt.Unix(),
	}
	key := RK_GROK_ACCOUNT + dto.Email
	if err := s.rdb.HSet(ctx, key, data).Err(); err != nil {
		slog.Warn("HSet failed", "module", "db-redis", "key", maskRedisKey(key), "error", err)
	}
}

// LoadGrokAccounts returns raw Redis hash maps keyed by email. Callers
// parse the field values themselves — the raw shape hasn't changed since
// the flat-package era and grok_account.go's parser expects it.
func (s *Store) LoadGrokAccounts() (map[string]map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pattern := RK_GROK_ACCOUNT + "*"
	iter := s.rdb.Scan(ctx, 0, pattern, 100).Iterator()
	results := make(map[string]map[string]string)

	for iter.Next(ctx) {
		key := iter.Val()
		email := key[len(RK_GROK_ACCOUNT):]
		vals, err := s.rdb.HGetAll(ctx, key).Result()
		if err != nil {
			continue
		}
		results[email] = vals
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// ============================================================================
// REDIS — CodeBuddy keys
// ============================================================================

func (s *Store) SaveCBKey(dto CBKeyDTO) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	credType := dto.CredType
	if credType == "" {
		credType = "api_key"
	}
	data := map[string]interface{}{
		"cred_type":      credType,
		"credits_used":   dto.CreditsUsed,
		"total_requests": dto.TotalReqs,
		"disabled":       dto.Disabled,
		"disabled_at":    dto.DisabledAt.Unix(),
		"updated_at":     time.Now().Unix(),
	}
	// Only persist OAuth secrets when this entry is an OAuth account —
	// avoids writing empty access/refresh tokens over API-key hashes.
	if credType == "oauth" {
		data["access_token"] = dto.AccessToken
		data["refresh_token"] = dto.RefreshToken
		data["expires_at"] = dto.ExpiresAt.Unix()
		data["email"] = dto.Email
	}
	rk := RK_CB_KEY + dto.Key
	if err := s.rdb.HSet(ctx, rk, data).Err(); err != nil {
		slog.Warn("HSet failed", "module", "db-redis", "key", maskRedisKey(rk), "error", err)
	}
}

func (s *Store) LoadCBKeys() (map[string]map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pattern := RK_CB_KEY + "*"
	iter := s.rdb.Scan(ctx, 0, pattern, 100).Iterator()
	results := make(map[string]map[string]string)

	for iter.Next(ctx) {
		key := iter.Val()
		apiKey := key[len(RK_CB_KEY):]
		vals, err := s.rdb.HGetAll(ctx, key).Result()
		if err != nil {
			continue
		}
		results[apiKey] = vals
	}
	return results, nil
}

// ============================================================================
// REDIS — Gateway keys (auth)
// ============================================================================

// SaveGatewayKey writes a full GatewayKeyDTO to Redis HASH.
func (s *Store) SaveGatewayKey(dto GatewayKeyDTO) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	data := map[string]interface{}{
		"key":            dto.Key,
		"name":           dto.Name,
		"role":           dto.Role,
		"allowed_models": strings.Join(dto.AllowedModels, ","),
		"rpm":            dto.RPM,
		"burst":          dto.Burst,
		"token_quota":    dto.TokenQuota,
		"tokens_used":    dto.TokensUsed,
		"requests":       dto.Requests,
		"created_at":     dto.CreatedAt.Unix(),
		"disabled":       dto.Disabled,
		"updated_at":     time.Now().Unix(),
	}
	rk := RK_GATEWAY_KEY + dto.Key
	if err := s.rdb.HSet(ctx, rk, data).Err(); err != nil {
		slog.Warn("HSet failed", "module", "db-redis", "key", maskRedisKey(rk), "error", err)
	}
}

// LoadGatewayKeys scans Redis for all gateway keys and returns them as DTOs.
func (s *Store) LoadGatewayKeys() ([]GatewayKeyDTO, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pattern := RK_GATEWAY_KEY + "*"
	iter := s.rdb.Scan(ctx, 0, pattern, 100).Iterator()
	var results []GatewayKeyDTO

	for iter.Next(ctx) {
		rk := iter.Val()
		vals, err := s.rdb.HGetAll(ctx, rk).Result()
		if err != nil {
			continue
		}
		dto, ok := parseGatewayKeyFromRedis(vals)
		if ok {
			results = append(results, dto)
		}
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// IncrementGatewayKeyTokens atomically increments tokens_used for a key.
func (s *Store) IncrementGatewayKeyTokens(key string, amount int64) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	rk := RK_GATEWAY_KEY + key
	if err := s.rdb.HIncrBy(ctx, rk, "tokens_used", amount).Err(); err != nil {
		slog.Warn("HINCRBY tokens_used failed", "module", "db-redis", "key", maskRedisKey(rk), "error", err)
	}
}

// IncrementGatewayKeyRequests atomically increments requests count for a key.
func (s *Store) IncrementGatewayKeyRequests(key string) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	rk := RK_GATEWAY_KEY + key
	if err := s.rdb.HIncrBy(ctx, rk, "requests", 1).Err(); err != nil {
		slog.Warn("HINCRBY requests failed", "module", "db-redis", "key", maskRedisKey(rk), "error", err)
	}
}

// parseGatewayKeyFromRedis builds a GatewayKeyDTO from Redis HASH fields.
// Second return is false when the row is empty/malformed.
func parseGatewayKeyFromRedis(vals map[string]string) (GatewayKeyDTO, bool) {
	key := vals["key"]
	if key == "" {
		return GatewayKeyDTO{}, false
	}
	dto := GatewayKeyDTO{
		Key:        key,
		Name:       vals["name"],
		Role:       vals["role"],
		RPM:        atoiSafe(vals["rpm"]),
		Burst:      atoiSafe(vals["burst"]),
		TokenQuota: atollSafe(vals["token_quota"]),
		TokensUsed: atollSafe(vals["tokens_used"]),
		Requests:   atollSafe(vals["requests"]),
		Disabled:   vals["disabled"] == "true" || vals["disabled"] == "1",
	}
	if am := vals["allowed_models"]; am != "" {
		for _, m := range strings.Split(am, ",") {
			m = strings.TrimSpace(m)
			if m != "" {
				dto.AllowedModels = append(dto.AllowedModels, m)
			}
		}
	}
	if ts := atollSafe(vals["created_at"]); ts > 0 {
		dto.CreatedAt = time.Unix(ts, 0)
	}
	return dto, true
}

// atoiSafe parses an int from string, returning 0 on error.
func atoiSafe(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// atollSafe parses an int64 from string, returning 0 on error.
func atollSafe(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// ============================================================================
// CLICKHOUSE — async log writers (non-blocking via channels)
// ============================================================================

// LogRequest queues a request log for async ClickHouse insert.
// Non-blocking: drops if buffer full (never blocks request processing).
func (s *Store) LogRequest(r RequestLog) {
	if s == nil {
		return
	}
	select {
	case s.reqLogCh <- r:
	default:
		slog.Warn("request log buffer full, dropping entry", "module", "db-ch")
	}
}

func (s *Store) LogRefresh(r RefreshLog) {
	if s == nil {
		return
	}
	select {
	case s.refreshCh <- r:
	default:
		slog.Warn("refresh log buffer full, dropping entry", "module", "db-ch")
	}
}

func (s *Store) LogEvent(e AccountEvent) {
	if s == nil {
		return
	}
	select {
	case s.eventCh <- e:
	default:
		slog.Warn("event buffer full, dropping entry", "module", "db-ch")
	}
}

func (s *Store) consumeRequestLogs() {
	defer s.wg.Done()
	batch := make([]RequestLog, 0, 200)
	ticker := time.NewTicker(LOG_FLUSH_INTERVAL)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 || s.logStore == nil {
			batch = batch[:0]
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := s.logStore.InsertRequestBatch(ctx, batch); err != nil {
			slog.Error("reqlog flush", "module", "db-log", "backend", s.logStore.Kind(), "error", err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case r := <-s.reqLogCh:
			batch = append(batch, r)
			if len(batch) >= 200 {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-s.done:
			flush()
			return
		}
	}
}

func (s *Store) consumeRefreshLogs() {
	defer s.wg.Done()
	batch := make([]RefreshLog, 0, 50)
	ticker := time.NewTicker(LOG_FLUSH_INTERVAL)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 || s.logStore == nil {
			batch = batch[:0]
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.logStore.InsertRefreshBatch(ctx, batch); err != nil {
			slog.Debug("refresh flush", "module", "db-log", "error", err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case r := <-s.refreshCh:
			batch = append(batch, r)
			if len(batch) >= 50 {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-s.done:
			flush()
			return
		}
	}
}

func (s *Store) consumeAccountEvents() {
	defer s.wg.Done()
	batch := make([]AccountEvent, 0, 50)
	ticker := time.NewTicker(LOG_FLUSH_INTERVAL)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 || s.logStore == nil {
			batch = batch[:0]
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.logStore.InsertEventBatch(ctx, batch); err != nil {
			slog.Debug("event flush", "module", "db-log", "error", err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case e := <-s.eventCh:
			batch = append(batch, e)
			if len(batch) >= 50 {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-s.done:
			flush()
			return
		}
	}
}

// ============================================================================
// HISTORY QUERIES — thin wrappers that forward to the LogStore backend
// ============================================================================

// GetRequestStats returns aggregate stats since the given time.
// Backward-compatible signature — the old *db.Store method name is preserved
// so handlers don't need to change.
func (s *Store) GetRequestStats(since time.Time) (*RequestStats, error) {
	if s == nil || s.logStore == nil {
		return &RequestStats{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.logStore.GetRequestStats(ctx, since)
}

// GetModelStats returns per-model breakdown of GetRequestStats.
func (s *Store) GetModelStats(since time.Time, limit int) ([]ModelStats, error) {
	if s == nil || s.logStore == nil {
		return []ModelStats{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.logStore.GetModelStats(ctx, since, limit)
}

// GetRecentRequests returns latest request log previews (dashboard table).
func (s *Store) GetRecentRequests(limit int) ([]RecentRequest, error) {
	if s == nil || s.logStore == nil {
		return []RecentRequest{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.logStore.GetRecentRequests(ctx, limit)
}

// GetRequestDetail returns a single request log with full JSON bodies.
func (s *Store) GetRequestDetail(id uint64) (*RequestDetail, error) {
	if s == nil || s.logStore == nil {
		return nil, fmt.Errorf("log store not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.logStore.GetRequestDetail(ctx, id)
}

// ============================================================================
// REDIS — Custom models + aliases (v1.3.0)
// ============================================================================

// CustomModel is a user-defined routing entry. `Upstream` picks the backend
// (codebuddy | grok), `ModelName` is the actual model string forwarded to
// that backend (after the cb/ prefix has been stripped for CodeBuddy), and
// `OwnedBy` is the label shown in /v1/models.
type CustomModel struct {
	Upstream  string `json:"upstream"`   // "codebuddy" | "grok"
	ModelName string `json:"model_name"` // actual name sent upstream
	OwnedBy   string `json:"owned_by"`   // display label for /v1/models
}

// LoadCustomModels returns all custom-model entries as a map keyed by model_id.
func (s *Store) LoadCustomModels() (map[string]CustomModel, error) {
	out := map[string]CustomModel{}
	if s == nil || s.rdb == nil {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	vals, err := s.rdb.HGetAll(ctx, RK_CUSTOM_MODELS).Result()
	if err != nil {
		return out, err
	}
	for id, raw := range vals {
		var cm CustomModel
		if err := json.Unmarshal([]byte(raw), &cm); err != nil {
			slog.Warn("bad custom_models entry", "module", "db-redis", "id", id, "error", err)
			continue
		}
		out[id] = cm
	}
	return out, nil
}

// SaveCustomModel stores one custom-model entry (upsert).
func (s *Store) SaveCustomModel(id string, cm CustomModel) error {
	if s == nil || s.rdb == nil {
		return fmt.Errorf("redis not ready")
	}
	blob, err := json.Marshal(cm)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	return s.rdb.HSet(ctx, RK_CUSTOM_MODELS, id, string(blob)).Err()
}

// DeleteCustomModel removes one custom-model entry.
func (s *Store) DeleteCustomModel(id string) error {
	if s == nil || s.rdb == nil {
		return fmt.Errorf("redis not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	return s.rdb.HDel(ctx, RK_CUSTOM_MODELS, id).Err()
}

// LoadCustomAliases returns all alias → target model_id mappings.
func (s *Store) LoadCustomAliases() (map[string]string, error) {
	out := map[string]string{}
	if s == nil || s.rdb == nil {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	vals, err := s.rdb.HGetAll(ctx, RK_CUSTOM_ALIASES).Result()
	if err != nil {
		return out, err
	}
	for k, v := range vals {
		out[k] = v
	}
	return out, nil
}

// SaveCustomAlias stores one alias → target mapping (upsert).
func (s *Store) SaveCustomAlias(alias, target string) error {
	if s == nil || s.rdb == nil {
		return fmt.Errorf("redis not ready")
	}
	if alias == "" || target == "" {
		return fmt.Errorf("alias and target required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	return s.rdb.HSet(ctx, RK_CUSTOM_ALIASES, alias, target).Err()
}

// DeleteCustomAlias removes one alias entry.
func (s *Store) DeleteCustomAlias(alias string) error {
	if s == nil || s.rdb == nil {
		return fmt.Errorf("redis not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	return s.rdb.HDel(ctx, RK_CUSTOM_ALIASES, alias).Err()
}

// ============================================================================
// REDIS — Combos (v1.4.0)
// ============================================================================

// Combo groups multiple models under a single logical name with a routing
// strategy applied per request. Callers reference the combo as
// "combo/<name>" — see ComboRegistry.Resolve.
//
//   Strategy "fallback"    — try models in order; on upstream failure the
//                            proxy falls through to the next entry (see the
//                            fallback retry loop in proxy.ProxyRequest).
//   Strategy "round_robin" — atomic INCR of combo:counter:<name> selects the
//                            next model modulo len(Models) per request.
type Combo struct {
	Name        string   `json:"name"`
	Strategy    string   `json:"strategy"`    // "fallback" | "round_robin"
	Models      []string `json:"models"`      // ordered list of model IDs
	Description string   `json:"description"` // optional
}

// LoadCombos returns every persisted combo keyed by name.
func (s *Store) LoadCombos() (map[string]Combo, error) {
	out := map[string]Combo{}
	if s == nil || s.rdb == nil {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	vals, err := s.rdb.HGetAll(ctx, RK_COMBOS).Result()
	if err != nil {
		return out, err
	}
	for name, raw := range vals {
		var cb Combo
		if err := json.Unmarshal([]byte(raw), &cb); err != nil {
			slog.Warn("bad combos entry", "module", "db-redis", "name", name, "error", err)
			continue
		}
		// Backfill Name if missing (older writes / migrations).
		if cb.Name == "" {
			cb.Name = name
		}
		out[name] = cb
	}
	return out, nil
}

// SaveCombo stores one combo (upsert).
func (s *Store) SaveCombo(c Combo) error {
	if s == nil || s.rdb == nil {
		return fmt.Errorf("redis not ready")
	}
	if c.Name == "" {
		return fmt.Errorf("combo name required")
	}
	blob, err := json.Marshal(c)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	return s.rdb.HSet(ctx, RK_COMBOS, c.Name, string(blob)).Err()
}

// DeleteCombo removes one combo entry and its round-robin counter.
func (s *Store) DeleteCombo(name string) error {
	if s == nil || s.rdb == nil {
		return fmt.Errorf("redis not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	// Best-effort delete both keys.
	if err := s.rdb.HDel(ctx, RK_COMBOS, name).Err(); err != nil {
		return err
	}
	_ = s.rdb.Del(ctx, RK_COMBO_COUNTER+name).Err()
	return nil
}

// IncrComboCounter atomically increments the round-robin counter for the
// given combo and returns the new value. The caller applies modulo over the
// combo's model list length.
func (s *Store) IncrComboCounter(name string) (int64, error) {
	if s == nil || s.rdb == nil {
		return 0, fmt.Errorf("redis not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	return s.rdb.Incr(ctx, RK_COMBO_COUNTER+name).Result()
}

// ============================================================================
// REDIS — Proxy pool (v1.5.0)
// ============================================================================

// ProxyEntryDTO is the persisted shape of one proxy pool entry.
// Passwords are stored as-is in Redis (single-tenant admin surface); API
// responses mask them via the pool layer.
//
// Upstreams scopes the proxy to specific upstream families ("all", "grok",
// "codebuddy"). Stored as a JSON array string in the HASH. Nil/empty means
// legacy pre-scoping entry; the pool layer treats that as ["all"].
type ProxyEntryDTO struct {
	ID         string
	Protocol   string // "http" | "socks5"
	Host       string
	Port       int
	Username   string
	Password   string
	Enabled    bool
	Label      string
	Upstreams  []string
	CreatedAt  time.Time
	LastUsedAt time.Time
	FailCount  int
}

// SaveProxy upserts a proxy entry into Redis. Enabled/disabled membership
// in the enabled-set is kept in sync so callers only need one call.
func (s *Store) SaveProxy(dto ProxyEntryDTO) error {
	if s == nil || s.rdb == nil {
		return fmt.Errorf("redis not ready")
	}
	if dto.ID == "" {
		return fmt.Errorf("proxy id required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	// Upstreams as JSON array string. Empty slice serialises to "[]" so we
	// can tell "explicitly no scope (illegal, but we tolerate)" from
	// "field missing (legacy)". LoadProxies falls back to nil on parse err.
	upstreamsJSON := "[]"
	if len(dto.Upstreams) > 0 {
		if b, err := json.Marshal(dto.Upstreams); err == nil {
			upstreamsJSON = string(b)
		}
	}
	data := map[string]interface{}{
		"id":           dto.ID,
		"protocol":     dto.Protocol,
		"host":         dto.Host,
		"port":         dto.Port,
		"username":     dto.Username,
		"password":     dto.Password,
		"enabled":      dto.Enabled,
		"label":        dto.Label,
		"upstreams":    upstreamsJSON,
		"created_at":   dto.CreatedAt.Unix(),
		"last_used_at": dto.LastUsedAt.Unix(),
		"fail_count":   dto.FailCount,
	}
	rk := RK_PROXY + dto.ID
	if err := s.rdb.HSet(ctx, rk, data).Err(); err != nil {
		return err
	}
	if dto.Enabled {
		if err := s.rdb.SAdd(ctx, RK_PROXY_ENABLED, dto.ID).Err(); err != nil {
			slog.Warn("SADD proxy enabled failed", "module", "db-redis", "id", dto.ID, "error", err)
		}
	} else {
		if err := s.rdb.SRem(ctx, RK_PROXY_ENABLED, dto.ID).Err(); err != nil {
			slog.Warn("SREM proxy enabled failed", "module", "db-redis", "id", dto.ID, "error", err)
		}
	}
	return nil
}

// UpdateProxyLastUsed touches the last_used_at field on a proxy hash.
// Best-effort — failures are logged but don't propagate.
func (s *Store) UpdateProxyLastUsed(id string, ts time.Time) {
	if s == nil || s.rdb == nil || id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := s.rdb.HSet(ctx, RK_PROXY+id, "last_used_at", ts.Unix()).Err(); err != nil {
		slog.Debug("HSet proxy last_used_at failed", "module", "db-redis", "id", id, "error", err)
	}
}

// UpdateProxyFailCount sets fail_count on a proxy hash.
func (s *Store) UpdateProxyFailCount(id string, count int) {
	if s == nil || s.rdb == nil || id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := s.rdb.HSet(ctx, RK_PROXY+id, "fail_count", count).Err(); err != nil {
		slog.Debug("HSet proxy fail_count failed", "module", "db-redis", "id", id, "error", err)
	}
}

// DeleteProxy removes a proxy hash + its membership in the enabled set.
func (s *Store) DeleteProxy(id string) error {
	if s == nil || s.rdb == nil {
		return fmt.Errorf("redis not ready")
	}
	if id == "" {
		return fmt.Errorf("id required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := s.rdb.Del(ctx, RK_PROXY+id).Err(); err != nil {
		return err
	}
	_ = s.rdb.SRem(ctx, RK_PROXY_ENABLED, id).Err()
	return nil
}

// LoadProxies scans every fr:proxy:<id> hash and returns the parsed DTOs.
// The enabled set is authoritative for the Enabled flag — a stale HASH
// enabled field never overrides membership in RK_PROXY_ENABLED.
func (s *Store) LoadProxies() ([]ProxyEntryDTO, error) {
	if s == nil || s.rdb == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// SMEMBERS enabled set once so we can override the stored enabled bit.
	enabledMembers, _ := s.rdb.SMembers(ctx, RK_PROXY_ENABLED).Result()
	enabledSet := make(map[string]struct{}, len(enabledMembers))
	for _, id := range enabledMembers {
		enabledSet[id] = struct{}{}
	}

	pattern := RK_PROXY + "*"
	iter := s.rdb.Scan(ctx, 0, pattern, 100).Iterator()
	out := []ProxyEntryDTO{}
	for iter.Next(ctx) {
		rk := iter.Val()
		// Skip the enabled-set key — same prefix, but not a HASH.
		if rk == RK_PROXY_ENABLED || rk == RK_PROXY_RR {
			continue
		}
		id := strings.TrimPrefix(rk, RK_PROXY)
		vals, err := s.rdb.HGetAll(ctx, rk).Result()
		if err != nil || len(vals) == 0 {
			continue
		}
		dto := ProxyEntryDTO{
			ID:       id,
			Protocol: vals["protocol"],
			Host:     vals["host"],
			Username: vals["username"],
			Password: vals["password"],
			Label:    vals["label"],
		}
		// Upstreams: parse JSON array string. Missing field or parse
		// failure leaves it nil — the pool layer defaults nil to ["all"]
		// for backward compatibility with pre-scoping entries.
		if raw, ok := vals["upstreams"]; ok && raw != "" && raw != "null" {
			var us []string
			if err := json.Unmarshal([]byte(raw), &us); err == nil {
				dto.Upstreams = us
			}
		}
		if v, err := strconv.Atoi(vals["port"]); err == nil {
			dto.Port = v
		}
		if v, err := strconv.Atoi(vals["fail_count"]); err == nil {
			dto.FailCount = v
		}
		if v, err := strconv.ParseInt(vals["created_at"], 10, 64); err == nil && v > 0 {
			dto.CreatedAt = time.Unix(v, 0)
		}
		if v, err := strconv.ParseInt(vals["last_used_at"], 10, 64); err == nil && v > 0 {
			dto.LastUsedAt = time.Unix(v, 0)
		}
		_, dto.Enabled = enabledSet[id]
		out = append(out, dto)
	}
	if err := iter.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// IncrProxyRR atomically increments the shared round-robin counter and
// returns the new value. Callers modulo the count of enabled proxies to
// pick an index.
func (s *Store) IncrProxyRR() (int64, error) {
	if s == nil || s.rdb == nil {
		return 0, fmt.Errorf("redis not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	return s.rdb.Incr(ctx, RK_PROXY_RR).Result()
}

// Close gracefully shuts down DB connections.
func (s *Store) Close() {
	close(s.done)
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		slog.Warn("consumer drain timed out", "module", "db", "timeout", "10s")
	}
	if s.rdb != nil {
		_ = s.rdb.Close()
	}
	if s.logStore != nil {
		_ = s.logStore.Close()
	}
}
