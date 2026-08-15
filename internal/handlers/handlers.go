// Package handlers exposes the HTTP handlers wired by main.go into gin.
//
// External state that lives in main.go — Version, dashboardHTML — is
// injected via package-level setters (SetVersion, SetDashboardHTML) so this
// package stays free of the top-level embed / ldflags plumbing.
package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"foxrouters/internal/auth"
	"foxrouters/internal/db"
	"foxrouters/internal/upstream"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// INJECTED STATE (owned by main.go)
// ============================================================================

// version defaults to "dev" so tests + local runs don't segfault when main
// forgets to call SetVersion (they see the same fallback main.go uses).
var version = "dev"

// dashboardHTML is the //go:embed dashboard.html string owned by main.
// SetDashboardHTML is called once from main.main() before routes wire up.
var dashboardHTML = ""

// SetVersion overrides the reported gateway version (called from main).
func SetVersion(v string) { version = v }

// SetDashboardHTML injects the embedded dashboard SPA payload from main.
func SetDashboardHTML(s string) { dashboardHTML = s }

// ============================================================================
// ENDPOINTS
// ============================================================================

// HandleHealthMinimal serves a bare-bones liveness response for HEAD /health
// and for unauthenticated GET. Avoids the full telemetry path that may hang
// when the authed branch touches am.Get() + cbKM.GetAll() under load.
// Gin auto-handles HEAD by calling the GET handler, but some clients send
// HEAD without Accept-Encoding, causing the gzip middleware to wrap the
// writer and then never flush (no body written). This explicit handler
// short-circuits that path.
func HandleHealthMinimal() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status":  "healthy",
			"service": "foxrouters",
			"version": version,
		})
	}
}

// HandleHealth reports overall status + (when authed) per-upstream telemetry.
func HandleHealth(grokAM *upstream.GrokAccountManager, cbKM *upstream.CBKeyManager, fbAM *upstream.FreebuffAccountManager, aliAM *upstream.AlibabaKeyManager, hc *upstream.HealthChecker, am *auth.Manager, sessions *auth.SessionStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		grokStats := hc.Grok.Stats()
		cbStats := hc.CB.Stats()
		var fbStats map[string]any
		if hc.FB != nil {
			fbStats = hc.FB.Stats()
		}
		var aliStats map[string]any
		if hc.Ali != nil {
			aliStats = hc.Ali.Stats()
		}

		// Overall status: unhealthy if any circuit is open
		grokCircuit := grokStats["circuit_state"].(string)
		cbCircuit := cbStats["circuit_state"].(string)
		overall := "healthy"
		if grokCircuit == "open" || cbCircuit == "open" {
			overall = "degraded"
		}
		if fbStats != nil {
			if fbCircuit, ok := fbStats["circuit_state"].(string); ok && fbCircuit == "open" {
				overall = "degraded"
			}
		}
		if aliStats != nil {
			if aliCircuit, ok := aliStats["circuit_state"].(string); ok && aliCircuit == "open" {
				overall = "degraded"
			}
		}

		// Public response: minimal liveness only.
		// Detailed telemetry only if caller presents a valid gateway key
		// (via Bearer header OR session cookie — dashboard uses cookie).
		// When GATEWAY_AUTH_DISABLE=1, everyone gets full telemetry (dev mode).
		authed := false
		if os.Getenv("GATEWAY_AUTH_DISABLE") == "1" {
			authed = true
		} else if a := c.GetHeader("Authorization"); strings.HasPrefix(a, "Bearer ") {
			if _, ok := am.Get(a[7:]); ok {
				authed = true
			}
		} else if ck, err := c.Cookie("foxrouters_session"); err == nil && ck != "" {
			// P3-3: cookie is now a session token, resolve to API key.
			var key string
			if sessions != nil {
				key = sessions.Lookup(ck)
			} else {
				key = ck // legacy fallback (pre-P3-3)
			}
			if key != "" {
				if _, ok := am.Get(key); ok {
					authed = true
				}
			}
		}
		if !authed {
			c.JSON(200, gin.H{
				"status":  overall,
				"service": "foxrouters",
				"version": version,
			})
			return
		}

		// Authed: full telemetry
		c.JSON(200, gin.H{
			"status":  overall,
			"service": "foxrouters",
			"version": version,
			"mode":    "unified (grok + codebuddy + freebuff)",
			"upstreams": gin.H{
				"grok":      grokStats,
				"codebuddy": cbStats,
				"freebuff":  fbStats,
				"alibaba":   aliStats,
			},
			"grok_accounts": grokAM.Len(),
			"grok_active": func() int {
				active := 0
				for _, a := range grokAM.GetAll() {
					if s := a.Snapshot(); !s.Disabled {
						active++
					}
				}
				return active
			}(),
			"grok_tokens_used": func() int64 {
				var total int64
				for _, a := range grokAM.GetAll() {
					total += a.Snapshot().TokensUsed
				}
				return total
			}(),
			"grok_tokens_quota": func() int64 {
				return int64(upstream.GROK_FREE_TIER_QUOTA) * int64(grokAM.Len())
			}(),
			"cb_credits_used": func() float64 {
				var total float64
				for _, k := range cbKM.GetAll() {
					total += float64(k.Snapshot().CreditsUsed)
				}
				return total
			}(),
			"cb_credits_limit": func() float64 {
				var total float64
				for _, k := range cbKM.GetAll() {
					total += float64(k.Snapshot().CreditLimit)
				}
				return total
			}(),
			"cb_keys": cbKM.Len(),
			"cb_keys_active": func() int {
				active := 0
				for _, k := range cbKM.GetAll() {
					if _, _, disabled := k.Stats(); !disabled {
						active++
					}
				}
				return active
			}(),
			"fb_accounts":  fbAM.Len(),
			"ali_accounts": aliAM.Len(),
			"ali_keys_active": func() int {
				active := 0
				for _, k := range aliAM.ListAccounts() {
					if d, _ := k["disabled"].(bool); !d {
						active++
					}
				}
				return active
			}(),
			"fb_tier_full": func() int {
				count := 0
				for _, a := range fbAM.ListAccounts() {
					if a["tier"] == "full" {
						count++
					}
				}
				return count
			}(),
			"fb_tier_limited": func() int {
				count := 0
				for _, a := range fbAM.ListAccounts() {
					if a["tier"] == "limited" {
						count++
					}
				}
				return count
			}(),
			"fb_tier_blocked": func() int {
				count := 0
				for _, a := range fbAM.ListAccounts() {
					if a["tier"] == "blocked" {
						count++
					}
				}
				return count
			}(),
			"fb_quota_used":  func() float64 { t, _, _ := fbAM.QuotaSummary(); return t }(),
			"fb_quota_limit": func() float64 { _, t, _ := fbAM.QuotaSummary(); return t }(),
			"fb_quota_exhausted": func() int {
				count := 0
				for _, a := range fbAM.ListAccounts() {
					lr, _ := a["quota_limit"].(float64)
					rr, _ := a["quota_recent"].(float64)
					if lr > 0 && rr >= lr {
						count++
					}
				}
				return count
			}(),
			"time": time.Now().Format(time.RFC3339),
		})
	}
}

