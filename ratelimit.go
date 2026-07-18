package main

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

// securityHeadersMiddleware adds security headers to all responses.
// Protects dashboard from clickjacking, MIME sniffing, and limits XSS impact.
func securityHeadersMiddleware() gin.HandlerFunc {
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

// requestIDMiddleware generates a unique ID per request (or honors inbound X-Request-ID),
// sets it on the response header, and stores it in the gin context for logging.
// Format: 8-byte hex (16 chars) — short enough for logs, unique enough for correlation.
func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Honor inbound X-Request-ID if provided (e.g. from upstream load balancer)
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

// gzipMiddleware compresses responses for clients that accept gzip.
// Creates the gzip.Writer once, closes it once after the handler finishes.
// Skips SSE / chat paths — gzip buffering breaks streaming.
func gzipMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
			c.Next()
			return
		}
		// Skip streaming requests — gzip buffers output and breaks SSE.
		// The OpenAI SDK sends stream:true in the JSON body, not Accept:
		// text/event-stream, so we must check the request body too.
		// The proxyGrok/proxyCodeBuddy handlers set Content-Type to
		// text/event-stream on the RESPONSE, but by then gzip middleware
		// has already wrapped the writer. So we check the Accept header
		// OR the stream flag in the body.
		accept := c.GetHeader("Accept")
		if strings.Contains(accept, "text/event-stream") {
			c.Next()
			return
		}
		// Also skip for /v1/chat/completions with stream=true (OpenAI SDK
		// sends Accept: application/json even for streaming requests)
		if c.Request.URL.Path == "/v1/chat/completions" {
			// Peek at body to check stream flag — but we can't re-read body.
			// Instead, just skip gzip for ALL chat completions requests
			// since they may be streaming. Non-stream responses are small
			// enough that gzip doesn't matter.
			c.Next()
			return
		}
		// Create writer once, close once after handler completes.
		// Old bug: defer Close() inside Write() closed the stream after
		// the first Write, corrupting multi-chunk JSON responses.
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

// ClientRateLimiter implements per-client sliding window rate limiting.
type ClientRateLimiter struct {
	limit   int
	burst   int
	window  time.Duration
	clients map[string]*clientBucket
	mu      sync.Mutex
	db      *DBStore // optional: for logging rate-limited requests
}

type clientBucket struct {
	tokens   float64
	lastTime time.Time
}

func newRateLimiter(limit, burst int, window time.Duration) *ClientRateLimiter {
	rl := &ClientRateLimiter{
		clients: make(map[string]*clientBucket),
		limit:   limit,
		burst:   burst,
		window:  window,
	}
	// Cleanup stale entries every 5 minutes
	go rl.cleanup()
	return rl
}

// Allow checks if a client is within rate limit (global default). Returns true if allowed.
func (rl *ClientRateLimiter) Allow(clientID string) bool {
	return rl.AllowWithLimit(clientID, rl.limit, rl.burst)
}

// AllowWithLimit checks if a client is within a specific RPM/burst limit.
// If rpm <= 0, allows unlimited.
func (rl *ClientRateLimiter) AllowWithLimit(clientID string, rpm, burst int) bool {
	if rpm <= 0 {
		return true // unlimited
	}
	if burst <= 0 {
		burst = rpm
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	bucket, exists := rl.clients[clientID]
	if !exists {
		// New client starts with burst tokens
		rl.clients[clientID] = &clientBucket{
			tokens:   float64(burst) - 1,
			lastTime: now,
		}
		return true
	}

	// Refill tokens based on elapsed time (token bucket)
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

func (rl *ClientRateLimiter) cleanup() {
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

// RateLimitMiddleware applies per-client rate limiting with per-key overrides.
func RateLimitMiddleware(rl *ClientRateLimiter, am *AuthManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip rate limit for health/info endpoints
		path := c.Request.URL.Path
		if path == "/health" || path == "/" || path == "/dashboard" {
			c.Next()
			return
		}

		// Client ID = full API key (or IP if no auth)
		clientID := c.ClientIP()
		fullKey := ""
		if key, exists := c.Get("client_key"); exists {
			clientID = key.(string)
			fullKey = key.(string)
		}

		// Determine per-key RPM/burst, fallback to global
		rpm := rl.limit
		burst := rl.burst
		if fullKey != "" && am != nil {
			if info := am.Get(fullKey); info != nil {
				if info.RPM == 0 {
					// RPM=0 means unlimited — bypass rate limit entirely
					c.Next()
					return
				}
				if info.RPM > 0 {
					rpm = info.RPM
				}
				if info.Burst > 0 {
					burst = info.Burst
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
