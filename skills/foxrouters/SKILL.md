---
name: foxrouters
description: Interact with FoxRouters AI Gateway API — OpenAI + Anthropic compatible proxy for Grok + CodeBuddy. Use when making LLM inference calls (OpenAI or Claude Code), managing API keys/accounts, or querying request history.
version: 1.0.0
tags: [ai-gateway, proxy, openai-compatible, grok, codebuddy, llm]
---

# FoxRouters AI Gateway

Unified OpenAI-compatible API gateway. Routes by model prefix: `grok-*` → Grok, `cb/*` → CodeBuddy. Multi-account/key round-robin, auto-refresh, circuit breaker, per-key RPM/quota, Redis hot-state, ClickHouse history.

## Connection

| Param | Value |
|-------|-------|
| **Base URL** | `http://127.0.0.1:20130` (localhost) or `http://<your-vps-ip>:20130` (public) |
| **Auth (API)** | `Authorization: Bearer <GATEWAY_KEY>` |
| **Auth (Dashboard)** | Cookie-based — visit `/login`, enter key, HttpOnly cookie set (v5.11.6). No more `?key=` URL param or localStorage. |
| **Gateway key file** | `/root/nexus-workspace/foxrouters/gateway-key.txt` (first line = admin key). On fresh deploy with empty Redis, gateway auto-generates a bootstrap admin key and writes it to `bootstrap-key.txt` (v5.11.6). |
| **Version** | v1.5.0 |
| **Dashboard** | `/dashboard` (redirects to `/login` if no session cookie) |
| **Deploy script** | `./start.sh` — one-command deploy (docker compose up + capture bootstrap key from logs + save to `bootstrap-key.txt`). Commands: `--reset`, `--status`, `--logs`, `--key`, `--stop`. |

### Get the key (for API/curl usage)
```bash
KEY=$(head -1 /root/nexus-workspace/foxrouters/gateway-key.txt)
```

**If Redis was wiped / fresh deploy** — the gateway auto-bootstraps a random admin key on first boot and writes it to `bootstrap-key.txt`:
```bash
KEY=$(cat /root/nexus-workspace/foxrouters/bootstrap-key.txt)
# or check the startup log: journalctl -u foxrouters --since "5 min ago" | grep "Key: gw-"
# After first login, delete bootstrap-key.txt (Redis now owns it)
```

### Login to dashboard (browser)
Open `http://<host>:20130/dashboard` → redirects to `/login` → enter gateway key → redirected back to `/dashboard` with HttpOnly session cookie (7 day expiry). Key never appears in URL or JavaScript. Logout button in sidebar footer.

## Quick Reference

### 1. LLM Inference (OpenAI-compatible)

All inference uses `POST /v1/chat/completions`. Key must have `role=inference` or `role=admin` and the model must be in the key's `allowed_models` whitelist (empty = all models).

```bash
curl -s http://127.0.0.1:20130/v1/chat/completions \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "grok-4.5",
    "messages": [{"role":"user","content":"Hello"}],
    "stream": false
  }'
```

**Model routing:**
| Prefix | Upstream | Example models |
|--------|----------|----------------|
| `grok-*` | cli-chat-proxy.grok.com | `grok-4.5`, `grok-4.5-high`, `grok-4.5-medium`, `grok-4.5-low`, `grok-4.5-xhigh`, `grok-4.5-auto`, `grok-4.5-none` |
| `cb/*` | www.codebuddy.ai/v2 | `cb/gpt-5.6-sol`, `cb/gpt-5.6-terra`, `cb/gpt-5.6-luna`, `cb/gpt-5.5`, `cb/claude-opus-4.7`, `cb/gemini-3.1-pro`, etc. (42 models) |

**Grok aliases:** `grok-4.5-{high,medium,low,xhigh,auto,none}` → `grok-4.5` + `reasoning_effort` param.

**List available models:**
```bash
curl -s http://127.0.0.1:20130/v1/models -H "Authorization: Bearer $KEY"
```

**Streaming (SSE):**
```bash
curl -N http://127.0.0.1:20130/v1/chat/completions \
  -H "Authorization: Bearer $KEY" \
  -d '{"model":"cb/gpt-5.5","messages":[...],"stream":true}'
```

