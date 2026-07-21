# FoxRouters

## Project Overview
Unified OpenAI-compatible API gateway for **Grok + CodeBuddy**. Routes by model prefix:
`grok-*` → cli-chat-proxy.grok.com, `cb/*` → www.codebuddy.ai/v2.

Multi-account/key round-robin, auto-refresh (singleflight + pre-warm), circuit breaker,
API key auth, per-key RPM/quota, Redis hot-state, **ClickHouse** full-body history, web dashboard.

**Version:** v1.5.0 (`-X main.Version` build flag)
**Port:** 20130 · **systemd:** `foxrouters.service`  
**Path:** `/root/nexus-workspace/foxrouters/`

## Architecture / Flow

```
Client → AuthMiddleware (Bearer) → RateLimitMiddleware
       → /v1/chat/completions
            ↓
       proxyRequest (model routing + expandGrokAlias)
       ├── grok-* → proxyGrok (O(k) RR, 401 retry, 403 ban/cooldown + Redis persist)
       └── cb/*   → proxyCodeBuddy (stream-only transform, credit/14018 disable + Redis)
            ↓
       async LogRequest → ClickHouse (full body, ZSTD, unlimited)
```

### Data stores

| Layer | Engine | Purpose |
|-------|--------|---------|
| Hot | **Redis** | Tokens, CB credits, disabled flags, gateway keys, rate state, **proxy pool** |
| Cold | **LogStore** (pluggable via `LOG_BACKEND`) | `request_logs` full request/response JSON, refresh/events, 90d TTL |
| Legacy | PostgreSQL | **Not used** by gateway for history (may remain on disk) |

Log backend choices (`LOG_BACKEND` env, default `sqlite`):

| Backend | When to use | Footprint |
|---------|-------------|-----------|
| `sqlite` (default) | Small deployments; no ops overhead | Single file at `LOG_SQLITE_PATH` (default `/var/lib/foxrouters/logs.db`), ~60MB total |
| `clickhouse`       | Analytics workloads, high-volume queries | Separate CH server, ~700MB image + RAM |

### Hot-path rules (do not regress)
1. `Next()` = O(k) RR only — re-enable in background workers only  
2. Counts via `Len()`, not `len(GetAll())`  
3. Refresh = singleflight + lock-split (no network under `acc.mu`)  
4. Any disable/enable/token mutate → `Save*()` after unlock  
5. History write async only; credentials never in CH  
6. Full body unlimited in CH; log `id` JSON **string** for browsers  
7. No live gateway key inject into `/dashboard` HTML  
8. Proxy pool: `getClient(default, upstream)` — returns proxied client if pool has enabled proxies matching upstream scope, else direct. Transport cache per proxy ID. Auto-disable after 5 fails.

### Token refresh
- Pre-warm every 30s, 30min window, 10 concurrent  
- `Next()` Pass1 valid token; Pass2 least-expired + async refresh  
- 401 rebuild request body + retry  

### Grok aliases
`grok-4.5-{high,medium,low,xhigh,auto,none}` → `grok-4.5` + `reasoning_effort` (client wins if set).

## File map
| File | Role |
|------|------|
| `main.go` | Version, HTTP clients, workers, routes, middleware, graceful shutdown |
| `auth_adapter.go` | Type aliases + bridges to `internal/auth` (Manager, SessionStore, etc.) |
| `handlers_adapter.go` | Handler function wrappers (for signature-changed handlers) |
| `csrf_guard.go` | Origin/Referer check on cookie-authed mutations (P2-2) |
| `login_limiter.go` | IP-based rate limiter for `/login` (5/min + 20/hour) |
| `grok_account.go` | Grok pool, refresh, proxyGrok, reenableWorker |
| `codebuddy.go` | CB pool, transform, proxyCodeBuddy, reenableCBWorker |
| `proxy.go` | Routing, RequestLog build |
| `db.go` | Redis + LogStore glue (async batch pipeline, factory) |
| `internal/db/logstore.go` | `LogStore` interface + shared DTOs (RequestLog, RequestStats, …) |
| `internal/db/logstore_sqlite.go` | modernc.org/sqlite backend (default) |
| `internal/db/logstore_clickhouse.go` | ClickHouse backend (opt-in via `LOG_BACKEND=clickhouse`) |
| `handlers.go` | health, accounts, history, keys, dashboard static |
| `auth.go` / `ratelimit.go` / `health.go` | Auth, RPM, circuit |
| `dashboard.html` | SPA — 5 nav routes (Dashboard/Accounts/Keys/Models/Proxies), Models page has 3 tabs (Models/Custom/Combos) |
| `internal/auth/session_store.go` | Session token → API key map (P3-3, 256-bit random tokens) |
| `internal/proxy/validate.go` | `validateName()` regex for id/alias/combo (P3-5) |
| `internal/proxy/combo.go` | ComboRegistry — fallback + round_robin strategies |
| `internal/proxy/pool.go` | ProxyPool — HTTP/SOCKS5 proxy pool, round-robin, per-upstream scoping, transport cache, auto-disable |
| `internal/handlers/combos.go` | Combos CRUD endpoints |
| `internal/handlers/custom.go` | Custom models + aliases CRUD endpoints |
| `internal/handlers/proxies.go` | Proxy pool CRUD + test + toggle endpoints |
| `proxy_pool_test.go` | Proxy pool tests (CRUD, validation, masking, round-robin, scoping) |
| `CHANGELOG.md` | Version history (v1.4.0 → v1.5.0) |
| `.gateway.env` | Secrets (chmod 600, gitignored) |

