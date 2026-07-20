// Package proxy — ProxyPool: dashboard-managed HTTP/SOCKS5 proxy pool used
// by upstream (Grok, CodeBuddy, token-refresh) HTTP clients.
//
// Design:
//
//   - Redis-backed. Every entry is a HASH at fr:proxy:<id>; the set of
//     enabled ids lives at fr:proxy:enabled; a shared INCR counter at
//     fr:proxy:rr drives cluster-safe round-robin.
//
//   - In-memory cache. Callers hit Next() on the hot path — no Redis round
//     trip. The cache is refreshed on Add/Update/Delete/Toggle and on Load()
//     at startup. Reads use RLock; mutations swap the whole slice under the
//     write lock so readers never see a torn view.
//
//   - Transports are cached by proxy id (sync.Map). Creating a new
//     http.Transport per request would burn CPU and defeat keepalive; the
//     cache is invalidated on Update/Delete.
//
//   - Auto-disable. Five consecutive MarkFailed() calls flip the entry to
//     disabled + remove it from the enabled set; a successful Next() after
//     an entry-scoped success resets the counter. The dashboard can re-enable.
//
//   - Password masking. List() returns a mask ("***") for non-empty
//     passwords; the full password only appears on POST (Add) or via the
//     internal URL() builder used to configure the transport.
package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	nurl "net/url"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foxrouters/internal/db"

	xproxy "golang.org/x/net/proxy"
)

const (
	// PROXY_FAIL_THRESHOLD: consecutive failures before auto-disable.
	PROXY_FAIL_THRESHOLD = 5

	// PROXY_TEST_TIMEOUT is the per-request timeout used by Test().
	PROXY_TEST_TIMEOUT = 10 * time.Second

	// PROXY_TEST_URL is the reachability probe target.
	PROXY_TEST_URL = "http://httpbin.org/ip"
)