// HandleAccounts lists Grok accounts and CodeBuddy keys (admin only).
// Server-side pagination: ?upstream=grok|codebuddy&page=N&page_size=S.
// Omitting `upstream` returns both full lists (legacy behavior).
func HandleAccounts(grokAM *upstream.GrokAccountManager, cbKM *upstream.CBKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		up := strings.ToLower(strings.TrimSpace(c.Query("upstream")))
		page := ParsePage(c.Query("page"))
		pageSize := ParsePageSize(c.Query("page_size"))

		result := gin.H{
			"grok_total":         grokAM.Len(),
			"cb_total":           cbKM.Len(),
			"grok_selector_mode": string(upstream.GetGrokSelectorMode()),
			"cb_selector_mode":   string(upstream.GetSelectorMode()),
		}

		if up == "" || up == "grok" {
			grokAccs := grokAM.GetAll()
			start, end := PageRange(page, pageSize, len(grokAccs))
			grokResult := make([]gin.H, 0, end-start)
			for _, a := range grokAccs[start:end] {
				s := a.Snapshot()
				grokResult = append(grokResult, gin.H{
					"provider": "grok", "email": s.Email, "sub": s.Sub,
					"expires_at": s.Expired, "expires_in": s.ExpiresIn,
					"last_refresh": s.LastRefresh, "disabled": s.Disabled,
					"disabled_at": s.DisabledAt, "token_status": s.TokenStatus,
					"billing_synced_at": s.BillingSyncedAt,
					"period_end":        s.PeriodEnd,
					"period_type":       s.PeriodType,
					"on_demand_cap":     s.OnDemandCap,
					"on_demand_used":    s.OnDemandUsed,
					"prepaid_balance":   s.PrepaidBalance,
					"unified_billing":   s.UnifiedBilling,
					"tokens_used":       s.TokensUsed,
					"prompt_tokens":     s.PromptTokens,
					"completion_tokens": s.CompletionTokens,
					"usage_reset_at":    s.UsageResetAt,
					"quota":             upstream.GROK_FREE_TIER_QUOTA,
					"img_quota_used":    s.ImgQuotaUsed,
					"img_quota_limit":   s.ImgQuotaLimit,
					"vid_quota_used":    s.VidQuotaUsed,
					"vid_quota_limit":   s.VidQuotaLimit,
					"chat_quota_used":   s.ChatQuotaUsed,
					"chat_quota_limit":  s.ChatQuotaLimit,
					"quota_synced_at":   s.QuotaSyncedAt,
				})
			}
			result["grok"] = grokResult
			result["grok_page"] = page
			result["grok_page_size"] = pageSize
		}

		if up == "" || up == "codebuddy" {
			cbKeys := cbKM.GetAll()
			start, end := PageRange(page, pageSize, len(cbKeys))
			cbResult := make([]gin.H, 0, end-start)
			for _, k := range cbKeys[start:end] {
				s := k.Snapshot()
				remain := s.CreditsRemain
				if remain == 0 && s.MeterSyncedAt.IsZero() {
					// Never synced — derive from fallback limit
					remain = s.CreditLimit - s.CreditsUsed
				}
				entry := gin.H{
					"provider":       "codebuddy",
					"cred_type":      string(s.CredType),
					"disabled":       s.Disabled,
					"credits_used":   s.CreditsUsed,
					"credit_limit":   s.CreditLimit,
					"credits_remain": remain,
					"credits_left":   remain,
					"total_requests": s.TotalReqs,
					"package_name":   s.PackageName,
					"cycle_end":      s.CycleEnd,
					"meter_status":   s.MeterStatus,
				}
				if !s.MeterSyncedAt.IsZero() {
					entry["meter_synced_at"] = s.MeterSyncedAt.Format(time.RFC3339)
				}
				if s.CredType == upstream.CBAuthOAuth {
					entry["email"] = s.Email
					entry["key"] = s.Email
					if !s.ExpiresAt.IsZero() {
						entry["expires_at"] = s.ExpiresAt.Format(time.RFC3339)
					}
				} else {
					entry["key"] = s.Key[:8] + "..." + s.Key[len(s.Key)-4:]
				}
				cbResult = append(cbResult, entry)
			}
			result["codebuddy"] = cbResult
			result["cb_page"] = page
			result["cb_page_size"] = pageSize
		}

		c.JSON(200, result)
	}
}

// parsePage parses a 1-based page query param (default 1).
func ParsePage(v string) int {
	p, err := strconv.Atoi(v)
	if err != nil || p < 1 {
		return 1
	}
	return p
}

// parsePageSize parses page_size (default 50, capped at 200).
func ParsePageSize(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return n
}

// pageRange returns the half-open [start, end) slice bounds for a page.
func PageRange(page, pageSize, total int) (int, int) {
	if page < 1 {
		page = 1
	}
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return start, end
}

