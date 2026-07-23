// handlers_adapter.go — thin bridge from package-main call sites (main.go)
// to internal/handlers. main.go still writes route handlers as
// handleHealth(...), handleDashboard(...), etc — those names forward to the
// extracted package.
//
// The dashboardHTML test in p1_remaining_test.go also uses the lowercase
// alias here (both dashboardHTML and handleDashboard live in package main
// for the test binary).
package main

import (
	"foxrouters/internal/auth"
	"foxrouters/internal/handlers"
	"foxrouters/internal/upstream"

	"github.com/gin-gonic/gin"
)

// Aliases: handler functions keep their old lowercase names in package main.
var (
	handleHealthMinimal    = handlers.HandleHealthMinimal
	handleAccounts         = handlers.HandleAccounts
	handleRefresh          = handlers.HandleRefresh
	handleImportCBKey      = handlers.HandleImportCBKey
	handleImportCBOAuth    = handlers.HandleImportCBOAuth
	handleImportAccount    = handlers.HandleImportAccount
	handleImportCBKeyBulk  = handlers.HandleImportCBKeyBulk
	handleImportAccountBulk = handlers.HandleImportAccountBulk
	handleDeleteAccount    = handlers.HandleDeleteAccount
	handleDeleteCBKey      = handlers.HandleDeleteCBKey
	handleCleanupDisabled  = handlers.HandleCleanupDisabled
	handleCleanupBanned    = handlers.HandleCleanupBanned
	handleHistory          = handlers.HandleHistory
	handleRecentRequests   = handlers.HandleRecentRequests
	handleHistoryDetail    = handlers.HandleHistoryDetail
	handleListKeys         = handlers.HandleListKeys
	handleCreateKey        = handlers.HandleCreateKey
	handleDeleteKey        = handlers.HandleDeleteKey
	handleUpdateKey        = handlers.HandleUpdateKey
	handleKeyUsage         = handlers.HandleKeyUsage
	handleDashboard        = handlers.HandleDashboard
	handleMessages         = handlers.HandleMessages
	anthropicAuthMiddleware = handlers.AnthropicAuthMiddleware

	// v1.3.0 — custom models + aliases
	handleListCustomModels  = handlers.HandleListCustomModels
	handleAddCustomModel    = handlers.HandleAddCustomModel
	handleDeleteCustomModel = handlers.HandleDeleteCustomModel
	handleListAliases       = handlers.HandleListAliases
	handleAddAlias          = handlers.HandleAddAlias
	handleDeleteAlias       = handlers.HandleDeleteAlias

	// v1.4.0 — combos
	handleListCombos  = handlers.HandleListCombos
	handleGetCombo    = handlers.HandleGetCombo
	handleAddCombo    = handlers.HandleAddCombo
	handleDeleteCombo = handlers.HandleDeleteCombo

	// v1.5.0 — proxy pool
	handleListProxies  = handlers.HandleListProxies
	handleAddProxy     = handlers.HandleAddProxy
	handleUpdateProxy  = handlers.HandleUpdateProxy
	handleDeleteProxy  = handlers.HandleDeleteProxy
	handleToggleProxy  = handlers.HandleToggleProxy
	handleTestProxy    = handlers.HandleTestProxy

	// v1.6.0 — Cloudflare Tunnel (first-class Go feature)
	handleTunnelStatus  = handlers.HandleTunnelStatus
	handleTunnelEnable  = handlers.HandleTunnelEnable
	handleTunnelDisable = handlers.HandleTunnelDisable
	handleTunnelRestart = handlers.HandleTunnelRestart
)

// Function wrappers for handlers whose signature changed to accept
// the session store (P3-3). Var aliases don't work because the
// function type no longer matches the package-main call site.
func handleHealth(grokAM *upstream.GrokAccountManager, cbKM *upstream.CBKeyManager, hc *upstream.HealthChecker, am *auth.Manager, sessions *auth.SessionStore) gin.HandlerFunc {
	return handlers.HandleHealth(grokAM, cbKM, hc, am, sessions)
}
func handleLogin(am *auth.Manager, sessions *auth.SessionStore) gin.HandlerFunc {
	return handlers.HandleLogin(am, sessions)
}
func handleLogout(sessions *auth.SessionStore) gin.HandlerFunc {
	return handlers.HandleLogout(sessions)
}
