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
	"foxrouters/internal/handlers"
)

// Aliases: handler functions keep their old lowercase names in package main.
var (
	handleHealthMinimal    = handlers.HandleHealthMinimal
	handleHealth           = handlers.HandleHealth
	handleAccounts         = handlers.HandleAccounts
	handleRefresh          = handlers.HandleRefresh
	handleImportCBKey      = handlers.HandleImportCBKey
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
	handleLogin            = handlers.HandleLogin
	handleLogout           = handlers.HandleLogout
	handleMessages         = handlers.HandleMessages
	anthropicAuthMiddleware = handlers.AnthropicAuthMiddleware

	// v1.3.0 — custom models + aliases
	handleListCustomModels  = handlers.HandleListCustomModels
	handleAddCustomModel    = handlers.HandleAddCustomModel
	handleDeleteCustomModel = handlers.HandleDeleteCustomModel
	handleListAliases       = handlers.HandleListAliases
	handleAddAlias          = handlers.HandleAddAlias
	handleDeleteAlias       = handlers.HandleDeleteAlias
)
