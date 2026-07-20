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
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
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

// ============================================================================
// STORE
// ============================================================================

// Store is the unified Redis + ClickHouse manager.
type Store struct {
	rdb *redis.Client
	ch  driver.Conn

	done chan struct{}
	wg   sync.WaitGroup

	reqLogCh  chan RequestLog
	refreshCh chan RefreshLog
	eventCh   chan AccountEvent
}

// RequestLog is a single row for the ClickHouse request_logs table.
type RequestLog struct {
	Timestamp    time.Time
	RequestID    string
	ClientKey    string
	Model        string
	Upstream     string
	AccountID    string
	StatusCode   int
	LatencyMs    int
	TokensIn     int
	TokensOut    int
	ErrorMsg     string
	InputText    string          // quick preview (last user msg, 500 chars)
	OutputText   string          // quick preview (first 1000 chars)
	RequestBody  json.RawMessage // full request JSON
	ResponseBody json.RawMessage // full response JSON
}

type RefreshLog struct {
	Timestamp    time.Time
	AccountEmail string
	Provider     string
	Success      bool
	ErrorMsg     string
	LatencyMs    int
}

type AccountEvent struct {
	Timestamp time.Time
	AccountID string
	Provider  string
	EventType string
	EventData map[string]interface{}
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
type CBKeyDTO struct {
	Key          string
	CreditsUsed  float64
	TotalReqs    int64
	Disabled     bool
	DisabledAt   time.Time
}

// NewStore initializes Redis + ClickHouse, ensures schema, and spawns the
// async log consumers. Call Close() to flush + tear down.
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

	chAddr := clickhouseAddr()
	chDB := clickhouseDatabase()
	chUser := envOr("CLICKHOUSE_USER", "default")
	chPass := envOr("CLICKHOUSE_PASSWORD", "")

	// Connect to 'default' database first to ensure target DB exists.
	bootstrapCh, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{chAddr},
		Auth: clickhouse.Auth{
			Database: "default",
			Username: chUser,
			Password: chPass,
		},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse open (bootstrap): %w", err)
	}
	if len(chDB) == 0 || !regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`).MatchString(chDB) {
		return nil, fmt.Errorf("invalid CLICKHOUSE_DB name %q: must be alphanumeric/underscore, start with letter", chDB)
	}
	if err := bootstrapCh.Exec(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", chDB)); err != nil {
		return nil, fmt.Errorf("clickhouse create database: %w", err)
	}
	bootstrapCh.Close()

	ch, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{chAddr},
		Auth: clickhouse.Auth{
			Database: chDB,
			Username: chUser,
			Password: chPass,
		},
		DialTimeout:  5 * time.Second,
		MaxOpenConns: 10,
		MaxIdleConns: 5,
		Compression: &clickhouse.Compression{
			Method: clickhouse.CompressionLZ4,
		},
		Settings: clickhouse.Settings{
			"async_insert":          1,
			"wait_for_async_insert": 0,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse open: %w", err)
	}
	if err := ch.Ping(ctx); err != nil {
		return nil, fmt.Errorf("clickhouse ping: %w", err)
	}

	s := &Store{
		rdb:       rdb,
		ch:        ch,
		done:      make(chan struct{}),
		reqLogCh:  make(chan RequestLog, LOG_BUFFER_SIZE),
		refreshCh: make(chan RefreshLog, LOG_BUFFER_SIZE),
		eventCh:   make(chan AccountEvent, LOG_BUFFER_SIZE),
	}

	if err := s.ensureClickHouseSchema(ctx); err != nil {
		slog.Warn("ensure schema failed", "module", "db-ch", "error", err)
	}

	s.wg.Add(3)
	go s.consumeRequestLogs()
	go s.consumeRefreshLogs()
	go s.consumeAccountEvents()

	slog.Info("Redis + ClickHouse connected", "module", "db", "redis", rAddr, "ch_addr", chAddr, "ch_db", chDB)
	return s, nil
}

func (s *Store) ensureClickHouseSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS request_logs (
			id UInt64,
			timestamp DateTime64(3, 'UTC'),
			request_id String,
			client_key LowCardinality(String),
			model LowCardinality(String),
			upstream LowCardinality(String),
			account_id String,
			status_code UInt16,
			latency_ms UInt32,
			tokens_in UInt32,
			tokens_out UInt32,
			error_msg String,
			input_text String,
			output_text String,
			request_body String CODEC(ZSTD(3)),
			response_body String CODEC(ZSTD(3))
		) ENGINE = MergeTree
		PARTITION BY toYYYYMMDD(timestamp)
		ORDER BY (timestamp, id)
		TTL toDateTime(timestamp) + INTERVAL 90 DAY
		SETTINGS index_granularity = 8192`,
		`ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS request_id String AFTER timestamp`,
		`CREATE TABLE IF NOT EXISTS token_refresh_history (
			timestamp DateTime64(3, 'UTC'),
			account_email String,
			provider LowCardinality(String),
			success UInt8,
			error_msg String,
			latency_ms UInt32
		) ENGINE = MergeTree
		PARTITION BY toYYYYMM(timestamp)
		ORDER BY (timestamp, account_email)
		TTL toDateTime(timestamp) + INTERVAL 90 DAY`,
		`CREATE TABLE IF NOT EXISTS account_events (
			timestamp DateTime64(3, 'UTC'),
			account_id String,
			provider LowCardinality(String),
			event_type LowCardinality(String),
			event_data String CODEC(ZSTD(3))
		) ENGINE = MergeTree
		PARTITION BY toYYYYMM(timestamp)
		ORDER BY (timestamp, provider, event_type)
		TTL toDateTime(timestamp) + INTERVAL 90 DAY`,
	}
	for _, q := range stmts {
		if err := s.ch.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func newLogID() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint64(time.Now().UnixNano())
	}
	return binary.LittleEndian.Uint64(b[:])
}

