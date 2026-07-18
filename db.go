// db.go — Redis (hot state) + ClickHouse (persistent history) integration layer.
// Redis: account tokens, CB key credits, rate limit buckets — sub-ms reads.
// ClickHouse: request_logs, token_refresh_history, account_events — full-body history.
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/redis/go-redis/v9"
)

// ============================================================================
// CONFIG — secrets via env (fallback defaults for local VPS only)
// ============================================================================

const (
	// Redis key prefixes
	RK_GROK_ACCOUNT = "grok:account:" // HASH: access_token, refresh_token, expires_at, disabled, etc.
	RK_CB_KEY       = "cb:key:"       // HASH: credits_used, total_requests, disabled
	RK_GATEWAY_KEY  = "gw:key:"       // HASH: name, total_requests
	RK_RATE_LIMIT   = "rate:"         // STRING: token bucket state per client key

	// Async log buffer sizes
	LOG_BUFFER_SIZE    = 10000
	LOG_FLUSH_INTERVAL = 2 * time.Second
)

// envOr returns os.Getenv(key) if set, otherwise def.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// redisConfig resolves Redis connection from env:
// REDIS_ADDR (default 127.0.0.1:6379), REDIS_PASSWORD, REDIS_DB (default 0).
func redisConfig() (addr, password string, db int) {
	addr = envOr("REDIS_ADDR", "127.0.0.1:6379")
	password = envOr("REDIS_PASSWORD", "")
	db = 0
	if v := os.Getenv("REDIS_DB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			db = n
		}
	}
	return
}

// clickhouseAddr returns host:port for native protocol (default 127.0.0.1:9001).
func clickhouseAddr() string {
	return envOr("CLICKHOUSE_ADDR", "127.0.0.1:9001")
}

func clickhouseDatabase() string {
	return envOr("CLICKHOUSE_DB", "gateway")
}

// ============================================================================
// DB STORE — unified Redis + ClickHouse manager
// ============================================================================

type DBStore struct {
	rdb *redis.Client
	ch  driver.Conn

	// Async log channels (non-blocking writes to ClickHouse)
	reqLogCh  chan RequestLog
	refreshCh chan RefreshLog
	eventCh   chan AccountEvent
}

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
	RequestBody  json.RawMessage // full request JSON (messages, tools, etc.)
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

func NewDBStore() (*DBStore, error) {
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

	// Test Redis connection
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
	// Fresh ClickHouse deployments only have the 'default' database.
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
	// Create target database if missing (idempotent).
	if err := bootstrapCh.Exec(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", chDB)); err != nil {
		return nil, fmt.Errorf("clickhouse create database: %w", err)
	}
	bootstrapCh.Close()

	// Now connect to the target database.
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

	s := &DBStore{
		rdb:       rdb,
		ch:        ch,
		reqLogCh:  make(chan RequestLog, LOG_BUFFER_SIZE),
		refreshCh: make(chan RefreshLog, LOG_BUFFER_SIZE),
		eventCh:   make(chan AccountEvent, LOG_BUFFER_SIZE),
	}

	// Ensure schema exists (idempotent)
	if err := s.ensureClickHouseSchema(ctx); err != nil {
		log.Printf("[db:ch] warn: ensure schema: %v", err)
	}

	// Start async log consumers
	go s.consumeRequestLogs()
	go s.consumeRefreshLogs()
	go s.consumeAccountEvents()

	log.Printf("[db] Redis connected %s, ClickHouse connected %s/%s", rAddr, chAddr, chDB)
	return s, nil
}

func (s *DBStore) ensureClickHouseSchema(ctx context.Context) error {
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
		// Migration: add request_id column to existing table (idempotent)
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

// newLogID generates a random UInt64 id for request_logs rows.
func newLogID() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint64(time.Now().UnixNano())
	}
	return binary.LittleEndian.Uint64(b[:])
}

// ============================================================================
// REDIS — Grok Account State
// ============================================================================

// SaveGrokAccount writes account state to Redis (non-blocking, best-effort).
func (s *DBStore) SaveGrokAccount(acc *GrokAccount) {
	if s == nil || acc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	data := map[string]interface{}{
		"email":         acc.Email,
		"access_token":  acc.AccessToken,
		"refresh_token": acc.RefreshToken,
		"id_token":      acc.IDToken,
		"expires_at":    acc.expiresAt.Unix(),
		"expires_in":    acc.ExpiresIn,
		"expired":       acc.Expired,
		"last_refresh":  acc.LastRefresh,
		"sub":           acc.Sub,
		"disabled":      acc.disabled,
		"disabled_at":   acc.disabledAt.Unix(),
	}
	key := RK_GROK_ACCOUNT + acc.Email
	if err := s.rdb.HSet(ctx, key, data).Err(); err != nil {
		log.Printf("[db:redis] warn: HSet %s: %v", key, err)
	}
}

