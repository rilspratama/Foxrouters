// foxrouters v5.0 — Unified OpenAI-compatible gateway for Grok + CodeBuddy.
// v4.0: Health checker (active + passive), circuit breaker, upstream latency tracking.
// v4.1: API key auth (Bearer token), per-client rate limiting (sliding window).
// v4.2: Web UI dashboard (/dashboard) — real-time stats, health, accounts, quick test.
// v5.0: Redis (hot state) + history DB — persistent accounts, request logs, analytics.
// v5.9: ClickHouse history (full request body, ZSTD) — PostgreSQL history retired.
// Routes by model name: grok-* → cli-chat-proxy.grok.com, cb-* → www.codebuddy.ai/v2.
// Grok: multi-account round-robin + auto refresh_token.
// CodeBuddy: multi-API-key round-robin, stream-only, auto system message injection.
package main

import (
	_ "embed"
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"foxrouters/internal/ratelimit"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

//go:embed dashboard.html
var dashboardHTML string

// init wires the stdlib slog default handler. LOG_LEVEL=debug (or DEBUG)
// enables verbose output; default is info. TextHandler keeps the output
// grep-friendly (journalctl-compatible), roughly matching log.Printf lines.
func init() {
	level := slog.LevelInfo
	switch os.Getenv("LOG_LEVEL") {
	case "debug", "DEBUG":
		level = slog.LevelDebug
	case "warn", "WARN":
		level = slog.LevelWarn
	case "error", "ERROR":
		level = slog.LevelError
	}
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
	// Route stdlib "log" package through slog too, so third-party code
	// (gin.Default's default logger, etc.) shows up in the same stream.
	log.SetFlags(0)
}

// ============================================================================
// CONFIG
// ============================================================================

// Version is the single source of truth for /health, /, and logs.
// Injected at build time via -ldflags "-X main.Version=<tag>", fallback "dev".
var Version = "dev"

const (
	XAI_CLIENT_ID    = "b1a00492-073a-47ea-816f-4c329264a828"
	XAI_TOKEN_URL    = "https://auth.x.ai/oauth2/token"
	XAI_UPSTREAM_URL = "https://cli-chat-proxy.grok.com/v1"
	CB_UPSTREAM_URL  = "https://www.codebuddy.ai/v2/chat/completions"
	REFRESH_BUFFER   = 10 * time.Minute
	DEFAULT_PORT     = "20130"

	GROK_CLIENT_VERSION    = "0.2.93"
	GROK_CLIENT_IDENTIFIER = "grok-shell"
	CB_DEFAULT_SYSTEM      = "You are a helpful assistant."

	// Health check constants
	HEALTH_CHECK_INTERVAL = 10 * time.Minute // active LLM test every 10 min
	HEALTH_CHECK_TIMEOUT  = 30 * time.Second  // LLM test timeout
	CB_OPEN_THRESHOLD     = 5                 // 5 consecutive errors → circuit open
	CB_OPEN_DURATION      = 60 * time.Second  // circuit stays open 60s before half-open
	CB_HALF_OPEN_PROBES   = 1                 // 1 probe in half-open

	// Grok token pre-warm — background worker refreshes tokens BEFORE they expire,
	// so request path never blocks on synchronous refresh.
	PRE_WARM_TICK          = 30 * time.Second // worker checks every 30s
	PRE_WARM_WINDOW        = 30 * time.Minute // refresh when <30min to expiry
	MAX_CONCURRENT_REFRESH = 10               // max parallel token refreshes per tick
	REENABLE_TICK          = 1 * time.Minute  // background cooldown re-enable (off Next hot path)

	// Auth + rate limiting constants
	// GATEWAY_KEY_FILE / CB_KEY_FILE resolved via env (see gatewayKeyFile())
	RATE_LIMIT_RPM   = 60             // requests per minute per client
	RATE_LIMIT_BURST = 10             // max burst (allow short spikes)
	RATE_LIMIT_WINDOW = 1 * time.Minute // sliding window duration
)

// ============================================================================
// SHARED HTTP CLIENT — connection pooling + HTTP/2 + keep-alive
// Reuses TCP connections across requests (eliminates TLS handshake overhead)
// ============================================================================
var (
	// upstreamClient: for Grok + CB API calls (long timeout, connection pool)
	upstreamClient = &http.Client{
		Timeout: 300 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,             // total idle connections
			MaxIdleConnsPerHost: 20,              // per-host idle (Grok + CB = 2 hosts)
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			ForceAttemptHTTP2:   true,            // enable HTTP/2 (multiplexing + header compression)
			// Disable compression — we handle gzip ourselves in middleware
			DisableCompression: false,
		},
	}

	// tokenRefreshClient: for auth.x.ai token refresh (shorter timeout)
	tokenRefreshClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 5,
			IdleConnTimeout:     60 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	}

	// healthCheckClient: for active health checks
	healthCheckClient = &http.Client{
		Timeout: HEALTH_CHECK_TIMEOUT,
		Transport: &http.Transport{
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 5,
			IdleConnTimeout:     60 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	}
)

