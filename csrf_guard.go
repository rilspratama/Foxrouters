package main

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// csrfGuard blocks cross-origin state-changing requests that rely on the
// session cookie. SameSite=Lax already blocks cross-site FORM POSTs, but
// a cross-site JSON POST (Content-Type: application/json) slips through
// Lax because it's a "non-simple" request — the browser fires a CORS
// preflight which, without ACAO headers from us, blocks the actual request.
// This middleware is a defense-in-depth check on top of that: for
// cookie-authenticated mutations (POST/PUT/PATCH/DELETE), reject if the
// Origin or Referer header is present and doesn't match our host.
//
// Bearer-authenticated requests are exempt (they don't carry the cookie
// and thus can't be CSRF'd). (P2-2)
func csrfGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		method := c.Request.Method
		if method == "GET" || method == "HEAD" || method == "OPTIONS" {
			c.Next()
			return
		}

		// Only enforce when the request carries the session cookie.
		_, err := c.Cookie("foxrouters_session")
		hasCookie := err == nil
		if !hasCookie {
			// Bearer-authenticated API call — CSRF doesn't apply.
			c.Next()
			return
		}

		// Bearer header present → cookie is incidental (e.g. browser calling API with Bearer).
		if authHeader := c.GetHeader("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
			c.Next()
			return
		}

		target := c.Request.Host // e.g. "127.0.0.1:20130" or "gateway.example.com"
		origin := c.GetHeader("Origin")
		referer := c.GetHeader("Referer")

		// If neither header is present, the request is same-origin (browsers
		// always send Origin for cross-site fetches). Allow it.
		if origin == "" && referer == "" {
			c.Next()
			return
		}

		// Check Origin first (preferred per spec).
		if origin != "" {
			if sameOrigin(origin, target) {
				c.Next()
				return
			}
			c.AbortWithStatusJSON(403, gin.H{"error": "cross-origin request blocked"})
			return
		}

		// Fall back to Referer.
		if referer != "" && !sameOrigin(referer, target) {
			c.AbortWithStatusJSON(403, gin.H{"error": "cross-origin request blocked"})
			return
		}

		c.Next()
	}
}

// sameOrigin reports whether originURL (e.g. "https://gateway.example.com:20130/path")
// targets the same host as hostPort (e.g. "gateway.example.com:20130").
func sameOrigin(originURL, hostPort string) bool {
	// Strip scheme.
	rest := originURL
	if idx := strings.Index(rest, "://"); idx != -1 {
		rest = rest[idx+3:]
	}
	// Strip path.
	if idx := strings.Index(rest, "/"); idx != -1 {
		rest = rest[:idx]
	}
	return rest == hostPort
}
