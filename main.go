// foxrouters v5.0 — Unified OpenAI-compatible gateway for Grok + CodeBuddy.
// v4.0: Health checker (active + passive), circuit breaker, upstream latency tracking.
// v4.1: API key auth (Bearer token), per-client rate limiting (sliding window).
// v4.2: Web UI dashboard (/dashboard) — real-time stats, health, accounts, quick test.
// v5.0: Redis (hot state) + history DB — persistent accounts, request logs, analytics.
// v5.9: ClickHouse history (full request body, ZSTD) — PostgreSQL history retired.
// v1.7.0: ClickHouse removed — SQLite is the only history backend (deprecated Aug 2026).
// Routes by model name: grok-* → cli-chat-proxy.grok.com, cb-* → www.codebuddy.ai/v2.
// Grok: multi-account round-robin + auto refresh_token.
// CodeBuddy: multi-API-key round-robin, stream-only, auto system message injection.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"foxrouters/internal/auth"
	"foxrouters/internal/db"
	"foxrouters/internal/handlers"
	"foxrouters/internal/proxy"
	"foxrouters/internal/ratelimit"
	"foxrouters/internal/tunnel"
	"foxrouters/internal/upstream"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

//go:embed dashboard
var dashboardFS embed.FS

// dashboardHTML is assembled at package init from the modular dashboard/
// parts (head/body/modals HTML + core/pages JS). Splitting the SPA into
// per-layer files keeps future edits small and reviewable while the served
// payload stays byte-identical to the old single-file version.
var dashboardHTML = assembleDashboard()

// landingHTML is the public info page served at GET / (no account/key counts).
var landingHTML = assembleLanding()

func assembleLanding() string {
	b, err := dashboardFS.ReadFile("dashboard/landing.html")
	if err != nil {
		panic("dashboard embed missing landing.html: " + err.Error())
	}
	html := string(b)
	// Inject the fox logo (shared with the dashboard header) and the version.
	if i := strings.Index(html, "__LOGO__"); i >= 0 {
		if lb, err2 := dashboardFS.ReadFile("dashboard/fox_logo.txt"); err2 == nil {
			html = strings.Replace(html, "__LOGO__", strings.TrimSpace(string(lb)), 1)
		}
	}
	return strings.ReplaceAll(html, "__VERSION__", Version)
}

func assembleDashboard() string {
	read := func(name string) string {
		b, err := dashboardFS.ReadFile("dashboard/" + name)
		if err != nil {
			panic("dashboard embed missing " + name + ": " + err.Error())
		}
		return string(b)
	}
	// NOTE: script tags carry their own trailing newline — do not add "\n"
	// between "<script>\n" and the JS body or the payload shifts by 1 byte.
	return read("head.html") +
		read("body.html") +
		"<script>\n" + read("core.js") + "</script>\n" +
		read("modals.html") +
		"<script>\n" + read("pages.js") + "</script>\n" +
		read("foot.html")
}

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

// isLoopbackAddr reports whether a listen address binds loopback only.
// Accepts "127.x.x.x:port", "localhost:port", and bare ":port" is NOT
// loopback (binds all interfaces).
func isLoopbackAddr(addr string) bool {
	host := addr
	if i := strings.IndexByte(addr, ':'); i >= 0 {
		host = addr[:i]
	}
	if host == "" {
		return false // ":port" = all interfaces
	}
	if strings.HasPrefix(host, "127.") {
		return true
	}
	// resolve hostnames (localhost, ::1) — only accept localhost literally
	return host == "localhost" || host == "::1"
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
	RATE_LIMIT_RPM    = 60              // requests per minute per client
	RATE_LIMIT_BURST  = 10              // max burst (allow short spikes)
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
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve", "server":
			runServer()
			return
		case "version", "--version", "-v":
			fmt.Println(Version)
			return
		case "config":
			os.Exit(runConfig(os.Args[2:]))
			return
		case "install":
			os.Exit(runInstall(os.Args[2:]))
			return
		case "update":
			os.Exit(runUpdate(os.Args[2:]))
			return
		case "health":
			os.Exit(runHealth(os.Args[2:]))
			return
		case "stop":
			os.Exit(runStop(os.Args[2:]))
			return
		case "help", "--help", "-h":
			printUsage()
			return
		}
	}
	runServer() // no args → serve (backward compatible with systemd/docker)
}

