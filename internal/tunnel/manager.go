// Package tunnel is the first-class Cloudflare Tunnel integration for
// FoxRouters. It runs cloudflared as a child subprocess (data plane) and
// drives named-tunnel lifecycle via the cloudflare-go v7 SDK (control
// plane). Config lives in Redis so mode + credentials survive restarts.
//
// Modes:
//
//	none    — no tunnel running (default).
//	quick   — `cloudflared tunnel --url http://127.0.0.1:<port>` produces
//	          a random *.trycloudflare.com URL (rotates on restart).
//	named   — SDK-managed named tunnel with a persistent domain. Requires
//	          Cloudflare API token + account ID + zone ID + hostname.
//	hybrid  — both quick and named running in parallel.
//
// The manager runs cloudflared as an ordinary child process. No Docker,
// no separate containers — one gateway binary, one (or two) cloudflared
// subprocesses. Logs are captured to an in-memory ring buffer so
// callers (the dashboard) can display the last few hundred lines.
package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/dns"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/zero_trust"
	"github.com/redis/go-redis/v9"
)

// Mode enumerates the supported tunnel run modes. Stored in Redis so the
// gateway can auto-start the same mode on boot.
type Mode string

const (
	ModeNone   Mode = "none"
	ModeQuick  Mode = "quick"
	ModeNamed  Mode = "named"
	ModeHybrid Mode = "hybrid"
)

// IsValid reports whether m is a known mode.
func (m Mode) IsValid() bool {
	switch m {
	case ModeNone, ModeQuick, ModeNamed, ModeHybrid:
		return true
	}
	return false
}

// tunnelName is the default named-tunnel name registered with Cloudflare.
// Kept constant so re-enabling reuses the same tunnel instead of piling
// up duplicates in the operator's account.
const tunnelName = "foxrouters"

// redisConfigKey is the single Redis key that carries the persisted
// tunnel configuration as a JSON blob.
const redisConfigKey = "fr:tunnel:config"

// logRingSize bounds the per-subprocess log buffer. Keeps memory bounded
// even if cloudflared spams (e.g. on network hiccups).
const logRingSize = 400

// quickURLTimeout is how long startQuick waits for cloudflared to emit
// the *.trycloudflare.com URL before giving up (best effort — the
// process stays running either way).
const quickURLTimeout = 30 * time.Second

// quickURLRe matches a *.trycloudflare.com URL in cloudflared stdout.
var quickURLRe = regexp.MustCompile(`https://[a-zA-Z0-9.-]+\.trycloudflare\.com`)

// Config is the persisted tunnel state. Credentials are stored in Redis
// verbatim (they need to be usable at boot without any operator
// interaction) and MUST NOT be logged. Handlers redact them before
// returning to the dashboard.
type Config struct {
	Mode                Mode   `json:"mode"`
	CloudflareAPIToken  string `json:"cloudflare_api_token,omitempty"`
	CloudflareAccountID string `json:"cloudflare_account_id,omitempty"`
	CloudflareZoneID    string `json:"cloudflare_zone_id,omitempty"`
	TunnelDomain        string `json:"tunnel_domain,omitempty"`
	TunnelID            string `json:"tunnel_id,omitempty"`
	TunnelToken         string `json:"tunnel_token,omitempty"`
	QuickURL            string `json:"quick_url,omitempty"`
	UpstreamURL         string `json:"upstream_url,omitempty"`
	UpdatedAt           int64  `json:"updated_at,omitempty"`
}

// Status is what the dashboard reads. It mirrors Config's non-secret
// fields plus derived runtime state.
type Status struct {
	Mode         Mode   `json:"mode"`
	QuickRunning bool   `json:"quick_running"`
	NamedRunning bool   `json:"named_running"`
	QuickURL     string `json:"quick_url,omitempty"`
	TunnelDomain string `json:"tunnel_domain,omitempty"`
	TunnelID     string `json:"tunnel_id,omitempty"`
	UpstreamURL  string `json:"upstream_url,omitempty"`
	// Redacted credentials — only booleans, so the dashboard can show
	// "configured" without leaking the actual value.
	HasAPIToken bool  `json:"has_api_token"`
	HasAccount  bool  `json:"has_account_id"`
	HasZone     bool  `json:"has_zone_id"`
	UpdatedAt   int64 `json:"updated_at,omitempty"`
	// Recent log lines from cloudflared subprocesses (both quick + named
	// concatenated, prefixed with [quick]/[named]). Capped at logRingSize
	// lines per subprocess.
	Logs []string `json:"logs,omitempty"`
}