// HandleRefresh forces a refresh on every Grok account (admin only).
func HandleRefresh(grokAM *upstream.GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		accounts := grokAM.GetAll()
		results := make([]gin.H, 0)
		for _, a := range accounts {
			err := a.Refresh()
			status := "ok"
			if err != nil {
				// Sanitize: don't leak upstream OAuth/internal details to client
				slog.Warn("refresh failed", "module", "grok", "email", a.Email, "error", err)
				status = "refresh_failed"
			}
			results = append(results, gin.H{"email": a.Email, "status": status})
		}
		c.JSON(200, gin.H{"results": results})
	}
}

// HandleImportCBKey hot-imports a CodeBuddy API key into runtime pool + Redis.
// Body: {"api_key":"ck_..."} or {"key":"ck_..."}. Idempotent — existing keys return added=false.
func HandleImportCBKey(cbKM *upstream.CBKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			APIKey string `json:"api_key"`
			Key    string `json:"key"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON body"})
			return
		}
		apiKey := strings.TrimSpace(req.APIKey)
		if apiKey == "" {
			apiKey = strings.TrimSpace(req.Key)
		}
		if apiKey == "" {
			c.JSON(400, gin.H{"error": "api_key (or key) is required"})
			return
		}
		added, total := cbKM.AddKey(apiKey)
		display := apiKey
		if len(display) > 12 {
			display = display[:8] + "..." + display[len(display)-4:]
		}
		if added {
			slog.Info("imported key", "module", "cb", "key", display, "total", total)
		}
		c.JSON(200, gin.H{
			"added":  added,
			"total":  total,
			"key":    display,
			"status": map[bool]string{true: "imported", false: "already_exists"}[added],
		})
	}
}

// HandleImportCBOAuth hot-imports a CodeBuddy OAuth account.
// Body: {"email":"...","access_token":"...","refresh_token":"...","expires_in":31535929}
// If expires_in is missing, the JWT exp claim is decoded from access_token.
func HandleImportCBOAuth(cbKM *upstream.CBKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Email        string `json:"email"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int64  `json:"expires_in"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON body"})
			return
		}
		email := strings.TrimSpace(req.Email)
		at := strings.TrimSpace(req.AccessToken)
		rt := strings.TrimSpace(req.RefreshToken)
		if email == "" || at == "" || rt == "" {
			c.JSON(400, gin.H{"error": "email, access_token, refresh_token are required"})
			return
		}
		expiresAt := resolveCBOAuthExpiry(at, req.ExpiresIn)
		added, total := cbKM.AddOAuthAccount(email, at, rt, expiresAt)
		if added {
			slog.Info("imported oauth account", "module", "cb", "email", email, "total", total)
		} else {
			slog.Info("updated oauth account", "module", "cb", "email", email, "total", total)
		}
		c.JSON(200, gin.H{
			"added":      added,
			"total":      total,
			"email":      email,
			"expires_at": expiresAt.Format(time.RFC3339),
			"status":     map[bool]string{true: "imported", false: "updated"}[added],
		})
	}
}

// resolveCBOAuthExpiry picks ExpiresAt from expires_in seconds, JWT exp, or 365d fallback.
func resolveCBOAuthExpiry(accessToken string, expiresIn int64) time.Time {
	if expiresIn > 0 {
		return time.Now().Add(time.Duration(expiresIn) * time.Second)
	}
	if exp := upstream.ParseJWTExp(accessToken); !exp.IsZero() {
		return exp
	}
	return time.Now().Add(365 * 24 * time.Hour)
}

// HandleCBOAuthDeviceStart kicks off a CodeBuddy OAuth device/login flow.
// Body (optional): {"platform":"CLI"}. Returns {state, auth_url}.
func HandleCBOAuthDeviceStart() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Platform string `json:"platform"`
		}
		// Body is optional; ignore bind errors for empty bodies.
		_ = c.ShouldBindJSON(&req)
		res, err := upstream.StartDeviceAuth(req.Platform)
		if err != nil {
			slog.Warn("cb oauth device start failed", "module", "cb-oauth-device", "error", err)
			c.JSON(502, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{
			"state":    res.State,
			"auth_url": res.AuthURL,
		})
	}
}

// HandleCBOAuthDevicePoll polls CodeBuddy for tokens after the user completes
// browser login. Query: ?state=...
// Returns {status:"pending"} | {status:"ready", access_token, refresh_token, expires_in, email?, nickname?}
// | {status:"error", error}.
// No server-side session store — each call is a fresh upstream poll. The
// client is expected to import tokens itself via POST /cb/oauth/import on ready
// (reuses the existing import + eager refresh path).
func HandleCBOAuthDevicePoll() gin.HandlerFunc {
	return func(c *gin.Context) {
		state := strings.TrimSpace(c.Query("state"))
		if state == "" {
			c.JSON(400, gin.H{"error": "state query parameter is required"})
			return
		}
		res, err := upstream.PollDeviceAuth(state)
		if err != nil {
			slog.Warn("cb oauth device poll failed", "module", "cb-oauth-device", "state", state, "error", err)
			c.JSON(502, gin.H{"status": "error", "error": err.Error()})
			return
		}
		switch res.Status {
		case "ready":
			email := upstream.ResolveOAuthImportEmail("", res.AccessToken, res.Nickname, res.UID)
			c.JSON(200, gin.H{
				"status":        "ready",
				"access_token":  res.AccessToken,
				"refresh_token": res.RefreshToken,
				"expires_in":    res.ExpiresIn,
				"email":         email,
				"nickname":      res.Nickname,
				"uid":           res.UID,
			})
		case "error":
			c.JSON(200, gin.H{"status": "error", "error": res.Error})
		default:
			c.JSON(200, gin.H{"status": "pending"})
		}
	}
}

// HandleImportCBOAuthBulk imports multiple CodeBuddy OAuth accounts.
// Body: {"accounts":[{"email":"...","access_token":"...","refresh_token":"...","expires_in":N},...]}
// or {"raw":"<json array string>"}.
// Idempotent — existing emails are updated (counted as updated, not failed).
func HandleImportCBOAuthBulk(cbKM *upstream.CBKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Accounts []struct {
				Email        string `json:"email"`
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
				ExpiresIn    int64  `json:"expires_in"`
			} `json:"accounts"`
			Raw string `json:"raw"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON body"})
			return
		}
		accounts := req.Accounts
		if len(accounts) == 0 && strings.TrimSpace(req.Raw) != "" {
			if err := json.Unmarshal([]byte(req.Raw), &accounts); err != nil {
				c.JSON(400, gin.H{"error": "raw must be a JSON array of oauth accounts", "detail": err.Error()})
				return
			}
		}
		if len(accounts) == 0 {
			c.JSON(400, gin.H{"error": "accounts array or raw JSON array is required"})
			return
		}

		added, updated, failed := 0, 0, 0
		var errors []gin.H
		var total int
		for i, a := range accounts {
			email := strings.TrimSpace(a.Email)
			at := strings.TrimSpace(a.AccessToken)
			rt := strings.TrimSpace(a.RefreshToken)
			if email == "" || at == "" || rt == "" {
				failed++
				errors = append(errors, gin.H{"index": i, "email": email, "error": "email, access_token, refresh_token required"})
				continue
			}
			expiresAt := resolveCBOAuthExpiry(at, a.ExpiresIn)
			wasNew, t := cbKM.AddOAuthAccount(email, at, rt, expiresAt)
			total = t
			if wasNew {
				added++
			} else {
				updated++
			}
		}
		slog.Info("bulk oauth import", "module", "cb", "added", added, "updated", updated, "failed", failed, "total", total)
		c.JSON(200, gin.H{
			"added":   added,
			"updated": updated,
			"failed":  failed,
			"total":   total,
			"errors":  errors,
		})
	}
}

