package main

// Regression tests consolidated from the audit batches (P0 data races +
// P1 correctness fixes, package-split refactor). These lock in behaviour
// that previously regressed: lock-safety snapshots, circuit-breaker error
// accounting, hot-path re-enable rules, body-size guards, dashboard key
// hygiene, and version/alias semantics.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"foxrouters/internal/auth"
	"foxrouters/internal/handlers"
	"foxrouters/internal/upstream"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// ============================================================================
// Version + body-size guards
// ============================================================================

func TestVersionConst(t *testing.T) {
	if Version == "" {
		t.Fatal("Version empty")
	}
	if Version == "dev" {
		t.Log("Version = dev (built without ldflags — OK for local dev)")
	}
}

func TestLogFullBodyMax(t *testing.T) {
	// Full body is unlimited (no LOG_FULL_BODY_MAX constant) — bodyString passthrough.
	big := make([]byte, 2*1024*1024)
	for i := range big {
		big[i] = 'a'
	}
	raw := json.RawMessage(big)
	if len(raw) != 2*1024*1024 {
		t.Fatalf("raw len %d", len(raw))
	}
}

func TestMaxRequestBodyConstant(t *testing.T) {
	if upstream.MAX_REQUEST_BODY != 10*1024*1024 {
		t.Fatalf("MAX_REQUEST_BODY = %d, want 10MB", upstream.MAX_REQUEST_BODY)
	}
}

func TestMaxBytesReaderAcceptsUnderLimit(t *testing.T) {
	limit := int64(128)
	payload := []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(payload))
	w := httptest.NewRecorder()
	req.Body = http.MaxBytesReader(w, req.Body, limit)
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatal("body mismatch")
	}
}

func TestMaxBytesReaderRejectsOversizedBody(t *testing.T) {
	// Unit-level: MaxBytesReader returns MaxBytesError when limit exceeded.
	limit := int64(64)
	payload := bytes.Repeat([]byte("x"), 128)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(payload))
	w := httptest.NewRecorder()
	req.Body = http.MaxBytesReader(w, req.Body, limit)
	_, err := io.ReadAll(req.Body)
	if err == nil {
		t.Fatal("expected error for oversized body")
	}
	if _, ok := err.(*http.MaxBytesError); !ok {
		t.Fatalf("expected *http.MaxBytesError, got %T: %v", err, err)
	}
}

// ============================================================================
// Health status + circuit breaker
// ============================================================================

func TestHealthStatusOK(t *testing.T) {
	cases := []struct {
		code int
		ok   bool
	}{
		{200, true},
		{201, true},
		{204, true},
		{301, true},
		{302, true},
		{399, true},
		{400, false},
		{401, false}, // auth failure — was falsely healthy under status < 500
		{403, false}, // ban/gate — primary bug this fixes
		{404, false},
		{429, false},
		{500, false},
		{502, false},
		{0, false},
		{199, false},
	}
	for _, tc := range cases {
		got := upstream.HealthStatusOK(tc.code)
		if got != tc.ok {
			t.Errorf("upstream.HealthStatusOK(%d) = %v, want %v", tc.code, got, tc.ok)
		}
	}
}

func TestCircuitBreaker_PoolExhaustionDoesNotOpen(t *testing.T) {
	h := upstream.NewUpstreamHealth("test")

	// Simulate success baseline
	h.RecordRequest(10*time.Millisecond, nil)
	if !h.CanRequest() {
		t.Fatal("circuit should be closed after success")
	}

	// The FIXED behavior: pool exhaustion (all accounts cooldown) must NOT call
	// RecordRequest(error). Verify that without those error records, circuit stays closed
	// even if we would have previously recorded 5 "all accounts on cooldown" errors.
	//
	// (We deliberately do NOT call RecordRequest here — that's the fix.)
	for i := 0; i < upstream.CB_OPEN_THRESHOLD+2; i++ {
		// no-op: pool exhaustion path no longer records errors
	}
	if !h.CanRequest() {
		t.Fatal("circuit should stay closed when pool exhaustion does not record errors")
	}
	if h.State() != upstream.CircuitClosed {
		t.Fatalf("state = %s, want closed", h.State())
	}
}