// Manager owns cloudflared subprocess lifecycle and named-tunnel control
// plane. Concurrency-safe.
type Manager struct {
	redis           *redis.Client
	cloudflaredPath string
	upstreamURL     string

	mu        sync.Mutex
	quickCmd  *exec.Cmd
	quickLog  *ringBuffer
	quickURL  string
	namedCmd  *exec.Cmd
	namedLog  *ringBuffer
}

// NewManager builds a Manager. cloudflaredPath is the filesystem path to
// the cloudflared binary (embedded in the gateway container at
// /usr/local/bin/cloudflared by default). upstreamURL is what
// cloudflared should proxy to — typically http://127.0.0.1:<PORT>
// because the gateway binds to localhost.
func NewManager(rdb *redis.Client, cloudflaredPath, upstreamURL string) *Manager {
	return &Manager{
		redis:           rdb,
		cloudflaredPath: cloudflaredPath,
		upstreamURL:     upstreamURL,
		quickLog:        newRingBuffer(logRingSize),
		namedLog:        newRingBuffer(logRingSize),
	}
}

// UpstreamURL returns the URL cloudflared subprocesses proxy to.
func (m *Manager) UpstreamURL() string { return m.upstreamURL }

// LoadConfig reads the persisted config from Redis. Returns a zero-value
// Config (mode=none) when nothing is stored.
func (m *Manager) LoadConfig() (*Config, error) {
	if m == nil || m.redis == nil {
		return &Config{Mode: ModeNone}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err := m.redis.Get(ctx, redisConfigKey).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return &Config{Mode: ModeNone}, nil
		}
		return nil, fmt.Errorf("redis GET: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if !cfg.Mode.IsValid() {
		cfg.Mode = ModeNone
	}
	return &cfg, nil
}

