// Package handlers — proxy pool admin API (v1.5.0).
//
// All handlers are admin-only (wire under AdminMiddleware). Mutations are
// CSRF-guarded upstream at the router. State lives in Redis via
// *proxy.ProxyPool; the pool caches entries in memory and reloads on every
// mutation.
package handlers

import (
	"strings"
	"time"

	"foxrouters/internal/proxy"

	"github.com/gin-gonic/gin"
)

// proxyInput is the wire schema for POST /api/proxies and PUT /api/proxies/:id.
type proxyInput struct {
	Protocol  string   `json:"protocol"`
	Host      string   `json:"host"`
	Port      int      `json:"port"`
	Username  string   `json:"username"`
	Password  string   `json:"password"`
	Label     string   `json:"label"`
	Upstreams []string `json:"upstreams"` // nil = keep-existing on Update, ["all"] default on Add
	Enabled   *bool    `json:"enabled"`   // pointer so we can distinguish unset vs false
}

// HandleListProxies: GET /api/proxies → { proxies: [ProxyEntry, …], stats: {...} }.
func HandleListProxies(pool *proxy.ProxyPool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if pool == nil {
			c.JSON(500, gin.H{"error": "proxy pool not initialised"})
			return
		}
		entries := pool.List()
		var enabled, disabled, active int
		activeCutoff := time.Now().Add(-5 * time.Minute)
		for _, e := range entries {
			if e.Enabled {
				enabled++
			} else {
				disabled++
			}
			if !e.LastUsedAt.IsZero() && e.LastUsedAt.After(activeCutoff) {
				active++
			}
		}
		c.JSON(200, gin.H{
			"proxies": entries,
			"stats": gin.H{
				"total":    len(entries),
				"enabled":  enabled,
				"disabled": disabled,
				"active":   active,
			},
		})
	}
}

// HandleAddProxy: POST /api/proxies.
// Response returns the created entry WITH the raw password (single reveal
// on create — the operator confirms what they saved).
func HandleAddProxy(pool *proxy.ProxyPool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if pool == nil {
			c.JSON(500, gin.H{"error": "proxy pool not initialised"})
			return
		}
		var in proxyInput
		if err := c.ShouldBindJSON(&in); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON: " + err.Error()})
			return
		}
		entry := proxy.ProxyEntry{
			Protocol:  in.Protocol,
			Host:      in.Host,
			Port:      in.Port,
			Username:  in.Username,
			Password:  in.Password,
			Label:     in.Label,
			Upstreams: in.Upstreams, // nil → validateEntry defaults to ["all"]
			Enabled:   true,         // default enabled on create
		}
		if in.Enabled != nil {
			entry.Enabled = *in.Enabled
		}
		created, err := pool.Add(entry)
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "proxy": created})
	}
}

// HandleUpdateProxy: PUT /api/proxies/:id.
// Password "***" means "keep existing"; any other value overwrites.
func HandleUpdateProxy(pool *proxy.ProxyPool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if pool == nil {
			c.JSON(500, gin.H{"error": "proxy pool not initialised"})
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		if id == "" {
			c.JSON(400, gin.H{"error": "id required"})
			return
		}
		var in proxyInput
		if err := c.ShouldBindJSON(&in); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON: " + err.Error()})
			return
		}
		entry := proxy.ProxyEntry{
			Protocol:  in.Protocol,
			Host:      in.Host,
			Port:      in.Port,
			Username:  in.Username,
			Password:  in.Password,
			Label:     in.Label,
			Upstreams: in.Upstreams, // nil = keep existing (Update honours this)
		}
		if in.Enabled != nil {
			entry.Enabled = *in.Enabled
		} else {
			// Preserve current enabled state — fetch current then set.
			existing, ok := pool.Get(id)
			if !ok {
				c.JSON(404, gin.H{"error": "proxy not found"})
				return
			}
			entry.Enabled = existing.Enabled
		}
		updated, err := pool.Update(id, entry)
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		// Mask password in the response — the create endpoint is the only
		// reveal.
		if updated.Password != "" {
			updated.Password = "***"
		}
		c.JSON(200, gin.H{"ok": true, "proxy": updated})
	}
}

// HandleDeleteProxy: DELETE /api/proxies/:id.
func HandleDeleteProxy(pool *proxy.ProxyPool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if pool == nil {
			c.JSON(500, gin.H{"error": "proxy pool not initialised"})
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		if id == "" {
			c.JSON(400, gin.H{"error": "id required"})
			return
		}
		if err := pool.Delete(id); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "id": id})
	}
}

// HandleToggleProxy: POST /api/proxies/:id/toggle.
func HandleToggleProxy(pool *proxy.ProxyPool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if pool == nil {
			c.JSON(500, gin.H{"error": "proxy pool not initialised"})
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		if id == "" {
			c.JSON(400, gin.H{"error": "id required"})
			return
		}
		enabled, err := pool.Toggle(id)
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "id": id, "enabled": enabled})
	}
}

// HandleTestProxy: POST /api/proxies/:id/test.
// Response: { success: bool, ip?: string, latency_ms: int, error?: string }.
func HandleTestProxy(pool *proxy.ProxyPool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if pool == nil {
			c.JSON(500, gin.H{"error": "proxy pool not initialised"})
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		if id == "" {
			c.JSON(400, gin.H{"error": "id required"})
			return
		}
		ok, ip, latency, err := pool.Test(id)
		resp := gin.H{
			"success":    ok,
			"latency_ms": latency,
		}
		if ip != "" {
			resp["ip"] = ip
		}
		if err != nil {
			resp["error"] = err.Error()
		}
		c.JSON(200, resp)
	}
}
