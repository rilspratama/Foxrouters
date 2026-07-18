package main

import (
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// KeyRole controls what a gateway key can access.
//   - "inference": only /v1/* (chat completions, models) — the default for new keys
//   - "admin":     everything (accounts, keys, history, cb-stats, imports)
type KeyRole string

const (
	RoleInference KeyRole = "inference"
	RoleAdmin     KeyRole = "admin"
)

// GatewayKeyInfo holds metadata + usage for a gateway API key.
type GatewayKeyInfo struct {
	Key           string    `json:"key"`            // full key (only returned on create)
	KeyMasked     string    `json:"key_masked"`     // gw-zUkr...DrrpE
	Name          string    `json:"name"`           // human label
	Role          KeyRole   `json:"role"`           // inference | admin
	AllowedModels []string  `json:"allowed_models"` // model whitelist (empty = all models allowed)
	RPM           int       `json:"rpm"`            // requests per minute limit (0 = unlimited)
	Burst         int       `json:"burst"`          // burst size (0 = use RPM)
	TokenQuota    int64     `json:"token_quota"`    // total token quota (0 = unlimited)
	TokensUsed    int64     `json:"tokens_used"`    // tokens consumed
	Requests      int64     `json:"requests"`       // total requests
	CreatedAt     time.Time `json:"created_at"`
	Disabled      bool      `json:"disabled"`
}

// IsModelAllowed checks if a model is in the key's allowed_models whitelist.
// Supports glob suffixes: "grok-*" matches "grok-4.5", "cb/*" matches "cb/gpt-5.5".
// Empty AllowedModels = all models allowed (backward compat).
func (k *GatewayKeyInfo) IsModelAllowed(model string) bool {
	if len(k.AllowedModels) == 0 {
		return true // no whitelist = unrestricted
	}
	for _, pattern := range k.AllowedModels {
		if matchModel(pattern, model) {
			return true
		}
	}
	return false
}

// matchModel checks if a model matches a pattern (exact or glob suffix).
// Examples: "grok-*" matches "grok-4.5", "cb/*" matches "cb/gpt-5.5",
// "grok-4.5" matches "grok-4.5" exactly.
func matchModel(pattern, model string) bool {
	if pattern == model {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(model, prefix)
	}
	// Handle "cb/*" style: strip "/*", check prefix "cb/"
	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "/*")
		return strings.HasPrefix(model, prefix+"/")
	}
	return false
}

// maskKey returns a masked version of the key: first 8 + ... + last 4.
func maskKey(key string) string {
	if len(key) > 12 {
		return key[:8] + "..." + key[len(key)-4:]
	}
	return key
}

// generateRandomKey returns a URL-safe random string of n bytes (2n hex chars).
// Uses crypto/rand — suitable for API keys.
func generateRandomKey(n int) string {
	b := make([]byte, n)
	if _, err := cryptorand.Read(b); err != nil {
		// Fallback: shouldn't happen, but fail safe
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b)
}

// AuthManager validates client API keys with per-key metadata + usage tracking.
type AuthManager struct {
	keys map[string]*GatewayKeyInfo
	mu   sync.RWMutex
	db   *DBStore
}