// SaveConfig persists the config to Redis. Sets UpdatedAt to now.
func (m *Manager) SaveConfig(cfg *Config) error {
	if m == nil || m.redis == nil {
		return errors.New("redis not initialised")
	}
	if cfg == nil {
		return errors.New("nil config")
	}
	cfg.UpdatedAt = time.Now().Unix()
	if cfg.UpstreamURL == "" {
		cfg.UpstreamURL = m.upstreamURL
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.redis.Set(ctx, redisConfigKey, string(data), 0).Err(); err != nil {
		return fmt.Errorf("redis SET: %w", err)
	}
	return nil
}

// Status returns the current runtime status (safe to expose to the
// dashboard — no credentials).
func (m *Manager) Status() (*Status, error) {
	cfg, err := m.LoadConfig()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	quickRunning := m.quickCmd != nil && m.quickCmd.Process != nil && m.quickCmd.ProcessState == nil
	namedRunning := m.namedCmd != nil && m.namedCmd.Process != nil && m.namedCmd.ProcessState == nil
	quickURL := m.quickURL
	// Snapshot recent logs — [quick] first, then [named] — bounded.
	var logs []string
	for _, l := range m.quickLog.Snapshot() {
		logs = append(logs, "[quick] "+l)
	}
	for _, l := range m.namedLog.Snapshot() {
		logs = append(logs, "[named] "+l)
	}
	m.mu.Unlock()

	if quickURL == "" {
		quickURL = cfg.QuickURL
	}

	return &Status{
		Mode:         cfg.Mode,
		QuickRunning: quickRunning,
		NamedRunning: namedRunning,
		QuickURL:     quickURL,
		TunnelDomain: cfg.TunnelDomain,
		TunnelID:     cfg.TunnelID,
		UpstreamURL:  cfg.UpstreamURL,
		HasAPIToken:  cfg.CloudflareAPIToken != "",
		HasAccount:   cfg.CloudflareAccountID != "",
		HasZone:      cfg.CloudflareZoneID != "",
		UpdatedAt:    cfg.UpdatedAt,
		Logs:         logs,
	}, nil
}

// Enable stops any existing subprocesses, applies cfg, persists it, and
// starts the requested mode. Passing cfg.Mode = ModeNone is equivalent
// to Disable().
func (m *Manager) Enable(cfg *Config) error {
	if cfg == nil {
		return errors.New("nil config")
	}
	if !cfg.Mode.IsValid() {
		return fmt.Errorf("invalid mode: %q", cfg.Mode)
	}
	// Stop everything first — even if the mode is unchanged, we want a
	// clean slate (e.g. domain change requires re-registering DNS).
	m.stopAll()

	if cfg.UpstreamURL == "" {
		cfg.UpstreamURL = m.upstreamURL
	}

	switch cfg.Mode {
	case ModeNone:
		// nothing to start; just persist the disabled state
	case ModeQuick:
		if err := m.startQuick(cfg); err != nil {
			return fmt.Errorf("quick start: %w", err)
		}
	case ModeNamed:
		if err := m.startNamed(cfg); err != nil {
			return fmt.Errorf("named start: %w", err)
		}
	case ModeHybrid:
		// Best-effort: try both, return an error only if BOTH fail. This
		// mirrors tunnel.sh's behaviour where a partial hybrid (only
		// quick, or only named) is still useful.
		qerr := m.startQuick(cfg)
		nerr := m.startNamed(cfg)
		if qerr != nil && nerr != nil {
			return fmt.Errorf("hybrid start: quick=%v named=%v", qerr, nerr)
		}
		if qerr != nil {
			slog.Warn("hybrid: quick start failed", "module", "tunnel", "error", qerr)
		}
		if nerr != nil {
			slog.Warn("hybrid: named start failed", "module", "tunnel", "error", nerr)
		}
	}

	if err := m.SaveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	return nil
}

// Disable stops all subprocesses and persists mode=none. Credentials
// stay in Redis so a subsequent Enable(named) can reuse them.
func (m *Manager) Disable() error {
	m.stopAll()
	cfg, err := m.LoadConfig()
	if err != nil {
		return err
	}
	cfg.Mode = ModeNone
	cfg.QuickURL = ""
	return m.SaveConfig(cfg)
}

// Restart re-applies the current persisted mode. Useful after
// cloudflared silently exits (network partition, upstream restart).
func (m *Manager) Restart() error {
	cfg, err := m.LoadConfig()
	if err != nil {
		return err
	}
	if cfg.Mode == ModeNone {
		return nil
	}
	return m.Enable(cfg)
}

// AutoStart is called once at gateway boot: loads the persisted mode
// and, if it's not ModeNone, starts the tunnel best-effort. Errors are
// logged but not returned — a broken tunnel must NOT block the gateway
// itself.
func (m *Manager) AutoStart() {
	cfg, err := m.LoadConfig()
	if err != nil {
		slog.Warn("tunnel LoadConfig failed", "module", "tunnel", "error", err)
		return
	}
	if cfg.Mode == ModeNone {
		return
	}
	slog.Info("auto-starting tunnel", "module", "tunnel", "mode", cfg.Mode)
	if err := m.Enable(cfg); err != nil {
		slog.Warn("tunnel auto-start failed", "module", "tunnel", "mode", cfg.Mode, "error", err)
	}
}

// Shutdown stops all subprocesses. Meant for graceful gateway shutdown —
// does NOT touch Redis so the mode persists across restarts.
func (m *Manager) Shutdown() {
	m.stopAll()
}

// ============================================================================
// QUICK TUNNEL
// ============================================================================

func (m *Manager) startQuick(cfg *Config) error {
	if m.cloudflaredPath == "" {
		return errors.New("cloudflared binary path not set")
	}
	if _, err := os.Stat(m.cloudflaredPath); err != nil {
		return fmt.Errorf("cloudflared binary not found at %s: %w", m.cloudflaredPath, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.quickCmd != nil {
		return errors.New("quick tunnel already running")
	}

	upstream := cfg.UpstreamURL
	if upstream == "" {
		upstream = m.upstreamURL
	}

	// #nosec G204 — cloudflaredPath is operator-configured (env var), not
	// user input; upstream comes from our own config.
	cmd := exec.Command(m.cloudflaredPath,
		"tunnel", "--no-autoupdate",
		"--url", upstream,
		"--metrics", "127.0.0.1:0", // avoid clashing with default metrics port
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	m.quickCmd = cmd
	m.quickLog.Reset()
	m.quickURL = ""

	// Reader goroutines feed the ring buffer + look for the URL.
	urlCh := make(chan string, 1)
	go m.consume(stdout, m.quickLog, urlCh)
	go m.consume(stderr, m.quickLog, urlCh)

	// URL capture is best-effort in a goroutine so we don't block the
	// caller. tunnel.sh has the same pattern (poll logs for 30s).
	go func() {
		select {
		case url := <-urlCh:
			m.mu.Lock()
			m.quickURL = url
			m.mu.Unlock()
			// Persist so the dashboard sees it after gateway restart.
			cfg, err := m.LoadConfig()
			if err == nil {
				cfg.QuickURL = url
				if saveErr := m.SaveConfig(cfg); saveErr != nil {
					slog.Warn("quick URL persist failed", "module", "tunnel", "error", saveErr)
				}
			}
			slog.Info("quick tunnel URL", "module", "tunnel", "url", url)
		case <-time.After(quickURLTimeout):
			slog.Warn("quick URL capture timeout — check logs", "module", "tunnel")
		}
	}()

	// Reap the process when it exits so ProcessState is populated.
	go func() {
		_ = cmd.Wait()
		m.mu.Lock()
		if m.quickCmd == cmd {
			// keep m.quickCmd around so Status shows exited state via
			// ProcessState; clear only when explicitly stopped.
		}
		m.mu.Unlock()
	}()

	return nil
}

// consume forwards subprocess output to the ring buffer and looks for
// the quick-tunnel URL, sending it once on urlCh when found.
func (m *Manager) consume(r io.Reader, ring *ringBuffer, urlCh chan<- string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	sentURL := false
	for sc.Scan() {
		line := sc.Text()
		ring.Add(line)
		if !sentURL {
			if match := quickURLRe.FindString(line); match != "" {
				select {
				case urlCh <- match:
					sentURL = true
				default:
				}
			}
		}
	}
}

// ============================================================================
// NAMED TUNNEL
// ============================================================================

func (m *Manager) startNamed(cfg *Config) error {
	if cfg.CloudflareAPIToken == "" || cfg.CloudflareAccountID == "" || cfg.CloudflareZoneID == "" || cfg.TunnelDomain == "" {
		return errors.New("named tunnel requires CloudflareAPIToken, CloudflareAccountID, CloudflareZoneID, and TunnelDomain")
	}
	if _, err := os.Stat(m.cloudflaredPath); err != nil {
		return fmt.Errorf("cloudflared binary not found at %s: %w", m.cloudflaredPath, err)
	}

	upstream := cfg.UpstreamURL
	if upstream == "" {
		upstream = m.upstreamURL
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Cloudflare-go v7 client. Uses http.DefaultTransport which has
	// ForceAttemptHTTP2=true — required for CF API (HTTP/1.1 hangs).
	// Do NOT override DialContext — it breaks HTTP/2 ALPN negotiation.
	client := cloudflare.NewClient(option.WithAPIToken(cfg.CloudflareAPIToken))

	// 1. Create tunnel (or reuse existing by ID if we already have one).
	tunnelID, token, err := m.createOrReuseTunnel(ctx, client, cfg)
	if err != nil {
		return fmt.Errorf("create/reuse tunnel: %w", err)
	}
	cfg.TunnelID = tunnelID
	cfg.TunnelToken = token

	// 2. Configure ingress: hostname -> upstream, catch-all 404.
	if err := m.configureIngress(ctx, client, cfg, tunnelID, upstream); err != nil {
		return fmt.Errorf("configure ingress: %w", err)
	}

	// 3. Route DNS CNAME <domain> -> <tunnelID>.cfargotunnel.com.
	if err := m.routeDNS(ctx, client, cfg, tunnelID); err != nil {
		return fmt.Errorf("route DNS: %w", err)
	}

	// 4. Exec cloudflared with the connector token.
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.namedCmd != nil {
		return errors.New("named tunnel already running")
	}
	// #nosec G204 — see startQuick.
	cmd := exec.Command(m.cloudflaredPath,
		"tunnel", "--no-autoupdate", "run",
		"--token", token,
	)
	// Suppress the token from ps by scrubbing PATH-tunneling env leaks;
	// cloudflared still reads it via argv, so the token IS visible in
	// /proc — that's inherent to cloudflared's CLI design.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	m.namedCmd = cmd
	m.namedLog.Reset()
	// No URL to capture for named — hostname is fixed. Reuse the same
	// consume() so logs feed the ring.
	sink := make(chan string, 1) // drained but unused
	go m.consume(stdout, m.namedLog, sink)
	go m.consume(stderr, m.namedLog, sink)
	go func() { _ = cmd.Wait() }()

	slog.Info("named tunnel started", "module", "tunnel", "domain", cfg.TunnelDomain, "tunnel_id", tunnelID)
	return nil
}

func (m *Manager) createOrReuseTunnel(ctx context.Context, _ *cloudflare.Client, cfg *Config) (tunnelID, token string, err error) {
	// Raw HTTP implementation — bypasses cloudflare-go SDK which hangs
	// in the gateway runtime (HTTP/2 ALPN issue with SDK's transport setup).
	baseURL := "https://api.cloudflare.com/client/v4"
	authHeader := "Bearer " + cfg.CloudflareAPIToken

	// If we already have a tunnel_id, fetch a fresh token for it.
	if cfg.TunnelID != "" {
		tokenURL := fmt.Sprintf("%s/accounts/%s/cfd_tunnel/%s/token", baseURL, cfg.CloudflareAccountID, cfg.TunnelID)
		req, _ := http.NewRequestWithContext(ctx, "GET", tokenURL, nil)
		req.Header.Set("Authorization", authHeader)
		resp, gerr := http.DefaultClient.Do(req)
		if gerr == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				var tokResp struct {
					Success bool   `json:"success"`
					Result  string `json:"result"`
				}
				if json.Unmarshal(body, &tokResp) == nil && tokResp.Success && tokResp.Result != "" {
					slog.Info("reusing existing tunnel", "module", "tunnel", "id", cfg.TunnelID)
					return cfg.TunnelID, tokResp.Result, nil
				}
			}
		}
		slog.Warn("existing tunnel token fetch failed, recreating", "module", "tunnel", "tunnel_id", cfg.TunnelID)
	}

	// Create fresh tunnel.
	slog.Info("creating tunnel via raw HTTP", "module", "tunnel", "account_id", cfg.CloudflareAccountID, "name", tunnelName)
	createBody, _ := json.Marshal(map[string]string{
		"name":        tunnelName,
		"config_src":  "cloudflare",
		"tunnel_secret": randomHex(32),
	})
	createURL := fmt.Sprintf("%s/accounts/%s/cfd_tunnel", baseURL, cfg.CloudflareAccountID)
	req, _ := http.NewRequestWithContext(ctx, "POST", createURL, bytes.NewReader(createBody))
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("tunnel creation HTTP failed", "module", "tunnel", "error", err, "elapsed", time.Since(start))
		return "", "", fmt.Errorf("create tunnel HTTP: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	slog.Info("tunnel creation response", "module", "tunnel", "status", resp.StatusCode, "elapsed", time.Since(start))
	if resp.StatusCode != 200 {
		// Truncate CF error body — may contain account metadata, never log raw
		bodySnippet := string(body)
		if len(bodySnippet) > 200 {
			bodySnippet = bodySnippet[:200] + "..."
		}
		return "", "", fmt.Errorf("create tunnel: HTTP %d: %s", resp.StatusCode, bodySnippet)
	}
	var createResp struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &createResp); err != nil {
		return "", "", fmt.Errorf("create tunnel: parse: %w", err)
	}
	if !createResp.Success || createResp.Result.ID == "" {
		return "", "", fmt.Errorf("create tunnel: API error: %v", createResp.Errors)
	}
	tunnelID = createResp.Result.ID
	slog.Info("tunnel created", "module", "tunnel", "id", tunnelID)

	// Fetch connector token.
	tokenURL := fmt.Sprintf("%s/accounts/%s/cfd_tunnel/%s/token", baseURL, cfg.CloudflareAccountID, tunnelID)
	tokenReq, _ := http.NewRequestWithContext(ctx, "GET", tokenURL, nil)
	tokenReq.Header.Set("Authorization", authHeader)
	tokenResp, err := http.DefaultClient.Do(tokenReq)
	if err != nil {
		return "", "", fmt.Errorf("get token: %w", err)
	}
	tokenBody, _ := io.ReadAll(tokenResp.Body)
	tokenResp.Body.Close()
	if tokenResp.StatusCode != 200 {
		return "", "", fmt.Errorf("get token: HTTP %d: %s", tokenResp.StatusCode, string(tokenBody))
	}
	var tokResp struct {
		Success bool   `json:"success"`
		Result  string `json:"result"`
	}
	if json.Unmarshal(tokenBody, &tokResp) != nil || !tokResp.Success || tokResp.Result == "" {
		return "", "", fmt.Errorf("get token: parse error")
	}
	return tunnelID, tokResp.Result, nil
}

func (m *Manager) configureIngress(ctx context.Context, client *cloudflare.Client, cfg *Config, tunnelID, upstream string) error {
	ingress := []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
		{
			Hostname: cloudflare.F(cfg.TunnelDomain),
			Service:  cloudflare.F(upstream),
		},
		{
			// Catch-all — required or the tunnel refuses the config.
			Service: cloudflare.F("http_status:404"),
		},
	}
	_, err := client.ZeroTrust.Tunnels.Cloudflared.Configurations.Update(ctx, tunnelID, zero_trust.TunnelCloudflaredConfigurationUpdateParams{
		AccountID: cloudflare.F(cfg.CloudflareAccountID),
		Config: cloudflare.F(zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfig{
			Ingress: cloudflare.F(ingress),
		}),
	})
	return err
}

func (m *Manager) routeDNS(ctx context.Context, client *cloudflare.Client, cfg *Config, tunnelID string) error {
	cnameTarget := tunnelID + ".cfargotunnel.com"

	// Look for existing record by name. list -> update-or-create.
	page, err := client.DNS.Records.List(ctx, dns.RecordListParams{
		ZoneID: cloudflare.F(cfg.CloudflareZoneID),
		Name: cloudflare.F(dns.RecordListParamsName{
			Exact: cloudflare.F(cfg.TunnelDomain),
		}),
	})
	if err != nil {
		return fmt.Errorf("list DNS: %w", err)
	}
	var existingID string
	if page != nil && len(page.Result) > 0 {
		existingID = page.Result[0].ID
	}

	body := dns.CNAMERecordParam{
		Name:    cloudflare.F(cfg.TunnelDomain),
		Type:    cloudflare.F(dns.CNAMERecordTypeCNAME),
		Content: cloudflare.F(cnameTarget),
		TTL:     cloudflare.F(dns.TTL(1)), // 1 = automatic
		Proxied: cloudflare.F(true),
	}

	if existingID != "" {
		_, err := client.DNS.Records.Update(ctx, existingID, dns.RecordUpdateParams{
			ZoneID: cloudflare.F(cfg.CloudflareZoneID),
			Body:   body,
		})
		return err
	}
	_, err = client.DNS.Records.New(ctx, dns.RecordNewParams{
		ZoneID: cloudflare.F(cfg.CloudflareZoneID),
		Body:   body,
	})
	return err
}

// ============================================================================
// SUBPROCESS LIFECYCLE
// ============================================================================

// stopAll kills both cloudflared subprocesses. Called by Enable (before
// starting) and Disable / Shutdown.
func (m *Manager) stopAll() {
	m.mu.Lock()
	q := m.quickCmd
	n := m.namedCmd
	m.quickCmd = nil
	m.namedCmd = nil
	m.quickURL = ""
	m.mu.Unlock()
	killGracefully(q, "quick")
	killGracefully(n, "named")
}

// killGracefully sends SIGTERM, waits up to 5s, then SIGKILLs.
func killGracefully(cmd *exec.Cmd, label string) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// If it already exited, nothing to do.
	if cmd.ProcessState != nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		slog.Warn("cloudflared did not exit within 5s, killing", "module", "tunnel", "label", label)
		_ = cmd.Process.Kill()
		<-done
	}
}

