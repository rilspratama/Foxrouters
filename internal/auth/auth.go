// Package auth handles gateway API-key management, model whitelisting,
// per-key rate/quota metadata, and the Gin middlewares that gate every
// non-public endpoint.
//
// The persistence layer (Redis) is reached via internal/db.  Domain code
// keeps rich types (GatewayKeyInfo / KeyRole); this package converts to
// and from db.GatewayKeyDTO on the way in and out.
package auth

import (
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"foxrouters/internal/db"
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
		if MatchModel(pattern, model) {
			return true
		}
	}
	return false
}

// MatchModel checks if a model matches a pattern (exact or glob suffix).
// Examples: "grok-*" matches "grok-4.5", "cb/*" matches "cb/gpt-5.5",
// "grok-4.5" matches "grok-4.5" exactly.
func MatchModel(pattern, model string) bool {
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

// MaskKey returns a masked version of the key: first 8 + ... + last 4.
func MaskKey(key string) string {
	if len(key) > 12 {
		return key[:8] + "..." + key[len(key)-4:]
	}
	return key
}

// GenerateRandomKey returns a URL-safe random string of n bytes (2n hex chars).
// Uses crypto/rand — suitable for API keys.
func GenerateRandomKey(n int) string {
	b := make([]byte, n)
	if _, err := cryptorand.Read(b); err != nil {
		// crypto/rand failure is unrecoverable — never use a deterministic fallback
		slog.Error("crypto/rand.Read failed (fatal)", "module", "auth", "error", err)
		os.Exit(1)
	}
	return hex.EncodeToString(b)
}

// Manager validates client API keys with per-key metadata + usage tracking.
type Manager struct {
	keys map[string]*GatewayKeyInfo
	mu   sync.RWMutex
	db   *db.Store
}

// NewManager loads keys from Redis (if available), bootstraps from
// GATEWAY_API_KEYS / GATEWAY_KEY_FILE / ./gateway-key.txt if the pool is
// empty, and auto-generates a random admin key on very first boot.
func NewManager(store *db.Store) *Manager {
	am := &Manager{keys: make(map[string]*GatewayKeyInfo), db: store}

	// 1. Load from Redis (single source of truth)
	if store != nil {
		if redisKeys, err := loadGatewayKeys(store); err == nil && len(redisKeys) > 0 {
			for _, info := range redisKeys {
				am.keys[info.Key] = info
			}
			slog.Info("loaded keys from Redis", "module", "auth", "count", len(redisKeys))
		} else if err != nil {
			slog.Warn("Redis load keys failed", "module", "auth", "error", err)
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
					KeyMasked:  MaskKey(k),
					Name:       "bootstrap",
					Role:       RoleAdmin,
					RPM:        0,
					Burst:      0,
					TokenQuota: 0,
					CreatedAt:  time.Now(),
				}
				am.keys[k] = info
				if store != nil {
					saveGatewayKey(store, info)
				}
			}
		}
		if len(bootstrapKeys) > 0 {
			slog.Info("bootstrapped keys from file/env → Redis (first run)", "module", "auth", "count", len(bootstrapKeys))
		}
	}

	if len(am.keys) == 0 {
		if os.Getenv("GATEWAY_AUTH_DISABLE") == "1" {
			slog.Warn("no gateway API keys loaded — auth DISABLED (GATEWAY_AUTH_DISABLE=1)", "module", "auth")
		} else if os.Getenv("GATEWAY_NO_AUTOBOOTSTRAP") == "1" {
			slog.Warn("no gateway API keys loaded — fail-closed mode (auto-bootstrap disabled)", "module", "auth")
		} else {
			// Auto-bootstrap: generate a random admin key on first boot,
			// persist to Redis, print to log ONCE, write to bootstrap-key.txt.
			// User reads log/file, logs in to dashboard, then deletes the file.
			bootstrapKey := "gw-" + GenerateRandomKey(32)
			info := &GatewayKeyInfo{
				Key:        bootstrapKey,
				KeyMasked:  MaskKey(bootstrapKey),
				Name:       "bootstrap",
				Role:       RoleAdmin,
				RPM:        0,
				Burst:      0,
				TokenQuota: 0,
				CreatedAt:  time.Now(),
			}
			am.keys[bootstrapKey] = info
			if store != nil {
				saveGatewayKey(store, info)
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
				slog.Warn("failed to write bootstrap key file", "module", "auth", "error", err)
			}
			port := os.Getenv("PORT")
			if port == "" {
				port = "20130"
			}
			slog.Info("AUTO-BOOTSTRAP: generated admin key (first boot)",
				"module", "auth",
				"key", bootstrapKey,
				"file", bootstrapFile,
				"login_url", "http://localhost:"+port+"/login")
		}
	} else {
		slog.Info("loaded gateway API keys", "module", "auth", "count", len(am.keys))
	}

	return am
}

