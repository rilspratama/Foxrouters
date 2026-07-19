# FoxRouters

[![Go Version](https://img.shields.io/badge/go-1.25.12%2B-00ADD8?logo=go)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](#license)
[![Version](https://img.shields.io/badge/version-v1.4.8-blue)](./CHANGELOG.md)
[![Security](https://img.shields.io/badge/security-audited%202x-brightgreen)](./CHANGELOG.md#v146--security-audit-fixes-2026-07-19)
[![Tests](https://img.shields.io/badge/tests-62%2F62%20PASS%20(%2Brace)-success)](./)

Unified **OpenAI-compatible** API gateway that fronts **Grok** and **CodeBuddy** behind
one `/v1/chat/completions` endpoint. Route by model prefix, round-robin across many
upstream accounts/keys, refresh tokens automatically, enforce per-key quotas, and log
every request/response to ClickHouse — all behind a single Bearer token.

---

## What it is

- **One endpoint, many backends.** Clients hit `POST /v1/chat/completions` with an
  OpenAI-shaped payload; the gateway dispatches by model prefix:
  - `grok-*` → `https://cli-chat-proxy.grok.com`
  - `cb/*`   → `https://www.codebuddy.ai/v2`
- **Multi-account / multi-key pools** with O(k) round-robin and automatic token
  refresh (singleflight + pre-warm), plus circuit-breaker style disable on
  auth / credit / quota errors.
- **Per-gateway-key** RPM, burst, token quota, model whitelist, and role
  (`admin` vs `inference`).
- **Redis** for hot state (tokens, credits, disabled flags, rate counters,
  gateway keys) and **ClickHouse** for cold, full-body history (ZSTD, 90-day TTL,
  unlimited body size).
- **Embedded web dashboard** for stats, accounts, keys, and models.

---

## Features

- **Model prefix routing** — `grok-*` → Grok, `cb/*` → CodeBuddy.
- **Grok alias expansion** — `grok-4.5-{high,medium,low,xhigh,auto,none}` collapse
  to `grok-4.5` + injected `reasoning_effort` (client value wins if set).
- **Multi-account / multi-key round-robin** — O(k) `Next()` on the hot path,
  background workers handle re-enable + refresh.
- **Auto token refresh** — singleflight-guarded, lock-split (no network calls under
  the account mutex), pre-warms tokens on a 30 s tick with a 30 min expiry window,
  up to 10 concurrent refreshes.
- **Circuit breaker** — passive (401/403/credit/`14018` disable + Redis persist)
  and active health checks every ~10 min.
- **Custom models + aliases** (v1.3.0) — runtime-configurable model aliases
  (`cb/kimi-k3` → `cb/gpt-5.5`) backed by Redis, no restart needed.
- **Combos** (v1.4.0) — group N models under `combo/<name>` virtual alias with
  **fallback** or **round_robin** strategy. Round-robin uses atomic Redis `INCR`
  (cluster-safe).
- **API-key auth** with role-based access — `inference` (default, least privilege)
  can only reach `/v1/*`; `admin` reaches everything.
- **Per-key model whitelist** with glob patterns (`grok-*`, `cb/*`, exact match).
- **Per-key rate limits** — RPM, burst, and cumulative token quota.
- **Redis hot state** — tokens, CB credits, disabled flags, gateway keys,
  rate/quota counters.
- **ClickHouse history** — full request + response JSON, ZSTD compression,
  90-day TTL, unlimited body length; refresh events and disable events too.
- **Web dashboard** — 4 nav items (Dashboard, Accounts & Keys, Gateway API Keys,
  Models) with Models page containing 3 tabs (Models \| Custom \| Combos).
- **Security hardened** (v1.4.6–v1.4.7, 2x audited) — XSS-safe `data-*` event
  delegation, CSRF guard (Origin/Referer check), session token indirection
  (cookie ≠ API key), login rate limit (IP-based, XFF-proof), input validation
  regex, last-admin lockout guard, `Secure`+`HttpOnly`+`SameSite=Lax` cookies.
- **Security headers** — CSP, `X-Frame-Options: DENY`, `X-Content-Type-Options:
  nosniff`, `Referrer-Policy`.
- **systemd hardening** — `NoNewPrivileges`, `ProtectSystem`, `ProtectHome`,
  private `/tmp`, etc.
- **Gzip SSE streaming fix** — correctly disables response compression on SSE
  streams so tokens actually arrive incrementally.
- **Graceful shutdown** — drains in-flight requests and flushes logs.

---

## Quick Start (Docker — Pre-built Image)

> Fastest path. No clone, no build — pulls image from ghcr.io.

```bash
curl -sL https://raw.githubusercontent.com/rilspratama/Foxrouters/master/deploy.sh | bash
```

**Output:**
```
🔑 Admin Bootstrap Key
  Key:    gw-a94c7befdb14cd6d2...819edd11
  Login:  http://localhost:20130/login
```

Or manual:
```bash
curl -sLO https://raw.githubusercontent.com/rilspratama/Foxrouters/master/docker-compose.ghcr.yml
docker compose -f docker-compose.ghcr.yml up -d
docker compose -f docker-compose.ghcr.yml logs foxrouters | grep "Key: gw-"
```

Open `http://localhost:20130/login`, paste the key, done.

---

## Quick Start (Docker — Build from Source)

> One command. The compose file wires `foxrouters`, `redis`, and `clickhouse`
> together — no `.env` editing needed for the default stack.

```bash
git clone https://github.com/rilspratama/Foxrouters.git foxrouters && cd foxrouters

# Start stack + capture bootstrap key (first boot auto-generates admin key)
./start.sh
```

**Output:**
```
🔑 Admin Bootstrap Key
  Key:    gw-a94c7befdb14cd6d2...819edd11
  Login:  http://localhost:20130/login
  Saved:  bootstrap-key.txt (chmod 600)
```

Then open `http://localhost:20130/login`, paste the key, done.

**Other commands:**
```bash
./start.sh --status    # container + health status
./start.sh --logs      # tail logs
./start.sh --key       # show captured key
./start.sh --reset     # wipe Redis volume + regenerate key
./start.sh --stop      # stop stack
```

### When do I need to edit `.env`?

| Scenario | Edit `.env`? |
|----------|-------------|
| Default docker-compose (Redis+CH+gw in same stack) | ❌ No — compose overrides everything |
| Custom Redis password in compose | ✅ Set `REDIS_PASSWORD` |
| Bare metal / systemd (Redis/CH on host) | ✅ Set `REDIS_ADDR`, `CLICKHOUSE_ADDR`, etc. |
| External Redis (managed/Cloudflare) | ✅ Set `REDIS_ADDR=host:port` + `REDIS_PASSWORD` |
| External ClickHouse | ✅ Set `CLICKHOUSE_ADDR` + auth |
| Custom port (20130 → 8080) | ✅ Set `PORT=8080` |

See [`.env.example`](./.env.example) for the full list of tunables.

---

## Quick Start (Manual)

**Prerequisites**

- Go **1.25+**
- Redis (local or remote)
- ClickHouse (local or remote; the schema is auto-migrated on boot)

```bash
# 1. Build
export PATH=$PATH:/usr/local/go/bin
go build -o foxrouters .

# 2. Configure
cp .env.example .env
$EDITOR .env

# 3. Run
./foxrouters
# → listening on :20130
```

Bootstrapping accounts / keys:

```bash
# Import a Grok account credential file (admin only)
curl -X POST http://127.0.0.1:20130/accounts/import \
     -H "Authorization: Bearer $ADMIN_KEY" \
     -H "Content-Type: application/json" \
     --data @path/to/grok-account.json

# Import a CodeBuddy key
curl -X POST http://127.0.0.1:20130/cb/import \
     -H "Authorization: Bearer $ADMIN_KEY" \
     -H "Content-Type: application/json" \
     -d '{"key":"YOUR_CB_KEY_HERE"}'
```

---

## Configuration

All configuration is read from environment variables (or a `.env` file loaded at
startup).

| Variable | Default | Description |
|---|---|---|
| `PORT` | `20130` | HTTP listen port. |
| `REDIS_ADDR` | `127.0.0.1:6379` | Redis host:port for hot state. |
| `REDIS_PASSWORD` | *(empty)* | Redis password if AUTH is enabled. |
| `REDIS_DB` | `0` | Redis logical DB index. |
| `CLICKHOUSE_ADDR` | `127.0.0.1:9001` | ClickHouse native-protocol host:port. |
| `CLICKHOUSE_DB` | `gateway` | ClickHouse database name (auto-created). |
| `CLICKHOUSE_USER` | `default` | ClickHouse username. |
| `CLICKHOUSE_PASSWORD` | *(empty)* | ClickHouse password. |
| `GATEWAY_KEY_FILE` | `./gateway-key.txt` | Path to the seed admin bearer token file. |
| `CB_KEY_FILE` | `./cb-keys.json` | Path to a JSON file of CodeBuddy keys to seed. |
| `CPA_AUTH_DIR` | `./` | Directory scanned for `xai-*.json` Grok credential files at boot. |
| `GATEWAY_AUTH_DISABLE` | `false` | **Dev only.** When `true`, bypasses auth on all routes. Never enable in production. |
| `COOKIE_SECURE` | `1` | Session cookie `Secure` flag. Set to `0` for dev HTTP (localhost). Default `1` = HTTPS-only. |

> **Do not** commit secrets. Put the `.env` outside the repo or use `chmod 600
> .gateway.env` alongside `.gitignore`.

---

## API Reference

Unless noted, all endpoints require `Authorization: Bearer <gateway-key>`.
Roles: **inference** may call `/v1/*` only; **admin** may call everything.

**Auth flow:**
- **API clients:** `Authorization: Bearer gw-...` header (preferred).
- **Dashboard:** session cookie (`foxrouters_session`) — random 256-bit token
  bound to API key server-side (NOT the raw key). 7-day TTL, sliding window.
- **Login:** `POST /login` with `key=gw-...` form body. Rate-limited 5/min per IP
  (XFF-spoof-proof via `SetTrustedProxies(nil)`).
- **CSRF:** cookie-authed mutations (POST/PUT/DELETE) require same-origin
  `Origin`/`Referer`. Bearer-authed calls are exempt.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/v1/chat/completions` | inference/admin | Main OpenAI-compatible chat proxy. Model prefix decides upstream. |
| `GET`  | `/v1/models` | inference/admin | Model list (includes Grok aliases and CB models). |
| `GET`  | `/health` | **public** | Liveness + readiness probe. |
| `GET`  | `/dashboard` | **public HTML** | Serves the SPA. Auth still required for its XHR calls. |
| `GET`  | `/accounts` | admin | List Grok accounts + CB keys with status. |
| `POST` | `/accounts/import` | admin | Import a Grok account credential JSON. |
| `POST` | `/cb/import` | admin | Import a CodeBuddy key. |
| `DELETE` | `/cb/keys/:key` | admin | Delete a CodeBuddy key. |
| `POST` | `/cleanup/disabled` | admin | Bulk-remove permanently disabled keys/accounts (`?type=all\|grok\|cb`). |
| `GET`  | `/cb-stats` | admin | CodeBuddy per-key credit / usage stats. |
| `GET`  | `/metrics` | **public** | Prometheus metrics (request count, duration, pool sizes, circuit state). |
| `POST` | `/v1/messages` | inference+ | **Anthropic Messages API** (Claude Code compatible). Accepts `x-api-key` or `Authorization: Bearer`. |
| `GET`  | `/api/keys` | admin | List gateway API keys. |
| `POST` | `/api/keys` | admin | Create a gateway key (role, allowed_models, RPM, burst, quota). |
| `PUT`  | `/api/keys` | admin | Update a gateway key. |
| `DELETE` | `/api/keys` | admin | Revoke a gateway key. |
| `GET`  | `/history` | admin | Aggregated stats over a time window (`?hours=24`). |
| `GET`  | `/history/recent` | admin | Recent request previews (`?limit=50`). `id` is a JSON **string**. |
| `GET`  | `/history/detail/:id` | admin | Full request + response JSON for one call. |
| `GET`  | `/api/models/custom` | admin | List runtime-registered custom models (v1.3.0). |
| `POST` | `/api/models/custom` | admin | Register a new custom model: `{id, upstream, model_name, owned_by?}`. |
| `DELETE` | `/api/models/custom/:id` | admin | Delete a custom model (id may contain `/`, e.g. `cb/kimi-k3`). |
| `GET`  | `/api/aliases` | admin | List model aliases. |
| `POST` | `/api/aliases` | admin | Create alias: `{alias, target}` (e.g. `my-claude` → `cb/claude-sonnet-4.6`). |
| `DELETE` | `/api/aliases/:alias` | admin | Delete an alias. |
| `GET`  | `/api/combos` | admin | List combos (v1.4.0). |
| `POST` | `/api/combos` | admin | Create combo: `{name, strategy, models[], description?}` — strategy is `fallback` or `round_robin`. |
| `GET`  | `/api/combos/*name` | admin | Fetch one combo. |
| `DELETE` | `/api/combos/*name` | admin | Delete combo + its round-robin counter. |

### Example: chat completion

```bash
curl -s http://127.0.0.1:20130/v1/chat/completions \
  -H "Authorization: Bearer $GATEWAY_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "grok-4.5-high",
    "stream": true,
    "messages": [{"role":"user","content":"hello"}]
  }'
```

### Example: Anthropic Messages API (Claude Code)

FoxRouters exposes `POST /v1/messages` — the Anthropic Messages API format. This lets **Claude Code CLI** use FoxRouters as its backend proxy → Grok/CodeBuddy.

```bash
curl -s http://127.0.0.1:20130/v1/messages \
  -H "x-api-key: $GATEWAY_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4",
    "max_tokens": 100,
    "messages": [{"role":"user","content":"Hello"}]
  }'
```

**Configure Claude Code to use FoxRouters:**

```bash
export ANTHROPIC_BASE_URL=http://localhost:20130
export ANTHROPIC_API_KEY=gw-xxx
claude
```

**Model mapping:**
- `claude-*` → `cb/claude-sonnet-4` (CodeBuddy, default)
- `claude-*-grok` → `grok-4.5` (Grok upstream)
- `cb/*` / `grok-*` explicit → passthrough

### Example: register a custom model + alias (v1.3.0)

Custom models let you route any client-facing model id to `codebuddy` or `grok`
with a chosen upstream model name — no rebuild, just a Redis-backed POST.

```bash
# 1. Register cb/kimi-k3 → codebuddy (upstream sees "kimi-k3")
curl -s -X POST http://127.0.0.1:20130/api/models/custom \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "content-type: application/json" \
  -d '{"id":"cb/kimi-k3","upstream":"codebuddy","model_name":"kimi-k3","owned_by":"codebuddy"}'

# 2. Add alias so clients can say "my-claude" and hit cb/claude-sonnet-4.6
curl -s -X POST http://127.0.0.1:20130/api/aliases \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "content-type: application/json" \
  -d '{"alias":"my-claude","target":"cb/claude-sonnet-4.6"}'

# 3. Use it — request goes to CodeBuddy with model=claude-sonnet-4.6.
curl -s http://127.0.0.1:20130/v1/chat/completions \
  -H "Authorization: Bearer $GATEWAY_KEY" -H "content-type: application/json" \
  -d '{"model":"my-claude","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}'

# 4. Cleanup
curl -s -X DELETE http://127.0.0.1:20130/api/aliases/my-claude \
  -H "Authorization: Bearer $ADMIN_KEY"
curl -s -X DELETE http://127.0.0.1:20130/api/models/custom/cb/kimi-k3 \
  -H "Authorization: Bearer $ADMIN_KEY"
```

Aliases are checked **before** the default `grok-*` / `cb/*` routing, so they
also work for the Anthropic Messages API — `mapAnthropicModel` consults
aliases first.

### Example: combos (v1.4.0)

Group multiple models under a virtual `combo/<name>` alias with automatic
failover or load-spreading:

```bash
ADMIN_KEY=<your-admin-gateway-key>
CLIENT_KEY=<any-gateway-key>

# 1. Create a Fallback combo — tries models in order, retries next on 5xx
curl -s -X POST http://127.0.0.1:20130/api/combos \
  -H "Authorization: Bearer $ADMIN_KEY" -H "content-type: application/json" \
  -d '{"name":"smart-fallback","strategy":"fallback",
       "models":["cb/gpt-5.5","cb/claude-sonnet-4.6","grok-4.5"],
       "description":"GPT then Claude then Grok"}'

# 2. Create a Round Robin combo — rotates models across requests
curl -s -X POST http://127.0.0.1:20130/api/combos \
  -H "Authorization: Bearer $ADMIN_KEY" -H "content-type: application/json" \
  -d '{"name":"rr-pool","strategy":"round_robin",
       "models":["cb/gpt-5.5","cb/claude-sonnet-4.6"]}'

# 3. Use them — client just calls combo/<name>
curl -s -X POST http://127.0.0.1:20130/v1/chat/completions \
  -H "Authorization: Bearer $CLIENT_KEY" -H "content-type: application/json" \
  -d '{"model":"combo/smart-fallback","messages":[{"role":"user","content":"hi"}]}'

# 4. Combos appear in /v1/models
curl -s http://127.0.0.1:20130/v1/models -H "Authorization: Bearer $CLIENT_KEY" \
  | jq '.data[] | select(.id | startswith("combo/"))'

# 5. Cleanup
curl -s -X DELETE http://127.0.0.1:20130/api/combos/smart-fallback \
  -H "Authorization: Bearer $ADMIN_KEY"
curl -s -X DELETE http://127.0.0.1:20130/api/combos/rr-pool \
  -H "Authorization: Bearer $ADMIN_KEY"
```

**Fallback semantics**
- Non-streaming: on 5xx from `models[i]`, response is buffered + discarded, next model tried. 4xx returns immediately (client error).
- Streaming (SSE): head-of-list model only — bytes already on the wire can't be retried.

**Round-robin semantics**
- Atomic `INCR combo:counter:<name>` (Redis) — cluster-safe fair rotation.
- Counter is auto-deleted when the combo is deleted.

---

## Authentication

The gateway uses **Bearer tokens** (opaque gateway keys) with two roles:

| Role | Access |
|---|---|
| `inference` *(default)* | `/v1/*` only. Rejected with `403` on any admin path. |
| `admin` | All endpoints, including account/key/history management. |

Each key also carries:

- **`allowed_models`** — a list of glob patterns. Requests whose `model` does
  not match any pattern get `403`. Patterns:
  - `grok-*` — all Grok models (and aliases).
  - `cb/*` — all CodeBuddy models.
  - `grok-4.5` — exact match.
  - `*` — allow everything (use sparingly).
- **`rpm`** — max requests per minute (rolling window).
- **`burst`** — token-bucket burst size.
- **`token_quota`** — cumulative token budget; `429` once exhausted.

Keys are created via `POST /api/keys` and stored in Redis.

---

## Model Routing

Routing is driven purely by the `model` field of the incoming request:

| Model prefix | Upstream | Notes |
|---|---|---|
| `grok-*` | `https://cli-chat-proxy.grok.com` | Multi-account pool, refresh + 401 retry. |
| `cb/*` | `https://www.codebuddy.ai/v2` | Multi-key pool, credit tracking, 14018 disable. |

### Grok alias expansion

Convenience aliases collapse to `grok-4.5` with `reasoning_effort` injected:

| Alias | Rewrites to | `reasoning_effort` |
|---|---|---|
| `grok-4.5-high` | `grok-4.5` | `high` |
| `grok-4.5-medium` | `grok-4.5` | `medium` |
| `grok-4.5-low` | `grok-4.5` | `low` |
| `grok-4.5-xhigh` | `grok-4.5` | `xhigh` |
| `grok-4.5-auto` | `grok-4.5` | `auto` |
| `grok-4.5-none` | `grok-4.5` | `none` |

If the client already sets `reasoning_effort` explicitly, the client value wins.

---

## Dashboard

Served at `GET /dashboard` (public HTML; XHRs still require a gateway key via
session cookie or `?key=`). The SPA has four nav routes:

| Route | Page |
|---|---|
| `#/` | **Dashboard** — health, request counts, token totals, recent history preview. |
| `#/accounts` | **Accounts & Keys** — Grok accounts + CodeBuddy keys with pagination and enable/disable/refresh. |
| `#/keys` | **Gateway API Keys** — key CRUD, role picker, allowed-models selector, RPM/burst/quota inputs. |
| `#/models` | **Models** — 3 tabs: **Models** (usage stats) \| **Custom** (custom models + aliases) \| **Combos** (group models under virtual alias). |

Live gateway keys are **never** rendered into the HTML server-side. Delete buttons
use `data-*` attributes + event delegation (XSS-safe, no inline `onclick`).

---

## Architecture

```
Client
  │  Bearer <gateway-key>
  ▼
AuthMiddleware           ── validate key, load role + limits
  ▼
RateLimitMiddleware      ── RPM / burst / token quota (Redis)
  ▼
proxyRequest             ── inspect "model", expand aliases
  ├── grok-*  → proxyGrok         (O(k) RR, refresh, 401 retry, 403 ban)
  └── cb/*    → proxyCodeBuddy    (RR, credit tracking, stream transform)
  ▼
async LogRequest → ClickHouse (full request + response, ZSTD, TTL 90d)
```

**Storage split**

| Layer | Engine | Contents |
|---|---|---|
| Hot | **Redis** | Tokens, CB credits, disabled flags, gateway keys, rate/quota counters. |
| Cold | **ClickHouse** | `request_logs` (full bodies), refresh events, disable events. |

**Hot-path invariants**

1. `Next()` is O(k) round-robin — re-enable/refresh happens in background workers.
2. Counts come from `Len()`, never `len(GetAll())`.
3. Refresh uses singleflight and never holds `acc.mu` across a network call.
4. Any disable/enable/token mutation calls `Save*()` **after** the lock is
   released.
5. History writes are async; credentials never land in ClickHouse.

---

## Development

```bash
export PATH=$PATH:/usr/local/go/bin

# Required before every build
go test -count=1 -race ./...
go vet ./...

# Build
go build -o foxrouters .

# Run
./foxrouters

# Smoke
curl -s http://127.0.0.1:20130/health
```

**Project layout**

| File | Role |
|---|---|
| `main.go` | Version, HTTP clients, workers, routes, graceful shutdown. |
| `proxy.go` | Model routing, alias expansion, `RequestLog` build. |
| `grok_account.go` | Grok pool, refresh loop, `proxyGrok`, re-enable worker. |
| `codebuddy.go` | CB pool, stream transform, `proxyCodeBuddy`, re-enable worker. |
| `auth.go` | Bearer auth, role check, allowed-models glob match. |
| `ratelimit.go` | RPM / burst / token-quota middleware. |
| `health.go` | Health endpoint + active health checks. |
| `handlers.go` | Account, key, history, dashboard handlers. |
| `db.go` | Redis + ClickHouse clients and schema. |
| `dashboard.html` | Embedded SPA (`go:embed`). |

**Patch order (please follow):** `test → build → restart → smoke`.

---

## License

Released under the **MIT License**. See [`LICENSE`](./LICENSE) for the full text.