// ============================================================================
// RING BUFFER (bounded per-subprocess log capture)
// ============================================================================

type ringBuffer struct {
	mu    sync.Mutex
	buf   []string
	size  int
	start int
	count int
}

func newRingBuffer(size int) *ringBuffer {
	if size <= 0 {
		size = 100
	}
	return &ringBuffer{buf: make([]string, size), size: size}
}

func (r *ringBuffer) Add(line string) {
	if r == nil {
		return
	}
	// Trim absurdly long lines so a bad log entry can't blow memory.
	if len(line) > 4096 {
		line = line[:4096] + "…"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count < r.size {
		r.buf[(r.start+r.count)%r.size] = line
		r.count++
		return
	}
	r.buf[r.start] = line
	r.start = (r.start + 1) % r.size
}

func (r *ringBuffer) Snapshot() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, r.count)
	for i := 0; i < r.count; i++ {
		out = append(out, r.buf[(r.start+i)%r.size])
	}
	return out
}

func (r *ringBuffer) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.start = 0
	r.count = 0
}

// MaskToken shortens a Cloudflare-connector token for display. Prevents
// accidental leaks in logs while preserving the "configured / not" bit.
func MaskToken(tok string) string {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return ""
	}
	if len(tok) <= 12 {
		return "***"
	}
	return tok[:6] + "…" + tok[len(tok)-4:]
}

// randomHex generates n random bytes as a hex string (2n chars).
func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
