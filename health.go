package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// healthStatusOK reports whether an upstream health-probe HTTP status is healthy.
// Only 2xx/3xx count. 401/403 (auth/ban/gate) and 4xx/5xx are unhealthy —
// the old `status < 500` rule treated 403 Access Denied as healthy.
func healthStatusOK(code int) bool {
	return code >= 200 && code < 400
}

// CircuitState represents circuit breaker state.
type CircuitState int

const (
	CircuitClosed   CircuitState = iota // healthy, requests flow normally
	CircuitOpen                         // unhealthy, requests rejected immediately
	CircuitHalfOpen                     // testing: limited probes allowed
)

func (s CircuitState) String() string {
	switch s {
	case CircuitClosed:
		return "closed"
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// UpstreamHealth tracks health state for one upstream (grok or codebuddy).
type UpstreamHealth struct {
	Name string

	// Circuit breaker
	mu              sync.Mutex
	state           CircuitState
	consecutiveErrs int
	openedAt        time.Time
	halfOpenProbes  int

	// Passive stats (atomic for lock-free reads)
	totalRequests  atomic.Int64
	totalErrors    atomic.Int64
	totalLatencyMs atomic.Int64 // cumulative latency for avg calc
	lastRequestAt  atomic.Int64 // unix nano
	lastErrorAt    atomic.Int64 // unix nano
	lastErrorMsg   atomic.Value // string

	// Active health check
	lastCheckAt    time.Time
	lastCheckLatMs int64
	lastCheckOK    bool
}

func newUpstreamHealth(name string) *UpstreamHealth {
	return &UpstreamHealth{Name: name, state: CircuitClosed}
}

// RecordRequest tracks a request attempt (passive health).
func (h *UpstreamHealth) RecordRequest(latency time.Duration, err error) {
	h.totalRequests.Add(1)
	h.totalLatencyMs.Add(latency.Milliseconds())
	h.lastRequestAt.Store(time.Now().UnixNano())

	if err != nil {
		h.totalErrors.Add(1)
		h.lastErrorAt.Store(time.Now().UnixNano())
		h.lastErrorMsg.Store(err.Error())
		h.mu.Lock()
		h.consecutiveErrs++
		if h.consecutiveErrs >= CB_OPEN_THRESHOLD && h.state == CircuitClosed {
			h.state = CircuitOpen
			h.openedAt = time.Now()
			log.Printf("[health] %s circuit OPENED (%d consecutive errors)", h.Name, h.consecutiveErrs)
		}
		h.mu.Unlock()
	} else {
		h.mu.Lock()
		if h.state == CircuitHalfOpen {
			// Successful probe → close circuit
			h.state = CircuitClosed
			h.consecutiveErrs = 0
			h.halfOpenProbes = 0
			log.Printf("[health] %s circuit CLOSED (probe succeeded)", h.Name)
		} else if h.state == CircuitClosed {
			h.consecutiveErrs = 0
		}
		h.mu.Unlock()
	}
}

// CanRequest checks if a request should be allowed through (circuit breaker).
// Returns true if allowed, false if circuit is open.
func (h *UpstreamHealth) CanRequest() bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	switch h.state {
	case CircuitClosed:
		return true
	case CircuitOpen:
		// Check if cooldown elapsed → transition to half-open
		if time.Since(h.openedAt) >= CB_OPEN_DURATION {
			h.state = CircuitHalfOpen
			h.halfOpenProbes = 0
			log.Printf("[health] %s circuit HALF-OPEN (testing)", h.Name)
			return true
		}
		return false
	case CircuitHalfOpen:
		// Allow limited probes
		if h.halfOpenProbes < CB_HALF_OPEN_PROBES {
			h.halfOpenProbes++
			return true
		}
		return false
	default:
		return true
	}
}

// Stats returns a snapshot of health metrics.
func (h *UpstreamHealth) Stats() gin.H {
	h.mu.Lock()
	state := h.state
	consecErrs := h.consecutiveErrs
	openedAt := h.openedAt
	h.mu.Unlock()

	totalReq := h.totalRequests.Load()
	totalErr := h.totalErrors.Load()
	latencyMs := h.totalLatencyMs.Load()
	var avgLatencyMs float64
	if totalReq > 0 {
		avgLatencyMs = float64(latencyMs) / float64(totalReq)
	}

	var errRate float64
	if totalReq > 0 {
		errRate = float64(totalErr) / float64(totalReq) * 100
	}

	lastReq := time.Unix(0, h.lastRequestAt.Load())
	lastErr := time.Unix(0, h.lastErrorAt.Load())
	lastErrMsg, _ := h.lastErrorMsg.Load().(string)

	return gin.H{
		"name":             h.Name,
		"circuit_state":    state.String(),
		"consecutive_errs": consecErrs,
		"total_requests":   totalReq,
		"total_errors":     totalErr,
		"error_rate_pct":   round2(errRate),
		"avg_latency_ms":   round2(avgLatencyMs),
		"last_request_at":  formatTime(lastReq),
		"last_error_at":    formatTime(lastErr),
		"last_error_msg":   lastErrMsg,
		"last_check_at":    formatTime(h.lastCheckAt),
		"last_check_ok":    h.lastCheckOK,
		"last_check_ms":    h.lastCheckLatMs,
		"opened_at":        formatTime(openedAt),
	}
}

// HealthChecker manages active health checks for all upstreams.
type HealthChecker struct {
	grok   *UpstreamHealth
	cb     *UpstreamHealth
	grokAM *GrokAccountManager
	cbKM   *CBKeyManager
}

func newHealthChecker(grokAM *GrokAccountManager, cbKM *CBKeyManager) *HealthChecker {
	return &HealthChecker{
		grok:   newUpstreamHealth("grok"),
		cb:     newUpstreamHealth("codebuddy"),
		grokAM: grokAM,
		cbKM:   cbKM,
	}
}

// Start launches periodic active health checks.
func (hc *HealthChecker) Start() {
	go hc.grokCheckLoop()
	go hc.cbCheckLoop()
	log.Printf("[health] active health checker started (interval=%s)", HEALTH_CHECK_INTERVAL)
}

func (hc *HealthChecker) grokCheckLoop() {
	hc.checkGrok()
	ticker := time.NewTicker(HEALTH_CHECK_INTERVAL)
	defer ticker.Stop()
	for range ticker.C {
		hc.checkGrok()
	}
}

func (hc *HealthChecker) cbCheckLoop() {
	hc.checkCB()
	ticker := time.NewTicker(HEALTH_CHECK_INTERVAL)
	defer ticker.Stop()
	for range ticker.C {
		hc.checkCB()
	}
}

func (hc *HealthChecker) checkGrok() {
	h := hc.grok
	start := time.Now()

	// LLM test: send minimal chat request via first available Grok account
	accounts := hc.grokAM.GetAll()
	if len(accounts) == 0 {
		h.mu.Lock()
		h.lastCheckAt = time.Now()
		h.lastCheckOK = false
		h.lastCheckLatMs = 0
		h.lastErrorMsg.Store("no grok accounts loaded")
		h.consecutiveErrs++
		if h.consecutiveErrs >= CB_OPEN_THRESHOLD && h.state == CircuitClosed {
			h.state = CircuitOpen
			h.openedAt = time.Now()
			log.Printf("[health] grok circuit OPENED (no accounts)")
		}
		h.mu.Unlock()
		return
	}

	// Pick first non-disabled account. Do NOT fall back to a banned account —
	// that would probe with a known-bad token and misclassify 403 as "upstream OK".
	var acc *GrokAccount
	for _, a := range accounts {
		if !a.IsDisabled() {
			acc = a
			break
		}
	}
	if acc == nil {
		h.mu.Lock()
		h.lastCheckAt = time.Now()
		h.lastCheckOK = false
		h.lastCheckLatMs = 0
		h.lastErrorMsg.Store("all grok accounts disabled")
		h.mu.Unlock()
		return
	}

	// Build minimal chat request
	body := `{"model":"grok-4.5","messages":[{"role":"user","content":"Hi"}],"stream":false,"max_tokens":5}`
	req, _ := http.NewRequest("POST", XAI_UPSTREAM_URL+"/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+acc.GetAccessToken())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-grok-client-version", GROK_CLIENT_VERSION)
	req.Header.Set("x-grok-client-identifier", "grok-shell")
	req.Header.Set("User-Agent", "grok-shell/"+GROK_CLIENT_VERSION)

	client := healthCheckClient
	resp, err := client.Do(req)
	latency := time.Since(start)

	h.mu.Lock()
	h.lastCheckAt = time.Now()
	h.lastCheckLatMs = latency.Milliseconds()
	if err != nil {
		h.lastCheckOK = false
		h.lastErrorMsg.Store(err.Error())
		h.consecutiveErrs++
		if h.consecutiveErrs >= CB_OPEN_THRESHOLD && h.state == CircuitClosed {
			h.state = CircuitOpen
			h.openedAt = time.Now()
			log.Printf("[health] grok circuit OPENED (LLM test failed: %v)", err)
		}
	} else {
		resp.Body.Close()
		// 2xx/3xx only. 401/403 (auth/ban/gate) are unhealthy — previously
		// status < 500 treated 403 Access Denied as healthy (false OK).
		h.lastCheckOK = healthStatusOK(resp.StatusCode)
		if h.lastCheckOK {
			h.lastErrorMsg.Store("")
			if h.state == CircuitHalfOpen {
				h.state = CircuitClosed
				h.consecutiveErrs = 0
				log.Printf("[health] grok circuit CLOSED (LLM test OK, %dms)", latency.Milliseconds())
			} else if h.state == CircuitClosed {
				h.consecutiveErrs = 0
			}
		} else {
			h.lastErrorMsg.Store(fmt.Sprintf("LLM test status %d", resp.StatusCode))
			h.consecutiveErrs++
			if h.consecutiveErrs >= CB_OPEN_THRESHOLD && h.state == CircuitClosed {
				h.state = CircuitOpen
				h.openedAt = time.Now()
				log.Printf("[health] grok circuit OPENED (LLM test status %d)", resp.StatusCode)
			}
		}
	}
	h.mu.Unlock()
}

func (hc *HealthChecker) checkCB() {
	h := hc.cb
	start := time.Now()

	// LLM test: send minimal chat request via first available CB key
	keys := hc.cbKM.GetAll()
	if len(keys) == 0 {
		h.mu.Lock()
		h.lastCheckAt = time.Now()
		h.lastCheckOK = false
		h.lastCheckLatMs = 0
		h.lastErrorMsg.Store("no cb keys loaded")
		h.consecutiveErrs++
		if h.consecutiveErrs >= CB_OPEN_THRESHOLD && h.state == CircuitClosed {
			h.state = CircuitOpen
			h.openedAt = time.Now()
			log.Printf("[health] codebuddy circuit OPENED (no keys)")
		}
		h.mu.Unlock()
		return
	}

	// Pick first non-disabled key. Do NOT fall back to a disabled key.
	var key *CBKey
	for _, k := range keys {
		k.mu.Lock()
		d := k.disabled
		k.mu.Unlock()
		if !d {
			key = k
			break
		}
	}
	if key == nil {
		h.mu.Lock()
		h.lastCheckAt = time.Now()
		h.lastCheckOK = false
		h.lastCheckLatMs = 0
		h.lastErrorMsg.Store("all cb keys disabled")
		h.mu.Unlock()
		return
	}

	// Build minimal CB chat request (stream=true mandatory, system message mandatory)
	body := `{"model":"gpt-5.2","messages":[{"role":"system","content":"You are a helpful assistant."},{"role":"user","content":"Hi"}],"stream":true,"max_completion_tokens":5}`
	req, _ := http.NewRequest("POST", CB_UPSTREAM_URL, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key.Key)
	req.Header.Set("Content-Type", "application/json")

	client := healthCheckClient
	resp, err := client.Do(req)
	latency := time.Since(start)

	h.mu.Lock()
	h.lastCheckAt = time.Now()
	h.lastCheckLatMs = latency.Milliseconds()
	if err != nil {
		h.lastCheckOK = false
		h.lastErrorMsg.Store(err.Error())
		h.consecutiveErrs++
		if h.consecutiveErrs >= CB_OPEN_THRESHOLD && h.state == CircuitClosed {
			h.state = CircuitOpen
			h.openedAt = time.Now()
			log.Printf("[health] codebuddy circuit OPENED (LLM test failed: %v)", err)
		}
	} else {
		resp.Body.Close()
		// 2xx/3xx only — 401/403/429 are unhealthy
		h.lastCheckOK = healthStatusOK(resp.StatusCode)
		if h.lastCheckOK {
			h.lastErrorMsg.Store("")
			if h.state == CircuitHalfOpen {
				h.state = CircuitClosed
				h.consecutiveErrs = 0
				log.Printf("[health] codebuddy circuit CLOSED (LLM test OK, %dms)", latency.Milliseconds())
			} else if h.state == CircuitClosed {
				h.consecutiveErrs = 0
			}
		} else {
			h.lastErrorMsg.Store(fmt.Sprintf("LLM test status %d", resp.StatusCode))
			h.consecutiveErrs++
			if h.consecutiveErrs >= CB_OPEN_THRESHOLD && h.state == CircuitClosed {
				h.state = CircuitOpen
				h.openedAt = time.Now()
				log.Printf("[health] codebuddy circuit OPENED (LLM test status %d)", resp.StatusCode)
			}
		}
	}
	h.mu.Unlock()
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