// LoadGrokAccounts reads all grok accounts from Redis into a map keyed by email.
func (s *DBStore) LoadGrokAccounts() (map[string]map[string]string, error) {
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
// REDIS — CodeBuddy Key State
// ============================================================================

func (s *DBStore) SaveCBKey(key string, creditsUsed float64, totalReqs int64, disabled bool, disabledAt time.Time) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	data := map[string]interface{}{
		"credits_used":   creditsUsed,
		"total_requests": totalReqs,
		"disabled":       disabled,
		"disabled_at":    disabledAt.Unix(),
		"updated_at":     time.Now().Unix(),
	}
	rk := RK_CB_KEY + key
	if err := s.rdb.HSet(ctx, rk, data).Err(); err != nil {
		log.Printf("[db:redis] warn: HSet %s: %v", rk, err)
	}
}

func (s *DBStore) LoadCBKeys() (map[string]map[string]string, error) {
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
// REDIS — Gateway Client Key State (full CRUD + usage tracking)
// ============================================================================

// SaveGatewayKey writes a full GatewayKeyInfo to Redis HASH.
func (s *DBStore) SaveGatewayKey(info *GatewayKeyInfo) {
	if s == nil || info == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	data := map[string]interface{}{
		"key":            info.Key,
		"name":           info.Name,
		"role":           string(info.Role),
		"allowed_models": strings.Join(info.AllowedModels, ","), // comma-separated
		"rpm":            info.RPM,
		"burst":          info.Burst,
		"token_quota":    info.TokenQuota,
		"tokens_used":    info.TokensUsed,
		"requests":       info.Requests,
		"created_at":     info.CreatedAt.Unix(),
		"disabled":       info.Disabled,
		"updated_at":     time.Now().Unix(),
	}
	rk := RK_GATEWAY_KEY + info.Key
	if err := s.rdb.HSet(ctx, rk, data).Err(); err != nil {
		log.Printf("[db:redis] warn: HSet %s: %v", rk, err)
	}
}

// LoadGatewayKeys scans Redis for all gateway keys and returns them.
func (s *DBStore) LoadGatewayKeys() ([]*GatewayKeyInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pattern := RK_GATEWAY_KEY + "*"
	iter := s.rdb.Scan(ctx, 0, pattern, 100).Iterator()
	var results []*GatewayKeyInfo

	for iter.Next(ctx) {
		rk := iter.Val()
		vals, err := s.rdb.HGetAll(ctx, rk).Result()
		if err != nil {
			continue
		}
		info := parseGatewayKeyFromRedis(vals)
		if info != nil {
			results = append(results, info)
		}
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// DeleteGatewayKey removes a gateway key from Redis.
func (s *DBStore) DeleteGatewayKey(key string) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	rk := RK_GATEWAY_KEY + key
	if err := s.rdb.Del(ctx, rk).Err(); err != nil {
		log.Printf("[db:redis] warn: DEL %s: %v", rk, err)
	}
}

// IncrementGatewayKeyTokens atomically increments tokens_used for a key.
func (s *DBStore) IncrementGatewayKeyTokens(key string, amount int64) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	rk := RK_GATEWAY_KEY + key
	if err := s.rdb.HIncrBy(ctx, rk, "tokens_used", amount).Err(); err != nil {
		log.Printf("[db:redis] warn: HINCRBY %s tokens_used: %v", rk, err)
	}
}

// IncrementGatewayKeyRequests atomically increments requests count for a key.
func (s *DBStore) IncrementGatewayKeyRequests(key string) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	rk := RK_GATEWAY_KEY + key
	if err := s.rdb.HIncrBy(ctx, rk, "requests", 1).Err(); err != nil {
		log.Printf("[db:redis] warn: HINCRBY %s requests: %v", rk, err)
	}
}