// ============================================================================
// MAIN
// ============================================================================

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = DEFAULT_PORT
	}

	// Initialize Redis + ClickHouse
	db, err := NewDBStore()
	if err != nil {
		slog.Error("DB init failed", "module", "main", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	grokAM := NewGrokAccountManager(db)
	if err := grokAM.LoadFromRedis(); err != nil {
		slog.Warn("LoadFromRedis failed, starting empty", "module", "grok", "error", err)
	}

	cbKM := NewCBKeyManager(db)
	if err := cbKM.LoadFromRedis(); err != nil {
		slog.Warn("LoadFromRedis failed, starting empty", "module", "cb", "error", err)
	}

	// Health checker: active + passive monitoring with circuit breaker
	hc := newHealthChecker(grokAM, cbKM)
	hc.Start()

	// Auth + rate limiter
	authMgr := newAuthManager(db)
	rateLimiter := ratelimit.New(RATE_LIMIT_RPM, RATE_LIMIT_BURST, RATE_LIMIT_WINDOW)
	// (rateLimiter previously carried a db handle for rate-limited request
	//  logging, but nothing actually consumed it — dropped in the split.)

	go autoRefreshWorker(grokAM)
	go reenableWorker(grokAM)
	go reenableCBWorker(cbKM)
	// Snapshot pool sizes into Prometheus gauges every 10s. Cheap RLock walk;
	// keeps activeKeys/disabledKeys eventually consistent without touching the
	// hot path. Circuit-state gauges are updated inline from health.go.
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		updatePoolGauges(grokAM, cbKM, authMgr) // prime once
		for range t.C {
			updatePoolGauges(grokAM, cbKM, authMgr)
		}
	}()

	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	// Middleware: request ID, security headers, gzip compression, auth, rate limit
	r.Use(ratelimit.RequestIDMiddleware())
	r.Use(ratelimit.SecurityHeadersMiddleware())
	r.Use(ratelimit.GzipMiddleware())
	r.Use(AuthMiddleware(authMgr))
	r.Use(ratelimit.Middleware(rateLimiter, authMgr))

	// API key management endpoints — admin only
	adminAuth := AdminMiddleware(authMgr)
	r.GET("/api/keys", adminAuth, handleListKeys(authMgr))
	r.POST("/api/keys", adminAuth, handleCreateKey(authMgr))
	r.DELETE("/api/keys/:key", adminAuth, handleDeleteKey(authMgr))
	r.PUT("/api/keys/:key", adminAuth, handleUpdateKey(authMgr))
	r.GET("/api/keys/:key/usage", adminAuth, handleKeyUsage(authMgr))

	r.GET("/dashboard", handleDashboard())
	r.GET("/login", handleLogin(authMgr))
	r.POST("/login", handleLogin(authMgr))
	r.GET("/logout", handleLogout())
	r.GET("/health", handleHealth(grokAM, cbKM, hc, authMgr))
	r.HEAD("/health", handleHealthMinimal())
	// Prometheus scrape endpoint — public, no auth (scraper isolation is
	// upstream's responsibility, e.g. a firewall / private network scrape).
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))
	// Accounts/keys/history — admin only (inference keys must not see other tenants' data)
	r.GET("/accounts", adminAuth, handleAccounts(grokAM, cbKM))
	r.GET("/cb-stats", adminAuth, func(c *gin.Context) {
		keys := cbKM.GetAll()
		stats := []gin.H{}
		for _, k := range keys {
			credits, reqs, disabled := k.Stats()
			keyDisplay := k.Key
			if len(keyDisplay) > 12 {
				keyDisplay = keyDisplay[:8] + "..." + keyDisplay[len(keyDisplay)-4:]
			}
			stats = append(stats, gin.H{
				"key":            keyDisplay,
				"credits_used":   credits,
				"credit_limit":   CB_CREDIT_LIMIT,
				"credits_left":   CB_CREDIT_LIMIT - credits,
				"total_requests": reqs,
				"disabled":       disabled,
			})
		}
		c.JSON(200, gin.H{"codebuddy_keys": stats})
	})
	r.POST("/accounts/refresh", adminAuth, handleRefresh(grokAM))
	r.POST("/accounts/import", adminAuth, handleImportAccount(grokAM))
	r.POST("/accounts/import/bulk", adminAuth, handleImportAccountBulk(grokAM))
	r.POST("/cb/import", adminAuth, handleImportCBKey(cbKM))
	r.POST("/cb/import/bulk", adminAuth, handleImportCBKeyBulk(cbKM))
	r.DELETE("/accounts/:email", adminAuth, handleDeleteAccount(grokAM))
	r.GET("/history", adminAuth, handleHistory(db))
	r.GET("/history/recent", adminAuth, handleRecentRequests(db))
	r.GET("/history/detail/:id", adminAuth, handleHistoryDetail(db))
	r.Any("/v1/*path", proxyRequest(grokAM, cbKM, hc, authMgr))

	r.GET("/", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"service": "foxrouters",
			"version": Version,
			"mode":    "unified (grok + codebuddy)",
			"endpoints": []string{
				"POST /v1/chat/completions — grok-* → Grok, cb/* → CodeBuddy",
				"GET  /v1/models",
				"GET  /accounts",
				"POST /accounts/refresh",
				"GET  /health — upstream health + circuit breaker status",
				"GET  /cb-stats",
				"GET  /history — request stats + model breakdown",
				"GET  /history/recent — recent request logs",
				"GET  /history/detail/:id — full request/response JSON for a single log",
				"GET  /dashboard — web UI dashboard",
			},
			"grok_accounts": grokAM.Len(),
			"cb_keys":       cbKM.Len(),
		})
	})

	slog.Info("foxrouters started",
		"module", "server",
		"version", Version,
		"port", port,
		"grok_accounts", grokAM.Len(),
		"cb_keys", cbKM.Len(),
		"auth", func() string {
			n := authMgr.Count()
			if n > 0 {
				return fmt.Sprintf("%d keys", n)
			}
			return "disabled"
		}(),
		"db", "redis+ch")
	slog.Info("dashboard ready", "module", "server", "url", fmt.Sprintf("http://localhost:%s/dashboard", port))

	// Graceful shutdown: drain in-flight requests, flush async DB logs.
	// Timeouts: ReadHeaderTimeout protects against Slowloris; WriteTimeout
	// must exceed upstream LLM latency (max ~300s) — set to 0 (no timeout)
	// for streaming responses; IdleTimeout drops keepalive conns.
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0, // body read handled by MaxBytesReader per-route
		WriteTimeout:      0, // streaming SSE — no global write timeout
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1MB header cap
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server fatal", "module", "server", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	slog.Info("shutdown signal received, draining", "module", "server", "signal", sig.String())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "module", "server", "error", err)
	}
	// db.Close() runs via defer — drains async log channels best-effort
	slog.Info("stopped", "module", "server")
}