// HandleImportAccount accepts a single Grok account (email + access_token + refresh_token)
// and stores it in Redis + adds it to the runtime pool. No JSON files needed.
func HandleImportAccount(grokAM *upstream.GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Email        string `json:"email"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
			ExpiresIn    int    `json:"expires_in"`
			Sub          string `json:"sub"`
			SSO          string `json:"sso"`
			SSORW        string `json:"sso_rw"`
			Password     string `json:"password"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON body"})
			return
		}
		if req.Email == "" || req.AccessToken == "" || req.RefreshToken == "" {
			c.JSON(400, gin.H{"error": "email, access_token, refresh_token are required"})
			return
		}
		_, total, acc := grokAM.UpsertAccount(req.Email, req.AccessToken, req.RefreshToken, req.IDToken, req.Sub, req.ExpiresIn)
		if req.SSO != "" {
			grokAM.SetSSO(req.Email, req.SSO, req.SSORW)
		}
		if req.Password != "" {
			grokAM.SetPassword(req.Email, req.Password)
		}
		slog.Info("imported account", "module", "grok", "email", req.Email, "total", total, "sso", req.SSO != "")
		c.JSON(200, gin.H{
			"status":  "imported",
			"email":   req.Email,
			"expired": acc.Expired,
			"total":   total,
		})
	}
}

// HandleImportCBKeyBulk imports multiple CodeBuddy API keys at once.
// Body: {"api_keys":["ck_...","ck_..."]} or raw newline/comma-separated string in "raw".
// Idempotent — existing keys are skipped (counted as skipped, not failed).
func HandleImportCBKeyBulk(cbKM *upstream.CBKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			APIKeys []string `json:"api_keys"`
			Raw     string   `json:"raw"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON body"})
			return
		}
		keys := req.APIKeys
		if len(keys) == 0 && req.Raw != "" {
			for _, k := range strings.FieldsFunc(req.Raw, func(r rune) bool {
				return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
			}) {
				k = strings.TrimSpace(k)
				if k != "" {
					keys = append(keys, k)
				}
			}
		}
		if len(keys) == 0 {
			c.JSON(400, gin.H{"error": "api_keys array or raw string is required"})
			return
		}
		added := 0
		skipped := 0
		for _, k := range keys {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			ok, _ := cbKM.AddKey(k)
			if ok {
				added++
			} else {
				skipped++
			}
		}
		slog.Info("bulk import", "module", "cb", "added", added, "skipped", skipped, "total", cbKM.Len())
		c.JSON(200, gin.H{
			"added":   added,
			"skipped": skipped,
			"total":   cbKM.Len(),
		})
	}
}

// HandleImportAccountBulk imports multiple Grok accounts at once.
// Body: {"accounts":[{"email":"...","access_token":"...","refresh_token":"..."},...]}
// Idempotent — existing accounts are updated (counted as updated, not failed).
func HandleImportAccountBulk(grokAM *upstream.GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Accounts []struct {
				Email        string `json:"email"`
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
				IDToken      string `json:"id_token"`
				ExpiresIn    int    `json:"expires_in"`
				Sub          string `json:"sub"`
				SSO          string `json:"sso"`
				SSORW        string `json:"sso_rw"`
				Password     string `json:"password"`
			} `json:"accounts"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON body"})
			return
		}
		if len(req.Accounts) == 0 {
			c.JSON(400, gin.H{"error": "accounts array is required"})
			return
		}
		added := 0
		updated := 0
		failed := 0
		for _, a := range req.Accounts {
			if a.Email == "" || a.AccessToken == "" || a.RefreshToken == "" {
				failed++
				continue
			}
			created, _, _ := grokAM.UpsertAccount(a.Email, a.AccessToken, a.RefreshToken, a.IDToken, a.Sub, a.ExpiresIn)
			if a.SSO != "" {
				grokAM.SetSSO(a.Email, a.SSO, a.SSORW)
			}
			if a.Password != "" {
				grokAM.SetPassword(a.Email, a.Password)
			}
			if created {
				added++
			} else {
				updated++
			}
		}
		slog.Info("bulk import", "module", "grok", "added", added, "updated", updated, "failed", failed, "total", grokAM.Len())
		c.JSON(200, gin.H{
			"added":   added,
			"updated": updated,
			"failed":  failed,
			"total":   grokAM.Len(),
		})
	}
}