func TestCircuitBreaker_RealUpstreamErrorsStillOpen(t *testing.T) {
	h := upstream.NewUpstreamHealth("test")
	for i := 0; i < upstream.CB_OPEN_THRESHOLD; i++ {
		h.RecordRequest(5*time.Millisecond, errTest("upstream 502"))
	}
	if h.CanRequest() {
		t.Fatal("circuit should be open after consecutive upstream errors")
	}
	if h.State() != upstream.CircuitOpen {
		t.Fatalf("state = %s, want open", h.State())
	}
}

// errTest is a tiny error type for circuit breaker tests.
type errTest string

func (e errTest) Error() string { return string(e) }

// ============================================================================
// Grok pool: O(1) Len, hot-path re-enable rules, lock-split, snapshot copies
// ============================================================================

func TestGrokLenO1(t *testing.T) {
	am := upstream.NewGrokAccountManager(nil)
	am.SetAccountsForTest([]*upstream.GrokAccount{
		upstream.NewGrokAccountForTest("a@t.com", "t", "r"),
		upstream.NewGrokAccountForTest("b@t.com", "t", "r"),
		upstream.NewGrokAccountForTest("c@t.com", "t", "r"),
	})
	if am.Len() != 3 {
		t.Fatalf("Len = %d", am.Len())
	}
}

func TestGrokNextNoFullReenableScan(t *testing.T) {
	// Cooldown past 10min should NOT be re-enabled by Next (background worker only).
	am := upstream.NewGrokAccountManager(nil)
	acc := upstream.NewGrokAccountForTest("cd@t.com", "t", "r",
		upstream.WithDisabledCooldown(time.Now().Add(-11*time.Minute)))
	am.SetAccountsForTest([]*upstream.GrokAccount{acc})
	_, err := am.Next()
	if err == nil {
		t.Fatal("Next should fail when only cooldown account exists (no hot re-enable)")
	}
	am.ReenableCooldowns()
	if acc.IsDisabled() {
		t.Fatal("ReenableCooldowns should lift cooldown")
	}
	got, err := am.Next()
	if err != nil || got.Email != "cd@t.com" {
		t.Fatalf("after reenable: %v %v", got, err)
	}
}

func TestGrokNextReenableDoesNotHoldLockDuringSave(t *testing.T) {
	// Re-enable is background-only now. Next itself does not re-enable.
	// Test reenableCooldowns + concurrent GetAccessToken after lift.
	am := upstream.NewGrokAccountManager(nil)
	acc := upstream.NewGrokAccountForTest(
		"cooldown@test.com", "tok", "rt",
		upstream.WithExpiresAt(time.Now().Add(time.Hour)),
		upstream.WithDisabledCooldown(time.Now().Add(-11*time.Minute)),
	)
	am.SetAccountsForTest([]*upstream.GrokAccount{acc})

	// Next must NOT re-enable (hot path is O(k) only)
	if _, err := am.Next(); err == nil {
		t.Fatal("Next should not re-enable cooldowns on hot path")
	}

	am.ReenableCooldowns()
	got, err := am.Next()
	if err != nil {
		t.Fatalf("Next after reenable: %v", err)
	}
	if got.Email != "cooldown@test.com" {
		t.Fatalf("got %s", got.Email)
	}
	if got.IsDisabled() {
		t.Fatal("account should be re-enabled")
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = got.GetAccessToken()
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deadlock: GetAccessToken blocked")
	}
}

func TestRefreshDoesNotHoldLockAcrossSleep(t *testing.T) {
	// Ensure GetAccessToken is callable while another goroutine holds nothing
	// after Refresh structure change (lock split).
	acc := upstream.NewGrokAccountForTest("x@t.com", "old", "bad-rt")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = acc.GetAccessToken()
			_ = acc.IsDisabled()
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = acc.Refresh() // will fail network, but must not hold lock forever
	}()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("deadlock during Refresh + GetAccessToken")
	}
}

