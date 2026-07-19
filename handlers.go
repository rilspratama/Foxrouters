package main

import (
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// ENDPOINTS
// ============================================================================

// handleHealthMinimal serves a bare-bones liveness response for HEAD /health
// and for unauthenticated GET. Avoids the full telemetry path that may hang
// when the authed branch touches am.Get() + cbKM.GetAll() under load.
// Gin auto-handles HEAD by calling the GET handler, but some clients send
// HEAD without Accept-Encoding, causing the gzip middleware to wrap the
// writer and then never flush (no body written). This explicit handler
// short-circuits that path.
func handleHealthMinimal() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status":  "healthy",
			"service": "foxrouters",
			"version": Version,
		})
	}
}

func handleHealth(grokAM *GrokAccountManager, cbKM *CBKeyManager, hc *HealthChecker, am *AuthManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		grokStats := hc.grok.Stats()
		cbStats := hc.cb.Stats()

		// Overall status: unhealthy if any circuit is open
		grokCircuit := grokStats["circuit_state"].(string)
		cbCircuit := cbStats["circuit_state"].(string)
		overall := "healthy"
		if grokCircuit == "open" || cbCircuit == "open" {
			overall = "degraded"
		}

		// Public response: minimal liveness only.
		// Detailed telemetry only if caller presents a valid gateway key
		// (via Bearer header OR session cookie — dashboard uses cookie).
		// When GATEWAY_AUTH_DISABLE=1, everyone gets full telemetry (dev mode).
		authed := false
		if os.Getenv("GATEWAY_AUTH_DISABLE") == "1" {
			authed = true
		} else if auth := c.GetHeader("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			if info := am.Get(auth[7:]); info != nil {
				authed = true
				_ = info
			}
		} else if ck, err := c.Cookie("foxrouters_session"); err == nil && ck != "" {
			if info := am.Get(ck); info != nil {
				authed = true
				_ = info
			}
		}
		if !authed {
			c.JSON(200, gin.H{
				"status":  overall,
				"service": "foxrouters",
				"version": Version,
			})
			return
		}

		// Authed: full telemetry
		c.JSON(200, gin.H{
			"status":  overall,
			"service": "foxrouters",
			"version": Version,
			"mode":    "unified (grok + codebuddy)",
			"upstreams": gin.H{
				"grok":      grokStats,
				"codebuddy": cbStats,
			},
			"grok_accounts": grokAM.Len(),
			"cb_keys":       cbKM.Len(),
			"cb_keys_active": func() int {
				active := 0
				for _, k := range cbKM.GetAll() {
					if _, _, disabled := k.Stats(); !disabled {
						active++
					}
				}
				return active
			}(),
			"time": time.Now().Format(time.RFC3339),
		})
	}
}

func handleAccounts(grokAM *GrokAccountManager, cbKM *CBKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		grokAccs := grokAM.GetAll()
		grokResult := make([]gin.H, 0)
		for _, a := range grokAccs {
			a.mu.RLock()
			tokenStatus := "active"
			if a.disabled && a.disabledAt.IsZero() {
				tokenStatus = "banned"
			} else if a.disabled {
				tokenStatus = "cooldown"
			} else if time.Now().After(a.expiresAt.Add(-REFRESH_BUFFER)) {
				tokenStatus = "expired"
			}
			grokResult = append(grokResult, gin.H{
				"provider": "grok", "email": a.Email, "sub": a.Sub,
				"expires_at": a.Expired, "expires_in": a.ExpiresIn,
				"last_refresh": a.LastRefresh, "disabled": a.disabled,
				"disabled_at": a.disabledAt, "token_status": tokenStatus,
			})
			a.mu.RUnlock()
		}
		cbKeys := cbKM.GetAll()
		cbResult := make([]gin.H, 0)
		for _, k := range cbKeys {
			credits, reqs, disabled := k.Stats()
			cbResult = append(cbResult, gin.H{
				"provider":       "codebuddy",
				"key":            k.Key[:8] + "..." + k.Key[len(k.Key)-4:],
				"disabled":       disabled,
				"credits_used":   credits,
				"credits_left":   CB_CREDIT_LIMIT - credits,
				"total_requests": reqs,
			})
		}
		c.JSON(200, gin.H{
			"grok": grokResult, "codebuddy": cbResult,
			"grok_total": len(grokResult), "cb_total": len(cbResult),
		})
	}
}

