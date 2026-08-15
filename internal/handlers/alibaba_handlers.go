package handlers

import (
	"net/http"
	"strings"

	"foxrouters/internal/upstream"
	"github.com/gin-gonic/gin"
)

// resolveAliKey accepts either an opaque key_hash (24 hex, from the
// dashboard) or a full sk-ws-* key (direct API use) and resolves it against
// the pool. Empty result means the id does not match any pool entry —
// unknown ids NEVER pass through verbatim (prevents using HandleAliTest as
// a third-party credential oracle).
func resolveAliKey(aliAM *upstream.AlibabaKeyManager, id string) string {
	if id == "" || aliAM == nil {
		return ""
	}
	if len(id) == 24 { // opaque SHA-256 hash (24 hex chars)
		return aliAM.FindKeyByHash(id)
	}
	if k := aliAM.Get(id); k != nil {
		return k.Key
	}
	return ""
}

// HandleAliAccounts lists the pool. Never exposes full keys — only masked
// display string + opaque hash.
// GET /ali/accounts
func HandleAliAccounts(aliAM *upstream.AlibabaKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		accs := aliAM.ListAccounts()
		c.JSON(http.StatusOK, gin.H{"accounts": accs, "total": len(accs)})
	}
}

// HandleAliDeleteAccount removes a key (rule: disable, never delete — this
// is the operator's last resort). Key goes in the POST body, NOT the URL
// path (a full sk-ws-* secret must never hit gin's access log).
// POST /ali/accounts/delete {"key_hash":"…"}
func HandleAliDeleteAccount(aliAM *upstream.AlibabaKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			KeyHash string `json:"key_hash"`
			Key     string `json:"key"` // optional direct full-key
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		id := body.KeyHash
		if id == "" {
			id = body.Key
		}
		key := resolveAliKey(aliAM, id)
		if key == "" {
			c.JSON(http.StatusNotFound, gin.H{"error": "key not found"})
			return
		}
		if err := aliAM.RemoveAccount(key); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "key not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"deleted": true})
	}
}

// HandleAliDisable disables a key (rule: disable, never delete).
// POST /ali/keys/disable {"key_hash":"…","reason":"…"}
func HandleAliDisable(aliAM *upstream.AlibabaKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			KeyHash string `json:"key_hash"`
			Key     string `json:"key"`
			Reason  string `json:"reason"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		id := body.KeyHash
		if id == "" {
			id = body.Key
		}
		key := resolveAliKey(aliAM, id)
		if key == "" {
			c.JSON(http.StatusNotFound, gin.H{"error": "key not found"})
			return
		}
		if err := aliAM.DisableKey(key, body.Reason); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "key not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"disabled": true})
	}
}

// HandleAliEnable re-enables a key.
// POST /ali/keys/enable {"key_hash":"…"}
func HandleAliEnable(aliAM *upstream.AlibabaKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			KeyHash string `json:"key_hash"`
			Key     string `json:"key"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		id := body.KeyHash
		if id == "" {
			id = body.Key
		}
		key := resolveAliKey(aliAM, id)
		if key == "" {
			c.JSON(http.StatusNotFound, gin.H{"error": "key not found"})
			return
		}
		if err := aliAM.EnableKey(key); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "key not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"enabled": true})
	}
}

// HandleAliTest probes a single key with a minimal chat request
// (qwen3.8-max "Say OK"). Disabled keys are still probed (operator check).
// POST /ali/keys/test {"key_hash":"…"}
func HandleAliTest(aliAM *upstream.AlibabaKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			KeyHash string `json:"key_hash"`
			Key     string `json:"key"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		id := body.KeyHash
		if id == "" {
			id = body.Key
		}
		key := resolveAliKey(aliAM, id)
		if key == "" {
			c.JSON(http.StatusNotFound, gin.H{"error": "key not found"})
			return
		}
		resp, err := upstream.TestAlibabaKey(key)
		if err != nil {
			// Never leak upstream internals (internal hostnames, proxy
			// pool addresses) back to the client — log server-side.
			c.JSON(http.StatusBadGateway, gin.H{"ok": false, "error": "upstream probe failed"})
			return
		}
		c.JSON(http.StatusOK, resp)
	}
}

// maskKey shortens a full sk-ws-* key for display (sk-ws-…abcd).
func maskKey(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:8] + "…" + key[len(key)-4:]
}

// HandleAliImport adds a single DashScope API key.
// POST /ali/import {"key":"sk-ws-…","email":"…"}
func HandleAliImport(aliAM *upstream.AlibabaKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Key   string `json:"key" binding:"required"`
			Email string `json:"email"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Key) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key is required"})
			return
		}
		key := strings.TrimSpace(req.Key)
		if !strings.HasPrefix(key, "sk-") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid key format (expected sk-…)"})
			return
		}
		added, total, err := aliAM.AddAccount(key, req.Email)
		if err != nil {
			// Generic message — never leak Redis/internal error detail.
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to add key"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"added": added, "total": total})
	}
}

// HandleAliImportBulk imports multiple DashScope keys at once.
// POST /ali/import/bulk {"keys":["sk-…",…]} or {"raw":"sk-…\nsk-…\n"}.
// Raw also accepts api_keys.txt lines "email | password | key | ts" — the
// third field (sk-ws-*) is extracted and the email (field 0) is kept.
// Capped at 500 keys per batch — a 1MB body ≈ 15k keys would hold the pool
// mutex for minutes (O(n²) sort + per-key Redis round-trip).
const aliBulkImportCap = 500

func HandleAliImportBulk(aliAM *upstream.AlibabaKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Keys []string `json:"keys"`
			Raw  string   `json:"raw"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
			return
		}
		type aliEntry struct {
			key   string
			email string
		}
		var entries []aliEntry
		if len(req.Keys) > 0 {
			for _, k := range req.Keys {
				k = strings.TrimSpace(k)
				if strings.HasPrefix(k, "sk-") {
					entries = append(entries, aliEntry{key: k})
				}
			}
		} else if strings.TrimSpace(req.Raw) != "" {
			for _, line := range strings.Split(req.Raw, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				for _, part := range strings.Split(line, ",") {
					part = strings.TrimSpace(part)
					if part == "" {
						continue
					}
					// "email | password | sk-ws-… | ts" → key = field 2, email = field 0
					if f := strings.Split(part, "|"); len(f) >= 3 && strings.HasPrefix(strings.TrimSpace(f[2]), "sk-") {
						email := strings.TrimSpace(f[0])
						if !strings.Contains(email, "@") {
							email = ""
						}
						entries = append(entries, aliEntry{key: strings.TrimSpace(f[2]), email: email})
						continue
					}
					if strings.HasPrefix(part, "sk-") {
						entries = append(entries, aliEntry{key: part})
					}
				}
			}
		}
		if len(entries) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no valid keys provided"})
			return
		}
		truncated := false
		if len(entries) > aliBulkImportCap {
			entries = entries[:aliBulkImportCap]
			truncated = true
		}
		added, failed := 0, 0
		for _, e := range entries {
			_, _, err := aliAM.AddAccount(e.key, e.email)
			if err != nil {
				failed++
			} else {
				added++
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"added":     added,
			"failed":    failed,
			"total":     len(entries),
			"truncated": truncated,
		})
	}
}