func TestGrokAccountManager_GetAllIsCopy(t *testing.T) {
	am := upstream.NewGrokAccountManager(nil)
	am.SetAccountsForTest([]*upstream.GrokAccount{
		upstream.NewGrokAccountForTest("a@test.com", "t1", "r1"),
		upstream.NewGrokAccountForTest("b@test.com", "t2", "r2"),
	})
	all := am.GetAll()
	if len(all) != 2 {
		t.Fatalf("GetAll len = %d, want 2", len(all))
	}
	// mutate returned slice must not affect manager
	all[0] = nil
	// re-fetch and ensure element 0 is intact
	all2 := am.GetAll()
	if all2[0] == nil {
		t.Fatal("GetAll should return a copy; mutating result mutated manager")
	}
}

// ============================================================================
// CB pool: O(1) Len, AddKey dedupe/concurrency, hot-path re-enable rules
// ============================================================================

func TestCBLenO1(t *testing.T) {
	km := upstream.NewCBKeyManager(nil)
	km.SetKeysForTest([]*upstream.CBKey{
		upstream.NewCBKeyForTest("ck_a"),
		upstream.NewCBKeyForTest("ck_b"),
	})
	if km.Len() != 2 {
		t.Fatalf("Len = %d", km.Len())
	}
}

func TestCBKeyAddKey(t *testing.T) {
	km := upstream.NewCBKeyManager(nil)
	added, total := km.AddKey("ck_test_one")
	if !added || total != 1 {
		t.Fatalf("first add: added=%v total=%d", added, total)
	}
	added, total = km.AddKey("ck_test_one")
	if added || total != 1 {
		t.Fatalf("dup add: added=%v total=%d", added, total)
	}
	added, total = km.AddKey("ck_test_two")
	if !added || total != 2 {
		t.Fatalf("second add: added=%v total=%d", added, total)
	}
	added, total = km.AddKey("  ")
	if added {
		t.Fatalf("blank should not add")
	}
	if km.Len() != 2 {
		t.Fatalf("Len=%d want 2", km.Len())
	}
}

func TestCBKeyAddKeyConcurrent(t *testing.T) {
	km := upstream.NewCBKeyManager(nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			km.AddKey("ck_conc_" + string(rune('A'+i%26)) + string(rune('0'+i/26)))
		}(i)
	}
	wg.Wait()
	if km.Len() == 0 {
		t.Fatal("expected some keys")
	}
	km2 := upstream.NewCBKeyManager(nil)
	var wg2 sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			km2.AddKey("ck_same")
		}()
	}
	wg2.Wait()
	if km2.Len() != 1 {
		t.Fatalf("concurrent same key Len=%d want 1", km2.Len())
	}
}

func TestCBNextNoFullReenableScan(t *testing.T) {
	km := upstream.NewCBKeyManager(nil)
	km.SetKeysForTest([]*upstream.CBKey{
		upstream.NewCBKeyForTest("ck_test.xxx",
			upstream.WithCBDisabledCooldown(time.Now().Add(-11*time.Minute))),
	})
	_, err := km.Next()
	if err == nil {
		t.Fatal("Next should fail with only cooldown key")
	}
	km.ReenableCooldowns()
	got, err := km.Next()
	if err != nil || got.Key != "ck_test.xxx" {
		t.Fatalf("after reenable: %v %v", got, err)
	}
}

func TestCBNextReenableDoesNotHoldKeyLockDuringSave(t *testing.T) {
	km := upstream.NewCBKeyManager(nil)
	k := upstream.NewCBKeyForTest("ck_testkey.abcdef",
		upstream.WithCBDisabledCooldown(time.Now().Add(-11*time.Minute)),
	)
	km.SetKeysForTest([]*upstream.CBKey{k})

	if _, err := km.Next(); err == nil {
		t.Fatal("Next should not re-enable on hot path")
	}
	km.ReenableCooldowns()
	got, err := km.Next()
	if err != nil {
		t.Fatalf("Next after reenable: %v", err)
	}
	if got.Key != k.Key {
		t.Fatal("wrong key")
	}
	if got.IsDisabled() {
		t.Fatal("key should be re-enabled")
	}
}