## Env (essentials)
```
REDIS_ADDR / REDIS_PASSWORD / REDIS_DB
LOG_BACKEND=sqlite               # sqlite (default) | clickhouse
LOG_SQLITE_PATH=/var/lib/foxrouters/logs.db  # only used when LOG_BACKEND=sqlite
CLICKHOUSE_ADDR=127.0.0.1:9000   # only used when LOG_BACKEND=clickhouse
CLICKHOUSE_DB=gateway
GATEWAY_KEY_FILE / CB_KEY_FILE
PORT=20130
COOKIE_SECURE=0  # dev HTTP; omit for prod (defaults to HTTPS-only)
```

## Build / test / deploy
```bash
cd /root/nexus-workspace/foxrouters
export PATH=$PATH:/usr/local/go/bin
go test -count=1 -race ./... && go vet ./...   # REQUIRED first
go build -o foxrouters .
systemctl restart foxrouters.service
curl -s http://127.0.0.1:20130/health
```

## API (auth Bearer unless noted)
| Endpoint | Notes |
|----------|--------|
| `POST /v1/chat/completions` | Main proxy |
| `GET /v1/models` | Includes Grok aliases |
| `GET /health` | Public |
| `GET /history?hours=24` | CH stats |
| `GET /history/recent?limit=50` | Previews; `id` is **string** |
| `GET /history/detail/:id` | Full request/response JSON |
| `GET/POST /accounts` … | Grok import/delete/refresh |
| `GET/POST /api/keys` … | Gateway key CRUD |
| `GET/POST /api/proxies` … | Proxy pool CRUD + test + toggle |
| `GET /dashboard` | Public HTML; key via `?key=` / localStorage only |

## Dashboard UX prefs
- I/O text: modal only, never table columns  
- Total tokens: top stats card  
- History full JSON: tabs Request/Response (lazy detail)  
- Grok table: client-side pagination  

## Skill / deeper docs
Hermes skill: `foxrouters-development`  
Key refs: `clickhouse-history-migration.md`, `v5.9-performance-optimizations.md`,
`p0-p1-correctness-audit.md`, `dashboard-history-json-tabs-uint64.md`,
`gzip-sse-streaming-bug.md`, `redis-only-persistence.md`.

## Cloudflare Tunnel (optional public exposure)
Gateway ports are bound to `127.0.0.1` — for public access without opening
host firewall ports, use a Cloudflare Tunnel. Two modes, no Go-side changes
(tunnel is infra-only; the gateway is unaware of it).

| Mode  | URL                            | Persistent? | Needs Cloudflare account |
|-------|--------------------------------|-------------|--------------------------|
| quick | random `*.trycloudflare.com`   | no (rotates on restart) | no |
| named | your `gateway.example.com`     | yes         | yes (zone + `cloudflared login`) |

Container: `foxrouters-tunnel` (image `cloudflare/cloudflared:latest`), joined
to `foxrouters-net` so it can reach `foxrouters:20130` directly.

**install.sh** prompts for tunnel mode after the gateway is healthy. Non-interactive:
`TUNNEL_MODE=quick|named|none` (default `none`).

**tunnel.sh** (repo root) manages the tunnel lifecycle:
```
./tunnel.sh enable [--quick|--named]   # start
./tunnel.sh disable                    # stop + rm container
./tunnel.sh status                     # container state + current URL
./tunnel.sh url                        # print URL only
./tunnel.sh restart                    # keeps prior mode (from ${CONFIG_DIR}/mode)
./tunnel.sh logs [-f]                  # tail cloudflared logs
```

Named-tunnel config lives at `/etc/foxrouters/cloudflared/`:
`cert.pem` (from `cloudflared tunnel login`), `<tunnel-id>.json` (from
`cloudflared tunnel create`), and `config.yml` with ingress rules pointing
`service: http://foxrouters:20130`. See the header of `tunnel.sh` for the
full setup recipe.

Compose profile: `docker compose --profile tunnel up -d` brings up the
cloudflared service in quick mode (equivalent to `./tunnel.sh enable --quick`).

## Operator notes (Rils)
- Optimasi latency LLM lanjutan (context trim, model pick, reasoning default) = **client-side** — deferred.  
- Gateway hot-path + CH full-body considered **done** for current phase.  
- Patch order always: **test → build → restart → smoke**.
