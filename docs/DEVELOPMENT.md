# Development

## Development

### Isolated Dev Stack (Recommended)

`dev.sh` runs a fully isolated development environment that never touches production:

```bash
./dev.sh build    # build foxrouters:dev image
./dev.sh up       # start dev gateway (:20131) + dev redis (:6381)
./dev.sh seed     # copy credentials from prod Redis (read-only)
./dev.sh logs     # tail dev gateway logs
./dev.sh down     # stop dev stack (add -v to wipe volumes)
./dev.sh test     # go vet + go test -race
```

**Safety gates (auto-set by `dev.sh up`):**

| Env Var | Effect |
|---------|--------|
| `WORKERS_DISABLED=1` | Skip background workers (token refresh, credit sync, billing sync) |
| `HEALTH_PROBES_DISABLED=1` | Skip health-check probes to upstream |
| `TOKEN_REFRESH_DISABLED=1` | Gate ALL refresh paths (on-demand + 401-retry) |

When `TOKEN_REFRESH_DISABLED=1`, `toDTO()` zeroes `AccessToken/RefreshToken/IDToken` so credentials never persist to dev Redis. Dev can test inference with seeded tokens, but near-expiry accounts will 401 (by design — protects prod refresh tokens).

### Local Build

```bash
export PATH=$PATH:/usr/local/go/bin

# Required before every build
go test -count=1 -race ./...
go vet ./...

# Deploy (Docker Compose — preferred)
docker compose up -d --build foxrouters

# Or local binary
go build -o foxrouters .
./foxrouters

# Smoke
curl -s http://127.0.0.1:20130/health
```

**Project layout**

| File | Role |
|---|---|
| `main.go` | Version, HTTP clients, workers, routes, graceful shutdown. |
| `internal/proxy/proxy.go` | Model routing, alias expansion, `RequestLog` build, content filters. |
| `internal/upstream/grok_*.go` | Grok pool (`grok_account`, `grok_billing`, `grok_selector`, `grok_proxy`), refresh loop, `proxyGrok`, re-enable worker. |
| `internal/upstream/cb*.go` | CB dual pool (`cbkey`, `cb_manager`, `cb_meter`, `cb_transform`, `cb_proxy`) — api_key + oauth, OAuth refresh, meter SyncCredits, credit worker. |
| `internal/upstream/freebuff_*.go` | Freebuff provider (`freebuff_models`, `freebuff_manager`, `freebuff_session`, `freebuff_device`). |
| `internal/upstream/alibaba*.go` | Alibaba DashScope provider + per-key quota + media studio. |
| `internal/upstream/model_registry.go` | Dynamic model registry (Freebuff + Grok sources, 6h refresh). |
| `internal/upstream/health.go` | Health endpoint + active health checks + circuit breakers. |
| `internal/upstream/*_test.go` | Unit tests for meter sync, collapse, streams, OAuth. |
| `internal/handlers/handlers_*.go` | HTTP handlers split by resource: `health`, `accounts`, `codebuddy`, `history`, `keys`, `dashboard`. |
| `internal/handlers/anthropic.go` | Anthropic Messages API adapter (`/v1/messages`). |
| `internal/db/db.go` | Redis + LogStore clients and schema. |
| `internal/auth` | Bearer auth, role check, allowed-models glob match, session store. |
| `internal/ratelimit` | RPM / burst / token-quota middleware. |
| `internal/tunnel` | Cloudflare Tunnel control plane. |
| `internal/metrics` | Prometheus metrics. |
| `dashboard/` | Embedded SPA (`go:embed` dir, assembled in `main.go`) — modular: head/body/modals HTML + core/pages JS. |
| `dev.sh` | Isolated dev stack (own Redis, port 20131, safety gates). |

**Patch order (please follow):** `test → build → restart → smoke`.