### 1b. LLM Inference (Anthropic Messages API — Claude Code compatible)

FoxRouters also exposes `POST /v1/messages` — the Anthropic Messages API format. This lets **Claude Code CLI** use FoxRouters as its backend.

```bash
curl -s http://127.0.0.1:20130/v1/messages \
  -H "x-api-key: $KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4",
    "max_tokens": 100,
    "messages": [{"role":"user","content":"Hello"}]
  }'
```

**Auth:** Accepts both `x-api-key` (Anthropic standard) and `Authorization: Bearer` (OpenAI standard).

**Model mapping:**
| Input model | Routes to |
|-------------|-----------|
| `claude-*` (default) | `cb/claude-sonnet-4` (CodeBuddy) |
| `claude-*-grok` | `grok-4.5` (Grok) |
| `cb/*` or `grok-*` (explicit) | passthrough |

**Configure Claude Code:**
```bash
export ANTHROPIC_BASE_URL=http://localhost:20130
export ANTHROPIC_API_KEY=gw-xxx
claude
```

**Streaming:** Returns Anthropic SSE event stream (`message_start`, `content_block_delta`, `message_stop`).

### 2. API Key Management (admin only)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| GET | `/api/keys` | List all gateway keys |
| POST | `/api/keys` | Create new key |
| GET | `/api/keys/:key/usage` | Get key usage stats |
| PUT | `/api/keys/:key` | Update key (role, models, rpm, quota) |
| DELETE | `/api/keys/:key` | Delete key |

**Create key:**
```bash
# Inference key — restricted to specific models
curl -X POST http://127.0.0.1:20130/api/keys \
  -H "Authorization: Bearer $KEY" \
  -d '{
    "name": "my-app",
    "role": "inference",
    "allowed_models": ["grok-*", "cb/gpt-5.5"],
    "rpm": 60,
    "burst": 10,
    "token_quota": 1000000
  }'

# Admin key — full access
curl -X POST http://127.0.0.1:20130/api/keys \
  -H "Authorization: Bearer $KEY" \
  -d '{"name":"ops","role":"admin"}'
```

**Roles:**
| Role | Access |
|------|--------|
| `inference` | Only `/v1/chat/completions` + `/v1/models` (default for new keys) |
| `admin` | All endpoints — keys, accounts, history, cb-stats |

**Allowed models glob patterns:**
| Pattern | Match |
|---------|-------|
| `grok-*` | grok-4.5, grok-4.5-high, etc. |
| `cb/*` | cb/gpt-5.5, cb/gemini-3.1-pro, etc. |
| `cb/gpt-5.5` | exact match only |
| *(empty)* | all models (backward compat) |

**Update key:**
```bash
curl -X PUT http://127.0.0.1:20130/api/keys/gw-abc... \
  -H "Authorization: Bearer $KEY" \
  -d '{"allowed_models":["cb/*"],"rpm":120}'
```

### 3. Grok Account Management (admin only)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| GET | `/accounts` | List all Grok accounts + CB keys summary |
| POST | `/accounts/import` | Add single Grok account |
| POST | `/accounts/import/bulk` | Add multiple Grok accounts |
| POST | `/accounts/refresh` | Trigger token refresh for all accounts |
| DELETE | `/accounts/:email` | Delete a Grok account |
| DELETE | `/cb/keys/:key` | Delete a CodeBuddy key |
| POST | `/cleanup/disabled?type=all\|grok\|cb` | Bulk-remove permanently disabled keys/accounts |

**Add single account:**
```bash
curl -X POST http://127.0.0.1:20130/accounts/import \
  -H "Authorization: Bearer $KEY" \
  -d '{
    "email": "user@example.com",
    "access_token": "eyJ...",
    "refresh_token": "...",
    "id_token": "...",
    "expires_in": 21600
  }'
```

**Bulk import accounts:**
```bash
curl -X POST http://127.0.0.1:20130/accounts/import/bulk \
  -H "Authorization: Bearer $KEY" \
  -d '{"accounts":[
    {"email":"u1@x.com","access_token":"...","refresh_token":"..."},
    {"email":"u2@x.com","access_token":"...","refresh_token":"..."}
  ]}'
# Response: {"added":2,"updated":0,"failed":0,"total":507}
```

