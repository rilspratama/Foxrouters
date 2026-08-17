// logstore.go — request-log storage backend (SQLite).
//
// FoxRouters historically offered ClickHouse as an analytics backend, but it
// was heavy (~700MB image, +RAM/disk cost) and removed Aug 2026. This file
// defines the LogStore interface so the store layer stays decoupled from the
// concrete SQLite implementation:
//
//	LOG_BACKEND=sqlite   (default) → embedded modernc.org/sqlite, ~0 ops cost
//	LOG_BACKEND=clickhouse         → deprecated; warns and falls back to sqlite
//
// The interface deliberately mirrors the async-batch pattern the old CH code
// used: producers push into channels (owned by Store), a consumer goroutine
// drains them into InsertRequestBatch/InsertRefreshBatch/InsertEventBatch.
// Query methods stay 1:1 with the pre-refactor Store method surface.
package db

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"
)

// LogStore is the pluggable persistence layer for request/refresh/event logs
// plus the dashboard analytics queries built on top. Every method must be
// safe to call from multiple goroutines (Store fans out batches from a
// single consumer goroutine, but callers may hit query methods concurrently).
type LogStore interface {
	// EnsureSchema creates tables/indexes if they don't exist. Called once
	// during Store construction; returning an error aborts startup.
	EnsureSchema(ctx context.Context) error

	// InsertRequestBatch persists a batch of request logs. Backends should
	// pick their own batching semantics (CH: PrepareBatch+Send, SQLite:
	// single tx with prepared insert).
	InsertRequestBatch(ctx context.Context, batch []RequestLog) error
	InsertRefreshBatch(ctx context.Context, batch []RefreshLog) error
	InsertEventBatch(ctx context.Context, batch []AccountEvent) error

	// Query methods — the dashboard hits these.
	GetRequestStats(ctx context.Context, since time.Time) (*RequestStats, error)
	GetModelStats(ctx context.Context, since time.Time, limit int) ([]ModelStats, error)
	GetRecentRequests(ctx context.Context, limit int, f RecentFilter) ([]RecentRequest, error)
	GetRequestDetail(ctx context.Context, id uint64) (*RequestDetail, error)

	// Close releases any backing resources.
	Close() error

	// Kind returns a short label ("sqlite") for logging.
	Kind() string
}

// ============================================================================
// Shared DTOs — used by every backend
// ============================================================================

// RequestLog is one persisted request/response entry.
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

// RequestStats is the aggregate over a time window (dashboard /history).
type RequestStats struct {
	TotalRequests  int     `json:"total_requests"`
	TotalErrors    int     `json:"total_errors"`
	ErrorRate      float64 `json:"error_rate_pct"`
	AvgLatencyMs   float64 `json:"avg_latency_ms"`
	TotalTokensIn  int     `json:"total_tokens_in"`
	TotalTokensOut int     `json:"total_tokens_out"`
	TotalTokens    int     `json:"total_tokens"`
}

// ModelStats is per-model breakdown of RequestStats (dashboard by_model).
type ModelStats struct {
	Model          string  `json:"model"`
	TotalRequests  int     `json:"total_requests"`
	TotalErrors    int     `json:"total_errors"`
	AvgLatencyMs   float64 `json:"avg_latency_ms"`
	TotalTokensIn  int     `json:"total_tokens_in"`
	TotalTokensOut int     `json:"total_tokens_out"`
	TotalTokens    int     `json:"total_tokens"`
}

// RecentFilter narrows the /history/recent listing. Zero values = no filter.
type RecentFilter struct {
	Model     string // exact model match
	Upstream  string // exact upstream match ("grok"|"codebuddy"|"freebuff")
	Status    string // "" (all), "200" (exact), or "2xx"/"3xx"/"4xx"/"5xx" (range)
	ErrorOnly bool   // only rows with a non-empty error_msg
	Hours     int    // >0: only rows within the last N hours; 0 = all time
}

// statusRange interprets a Status filter: "200" → exact, "2xx" → [200,300).
// Returns ok=false for any other value (including "").
func statusRange(s string) (lo, hi int, ok bool) {
	if len(s) == 3 && s[2] == 'x' && s[0] >= '0' && s[0] <= '5' && s[1] == 'x' {
		d := int(s[0] - '0')
		return d * 100, (d + 1) * 100, true
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 100 && n <= 599 {
		return n, n + 1, true
	}
	return 0, 0, false
}

// RecentRequest is the previews row used by /history/recent.
//
// ID is a decimal string so JavaScript JSON.parse does not lose UInt64
// precision (Number.MAX_SAFE_INTEGER = 2^53-1; our random 64-bit ids exceed
// that). The frontend passes it back verbatim to /history/detail/:id.
type RecentRequest struct {
	ID          string  `json:"id"`
	Timestamp   string  `json:"timestamp"`
	ClientKey   string  `json:"client_key"`
	Model       string  `json:"model"`
	Upstream    string  `json:"upstream"`
	AccountID   string  `json:"account_id"`
	StatusCode  int     `json:"status_code"`
	LatencyMs   int     `json:"latency_ms"`
	TokensIn    int     `json:"tokens_in"`
	TokensOut   int     `json:"tokens_out"`
	CacheHitPct float64 `json:"cache_hit_pct"`
	InputText   string  `json:"input_text,omitempty"`
	OutputText  string  `json:"output_text,omitempty"`
	ErrorMsg    string  `json:"error_msg,omitempty"`
}

// RequestDetail is a single log with full request/response JSON bodies.
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

// logBodyCap bounds a single stored request/response body in SQLite.
// Default 1 MiB — keeps full bodies for normal traffic while preventing
// multi-MB rows (huge-context requests) from bloating the DB and blocking
// readers past busy_timeout (prod incident 2026-08-17: request_body alone
// reached 20.6 GB and /history/recent 500'd with context deadline).
// Override via LOG_BODY_CAP_BYTES.
var logBodyCap = func() int {
	if v := os.Getenv("LOG_BODY_CAP_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 1 << 20 // 1 MiB
}()

// bodyString safely stringifies a nil-or-empty json.RawMessage, bounded to
// logBodyCap bytes. Oversized bodies are truncated with a marker so the
// detail view shows the head of the payload instead of silently dropping it.
func bodyString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if len(raw) <= logBodyCap {
		return string(raw)
	}
	out := string(raw[:logBodyCap])
	// Keep valid JSON shape when possible: close an open object/array.
	if strings.HasPrefix(out, "{") {
		out += " ...(truncated)"
	}
	return out
}

// max0 clamps a negative int to 0 (used when packing into unsigned CH columns).
func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
