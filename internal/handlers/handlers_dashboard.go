package handlers

import (
	"foxrouters/internal/auth"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

var version = "dev"

// dashboardHTML is the //go:embed dashboard.html string owned by main.
// SetDashboardHTML is called once from main.main() before routes wire up.
var dashboardHTML = ""

// SetVersion overrides the reported gateway version (called from main).
func SetVersion(v string) { version = v }

// SetDashboardHTML injects the embedded dashboard SPA payload from main.
func SetDashboardHTML(s string) { dashboardHTML = s }

func HandleDashboard() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Data(200, "text/html; charset=utf-8", []byte(dashboardHTML))
	}
}

// HandleLogin serves the login page (GET) and processes login (POST).
// On successful auth, sets an HttpOnly cookie with a random session token
// (NOT the raw API key — P3-3 session fixation fix) and redirects to /dashboard.
func HandleLogin(am *auth.Manager, sessions *auth.SessionStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == "GET" {
			c.Data(200, "text/html; charset=utf-8", []byte(loginPageHTML))
			return
		}

		// POST: verify key
		var req struct {
			Key string `form:"key" json:"key"`
		}
		if err := c.ShouldBind(&req); err != nil || req.Key == "" {
			c.Data(200, "text/html; charset=utf-8", []byte(loginPageHTMLWithError("Key is required")))
			return
		}
		req.Key = strings.TrimSpace(req.Key)

		if !am.Valid(req.Key) {
			c.Data(200, "text/html; charset=utf-8", []byte(loginPageHTMLWithError("Invalid API key")))
			return
		}

		// D2: only admin keys can log into the dashboard. Inference-role keys can
		// call /v1/* with a Bearer token, but the dashboard endpoints are
		// admin-only — letting them in produces a redirect loop (dashboard XHR
		// gets 401 → JS redirects to /login → login succeeds → loop).
		if info, ok := am.Get(req.Key); !ok || info.Role != auth.RoleAdmin {
			c.Data(200, "text/html; charset=utf-8", []byte(loginPageHTMLWithError("This key does not have dashboard access (admin role required)")))
			return
		}

		// P3-3: issue a random session token bound to the key (not the key itself).
		token, err := sessions.Create(req.Key)
		if err != nil {
			c.Data(200, "text/html; charset=utf-8", []byte(loginPageHTMLWithError("Session error")))
			return
		}

		c.SetSameSite(http.SameSiteLaxMode)
		cookieSecure := os.Getenv("COOKIE_SECURE") != "0"
		c.SetCookie("foxrouters_session", token, int(auth.SessionTTL.Seconds()), "/", "", cookieSecure, true)
		c.Redirect(302, "/dashboard")
	}
}

// HandleLogout clears the session cookie and redirects to /login.
func HandleLogout(sessions *auth.SessionStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, _ := c.Cookie("foxrouters_session")
		sessions.Revoke(token)
		cookieSecure := os.Getenv("COOKIE_SECURE") != "0"
		c.SetCookie("foxrouters_session", "", -1, "/", "", cookieSecure, true)
		c.Redirect(302, "/login")
	}
}
