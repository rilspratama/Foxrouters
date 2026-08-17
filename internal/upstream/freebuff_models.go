package upstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"log/slog"
)

// FreebuffAPIBase returns the base URL for session/chat/ads/streak/run API
// calls. Runtime-overridable: SetFreebuffAPIBase stores the value in an
// atomic.Value (hot path) AND persists it to Redis (fb:config hash) so the
// operator's choice survives restarts. Precedence at boot:
//
//  1. fb:config.api_base from Redis (persisted runtime override)
//  2. FREEBUFF_BASE_URL env (relay for full-access tier)
//  3. https://www.codebuff.com (direct)
//
// The device flow (OAuth login URL + poll) always hits codebuff.com via
// FREEBUFF_DEVICE_BASE — the browser PKCE callback is origin-bound and relays
// cannot handle it.
var freebuffAPIBase atomic.Value // holds string

func init() {
	freebuffAPIBase.Store(defaultFreebuffAPIBase())
}

func defaultFreebuffAPIBase() string {
	if v := os.Getenv("FREEBUFF_BASE_URL"); v != "" {
		// Prefer a validated value; fall back to the compiled default if the
		// env override is rejected (e.g. http:// without the insecure flag).
		if norm, err := validateFreebuffAPIBase(v); err == nil {
			return norm
		} else {
			slog.Warn("FREEBUFF_BASE_URL rejected, using default", "module", "freebuff", "error", err)
		}
	}
	return "https://www.codebuff.com"
}

// FreebuffAPIBase returns the current API base URL (atomic read, hot path).
func FreebuffAPIBase() string {
	if v, ok := freebuffAPIBase.Load().(string); ok && v != "" {
		return v
	}
	return "https://www.codebuff.com"
}

// fbBaseMaxLen bounds the accepted api_base URL to prevent absurd values.
const fbBaseMaxLen = 256

// validateFreebuffAPIBase normalizes + validates a base URL. It is the single
// choke point for ALL Freebuff api_base inputs (env, Redis-persisted override,
// PUT /fb/config handler) so no validation can be bypassed. Rules:
//   - must be http(s); http:// is rejected unless FB_ALLOW_INSECURE_BASE=1
//   - must parse as a URL with a non-empty host
//   - no userinfo, no path/query/fragment (base URL only)
//   - host must not resolve to loopback/private/link-local/unspecified
//   - max length fbBaseMaxLen
//
// Returns the normalized URL (trimmed, single trailing slash stripped) or an
// error describing the rejection.
func validateFreebuffAPIBase(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", errors.New("api_base must not be empty")
	}
	if len(v) > fbBaseMaxLen {
		return "", fmt.Errorf("api_base too long (%d > %d)", len(v), fbBaseMaxLen)
	}
	// Strip a single trailing slash (not TrimRight — that would eat "https://").
	v = strings.TrimSuffix(v, "/")
	u, err := url.Parse(v)
	if err != nil {
		return "", fmt.Errorf("invalid api_base URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && !(scheme == "http" && os.Getenv("FB_ALLOW_INSECURE_BASE") == "1") {
		return "", fmt.Errorf("api_base must use https:// (http:// requires FB_ALLOW_INSECURE_BASE=1)")
	}
	if u.Host == "" {
		return "", errors.New("api_base must include a host")
	}
	if u.User != nil {
		return "", errors.New("api_base must not include userinfo")
	}
	if u.Path != "" && u.Path != "/" {
		return "", errors.New("api_base must be a bare origin (no path)")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("api_base must be a bare origin (no query/fragment)")
	}
	// Reject loopback / private / link-local / unspecified hosts (SSRF guard).
	if err := rejectNonPublicHost(u.Hostname()); err != nil {
		return "", err
	}
	return v, nil
}