func handleRefresh(grokAM *GrokAccountManager) gin.HandlerFunc {
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

// handleImportCBKey hot-imports a CodeBuddy API key into runtime pool + Redis.
// Body: {"api_key":"ck_..."} or {"key":"ck_..."}. Idempotent — existing keys return added=false.
func handleImportCBKey(cbKM *CBKeyManager) gin.HandlerFunc {
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

// handleImportAccount accepts a single Grok account (email + access_token + refresh_token)
// and stores it in Redis + adds it to the runtime pool. No JSON files needed.
func handleImportAccount(grokAM *GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Email        string `json:"email"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
			ExpiresIn    int    `json:"expires_in"`
			Sub          string `json:"sub"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON body"})
			return
		}
		if req.Email == "" || req.AccessToken == "" || req.RefreshToken == "" {
			c.JSON(400, gin.H{"error": "email, access_token, refresh_token are required"})
			return
		}
		if req.ExpiresIn == 0 {
			req.ExpiresIn = 21600 // default 6h
		}
		expiresAt := time.Now().Add(time.Duration(req.ExpiresIn) * time.Second)
		acc := &GrokAccount{
			Email:        req.Email,
			AccessToken:  req.AccessToken,
			RefreshToken: req.RefreshToken,
			IDToken:      req.IDToken,
			ExpiresIn:     req.ExpiresIn,
			Expired:       expiresAt.Format(time.RFC3339),
			LastRefresh:   time.Now().Format(time.RFC3339),
			Sub:           req.Sub,
			expiresAt:     expiresAt,
			db:            grokAM.db,
		}
		// Save to Redis
		if grokAM.db != nil {
			dbSaveGrokAccount(grokAM.db, acc)
		}
		// Add to runtime pool — capture total under lock (race-free)
		grokAM.mu.Lock()
		// Check if already exists
		found := false
		for _, existing := range grokAM.accounts {
			if existing.Email == req.Email {
				existing.mu.Lock()
				existing.AccessToken = req.AccessToken
				existing.RefreshToken = req.RefreshToken
				existing.IDToken = req.IDToken
				existing.ExpiresIn = req.ExpiresIn
				existing.Expired = acc.Expired
				existing.LastRefresh = acc.LastRefresh
				existing.Sub = req.Sub
				existing.expiresAt = expiresAt
				existing.disabled = false
				existing.disabledAt = time.Time{}
				existing.mu.Unlock()
				// Persist mutated state to Redis (P1 fix: was missing SaveGrokAccount on existing-account update)
				if grokAM.db != nil {
					dbSaveGrokAccount(grokAM.db, existing)
				}
				found = true
				break
			}
		}
		if !found {
			grokAM.accounts = append(grokAM.accounts, acc)
		}
		total := len(grokAM.accounts)
		grokAM.mu.Unlock()
		slog.Info("imported account", "module", "grok", "email", req.Email, "total", total)
		c.JSON(200, gin.H{
			"status":  "imported",
			"email":   req.Email,
			"expired": acc.Expired,
			"total":   total,
		})
	}
}