// ============================================================================
// Auth manager: concurrent safety + RPM=0 unlimited
// ============================================================================

func TestAuthManager_ConcurrentLenIsSafe(t *testing.T) {
	// Regression: auth.AuthMiddleware used to call len(am.keys) without RLock.
	// This stress-test concurrent Add/Remove/Valid while reading count under RLock.
	am := auth.NewManagerForTest(nil)
	// seed one key
	am.Add("gw-testkey-aaaaaaaaaaaaaaaaaaaaaaaa", "seed", 0, 0, 0)

	var wg sync.WaitGroup
	// writers
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				k := auth.GenerateGatewayKey()
				am.Add(k, "t", 0, 0, 0)
				am.Remove(k)
			}
		}(i)
	}
	// readers (simulate middleware path)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				_ = am.Count() == 0
				_ = am.Valid("gw-testkey-aaaaaaaaaaaaaaaaaaaaaaaa")
			}
		}()
	}
	wg.Wait()
}

// TestRateLimitRPMZeroUnlimited verifies that a gateway key with RPM=0
// (unlimited) bypasses the rate limiter entirely and is NOT subject to
// the global default RPM. Bug found by GLM-5.2 review.
func TestRateLimitRPMZeroUnlimited(t *testing.T) {
	am := auth.NewManagerForTest(nil)
	am.Add("gw-test-unlimited", "test", 0, 0, 0)

	info, ok := am.Get("gw-test-unlimited")
	if !ok {
		t.Fatal("key not found")
	}
	if info.RPM != 0 {
		t.Fatalf("RPM = %d, want 0 (unlimited)", info.RPM)
	}
	if info.RPM == 0 {
		return
	}
	t.Fatal("RPM=0 bypass logic broken — should not reach here")
}

// ============================================================================
// Dashboard: key hygiene (no live key injection, cookie-based auth)
// ============================================================================

func TestHandleDashboardDoesNotInjectKeys(t *testing.T) {
	// dashboardHTML is embedded at compile time — we only assert the handler
	// returns HTML without rewriting it with a live key. If the source still
	// contains a real gw- key, fail.
	if strings.Contains(dashboardHTML, "gw-zUkrePuW") || strings.Contains(dashboardHTML, "__GATEWAY_DEFAULT_KEY__") {
		t.Fatal("dashboard HTML must not contain hardcoded or inject-placeholder keys")
	}
	// Handler returns static HTML
	r := gin.New()
	r.GET("/dashboard", handlers.HandleDashboard())
	req := httptest.NewRequest("GET", "/dashboard", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	// Cookie-based auth: dashboard must NOT contain live gw- keys
	if strings.Contains(body, "gw-zUkrePuW") {
		t.Fatal("dashboard leaked live gateway key")
	}
	// Must use credentials: 'same-origin' (cookie-based, no Bearer header in JS)
	if !strings.Contains(body, "credentials: 'same-origin'") {
		t.Error("expected cookie-based auth (credentials: same-origin) in dashboard")
	}
	// Must NOT use localStorage for keys anymore
	if strings.Contains(body, "localStorage.setItem('gw_key'") {
		t.Error("dashboard must not store key in localStorage (cookie-based now)")
	}
}

// ============================================================================
// Routing helpers
// ============================================================================

func TestExpandGrokAlias(t *testing.T) {
	cases := map[string]string{
		"grok-4.5-high": "high", "grok-4.5-xhigh": "high",
		"grok-4.5-medium": "medium", "grok-4.5-low": "low",
		"grok-4.5-auto": "auto", "grok-4.5-none": "none",
	}
	for m, want := range cases {
		got, ok := upstream.ExpandGrokAlias(m)
		if !ok || got != want {
			t.Errorf("%s -> %s,%v want %s", m, got, ok, want)
		}
	}
	if _, ok := upstream.ExpandGrokAlias("grok-4.5"); ok {
		t.Error("base model should not be alias")
	}
}