// HandleDeleteAccount removes a Grok account from Redis + runtime pool.
func HandleDeleteAccount(grokAM *upstream.GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		email := c.Param("email")
		if email == "" {
			c.JSON(400, gin.H{"error": "email required"})
			return
		}
		if !grokAM.DeleteAccount(email) {
			c.JSON(404, gin.H{"error": "account not found", "email": email})
			return
		}
		remaining := grokAM.Len()
		slog.Info("deleted account", "module", "grok", "email", email, "remaining", remaining)
		c.JSON(200, gin.H{"status": "deleted", "email": email, "remaining": remaining})
	}
}

// ============================================================================
// HISTORY ENDPOINTS (v5.0)
// ============================================================================

// HandleHistory aggregates request stats + per-model breakdown.
func HandleHistory(store *db.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		hours := 24
		if h := c.Query("hours"); h != "" {
			if v, err := strconv.Atoi(h); err == nil && v > 0 {
				hours = v
			}
		}
		since := time.Now().Add(-time.Duration(hours) * time.Hour)

		stats, err := store.GetRequestStats(since)
		if err != nil {
			slog.Error("internal error", "module", "handler", "error", err)
			c.JSON(500, gin.H{"error": "internal server error"})
			return
		}

		modelStats, err := store.GetModelStats(since, 20)
		if err != nil {
			slog.Error("internal error", "module", "handler", "error", err)
			c.JSON(500, gin.H{"error": "internal server error"})
			return
		}

		c.JSON(200, gin.H{
			"period_hours":     hours,
			"since":            since.Format(time.RFC3339),
			"total_requests":   stats.TotalRequests,
			"total_errors":     stats.TotalErrors,
			"error_rate_pct":   stats.ErrorRate,
			"avg_latency_ms":   stats.AvgLatencyMs,
			"total_tokens_in":  stats.TotalTokensIn,
			"total_tokens_out": stats.TotalTokensOut,
			"total_tokens":     stats.TotalTokens,
			"by_model":         modelStats,
		})
	}
}

// HandleRecentRequests returns recent request previews (id as string for JS).
// Optional query filters: model, upstream, status (exact or 2xx/3xx/4xx/5xx),
// errors=1 (only errored rows), hours=N (within last N hours), limit=N.
func HandleRecentRequests(store *db.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := 50
		if l := c.Query("limit"); l != "" {
			if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 500 {
				limit = v
			}
		}
		f := db.RecentFilter{
			Model:    c.Query("model"),
			Upstream: c.Query("upstream"),
			Status:   c.Query("status"),
		}
		if c.Query("errors") == "1" || c.Query("errors") == "true" {
			f.ErrorOnly = true
		}
		if h := c.Query("hours"); h != "" {
			if v, err := strconv.Atoi(h); err == nil && v > 0 && v <= 24*365 {
				f.Hours = v
			}
		}
		logs, err := store.GetRecentRequests(limit, f)
		if err != nil {
			slog.Error("internal error", "module", "handler", "error", err)
			c.JSON(500, gin.H{"error": "internal server error"})
			return
		}
		c.JSON(200, gin.H{"recent_requests": logs, "count": len(logs)})
	}
}

// HandleHistoryDetail returns the full request/response JSON for a single log.
func HandleHistoryDetail(store *db.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		idStr := c.Param("id")
		id, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil || id == 0 {
			c.JSON(400, gin.H{"error": "invalid id"})
			return
		}
		detail, err := store.GetRequestDetail(id)
		if err != nil {
			c.JSON(404, gin.H{"error": "request not found"})
			return
		}
		c.JSON(200, detail)
	}
}

// ============================================================================
// API KEY MANAGEMENT ENDPOINTS
// ============================================================================

// HandleListKeys returns all gateway keys (masked, with metadata).
func HandleListKeys(am *auth.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		keys := am.GetAll()
		result := make([]gin.H, 0, len(keys))
		for _, info := range keys {
			result = append(result, gin.H{
				"key_masked":     auth.MaskKey(info.Key),
				"name":           info.Name,
				"role":           info.Role,
				"allowed_models": info.AllowedModels,
				"rpm":            info.RPM,
				"burst":          info.Burst,
				"token_quota":    info.TokenQuota,
				"tokens_used":    info.TokensUsed,
				"requests":       info.Requests,
				"created_at":     info.CreatedAt,
				"disabled":       info.Disabled,
			})
		}
		c.JSON(200, gin.H{"keys": result, "count": len(result)})
	}
}