// parseGatewayKeyFromRedis builds a GatewayKeyInfo from Redis HASH fields.
func parseGatewayKeyFromRedis(vals map[string]string) *GatewayKeyInfo {
	key := vals["key"]
	if key == "" {
		return nil
	}
	info := &GatewayKeyInfo{
		Key:        key,
		Name:       vals["name"],
		Role:       KeyRole(vals["role"]),
		RPM:        atoiSafe(vals["rpm"]),
		Burst:      atoiSafe(vals["burst"]),
		TokenQuota: atollSafe(vals["token_quota"]),
		TokensUsed: atollSafe(vals["tokens_used"]),
		Requests:   atollSafe(vals["requests"]),
		Disabled:   vals["disabled"] == "true" || vals["disabled"] == "1",
	}
	// Backward compat: keys created before role field existed default to admin
	// (they were bootstrap/trusted keys). New keys get role from API.
	if info.Role == "" {
		info.Role = RoleAdmin
	}
	// Parse allowed_models (comma-separated → []string)
	if am := vals["allowed_models"]; am != "" {
		for _, m := range strings.Split(am, ",") {
			m = strings.TrimSpace(m)
			if m != "" {
				info.AllowedModels = append(info.AllowedModels, m)
			}
		}
	}
	if ts := atollSafe(vals["created_at"]); ts > 0 {
		info.CreatedAt = time.Unix(ts, 0)
	}
	return info
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
// CLICKHOUSE — Async Log Writers (non-blocking via channels)
// ============================================================================

// LogRequest queues a request log for async ClickHouse insert.
// Non-blocking: drops if buffer full (never blocks request processing).
func (s *DBStore) LogRequest(r RequestLog) {
	if s == nil {
		return
	}
	select {
	case s.reqLogCh <- r:
	default:
		log.Printf("[db:ch] warn: request log buffer full, dropping entry")
	}
}

func (s *DBStore) LogRefresh(r RefreshLog) {
	if s == nil {
		return
	}
	select {
	case s.refreshCh <- r:
	default:
		log.Printf("[db:ch] warn: refresh log buffer full, dropping entry")
	}
}

func (s *DBStore) LogEvent(e AccountEvent) {
	if s == nil {
		return
	}
	select {
	case s.eventCh <- e:
	default:
		log.Printf("[db:ch] warn: event buffer full, dropping entry")
	}
}

func bodyString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Unlimited full body — ClickHouse ZSTD handles large chat payloads.
	return string(raw)
}

func (s *DBStore) consumeRequestLogs() {
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
			log.Printf("[db:ch] reqlog prepare: %v", err)
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
				log.Printf("[db:ch] reqlog append: %v", err)
			}
		}
		if err := batchSQL.Send(); err != nil {
			log.Printf("[db:ch] reqlog send: %v", err)
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
		}
	}
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func (s *DBStore) consumeRefreshLogs() {
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
		}
	}
}

func (s *DBStore) consumeAccountEvents() {
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
		}
	}
}

// ============================================================================
// CLICKHOUSE — History Queries (dashboard / analytics)
// ============================================================================

type RequestStats struct {
	TotalRequests   int     `json:"total_requests"`
	TotalErrors     int     `json:"total_errors"`
	ErrorRate       float64 `json:"error_rate_pct"`
	AvgLatencyMs    float64 `json:"avg_latency_ms"`
	TotalTokensIn   int     `json:"total_tokens_in"`
	TotalTokensOut  int     `json:"total_tokens_out"`
	TotalTokens     int     `json:"total_tokens"`
}

func (s *DBStore) GetRequestStats(since time.Time) (*RequestStats, error) {
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
	Model         string  `json:"model"`
	TotalRequests int     `json:"total_requests"`
	TotalErrors   int     `json:"total_errors"`
	AvgLatencyMs  float64 `json:"avg_latency_ms"`
	TotalTokensIn int     `json:"total_tokens_in"`
	TotalTokensOut int    `json:"total_tokens_out"`
	TotalTokens   int     `json:"total_tokens"`
}

func (s *DBStore) GetModelStats(since time.Time, limit int) ([]ModelStats, error) {
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
	return results, nil
}

// UpsertGrokAccount is a no-op for history DB — Redis is source of truth.
// Kept for API compatibility with Refresh() callers.
func (s *DBStore) UpsertGrokAccount(acc *GrokAccount) {
	// intentionally empty — credentials live in Redis only
}

// UpsertCBKey is a no-op (Redis is source of truth for CB credits).
func (s *DBStore) UpsertCBKey(key string, creditsUsed float64, totalReqs int64, disabled bool) {
	// intentionally empty
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

func (s *DBStore) GetRecentRequests(limit int) ([]RecentRequest, error) {
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
			log.Printf("[db:ch] recent scan: %v", err)
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
	return out, nil
}

// GetRequestDetail fetches a single request log by ID, including full JSON bodies.
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

func (s *DBStore) GetRequestDetail(id uint64) (*RequestDetail, error) {
	if s == nil || s.ch == nil {
		return nil, fmt.Errorf("clickhouse not available")
	}
	// Large full bodies can take longer to pull over the wire.
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

// Close gracefully shuts down DB connections.
func (s *DBStore) Close() {
	if s.rdb != nil {
		_ = s.rdb.Close()
	}
	if s.ch != nil {
		_ = s.ch.Close()
	}
}
