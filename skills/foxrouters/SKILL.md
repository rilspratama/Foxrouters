---
name: foxrouters
description: Interact with FoxRouters AI Gateway API — OpenAI-compatible proxy for Grok + CodeBuddy. Use when making LLM inference calls, managing API keys/accounts, or querying request history.
version: 1.0.0
tags: [ai-gateway, proxy, openai-compatible, grok, codebuddy, llm]
---

# FoxRouters AI Gateway

Unified OpenAI-compatible API gateway. Routes by model prefix: `grok-*` → Grok, `cb/*` → CodeBuddy. Multi-account/key round-robin, auto-refresh, circuit breaker, per-key RPM/quota, Redis hot-state, ClickHouse history.

## Connection

| Param | Value |
|-------|-------|
| **Base URL** | `http://127.0.0.1:20130` (localhost) or `http://76.13.18.146:20130` (public) |
| **Auth (API)** | `Authorization: Bearer <GATEWAY_KEY>` |
| **Auth (Dashboard)** | Cookie-based — visit `/login`, enter key, HttpOnly cookie set (v5.11.6). No more `?key=` URL param or localStorage. |
| **Gateway key file** | `/root/nexus-workspace/foxrouters/gateway-key.txt` (first line = admin key). On fresh deploy with empty Redis, gateway auto-generates a bootstrap admin key and writes it to `bootstrap-key.txt` (v5.11.6). |
| **Version** | 5.11.7 |
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
| `cb/*` | www.codebuddy.ai/v2 | `cb/gpt-5.5`, `cb/claude-opus-4.7`, `cb/gemini-3.1-pro`, etc. (39 models) |

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

## Environment

- **Port:** 20130
- **Redis:** 127.0.0.1:6379 (hot state)
- **ClickHouse:** 127.0.0.1:9001 (history, native protocol)
- **Pool:** ~505 Grok accounts, ~1047 CB keys, 2 gateway keys
- **systemd:** `foxrouters.service`
- **Binary:** `/root/nexus-workspace/foxrouters/foxrouters` (30MB static)
