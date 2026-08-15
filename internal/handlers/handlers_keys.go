package handlers

import (
	"foxrouters/internal/auth"
	"log/slog"

	"github.com/gin-gonic/gin"
)

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
