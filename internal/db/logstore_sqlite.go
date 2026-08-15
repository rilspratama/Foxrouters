// logstore_sqlite.go — modernc.org/sqlite implementation of LogStore.
//
// Selected when LOG_BACKEND=sqlite (the default). Pure Go, no CGO, works
// in the alpine runtime image without pulling gcc.
//
//	File location: LOG_SQLITE_PATH (default /var/lib/foxrouters/logs.db)
//	Retention:     rows older than 90 days pruned by a background goroutine
//	               once per hour (mirrors the CH TTL semantics)
//	Concurrency:   sqlite serialises writes; we run WAL mode + a 5s busy
//	               timeout so reads and writes don't deadlock under load
//
// modernc.org/sqlite registers itself as the "sqlite" driver in database/sql.
package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const defaultSQLitePath = "/var/lib/foxrouters/logs.db"

// sqliteStore implements LogStore against a local file-backed SQLite DB.
type sqliteStore struct {
	db     *sql.DB
	path   string
	done   chan struct{}
	closed bool
}

func newSqliteStore() (LogStore, error) {
	path := envOr("LOG_SQLITE_PATH", defaultSQLitePath)
	// Create parent dir if writable — best effort; on read-only images the
	// user is expected to bind-mount the target.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			// Non-fatal: user might have pre-created the dir with tighter perms.
			slog.Warn("sqlite mkdir parent failed (continuing)", "module", "db-sqlite", "dir", dir, "error", err)
		}
	}

	// modernc.org/sqlite DSN mirrors mattn's format. Key pragmas:
	//   _journal_mode=WAL   → readers don't block writers
	//   _busy_timeout=5000  → 5s block on locked DB before returning SQLITE_BUSY
	//   _synchronous=NORMAL → fsync at commit only, fast + durable-enough for logs
	//   _foreign_keys=on    → future-proof
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(on)"
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite open %s: %w", path, err)
	}
	// WAL mode allows ONE writer + multiple concurrent readers. Capping the
	// pool at 1 serialised EVERY query (SELECTs included) behind a slow
	// 5MB-body INSERT, so dashboard /history polling hit busy_timeout and
	// returned 500 "context deadline exceeded". Use a small pool: writes
	// still serialise via busy_timeout, reads run in parallel.
	sdb.SetMaxOpenConns(4)
	sdb.SetMaxIdleConns(4)
	// Verify the DB is actually reachable.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := sdb.PingContext(ctx); err != nil {
		sdb.Close()
		return nil, fmt.Errorf("sqlite ping %s: %w", path, err)
	}

	s := &sqliteStore{
		db:   sdb,
		path: path,
		done: make(chan struct{}),
	}
	slog.Info("sqlite log store ready", "module", "db-sqlite", "path", path)
	return s, nil
}

func (s *sqliteStore) Kind() string { return "sqlite" }

