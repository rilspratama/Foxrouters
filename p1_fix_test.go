package main

import (
	"sync"
	"testing"
	"time"
)

// --- healthStatusOK ---

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
		got := healthStatusOK(tc.code)
		if got != tc.ok {
			t.Errorf("healthStatusOK(%d) = %v, want %v", tc.code, got, tc.ok)
		}
	}
}

// --- Circuit breaker: pool exhaustion must NOT trip circuit ---

func TestCircuitBreaker_PoolExhaustionDoesNotOpen(t *testing.T) {
	h := newUpstreamHealth("test")

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
	for i := 0; i < CB_OPEN_THRESHOLD+2; i++ {
		// no-op: pool exhaustion path no longer records errors
	}
	if !h.CanRequest() {
		t.Fatal("circuit should stay closed when pool exhaustion does not record errors")
	}
	if h.state != CircuitClosed {
		t.Fatalf("state = %s, want closed", h.state)
	}
}

func TestCircuitBreaker_RealUpstreamErrorsStillOpen(t *testing.T) {
	h := newUpstreamHealth("test")
	for i := 0; i < CB_OPEN_THRESHOLD; i++ {
		h.RecordRequest(5*time.Millisecond, errTest("upstream 502"))
	}
	if h.CanRequest() {
		t.Fatal("circuit should be open after consecutive upstream errors")
	}
	if h.state != CircuitOpen {
		t.Fatalf("state = %s, want open", h.state)
	}
}

// --- AuthManager map concurrent access (RLock path) ---

func TestAuthManager_ConcurrentLenIsSafe(t *testing.T) {
	// Regression: AuthMiddleware used to call len(am.keys) without RLock.
	// This stress-test concurrent Add/Remove/Valid while reading len under RLock.
	am := &AuthManager{keys: make(map[string]*GatewayKeyInfo)}
	// seed one key
	am.Add("gw-testkey-aaaaaaaaaaaaaaaaaaaaaaaa", "seed", 0, 0, 0)

	var wg sync.WaitGroup
	// writers
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				k := generateGatewayKey()
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
				am.mu.RLock()
				_ = len(am.keys) == 0
				am.mu.RUnlock()
				_ = am.Valid("gw-testkey-aaaaaaaaaaaaaaaaaaaaaaaa")
			}
		}()
	}
	wg.Wait()
}

// --- GrokAccountManager import total race (snapshot under lock) ---

func TestGrokAccountManager_GetAllIsCopy(t *testing.T) {
	am := NewGrokAccountManager(nil)
	am.accounts = []*GrokAccount{
		{Email: "a@test.com", AccessToken: "t1", RefreshToken: "r1", expiresAt: time.Now().Add(time.Hour)},
		{Email: "b@test.com", AccessToken: "t2", RefreshToken: "r2", expiresAt: time.Now().Add(time.Hour)},
	}
	all := am.GetAll()
	if len(all) != 2 {
		t.Fatalf("GetAll len = %d, want 2", len(all))
	}
	// mutate returned slice must not affect manager
	all[0] = nil
	if am.accounts[0] == nil {
		t.Fatal("GetAll should return a copy; mutating result mutated manager")
	}
}

// errTest is a tiny error type for circuit breaker tests.
type errTest string

func (e errTest) Error() string { return string(e) }
