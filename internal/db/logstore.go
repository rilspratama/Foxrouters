// logstore.go — pluggable request-log storage backend.
//
// FoxRouters historically only wrote request/response history to ClickHouse.
// ClickHouse is great for analytics but heavy (~700MB image, +RAM/disk cost)
// which is overkill for small deployments. This file defines a LogStore
// interface so the caller can pick between backends via LOG_BACKEND env:
//
//	LOG_BACKEND=sqlite      (default) → embedded modernc.org/sqlite, ~0 ops cost
//	LOG_BACKEND=clickhouse            → existing analytics backend
//
// The interface deliberately mirrors the async-batch pattern the old CH code
// used: producers push into channels (owned by Store), a consumer goroutine
// drains them into InsertRequestBatch/InsertRefreshBatch/InsertEventBatch.
// Query methods stay 1:1 with the pre-refactor Store method surface.
package db

import (
	"context"
	"encoding/json"
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
	GetRecentRequests(ctx context.Context, limit int) ([]RecentRequest, error)
	GetRequestDetail(ctx context.Context, id uint64) (*RequestDetail, error)

	// Close releases any backing resources.
	Close() error

	// Kind returns a short label ("clickhouse"|"sqlite") for logging.
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

// bodyString safely stringifies a nil-or-empty json.RawMessage.
func bodyString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	return string(raw)
}

// max0 clamps a negative int to 0 (used when packing into unsigned CH columns).
func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