// handleImportCBKeyBulk imports multiple CodeBuddy API keys at once.
// Body: {"api_keys":["ck_...","ck_..."]} or raw newline/comma-separated string in "raw".
// Idempotent — existing keys are skipped (counted as skipped, not failed).
func handleImportCBKeyBulk(cbKM *CBKeyManager) gin.HandlerFunc {
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
				return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '	'
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

// handleImportAccountBulk imports multiple Grok accounts at once.
// Body: {"accounts":[{"email":"...","access_token":"...","refresh_token":"..."},...]}
// Idempotent — existing accounts are updated (counted as updated, not failed).
func handleImportAccountBulk(grokAM *GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Accounts []struct {
				Email        string `json:"email"`
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
				IDToken      string `json:"id_token"`
				ExpiresIn    int    `json:"expires_in"`
				Sub          string `json:"sub"`
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
			expiresIn := a.ExpiresIn
			if expiresIn == 0 {
				expiresIn = 21600
			}
			expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)
			acc := &GrokAccount{
				Email:        a.Email,
				AccessToken:  a.AccessToken,
				RefreshToken: a.RefreshToken,
				IDToken:      a.IDToken,
				ExpiresIn:     expiresIn,
				Expired:       expiresAt.Format(time.RFC3339),
				LastRefresh:   time.Now().Format(time.RFC3339),
				Sub:           a.Sub,
				expiresAt:     expiresAt,
				db:            grokAM.db,
			}
			if grokAM.db != nil {
				dbSaveGrokAccount(grokAM.db, acc)
			}
			grokAM.mu.Lock()
			found := false
			for _, existing := range grokAM.accounts {
				if existing.Email == a.Email {
					existing.mu.Lock()
					existing.AccessToken = a.AccessToken
					existing.RefreshToken = a.RefreshToken
					existing.IDToken = a.IDToken
					existing.ExpiresIn = expiresIn
					existing.Expired = acc.Expired
					existing.LastRefresh = acc.LastRefresh
					existing.Sub = a.Sub
					existing.expiresAt = expiresAt
					existing.disabled = false
					existing.disabledAt = time.Time{}
					existing.mu.Unlock()
					if grokAM.db != nil {
						dbSaveGrokAccount(grokAM.db, existing)
					}
					found = true
					break
				}
			}
			if !found {
				grokAM.accounts = append(grokAM.accounts, acc)
			}
			grokAM.mu.Unlock()
			if found {
				updated++
			} else {
				added++
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

// handleDeleteAccount removes a Grok account from Redis + runtime pool.
func handleDeleteAccount(grokAM *GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		email := c.Param("email")
		if email == "" {
			c.JSON(400, gin.H{"error": "email required"})
			return
		}
		// Remove from Redis
		grokAM.db.DeleteGrokAccount(email)
		// Remove from runtime pool
		grokAM.mu.Lock()
		for i, acc := range grokAM.accounts {
			if acc.Email == email {
				grokAM.accounts = append(grokAM.accounts[:i], grokAM.accounts[i+1:]...)
				break
			}
		}
		remaining := len(grokAM.accounts)
		grokAM.mu.Unlock()
		slog.Info("deleted account", "module", "grok", "email", email, "remaining", remaining)
		c.JSON(200, gin.H{"status": "deleted", "email": email, "remaining": remaining})
	}
}

// ============================================================================
// HISTORY ENDPOINTS (v5.0)
// ============================================================================

func handleHistory(db *DBStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		hours := 24
		if h := c.Query("hours"); h != "" {
			if v, err := strconv.Atoi(h); err == nil && v > 0 {
				hours = v
			}
		}
		since := time.Now().Add(-time.Duration(hours) * time.Hour)

		stats, err := db.GetRequestStats(since)
		if err != nil {
			slog.Error("internal error", "module", "handler", "error", err); c.JSON(500, gin.H{"error": "internal server error"})
			return
		}

		modelStats, err := db.GetModelStats(since, 20)
		if err != nil {
			slog.Error("internal error", "module", "handler", "error", err); c.JSON(500, gin.H{"error": "internal server error"})
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
			"total_tokens":      stats.TotalTokens,
			"by_model":         modelStats,
		})
	}
}

func handleRecentRequests(db *DBStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := 50
		if l := c.Query("limit"); l != "" {
			if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 500 {
				limit = v
			}
		}
		logs, err := db.GetRecentRequests(limit)
		if err != nil {
			slog.Error("internal error", "module", "handler", "error", err); c.JSON(500, gin.H{"error": "internal server error"})
			return
		}
		c.JSON(200, gin.H{"recent_requests": logs, "count": len(logs)})
	}
}

