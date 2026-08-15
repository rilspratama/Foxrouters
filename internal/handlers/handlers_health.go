package handlers

import (
	"foxrouters/internal/auth"
	"foxrouters/internal/upstream"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func HandleHealthMinimal() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status":  "healthy",
			"service": "foxrouters",
			"version": version,
		})
	}
}

// HandleHealth reports overall status + (when authed) per-upstream telemetry.
func HandleHealth(grokAM *upstream.GrokAccountManager, cbKM *upstream.CBKeyManager, fbAM *upstream.FreebuffAccountManager, aliAM *upstream.AlibabaKeyManager, hc *upstream.HealthChecker, am *auth.Manager, sessions *auth.SessionStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		grokStats := hc.Grok.Stats()
		cbStats := hc.CB.Stats()
		var fbStats map[string]any
		if hc.FB != nil {
			fbStats = hc.FB.Stats()
		}
		var aliStats map[string]any
		if hc.Ali != nil {
			aliStats = hc.Ali.Stats()
		}

		// Overall status: unhealthy if any circuit is open
		grokCircuit := grokStats["circuit_state"].(string)
		cbCircuit := cbStats["circuit_state"].(string)
		overall := "healthy"
		if grokCircuit == "open" || cbCircuit == "open" {
			overall = "degraded"
		}
		if fbStats != nil {
			if fbCircuit, ok := fbStats["circuit_state"].(string); ok && fbCircuit == "open" {
				overall = "degraded"
			}
		}
		if aliStats != nil {
			if aliCircuit, ok := aliStats["circuit_state"].(string); ok && aliCircuit == "open" {
				overall = "degraded"
			}
		}

		// Public response: minimal liveness only.
		// Detailed telemetry only if caller presents a valid gateway key
		// (via Bearer header OR session cookie — dashboard uses cookie).
		// When GATEWAY_AUTH_DISABLE=1, everyone gets full telemetry (dev mode).
		authed := false
		if os.Getenv("GATEWAY_AUTH_DISABLE") == "1" {
			authed = true
		} else if a := c.GetHeader("Authorization"); strings.HasPrefix(a, "Bearer ") {
			if _, ok := am.Get(a[7:]); ok {
				authed = true
			}
		} else if ck, err := c.Cookie("foxrouters_session"); err == nil && ck != "" {
			// P3-3: cookie is now a session token, resolve to API key.
			var key string
			if sessions != nil {
				key = sessions.Lookup(ck)
			} else {
				key = ck // legacy fallback (pre-P3-3)
			}
			if key != "" {
				if _, ok := am.Get(key); ok {
					authed = true
				}
			}
		}
		if !authed {
			c.JSON(200, gin.H{
				"status":  overall,
				"service": "foxrouters",
				"version": version,
			})
			return
		}

		// Authed: full telemetry
		c.JSON(200, gin.H{
			"status":  overall,
			"service": "foxrouters",
			"version": version,
			"mode":    "unified (grok + codebuddy + freebuff)",
			"upstreams": gin.H{
				"grok":      grokStats,
				"codebuddy": cbStats,
				"freebuff":  fbStats,
				"alibaba":   aliStats,
			},
			"grok_accounts": grokAM.Len(),
			"grok_active": func() int {
				active := 0
				for _, a := range grokAM.GetAll() {
					if s := a.Snapshot(); !s.Disabled {
						active++
					}
				}
				return active
			}(),
			"grok_tokens_used": func() int64 {
				var total int64
				for _, a := range grokAM.GetAll() {
					total += a.Snapshot().TokensUsed
				}
				return total
			}(),
			"grok_tokens_quota": func() int64 {
				return int64(upstream.GROK_FREE_TIER_QUOTA) * int64(grokAM.Len())
			}(),
			"cb_credits_used": func() float64 {
				var total float64
				for _, k := range cbKM.GetAll() {
					total += float64(k.Snapshot().CreditsUsed)
				}
				return total
			}(),
			"cb_credits_limit": func() float64 {
				var total float64
				for _, k := range cbKM.GetAll() {
					total += float64(k.Snapshot().CreditLimit)
				}
				return total
			}(),
			"cb_keys": cbKM.Len(),
			"cb_keys_active": func() int {
				active := 0
				for _, k := range cbKM.GetAll() {
					if _, _, disabled := k.Stats(); !disabled {
						active++
					}
				}
				return active
			}(),
			"fb_accounts":  fbAM.Len(),
			"ali_accounts": aliAM.Len(),
			"ali_keys_active": func() int {
				active := 0
				for _, k := range aliAM.ListAccounts() {
					if d, _ := k["disabled"].(bool); !d {
						active++
					}
				}
				return active
			}(),
			"fb_tier_full": func() int {
				count := 0
				for _, a := range fbAM.ListAccounts() {
					if a["tier"] == "full" {
						count++
					}
				}
				return count
			}(),
			"fb_tier_limited": func() int {
				count := 0
				for _, a := range fbAM.ListAccounts() {
					if a["tier"] == "limited" {
						count++
					}
				}
				return count
			}(),
			"fb_tier_blocked": func() int {
				count := 0
				for _, a := range fbAM.ListAccounts() {
					if a["tier"] == "blocked" {
						count++
					}
				}
				return count
			}(),
			"fb_quota_used":  func() float64 { t, _, _ := fbAM.QuotaSummary(); return t }(),
			"fb_quota_limit": func() float64 { _, t, _ := fbAM.QuotaSummary(); return t }(),
			"fb_quota_exhausted": func() int {
				count := 0
				for _, a := range fbAM.ListAccounts() {
					lr, _ := a["quota_limit"].(float64)
					rr, _ := a["quota_recent"].(float64)
					if lr > 0 && rr >= lr {
						count++
					}
				}
				return count
			}(),
			"time": time.Now().Format(time.RFC3339),
		})
	}
}
