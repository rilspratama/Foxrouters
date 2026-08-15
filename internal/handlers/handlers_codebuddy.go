package handlers

import (
	"encoding/json"
	"foxrouters/internal/upstream"
	"log/slog"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

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