func printUsage() {
	fmt.Printf(`FoxRouters %s — AI Gateway (Grok + CodeBuddy + Freebuff + Alibaba)

Usage:
  foxrouters                start the gateway server (default)
  foxrouters serve          start the gateway server
  foxrouters version        print version and exit
  foxrouters config         interactive editor for the config file
  foxrouters install        interactive first-install wizard (port + Redis)
  foxrouters stop           stop the gateway (systemd / docker / PID)
  foxrouters config list    print current config (.env, secrets masked)
  foxrouters config get K   print one config value
  foxrouters config set K V set/update a config value (.env)
  foxrouters update         update to the latest GitHub release (self-replace)
  foxrouters update --tag=vX.Y.Z  update to a specific release
  foxrouters health         probe the local gateway health endpoint
  foxrouters help           show this help

Config file: %s (override with FOXROUTERS_ENV)
`, Version, cliEnvFile())
}

func runServer() {
	port := os.Getenv("PORT")
	if port == "" {
		port = DEFAULT_PORT
	}

	// Initialize Redis + SQLite log store
	db, err := db.NewStore()
	if err != nil {
		slog.Error("DB init failed", "module", "main", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// Content filter config: Redis gw:config (Settings page) → runtime.
	proxy.LoadFilterConfig(db)

	grokAM := upstream.NewGrokAccountManager(db)
	if err := grokAM.LoadFromRedis(); err != nil {
		slog.Warn("LoadFromRedis failed, starting empty", "module", "grok", "error", err)
	}

	cbKM := upstream.NewCBKeyManager(db)
	if err := cbKM.LoadFromRedis(); err != nil {
		slog.Warn("LoadFromRedis failed, starting empty", "module", "cb", "error", err)
	}

	fbAM := upstream.NewFreebuffAccountManager(db)
	if err := fbAM.LoadFromRedis(); err != nil {
		slog.Warn("Freebuff LoadFromRedis failed, starting empty", "module", "freebuff", "error", err)
	}

	aliAM := upstream.NewAlibabaKeyManager(db)
	if err := aliAM.LoadFromRedis(); err != nil {
		slog.Warn("Alibaba LoadFromRedis failed, starting empty", "module", "alibaba", "error", err)
	}
	upstream.LoadSelectorMode(db)     // restore persisted CB selector mode (rr|sticky|content-hash|hybrid)
	upstream.LoadGrokSelectorMode(db) // restore persisted Grok selector mode

	// Health checker: active + passive monitoring with circuit breaker.
	// Gated by HEALTH_PROBES_DISABLED=1 — dev containers must NOT probe
	// upstream: probes burn credits and can flip circuit state on shared
	// pool credentials.
	hc := upstream.NewHealthChecker(grokAM, cbKM)
	healthProbesDisabled := os.Getenv("HEALTH_PROBES_DISABLED") == "1"
	// Worker context — cancelled on SIGTERM so all background goroutines
	// (health checks, token refresh, credit sync, cooldown re-enable, pool
	// gauges) exit cleanly during the drain window instead of running until
	// the process is force-killed.
	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	if healthProbesDisabled {
		slog.Warn("health probes DISABLED via HEALTH_PROBES_DISABLED=1", "module", "main")
	} else {
		hc.Start(workerCtx)
	}

	// Auth + rate limiter
	authMgr := auth.NewManager(db)
	sessions := auth.NewSessionStore() // P3-3: session token indirection (cookie ≠ API key)
	rateLimiter := ratelimit.New(RATE_LIMIT_RPM, RATE_LIMIT_BURST, RATE_LIMIT_WINDOW)
	// (rateLimiter previously carried a db handle for rate-limited request
	//  logging, but nothing actually consumed it — dropped in the split.)

	// Custom models + aliases registry (v1.3.0). Redis-backed, cached in
	// memory — reloaded on every mutation via handlers.
	customReg := proxy.NewCustomRegistry(db)
	if err := customReg.Load(); err != nil {
		slog.Warn("custom registry Load failed, starting empty", "module", "custom", "error", err)
	}

	// Combos registry (v1.4.0). Redis-backed, cached in memory.
	comboReg := proxy.NewComboRegistry(db)
	if err := comboReg.Load(); err != nil {
		slog.Warn("combo registry Load failed, starting empty", "module", "combo", "error", err)
	}

	// Proxy pool (v1.5.0). Redis-backed, cached in memory. Wired into
	// internal/upstream so Grok / CodeBuddy / token-refresh HTTP calls
	// route through enabled proxies (round-robin).
	proxyPool := proxy.NewProxyPool(db)
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

	// Background workers that hit upstream (token refresh, credit sync,
	// billing sync, re-enable). Gated by WORKERS_DISABLED=1 — dev containers
	// must NOT run these: Grok/CB use rotating refresh tokens, so two
	// instances racing on the same account triggers invalid_grant and
	// permanently disables the credential in BOTH environments.
	workersDisabled := os.Getenv("WORKERS_DISABLED") == "1"
	if workersDisabled {
		slog.Warn("background workers DISABLED via WORKERS_DISABLED=1", "module", "main")
	} else {
		go upstream.AutoRefreshWorker(workerCtx, grokAM)
		go upstream.ReenableWorker(workerCtx, grokAM)
		go upstream.ReenableCBWorker(workerCtx, cbKM)
		go upstream.CBOAuthRefreshWorker(workerCtx, cbKM)
		go upstream.CBCreditSyncWorker(workerCtx, cbKM)
		go upstream.GrokBillingSyncWorker(workerCtx, grokAM)
		go upstream.FbQuotaSyncWorker(workerCtx, fbAM)
		go upstream.FBStreakWorker(workerCtx, fbAM, upstream.FreebuffStreakInterval())
		go upstream.FbModelsWorker(workerCtx)
		go upstream.GrokModelsWorker(workerCtx, grokAM)
		go upstream.AliModelsWorker(workerCtx, aliAM)
	}
	// Snapshot pool sizes into Prometheus gauges every 10s. Cheap RLock walk;
	// keeps activeKeys/disabledKeys eventually consistent without touching the
	// hot path. Circuit-state gauges are updated inline from health.go.
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		updatePoolGauges(grokAM, cbKM, authMgr) // prime once
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-t.C:
				updatePoolGauges(grokAM, cbKM, authMgr)
			}
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
	// BEFORE the main auth.AuthMiddleware validates it.
	r.Use(handlers.AnthropicAuthMiddleware())
	r.Use(auth.AuthMiddleware(authMgr, sessions.Lookup))
	r.Use(ratelimit.Middleware(rateLimiter, authMgr))

	// API key management endpoints — admin only
	adminAuth := auth.AdminMiddleware(authMgr)
	r.GET("/api/keys", adminAuth, handlers.HandleListKeys(authMgr))
	r.POST("/api/keys", csrfGuard(), adminAuth, handlers.HandleCreateKey(authMgr))
	r.DELETE("/api/keys/:key", csrfGuard(), adminAuth, handlers.HandleDeleteKey(authMgr))
	r.PUT("/api/keys/:key", csrfGuard(), adminAuth, handlers.HandleUpdateKey(authMgr))
	r.GET("/api/keys/:key/usage", adminAuth, handlers.HandleKeyUsage(authMgr))

	r.GET("/dashboard", handlers.HandleDashboard())
	r.GET("/login", handlers.HandleLogin(authMgr, sessions))
	// P3 #6: rate limit /login POST by client IP (5/min, 20/hour) to prevent brute-force.
	loginLimiter := newLoginLimiter()
	r.POST("/login", loginLimiter.middleware(), handlers.HandleLogin(authMgr, sessions))
	r.POST("/logout", csrfGuard(), handlers.HandleLogout(sessions))
	r.GET("/health", handlers.HandleHealth(grokAM, cbKM, fbAM, aliAM, hc, authMgr, sessions))
	r.HEAD("/health", handlers.HandleHealthMinimal())
	// Prometheus scrape endpoint — admin only (exposes pool sizes, traffic
	// patterns, circuit state). External scrapers should authenticate via
	// Bearer token or run on a private network.
	r.GET("/metrics", adminAuth, gin.WrapH(promhttp.Handler()))
	// Accounts/keys/history — admin only (inference keys must not see other tenants' data)
	r.GET("/accounts", adminAuth, handlers.HandleAccounts(grokAM, cbKM))
	r.GET("/cb-stats", adminAuth, func(c *gin.Context) {
		page := handlers.ParsePage(c.Query("page"))
		pageSize := handlers.ParsePageSize(c.Query("page_size"))
		keys := cbKM.GetAll()
		start, end := handlers.PageRange(page, pageSize, len(keys))
		stats := []gin.H{}
		for _, k := range keys[start:end] {
			s := k.Snapshot()
			remain := s.CreditsRemain
			if remain == 0 && s.MeterSyncedAt.IsZero() {
				remain = s.CreditLimit - s.CreditsUsed
			}
			entry := gin.H{
				"cred_type":      string(s.CredType),
				"credits_used":   s.CreditsUsed,
				"credit_limit":   s.CreditLimit,
				"credits_remain": remain,
				"credits_left":   remain,
				"total_requests": s.TotalReqs,
				"disabled":       s.Disabled,
				"package_name":   s.PackageName,
				"cycle_end":      s.CycleEnd,
				"meter_status":   s.MeterStatus,
			}
			if !s.MeterSyncedAt.IsZero() {
				entry["meter_synced_at"] = s.MeterSyncedAt.Format(time.RFC3339)
			}
			if s.CredType == upstream.CBAuthOAuth {
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
		c.JSON(200, gin.H{"codebuddy_keys": stats, "cb_total": len(keys), "cb_page": page, "cb_page_size": pageSize})
	})
	r.POST("/accounts/refresh", csrfGuard(), adminAuth, handlers.HandleRefresh(grokAM))
	r.POST("/accounts/import", csrfGuard(), adminAuth, handlers.HandleImportAccount(grokAM))
	r.POST("/accounts/import/bulk", csrfGuard(), adminAuth, handlers.HandleImportAccountBulk(grokAM))
	r.POST("/cb/import", csrfGuard(), adminAuth, handlers.HandleImportCBKey(cbKM))
	r.POST("/cb/import/bulk", csrfGuard(), adminAuth, handlers.HandleImportCBKeyBulk(cbKM))
	r.POST("/cb/oauth/import", csrfGuard(), adminAuth, handlers.HandleImportCBOAuth(cbKM))
	r.POST("/cb/oauth/import/bulk", csrfGuard(), adminAuth, handlers.HandleImportCBOAuthBulk(cbKM))
	r.POST("/cb/oauth/device/start", csrfGuard(), adminAuth, handlers.HandleCBOAuthDeviceStart())
	r.GET("/cb/oauth/device/poll", csrfGuard(), adminAuth, handlers.HandleCBOAuthDevicePoll())
	r.POST("/cb/credits/sync", csrfGuard(), adminAuth, handlers.HandleSyncCBCredits(cbKM))
	r.POST("/accounts/billing/sync", csrfGuard(), adminAuth, handlers.HandleSyncGrokBilling(grokAM))
	// CB selector mode: rr | sticky | content-hash | hybrid (runtime switch, Redis-persisted)
	r.GET("/cb/selector-mode", adminAuth, func(c *gin.Context) {
		c.JSON(200, gin.H{
			"mode":          string(upstream.GetSelectorMode()),
			"valid_modes":   []string{string(upstream.SelectorRR), string(upstream.SelectorSticky), string(upstream.SelectorContentHash), string(upstream.SelectorHybrid)},
			"sticky_active": cbKM.StickyCount(),
		})
	})
	r.PUT("/cb/selector-mode", csrfGuard(), adminAuth, func(c *gin.Context) {
		var body struct {
			Mode string `json:"mode"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": "invalid json"})
			return
		}
		if err := upstream.SetSelectorMode(db, upstream.CBSelectorMode(body.Mode)); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"mode": body.Mode, "persisted": true})
	})
	// Grok selector mode: rr | sticky | content-hash | hybrid (runtime switch, Redis-persisted)
	r.GET("/grok/selector-mode", adminAuth, func(c *gin.Context) {
		c.JSON(200, gin.H{
			"mode":          string(upstream.GetGrokSelectorMode()),
			"valid_modes":   []string{string(upstream.GrokSelectorRR), string(upstream.GrokSelectorSticky), string(upstream.GrokSelectorContentHash), string(upstream.GrokSelectorHybrid)},
			"sticky_active": grokAM.StickyCount(),
		})
	})
	r.PUT("/grok/selector-mode", csrfGuard(), adminAuth, func(c *gin.Context) {
		var body struct {
			Mode string `json:"mode"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": "invalid json"})
			return
		}
		if err := upstream.SetGrokSelectorMode(db, upstream.GrokSelectorMode(body.Mode)); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"mode": body.Mode, "persisted": true})
	})
	r.POST("/cb/keys/test", csrfGuard(), adminAuth, handlers.HandleTestCBKey(cbKM))
	r.POST("/cb/keys/disable", csrfGuard(), adminAuth, handlers.HandleDisableCBKey(cbKM))
	r.POST("/cb/keys/enable", csrfGuard(), adminAuth, handlers.HandleEnableCBKey(cbKM))
	// Content filter settings (dashboard Settings page) — runtime + Redis gw:config
	r.GET("/settings/filters", adminAuth, func(c *gin.Context) {
		rules := proxy.FiltersList()
		out := make([]gin.H, 0, len(rules))
		for _, r := range rules {
			out = append(out, gin.H{"id": r.ID, "label": r.Label, "is_active": proxy.FilterActive(r.ID)})
		}
		c.JSON(200, gin.H{"enabled": proxy.FiltersEnabled(), "rules": out})
	})
	r.PUT("/settings/filters", csrfGuard(), adminAuth, func(c *gin.Context) {
		var body struct {
			Enabled bool `json:"enabled"`
			Rules   []struct {
				ID       string `json:"id"`
				IsActive bool   `json:"is_active"`
			} `json:"rules"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": "invalid json"})
			return
		}
		active := map[string]bool{}
		for _, r := range body.Rules {
			if r.ID != "" {
				active[r.ID] = r.IsActive
			}
		}
		proxy.SetFilterConfig(body.Enabled, active)
		if db != nil {
			_ = db.SetGWConfig("filters_enabled", map[bool]string{true: "1", false: "0"}[body.Enabled])
			b, _ := json.Marshal(active)
			_ = db.SetGWConfig("filters_active_rules", string(b))
		}
		slog.Info("content filters updated", "module", "settings", "enabled", body.Enabled, "rules", len(active))
		c.JSON(200, gin.H{"status": "saved", "enabled": body.Enabled, "rules": len(active)})
	})
	r.POST("/accounts/test", csrfGuard(), adminAuth, handlers.HandleTestGrokAccount(grokAM))
	r.POST("/fb/accounts/test", csrfGuard(), adminAuth, handlers.HandleTestFBAccount(fbAM))
	r.POST("/fb/streak/checkin", csrfGuard(), adminAuth, handlers.HandleFBStreakCheckin(fbAM))
	r.DELETE("/accounts/:email", csrfGuard(), adminAuth, handlers.HandleDeleteAccount(grokAM))
	r.DELETE("/cb/keys/:key", csrfGuard(), adminAuth, handlers.HandleDeleteCBKey(cbKM))
	r.POST("/fb/import", csrfGuard(), adminAuth, handlers.HandleFBImport(fbAM))
	r.POST("/fb/import/bulk", csrfGuard(), adminAuth, handlers.HandleFBImportBulk(fbAM))
	r.POST("/fb/quota/sync", csrfGuard(), adminAuth, handlers.HandleFBQuotaSync(fbAM))
	r.POST("/models/refresh", csrfGuard(), adminAuth, handlers.HandleModelsRefresh(grokAM, aliAM))
	r.GET("/fb/accounts", adminAuth, handlers.HandleFBAccounts(fbAM))
	r.DELETE("/fb/accounts/:token", csrfGuard(), adminAuth, handlers.HandleFBDeleteAccount(fbAM))
	r.POST("/fb/oauth/device/start", csrfGuard(), adminAuth, handlers.HandleFBDeviceStart(fbAM))
	r.GET("/fb/oauth/device/poll", adminAuth, handlers.HandleFBDevicePoll(fbAM))

	// Alibaba (DashScope) keys
	r.POST("/ali/import", csrfGuard(), adminAuth, handlers.HandleAliImport(aliAM))
	r.POST("/ali/import/bulk", csrfGuard(), adminAuth, handlers.HandleAliImportBulk(aliAM))
	r.GET("/ali/accounts", adminAuth, handlers.HandleAliAccounts(aliAM))
	r.GET("/ali/models/usage", adminAuth, handlers.HandleAliModelUsage(aliAM))
	r.POST("/ali/accounts/delete", csrfGuard(), adminAuth, handlers.HandleAliDeleteAccount(aliAM))
	r.POST("/ali/keys/disable", csrfGuard(), adminAuth, handlers.HandleAliDisable(aliAM))
	r.POST("/ali/keys/enable", csrfGuard(), adminAuth, handlers.HandleAliEnable(aliAM))
	r.POST("/ali/keys/test", csrfGuard(), adminAuth, handlers.HandleAliTest(aliAM))
	r.POST("/cleanup/disabled", csrfGuard(), adminAuth, handlers.HandleCleanupDisabled(grokAM, cbKM))
	r.POST("/cleanup/banned", csrfGuard(), adminAuth, handlers.HandleCleanupBanned(grokAM))
	r.GET("/history", adminAuth, handlers.HandleHistory(db))
	r.GET("/history/recent", adminAuth, handlers.HandleRecentRequests(db))
	r.GET("/history/detail/:id", adminAuth, handlers.HandleHistoryDetail(db))

	// Custom models + aliases (v1.3.0) — admin only, runtime-configurable.
	// The /api/models/custom/*id catch-all preserves slashes in ids like
	// "cb/kimi-k3" (gin's :id param would only match one non-slash segment).
	r.GET("/api/models/custom", adminAuth, handlers.HandleListCustomModels(customReg))
	r.POST("/api/models/custom", csrfGuard(), adminAuth, handlers.HandleAddCustomModel(customReg))
	r.DELETE("/api/models/custom/*id", csrfGuard(), adminAuth, handlers.HandleDeleteCustomModel(customReg))
	r.GET("/api/aliases", adminAuth, handlers.HandleListAliases(customReg))
	r.POST("/api/aliases", csrfGuard(), adminAuth, handlers.HandleAddAlias(customReg))
	r.DELETE("/api/aliases/*alias", csrfGuard(), adminAuth, handlers.HandleDeleteAlias(customReg))

	// Combos (v1.4.0) — admin only. Combos group models under a virtual
	// "combo/<name>" alias with a strategy (fallback | round_robin).
	r.GET("/api/combos", adminAuth, handlers.HandleListCombos(comboReg))
	r.POST("/api/combos", csrfGuard(), adminAuth, handlers.HandleAddCombo(comboReg))
	r.GET("/api/combos/*name", adminAuth, handlers.HandleGetCombo(comboReg))
	r.DELETE("/api/combos/*name", csrfGuard(), adminAuth, handlers.HandleDeleteCombo(comboReg))

	// Proxy pool (v1.5.0) — admin only. Dashboard-managed HTTP/SOCKS5
	// proxies used by upstream (Grok/CodeBuddy/token-refresh) HTTP calls.
	r.GET("/api/proxies", adminAuth, handlers.HandleListProxies(proxyPool))
	r.POST("/api/proxies", csrfGuard(), adminAuth, handlers.HandleAddProxy(proxyPool))
	r.PUT("/api/proxies/:id", csrfGuard(), adminAuth, handlers.HandleUpdateProxy(proxyPool))
	r.DELETE("/api/proxies/:id", csrfGuard(), adminAuth, handlers.HandleDeleteProxy(proxyPool))
	r.POST("/api/proxies/:id/toggle", csrfGuard(), adminAuth, handlers.HandleToggleProxy(proxyPool))
	r.POST("/api/proxies/:id/test", csrfGuard(), adminAuth, handlers.HandleTestProxy(proxyPool))

	// Cloudflare Tunnel (v1.6.0) — admin only. Manages the embedded
	// cloudflared subprocess (data plane) + named-tunnel lifecycle via
	// cloudflare-go v7 SDK (control plane). Config persisted in Redis.
	r.GET("/api/tunnel/status", adminAuth, handlers.HandleTunnelStatus(tunnelMgr))
	r.POST("/api/tunnel/enable", csrfGuard(), adminAuth, handlers.HandleTunnelEnable(tunnelMgr))
	r.POST("/api/tunnel/disable", csrfGuard(), adminAuth, handlers.HandleTunnelDisable(tunnelMgr))
	r.POST("/api/tunnel/restart", csrfGuard(), adminAuth, handlers.HandleTunnelRestart(tunnelMgr))

	// /v1/*path catch-all — gin's httprouter doesn't allow a static
	// /v1/messages segment alongside /v1/*path, so we dispatch the
	// Anthropic Messages API adapter from inside the catch-all (POST only).
	// Auth is handled by the global auth.AuthMiddleware (Bearer) +
	// handlers.AnthropicAuthMiddleware (rewrites x-api-key → Authorization: Bearer).
	r.Any("/v1/*path", func(c *gin.Context) {
		if c.Request.URL.Path == "/v1/messages" && c.Request.Method == http.MethodPost {
			handlers.HandleMessages(grokAM, cbKM, fbAM, aliAM, hc, authMgr, customReg, comboReg)(c)
			return
		}
		if c.Request.URL.Path == "/v1/images/generations" && c.Request.Method == http.MethodPost {
			// Media Studio — Alibaba DashScope (qwen-image / wan). Grok console
			// SSO+DPoP+Turnstile path removed (see grok_images.go — legacy).
			handlers.HandleAliImages(aliAM)(c)
			return
		}
		if c.Request.URL.Path == "/v1/images/edits" && c.Request.Method == http.MethodPost {
			handlers.HandleAliImagesEdit(aliAM)(c)
			return
		}
		if c.Request.URL.Path == "/v1/videos/generations" && c.Request.Method == http.MethodPost {
			handlers.HandleAliVideoCreate(aliAM)(c)
			return
		}
		if strings.HasPrefix(c.Request.URL.Path, "/v1/videos/") && c.Request.Method == http.MethodGet {
			handlers.HandleAliVideoStatus(aliAM)(c)
			return
		}
		proxy.ProxyRequest(grokAM, cbKM, fbAM, aliAM, hc, authMgr, customReg, comboReg)(c)
	})

	r.GET("/", func(c *gin.Context) {
		// Public landing page — deliberately no account/key counts (info leak).
		// Version is baked in at assembly time; health status is fetched
		// client-side from /health.
		c.Data(200, "text/html; charset=utf-8", []byte(landingHTML))
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
	// Bind address. Default ":port" = all interfaces. When GATEWAY_AUTH_DISABLE=1
	// (dev mode) an explicit loopback bind is REQUIRED — running with auth
	// disabled on a non-loopback address would expose every admin endpoint.
	bindAddr := os.Getenv("GATEWAY_BIND")
	if bindAddr == "" {
		bindAddr = ":" + port
	}
	if os.Getenv("GATEWAY_AUTH_DISABLE") == "1" && !isLoopbackAddr(bindAddr) {
		slog.Error("refusing to start with GATEWAY_AUTH_DISABLE=1 on non-loopback bind",
			"module", "server", "bind", bindAddr,
			"hint", "set GATEWAY_BIND=127.0.0.1:"+port+" (or remove GATEWAY_AUTH_DISABLE)")
		os.Exit(1)
	}
	srv := &http.Server{
		Addr:              bindAddr,
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

	// Cancel background workers first so they stop making upstream calls
	// during the drain window (prevents wasted API credits + partial writes).
	workerCancel()

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
