package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"foxrouters/internal/ratelimit"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// setupTestRouter builds a minimal router mirroring main.go's public routes
// for integration testing without needing Redis/ClickHouse.
func setupTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(ratelimit.RequestIDMiddleware())
	r.Use(ratelimit.SecurityHeadersMiddleware())

	// Public routes (no auth)
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"service": "foxrouters", "status": "healthy", "version": Version})
	})
	r.HEAD("/health", func(c *gin.Context) { c.Status(200) })
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))
	r.GET("/login", func(c *gin.Context) {
		c.Data(200, "text/html; charset=utf-8", []byte("<html>login</html>"))
	})
	r.POST("/login", func(c *gin.Context) {
		key := c.PostForm("key")
		if key == "" {
			c.JSON(401, gin.H{"error": "key required"})
			return
		}
		c.SetSameSite(http.SameSiteLaxMode)
		c.SetCookie("foxrouters_session", key, 7*24*3600, "/", "", false, true)
		c.Redirect(302, "/dashboard")
	})
	r.GET("/logout", func(c *gin.Context) {
		c.SetCookie("foxrouters_session", "", -1, "/", "", false, true)
		c.Redirect(302, "/login")
	})
	r.GET("/dashboard", func(c *gin.Context) {
		c.Data(200, "text/html; charset=utf-8", []byte("<html>dashboard</html>"))
	})

	// Protected route (mock auth)
	authGroup := r.Group("/", func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		cookie, _ := c.Cookie("foxrouters_session")
		if strings.HasPrefix(auth, "Bearer gw-test-key") || cookie == "gw-test-key" {
			c.Set("client_key", "gw-test-key")
			c.Next()
			return
		}
		c.JSON(401, gin.H{"error": "auth required"})
		c.Abort()
	})
	authGroup.GET("/api/keys", func(c *gin.Context) {
		c.JSON(200, gin.H{"keys": []gin.H{{"name": "test", "role": "admin"}}})
	})

	return r
}

func TestHealthEndpoint_Public(t *testing.T) {
	r := setupTestRouter()
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "healthy") {
		t.Errorf("expected 'healthy' in body, got: %s", body)
	}
}

func TestHealthEndpoint_HEAD(t *testing.T) {
	r := setupTestRouter()
	req := httptest.NewRequest("HEAD", "/health", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestMetricsEndpoint_Public(t *testing.T) {
	r := setupTestRouter()
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Prometheus metrics should contain our custom metrics or standard go_ metrics
	if !strings.Contains(body, "go_") && !strings.Contains(body, "foxrouters_") {
		t.Errorf("expected Prometheus metrics in body, got first 200 chars: %s", body[:200])
	}
}

func TestMetricsEndpoint_NoAuth(t *testing.T) {
	r := setupTestRouter()
	// Should NOT require Authorization header
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code == 401 {
		t.Fatal("/metrics should be public (no auth), got 401")
	}
}

func TestLoginEndpoint_GET(t *testing.T) {
	r := setupTestRouter()
	req := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestLoginEndpoint_POST_ValidKey(t *testing.T) {
	r := setupTestRouter()
	form := strings.NewReader("key=gw-test-key")
	req := httptest.NewRequest("POST", "/login", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 302 {
		t.Fatalf("expected 302 redirect, got %d", w.Code)
	}
	if w.Header().Get("Location") != "/dashboard" {
		t.Errorf("expected redirect to /dashboard, got: %s", w.Header().Get("Location"))
	}
	// Verify cookie set
	cookies := w.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "foxrouters_session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected foxrouters_session cookie to be set")
	}
	if sessionCookie.HttpOnly != true {
		t.Error("expected HttpOnly=true")
	}
	if sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Error("expected SameSite=Lax")
	}
}

func TestLoginEndpoint_POST_EmptyKey(t *testing.T) {
	r := setupTestRouter()
	req := httptest.NewRequest("POST", "/login", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expected 401 for empty key, got %d", w.Code)
	}
}

func TestLogoutEndpoint(t *testing.T) {
	r := setupTestRouter()
	req := httptest.NewRequest("GET", "/logout", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 302 {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	if w.Header().Get("Location") != "/login" {
		t.Errorf("expected redirect to /login, got: %s", w.Header().Get("Location"))
	}
	// Cookie should be cleared (MaxAge < 0)
	cookies := w.Result().Cookies()
	for _, c := range cookies {
		if c.Name == "foxrouters_session" && c.MaxAge > 0 {
			t.Error("expected session cookie to be cleared (MaxAge < 0)")
		}
	}
}

func TestAuthBearerFlow(t *testing.T) {
	r := setupTestRouter()
	req := httptest.NewRequest("GET", "/api/keys", nil)
	req.Header.Set("Authorization", "Bearer gw-test-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200 with valid Bearer, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "test") {
		t.Errorf("expected key name in body, got: %s", body)
	}
}

func TestAuthBearerFlow_InvalidToken(t *testing.T) {
	r := setupTestRouter()
	req := httptest.NewRequest("GET", "/api/keys", nil)
	req.Header.Set("Authorization", "Bearer gw-wrong-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expected 401 with invalid Bearer, got %d", w.Code)
	}
}

func TestAuthCookieFlow(t *testing.T) {
	r := setupTestRouter()
	req := httptest.NewRequest("GET", "/api/keys", nil)
	req.AddCookie(&http.Cookie{Name: "foxrouters_session", Value: "gw-test-key"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200 with valid cookie, got %d", w.Code)
	}
}

func TestAuthFlow_NoAuth(t *testing.T) {
	r := setupTestRouter()
	req := httptest.NewRequest("GET", "/api/keys", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expected 401 without auth, got %d", w.Code)
	}
}

func TestDashboardEndpoint_Public(t *testing.T) {
	r := setupTestRouter()
	req := httptest.NewRequest("GET", "/dashboard", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	r := setupTestRouter()
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	headers := w.Header()
	checks := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for h, want := range checks {
		if got := headers.Get(h); got != want {
			t.Errorf("header %s = %q, want %q", h, got, want)
		}
	}
}

func TestRequestIDMiddleware(t *testing.T) {
	r := setupTestRouter()
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if rid := w.Header().Get("X-Request-Id"); rid == "" {
		t.Error("expected X-Request-Id header to be set")
	}
}

func TestRequestIDMiddleware_HonorInbound(t *testing.T) {
	r := setupTestRouter()
	req := httptest.NewRequest("GET", "/health", nil)
	req.Header.Set("X-Request-Id", "test-id-123")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if rid := w.Header().Get("X-Request-Id"); rid != "test-id-123" {
		t.Errorf("expected X-Request-Id to honor inbound 'test-id-123', got %q", rid)
	}
}