### 4. CodeBuddy Key Management (admin only)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| POST | `/cb/import` | Add single CB key |
| POST | `/cb/import/bulk` | Add multiple CB keys |

**Add single key:**
```bash
curl -X POST http://127.0.0.1:20130/cb/import \
  -H "Authorization: Bearer $KEY" \
  -d '{"api_key":"ck_..."}'
```

**Bulk import keys (supports raw paste or array):**
```bash
# Array format
curl -X POST http://127.0.0.1:20130/cb/import/bulk \
  -H "Authorization: Bearer $KEY" \
  -d '{"api_keys":["ck_abc","ck_def","ck_ghi"]}'

# Raw paste (newline/comma/space separated)
curl -X POST http://127.0.0.1:20130/cb/import/bulk \
  -H "Authorization: Bearer $KEY" \
  -d '{"raw":"ck_abc\nck_def,ck_ghi"}'
# Response: {"added":3,"skipped":0,"total":1050}
```

### 5. History & Monitoring (admin only)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| GET | `/health` | Service health (public minimal, authed = full telemetry) |
| HEAD | `/health` | Liveness probe (public) |
| GET | `/cb-stats` | CodeBuddy keys + credits detail |
| GET | `/history?hours=24` | Request log stats (count, p50/p95 latency, error rate) |
| GET | `/history/recent?limit=50` | Recent request previews |
| GET | `/history/detail/:id` | Full request/response JSON for a request |

**Health check:**
```bash
curl -s http://127.0.0.1:20130/health -H "Authorization: Bearer $KEY" | python3 -m json.tool
```

**Recent requests:**
```bash
curl -s "http://127.0.0.1:20130/history/recent?limit=10" -H "Authorization: Bearer $KEY"
```

### 6. Custom Models & Aliases (admin only, v1.3.0)

Route any client-facing model id to `codebuddy` / `grok` with a chosen upstream
`model_name` — Redis-backed, no rebuild. Aliases rewrite the incoming model
name before routing (also honoured by the Anthropic Messages API).

**List custom models:**
```bash
curl -s http://127.0.0.1:20130/api/models/custom -H "Authorization: Bearer $KEY"
```

**Register a custom model:**
```bash
curl -s -X POST http://127.0.0.1:20130/api/models/custom \
  -H "Authorization: Bearer $KEY" -H "content-type: application/json" \
  -d '{"id":"cb/kimi-k3","upstream":"codebuddy","model_name":"kimi-k3","owned_by":"codebuddy"}'
# → {"ok":true,"id":"cb/kimi-k3"}
# Now visible in /v1/models AND routable via POST /v1/chat/completions {"model":"cb/kimi-k3",...}
```

**Delete a custom model** (id may contain `/`):
```bash
curl -s -X DELETE http://127.0.0.1:20130/api/models/custom/cb/kimi-k3 -H "Authorization: Bearer $KEY"
```

**Add / delete alias:**
```bash
curl -s -X POST http://127.0.0.1:20130/api/aliases \
  -H "Authorization: Bearer $KEY" -H "content-type: application/json" \
  -d '{"alias":"my-claude","target":"cb/claude-sonnet-4.6"}'
# → {"ok":true,"alias":"my-claude","target":"cb/claude-sonnet-4.6"}

curl -s -X DELETE http://127.0.0.1:20130/api/aliases/my-claude -H "Authorization: Bearer $KEY"
```

**Resolve order** (inside `proxy.ProxyRequest`):
1. Alias lookup (single hop; `a→b` does *not* chain to `b→c`)
2. Custom-model lookup on the resolved id — sets upstream + upstream model_name
3. Fall through to prefix routing (`grok-*` / `cb/*`)

Aliases and custom models live in Redis HASHes `custom_aliases` and
`custom_models`; the process caches them in memory behind a `sync.RWMutex` and
refreshes the map on every mutation.

## Python Helper