func handleHistoryDetail(db *DBStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		idStr := c.Param("id")
		id, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil || id == 0 {
			c.JSON(400, gin.H{"error": "invalid id"})
			return
		}
		detail, err := db.GetRequestDetail(id)
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

// handleListKeys returns all gateway keys (masked, with metadata).
func handleListKeys(am *AuthManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		keys := am.GetAll()
		result := make([]gin.H, 0, len(keys))
		for _, info := range keys {
			result = append(result, gin.H{
				"key_masked":     maskKey(info.Key),
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

// handleCreateKey creates a new gateway key.
func handleCreateKey(am *AuthManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Name          string   `json:"name"`
			Role          KeyRole  `json:"role"`
			AllowedModels []string `json:"allowed_models"`
			RPM           int      `json:"rpm"`
			Burst         int      `json:"burst"`
			TokenQuota    int64    `json:"token_quota"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON body"})
			return
		}
		if req.Name == "" {
			req.Name = "unnamed"
		}
		// Default role = inference. Only accept "admin" or "inference".
		if req.Role != RoleAdmin && req.Role != RoleInference {
			req.Role = RoleInference
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
		key := generateGatewayKey()
		info := am.AddWithRole(key, req.Name, req.Role, req.AllowedModels, req.RPM, req.Burst, req.TokenQuota)
		slog.Info("created key",
			"module", "auth",
			"key", maskKey(key),
			"name", req.Name,
			"role", string(req.Role),
			"models", req.AllowedModels,
			"rpm", req.RPM,
			"burst", req.Burst,
			"quota", req.TokenQuota)
		c.JSON(201, gin.H{
			"key":            info.Key,
			"key_masked":     maskKey(info.Key),
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

// handleDeleteKey deletes a gateway key (accepts full or masked key).
func handleDeleteKey(am *AuthManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		keyParam := c.Param("key")
		fullKey, ok := am.resolveKey(keyParam)
		if !ok {
			c.JSON(404, gin.H{"error": "key not found"})
			return
		}
		am.Remove(fullKey)
		slog.Info("deleted key", "module", "auth", "key", maskKey(fullKey))
		c.JSON(200, gin.H{"deleted": maskKey(fullKey)})
	}
}

// handleUpdateKey updates a gateway key's metadata.
func handleUpdateKey(am *AuthManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		keyParam := c.Param("key")
		fullKey, ok := am.resolveKey(keyParam)
		if !ok {
			c.JSON(404, gin.H{"error": "key not found"})
			return
		}
		var req struct {
			Name          *string   `json:"name"`
			Role          *KeyRole  `json:"role"`
			AllowedModels *[]string `json:"allowed_models"`
			RPM           *int      `json:"rpm"`
			Burst         *int      `json:"burst"`
			TokenQuota    *int64    `json:"token_quota"`
			Disabled      *bool     `json:"disabled"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON body"})
			return
		}
		name := ""
		var role KeyRole = "" // empty = no change
		var allowedModels []string = nil // nil = no change, empty slice = clear whitelist
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
		if req.Role != nil && (*req.Role == RoleAdmin || *req.Role == RoleInference) {
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
		if !am.Update(fullKey, name, role, allowedModels, rpm, burst, quota, req.Disabled) {
			c.JSON(404, gin.H{"error": "key not found"})
			return
		}
		info := am.Get(fullKey)
		slog.Info("updated key", "module", "auth", "key", maskKey(fullKey))
		c.JSON(200, gin.H{
			"key_masked":     maskKey(info.Key),
			"name":           info.Name,
			"role":           info.Role,
			"allowed_models": info.AllowedModels,
			"rpm":         info.RPM,
			"burst":       info.Burst,
			"token_quota": info.TokenQuota,
			"tokens_used": info.TokensUsed,
			"requests":    info.Requests,
			"created_at":  info.CreatedAt,
			"disabled":    info.Disabled,
		})
	}
}

// handleKeyUsage returns usage stats for a specific key.
func handleKeyUsage(am *AuthManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		keyParam := c.Param("key")
		fullKey, ok := am.resolveKey(keyParam)
		if !ok {
			c.JSON(404, gin.H{"error": "key not found"})
			return
		}
		info := am.Get(fullKey)
		c.JSON(200, gin.H{
			"key_masked":     maskKey(info.Key),
			"name":           info.Name,
			"role":           info.Role,
			"allowed_models": info.AllowedModels,
			"rpm":            info.RPM,
			"burst":        info.Burst,
			"token_quota":  info.TokenQuota,
			"tokens_used":  info.TokensUsed,
			"tokens_left":  func() int64 { if info.TokenQuota > 0 { return info.TokenQuota - info.TokensUsed }; return -1 }(),
			"requests":     info.Requests,
			"created_at":   info.CreatedAt,
			"disabled":     info.Disabled,
			"quota_pct":    func() float64 { if info.TokenQuota > 0 { return float64(info.TokensUsed) / float64(info.TokenQuota) * 100 }; return 0 }(),
		})
	}
}

// ============================================================================
// WEB UI DASHBOARD
// ============================================================================

// handleDashboard serves the embedded SPA.
// IMPORTANT: never inject live gateway keys into public HTML — /dashboard is
// unauthenticated. Clients set the key via localStorage or ?key= URL param.
func handleDashboard() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Data(200, "text/html; charset=utf-8", []byte(dashboardHTML))
	}
}

// handleLogin serves the login page (GET) and processes login (POST).
// On successful auth, sets an HttpOnly cookie with the gateway key and redirects to /dashboard.
func handleLogin(am *AuthManager) gin.HandlerFunc {
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

		am.mu.RLock()
		_, valid := am.keys[req.Key]
		am.mu.RUnlock()
		if !valid {
			c.Data(200, "text/html; charset=utf-8", []byte(loginPageHTMLWithError("Invalid API key")))
			return
		}

		// Set HttpOnly cookie — 7 day expiry, not accessible via JS (XSS protection)
		// SameSite=Lax prevents CSRF on cross-site form submissions (most browsers default to Lax,
		// but we set it explicitly for defense-in-depth). Secure=false because the gateway
		// typically runs behind a reverse proxy that terminates TLS.
		c.SetSameSite(http.SameSiteLaxMode)
		c.SetCookie("foxrouters_session", req.Key, 7*24*3600, "/", "", false, true)
		c.Redirect(302, "/dashboard")
	}
}