func (s *sqliteStore) EnsureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS request_logs (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp     DATETIME NOT NULL,
			request_id    TEXT NOT NULL DEFAULT '',
			client_key    TEXT NOT NULL DEFAULT '',
			model         TEXT NOT NULL DEFAULT '',
			upstream      TEXT NOT NULL DEFAULT '',
			account_id    TEXT NOT NULL DEFAULT '',
			status_code   INTEGER NOT NULL DEFAULT 0,
			latency_ms    INTEGER NOT NULL DEFAULT 0,
			tokens_in     INTEGER NOT NULL DEFAULT 0,
			tokens_out    INTEGER NOT NULL DEFAULT 0,
			cache_hit_pct REAL NOT NULL DEFAULT -1,
			error_msg     TEXT NOT NULL DEFAULT '',
			input_text    TEXT NOT NULL DEFAULT '',
			output_text   TEXT NOT NULL DEFAULT '',
			request_body  TEXT NOT NULL DEFAULT '',
			response_body TEXT NOT NULL DEFAULT ''
		)`,
		// Migration: add cache_hit_pct column if missing. SQLite ALTER TABLE
		// ADD COLUMN is NOT idempotent — "duplicate column" error is expected
		// on restarts and silently ignored below.
		`ALTER TABLE request_logs ADD COLUMN cache_hit_pct REAL NOT NULL DEFAULT -1`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_timestamp ON request_logs(timestamp DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_model     ON request_logs(model)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_client    ON request_logs(client_key)`,

		`CREATE TABLE IF NOT EXISTS token_refresh_history (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp     DATETIME NOT NULL,
			account_email TEXT NOT NULL DEFAULT '',
			provider      TEXT NOT NULL DEFAULT '',
			success       INTEGER NOT NULL DEFAULT 0,
			error_msg     TEXT NOT NULL DEFAULT '',
			latency_ms    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_refresh_timestamp ON token_refresh_history(timestamp DESC)`,

		`CREATE TABLE IF NOT EXISTS account_events (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp  DATETIME NOT NULL,
			account_id TEXT NOT NULL DEFAULT '',
			provider   TEXT NOT NULL DEFAULT '',
			event_type TEXT NOT NULL DEFAULT '',
			event_data TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_timestamp ON account_events(timestamp DESC)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			// ALTER TABLE ADD COLUMN fails with "duplicate column" on restarts
			// — that's expected and safe to ignore. Everything else is fatal.
			if !strings.Contains(err.Error(), "duplicate column") {
				return fmt.Errorf("sqlite ensure schema: %w", err)
			}
		}
	}
	// Start the TTL sweeper only after schema is confirmed.
	go s.retentionLoop()
	return nil
}

// retentionLoop periodically deletes rows older than 90 days across all
// three log tables — mirrors the old ClickHouse TTL 90 DAY behaviour.
func (s *sqliteStore) retentionLoop() {
	// Run once shortly after startup, then hourly.
	first := time.NewTimer(30 * time.Second)
	defer first.Stop()
	tick := time.NewTicker(1 * time.Hour)
	defer tick.Stop()

	prune := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, tbl := range []string{"request_logs", "token_refresh_history", "account_events"} {
			q := fmt.Sprintf(`DELETE FROM %s WHERE timestamp < datetime('now', '-90 days')`, tbl)
			if _, err := s.db.ExecContext(ctx, q); err != nil {
				slog.Warn("sqlite retention prune failed", "module", "db-sqlite", "table", tbl, "error", err)
			}
		}
	}

	for {
		select {
		case <-first.C:
			prune()
		case <-tick.C:
			prune()
		case <-s.done:
			return
		}
	}
}

func (s *sqliteStore) InsertRequestBatch(ctx context.Context, batch []RequestLog) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		// Rollback is a no-op if Commit already succeeded.
		_ = tx.Rollback()
	}()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO request_logs (
		timestamp, request_id, client_key, model, upstream, account_id,
		status_code, latency_ms, tokens_in, tokens_out, error_msg,
		input_text, output_text, request_body, response_body, cache_hit_pct
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range batch {
		ts := r.Timestamp.UTC()
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		// Extract cache hit % from response body at write time (cheap, runs
		// once per request in the async batch — not on every list query).
		ch := extractCacheHitPct(r.ResponseBody)
		if _, err := stmt.ExecContext(ctx,
			ts,
			r.RequestID,
			r.ClientKey,
			r.Model,
			r.Upstream,
			r.AccountID,
			r.StatusCode,
			max0(r.LatencyMs),
			max0(r.TokensIn),
			max0(r.TokensOut),
			r.ErrorMsg,
			r.InputText,
			r.OutputText,
			bodyString(r.RequestBody),
			bodyString(r.ResponseBody),
			ch,
		); err != nil {
			slog.Error("sqlite reqlog insert", "module", "db-sqlite", "error", err)
		}
	}
	return tx.Commit()
}

func (s *sqliteStore) InsertRefreshBatch(ctx context.Context, batch []RefreshLog) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO token_refresh_history (
		timestamp, account_email, provider, success, error_msg, latency_ms
	) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range batch {
		suc := 0
		if r.Success {
			suc = 1
		}
		ts := r.Timestamp.UTC()
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		if _, err := stmt.ExecContext(ctx, ts, r.AccountEmail, r.Provider, suc, r.ErrorMsg, max0(r.LatencyMs)); err != nil {
			slog.Error("sqlite refresh insert", "module", "db-sqlite", "error", err)
		}
	}
	return tx.Commit()
}

func (s *sqliteStore) InsertEventBatch(ctx context.Context, batch []AccountEvent) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO account_events (
		timestamp, account_id, provider, event_type, event_data
	) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range batch {
		var data string
		if e.EventData != nil {
			raw, _ := json.Marshal(e.EventData)
			data = string(raw)
		}
		ts := e.Timestamp.UTC()
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		if _, err := stmt.ExecContext(ctx, ts, e.AccountID, e.Provider, e.EventType, data); err != nil {
			slog.Error("sqlite event insert", "module", "db-sqlite", "error", err)
		}
	}
	return tx.Commit()
}

func (s *sqliteStore) GetRequestStats(ctx context.Context, since time.Time) (*RequestStats, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0),
			COALESCE(AVG(latency_ms), 0),
			COALESCE(SUM(tokens_in), 0),
			COALESCE(SUM(tokens_out), 0)
		FROM request_logs
		WHERE timestamp >= ?
	`, since.UTC())

	stats := &RequestStats{}
	var total, errors, tin, tout int64
	var avg float64
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