// HandleCreateKey creates a new gateway key.
func HandleCreateKey(am *auth.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Name          string       `json:"name"`
			Role          auth.KeyRole `json:"role"`
			AllowedModels []string     `json:"allowed_models"`
			RPM           int          `json:"rpm"`
			Burst         int          `json:"burst"`
			TokenQuota    int64        `json:"token_quota"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON body"})
			return
		}
		if req.Name == "" {
			req.Name = "unnamed"
		}
		// Default role = inference. Only accept "admin" or "inference".
		if req.Role != auth.RoleAdmin && req.Role != auth.RoleInference {
			req.Role = auth.RoleInference
		}
		// Input validation: cap name length + reject control chars
		if len(req.Name) > 128 {
			c.JSON(400, gin.H{"error": "name too long (max 128 chars)"})
			return
		}
		for _, r := range req.Name {
			if r < 0x20 || r == 0x7f {
				c.JSON(400, gin.H{"error": "name contains control characters"})
				return
			}
		}
		// AK-1: reject negative / absurd rate & quota values (would break the
		// limiter math or overflow the token counter).
		if req.RPM < 0 || req.Burst < 0 || req.TokenQuota < 0 {
			c.JSON(400, gin.H{"error": "rpm, burst, and token_quota must be non-negative"})
			return
		}
		if req.RPM > 1_000_000 || req.Burst > 1_000_000 {
			c.JSON(400, gin.H{"error": "rpm and burst too large (max 1000000)"})
			return
		}
		if req.TokenQuota > 1_000_000_000_000_000 {
			c.JSON(400, gin.H{"error": "token_quota too large (max 1e15)"})
			return
		}
		key := auth.GenerateGatewayKey()
		info := am.AddWithRole(key, req.Name, req.Role, req.AllowedModels, req.RPM, req.Burst, req.TokenQuota)
		slog.Info("created key",
			"module", "auth",
			"key", auth.MaskKey(key),
			"name", req.Name,
			"role", string(req.Role),
			"models", req.AllowedModels,
			"rpm", req.RPM,
			"burst", req.Burst,
			"quota", req.TokenQuota)
		c.JSON(201, gin.H{
			"key":            info.Key,
			"key_masked":     auth.MaskKey(info.Key),
			"name":           info.Name,
			"role":           info.Role,
			"allowed_models": info.AllowedModels,
			"rpm":            info.RPM,
			"burst":          info.Burst,
			"token_quota":    info.TokenQuota,
			"tokens_used":    info.TokensUsed,
			"requests":       info.Requests,
			"created_at":     info.CreatedAt,
			"disabled":       info.Disabled,
			"message":        "Save this key now — it will not be shown again.",
		})
	}
}

// HandleDeleteKey deletes a gateway key (accepts full or masked key).
func HandleDeleteKey(am *auth.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		keyParam := c.Param("key")
		fullKey, ok := am.ResolveKey(keyParam)
		if !ok {
			c.JSON(404, gin.H{"error": "key not found"})
			return
		}
		// P3-1: prevent last-admin lockout (self-DoS).
		// Refuse to delete if this key is admin AND it's the only admin left.
		info, ok := am.Get(fullKey)
		if ok && info.Role == auth.RoleAdmin && am.CountAdmins() <= 1 {
			c.JSON(409, gin.H{"error": "cannot delete the last admin key — create another admin key first"})
			return
		}
		am.Remove(fullKey)
		slog.Info("deleted key", "module", "auth", "key", auth.MaskKey(fullKey))
		c.JSON(200, gin.H{"deleted": auth.MaskKey(fullKey)})
	}
}

// HandleUpdateKey updates a gateway key's metadata.
func HandleUpdateKey(am *auth.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		keyParam := c.Param("key")
		fullKey, ok := am.ResolveKey(keyParam)
		if !ok {
			c.JSON(404, gin.H{"error": "key not found"})
			return
		}
		var req struct {
			Name          *string       `json:"name"`
			Role          *auth.KeyRole `json:"role"`
			AllowedModels *[]string     `json:"allowed_models"`
			RPM           *int          `json:"rpm"`
			Burst         *int          `json:"burst"`
			TokenQuota    *int64        `json:"token_quota"`
			Disabled      *bool         `json:"disabled"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON body"})
			return
		}
		name := ""
		var role auth.KeyRole = "" // empty = no change
		var allowedModels []string // nil = no change, empty slice = clear whitelist
		rpm := -1
		burst := -1
		var quota int64 = -1
		if req.Name != nil {
			name = *req.Name
			// Input validation on update too
			if len(name) > 128 {
				c.JSON(400, gin.H{"error": "name too long (max 128 chars)"})
				return
			}
			for _, r := range name {
				if r < 0x20 || r == 0x7f {
					c.JSON(400, gin.H{"error": "name contains control characters"})
					return
				}
			}
		}
		if req.Role != nil && (*req.Role == auth.RoleAdmin || *req.Role == auth.RoleInference) {
			role = *req.Role
		}
		if req.AllowedModels != nil {
			allowedModels = *req.AllowedModels
		}
		if req.RPM != nil {
			rpm = *req.RPM
		}
		if req.Burst != nil {
			burst = *req.Burst
		}
		if req.TokenQuota != nil {
			quota = *req.TokenQuota
		}
		// AK-1: validate rate/quota on update too. -1 is the "no change" sentinel;
		// any other negative value is rejected, huge values are capped.
		if (rpm != -1 && rpm < 0) || (burst != -1 && burst < 0) || (quota != -1 && quota < 0) {
			c.JSON(400, gin.H{"error": "rpm, burst, and token_quota must be non-negative"})
			return
		}
		if rpm > 1_000_000 || burst > 1_000_000 {
			c.JSON(400, gin.H{"error": "rpm and burst too large (max 1000000)"})
			return
		}
		if quota > 1_000_000_000_000_000 {
			c.JSON(400, gin.H{"error": "token_quota too large (max 1e15)"})
			return
		}
		if !am.Update(fullKey, name, role, allowedModels, rpm, burst, quota, req.Disabled) {
			c.JSON(404, gin.H{"error": "key not found"})
			return
		}
		info, _ := am.Get(fullKey)
		slog.Info("updated key", "module", "auth", "key", auth.MaskKey(fullKey))
		c.JSON(200, gin.H{
			"key_masked":     auth.MaskKey(info.Key),
			"name":           info.Name,
			"role":           info.Role,
			"allowed_models": info.AllowedModels,
			"rpm":            info.RPM,
			"burst":          info.Burst,
			"token_quota":    info.TokenQuota,
			"tokens_used":    info.TokensUsed,
			"requests":       info.Requests,
			"created_at":     info.CreatedAt,
			"disabled":       info.Disabled,
		})
	}
}

// HandleKeyUsage returns usage stats for a specific key.
func HandleKeyUsage(am *auth.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		keyParam := c.Param("key")
		fullKey, ok := am.ResolveKey(keyParam)
		if !ok {
			c.JSON(404, gin.H{"error": "key not found"})
			return
		}
		info, _ := am.Get(fullKey)
		c.JSON(200, gin.H{
			"key_masked":     auth.MaskKey(info.Key),
			"name":           info.Name,
			"role":           info.Role,
			"allowed_models": info.AllowedModels,
			"rpm":            info.RPM,
			"burst":          info.Burst,
			"token_quota":    info.TokenQuota,
			"tokens_used":    info.TokensUsed,
			"tokens_left": func() int64 {
				if info.TokenQuota > 0 {
					return info.TokenQuota - info.TokensUsed
				}
				return -1
			}(),
			"requests":   info.Requests,
			"created_at": info.CreatedAt,
			"disabled":   info.Disabled,
			"quota_pct": func() float64 {
				if info.TokenQuota > 0 {
					return float64(info.TokensUsed) / float64(info.TokenQuota) * 100
				}
				return 0
			}(),
		})
	}
}