// Ready returns true if the store's Redis connection is initialized.
// Used by callers who want a fast pre-check before attempting a query.
func (s *Store) Ready() bool { return s != nil && s.rdb != nil }

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

	data := map[string]interface{}{
		"credits_used":   dto.CreditsUsed,
		"total_requests": dto.TotalReqs,
		"disabled":       dto.Disabled,
		"disabled_at":    dto.DisabledAt.Unix(),
		"updated_at":     time.Now().Unix(),
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

func bodyString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	return string(raw)
}

func (s *Store) consumeRequestLogs() {
	defer s.wg.Done()
	batch := make([]RequestLog, 0, 200)
	ticker := time.NewTicker(LOG_FLUSH_INTERVAL)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 || s.ch == nil {
			batch = batch[:0]
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		batchSQL, err := s.ch.PrepareBatch(ctx, `INSERT INTO request_logs (
			id, timestamp, request_id, client_key, model, upstream, account_id,
			status_code, latency_ms, tokens_in, tokens_out, error_msg,
			input_text, output_text, request_body, response_body
		)`)
		if err != nil {
			slog.Error("reqlog prepare", "module", "db-ch", "error", err)
			batch = batch[:0]
			return
		}
		for _, r := range batch {
			ts := r.Timestamp.UTC()
			if ts.IsZero() {
				ts = time.Now().UTC()
			}
			if err := batchSQL.Append(
				newLogID(),
				ts,
				r.RequestID,
				r.ClientKey,
				r.Model,
				r.Upstream,
				r.AccountID,
				uint16(r.StatusCode),
				uint32(max0(r.LatencyMs)),
				uint32(max0(r.TokensIn)),
				uint32(max0(r.TokensOut)),
				r.ErrorMsg,
				r.InputText,
				r.OutputText,
				bodyString(r.RequestBody),
				bodyString(r.ResponseBody),
			); err != nil {
				slog.Error("reqlog append", "module", "db-ch", "error", err)
			}
		}
		if err := batchSQL.Send(); err != nil {
			slog.Error("reqlog send", "module", "db-ch", "error", err)
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

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func (s *Store) consumeRefreshLogs() {
	defer s.wg.Done()
	batch := make([]RefreshLog, 0, 50)
	ticker := time.NewTicker(LOG_FLUSH_INTERVAL)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 || s.ch == nil {
			batch = batch[:0]
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		b, err := s.ch.PrepareBatch(ctx, `INSERT INTO token_refresh_history (
			timestamp, account_email, provider, success, error_msg, latency_ms
		)`)
		if err != nil {
			batch = batch[:0]
			return
		}
		for _, r := range batch {
			suc := uint8(0)
			if r.Success {
				suc = 1
			}
			ts := r.Timestamp.UTC()
			_ = b.Append(ts, r.AccountEmail, r.Provider, suc, r.ErrorMsg, uint32(max0(r.LatencyMs)))
		}
		_ = b.Send()
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
		if len(batch) == 0 || s.ch == nil {
			batch = batch[:0]
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		b, err := s.ch.PrepareBatch(ctx, `INSERT INTO account_events (
			timestamp, account_id, provider, event_type, event_data
		)`)
		if err != nil {
			batch = batch[:0]
			return
		}
		for _, e := range batch {
			var data string
			if e.EventData != nil {
				raw, _ := json.Marshal(e.EventData)
				data = string(raw)
			}
			_ = b.Append(e.Timestamp.UTC(), e.AccountID, e.Provider, e.EventType, data)
		}
		_ = b.Send()
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
// CLICKHOUSE — history queries (dashboard / analytics)
// ============================================================================

type RequestStats struct {
	TotalRequests  int     `json:"total_requests"`
	TotalErrors    int     `json:"total_errors"`
	ErrorRate      float64 `json:"error_rate_pct"`
	AvgLatencyMs   float64 `json:"avg_latency_ms"`
	TotalTokensIn  int     `json:"total_tokens_in"`
	TotalTokensOut int     `json:"total_tokens_out"`
	TotalTokens    int     `json:"total_tokens"`
}

func (s *Store) GetRequestStats(since time.Time) (*RequestStats, error) {
	if s == nil || s.ch == nil {
		return &RequestStats{}, nil
	}
	stats := &RequestStats{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	row := s.ch.QueryRow(ctx, `
		SELECT
			count(),
			countIf(status_code >= 400),
			ifNull(avg(latency_ms), 0),
			ifNull(sum(tokens_in), 0),
			ifNull(sum(tokens_out), 0)
		FROM request_logs
		WHERE timestamp >= ?
	`, since.UTC())
	var total, errors uint64
	var avg float64
	var tin, tout uint64
	if err := row.Scan(&total, &errors, &avg, &tin, &tout); err != nil {
		return nil, err
	}
	stats.TotalRequests = int(total)
	stats.TotalErrors = int(errors)
	stats.AvgLatencyMs = avg
	stats.TotalTokensIn = int(tin)
	stats.TotalTokensOut = int(tout)
	stats.TotalTokens = int(tin + tout)
	if stats.TotalRequests > 0 {
		stats.ErrorRate = float64(stats.TotalErrors) / float64(stats.TotalRequests) * 100
	}
	return stats, nil
}

type ModelStats struct {
	Model          string  `json:"model"`
	TotalRequests  int     `json:"total_requests"`
	TotalErrors    int     `json:"total_errors"`
	AvgLatencyMs   float64 `json:"avg_latency_ms"`
	TotalTokensIn  int     `json:"total_tokens_in"`
	TotalTokensOut int     `json:"total_tokens_out"`
	TotalTokens    int     `json:"total_tokens"`
}

func (s *Store) GetModelStats(since time.Time, limit int) ([]ModelStats, error) {
	if s == nil || s.ch == nil {
		return []ModelStats{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := s.ch.Query(ctx, `
		SELECT
			model,
			count() AS total_requests,
			countIf(status_code >= 400) AS total_errors,
			ifNull(avg(latency_ms), 0) AS avg_latency,
			ifNull(sum(tokens_in), 0) AS tokens_in,
			ifNull(sum(tokens_out), 0) AS tokens_out
		FROM request_logs
		WHERE timestamp >= ?
		GROUP BY model
		ORDER BY total_requests DESC
		LIMIT ?
	`, since.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []ModelStats
	for rows.Next() {
		var m ModelStats
		var tr, te, tin, tout uint64
		var avg float64
		if err := rows.Scan(&m.Model, &tr, &te, &avg, &tin, &tout); err != nil {
			continue
		}
		m.TotalRequests = int(tr)
		m.TotalErrors = int(te)
		m.AvgLatencyMs = avg
		m.TotalTokensIn = int(tin)
		m.TotalTokensOut = int(tout)
		m.TotalTokens = int(tin + tout)
		results = append(results, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("model stats iteration: %w", err)
	}
	return results, nil
}

// GetRecentRequests returns latest request logs for dashboard table.
// ID is a decimal string so JavaScript JSON.parse does not lose UInt64 precision
// (Number.MAX_SAFE_INTEGER is only 2^53-1; our random UInt64 ids exceed that).
type RecentRequest struct {
	ID         string `json:"id"`
	Timestamp  string `json:"timestamp"`
	ClientKey  string `json:"client_key"`
	Model      string `json:"model"`
	Upstream   string `json:"upstream"`
	AccountID  string `json:"account_id"`
	StatusCode int    `json:"status_code"`
	LatencyMs  int    `json:"latency_ms"`
	TokensIn   int    `json:"tokens_in"`
	TokensOut  int    `json:"tokens_out"`
	InputText  string `json:"input_text,omitempty"`
	OutputText string `json:"output_text,omitempty"`
	ErrorMsg   string `json:"error_msg,omitempty"`
}

func (s *Store) GetRecentRequests(limit int) ([]RecentRequest, error) {
	if s == nil || s.ch == nil {
		return []RecentRequest{}, nil
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := s.ch.Query(ctx, `
		SELECT id, timestamp, client_key, model, upstream, account_id,
		       status_code, latency_ms, tokens_in, tokens_out, error_msg,
		       input_text, output_text
		FROM request_logs
		ORDER BY timestamp DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RecentRequest, 0, limit)
	for rows.Next() {
		var r RecentRequest
		var id uint64
		var ts time.Time
		var sc uint16
		var lat, tin, tout uint32
		if err := rows.Scan(&id, &ts, &r.ClientKey, &r.Model, &r.Upstream,
			&r.AccountID, &sc, &lat, &tin, &tout, &r.ErrorMsg,
			&r.InputText, &r.OutputText); err != nil {
			slog.Error("recent scan", "module", "db-ch", "error", err)
			continue
		}
		r.ID = strconv.FormatUint(id, 10)
		r.StatusCode = int(sc)
		r.LatencyMs = int(lat)
		r.TokensIn = int(tin)
		r.TokensOut = int(tout)
		r.Timestamp = ts.UTC().Format("2006-01-02 15:04:05")
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		slog.Error("recent requests iteration", "module", "db-ch", "error", err)
	}
	return out, nil
}

// RequestDetail is a single request log with full JSON bodies.
type RequestDetail struct {
	ID           string          `json:"id"`
	Timestamp    string          `json:"timestamp"`
	ClientKey    string          `json:"client_key"`
	Model        string          `json:"model"`
	Upstream     string          `json:"upstream"`
	AccountID    string          `json:"account_id"`
	StatusCode   int             `json:"status_code"`
	LatencyMs    int             `json:"latency_ms"`
	TokensIn     int             `json:"tokens_in"`
	TokensOut    int             `json:"tokens_out"`
	ErrorMsg     string          `json:"error_msg"`
	InputText    string          `json:"input_text"`
	OutputText   string          `json:"output_text"`
	RequestBody  json.RawMessage `json:"request_body"`
	ResponseBody json.RawMessage `json:"response_body"`
}

func (s *Store) GetRequestDetail(id uint64) (*RequestDetail, error) {
	if s == nil || s.ch == nil {
		return nil, fmt.Errorf("clickhouse not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var d RequestDetail
	var rawID uint64
	var ts time.Time
	var sc uint16
	var lat, tin, tout uint32
	var reqBody, respBody string
	err := s.ch.QueryRow(ctx, `
		SELECT id, timestamp, client_key, model, upstream, account_id,
		       status_code, latency_ms, tokens_in, tokens_out, error_msg,
		       input_text, output_text, request_body, response_body
		FROM request_logs
		WHERE id = ?
		LIMIT 1
	`, id).Scan(&rawID, &ts, &d.ClientKey, &d.Model, &d.Upstream,
		&d.AccountID, &sc, &lat, &tin, &tout, &d.ErrorMsg,
		&d.InputText, &d.OutputText, &reqBody, &respBody)
	if err != nil {
		return nil, err
	}
	d.ID = strconv.FormatUint(rawID, 10)
	d.StatusCode = int(sc)
	d.LatencyMs = int(lat)
	d.TokensIn = int(tin)
	d.TokensOut = int(tout)
	d.Timestamp = ts.UTC().Format("2006-01-02 15:04:05")
	if reqBody != "" {
		d.RequestBody = json.RawMessage(reqBody)
	}
	if respBody != "" {
		d.ResponseBody = json.RawMessage(respBody)
	}
	return &d, nil
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
	if s.ch != nil {
		_ = s.ch.Close()
	}
}