func (s *sqliteStore) GetModelStats(ctx context.Context, since time.Time, limit int) ([]ModelStats, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			model,
			COUNT(*) AS total_requests,
			SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END) AS total_errors,
			COALESCE(AVG(latency_ms), 0) AS avg_latency,
			COALESCE(SUM(tokens_in), 0) AS tokens_in,
			COALESCE(SUM(tokens_out), 0) AS tokens_out
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
		var tr, te, tin, tout int64
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
		return nil, fmt.Errorf("sqlite model stats iteration: %w", err)
	}
	return out, nil
}

func (s *sqliteStore) GetRecentRequests(ctx context.Context, limit int, f RecentFilter) ([]RecentRequest, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	q := `SELECT id, timestamp, client_key, model, upstream, account_id,
	       status_code, latency_ms, tokens_in, tokens_out, error_msg,
	       input_text, output_text, cache_hit_pct
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
		where = append(where, "timestamp >= datetime('now', ?)")
		args = append(args, fmt.Sprintf("-%d hours", f.Hours))
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY timestamp DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RecentRequest, 0, limit)
	for rows.Next() {
		var r RecentRequest
		var id int64
		var ts time.Time
		if err := rows.Scan(&id, &ts, &r.ClientKey, &r.Model, &r.Upstream,
			&r.AccountID, &r.StatusCode, &r.LatencyMs, &r.TokensIn, &r.TokensOut,
			&r.ErrorMsg, &r.InputText, &r.OutputText, &r.CacheHitPct); err != nil {
			slog.Error("sqlite recent scan", "module", "db-sqlite", "error", err)
			continue
		}
		// ID is stringified for JS precision parity with the CH backend
		// (the frontend sends this exact value back on /history/detail/:id).
		r.ID = strconv.FormatInt(id, 10)
		r.Timestamp = ts.UTC().Format("2006-01-02 15:04:05")
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		slog.Error("sqlite recent iteration", "module", "db-sqlite", "error", err)
	}
	return out, nil
}

func (s *sqliteStore) GetRequestDetail(ctx context.Context, id uint64) (*RequestDetail, error) {
	// Callers pass a uint64 — SQLite's PK is signed int64 but we only ever
	// insert positive AUTOINCREMENT values, so the range fits.
	var d RequestDetail
	var rawID int64
	var ts time.Time
	var reqBody, respBody string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, timestamp, client_key, model, upstream, account_id,
		       status_code, latency_ms, tokens_in, tokens_out, error_msg,
		       input_text, output_text, request_body, response_body
		FROM request_logs
		WHERE id = ?
		LIMIT 1
	`, int64(id)).Scan(&rawID, &ts, &d.ClientKey, &d.Model, &d.Upstream,
		&d.AccountID, &d.StatusCode, &d.LatencyMs, &d.TokensIn, &d.TokensOut,
		&d.ErrorMsg, &d.InputText, &d.OutputText, &reqBody, &respBody)
	if err != nil {
		return nil, err
	}
	d.ID = strconv.FormatInt(rawID, 10)
	d.Timestamp = ts.UTC().Format("2006-01-02 15:04:05")
	// Bodies must be valid JSON for json.RawMessage marshaling. Some legacy /
	// streamed rows hold raw SSE text (`data: {...}` lines) which is NOT valid
	// JSON — wrap those as a JSON string so the dashboard never fails to load.
	if reqBody != "" {
		if json.Valid([]byte(reqBody)) {
			d.RequestBody = []byte(reqBody)
		} else {
			d.RequestBody, _ = json.Marshal(reqBody)
		}
	}
	if respBody != "" {
		if json.Valid([]byte(respBody)) {
			d.ResponseBody = []byte(respBody)
		} else {
			d.ResponseBody, _ = json.Marshal(respBody)
		}
	}
	return &d, nil
}

