# FoxRouters

## Project Overview
Unified OpenAI-compatible API gateway for **Grok + CodeBuddy**. Routes by model prefix:
`grok-*` → cli-chat-proxy.grok.com, `cb/*` → www.codebuddy.ai/v2.

Multi-account/key round-robin, auto-refresh (singleflight + pre-warm), circuit breaker,
API key auth, per-key RPM/quota, Redis hot-state, **ClickHouse** full-body history, web dashboard.

**Version:** v1.4.8 (`-X main.Version` build flag)
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
| Hot | **Redis** | Tokens, CB credits, disabled flags, gateway keys, rate state |
| Cold | **ClickHouse** (`127.0.0.1:9000`) | `request_logs` full request/response JSON, refresh/events, 90d TTL |
| Legacy | PostgreSQL | **Not used** by gateway for history (may remain on disk) |

### Hot-path rules (do not regress)
1. `Next()` = O(k) RR only — re-enable in background workers only  
2. Counts via `Len()`, not `len(GetAll())`  
3. Refresh = singleflight + lock-split (no network under `acc.mu`)  
4. Any disable/enable/token mutate → `Save*()` after unlock  
5. History write async only; credentials never in CH  
6. Full body unlimited in CH; log `id` JSON **string** for browsers  
7. No live gateway key inject into `/dashboard` HTML  

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
| `db.go` | Redis + ClickHouse |
| `handlers.go` | health, accounts, history, keys, dashboard static |
| `auth.go` / `ratelimit.go` / `health.go` | Auth, RPM, circuit |
| `dashboard.html` | SPA — 4 nav routes, Models page has 3 tabs (Models/Custom/Combos) |
| `internal/auth/session_store.go` | Session token → API key map (P3-3, 256-bit random tokens) |
| `internal/proxy/validate.go` | `validateName()` regex for id/alias/combo (P3-5) |
| `internal/proxy/combo.go` | ComboRegistry — fallback + round_robin strategies |
| `internal/handlers/combos.go` | Combos CRUD endpoints |
| `internal/handlers/custom.go` | Custom models + aliases CRUD endpoints |
| `CHANGELOG.md` | Version history (v1.4.0 → v1.4.8) |
| `.gateway.env` | Secrets (chmod 600, gitignored) |

## Env (essentials)
```
REDIS_ADDR / REDIS_PASSWORD / REDIS_DB
CLICKHOUSE_ADDR=127.0.0.1:9000
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

## Operator notes (Rils)
- Optimasi latency LLM lanjutan (context trim, model pick, reasoning default) = **client-side** — deferred.  
- Gateway hot-path + CH full-body considered **done** for current phase.  
- Patch order always: **test → build → restart → smoke**.