// NewManagerForTest constructs an empty Manager (no db) suitable for
// whitebox tests that just want to exercise Add/Remove/Valid.
// The optional `keys` map lets callers pre-seed the pool.
func NewManagerForTest(keys map[string]*GatewayKeyInfo) *Manager {
	am := &Manager{keys: make(map[string]*GatewayKeyInfo)}
	for k, v := range keys {
		am.keys[k] = v
	}
	return am
}

// Valid checks if a key is authorized and not disabled.
func (am *Manager) Valid(key string) bool {
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
func (am *Manager) Get(key string) *GatewayKeyInfo {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.keys[key]
}

// LookupKey implements ratelimit.AuthLookup. Returns per-key rpm/burst if
// the key is registered. ok=false means the key isn't in the pool.
func (am *Manager) LookupKey(key string) (rpm, burst int, ok bool) {
	am.mu.RLock()
	defer am.mu.RUnlock()
	info, exists := am.keys[key]
	if !exists {
		return 0, 0, false
	}
	return info.RPM, info.Burst, true
}

// GetAll returns a slice copy of all keys.
func (am *Manager) GetAll() []*GatewayKeyInfo {
	am.mu.RLock()
	defer am.mu.RUnlock()
	result := make([]*GatewayKeyInfo, 0, len(am.keys))
	for _, info := range am.keys {
		result = append(result, info)
	}
	return result
}

// CountAdmins returns the number of admin-role keys (P3-1: last-admin lockout guard).
func (am *Manager) CountAdmins() int {
	am.mu.RLock()
	defer am.mu.RUnlock()
	count := 0
	for _, info := range am.keys {
		if info.Role == RoleAdmin && !info.Disabled {
			count++
		}
	}
	return count
}

// Add creates a new inference-role key with metadata and persists to Redis.
func (am *Manager) Add(key, name string, rpm, burst int, tokenQuota int64) *GatewayKeyInfo {
	return am.AddWithRole(key, name, RoleInference, nil, rpm, burst, tokenQuota)
}

// AddWithRole creates a key with an explicit role and persists to Redis.
func (am *Manager) AddWithRole(key, name string, role KeyRole, allowedModels []string, rpm, burst int, tokenQuota int64) *GatewayKeyInfo {
	if role == "" {
		role = RoleInference
	}
	info := &GatewayKeyInfo{
		Key:           key,
		KeyMasked:     MaskKey(key),
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
		saveGatewayKey(am.db, info)
	}
	return info
}

// Remove deletes a key from memory + Redis.
func (am *Manager) Remove(key string) {
	am.mu.Lock()
	delete(am.keys, key)
	am.mu.Unlock()
	if am.db != nil {
		am.db.DeleteGatewayKey(key)
	}
}

// Update modifies key metadata in memory + Redis.
func (am *Manager) Update(key, name string, role KeyRole, allowedModels []string, rpm, burst int, tokenQuota int64, disabled *bool) bool {
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
	info.KeyMasked = MaskKey(info.Key)
	am.mu.Unlock()
	if am.db != nil {
		saveGatewayKey(am.db, info)
	}
	return true
}

// IncrementTokens adds to the token usage for a key (memory + Redis).
// If quota is set and exceeded, auto-disables the key.
func (am *Manager) IncrementTokens(key string, amount int64) {
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
		slog.Warn("key auto-disabled (quota exceeded)",
			"module", "auth",
			"key", MaskKey(key),
			"tokens_used", info.TokensUsed,
			"token_quota", info.TokenQuota)
	}
	am.mu.Unlock()
	if am.db != nil {
		am.db.IncrementGatewayKeyTokens(key, amount)
		am.db.IncrementGatewayKeyRequests(key)
		if info.Disabled {
			saveGatewayKey(am.db, info)
		}
	}
}

