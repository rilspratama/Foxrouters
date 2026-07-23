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

	"foxrouters/internal/handlers"
	"foxrouters/internal/ratelimit"
	"foxrouters/internal/tunnel"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

//go:embed dashboard.html
var dashboardHTML string

// Propagate main-owned state into the extracted handlers package at binary
// startup so both `go test` and `go run` see the same dashboardHTML/Version.
// (main() still calls SetVersion after Version is finalised, which is fine —
// tests run without ldflags so Version stays "dev".)
func init() {
	handlers.SetDashboardHTML(dashboardHTML)
	handlers.SetVersion(Version)
}

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
	DEFAULT_PORT = "20130"

	// Auth + rate limiting constants
	// GATEWAY_KEY_FILE / CB_KEY_FILE resolved via env (see gatewayKeyFile())
	RATE_LIMIT_RPM    = 60             // requests per minute per client
	RATE_LIMIT_BURST  = 10             // max burst (allow short spikes)
	RATE_LIMIT_WINDOW = 1 * time.Minute // sliding window duration
)

// ============================================================================
// SHARED HTTP CLIENT
// Upstream / token-refresh / health-check clients live in internal/upstream —
// main.go doesn't need them any more.
// ============================================================================

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
	sessions := NewSessionStore() // P3-3: session token indirection (cookie ≠ API key)
	rateLimiter := ratelimit.New(RATE_LIMIT_RPM, RATE_LIMIT_BURST, RATE_LIMIT_WINDOW)
	// (rateLimiter previously carried a db handle for rate-limited request
	//  logging, but nothing actually consumed it — dropped in the split.)

	// Custom models + aliases registry (v1.3.0). Redis-backed, cached in
	// memory — reloaded on every mutation via handlers.
	customReg := NewCustomRegistry(db)
	if err := customReg.Load(); err != nil {
		slog.Warn("custom registry Load failed, starting empty", "module", "custom", "error", err)
	}

	// Combos registry (v1.4.0). Redis-backed, cached in memory.
	comboReg := NewComboRegistry(db)
	if err := comboReg.Load(); err != nil {
		slog.Warn("combo registry Load failed, starting empty", "module", "combo", "error", err)
	}

	// Proxy pool (v1.5.0). Redis-backed, cached in memory. Wired into
	// internal/upstream so Grok / CodeBuddy / token-refresh HTTP calls
	// route through enabled proxies (round-robin).
	proxyPool := NewProxyPool(db)
	if err := proxyPool.Load(); err != nil {
		slog.Warn("proxy pool Load failed, starting empty", "module", "proxy-pool", "error", err)
	}
	setUpstreamProxyPool(proxyPool)

	// Cloudflare Tunnel manager (v1.6.0). Owns cloudflared subprocess
	// lifecycle + named-tunnel control plane via cloudflare-go v7 SDK.
	// Config lives in Redis so mode + credentials survive restarts.
	cloudflaredPath := os.Getenv("CLOUDFLARED_PATH")
	if cloudflaredPath == "" {
		cloudflaredPath = "/usr/local/bin/cloudflared"
	}
	tunnelUpstream := os.Getenv("TUNNEL_UPSTREAM_URL")
	if tunnelUpstream == "" {
		tunnelUpstream = "http://127.0.0.1:" + port
	}
	tunnelMgr := tunnel.NewManager(db.Redis(), cloudflaredPath, tunnelUpstream)
	// Auto-start in background — must NOT block gateway boot even if
	// cloudflared is missing or Cloudflare API is unreachable.
	go tunnelMgr.AutoStart()

	go autoRefreshWorker(grokAM)
	go reenableWorker(grokAM)
	go reenableCBWorker(cbKM)
	go cbOAuthRefreshWorker(cbKM)
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
	// P2-1: Don't trust X-Forwarded-For/X-Real-IP from clients.
	// Without this, attackers spoof XFF to bypass IP-based rate limits
	// (login limiter) and IP-based tracking. RemoteAddr is the real source.
	if err := r.SetTrustedProxies(nil); err != nil {
		slog.Warn("failed to set trusted proxies", "error", err)
	}

	// Middleware: request ID, security headers, gzip compression, auth, rate limit
	r.Use(ratelimit.RequestIDMiddleware())
	r.Use(ratelimit.SecurityHeadersMiddleware())
	r.Use(ratelimit.GzipMiddleware())
	// Anthropic /v1/messages: normalise x-api-key → Authorization: Bearer
	// BEFORE the main AuthMiddleware validates it.
	r.Use(anthropicAuthMiddleware())
	r.Use(AuthMiddleware(authMgr, sessions.Lookup))
	r.Use(ratelimit.Middleware(rateLimiter, authMgr))

	// API key management endpoints — admin only
	adminAuth := AdminMiddleware(authMgr)
	r.GET("/api/keys", adminAuth, handleListKeys(authMgr))
	r.POST("/api/keys", csrfGuard(), adminAuth, handleCreateKey(authMgr))
	r.DELETE("/api/keys/:key", csrfGuard(), adminAuth, handleDeleteKey(authMgr))
	r.PUT("/api/keys/:key", csrfGuard(), adminAuth, handleUpdateKey(authMgr))
	r.GET("/api/keys/:key/usage", adminAuth, handleKeyUsage(authMgr))

	r.GET("/dashboard", handleDashboard())
	r.GET("/login", handleLogin(authMgr, sessions))
	// P3 #6: rate limit /login POST by client IP (5/min, 20/hour) to prevent brute-force.
	loginLimiter := newLoginLimiter()
	r.POST("/login", loginLimiter.middleware(), handleLogin(authMgr, sessions))
	r.GET("/logout", handleLogout(sessions))
	r.GET("/health", handleHealth(grokAM, cbKM, hc, authMgr, sessions))
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
			s := k.Snapshot()
			entry := gin.H{
				"cred_type":      string(s.CredType),
				"credits_used":   s.CreditsUsed,
				"credit_limit":   CB_CREDIT_LIMIT,
				"credits_left":   CB_CREDIT_LIMIT - s.CreditsUsed,
				"total_requests": s.TotalReqs,
				"disabled":       s.Disabled,
			}
			if s.CredType == CBAuthOAuth {
				entry["email"] = s.Email
				entry["key"] = s.Email
				if !s.ExpiresAt.IsZero() {
					entry["expires_at"] = s.ExpiresAt.Format(time.RFC3339)
				}
			} else {
				keyDisplay := s.Key
				if len(keyDisplay) > 12 {
					keyDisplay = keyDisplay[:8] + "..." + keyDisplay[len(keyDisplay)-4:]
				}
				entry["key"] = keyDisplay
			}
			stats = append(stats, entry)
		}
		c.JSON(200, gin.H{"codebuddy_keys": stats})
	})
	r.POST("/accounts/refresh", csrfGuard(), adminAuth, handleRefresh(grokAM))
	r.POST("/accounts/import", csrfGuard(), adminAuth, handleImportAccount(grokAM))
	r.POST("/accounts/import/bulk", csrfGuard(), adminAuth, handleImportAccountBulk(grokAM))
	r.POST("/cb/import", csrfGuard(), adminAuth, handleImportCBKey(cbKM))
	r.POST("/cb/import/bulk", csrfGuard(), adminAuth, handleImportCBKeyBulk(cbKM))
	r.POST("/cb/oauth/import", csrfGuard(), adminAuth, handleImportCBOAuth(cbKM))
	r.DELETE("/accounts/:email", csrfGuard(), adminAuth, handleDeleteAccount(grokAM))
	r.DELETE("/cb/keys/:key", csrfGuard(), adminAuth, handleDeleteCBKey(cbKM))
	r.POST("/cleanup/disabled", csrfGuard(), adminAuth, handleCleanupDisabled(grokAM, cbKM))
	r.POST("/cleanup/banned", csrfGuard(), adminAuth, handleCleanupBanned(grokAM))
	r.GET("/history", adminAuth, handleHistory(db))
	r.GET("/history/recent", adminAuth, handleRecentRequests(db))
	r.GET("/history/detail/:id", adminAuth, handleHistoryDetail(db))

	// Custom models + aliases (v1.3.0) — admin only, runtime-configurable.
	// The /api/models/custom/*id catch-all preserves slashes in ids like
	// "cb/kimi-k3" (gin's :id param would only match one non-slash segment).
	r.GET("/api/models/custom", adminAuth, handleListCustomModels(customReg))
	r.POST("/api/models/custom", csrfGuard(), adminAuth, handleAddCustomModel(customReg))
	r.DELETE("/api/models/custom/*id", csrfGuard(), adminAuth, handleDeleteCustomModel(customReg))
	r.GET("/api/aliases", adminAuth, handleListAliases(customReg))
	r.POST("/api/aliases", csrfGuard(), adminAuth, handleAddAlias(customReg))
	r.DELETE("/api/aliases/*alias", csrfGuard(), adminAuth, handleDeleteAlias(customReg))

	// Combos (v1.4.0) — admin only. Combos group models under a virtual
	// "combo/<name>" alias with a strategy (fallback | round_robin).
	r.GET("/api/combos", adminAuth, handleListCombos(comboReg))
	r.POST("/api/combos", csrfGuard(), adminAuth, handleAddCombo(comboReg))
	r.GET("/api/combos/*name", adminAuth, handleGetCombo(comboReg))
	r.DELETE("/api/combos/*name", csrfGuard(), adminAuth, handleDeleteCombo(comboReg))

	// Proxy pool (v1.5.0) — admin only. Dashboard-managed HTTP/SOCKS5
	// proxies used by upstream (Grok/CodeBuddy/token-refresh) HTTP calls.
	r.GET("/api/proxies", adminAuth, handleListProxies(proxyPool))
	r.POST("/api/proxies", csrfGuard(), adminAuth, handleAddProxy(proxyPool))
	r.PUT("/api/proxies/:id", csrfGuard(), adminAuth, handleUpdateProxy(proxyPool))
	r.DELETE("/api/proxies/:id", csrfGuard(), adminAuth, handleDeleteProxy(proxyPool))
	r.POST("/api/proxies/:id/toggle", csrfGuard(), adminAuth, handleToggleProxy(proxyPool))
	r.POST("/api/proxies/:id/test", csrfGuard(), adminAuth, handleTestProxy(proxyPool))

	// Cloudflare Tunnel (v1.6.0) — admin only. Manages the embedded
	// cloudflared subprocess (data plane) + named-tunnel lifecycle via
	// cloudflare-go v7 SDK (control plane). Config persisted in Redis.
	r.GET("/api/tunnel/status", adminAuth, handleTunnelStatus(tunnelMgr))
	r.POST("/api/tunnel/enable", csrfGuard(), adminAuth, handleTunnelEnable(tunnelMgr))
	r.POST("/api/tunnel/disable", csrfGuard(), adminAuth, handleTunnelDisable(tunnelMgr))
	r.POST("/api/tunnel/restart", csrfGuard(), adminAuth, handleTunnelRestart(tunnelMgr))

	// /v1/*path catch-all — gin's httprouter doesn't allow a static
	// /v1/messages segment alongside /v1/*path, so we dispatch the
	// Anthropic Messages API adapter from inside the catch-all (POST only).
	// Auth is handled by the global AuthMiddleware (Bearer) +
	// anthropicAuthMiddleware (rewrites x-api-key → Authorization: Bearer).
	r.Any("/v1/*path", func(c *gin.Context) {
		if c.Request.URL.Path == "/v1/messages" && c.Request.Method == http.MethodPost {
			handleMessages(grokAM, cbKM, hc, authMgr, customReg, comboReg)(c)
			return
		}
		proxyRequest(grokAM, cbKM, hc, authMgr, customReg, comboReg)(c)
	})

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
	// Stop cloudflared subprocesses cleanly before Redis close so any
	// final status writes go through.
	tunnelMgr.Shutdown()
	// db.Close() runs via defer — drains async log channels best-effort
	slog.Info("stopped", "module", "server")
}
