// proxy_adapter.go — thin bridge from package-main call sites to internal/proxy.
// main.go still writes `proxyRequest(...)` as a route handler; that name now
// forwards into the extracted internal/proxy package.
package main

import (
	"net/http"

	"foxrouters/internal/proxy"
	"foxrouters/internal/upstream"
)

// proxyRequest preserves the lowercase name used by main.go's routes wiring.
var proxyRequest = proxy.ProxyRequest

// v1.3.0 — custom models + aliases registry.
type CustomRegistry = proxy.CustomRegistry

// NewCustomRegistry preserves the constructor name in package main.
var NewCustomRegistry = proxy.NewCustomRegistry

// v1.4.0 — combos registry.
type ComboRegistry = proxy.ComboRegistry

// NewComboRegistry preserves the constructor name in package main.
var NewComboRegistry = proxy.NewComboRegistry

// v1.5.0 — proxy pool.
type ProxyPool = proxy.ProxyPool

// NewProxyPool preserves the constructor name in package main.
var NewProxyPool = proxy.NewProxyPool

// upstreamProxyPoolAdapter bridges the concrete *proxy.ProxyPool over to the
// abstract upstream.ProxyPool interface. The interface exists because
// internal/upstream cannot import internal/proxy (proxy already imports
// upstream — cycle). The adapter is trivial: forward each method and rewrap
// the entry struct.
type upstreamProxyPoolAdapter struct {
	pool *proxy.ProxyPool
}

func (a upstreamProxyPoolAdapter) Next(upstreamName string) (*upstream.ProxyEntry, error) {
	e, err := a.pool.Next(upstreamName)
	if err != nil || e == nil {
		return nil, err
	}
	return &upstream.ProxyEntry{
		ID:       e.ID,
		Protocol: e.Protocol,
		Host:     e.Host,
		Port:     e.Port,
		Username: e.Username,
		Password: e.Password,
	}, nil
}

func (a upstreamProxyPoolAdapter) Transport(entry *upstream.ProxyEntry) (*http.Transport, error) {
	if entry == nil {
		return nil, nil
	}
	// Rewrap so the pool's transport cache keys off entry.ID.
	pe := &proxy.ProxyEntry{
		ID:       entry.ID,
		Protocol: entry.Protocol,
		Host:     entry.Host,
		Port:     entry.Port,
		Username: entry.Username,
		Password: entry.Password,
	}
	return a.pool.Transport(pe)
}

func (a upstreamProxyPoolAdapter) MarkFailed(id string)  { a.pool.MarkFailed(id) }
func (a upstreamProxyPoolAdapter) MarkSuccess(id string) { a.pool.MarkSuccess(id) }

// setUpstreamProxyPool wires the concrete pool into the upstream package.
func setUpstreamProxyPool(pp *proxy.ProxyPool) {
	upstream.SetProxyPool(upstreamProxyPoolAdapter{pool: pp})
}
