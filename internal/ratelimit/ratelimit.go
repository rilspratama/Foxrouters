// Package ratelimit provides per-client token-bucket rate limiting plus a
// couple of small companion middlewares (security headers, request ID, gzip)
// that don't really belong anywhere else.
//
// The rate limiter accepts an AuthLookup interface so it doesn't have to
// depend on the concrete AuthManager type. Any type providing
// LookupKey(key) → (rpm, burst, ok) can plug in.
package ratelimit

import (
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// AuthLookup is the tiny slice of AuthManager that the rate-limit middleware
// needs. Kept as an interface so this package can live in internal/ without
// pulling in the auth package (which would create a cycle once auth also
// wants to log through internal/db, etc).
//
// LookupKey returns per-key overrides: rpm and burst.
//   - ok=false  → key not registered / no override, fall back to global limits.
//   - rpm == 0  → key is explicitly unlimited (bypass rate limit entirely).
//   - rpm  > 0  → use this rpm (and burst if > 0).
type AuthLookup interface {
	LookupKey(key string) (rpm, burst int, ok bool)
}

// SecurityHeadersMiddleware adds security headers to all responses.
// Protects dashboard from clickjacking, MIME sniffing, and limits XSS impact.
func SecurityHeadersMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		// CSP: allow self + inline styles/scripts (dashboard SPA) + Google Fonts
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'unsafe-inline'; "+
				"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; "+
				"font-src 'self' https://fonts.gstatic.com; "+
				"connect-src 'self'; "+
				"img-src 'self' data:; "+
				"frame-ancestors 'none'")
		c.Next()
	}
}

// RequestIDMiddleware generates a unique ID per request (or honors inbound X-Request-ID),
// sets it on the response header, and stores it in the gin context for logging.
// Format: 8-byte hex (16 chars) — short enough for logs, unique enough for correlation.
func RequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader("X-Request-ID")
		if rid == "" {
			b := make([]byte, 8)
			if _, err := rand.Read(b); err != nil {
				rid = "0000000000000000"
			} else {
				rid = hex.EncodeToString(b)
			}
		}
		c.Writer.Header().Set("X-Request-ID", rid)
		c.Set("request_id", rid)
		c.Next()
	}
}

// GzipMiddleware compresses responses for clients that accept gzip.
// Skips SSE / chat paths — gzip buffering breaks streaming.
func GzipMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
			c.Next()
			return
		}
		accept := c.GetHeader("Accept")
		if strings.Contains(accept, "text/event-stream") {
			c.Next()
			return
		}
		if c.Request.URL.Path == "/v1/chat/completions" {
			// Chat completions may stream — skip gzip.
			c.Next()
			return
		}
		c.Header("Content-Encoding", "gzip")
		c.Header("Vary", "Accept-Encoding")
		gz := gzip.NewWriter(c.Writer)
		c.Writer = &gzipResponseWriter{ResponseWriter: c.Writer, writer: gz}
		c.Next()
		gz.Close()
	}
}

type gzipResponseWriter struct {
	gin.ResponseWriter
	writer *gzip.Writer
}

func (w *gzipResponseWriter) Write(data []byte) (int, error) {
	return w.writer.Write(data)
}

func (w *gzipResponseWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

// Limiter implements per-client token-bucket rate limiting.
type Limiter struct {
	limit   int
	burst   int
	window  time.Duration
	clients map[string]*clientBucket
	mu      sync.Mutex
}

type clientBucket struct {
	tokens   float64
	lastTime time.Time
}

// New creates a new limiter with the given global limit/burst/window.
// A background goroutine cleans up idle clients every 5 minutes.
func New(limit, burst int, window time.Duration) *Limiter {
	rl := &Limiter{
		clients: make(map[string]*clientBucket),
		limit:   limit,
		burst:   burst,
		window:  window,
	}
	go rl.cleanup()
	return rl
}

// Allow checks if a client is within the global rate limit.
func (rl *Limiter) Allow(clientID string) bool {
	return rl.AllowWithLimit(clientID, rl.limit, rl.burst)
}

// AllowWithLimit checks if a client is within a specific RPM/burst limit.
// rpm <= 0 → allow unlimited.
func (rl *Limiter) AllowWithLimit(clientID string, rpm, burst int) bool {
	if rpm <= 0 {
		return true
	}
	if burst <= 0 {
		burst = rpm
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	bucket, exists := rl.clients[clientID]
	if !exists {
		rl.clients[clientID] = &clientBucket{
			tokens:   float64(burst) - 1,
			lastTime: now,
		}
		return true
	}

	elapsed := now.Sub(bucket.lastTime)
	refill := elapsed.Seconds() * float64(rpm) / rl.window.Seconds()
	bucket.tokens += refill
	if bucket.tokens > float64(burst) {
		bucket.tokens = float64(burst)
	}
	bucket.lastTime = now

	if bucket.tokens >= 1 {
		bucket.tokens -= 1
		return true
	}
	return false
}

func (rl *Limiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		cutoff := time.Now().Add(-10 * time.Minute)
		for id, bucket := range rl.clients {
			if bucket.lastTime.Before(cutoff) {
				delete(rl.clients, id)
			}
		}
		rl.mu.Unlock()
	}
}

// Middleware applies per-client rate limiting with per-key overrides.
// am may be nil (falls back to global limits + client IP as the client id).
func Middleware(rl *Limiter, am AuthLookup) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if path == "/health" || path == "/" || path == "/dashboard" {
			c.Next()
			return
		}

		clientID := c.ClientIP()
		fullKey := ""
		if key, exists := c.Get("client_key"); exists {
			clientID = key.(string)
			fullKey = key.(string)
		}

		rpm := rl.limit
		burst := rl.burst
		if fullKey != "" && am != nil {
			if kRPM, kBurst, ok := am.LookupKey(fullKey); ok {
				if kRPM == 0 {
					// RPM=0 means unlimited — bypass rate limit entirely
					c.Next()
					return
				}
				if kRPM > 0 {
					rpm = kRPM
				}
				if kBurst > 0 {
					burst = kBurst
				}
			}
		}

		if !rl.AllowWithLimit(clientID, rpm, burst) {
			c.JSON(429, gin.H{
				"error": "rate limit exceeded",
				"hint":  fmt.Sprintf("limit: %d req/%s, burst: %d", rpm, rl.window, burst),
			})
			c.Abort()
			return
		}
		c.Next()
	}
}