```python
import requests

FOXROUTERS_URL = "http://127.0.0.1:20130"

def get_key():
    with open("/root/nexus-workspace/foxrouters/gateway-key.txt") as f:
        return f.readline().strip()

def chat(model, messages, stream=False, **kwargs):
    """OpenAI-compatible chat completion via FoxRouters."""
    r = requests.post(
        f"{FOXROUTERS_URL}/v1/chat/completions",
        headers={"Authorization": f"Bearer {get_key()}"},
        json={"model": model, "messages": messages, "stream": stream, **kwargs},
        stream=stream,
        timeout=300,
    )
    r.raise_for_status()
    return r.json() if not stream else r.iter_lines()

def list_models():
    r = requests.get(f"{FOXROUTERS_URL}/v1/models", headers={"Authorization": f"Bearer {get_key()}"})
    return [m["id"] for m in r.json()["data"]]

def health():
    r = requests.get(f"{FOXROUTERS_URL}/health", headers={"Authorization": f"Bearer {get_key()}"})
    return r.json()
```

### 7. Proxy Pool Management (admin only, v1.5.0)

Dashboard-managed HTTP/SOCKS5 proxy pool with round-robin rotation. All upstream calls (Grok, CodeBuddy, token refresh, health checks) route through enabled proxies. Per-upstream scoping — assign a proxy to Grok only, CodeBuddy only, or both.

| Method | Endpoint | Purpose |
|--------|----------|---------|
| GET | `/api/proxies` | List all proxies (password masked) |
| POST | `/api/proxies` | Add proxy |
| PUT | `/api/proxies/:id` | Update proxy |
| DELETE | `/api/proxies/:id` | Delete proxy |
| POST | `/api/proxies/:id/toggle` | Enable/disable proxy |
| POST | `/api/proxies/:id/test` | Test proxy connectivity (returns exit IP + latency) |

**Add proxy:**
```bash
curl -X POST http://127.0.0.1:20130/api/proxies \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{
    "protocol": "socks5",
    "host": "proxy.example.com",
    "port": 1080,
    "username": "user",
    "password": "pass",
    "label": "datacenter-us",
    "upstreams": ["grok"]
  }'
# upstreams: ["all"] (default), ["grok"], ["codebuddy"], or ["grok","codebuddy"]
```

**Test proxy:**
```bash
curl -X POST http://127.0.0.1:20130/api/proxies/abc12345/test \
  -H "Authorization: Bearer $KEY"
# → {"success":true,"ip":"104.28.245.128","latency_ms":554}
```

**Toggle proxy:**
```bash
curl -X POST http://127.0.0.1:20130/api/proxies/abc12345/toggle \
  -H "Authorization: Bearer $KEY"
# → {"ok":true,"id":"abc12345","enabled":false}
```

**How it works:**
- `getClient(defaultClient, upstream)` checks pool for enabled proxies matching the upstream scope
- If found → returns `http.Client` with proxy transport (cached per proxy ID)
- If none → returns `defaultClient` (direct connection)
- Auto-disable after 5 consecutive failures (transport errors, 502/503/504)
- `MarkSuccess` resets fail count
- Transport cache: `sync.Map[proxyID → *http.Transport]`, invalidated on update/delete
- Round-robin: `atomic.Uint64` index, scoped by upstream filter

## Common Patterns

### Pick a model for a task
```bash
# Fast + cheap (CodeBuddy)
cb/gpt-5.5
cb/claude-sonnet-4

# High reasoning (Grok)
grok-4.5-high
grok-4.5-xhigh

# Balanced
grok-4.5
cb/gemini-3.1-pro
```

### Create a restricted key for a client
```bash
KEY=$(head -1 /root/nexus-workspace/foxrouters/gateway-key.txt)
curl -X POST http://127.0.0.1:20130/api/keys \
  -H "Authorization: Bearer $KEY" \
  -d '{
    "name": "client-x",
    "role": "inference",
    "allowed_models": ["cb/gpt-5.5"],
    "rpm": 30,
    "token_quota": 500000
  }'
# Save the returned key — only shown once on create
```

### Check if service is up
```bash
curl -s http://127.0.0.1:20130/health | python3 -c "import sys,json;d=json.load(sys.stdin);print(d['status'],d['version'])"
# Or HEAD only (fastest):
curl -sI http://127.0.0.1:20130/health | head -1
```

