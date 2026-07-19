// Package handlers — custom models + aliases admin API (v1.3.0).
//
// All six handlers are admin-only (wire them under AdminMiddleware).
// State lives in Redis and is cached in memory via *proxy.CustomRegistry.
// Mutations go DB-first, cache-second so a Redis error surfaces before the
// in-memory view diverges from persistent storage.
package handlers

import (
	"foxrouters/internal/db"
	"foxrouters/internal/proxy"
	"strings"

	"github.com/gin-gonic/gin"
)

// customModelInput is the wire schema for POST /api/models/custom.
type customModelInput struct {
	ID        string `json:"id"`
	Upstream  string `json:"upstream"`
	ModelName string `json:"model_name"`
	OwnedBy   string `json:"owned_by"`
}

// aliasInput is the wire schema for POST /api/aliases.
type aliasInput struct {
	Alias  string `json:"alias"`
	Target string `json:"target"`
}

// HandleListCustomModels: GET /api/models/custom → { models: {id: {...}} }.
func HandleListCustomModels(reg *proxy.CustomRegistry) gin.HandlerFunc {
	return func(c *gin.Context) {
		if reg == nil {
			c.JSON(500, gin.H{"error": "custom registry not initialised"})
			return
		}
		c.JSON(200, gin.H{"models": reg.SnapshotModels()})
	}
}

// HandleAddCustomModel: POST /api/models/custom { id, upstream, model_name, owned_by? }.
func HandleAddCustomModel(reg *proxy.CustomRegistry) gin.HandlerFunc {
	return func(c *gin.Context) {
		if reg == nil {
			c.JSON(500, gin.H{"error": "custom registry not initialised"})
			return
		}
		var in customModelInput
		if err := c.ShouldBindJSON(&in); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON: " + err.Error()})
			return
		}
		cm := db.CustomModel{
			Upstream:  in.Upstream,
			ModelName: in.ModelName,
			OwnedBy:   in.OwnedBy,
		}
		if err := reg.AddModel(in.ID, cm); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "id": in.ID})
	}
}

// HandleDeleteCustomModel: DELETE /api/models/custom/:id
// Note: id may contain a slash (e.g. "cb/kimi-k3") — route as /*id catch-all.
func HandleDeleteCustomModel(reg *proxy.CustomRegistry) gin.HandlerFunc {
	return func(c *gin.Context) {
		if reg == nil {
			c.JSON(500, gin.H{"error": "custom registry not initialised"})
			return
		}
		id := c.Param("id")
		// gin's *param captures with a leading slash — strip it.
		if len(id) > 0 && id[0] == '/' {
			id = id[1:]
		}
		if id == "" {
			c.JSON(400, gin.H{"error": "id required"})
			return
		}
		if err := reg.DeleteModel(id); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "id": id})
	}
}

// HandleListAliases: GET /api/aliases → { aliases: {alias: target} }.
func HandleListAliases(reg *proxy.CustomRegistry) gin.HandlerFunc {
	return func(c *gin.Context) {
		if reg == nil {
			c.JSON(500, gin.H{"error": "custom registry not initialised"})
			return
		}
		c.JSON(200, gin.H{"aliases": reg.SnapshotAliases()})
	}
}

// HandleAddAlias: POST /api/aliases { alias, target }.
func HandleAddAlias(reg *proxy.CustomRegistry) gin.HandlerFunc {
	return func(c *gin.Context) {
		if reg == nil {
			c.JSON(500, gin.H{"error": "custom registry not initialised"})
			return
		}
		var in aliasInput
		if err := c.ShouldBindJSON(&in); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON: " + err.Error()})
			return
		}
		if err := reg.AddAlias(in.Alias, in.Target); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "alias": in.Alias, "target": in.Target})
	}
}

// HandleDeleteAlias: DELETE /api/aliases/:alias.
func HandleDeleteAlias(reg *proxy.CustomRegistry) gin.HandlerFunc {
	return func(c *gin.Context) {
		if reg == nil {
			c.JSON(500, gin.H{"error": "custom registry not initialised"})
			return
		}
		alias := strings.TrimPrefix(c.Param("alias"), "/")
		if alias == "" {
			c.JSON(400, gin.H{"error": "alias required"})
			return
		}
		if err := reg.DeleteAlias(alias); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "alias": alias})
	}
}