// ============================================================================
// WEB UI DASHBOARD
// ============================================================================

// HandleDashboard serves the embedded SPA.
// IMPORTANT: never inject live gateway keys into public HTML — /dashboard is
// unauthenticated. Clients set the key via localStorage or ?key= URL param.
func HandleDashboard() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Data(200, "text/html; charset=utf-8", []byte(dashboardHTML))
	}
}

// HandleLogin serves the login page (GET) and processes login (POST).
// On successful auth, sets an HttpOnly cookie with a random session token
// (NOT the raw API key — P3-3 session fixation fix) and redirects to /dashboard.
func HandleLogin(am *auth.Manager, sessions *auth.SessionStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == "GET" {
			c.Data(200, "text/html; charset=utf-8", []byte(loginPageHTML))
			return
		}

		// POST: verify key
		var req struct {
			Key string `form:"key" json:"key"`
		}
		if err := c.ShouldBind(&req); err != nil || req.Key == "" {
			c.Data(200, "text/html; charset=utf-8", []byte(loginPageHTMLWithError("Key is required")))
			return
		}
		req.Key = strings.TrimSpace(req.Key)

		if !am.Valid(req.Key) {
			c.Data(200, "text/html; charset=utf-8", []byte(loginPageHTMLWithError("Invalid API key")))
			return
		}

		// D2: only admin keys can log into the dashboard. Inference-role keys can
		// call /v1/* with a Bearer token, but the dashboard endpoints are
		// admin-only — letting them in produces a redirect loop (dashboard XHR
		// gets 401 → JS redirects to /login → login succeeds → loop).
		if info, ok := am.Get(req.Key); !ok || info.Role != auth.RoleAdmin {
			c.Data(200, "text/html; charset=utf-8", []byte(loginPageHTMLWithError("This key does not have dashboard access (admin role required)")))
			return
		}

		// P3-3: issue a random session token bound to the key (not the key itself).
		token, err := sessions.Create(req.Key)
		if err != nil {
			c.Data(200, "text/html; charset=utf-8", []byte(loginPageHTMLWithError("Session error")))
			return
		}

		c.SetSameSite(http.SameSiteLaxMode)
		cookieSecure := os.Getenv("COOKIE_SECURE") != "0"
		c.SetCookie("foxrouters_session", token, int(auth.SessionTTL.Seconds()), "/", "", cookieSecure, true)
		c.Redirect(302, "/dashboard")
	}
}

// HandleLogout clears the session cookie and redirects to /login.
func HandleLogout(sessions *auth.SessionStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, _ := c.Cookie("foxrouters_session")
		sessions.Revoke(token)
		cookieSecure := os.Getenv("COOKIE_SECURE") != "0"
		c.SetCookie("foxrouters_session", "", -1, "/", "", cookieSecure, true)
		c.Redirect(302, "/login")
	}
}

// ============================================================================
// CB KEY MANAGEMENT (delete + cleanup)
// ============================================================================

// HandleSyncCBCredits triggers a live meter sync for one or all CodeBuddy keys.
// Body optional: { "email": "..." } or { "key": "..." } to sync one; empty = all.
// Returns {synced, failed, results:[{key, used, remain, limit, status, error?}]}.
func HandleSyncCBCredits(cbKM *upstream.CBKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Email string `json:"email"`
			Key   string `json:"key"`
		}
		_ = c.ShouldBindJSON(&req) // empty body is fine

		target := strings.TrimSpace(req.Key)
		if target == "" {
			target = strings.TrimSpace(req.Email)
		}

		type result struct {
			Key    string  `json:"key"`
			Used   float64 `json:"used"`
			Remain float64 `json:"remain"`
			Limit  float64 `json:"limit"`
			Status int     `json:"status"`
			Error  string  `json:"error,omitempty"`
		}

		var keys []*upstream.CBKey
		if target != "" {
			full := cbKM.ResolveKey(target)
			if full == "" {
				c.JSON(404, gin.H{"error": "key not found"})
				return
			}
			for _, k := range cbKM.GetAll() {
				if k.Key == full {
					keys = append(keys, k)
					break
				}
			}
			if len(keys) == 0 {
				c.JSON(404, gin.H{"error": "key not found"})
				return
			}
		} else {
			keys = cbKM.GetAll()
		}

		results := make([]result, 0, len(keys))
		synced, failed := 0, 0
		for _, k := range keys {
			display := k.DisplayID()
			r := result{Key: display}
			if err := k.SyncCredits(); err != nil {
				failed++
				r.Error = err.Error()
				s := k.Snapshot()
				r.Used = s.CreditsUsed
				r.Remain = s.CreditsRemain
				r.Limit = s.CreditLimit
				r.Status = s.MeterStatus
			} else {
				synced++
				s := k.Snapshot()
				r.Used = s.CreditsUsed
				r.Remain = s.CreditsRemain
				r.Limit = s.CreditLimit
				r.Status = s.MeterStatus
			}
			results = append(results, r)
		}
		c.JSON(200, gin.H{
			"synced":  synced,
			"failed":  failed,
			"results": results,
		})
	}
}

// HandleDeleteCBKey deletes a CodeBuddy key by its key string (full or masked).
func HandleDeleteCBKey(cbKM *upstream.CBKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		keyParam := c.Param("key")
		if keyParam == "" {
			c.JSON(400, gin.H{"error": "key required"})
			return
		}
		// Resolve masked key → full key (P3 #4: don't return full keys in lists)
		fullKey := cbKM.ResolveKey(keyParam)
		if fullKey == "" {
			c.JSON(404, gin.H{"error": "cb key not found"})
			return
		}
		if !cbKM.DeleteKey(fullKey) {
			c.JSON(404, gin.H{"error": "cb key not found"})
			return
		}
		remaining := cbKM.Len()
		slog.Info("deleted cb key", "module", "cb", "remaining", remaining)
		c.JSON(200, gin.H{"status": "deleted", "remaining": remaining})
	}
}

