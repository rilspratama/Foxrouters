// logstore_clickhouse.go — ClickHouse implementation of LogStore.
//
// This is the historic FoxRouters backend: analytics-optimised columnar
// storage with ZSTD compression on the body columns and 90-day TTL.
// Selected when LOG_BACKEND=clickhouse.
package db

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// clickhouseStore implements LogStore over the CH native protocol.
type clickhouseStore struct {
	conn driver.Conn
}

// newClickhouseStore opens a connection to CH, auto-creates the database
// via a bootstrap client, then returns the ready-to-use store. Callers are
// expected to run EnsureSchema afterwards.
func newClickhouseStore() (LogStore, error) {
	addr := clickhouseAddr()
	dbName := clickhouseDatabase()
	user := envOr("CLICKHOUSE_USER", "default")
	pass := envOr("CLICKHOUSE_PASSWORD", "")

	// Connect to 'default' first so we can CREATE DATABASE IF NOT EXISTS.
	bootstrap, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{
			Database: "default",
			Username: user,
			Password: pass,
		},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse open (bootstrap): %w", err)
	}
	if len(dbName) == 0 || !regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`).MatchString(dbName) {
		bootstrap.Close()
		return nil, fmt.Errorf("invalid CLICKHOUSE_DB name %q: must be alphanumeric/underscore, start with letter", dbName)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := bootstrap.Exec(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", dbName)); err != nil {
		bootstrap.Close()
		return nil, fmt.Errorf("clickhouse create database: %w", err)
	}
	bootstrap.Close()

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{
			Database: dbName,
			Username: user,
			Password: pass,
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
	if err := conn.Ping(ctx); err != nil {
		return nil, fmt.Errorf("clickhouse ping: %w", err)
	}
	slog.Info("clickhouse log store ready", "module", "db-ch", "addr", addr, "db", dbName)
	return &clickhouseStore{conn: conn}, nil
}

func (c *clickhouseStore) Kind() string { return "clickhouse" }

func (c *clickhouseStore) EnsureSchema(ctx context.Context) error {
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
		if err := c.conn.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// newLogID returns a random 64-bit id for a request_logs row. Falls back to
// UnixNano on rand failure — good enough for uniqueness at our RPS.
func newLogID() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint64(time.Now().UnixNano())
	}
	return binary.LittleEndian.Uint64(b[:])
}

func (c *clickhouseStore) InsertRequestBatch(ctx context.Context, batch []RequestLog) error {
	if len(batch) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, `INSERT INTO request_logs (
		id, timestamp, request_id, client_key, model, upstream, account_id,
		status_code, latency_ms, tokens_in, tokens_out, error_msg,
		input_text, output_text, request_body, response_body
	)`)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	for _, r := range batch {
		ts := r.Timestamp.UTC()
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		if err := b.Append(
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
	return b.Send()
}

func (c *clickhouseStore) InsertRefreshBatch(ctx context.Context, batch []RefreshLog) error {
	if len(batch) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, `INSERT INTO token_refresh_history (
		timestamp, account_email, provider, success, error_msg, latency_ms
	)`)
	if err != nil {
		return err
	}
	for _, r := range batch {
		suc := uint8(0)
		if r.Success {
			suc = 1
		}
		ts := r.Timestamp.UTC()
		_ = b.Append(ts, r.AccountEmail, r.Provider, suc, r.ErrorMsg, uint32(max0(r.LatencyMs)))
	}
	return b.Send()
}

func (c *clickhouseStore) InsertEventBatch(ctx context.Context, batch []AccountEvent) error {
	if len(batch) == 0 {
		return nil
	}
	b, err := c.conn.PrepareBatch(ctx, `INSERT INTO account_events (
		timestamp, account_id, provider, event_type, event_data
	)`)
	if err != nil {
		return err
	}
	for _, e := range batch {
		var data string
		if e.EventData != nil {
			raw, _ := json.Marshal(e.EventData)
			data = string(raw)
		}
		_ = b.Append(e.Timestamp.UTC(), e.AccountID, e.Provider, e.EventType, data)
	}
	return b.Send()
}

func (c *clickhouseStore) GetRequestStats(ctx context.Context, since time.Time) (*RequestStats, error) {
	stats := &RequestStats{}
	row := c.conn.QueryRow(ctx, `
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

func (c *clickhouseStore) GetModelStats(ctx context.Context, since time.Time, limit int) ([]ModelStats, error) {
	rows, err := c.conn.Query(ctx, `
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
	var out []ModelStats
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
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("model stats iteration: %w", err)
	}
	return out, nil
}

func (c *clickhouseStore) GetRecentRequests(ctx context.Context, limit int, f RecentFilter) ([]RecentRequest, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	q := `SELECT id, timestamp, client_key, model, upstream, account_id,
	       status_code, latency_ms, tokens_in, tokens_out, error_msg,
	       input_text, output_text
	FROM request_logs`
	args := make([]any, 0, 6)
	var where []string
	if f.Model != "" {
		where = append(where, "model = ?")
		args = append(args, f.Model)
	}
	if f.Upstream != "" {
		where = append(where, "upstream = ?")
		args = append(args, f.Upstream)
	}
	if f.Status != "" {
		if lo, hi, ok := statusRange(f.Status); ok {
			where = append(where, "status_code >= ? AND status_code < ?")
			args = append(args, lo, hi)
		}
	}
	if f.ErrorOnly {
		where = append(where, "error_msg != ''")
	}
	if f.Hours > 0 {
		where = append(where, "timestamp >= now() - INTERVAL ? HOUR")
		args = append(args, f.Hours)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY timestamp DESC LIMIT ?"
	args = append(args, limit)

	rows, err := c.conn.Query(ctx, q, args...)
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

func (c *clickhouseStore) GetRequestDetail(ctx context.Context, id uint64) (*RequestDetail, error) {
	var d RequestDetail
	var rawID uint64
	var ts time.Time
	var sc uint16
	var lat, tin, tout uint32
	var reqBody, respBody string
	err := c.conn.QueryRow(ctx, `
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
		d.RequestBody = []byte(reqBody)
	}
	if respBody != "" {
		d.ResponseBody = []byte(respBody)
	}
	return &d, nil
}

func (c *clickhouseStore) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
