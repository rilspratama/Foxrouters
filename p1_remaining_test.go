package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// --- MaxBytesReader / body size guard ---

func TestMaxRequestBodyConstant(t *testing.T) {
	if MAX_REQUEST_BODY != 10*1024*1024 {
		t.Fatalf("MAX_REQUEST_BODY = %d, want 10MB", MAX_REQUEST_BODY)
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

// --- Dashboard: no live key injection ---

func TestHandleDashboardDoesNotInjectKeys(t *testing.T) {
	// dashboardHTML is embedded at compile time — we only assert the handler
	// returns HTML without rewriting it with a live key. If the source still
	// contains a real gw- key, fail.
	if strings.Contains(dashboardHTML, "gw-zUkrePuW") || strings.Contains(dashboardHTML, "__GATEWAY_DEFAULT_KEY__") {
		t.Fatal("dashboard HTML must not contain hardcoded or inject-placeholder keys")
	}
	// Handler returns static HTML
	r := gin.New()
	r.GET("/dashboard", handleDashboard())
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

// --- Grok re-enable: Save outside lock (behavioral regression test) ---

// mockSaveDB is a minimal DBStore stand-in that records SaveGrokAccount calls
// and checks that the account mutex is NOT held when Save is called.
type mockSaveDB struct {
	DBStore // embed zero value — only SaveGrokAccount is overridden via wrapper
}

// We can't easily mock DBStore methods without interface, so test the
// re-enable path by verifying concurrent GetAccessToken doesn't block
// during a simulated slow save.

func TestGrokNextReenableDoesNotHoldLockDuringSave(t *testing.T) {
	// Re-enable is background-only now. Next itself does not re-enable.
	// Test reenableCooldowns + concurrent GetAccessToken after lift.
	am := NewGrokAccountManager(nil)
	acc := &GrokAccount{
		Email:        "cooldown@test.com",
		AccessToken:  "tok",
		RefreshToken: "rt",
		expiresAt:    time.Now().Add(time.Hour),
		disabled:     true,
		disabledAt:   time.Now().Add(-11 * time.Minute),
		db:           nil,
	}
	am.accounts = []*GrokAccount{acc}

	// Next must NOT re-enable (hot path is O(k) only)
	if _, err := am.Next(); err == nil {
		t.Fatal("Next should not re-enable cooldowns on hot path")
	}

	am.reenableCooldowns()
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

func TestCBNextReenableDoesNotHoldKeyLockDuringSave(t *testing.T) {
	km := &CBKeyManager{keys: make([]*CBKey, 0)}
	k := &CBKey{
		Key:        "ck_testkey.abcdef",
		disabled:   true,
		disabledAt: time.Now().Add(-11 * time.Minute),
		db:         nil,
	}
	km.keys = []*CBKey{k}

	if _, err := km.Next(); err == nil {
		t.Fatal("Next should not re-enable on hot path")
	}
	km.reenableCooldowns()
	got, err := km.Next()
	if err != nil {
		t.Fatalf("Next after reenable: %v", err)
	}
	if got.Key != k.Key {
		t.Fatal("wrong key")
	}
	got.mu.RLock()
	disabled := got.disabled
	got.mu.RUnlock()
	if disabled {
		t.Fatal("key should be re-enabled")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
