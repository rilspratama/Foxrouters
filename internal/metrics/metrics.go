// Package metrics registers and exposes Prometheus metrics for FoxRouters.
//
// Exposed at /metrics (public, no auth) so Prometheus scrapers can consume
// them. Metrics are instrumented from:
//   - proxy handlers (proxyGrok, proxyCodeBuddy) → RequestsTotal + duration
//   - pool managers (grok_account.go, codebuddy.go) → ActiveKeys/DisabledKeys gauges
//   - circuit breaker (health.go) → CircuitState
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// RequestsTotal counts proxied requests bucketed by upstream + response
	// status class. Status is a 3-digit HTTP code as a string (e.g. "200", "429").
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "foxrouters_requests_total",
		Help: "Total proxied requests by upstream and status code",
	}, []string{"upstream", "status"})

	// RequestDuration observes end-to-end proxy latency in seconds.
	RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "foxrouters_request_duration_seconds",
		Help:    "Proxy request duration (seconds) by upstream",
		Buckets: prometheus.DefBuckets,
	}, []string{"upstream"})

	// ActiveKeys tracks the number of usable keys/accounts by pool type.
	// Type: "grok" (accounts), "codebuddy" (API keys), "auth" (gateway keys).
	ActiveKeys = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "foxrouters_active_keys",
		Help: "Number of currently active (usable) keys/accounts by pool",
	}, []string{"type"})

	// DisabledKeys tracks disabled/cooldown entries by pool type.
	DisabledKeys = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "foxrouters_disabled_keys",
		Help: "Number of disabled/cooldown keys/accounts by pool",
	}, []string{"type"})

	// CircuitState reports the current circuit-breaker state per upstream.
	// 0 = closed (healthy), 1 = open (blocked), 2 = half-open (probing).
	CircuitState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "foxrouters_circuit_state",
		Help: "Circuit breaker state per upstream (0=closed, 1=open, 2=half-open)",
	}, []string{"upstream"})
)

// SetCircuitState sets the gauge for a given upstream.
// Accepts a raw int (0=closed, 1=open, 2=half-open) to avoid a dependency on
// the caller's CircuitState enum type.
func SetCircuitState(upstream string, state int) {
	CircuitState.WithLabelValues(upstream).Set(float64(state))
}

// SetPoolGauges is a convenience helper that sets the Active/Disabled gauges
// for one pool `type` in a single call.
func SetPoolGauges(poolType string, active, disabled int) {
	ActiveKeys.WithLabelValues(poolType).Set(float64(active))
	DisabledKeys.WithLabelValues(poolType).Set(float64(disabled))
}
