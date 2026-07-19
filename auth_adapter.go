// auth_adapter.go — bridges package-main call sites to internal/auth.
// Domain files (main, handlers, proxy, grok_account, codebuddy, metrics_pool,
// tests) still refer to AuthManager / GatewayKeyInfo / KeyRole / etc; those
// names now alias into the extracted internal/auth package.
package main

import (
	"foxrouters/internal/auth"
)

// Type aliases keep existing call sites compiling.
type AuthManager = auth.Manager
type GatewayKeyInfo = auth.GatewayKeyInfo
type KeyRole = auth.KeyRole
type SessionStore = auth.SessionStore

const (
	RoleInference = auth.RoleInference
	RoleAdmin     = auth.RoleAdmin
)

// Function/middleware re-exports.
var (
	AuthMiddleware  = auth.AuthMiddleware
	AdminMiddleware = auth.AdminMiddleware
)

// newAuthManager preserves the old lowercase constructor name.
func newAuthManager(s *DBStore) *AuthManager { return auth.NewManager(s) }

// NewSessionStore bridges to auth.NewSessionStore (P3-3).
func NewSessionStore() *SessionStore { return auth.NewSessionStore() }

// NewAuthManagerForTest returns an empty in-memory Manager (no db) with the
// provided pre-seeded keys. Package-main tests use this to avoid reaching
// into unexported fields.
func NewAuthManagerForTest(keys map[string]*GatewayKeyInfo) *AuthManager {
	return auth.NewManagerForTest(keys)
}

// generateGatewayKey / generateRandomKey / maskKey stay lowercase in
// package main to avoid renaming every caller. They forward to the
// exported names in internal/auth.
func generateGatewayKey() string        { return auth.GenerateGatewayKey() }
func generateRandomKey(n int) string    { return auth.GenerateRandomKey(n) }
func maskKey(k string) string           { return auth.MaskKey(k) }
