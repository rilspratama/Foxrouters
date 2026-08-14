package main

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// loginLimiter is an in-memory rate limiter for /login POST.
// Prevents brute-force key spraying (P3 #6).
// Dimensions:
//   - per client IP:     5 attempts/min, 20/hour
//   - per key prefix:   10 attempts/min, 50/hour  (defense-in-depth vs
//     distributed IP rotation spraying the same guessed key; the first 8
//     chars are enough to group attempts on the same key)
type loginLimiter struct {
	mu         sync.Mutex
	entries    map[string]*loginEntry // keyed by client IP
	keyEntries map[string]*loginEntry // keyed by key-prefix
}

type loginEntry struct {
	minuteWindow []time.Time // timestamps within last 1 min
	hourWindow   []time.Time // timestamps within last 1 hour
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{entries: make(map[string]*loginEntry), keyEntries: make(map[string]*loginEntry)}
}

// check enforces limit/attempts within the two windows; returns true if allowed.
func (e *loginEntry) check(now time.Time, minLimit, hourLimit int) bool {
	e.minuteWindow = trimBefore(e.minuteWindow, now.Add(-1*time.Minute))
	e.hourWindow = trimBefore(e.hourWindow, now.Add(-1*time.Hour))
	if len(e.minuteWindow) >= minLimit || len(e.hourWindow) >= hourLimit {
		return false
	}
	e.minuteWindow = append(e.minuteWindow, now)
	e.hourWindow = append(e.hourWindow, now)
	return true
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
		allowed := e.check(now, 5, 20)
		l.mu.Unlock()
		if !allowed {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "too many login attempts, try again later",
			})
			c.Abort()
			return
		}

		// Second dimension: per-key-prefix. A distributed spray that rotates
		// IPs while trying the same guessed key still trips this.
		if k := c.PostForm("key"); len(k) >= 8 {
			kp := k[:8]
			l.mu.Lock()
			ke, ok := l.keyEntries[kp]
			if !ok {
				ke = &loginEntry{}
				l.keyEntries[kp] = ke
			}
			allowed = ke.check(now, 10, 50)
			l.mu.Unlock()
			if !allowed {
				c.JSON(http.StatusTooManyRequests, gin.H{
					"error": "too many attempts for this key, try again later",
				})
				c.Abort()
				return
			}
		}

		c.Next()
	}
}

func (l *loginLimiter) cleanup() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	minCutoff := now.Add(-1 * time.Minute) // P4-1: was hourCutoff (bug)
	hourCutoff := now.Add(-1 * time.Hour)
	for ip, e := range l.entries {
		e.minuteWindow = trimBefore(e.minuteWindow, minCutoff)
		e.hourWindow = trimBefore(e.hourWindow, hourCutoff)
		if len(e.hourWindow) == 0 {
			delete(l.entries, ip)
		}
	}
	for kp, e := range l.keyEntries {
		e.minuteWindow = trimBefore(e.minuteWindow, minCutoff)
		e.hourWindow = trimBefore(e.hourWindow, hourCutoff)
		if len(e.hourWindow) == 0 {
			delete(l.keyEntries, kp)
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