### Service management (host)
```bash
systemctl status foxrouters
systemctl restart foxrouters
journalctl -u foxrouters -f --since "5 min ago"
```

## Architecture

```
Client → AuthMiddleware (Bearer) → RateLimitMiddleware
       → /v1/chat/completions
            ↓
       proxyRequest (model routing + expandGrokAlias)
       ├── grok-* → proxyGrok (round-robin, 401 retry, 403 ban/cooldown)
       └── cb/*   → proxyCodeBuddy (stream transform, credit disable)
            ↓
       async LogRequest → ClickHouse (full body, ZSTD, 90d TTL)
```

- **Redis** = hot state (tokens, CB credits, disabled flags, gateway keys, rate state)
- **ClickHouse** = cold storage (full request/response JSON, 90d TTL)
- **Dashboard** = single-page app at `/dashboard`

## Pitfalls

- **Key shown only on create** — `POST /api/keys` returns full key once. Store it immediately.
- **Model must be in whitelist** — if key has `allowed_models` set, non-matching models return 403.
- **HEAD /health** returns 200 instantly (use for liveness probes). GET /health without auth = minimal, with auth = full telemetry.
- **Gin HEAD pitfall** — `curl -sI /login` or `curl -sI /dashboard` returns 404 because Gin doesn't auto-register HEAD for custom GET routes. Use `curl -s` (GET) to smoke-test these. Only `/health` has explicit HEAD registration.
- **Streaming** uses SSE — use `curl -N` or `iter_lines()` in Python.
- **Bulk import is idempotent** — duplicates are skipped (CB) or updated (Grok), not errored.
- **Grok account import** requires `email`, `access_token`, `refresh_token` (all 3 required). `id_token` and `expires_in` are optional (default 6h).
- **Admin endpoints** return 403 if key role is `inference`. Use admin key for management.
- **Service path** — binary at `/root/nexus-workspace/foxrouters/foxrouters`, config at `.gateway.env`.
- **X-Request-ID** — every response has an `X-Request-Id` header (8-byte hex). Honor inbound `X-Request-Id` from load balancers for end-to-end tracing. Request IDs are persisted to ClickHouse `request_logs.request_id` for correlation.
- **Dashboard auth is cookie-based (v5.11.6)** — `?key=` URL param and localStorage no longer work. Use `/login` page or Bearer header for API. Session cookie is HttpOnly (7 day expiry). Invalid/expired cookie → auto-redirect to `/login`.
- **`/health` auth check honors both Bearer AND cookie** — `handleHealth` returns minimal `{service,status,version}` to public callers but full telemetry (grok_accounts, cb_keys, upstreams) to authed callers. It checks `Authorization: Bearer` OR `foxrouters_session` cookie. If a dashboard shows "undefined" for stats, the cookie wasn't being honored (was Bearer-only before v5.11.6).
- **Gin HEAD pitfall for browser routes** — `curl -sI /login` or `curl -sI /dashboard` returns 404 because Gin only auto-registers HEAD for routes with explicit `.HEAD()`. Use `curl -s` (GET) to smoke-test browser routes. Only `/health` has explicit HEAD registration.
- **Fresh deploy auto-bootstraps** — on first boot with empty Redis, gateway generates a random admin key (`gw-` + 32 hex bytes), persists to Redis with `role=admin, name=bootstrap`, writes to `bootstrap-key.txt` (chmod 600, gitignored), and prints the key + login URL to the log once. Set `GATEWAY_NO_AUTOBOOTSTRAP=1` to disable (fail-closed mode) or `GATEWAY_AUTH_DISABLE=1` for dev.
- **Dashboard JS: `apiFetch` not `api`** — the dashboard has ONE fetch helper called `apiFetch(url, opts)` (async, auto-injects Bearer). Subagents writing new dashboard functions have repeatedly used `api(...)` (undefined) instead. If a dashboard page throws `api is not defined` or `X is not defined` at a custom-models/aliases button, grep `dashboard.html` for `\bapi\(` (excluding `apiFetch` / `.api` / comments) and fix every call site to `apiFetch(...)`.
- **Dashboard JS: `</script>` placement orphans functions** — when appending new JS functions (e.g. `loadCustomModels`, `addAlias`) to `dashboard.html`, the new functions MUST sit BEFORE the final `</script>` tag. A premature `</script>` (e.g. placed after the INIT block but before function definitions) causes `ReferenceError: loadCustomModels is not defined` at boot because the browser parses them as plain text. Before committing dashboard JS, verify: `grep -nE "<script>|</script>" dashboard.html` — expect one opening `<script>` (possibly a second for modal blocks, nested) and exactly ONE closing `</script>` at end-of-file, with every `async function X` between them.
- **DELETE route with slash-containing ids** — gin's `:param` matches a single path segment; ids like `cb/kimi-k3` or alias `foo/bar` will 404 on `DELETE /api/aliases/:alias`. Use the catch-all `*alias` / `*id` form (`r.DELETE("/api/aliases/*alias", ...)`) and strip the leading `/` inside the handler (`strings.TrimPrefix(c.Param("alias"), "/")`). The custom-models DELETE already uses `*id`; aliases had to be patched to match.
- **Anthropic adapter error envelope** — on upstream 4xx, surface a human-readable `message` string, NOT `string(bodyBytes)`. The upstream body is usually JSON (`{"error":"model not available on CodeBuddy","detail":"..."}`); embedding it raw inside `{"error":{"message":"<raw json>"}}` produces triple-escaped `JSON-in-JSON-in-JSON`. Use `extractUpstreamErrorMessage(raw, bodyBytes)` which tries `message`/`msg`/`error`/`detail` fields (including nested `{"error":{"message":...}}`) and falls back to trimmed raw body only if not JSON.
- **streamWriter error path** — when `stream:true` and the upstream errors BEFORE the first SSE frame, `streamWriter.WriteHeader(code)` must NOT commit to the real writer (delays double-WriteHeader panics). `streamWriter.Write` must buffer non-`data:` bytes into `errBuf` when `!started && statusCode>=400` (otherwise the line-splitter silently drops the upstream error body). The outer `HandleMessages` reads `sw.errBuf` first, falls back to `c.Get("response_body")`, then calls `extractUpstreamErrorMessage`.
- **Public repo = leak surface** — `https://github.com/rilspratama/Foxrouters` is PUBLIC. Before any push, scan for the VPS public IP and other secrets: `grep -rnE "([0-9]{1,3}\.){3}[0-9]{1,3}" . --include="*.md" --include="*.go" --include="*.yml" --include="*.sh" --include="*.html" | grep -v ".git/" | grep -vE "127\.0\.0\.1|0\.0\.0\.0|255\.255|1\.1\.1\.1|8\.8\.8\.8|192\.168\.|10\.0\.0|172\.(1[6-9]|2[0-9]|3[01])\."`. Replace any public IP with `<your-vps-ip>`. Note: git history still carries old leaks — only BFG rewrite scrubs them (risky for forks).
- **Git workflow: commit-local, batch-push** — the operator prefers accumulating commits locally and pushing in a batch (ideally with a version bump) rather than pushing every fix immediately. Fast pushes during/after big refactors (package split, file renames) cause structural conflicts with open external PRs (PR #2 was based on pre-split layout and couldn't merge). When an external PR is open, hold big structural pushes or coordinate with the contributor. Default cadence: commit → wait for operator's "push"/"gas" → push.
- **kimi-k3 not on CodeBuddy upstream** — as of 2026-07-19, `kimi-k3` returns `{"code":11102,"msg":"model [kimi-k3] service info not found"}` from www.codebuddy.ai/v2. `kimi-k2.5` works. Don't hardcode `cb/kimi-k3` into the model list; use the custom-models API (`POST /api/models/custom`) to register it at runtime if/when upstream enables it.

## Environment

- **Port:** 20130
- **Redis:** 127.0.0.1:6379 (hot state)
- **ClickHouse:** 127.0.0.1:9001 (history, native protocol)
- **Pool:** ~505 Grok accounts, ~1047 CB keys, 2 gateway keys, proxy pool (variable)
- **systemd:** `foxrouters.service`
- **Binary:** `/root/nexus-workspace/foxrouters/foxrouters` (30MB static)