// handleLogout clears the session cookie and redirects to /login.
func handleLogout() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.SetCookie("foxrouters_session", "", -1, "/", "", false, true)
		c.Redirect(302, "/login")
	}
}

// loginPageHTML returns the login page with FoxRouters branding.
const loginPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>FoxRouters — Login</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;590;600;700&family=JetBrains+Mono:wght@400;500;600&display=swap" rel="stylesheet">
<style>
:root {
  --bg: #0d1117; --bg-panel: #161b22; --bg-elevated: #21262d;
  --text: #e6edf3; --text-tertiary: #6e7681; --text-quaternary: #484f58;
  --brand: #6366f1; --brand-hover: #818cf8;
  --border: #30363d; --border-bright: rgba(255,255,255,0.16);
  --red: #f85149; --red-subtle: rgba(248,81,73,0.12);
  --radius: 8px; --radius-lg: 12px;
  --font: 'Inter', -apple-system, sans-serif; --mono: 'JetBrains Mono', monospace;
  --shadow-modal: 0 8px 32px rgba(0,0,0,0.5);
}
* { margin:0; padding:0; box-sizing:border-box; }
body {
  font-family: var(--font); background: var(--bg); color: var(--text);
  min-height: 100vh; display: flex; align-items: center; justify-content: center;
  -webkit-font-smoothing: antialiased;
}
.login-card {
  background: var(--bg-panel); border: 1px solid var(--border);
  border-radius: var(--radius-lg); padding: 40px; width: 90%; max-width: 400px;
  box-shadow: var(--shadow-modal);
}
.login-logo {
  width: 48px; height: 48px; border-radius: var(--radius);
  background: var(--brand); display: flex; align-items: center; justify-content: center;
  margin: 0 auto 20px; color: #fff; box-shadow: 0 4px 12px rgba(99,102,241,0.4);
}
.login-title { text-align: center; font-size: 20px; font-weight: 590; margin-bottom: 6px; }
.login-sub { text-align: center; font-size: 13px; color: var(--text-tertiary); margin-bottom: 28px; }
.login-error {
  background: var(--red-subtle); color: var(--red); border: 1px solid rgba(248,81,73,0.3);
  border-radius: var(--radius); padding: 10px 14px; font-size: 13px; margin-bottom: 16px;
  text-align: center;
}
.login-field { margin-bottom: 16px; }
.login-label { font-size: 12px; color: var(--text-tertiary); display: block; margin-bottom: 6px; font-weight: 500; }
.login-input {
  width: 100%; padding: 10px 14px; background: var(--bg); border: 1px solid var(--border);
  border-radius: var(--radius); color: var(--text); font-family: var(--mono); font-size: 13px;
  transition: border-color 150ms ease;
}
.login-input:focus { outline: none; border-color: var(--brand); box-shadow: 0 0 0 3px rgba(99,102,241,0.15); }
.login-btn {
  width: 100%; padding: 11px; background: var(--brand); color: #fff; border: none;
  border-radius: var(--radius); font-size: 14px; font-weight: 590; cursor: pointer;
  font-family: var(--font); transition: background 150ms ease, box-shadow 150ms ease, transform 200ms ease;
  box-shadow: 0 1px 3px rgba(99,102,241,0.3);
}
.login-btn:hover { background: var(--brand-hover); box-shadow: 0 4px 12px rgba(99,102,241,0.4); transform: translateY(-1px); }
.login-btn:active { transform: translateY(0); }
.login-footer { text-align: center; margin-top: 20px; font-size: 11px; color: var(--text-quaternary); font-family: var(--mono); }
</style>
</head>
<body>
<div class="login-card">
  <div class="login-logo">
    <svg width="24" height="24" viewBox="0 0 32 32" fill="none" xmlns="http://www.w3.org/2000/svg">
      <path d="M8 4L11 12L6 10L8 4Z" fill="currentColor"/>
      <path d="M24 4L21 12L26 10L24 4Z" fill="currentColor"/>
      <path d="M16 7C11 7 7 11 7 16C7 20 10 23 16 25C22 23 25 20 25 16C25 11 21 7 16 7Z" fill="currentColor"/>
      <path d="M12 15C13 14 14 14 16 14C18 14 19 14 20 15" stroke="rgba(255,255,255,0.9)" stroke-width="1.2" stroke-linecap="round" fill="none"/>
      <path d="M11 17C13 16 14.5 16 16 16C17.5 16 19 16 21 17" stroke="rgba(255,255,255,0.7)" stroke-width="1.2" stroke-linecap="round" fill="none"/>
      <circle cx="13" cy="13" r="1.2" fill="rgba(255,255,255,0.95)"/>
      <circle cx="19" cy="13" r="1.2" fill="rgba(255,255,255,0.95)"/>
      <circle cx="16" cy="19" r="1" fill="rgba(255,255,255,0.9)"/>
    </svg>
  </div>
  <div class="login-title">FoxRouters</div>
  <div class="login-sub">Gateway Control Panel</div>
  <form method="POST" action="/login">
    <div class="login-field">
      <label class="login-label" for="key">Gateway API Key</label>
      <input class="login-input" type="password" id="key" name="key" placeholder="gw-..." autofocus required>
    </div>
    <button class="login-btn" type="submit">Sign In</button>
  </form>
  <div class="login-footer">FoxRouters v5.11</div>
</div>
</body>
</html>`

// loginPageHTMLWithError returns the login page with an error message.
func loginPageHTMLWithError(msg string) string {
	return strings.Replace(loginPageHTML,
		`<div class="login-sub">Gateway Control Panel</div>`,
		`<div class="login-sub">Gateway Control Panel</div><div class="login-error">`+msg+`</div>`, 1)
}
