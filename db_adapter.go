// db_adapter.go — thin bridge from the old package-main names to
// internal/db.  Domain files (auth, grok_account, codebuddy, handlers,
// proxy) still refer to *DBStore / RequestLog / etc; those names now
// alias into the extracted internal/db package.
//
// This file also holds the DTO conversion helpers that let domain code
// call save/load methods with rich types while the persistence layer
// speaks in plain DTOs.
package main

import (
	"strings"
	"time"

	"foxrouters/internal/db"
)

// Type aliases keep the existing call sites (a.db.Save*, RequestLog{...},
// etc.) compiling without touching each file individually.
type DBStore = db.Store
type RequestLog = db.RequestLog
type RefreshLog = db.RefreshLog
type AccountEvent = db.AccountEvent
type RequestStats = db.RequestStats
type ModelStats = db.ModelStats
type RecentRequest = db.RecentRequest
type RequestDetail = db.RequestDetail

// Re-exported constants some domain files still reference by short name.
const (
	RK_GROK_ACCOUNT = db.RK_GROK_ACCOUNT
	RK_CB_KEY       = db.RK_CB_KEY
	RK_GATEWAY_KEY  = db.RK_GATEWAY_KEY
	RK_RATE_LIMIT   = db.RK_RATE_LIMIT
)

// NewDBStore is a thin wrapper preserving the old constructor name.
func NewDBStore() (*DBStore, error) { return db.NewStore() }

// ---------------------------------------------------------------------------
// DTO ↔ domain conversion helpers.
// ---------------------------------------------------------------------------

// dbSaveGrokAccount converts a GrokAccount to a db.GrokAccountDTO and persists.
func dbSaveGrokAccount(s *DBStore, acc *GrokAccount) {
	if s == nil || acc == nil {
		return
	}
	s.SaveGrokAccount(db.GrokAccountDTO{
		Email:        acc.Email,
		AccessToken:  acc.AccessToken,
		RefreshToken: acc.RefreshToken,
		IDToken:      acc.IDToken,
		ExpiresAt:    acc.expiresAt,
		ExpiresIn:    acc.ExpiresIn,
		Expired:      acc.Expired,
		LastRefresh:  acc.LastRefresh,
		Sub:          acc.Sub,
		Disabled:     acc.disabled,
		DisabledAt:   acc.disabledAt,
	})
}

// dbSaveCBKey converts CB pool state to a db.CBKeyDTO and persists.
func dbSaveCBKey(s *DBStore, key string, creditsUsed float64, totalReqs int64, disabled bool, disabledAt time.Time) {
	if s == nil {
		return
	}
	s.SaveCBKey(db.CBKeyDTO{
		Key:         key,
		CreditsUsed: creditsUsed,
		TotalReqs:   totalReqs,
		Disabled:    disabled,
		DisabledAt:  disabledAt,
	})
}

// dbSaveGatewayKey / dbLoadGatewayKeys used to live here as the DTO
// conversion bridge for auth. That bridge now lives inside internal/auth
// itself (auth owns its own persistence, GatewayKeyInfo is its type).

// UpsertGrokAccount/UpsertCBKey were no-op sinks kept for API compatibility
// with earlier callers. Preserve the same no-op behavior in this package
// so we don't have to touch each callsite.
func dbUpsertGrokAccount(s *DBStore, acc *GrokAccount) { _ = s; _ = acc }
func dbUpsertCBKey(s *DBStore, key string, creditsUsed float64, totalReqs int64, disabled bool) {
	_ = s
	_ = key
	_ = creditsUsed
	_ = totalReqs
	_ = disabled
}

// Silence unused-import when strings/time aren't otherwise touched at build time.
var _ = strings.Join
var _ = time.Unix