// HandleDisableCBKey permanently disables a CodeBuddy key (full or masked).
// Body: {"key":"...", "reason":"..."} — reason optional, persisted to Redis.
// Uses the in-manager disable path so the state is consistent in memory + Redis
// and survives the credit-sync worker (unlike raw Redis HSET disables).
func HandleDisableCBKey(cbKM *upstream.CBKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Key    string `json:"key"`
			Reason string `json:"reason"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || req.Key == "" {
			c.JSON(400, gin.H{"error": "key required"})
			return
		}
		fullKey := cbKM.ResolveKey(req.Key)
		if fullKey == "" {
			c.JSON(404, gin.H{"error": "cb key not found"})
			return
		}
		if !cbKM.DisableKey(fullKey, req.Reason) {
			c.JSON(404, gin.H{"error": "cb key not found"})
			return
		}
		slog.Info("disabled cb key via api", "module", "cb", "key", maskSecret(fullKey), "reason", req.Reason)
		c.JSON(200, gin.H{"status": "disabled", "remaining": cbKM.Len()})
	}
}

// HandleEnableCBKey re-enables a previously disabled CodeBuddy key (full or masked).
// Body: {"key":"..."} — uses the in-manager path so memory + Redis stay consistent.
func HandleEnableCBKey(cbKM *upstream.CBKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Key string `json:"key"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || req.Key == "" {
			c.JSON(400, gin.H{"error": "key required"})
			return
		}
		fullKey := cbKM.ResolveKey(req.Key)
		if fullKey == "" {
			c.JSON(404, gin.H{"error": "cb key not found"})
			return
		}
		if !cbKM.EnableKey(fullKey) {
			c.JSON(404, gin.H{"error": "cb key not found"})
			return
		}
		slog.Info("enabled cb key via api", "module", "cb", "key", maskSecret(fullKey))
		c.JSON(200, gin.H{"status": "enabled", "remaining": cbKM.Len()})
	}
}

// maskSecret masks a secret for logs: first 8 chars + "..." + last 4.
func maskSecret(s string) string {
	if len(s) <= 12 {
		return "***"
	}
	return s[:8] + "..." + s[len(s)-4:]
}

// HandleCleanupDisabled removes all permanently disabled keys/accounts.
// Query param ?type=grok|cb|all (default: all)
func HandleCleanupDisabled(grokAM *upstream.GrokAccountManager, cbKM *upstream.CBKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		typ := c.DefaultQuery("type", "all")
		result := gin.H{"type": typ}

		if typ == "grok" || typ == "all" {
			removed := grokAM.CleanupDisabled()
			result["grok_removed"] = removed
			result["grok_remaining"] = grokAM.Len()
		}
		if typ == "cb" || typ == "all" {
			removed := cbKM.CleanupDisabled()
			result["cb_removed"] = removed
			result["cb_remaining"] = cbKM.Len()
		}

		slog.Info("cleanup disabled", "module", "admin", "type", typ,
			"grok_removed", result["grok_removed"], "cb_removed", result["cb_removed"])
		c.JSON(200, result)
	}
}

// HandleCleanupBanned removes all banned Grok accounts (token_status == "banned").
// Query param ?type=grok|all (default: grok). CB has no "banned" status.
// Note: banned ≡ permanently disabled (disabled && disabledAt zero). Cooldown preserved.
func HandleCleanupBanned(grokAM *upstream.GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		typ := c.DefaultQuery("type", "grok")
		result := gin.H{"type": typ}

		if typ == "grok" || typ == "all" {
			removed := grokAM.CleanupBanned()
			result["grok_removed"] = removed
			result["grok_remaining"] = grokAM.Len()
		}

		slog.Info("cleanup banned", "module", "admin", "type", typ,
			"grok_removed", result["grok_removed"])
		c.JSON(200, result)
	}
}

// HandleSyncGrokBilling triggers a billing sync for one or all Grok accounts.
// Body optional: {"email": "..."} to sync one; empty = all non-banned accounts.
func HandleSyncGrokBilling(grokAM *upstream.GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Email string `json:"email"`
		}
		_ = c.ShouldBindJSON(&req)
		target := strings.TrimSpace(req.Email)

		type result struct {
			Email          string `json:"email"`
			PeriodEnd      string `json:"period_end,omitempty"`
			OnDemandCap    int64  `json:"on_demand_cap"`
			OnDemandUsed   int64  `json:"on_demand_used"`
			PrepaidBalance int64  `json:"prepaid_balance"`
			UnifiedBilling bool   `json:"unified_billing"`
			Error          string `json:"error,omitempty"`
		}

		var accounts []*upstream.GrokAccount
		if target != "" {
			found := false
			for _, a := range grokAM.GetAll() {
				if a.Email == target {
					accounts = append(accounts, a)
					found = true
					break
				}
			}
			if !found {
				c.JSON(404, gin.H{"error": "account not found"})
				return
			}
		} else {
			accounts = grokAM.GetAll()
		}

		results := make([]result, 0, len(accounts))
		synced, failed := 0, 0
		for _, a := range accounts {
			r := result{Email: a.Email}
			if err := a.SyncBilling(); err != nil {
				failed++
				r.Error = err.Error()
			} else {
				synced++
				s := a.Snapshot()
				r.PeriodEnd = s.PeriodEnd
				r.OnDemandCap = s.OnDemandCap
				r.OnDemandUsed = s.OnDemandUsed
				r.PrepaidBalance = s.PrepaidBalance
				r.UnifiedBilling = s.UnifiedBilling
			}
			results = append(results, r)
		}
		c.JSON(200, gin.H{
			"synced":  synced,
			"failed":  failed,
			"results": results,
		})
	}
}
