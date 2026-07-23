// upstream_adapter.go — bridges package-main call sites to internal/upstream.
// Domain files (handlers.go, proxy.go, main.go, metrics_pool.go, tests) still
// use the short names GrokAccount, GrokAccountManager, CBKey, CBKeyManager,
// HealthChecker, etc. — those names now alias into internal/upstream.
package main

import (
	"time"

	"foxrouters/internal/upstream"
)

// Type aliases
type GrokAccount = upstream.GrokAccount
type GrokAccountManager = upstream.GrokAccountManager
type CBKey = upstream.CBKey
type CBKeyManager = upstream.CBKeyManager
type UpstreamHealth = upstream.UpstreamHealth
type HealthChecker = upstream.HealthChecker
type CircuitState = upstream.CircuitState

// Constants re-exported (kept in main for legacy handlers.go / main.go references).
const (
	CircuitClosed   = upstream.CircuitClosed
	CircuitOpen     = upstream.CircuitOpen
	CircuitHalfOpen = upstream.CircuitHalfOpen

	CB_CREDIT_LIMIT  = upstream.CB_CREDIT_LIMIT
	MAX_REQUEST_BODY = upstream.MAX_REQUEST_BODY

	// Health/circuit constants (used by tests + config)
	CB_OPEN_THRESHOLD = upstream.CB_OPEN_THRESHOLD
	CB_OPEN_DURATION  = upstream.CB_OPEN_DURATION

	// Grok/CB endpoint constants (referenced by tests + docs)
	XAI_UPSTREAM_URL = upstream.XAI_UPSTREAM_URL
	CB_UPSTREAM_URL  = upstream.CB_UPSTREAM_URL

	// CodeBuddy credential types
	CBAuthAPIKey = upstream.CBAuthAPIKey
	CBAuthOAuth  = upstream.CBAuthOAuth
)

// Function/worker re-exports
var (
	NewGrokAccountManager = upstream.NewGrokAccountManager
	NewCBKeyManager       = upstream.NewCBKeyManager

	autoRefreshWorker   = upstream.AutoRefreshWorker
	reenableWorker      = upstream.ReenableWorker
	reenableCBWorker    = upstream.ReenableCBWorker
	cbOAuthRefreshWorker = upstream.CBOAuthRefreshWorker
	cbCreditSyncWorker  = upstream.CBCreditSyncWorker

	isGrokModel     = upstream.IsGrokModel
	expandGrokAlias = upstream.ExpandGrokAlias
	proxyGrok       = upstream.ProxyGrok
	proxyCodeBuddy  = upstream.ProxyCodeBuddy
	healthStatusOK  = upstream.HealthStatusOK

	truncateLog = upstream.TruncateLog
)

// newHealthChecker wraps the exported constructor with the old lower-case name.
func newHealthChecker(am *GrokAccountManager, km *CBKeyManager) *HealthChecker {
	return upstream.NewHealthChecker(am, km)
}

// newUpstreamHealth alias for tests.
func newUpstreamHealth(name string) *UpstreamHealth {
	return upstream.NewUpstreamHealth(name)
}

// Test constructors + options (re-exported for whitebox tests in package main)
var (
	NewGrokAccountForTest   = upstream.NewGrokAccountForTest
	NewCBKeyForTest         = upstream.NewCBKeyForTest
	WithExpiresAt           = upstream.WithExpiresAt
	WithDisabledCooldown    = upstream.WithDisabledCooldown
	WithCBDisabledCooldown  = upstream.WithCBDisabledCooldown
)

// silence unused import warnings if these package-level references get pruned
var _ = time.Second