func newAuthManager(db *DBStore) *AuthManager {
	am := &AuthManager{keys: make(map[string]*GatewayKeyInfo), db: db}

	// 1. Load from Redis (single source of truth)
	if db != nil {
		if redisKeys, err := db.LoadGatewayKeys(); err == nil && len(redisKeys) > 0 {
			for _, info := range redisKeys {
				am.keys[info.Key] = info
			}
			log.Printf("[auth] loaded %d keys from Redis", len(redisKeys))
		} else if err != nil {
			log.Printf("[auth] warn: Redis load keys: %v", err)
		}
	}

	// 2. If Redis is empty (fresh deploy), bootstrap from file/env
	//    then persist to Redis so future starts are file-independent.
	if len(am.keys) == 0 {
		bootstrapKeys := []string{}
		if envKeysStr := os.Getenv("GATEWAY_API_KEYS"); envKeysStr != "" {
			for _, k := range strings.FieldsFunc(envKeysStr, func(r rune) bool { return r == ',' || r == '\n' }) {
				k = strings.TrimSpace(k)
				if k != "" {
					bootstrapKeys = append(bootstrapKeys, k)
				}
			}
		}
		if keyFile := os.Getenv("GATEWAY_KEY_FILE"); keyFile != "" {
			if data, err := os.ReadFile(keyFile); err == nil {
				for _, k := range strings.FieldsFunc(string(data), func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
					k = strings.TrimSpace(k)
					if k != "" {
						bootstrapKeys = append(bootstrapKeys, k)
					}
				}
			}
		} else {
			if data, err := os.ReadFile("./gateway-key.txt"); err == nil {
				for _, k := range strings.FieldsFunc(string(data), func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
					k = strings.TrimSpace(k)
					if k != "" {
						bootstrapKeys = append(bootstrapKeys, k)
					}
				}
			}
		}
		for _, k := range bootstrapKeys {
			if _, exists := am.keys[k]; !exists {
				info := &GatewayKeyInfo{
					Key:        k,
					KeyMasked:  maskKey(k),
					Name:       "bootstrap",
					Role:       RoleAdmin,
					RPM:        0,
					Burst:      0,
					TokenQuota: 0,
					CreatedAt:  time.Now(),
				}
				am.keys[k] = info
				if db != nil {
					db.SaveGatewayKey(info)
				}
			}
		}
		if len(bootstrapKeys) > 0 {
			log.Printf("[auth] bootstrapped %d keys from file/env → Redis (first run)", len(bootstrapKeys))
		}
	}

	if len(am.keys) == 0 {
		if os.Getenv("GATEWAY_AUTH_DISABLE") == "1" {
			log.Printf("[auth] WARNING: no gateway API keys loaded — auth DISABLED (GATEWAY_AUTH_DISABLE=1)")
		} else if os.Getenv("GATEWAY_NO_AUTOBOOTSTRAP") == "1" {
			log.Printf("[auth] WARNING: no gateway API keys loaded — fail-closed mode (auto-bootstrap disabled via GATEWAY_NO_AUTOBOOTSTRAP=1)")
		} else {
			// Auto-bootstrap: generate a random admin key on first boot,
			// persist to Redis, print to log ONCE, write to bootstrap-key.txt.
			// User reads log/file, logs in to dashboard, then deletes the file.
			bootstrapKey := "gw-" + generateRandomKey(32)
			info := &GatewayKeyInfo{
				Key:        bootstrapKey,
				KeyMasked:  maskKey(bootstrapKey),
				Name:       "bootstrap",
				Role:       RoleAdmin,
				RPM:        0,
				Burst:      0,
				TokenQuota: 0,
				CreatedAt:  time.Now(),
			}
			am.keys[bootstrapKey] = info
			if db != nil {
				db.SaveGatewayKey(info)
			}
			// Write to bootstrap-key.txt (chmod 600) so user can retrieve it.
			// File is gitignored. Delete after first login.
			bootstrapFile := "bootstrap-key.txt"
			if p := os.Getenv("GATEWAY_KEY_FILE"); p != "" {
				bootstrapFile = p
			}
			// Resolve to absolute path (relative to CWD) for reliable access
			if absPath, err := filepath.Abs(bootstrapFile); err == nil {
				bootstrapFile = absPath
			}
			if err := os.WriteFile(bootstrapFile, []byte(bootstrapKey+"\n"), 0600); err != nil {
				log.Printf("[auth] WARNING: failed to write bootstrap key file: %v", err)
			}
			log.Printf("╔══════════════════════════════════════════════════════════════╗")
			log.Printf("║  [auth] AUTO-BOOTSTRAP: generated admin key (first boot)     ║")
			log.Printf("║  Key: %s", bootstrapKey)
			log.Printf("║  Saved to: %s (chmod 600 — delete after first login)", bootstrapFile)
			port := os.Getenv("PORT")
			if port == "" {
				port = "20130"
			}
			log.Printf("║  Login at: http://localhost:%s/login", port)
			log.Printf("╚══════════════════════════════════════════════════════════════╝")
		}
	} else {
		log.Printf("[auth] loaded %d gateway API keys", len(am.keys))
	}

	return am
}

// Valid checks if a key is authorized and not disabled.
func (am *AuthManager) Valid(key string) bool {
	am.mu.RLock()
	defer am.mu.RUnlock()
	// Fail-closed: no keys = reject. Auth bypass only via GATEWAY_AUTH_DISABLE env
	// (handled in AuthMiddleware, not here).
	if len(am.keys) == 0 {
		return false
	}
	info, ok := am.keys[key]
	return ok && !info.Disabled
}

// Get returns the GatewayKeyInfo for a key, or nil.
func (am *AuthManager) Get(key string) *GatewayKeyInfo {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.keys[key]
}

