// proxy_pool_bridge.go — concrete *proxy.ProxyPool bridged over to the abstract
// upstream.ProxyPool interface. The interface exists because internal/upstream
// cannot import internal/proxy (proxy already imports upstream — cycle). This
// adapter is real logic, not a migration alias: it must live in package main.
package main

import (
	"net/http"

	"foxrouters/internal/proxy"
	"foxrouters/internal/upstream"
)

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