func (s *sqliteStore) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.done)
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// extractCacheHitPct parses usage from a stored response body and returns
// prompt_cache_hit_tokens / prompt_tokens * 100 (rounded to 1 decimal).
// Returns -1 if usage is absent or prompt_tokens is 0; 0 if no cache hit.
// Handles both non-stream (usage at top level) and stream (usage in last
// SSE data chunk) response bodies stored in request_logs.response_body.
func extractCacheHitPct(body []byte) float64 {
	if len(body) == 0 {
		return -1
	}
	// Try top-level usage (non-stream response)
	var top map[string]any
	if err := json.Unmarshal(body, &top); err == nil {
		if u, ok := top["usage"].(map[string]any); ok {
			if pct := cachePctFromUsage(u); pct >= 0 {
				return pct
			}
		}
	}
	// Try SSE stream: scan for last data chunk with usage
	s := string(body)
	for i := len(s) - 1; i >= 0; {
		idx := lastIndexDataLine(s[:i])
		if idx < 0 {
			break
		}
		chunk := s[idx:]
		// trim to end of this data line
		if nl := indexOf(chunk, "\n"); nl >= 0 {
			chunk = chunk[:nl]
		}
		chunk = strings.TrimPrefix(chunk, "data: ")
		var m map[string]any
		if json.Unmarshal([]byte(chunk), &m) == nil {
			if u, ok := m["usage"].(map[string]any); ok {
				return cachePctFromUsage(u)
			}
		}
		i = idx
	}
	return -1
}

// cachePctFromUsage computes hit/prompt*100 from a usage map.
func cachePctFromUsage(u map[string]any) float64 {
	pt, _ := toFloat64(u["prompt_tokens"])
	if pt <= 0 {
		return -1
	}
	hit, ok := toFloat64(u["prompt_cache_hit_tokens"])
	if !ok || hit <= 0 {
		if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
			hit, _ = toFloat64(d["cached_tokens"])
		}
	}
	if hit <= 0 {
		return 0
	}
	return float64(int(hit/pt*1000)) / 10
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// lastIndexDataLine finds the start of the last "data: " line in s.
func lastIndexDataLine(s string) int {
	for i := len(s) - 1; i >= 5; i-- {
		if s[i] == ' ' && s[i-1] == ':' && s[i-2] == 'a' && s[i-3] == 't' && s[i-4] == 'a' && s[i-5] == 'd' {
			if i >= 5 && (i-5 == 0 || s[i-6] == '\n') {
				return i - 5
			}
		}
	}
	return -1
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