// GetAll returns a slice copy of all keys.
func (am *AuthManager) GetAll() []*GatewayKeyInfo {
	am.mu.RLock()
	defer am.mu.RUnlock()
	result := make([]*GatewayKeyInfo, 0, len(am.keys))
	for _, info := range am.keys {
		result = append(result, info)
	}
	return result
}

// Add creates a new inference-role key with metadata and persists to Redis.
func (am *AuthManager) Add(key, name string, rpm, burst int, tokenQuota int64) *GatewayKeyInfo {
	return am.AddWithRole(key, name, RoleInference, nil, rpm, burst, tokenQuota)
}

// AddWithRole creates a key with an explicit role and persists to Redis.
func (am *AuthManager) AddWithRole(key, name string, role KeyRole, allowedModels []string, rpm, burst int, tokenQuota int64) *GatewayKeyInfo {
	if role == "" {
		role = RoleInference
	}
	info := &GatewayKeyInfo{
		Key:           key,
		KeyMasked:     maskKey(key),
		Name:          name,
		Role:          role,
		AllowedModels: allowedModels,
		RPM:           rpm,
		Burst:         burst,
		TokenQuota:    tokenQuota,
		CreatedAt:     time.Now(),
	}
	am.mu.Lock()
	am.keys[key] = info
	am.mu.Unlock()
	if am.db != nil {
		am.db.SaveGatewayKey(info)
	}
	return info
}

// Remove deletes a key from memory + Redis.
func (am *AuthManager) Remove(key string) {
	am.mu.Lock()
	delete(am.keys, key)
	am.mu.Unlock()
	if am.db != nil {
		am.db.DeleteGatewayKey(key)
	}
}

// Update modifies key metadata in memory + Redis.
func (am *AuthManager) Update(key, name string, role KeyRole, allowedModels []string, rpm, burst int, tokenQuota int64, disabled *bool) bool {
	am.mu.Lock()
	info, ok := am.keys[key]
	if !ok {
		am.mu.Unlock()
		return false
	}
	if name != "" {
		info.Name = name
	}
	if role == RoleAdmin || role == RoleInference {
		info.Role = role
	}
	if allowedModels != nil {
		info.AllowedModels = allowedModels
	}
	if rpm >= 0 {
		info.RPM = rpm
	}
	if burst >= 0 {
		info.Burst = burst
	}
	if tokenQuota >= 0 {
		info.TokenQuota = tokenQuota
	}
	if disabled != nil {
		info.Disabled = *disabled
	}
	info.KeyMasked = maskKey(info.Key)
	am.mu.Unlock()
	if am.db != nil {
		am.db.SaveGatewayKey(info)
	}
	return true
}

// IncrementTokens adds to the token usage for a key (memory + Redis).
// If quota is set and exceeded, auto-disables the key.
func (am *AuthManager) IncrementTokens(key string, amount int64) {
	am.mu.Lock()
	info, ok := am.keys[key]
	if !ok {
		am.mu.Unlock()
		return
	}
	info.TokensUsed += amount
	info.Requests++
	// Auto-disable if quota exceeded
	if info.TokenQuota > 0 && info.TokensUsed >= info.TokenQuota {
		info.Disabled = true
		log.Printf("[auth] key %s auto-disabled (quota: %d/%d)", maskKey(key), info.TokensUsed, info.TokenQuota)
	}
	am.mu.Unlock()
	if am.db != nil {
		am.db.IncrementGatewayKeyTokens(key, amount)
		am.db.IncrementGatewayKeyRequests(key)
		if info.Disabled {
			am.db.SaveGatewayKey(info)
		}
	}
}

// IncrementRequests adds 1 to the request count for a key.
func (am *AuthManager) IncrementRequests(key string) {
	am.mu.Lock()
	info, ok := am.keys[key]
	if !ok {
		am.mu.Unlock()
		return
	}
	info.Requests++
	am.mu.Unlock()
	if am.db != nil {
		am.db.IncrementGatewayKeyRequests(key)
	}
}