// ProxyEntry is the API-visible shape of one proxy pool entry.
type ProxyEntry struct {
	ID         string    `json:"id"`
	Protocol   string    `json:"protocol"` // "http" | "socks5"
	Host       string    `json:"host"`
	Port       int       `json:"port"`
	Username   string    `json:"username,omitempty"`
	Password   string    `json:"password,omitempty"`
	Enabled    bool      `json:"enabled"`
	Label      string    `json:"label,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	FailCount  int       `json:"fail_count"`
}

// URL returns the proxy URL suitable for http.Transport.Proxy or the SOCKS5
// dialer factory. Empty on invalid entries.
func (p *ProxyEntry) URL() string {
	if p == nil || p.Host == "" || p.Port == 0 {
		return ""
	}
	if p.Username != "" {
		return fmt.Sprintf("%s://%s:%s@%s:%d",
			p.Protocol,
			nurl.QueryEscape(p.Username),
			nurl.QueryEscape(p.Password),
			p.Host, p.Port)
	}
	return fmt.Sprintf("%s://%s:%d", p.Protocol, p.Host, p.Port)
}

// masked returns a copy of the entry with password redacted to "***" when
// non-empty. Zero-length passwords stay empty (nothing to hide).
func (p ProxyEntry) masked() ProxyEntry {
	if p.Password != "" {
		p.Password = "***"
	}
	return p
}

// ProxyPool is the runtime container. Zero value is not usable — call
// NewProxyPool.
type ProxyPool struct {
	db *db.Store

	mu      sync.RWMutex
	entries []ProxyEntry // full list (cache)
	// enabled: pre-filtered slice of Enabled entries for Next(). Rebuilt on
	// mutation. Order matches insertion order in `entries` after a full
	// Load() (deterministic RR indexing across restarts uses Redis INCR).
	enabled []ProxyEntry

	// rrIndex is an in-process counter used when Redis is unavailable.
	rrIndex atomic.Uint64

	// transports caches *http.Transport by proxy id.
	transports sync.Map // map[string]*http.Transport
}

// NewProxyPool constructs an empty pool bound to a DB store.
func NewProxyPool(store *db.Store) *ProxyPool {
	return &ProxyPool{db: store}
}

// Load pulls all proxy entries from Redis and rebuilds the in-memory cache.
// Safe to call at startup and after external state resets.
func (p *ProxyPool) Load() error {
	if p == nil {
		return fmt.Errorf("pool not initialised")
	}
	var entries []ProxyEntry
	if p.db != nil {
		dtos, err := p.db.LoadProxies()
		if err != nil {
			return err
		}
		entries = make([]ProxyEntry, 0, len(dtos))
		for _, d := range dtos {
			entries = append(entries, dtoToEntry(d))
		}
	}
	p.mu.Lock()
	p.entries = entries
	p.rebuildEnabledLocked()
	p.mu.Unlock()
	return nil
}

// List returns a snapshot of all entries with passwords masked.
func (p *ProxyPool) List() []ProxyEntry {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]ProxyEntry, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, e.masked())
	}
	return out
}

// Get returns a single entry (masked) by id.
func (p *ProxyPool) Get(id string) (ProxyEntry, bool) {
	if p == nil || id == "" {
		return ProxyEntry{}, false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, e := range p.entries {
		if e.ID == id {
			return e.masked(), true
		}
	}
	return ProxyEntry{}, false
}

// Len returns the total number of entries.
func (p *ProxyPool) Len() int {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.entries)
}

// EnabledLen returns the number of currently enabled entries.
func (p *ProxyPool) EnabledLen() int {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.enabled)
}

// Add validates the entry, generates an ID, persists it to Redis, and
// updates the in-memory cache. Returns the created entry (with password
// preserved — the caller decides whether to surface it).
func (p *ProxyPool) Add(entry ProxyEntry) (ProxyEntry, error) {
	if p == nil {
		return ProxyEntry{}, fmt.Errorf("pool not initialised")
	}
	if err := validateEntry(&entry); err != nil {
		return ProxyEntry{}, err
	}
	id, err := newProxyID()
	if err != nil {
		return ProxyEntry{}, fmt.Errorf("id gen: %w", err)
	}
	entry.ID = id
	entry.CreatedAt = time.Now()
	entry.FailCount = 0
	// New entries default to enabled unless explicitly disabled by caller.

	if p.db != nil {
		if err := p.db.SaveProxy(entryToDTO(entry)); err != nil {
			return ProxyEntry{}, err
		}
	}
	p.mu.Lock()
	p.entries = append(p.entries, entry)
	p.rebuildEnabledLocked()
	p.mu.Unlock()
	return entry, nil
}

// Update replaces the entry with the given ID. Fields left blank on `in`
// fall back to the existing value (host/port/protocol/label). Password of
// "***" is treated as unchanged; any other value (including "") overwrites.
func (p *ProxyPool) Update(id string, in ProxyEntry) (ProxyEntry, error) {
	if p == nil {
		return ProxyEntry{}, fmt.Errorf("pool not initialised")
	}
	if id == "" {
		return ProxyEntry{}, fmt.Errorf("id required")
	}
	p.mu.Lock()
	idx := -1
	for i, e := range p.entries {
		if e.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		p.mu.Unlock()
		return ProxyEntry{}, fmt.Errorf("proxy not found: %s", id)
	}
	current := p.entries[idx]
	if in.Protocol != "" {
		current.Protocol = in.Protocol
	}
	if in.Host != "" {
		current.Host = in.Host
	}
	if in.Port != 0 {
		current.Port = in.Port
	}
	// Username/label can be reset to "" via explicit empty — we always take input.
	current.Username = in.Username
	current.Label = in.Label
	// Password: "***" means "keep existing"; anything else (including empty) overwrites.
	if in.Password != "***" {
		current.Password = in.Password
	}
	current.Enabled = in.Enabled
	if err := validateEntry(&current); err != nil {
		p.mu.Unlock()
		return ProxyEntry{}, err
	}
	// Reset fail count on any mutation — operator explicitly asked for this proxy again.
	current.FailCount = 0
	p.entries[idx] = current
	p.rebuildEnabledLocked()
	p.mu.Unlock()

	// Invalidate cached transport.
	p.transports.Delete(id)

	if p.db != nil {
		if err := p.db.SaveProxy(entryToDTO(current)); err != nil {
			return current, err
		}
	}
	return current, nil
}

// Delete removes an entry by id.
func (p *ProxyPool) Delete(id string) error {
	if p == nil {
		return fmt.Errorf("pool not initialised")
	}
	if id == "" {
		return fmt.Errorf("id required")
	}
	p.mu.Lock()
	next := p.entries[:0]
	found := false
	for _, e := range p.entries {
		if e.ID == id {
			found = true
			continue
		}
		next = append(next, e)
	}
	if !found {
		p.mu.Unlock()
		return fmt.Errorf("proxy not found: %s", id)
	}
	// Copy to a fresh slice so future appends don't alias.
	p.entries = append([]ProxyEntry(nil), next...)
	p.rebuildEnabledLocked()
	p.mu.Unlock()

	p.transports.Delete(id)

	if p.db != nil {
		return p.db.DeleteProxy(id)
	}
	return nil
}

// Toggle flips the Enabled flag on a single entry and returns the new state.
func (p *ProxyPool) Toggle(id string) (bool, error) {
	if p == nil {
		return false, fmt.Errorf("pool not initialised")
	}
	p.mu.Lock()
	idx := -1
	for i, e := range p.entries {
		if e.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		p.mu.Unlock()
		return false, fmt.Errorf("proxy not found: %s", id)
	}
	p.entries[idx].Enabled = !p.entries[idx].Enabled
	// Reset fail count when re-enabling so the operator gets a fresh try.
	if p.entries[idx].Enabled {
		p.entries[idx].FailCount = 0
	}
	updated := p.entries[idx]
	p.rebuildEnabledLocked()
	p.mu.Unlock()

	if p.db != nil {
		if err := p.db.SaveProxy(entryToDTO(updated)); err != nil {
			return updated.Enabled, err
		}
	}
	return updated.Enabled, nil
}

// Next returns the next enabled entry via round-robin. Returns
// (nil, error) when no proxies are enabled — callers treat this as a
// signal to use direct (no-proxy) connections.
func (p *ProxyPool) Next() (*ProxyEntry, error) {
	if p == nil {
		return nil, fmt.Errorf("pool not initialised")
	}
	p.mu.RLock()
	n := len(p.enabled)
	if n == 0 {
		p.mu.RUnlock()
		return nil, fmt.Errorf("no proxies enabled")
	}

	// Prefer Redis atomic INCR for cluster-safe RR, fall back to in-process
	// counter when Redis is unreachable.
	var idx int
	if p.db != nil {
		if v, err := p.db.IncrProxyRR(); err == nil {
			// v is 1-based
			idx = int((v - 1) % int64(n))
			if idx < 0 {
				idx += n
			}
		} else {
			v := p.rrIndex.Add(1)
			idx = int((v - 1) % uint64(n))
		}
	} else {
		v := p.rrIndex.Add(1)
		idx = int((v - 1) % uint64(n))
	}
	entry := p.enabled[idx]
	p.mu.RUnlock()

	// Best-effort async touch of last_used_at so the dashboard can show
	// "active in last 5m" without impacting the hot path.
	if p.db != nil {
		go p.db.UpdateProxyLastUsed(entry.ID, time.Now())
	}
	return &entry, nil
}

// MarkFailed increments the failure counter for a proxy. When it reaches
// PROXY_FAIL_THRESHOLD the entry is auto-disabled.
func (p *ProxyPool) MarkFailed(id string) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	idx := -1
	for i, e := range p.entries {
		if e.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		p.mu.Unlock()
		return
	}
	p.entries[idx].FailCount++
	fc := p.entries[idx].FailCount
	autoDisabled := false
	if fc >= PROXY_FAIL_THRESHOLD && p.entries[idx].Enabled {
		p.entries[idx].Enabled = false
		autoDisabled = true
		p.rebuildEnabledLocked()
	}
	updated := p.entries[idx]
	p.mu.Unlock()

	if autoDisabled {
		slog.Warn("proxy auto-disabled (fail threshold reached)",
			"module", "proxy-pool",
			"id", id, "fails", fc, "threshold", PROXY_FAIL_THRESHOLD)
		p.transports.Delete(id)
	}

	if p.db != nil {
		if autoDisabled {
			_ = p.db.SaveProxy(entryToDTO(updated))
		} else {
			p.db.UpdateProxyFailCount(id, fc)
		}
	}
}

// MarkSuccess resets the failure counter for a proxy after a good request.
func (p *ProxyPool) MarkSuccess(id string) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	changed := false
	for i, e := range p.entries {
		if e.ID != id {
			continue
		}
		if e.FailCount != 0 {
			p.entries[i].FailCount = 0
			changed = true
		}
		break
	}
	p.mu.Unlock()
	if changed && p.db != nil {
		p.db.UpdateProxyFailCount(id, 0)
	}
}

// Transport returns a cached *http.Transport for the given proxy entry. The
// returned transport is safe for concurrent use across goroutines and keeps
// its own idle-conn pool. Callers should NOT close it.
//
// idleTimeout / tlsHandshake are supplied by the caller to match the
// default transport settings from the upstream package.
func (p *ProxyPool) Transport(entry *ProxyEntry) (*http.Transport, error) {
	if p == nil || entry == nil {
		return nil, fmt.Errorf("nil entry")
	}
	if v, ok := p.transports.Load(entry.ID); ok {
		return v.(*http.Transport), nil
	}
	t, err := buildTransport(entry)
	if err != nil {
		return nil, err
	}
	// LoadOrStore so concurrent constructors converge on one transport.
	actual, _ := p.transports.LoadOrStore(entry.ID, t)
	return actual.(*http.Transport), nil
}

// Test opens a short-lived probe request through the proxy at `id` and
// returns the observed egress IP + latency. Does not affect the fail
// counter — the operator gets an explicit success/fail signal instead.
func (p *ProxyPool) Test(id string) (success bool, egressIP string, latencyMs int64, testErr error) {
	p.mu.RLock()
	var target *ProxyEntry
	for i := range p.entries {
		if p.entries[i].ID == id {
			e := p.entries[i]
			target = &e
			break
		}
	}
	p.mu.RUnlock()
	if target == nil {
		return false, "", 0, fmt.Errorf("proxy not found: %s", id)
	}

	transport, err := buildTransport(target)
	if err != nil {
		return false, "", 0, err
	}
	// Fresh, throwaway client — we don't want to cache a bad transport.
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Timeout:   PROXY_TEST_TIMEOUT,
		Transport: transport,
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), PROXY_TEST_TIMEOUT)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", PROXY_TEST_URL, nil)
	resp, err := client.Do(req)
	latencyMs = time.Since(start).Milliseconds()
	if err != nil {
		return false, "", latencyMs, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return false, "", latencyMs, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return true, strings.TrimSpace(string(body)), latencyMs, nil
}

// -------- helpers --------

// rebuildEnabledLocked must be called with the write lock held.
func (p *ProxyPool) rebuildEnabledLocked() {
	next := make([]ProxyEntry, 0, len(p.entries))
	for _, e := range p.entries {
		if e.Enabled {
			next = append(next, e)
		}
	}
	p.enabled = next
}

func newProxyID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func validateEntry(e *ProxyEntry) error {
	e.Protocol = strings.ToLower(strings.TrimSpace(e.Protocol))
	e.Host = strings.TrimSpace(e.Host)
	e.Label = strings.TrimSpace(e.Label)
	e.Username = strings.TrimSpace(e.Username)
	switch e.Protocol {
	case "http", "socks5":
	default:
		return fmt.Errorf("protocol must be 'http' or 'socks5' (got %q)", e.Protocol)
	}
	if e.Host == "" {
		return fmt.Errorf("host required")
	}
	if len(e.Host) > 253 {
		return fmt.Errorf("host too long (max 253 chars)")
	}
	// Reject anything that looks structural — spaces, control chars, embedded schemes.
	if strings.ContainsAny(e.Host, " \t\r\n/\\") {
		return fmt.Errorf("host contains invalid characters")
	}
	if e.Port < 1 || e.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535 (got %d)", e.Port)
	}
	if len(e.Label) > 64 {
		return fmt.Errorf("label too long (max 64 chars)")
	}
	if len(e.Username) > 256 || len(e.Password) > 256 {
		return fmt.Errorf("username/password too long (max 256 chars)")
	}
	return nil
}

func entryToDTO(e ProxyEntry) db.ProxyEntryDTO {
	return db.ProxyEntryDTO{
		ID:         e.ID,
		Protocol:   e.Protocol,
		Host:       e.Host,
		Port:       e.Port,
		Username:   e.Username,
		Password:   e.Password,
		Enabled:    e.Enabled,
		Label:      e.Label,
		CreatedAt:  e.CreatedAt,
		LastUsedAt: e.LastUsedAt,
		FailCount:  e.FailCount,
	}
}

func dtoToEntry(d db.ProxyEntryDTO) ProxyEntry {
	return ProxyEntry{
		ID:         d.ID,
		Protocol:   d.Protocol,
		Host:       d.Host,
		Port:       d.Port,
		Username:   d.Username,
		Password:   d.Password,
		Enabled:    d.Enabled,
		Label:      d.Label,
		CreatedAt:  d.CreatedAt,
		LastUsedAt: d.LastUsedAt,
		FailCount:  d.FailCount,
	}
}

// buildTransport returns an *http.Transport routing through the given
// proxy. HTTP proxies use net/http's built-in Proxy field; SOCKS5 uses
// golang.org/x/net/proxy for the connect dialer.
func buildTransport(entry *ProxyEntry) (*http.Transport, error) {
	if entry == nil {
		return nil, fmt.Errorf("nil entry")
	}
	base := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	switch entry.Protocol {
	case "http":
		u, err := nurl.Parse(entry.URL())
		if err != nil {
			return nil, fmt.Errorf("parse proxy URL: %w", err)
		}
		base.Proxy = http.ProxyURL(u)
		return base, nil
	case "socks5":
		var auth *xproxy.Auth
		if entry.Username != "" {
			auth = &xproxy.Auth{User: entry.Username, Password: entry.Password}
		}
		addr := fmt.Sprintf("%s:%d", entry.Host, entry.Port)
		dialer, err := xproxy.SOCKS5("tcp", addr, auth, &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second})
		if err != nil {
			return nil, fmt.Errorf("socks5 dialer: %w", err)
		}
		// ContextDialer support is optional — the returned dialer from
		// x/net/proxy usually implements it.
		if cd, ok := dialer.(xproxy.ContextDialer); ok {
			base.DialContext = cd.DialContext
		} else {
			base.DialContext = func(_ context.Context, network, address string) (net.Conn, error) {
				return dialer.Dial(network, address)
			}
		}
		// ForceAttemptHTTP2 requires a TLS transport; keeping HTTP2 opt-in
		// via ALPN when the origin negotiates it.
		return base, nil
	default:
		return nil, fmt.Errorf("unsupported protocol: %q", entry.Protocol)
	}
}
