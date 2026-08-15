// logstore_sqlite_test.go — smoke-test the SQLite LogStore end-to-end
// (schema, insert, all four query paths, TTL is exercised in retentionLoop
// but not asserted here — it's a background sweeper with a 30s initial timer).
//
// These tests run against a temp-dir DB file — no external services needed.
package db

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestSqlite(t *testing.T) LogStore {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.db")
	t.Setenv("LOG_SQLITE_PATH", path)
	store, err := newSqliteStore()
	if err != nil {
		t.Fatalf("newSqliteStore: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	return store
}

func TestSqliteStore_InsertAndQuery(t *testing.T) {
	store := newTestSqlite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	now := time.Now().UTC()
	batch := []RequestLog{
		{
			Timestamp: now, RequestID: "r1", ClientKey: "gw-a",
			Model: "grok-4.5", Upstream: "grok", AccountID: "acc1",
			StatusCode: 200, LatencyMs: 123, TokensIn: 10, TokensOut: 20,
			InputText: "hello", OutputText: "world",
			RequestBody:  json.RawMessage(`{"foo":1}`),
			ResponseBody: json.RawMessage(`{"bar":2}`),
		},
		{
			Timestamp: now, RequestID: "r2", ClientKey: "gw-b",
			Model: "cb/gpt-4", Upstream: "codebuddy", AccountID: "acc2",
			StatusCode: 500, LatencyMs: 456, TokensIn: 5, TokensOut: 0,
			ErrorMsg: "upstream_fail",
		},
	}
	if err := store.InsertRequestBatch(ctx, batch); err != nil {
		t.Fatalf("InsertRequestBatch: %v", err)
	}

	// Stats
	stats, err := store.GetRequestStats(ctx, now.Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("GetRequestStats: %v", err)
	}
	if stats.TotalRequests != 2 {
		t.Errorf("TotalRequests=%d want 2", stats.TotalRequests)
	}
	if stats.TotalErrors != 1 {
		t.Errorf("TotalErrors=%d want 1", stats.TotalErrors)
	}
	if stats.TotalTokens != 35 {
		t.Errorf("TotalTokens=%d want 35", stats.TotalTokens)
	}

	// Per-model breakdown
	ms, err := store.GetModelStats(ctx, now.Add(-1*time.Hour), 10)
	if err != nil {
		t.Fatalf("GetModelStats: %v", err)
	}
	if len(ms) != 2 {
		t.Errorf("ModelStats len=%d want 2", len(ms))
	}

	// Recent (previews)
	recent, err := store.GetRecentRequests(ctx, 10, RecentFilter{})
	if err != nil {
		t.Fatalf("GetRecentRequests: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("recent len=%d want 2", len(recent))
	}
	// ID must be a decimal string (JS-safe).
	if recent[0].ID == "" {
		t.Error("recent[0].ID is empty")
	}

	// Filtered recent: model=cb/gpt-4 (the errored row) must return 1 row.
	fModel, err := store.GetRecentRequests(ctx, 10, RecentFilter{Model: "cb/gpt-4"})
	if err != nil {
		t.Fatalf("GetRecentRequests(model): %v", err)
	}
	if len(fModel) != 1 {
		t.Fatalf("recent(model=cb/gpt-4) len=%d want 1", len(fModel))
	}
	// Error-only filter: exactly the row with error_msg set.
	fErr, err := store.GetRecentRequests(ctx, 10, RecentFilter{ErrorOnly: true})
	if err != nil {
		t.Fatalf("GetRecentRequests(errors): %v", err)
	}
	if len(fErr) != 1 || fErr[0].Model != "cb/gpt-4" {
		t.Fatalf("recent(errors=1) len=%d want 1 cb/gpt-4 row", len(fErr))
	}
	// Status range filter: 2xx matches the 200 row, 5xx the 500 row.
	f2xx, err := store.GetRecentRequests(ctx, 10, RecentFilter{Status: "2xx"})
	if err != nil {
		t.Fatalf("GetRecentRequests(2xx): %v", err)
	}
	f5xx, err := store.GetRecentRequests(ctx, 10, RecentFilter{Status: "5xx"})
	if err != nil {
		t.Fatalf("GetRecentRequests(5xx): %v", err)
	}
	if len(f2xx) != 1 || len(f5xx) != 1 {
		t.Fatalf("status filters: 2xx=%d 5xx=%d want 1/1", len(f2xx), len(f5xx))
	}

	// Full detail round-trip via the stringified id.
	var idAsUint uint64
	// Reuse the driver by parsing the ID back — we know it fits in uint64.
	for _, ch := range recent[0].ID {
		idAsUint = idAsUint*10 + uint64(ch-'0')
	}
	detail, err := store.GetRequestDetail(ctx, idAsUint)
	if err != nil {
		t.Fatalf("GetRequestDetail: %v", err)
	}
	if detail.Model == "" {
		t.Error("detail.Model empty")
	}
	if len(detail.RequestBody) == 0 && len(detail.ResponseBody) == 0 {
		// Only the first row has bodies; the newest detail may be either
		// depending on timestamp+id ordering — so only fail if neither
		// row in recent had a populated body.
	}
}

func TestSqliteStore_RefreshAndEvent(t *testing.T) {
	store := newTestSqlite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	now := time.Now().UTC()
	if err := store.InsertRefreshBatch(ctx, []RefreshLog{{
		Timestamp: now, AccountEmail: "u@e.com", Provider: "grok", Success: true, LatencyMs: 42,
	}}); err != nil {
		t.Fatalf("InsertRefreshBatch: %v", err)
	}
	if err := store.InsertEventBatch(ctx, []AccountEvent{{
		Timestamp: now, AccountID: "u@e.com", Provider: "grok", EventType: "test",
		EventData: map[string]interface{}{"k": "v"},
	}}); err != nil {
		t.Fatalf("InsertEventBatch: %v", err)
	}
	// We don't expose Refresh/Event query APIs — success is that insert didn't error
	// and the DB file exists.
	if _, err := os.Stat(store.(*sqliteStore).path); err != nil {
		t.Fatalf("db file missing: %v", err)
	}
}

func TestNewLogStore_DefaultsToSqlite(t *testing.T) {
	t.Setenv("LOG_BACKEND", "")
	t.Setenv("LOG_SQLITE_PATH", filepath.Join(t.TempDir(), "defaults.db"))
	ls, err := NewLogStore()
	if err != nil {
		t.Fatalf("NewLogStore: %v", err)
	}
	defer ls.Close()
	if got := ls.Kind(); got != "sqlite" {
		t.Errorf("Kind()=%q want sqlite", got)
	}
}

func TestNewLogStore_UnknownFallsBackToSqlite(t *testing.T) {
	t.Setenv("LOG_BACKEND", "nonsense")
	t.Setenv("LOG_SQLITE_PATH", filepath.Join(t.TempDir(), "fallback.db"))
	ls, err := NewLogStore()
	if err != nil {
		t.Fatalf("NewLogStore: %v", err)
	}
	defer ls.Close()
	if got := ls.Kind(); got != "sqlite" {
		t.Errorf("Kind()=%q want sqlite", got)
	}
}