// IncrementRequests adds 1 to the request count for a key.
func (am *Manager) IncrementRequests(key string) {
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

// GenerateGatewayKey creates a new random key: gw- + 32 chars base62.
func GenerateGatewayKey() string {
	b := make([]byte, 24) // 24 bytes → 32 base64 chars
	if _, err := cryptorand.Read(b); err != nil {
		// crypto/rand failure is unrecoverable — never use a predictable fallback
		slog.Error("crypto/rand.Read failed (fatal)", "module", "auth", "error", err)
		os.Exit(1)
	}
	// URL-safe base64, strip padding
	encoded := base64.RawURLEncoding.EncodeToString(b)
	return "gw-" + encoded
}

// ResolveKey resolves a full key from either a full key or a masked key.
func (am *Manager) ResolveKey(keyOrMasked string) (string, bool) {
	am.mu.RLock()
	defer am.mu.RUnlock()
	// Direct lookup
	if _, ok := am.keys[keyOrMasked]; ok {
		return keyOrMasked, true
	}
	// Try masked match
	for k, info := range am.keys {
		if info.KeyMasked == keyOrMasked || MaskKey(k) == keyOrMasked {
			return k, true
		}
	}
	return "", false
}

// Count returns the number of registered keys (thread-safe).
// Used by tests + middleware fail-closed checks.
func (am *Manager) Count() int {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return len(am.keys)
}

// AdminMiddleware gates an endpoint to admin-role keys only.
// Must run AFTER AuthMiddleware (which sets "client_key" in context).
// Endpoints mounting this middleware: /api/keys*, /accounts*, /cb/import,
// /history*, /cb-stats — everything except /v1/* and /health.
func AdminMiddleware(am *Manager) gin.HandlerFunc {
	authDisabled := os.Getenv("GATEWAY_AUTH_DISABLE") == "1"
	return func(c *gin.Context) {
		// If auth is explicitly disabled (dev mode), allow all as admin.
		if authDisabled {
			c.Next()
			return
		}
		fullKey, exists := c.Get("client_key")
		if !exists {
			// No auth context — always reject (defense-in-depth).
			// AuthMiddleware should have already returned 503 when no keys are loaded.
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

// AuthMiddleware validates Bearer tokens (or session cookies) against the
// key pool. Public paths bypass; unauthenticated browser requests to
// text/html endpoints redirect to /login.
func AuthMiddleware(am *Manager, sessionLookup ...func(string) string) gin.HandlerFunc {
	// Fail-closed by default: if no keys are loaded, reject all requests
	// EXCEPT when GATEWAY_AUTH_DISABLE=1 is set explicitly (dev mode).
	authDisabled := os.Getenv("GATEWAY_AUTH_DISABLE") == "1"
	var sl func(string) string
	if len(sessionLookup) > 0 {
		sl = sessionLookup[0]
	}
	return func(c *gin.Context) {
		// Skip auth for public endpoints
		path := c.Request.URL.Path
		if path == "/health" || path == "/" || path == "/dashboard" || path == "/login" || path == "/logout" || path == "/metrics" {
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
			// P3-3: cookie value is a session token, resolve to API key.
			if sl != nil {
				key = sl(ck)
			} else {
				key = ck // legacy: cookie was raw key (pre-P3-3)
			}
		}

		if key == "" {
			// For browser requests (Accept: text/html), redirect to login
			if strings.Contains(c.GetHeader("Accept"), "text/html") && !strings.HasPrefix(path, "/api/") {
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
				cookieSecure := os.Getenv("COOKIE_SECURE") != "0"
				c.SetCookie("foxrouters_session", "", -1, "/", "", cookieSecure, true)
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
		c.Set("client_key_masked", MaskKey(key))
		c.Next()
	}
}

// ---------------------------------------------------------------------------
// Persistence bridge — GatewayKeyInfo ↔ db.GatewayKeyDTO
// ---------------------------------------------------------------------------

// saveGatewayKey persists a GatewayKeyInfo. The caller should own the read
// lock or work on a private copy — we don't grab am.mu here.
func saveGatewayKey(s *db.Store, info *GatewayKeyInfo) {
	if s == nil || info == nil {
		return
	}
	s.SaveGatewayKey(db.GatewayKeyDTO{
		Key:           info.Key,
		Name:          info.Name,
		Role:          string(info.Role),
		AllowedModels: info.AllowedModels,
		RPM:           info.RPM,
		Burst:         info.Burst,
		TokenQuota:    info.TokenQuota,
		TokensUsed:    info.TokensUsed,
		Requests:      info.Requests,
		CreatedAt:     info.CreatedAt,
		Disabled:      info.Disabled,
	})
}

// loadGatewayKeys returns []*GatewayKeyInfo rehydrated from Redis DTOs.
// The role-fallback (empty → RoleAdmin) preserves pre-role-field bootstrap
// key behavior.
func loadGatewayKeys(s *db.Store) ([]*GatewayKeyInfo, error) {
	if s == nil {
		return nil, nil
	}
	dtos, err := s.LoadGatewayKeys()
	if err != nil {
		return nil, err
	}
	out := make([]*GatewayKeyInfo, 0, len(dtos))
	for _, d := range dtos {
		info := &GatewayKeyInfo{
			Key:           d.Key,
			KeyMasked:     MaskKey(d.Key),
			Name:          d.Name,
			Role:          KeyRole(d.Role),
			AllowedModels: append([]string(nil), d.AllowedModels...),
			RPM:           d.RPM,
			Burst:         d.Burst,
			TokenQuota:    d.TokenQuota,
			TokensUsed:    d.TokensUsed,
			Requests:      d.Requests,
			CreatedAt:     d.CreatedAt,
			Disabled:      d.Disabled,
		}
		if info.Role == "" {
			// Least-privilege default: inference only. Only bootstrap keys
			// (explicitly set to RoleAdmin at creation) should be admin.
			// This prevents latent privilege escalation if a persistence bug
			// ever clears the role field (P3 #8 security fix).
			info.Role = RoleInference
		}
		out = append(out, info)
	}
	return out, nil
}
