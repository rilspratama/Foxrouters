package main

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// loginLimiter is an in-memory IP-based rate limiter for /login POST.
// Prevents brute-force key spraying (P3 #6).
// Limits: 5 attempts per minute, 20 per hour per client IP.
type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]*loginEntry
}

type loginEntry struct {
	minuteWindow []time.Time // timestamps within last 1 min
	hourWindow   []time.Time // timestamps within last 1 hour
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{entries: make(map[string]*loginEntry)}
}

func (l *loginLimiter) middleware() gin.HandlerFunc {
	// Background cleanup every 10 min (remove stale entries)
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			l.cleanup()
		}
	}()
	return func(c *gin.Context) {
		ip := c.ClientIP()
		now := time.Now()

		l.mu.Lock()
		e, ok := l.entries[ip]
		if !ok {
			e = &loginEntry{}
			l.entries[ip] = e
		}

		// Trim expired entries
		minCutoff := now.Add(-1 * time.Minute)
		hourCutoff := now.Add(-1 * time.Hour)
		e.minuteWindow = trimBefore(e.minuteWindow, minCutoff)
		e.hourWindow = trimBefore(e.hourWindow, hourCutoff)

		// Check limits
		if len(e.minuteWindow) >= 5 {
			l.mu.Unlock()
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "too many login attempts, try again in a minute",
			})
			c.Abort()
			return
		}
		if len(e.hourWindow) >= 20 {
			l.mu.Unlock()
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "too many login attempts, try again in an hour",
			})
			c.Abort()
			return
		}

		// Record this attempt
		e.minuteWindow = append(e.minuteWindow, now)
		e.hourWindow = append(e.hourWindow, now)
		l.mu.Unlock()

		c.Next()
	}
}

func (l *loginLimiter) cleanup() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	minCutoff := now.Add(-1 * time.Minute)  // P4-1: was hourCutoff (bug)
	hourCutoff := now.Add(-1 * time.Hour)
	for ip, e := range l.entries {
		e.minuteWindow = trimBefore(e.minuteWindow, minCutoff)
		e.hourWindow = trimBefore(e.hourWindow, hourCutoff)
		if len(e.hourWindow) == 0 {
			delete(l.entries, ip)
		}
	}
}

func trimBefore(times []time.Time, cutoff time.Time) []time.Time {
	// times are appended in order, so we can binary-search for the cutoff
	// but linear scan is fine for small windows (≤20 entries)
	out := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	return out
}