// fbSSRFDenyPrefixes is the explicit deny list for the SSRF guard (SEC2-03):
// IANA special-purpose + private + link-local + CGNAT + benchmarking + NAT64
// ranges. Go's net.IP.IsPrivate() only covers RFC1918 + fc00::/7, so the
// rest must be listed explicitly.
var fbSSRFDenyPrefixes = []netip.Prefix{
	// Loopback
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	// RFC1918 private
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	// CGNAT (RFC 6598) — Go's IsPrivate() misses this
	netip.MustParsePrefix("100.64.0.0/10"),
	// Link-local
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("fe80::/10"),
	// Benchmarking (RFC 2544)
	netip.MustParsePrefix("198.18.0.0/15"),
	// Reserved / future use
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("192.0.0.0/24"),
	// NAT64 / IPv4-compatible + mapped (catch ::ffff:127.0.0.1)
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("::/96"),
	// Unspecified + multicast
	netip.MustParsePrefix("0.0.0.0/32"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("ff00::/8"),
}

// rejectNonPublicHost rejects loopback/private/link-local/unspecified literal
// IPs, and hostnames that resolve to any such address (SSRF guard — the base
// URL is used with Freebuff OAuth bearer tokens attached). Fails CLOSED on
// resolution failure (SEC2-02): an unresolvable host is not accepted, since a
// host can be NXDOMAIN at set time and resolve to an internal address later.
func rejectNonPublicHost(host string) error {
	if ip := net.ParseIP(host); ip != nil {
		if rejectNonPublicIP(ip) {
			return fmt.Errorf("api_base host %q is not a public address", host)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		// SEC2-02: fail closed — unresolvable hosts are rejected, not allowed.
		return fmt.Errorf("api_base host %q could not be resolved: %w", host, err)
	}
	for _, ip := range ips {
		if rejectNonPublicIP(ip.IP) {
			return fmt.Errorf("api_base host %q resolves to a non-public address", host)
		}
	}
	return nil
}

// rejectNonPublicIP reports whether ip is loopback, private (RFC1918 +
// CGNAT), link-local, multicast, benchmarking, reserved, or unspecified —
// the address classes an SSRF guard must never dial. Used at both validation
// time (setter) and dial time (Freebuff HTTP client, C2-07) so DNS-rebinding
// cannot redirect an authorized api_base to an internal address after
// validation.
func rejectNonPublicIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true // unparseable → reject
	}
	addr = addr.Unmap()
	for _, p := range fbSSRFDenyPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// NormalizeFreebuffAPIBase is the exported validation entry point used by the
// /fb/config handler (validates without mutating state, returns the normalized
// URL). SetFreebuffAPIBase applies the same rules then stores.
func NormalizeFreebuffAPIBase(raw string) (string, error) {
	return validateFreebuffAPIBase(raw)
}

// SetFreebuffAPIBase validates + updates the in-memory base URL. Returns an
// error (and does NOT change anything) when the value fails validation.
// Persisting to Redis is the caller's responsibility (see LoadFreebuffAPIBase
// / the /fb/config handler) so tests can set it without a store.
func SetFreebuffAPIBase(url string) error {
	norm, err := validateFreebuffAPIBase(url)
	if err != nil {
		return err
	}
	freebuffAPIBase.Store(norm)
	return nil
}

// SetFreebuffAPIBaseValidated applies a value that ALREADY passed
// NormalizeFreebuffAPIBase, skipping re-validation (SEC2-04: the handler must
// not resolve DNS twice per PUT — a hanging resolver would pin the gin
// goroutine, and a second lookup reopens the validate→validate TOCTOU window).
// The caller MUST have validated the exact same normalized string first.
func SetFreebuffAPIBaseValidated(norm string) {
	freebuffAPIBase.Store(norm)
}

// LoadFreebuffAPIBase restores a persisted runtime override from Redis
// (fb:config.api_base) at startup. Called once before serving; a persisted
// value wins over the env default. If the persisted value fails validation it
// is logged and skipped (falls back to env/default) rather than poisoning the
// running gateway.
func LoadFreebuffAPIBase(store interface {
	GetFBConfig(field string) (string, error)
}) {
	if store == nil {
		return
	}
	if v, err := store.GetFBConfig("api_base"); err == nil && v != "" {
		if err := SetFreebuffAPIBase(v); err != nil {
			slog.Warn("fb:config api_base rejected at boot, using env/default", "module", "freebuff", "error", err)
		}
	}
}

// FREEBUFF_DEVICE_BASE is always codebuff.com — device flow (OAuth login URL +
// poll) MUST hit codebuff.com directly because the browser OAuth callback
// (PKCE) is bound to the codebuff.com origin. Relays cannot handle this.
const FREEBUFF_DEVICE_BASE = "https://www.codebuff.com"

const (
	FREEBUFF_CHAT_PATH    = "/api/v1/chat/completions"
	FREEBUFF_SESSION_PATH = "/api/v1/freebuff/session"
	FREEBUFF_RUN_PATH     = "/api/v1/agent-runs"
	FREEBUFF_ME_PATH      = "/api/v1/me"
	FREEBUFF_STREAK_PATH  = "/api/v1/freebuff/streak"
	FREEBUFF_ADS_PATH     = "/api/v1/ads"

	FREEBUFF_BUFFY_PREFIX = "You are Buffy, the strategic coding assistant."

	// Timeouts
	FREEBUFF_UPSTREAM_TIMEOUT  = 25 * time.Second
	FREEBUFF_SESSION_TIMEOUT   = 12 * time.Second
	FREEBUFF_NONSTREAM_TIMEOUT = 50 * time.Second

	// Session cache: reuse if >60s remaining
	FREEBUFF_SESSION_MIN_REMAINING = 60 * time.Second

	// Run cache: reuse runId for 10 minutes
	FREEBUFF_RUN_CACHE_TTL = 10 * time.Minute

	// Redis key prefixes (persistent session/run cache — survives gateway restart)
	fbSessionKeyPrefix = "fb:session:"
	fbRunKeyPrefix     = "fb:run:"
)

// ErrFBBanned marks an account permanently banned by Freebuff upstream.
// Detected from POST /session 403 {"status":"banned"} or GET /session status:"banned".
var ErrFBBanned = errors.New("freebuff account banned")

// ErrFBQuotaExceeded signals upstream rate limit (429) hit during session
// creation — the daily session quota is consumed (e.g. 6/6), so the account
// must be cooled down instead of immediately retried.
var ErrFBQuotaExceeded = errors.New("freebuff quota exceeded")

// FreebuffModels maps gateway model IDs (fb/ prefix) to upstream config.
type FreebuffModelConfig struct {
	GatewayID string // e.g. "fb/deepseek-v4-flash"
	Upstream  string // e.g. "deepseek/deepseek-v4-flash"
	Agent     string // e.g. "base2-free-deepseek-flash"
	Reasoning bool
	FullMode  bool // true = only available in full access mode (US/EU exit)
}

var FreebuffModels = []FreebuffModelConfig{
	{"fb/deepseek-v4-flash", "deepseek/deepseek-v4-flash", "base2-free-deepseek-flash", true, false},
	{"fb/mimo-v2.5", "mimo/mimo-v2.5", "base2-free-mimo", false, false},
	{"fb/deepseek-v4-pro", "deepseek/deepseek-v4-pro", "base2-free-deepseek", true, true},
	{"fb/minimax-m3", "minimax/minimax-m3", "base2-free-minimax-m3", false, true},
	{"fb/gpt-5.6-luna", "openai/gpt-5.6-luna", "base2-free-luna", false, true},
	{"fb/glm-5.2", "z-ai/glm-5.2", "base2-free-glm", true, true},
}

// IsFreebuffModel returns true if the model routes to the Freebuff upstream.
func IsFreebuffModel(model string) bool {
	return strings.HasPrefix(model, "fb/")
}

// fbStripPrefix removes the "fb/" prefix → upstream model name.
func fbStripPrefix(model string) string {
	return strings.TrimPrefix(model, "fb/")
}

// fbModelConfig looks up the model config by gateway ID (dynamic registry
// first, static fallback).
func fbModelConfig(gatewayID string) *FreebuffModelConfig {
	models := GetFBModels()
	for i := range models {
		if models[i].GatewayID == gatewayID {
			return &models[i]
		}
	}
	// Default to deepseek-v4-flash
	return &models[0]
}