// generateGatewayKey creates a new random key: gw- + 32 chars base62.
func generateGatewayKey() string {
	b := make([]byte, 24) // 24 bytes → 32 base64 chars
	if _, err := cryptorand.Read(b); err != nil {
		// Fallback (should never happen)
		return "gw-" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	// URL-safe base64, strip padding
	encoded := base64.RawURLEncoding.EncodeToString(b)
	return "gw-" + encoded
}

// resolveKey resolves a full key from either a full key or a masked key.
func (am *AuthManager) resolveKey(keyOrMasked string) (string, bool) {
	am.mu.RLock()
	defer am.mu.RUnlock()
	// Direct lookup
	if _, ok := am.keys[keyOrMasked]; ok {
		return keyOrMasked, true
	}
	// Try masked match
	for k, info := range am.keys {
		if info.KeyMasked == keyOrMasked || maskKey(k) == keyOrMasked {
			return k, true
		}
	}
	return "", false
}

// AdminMiddleware gates an endpoint to admin-role keys only.
// Must run AFTER AuthMiddleware (which sets "client_key" in context).
// Endpoints mounting this middleware: /api/keys*, /accounts*, /cb/import,
// /history*, /cb-stats — everything except /v1/* and /health.
func AdminMiddleware(am *AuthManager) gin.HandlerFunc {
	authDisabled := os.Getenv("GATEWAY_AUTH_DISABLE") == "1"
	return func(c *gin.Context) {
		// If auth is explicitly disabled (dev mode), allow all as admin.
		if authDisabled {
			c.Next()
			return
		}
		fullKey, exists := c.Get("client_key")
		if !exists {
			// No auth context — either auth is disabled (no keys) or a bug.
			// When no keys loaded (fresh deploy), treat as admin (dev fallback).
			am.mu.RLock()
			noKeys := len(am.keys) == 0
			am.mu.RUnlock()
			if noKeys {
				c.Next()
				return
			}
			c.JSON(401, gin.H{"error": "authentication required"})
			c.Abort()
			return
		}
		info := am.Get(fullKey.(string))
		if info == nil {
			c.JSON(401, gin.H{"error": "invalid API key"})
			c.Abort()
			return
		}
		if info.Role != RoleAdmin {
			c.JSON(403, gin.H{"error": "admin role required for this endpoint"})
			c.Abort()
			return
		}
		c.Next()
	}
}

func AuthMiddleware(am *AuthManager) gin.HandlerFunc {
	// Fail-closed by default: if no keys are loaded, reject all requests
	// EXCEPT when GATEWAY_AUTH_DISABLE=1 is set explicitly (dev mode).
	authDisabled := os.Getenv("GATEWAY_AUTH_DISABLE") == "1"
	return func(c *gin.Context) {
		// Skip auth for public endpoints
		path := c.Request.URL.Path
		if path == "/health" || path == "/" || path == "/dashboard" || path == "/login" || path == "/logout" {
			c.Next()
			return
		}

		// If auth is explicitly disabled (dev mode), allow all.
		if authDisabled {
			c.Next()
			return
		}

		// Fail-closed: no keys loaded = reject (not "allow all").
		// Must hold RLock — concurrent Add/Remove/Update mutate the map.
		am.mu.RLock()
		noKeys := len(am.keys) == 0
		am.mu.RUnlock()
		if noKeys {
			c.JSON(503, gin.H{"error": "no gateway API keys configured — set GATEWAY_KEY_FILE or GATEWAY_AUTH_DISABLE=1 for dev"})
			c.Abort()
			return
		}

		// Extract key: try Bearer token first, then fall back to session cookie
		var key string
		auth := c.GetHeader("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			key = strings.TrimPrefix(auth, "Bearer ")
		} else if ck, err := c.Cookie("foxrouters_session"); err == nil && ck != "" {
			key = ck
		}

		if key == "" {
			// For browser requests (Accept: text/html), redirect to login
			if strings.Contains(c.GetHeader("Accept"), "text/html") && path != "/api/" {
				c.Redirect(302, "/login")
				c.Abort()
				return
			}
			c.JSON(401, gin.H{"error": "missing or invalid Authorization header (expected Bearer <key>)"})
			c.Abort()
			return
		}
		if !am.Valid(key) {
			// Invalid cookie session → clear it and redirect to login
			if _, err := c.Cookie("foxrouters_session"); err == nil {
				c.SetCookie("foxrouters_session", "", -1, "/", "", false, true)
				if strings.Contains(c.GetHeader("Accept"), "text/html") {
					c.Redirect(302, "/login")
					c.Abort()
					return
				}
			}
			c.JSON(401, gin.H{"error": "invalid API key"})
			c.Abort()
			return
		}
		// Store FULL key for rate limiter + token tracking; mask only for display/logging
		c.Set("client_key", key)
		c.Set("client_key_masked", maskKey(key))
		c.Next()
	}
}
