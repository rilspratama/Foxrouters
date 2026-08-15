# API Reference

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
| `POST` | `/cb/import` | admin | Import a CodeBuddy API key (`ck_*`). |
| `POST` | `/cb/import/bulk` | admin | Bulk import CodeBuddy API keys. |
| `POST` | `/cb/oauth/import` | admin | Import CodeBuddy OAuth (email + AT + RT + expires_in?). Eager refresh if AT near-expiry. |
| `POST` | `/cb/oauth/import/bulk` | admin | Bulk OAuth import (`accounts[]`). Idempotent by email. |
| `POST` | `/cb/oauth/device/start` | admin | OAuth login URL flow — returns `{state, auth_url}` (platform default `CLI`). |
| `GET`  | `/cb/oauth/device/poll` | admin | Poll after browser login — `pending` \| `ready` (tokens + email?) \| `error`. |
| `POST` | `/cb/keys/test` | admin | Probe one CB credential upstream (`{key}` or `{email}`). Model `gpt-5.5`. |
| `POST` | `/accounts/test` | admin | Probe one Grok account upstream (`{email}`). Model `grok-4.5`. |
| `POST` | `/cb/credits/sync` | admin | Realtime meter sync — `{}` all, or `{email|key}` one. |
| `DELETE` | `/cb/keys/:key` | admin | Delete a CodeBuddy key (or email for OAuth). |
| `POST` | `/cleanup/disabled` | admin | Bulk-remove permanently disabled keys/accounts (`?type=all\|grok\|cb`). |
| `GET`  | `/cb-stats` | admin | CodeBuddy per-key credit / usage stats (`cred_type`, remain, package, meter_*). |
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

## Model Routing

Routing is driven purely by the `model` field of the incoming request:

| Model prefix | Upstream | Notes |
|---|---|---|
| `grok-*` | `https://cli-chat-proxy.grok.com` | Multi-account pool, refresh + 401 retry. |
| `cb/*` | `https://www.codebuddy.ai/v2` | Dual pool (`api_key` + `oauth`), mixed RR, meter credit sync, 14018/Status==3 disable. |
| `fb/*` | `https://www.codebuff.com/api/v1` | Freebuff free tier, session/run caching, quota sync + auto-cooldown. |
| `ali/*` | `https://dashscope-intl.aliyuncs.com/compatible-mode/v1` | Alibaba DashScope, key RR, per-key quota accumulator, AccessDenied disable+rotate. |

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

