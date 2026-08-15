# FoxRouters

## Project Overview
Unified OpenAI-compatible API gateway for **Grok + CodeBuddy**. Routes by model prefix:
`grok-*` → cli-chat-proxy.grok.com, `cb/*` → www.codebuddy.ai/v2.

Multi-account/key round-robin, auto-refresh (singleflight + pre-warm), circuit breaker,
API key auth, per-key RPM/quota, Redis hot-state, **SQLite** full-body history, web dashboard.

**Version:** v1.6.13-audit (`-X main.Version` build flag)
**Port:** 20130 · **Deploy:** Docker Compose (`docker compose up -d --build foxrouters`)

> **⚠️ DEV MUST NOT TOUCH PROD.** `docker compose up --build` from a dev
> session REPLACES the running prod container; if the build/start gets
> interrupted the new container is left stuck in `Created` and prod goes
> down. Use `./dev.sh` for dev work — it runs a FULLY ISOLATED stack with
> its **own Redis** (`foxrouters-dev-redis`, port 6381, own volume), its own
> sqlite volume, and container `foxrouters-dev` on **port 20131**. No shared
> Redis → no token-refresh/credit-sync races with prod. Prod (`:20130`) is
> never touched. Dev Redis starts empty (seed accounts manually if needed).
> `./dev.sh build|up|down|logs|test`.
>
> **Dev background workers + health probes are OFF.** `dev.sh` passes
> `WORKERS_DISABLED=1` + `HEALTH_PROBES_DISABLED=1`. Dev runs request-path
> only — no token refresh, credit sync, billing sync, re-enable, or upstream
> probes. This prevents rotating-refresh-token races with prod (Grok/CB RT
> rotation → `invalid_grant` → permanent disable in BOTH environments if two
> instances refresh the same account). To test worker behavior in dev,
> explicitly unset these env vars AND seed dev Redis with disposable test
> accounts — never prod credentials.  
**Path:** `/root/nexus-workspace/foxrouters/`

## Architecture / Flow

```
Client → AuthMiddleware (Bearer) → RateLimitMiddleware
       → /v1/chat/completions
            ↓
       proxyRequest (model routing + expandGrokAlias)
       ├── grok-* → proxyGrok (selector modes, 401 retry, 402/403 ban, 429 cooldown, 400 passthrough, billing sync, usage tracking)
       └── cb/*   → proxyCodeBuddy (stream-only transform, credit/14018 disable + Redis)
            ↓
       async LogRequest → SQLite (full body, TTL 90d)
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
8. Proxy pool: `getClient(default, upstream)` — returns proxied client if pool has enabled proxies matching upstream scope (`all`/`grok`/`codebuddy`/`freebuff`), else direct. Transport cache per proxy ID. Auto-disable after 5 fails.

### Token refresh
- **Grok:** Pre-warm every 30s, 30min window, 10 concurrent  
- `Next()` Pass1 valid token; Pass2 least-expired + async refresh  
- 401 rebuild request body + retry  
- **CB OAuth:** Pre-warm worker (30s tick, 30m window) + `EnsureValid` before chat + 401 refresh-retry. Singleflight + lock-split. Refresh via `POST /v2/plugin/auth/token/refresh` (`X-Refresh-Token`). Eager refresh on import when AT is expired/near-expiry and RT is valid.
- **Alibaba:** plain key RR (`ali/` prefix → DashScope compatible-mode). `Next()` snapshots the pool under one RLock (TOCTOU-safe) + `atomic.LoadUint64` cursor. Per-key usage (`RecordUsage`) + per-model usage (`RecordUsageModel` → `ali:model_usage:<model>`). Permanent disable on `AccessDenied.Unpurchased` (403), cooldown on 429, 4xx passthrough. Key actions via opaque `key_hash` (SHA-256 24 hex) — full secrets never leave the server. Bulk import capped at 500/batch. **Env gate:** `ALIBABA_DISABLED=1` skips provider. **Media Studio** (qwen-image gen/edit, wan2.x video) — plain Bearer, no SSO/DPoP/Turnstile.
- **Grok selector modes:** rr | sticky | content-hash | hybrid (default sticky). `GROK_SELECTOR_MODE` env, `GET/PUT /grok/selector-mode` (Redis `grok:config`). `NextForMode()` dispatches by mode. Sticky map + 30min TTL janitor. `x-session-id`/`x-conversation-id`/`x-chat-id` header pins conversation. Content-hash = FNV-1a(model + first system message). Hybrid = 3-key bucket + sticky within bucket.
- **Grok billing sync:** `GET /v1/billing?format=credits` every 5min (`GrokBillingSyncWorker`). 8 fields persisted (periodStart/End/Type, onDemandCap/Used, prepaidBalance, unifiedBilling). `POST /accounts/billing/sync` manual trigger. Weekly period rollover auto-resets usage counters.
- **Grok usage tracking:** Per-account cumulative tokens from response `usage` field. `RecordUsage()` non-blocking Redis persist. `GROK_FREE_TIER_QUOTA=1M`. Dashboard: 'tokens_used / 1M (pct%)'.
- **Grok error handling:** 401→refresh+retry (invalid_grant=permanent), 402/403+banned/spending-limit→permanent, 403 other→cooldown, 429→cooldown+Retry-After, 400→passthrough, 5xx→retry next.

### CB dual pool (`api_key` + `oauth`)
- Same chat endpoint: `www.codebuddy.ai/v2/chat/completions`
- Mixed round-robin over one `CBKey` pool (`cred_type`: `api_key` | `oauth`)
- Upstream auth: API key = Bearer or `X-API-Key`; OAuth = `Authorization: Bearer <AT>` only
- **Chat headers** (`cbChatHeaders`): mirrors current CLI (2.134.x) — `X-IDE-Type/Name/Version: CLI/2.134.0`, `X-Agent-Intent: craft`, `X-Agent-Purpose: conversation`, `X-Product: SaaS`, `X-Private-Data: false`, `x-codebuddy-request: 1`, `X-User-Id: anonymous_<last8>` (per credential), `User-Agent: CLI/2.134.0 CodeBuddy/2.134.0`, per-request `X-Conversation-ID`/`X-Request-ID` UUIDs. All optional (minimal header set works), sent to match upstream expectations. OTel (`traceparent`/`b3`) + `x-stainless-*` skipped.
- **Credit sync:** worker every 5m + `POST /cb/credits/sync`. Meter API `POST /v2/billing/meter/get-user-resource` (works for both modes). Persist limit/remain/package/cycle/status. Permanent disable on `Status==3`. Fallback `CB_CREDIT_LIMIT=240` if never synced.
- **OAuth login URL (device flow):** `POST /cb/oauth/device/start` → `auth_url` + `state` (upstream `POST /v2/plugin/auth/state?platform=CLI`); poll `GET /cb/oauth/device/poll?state=` until ready → client imports via `/cb/oauth/import`. Dashboard Add OAuth modal: **Manual | Login URL**.
- **Credential probe (Test):** `POST /cb/keys/test` (key or email) and `POST /accounts/test` (Grok email). Hits upstream directly with that credential (CB `gpt-5.5`, Grok `grok-4.5`); not pool RR. Disabled credentials still probed.

### Grok image generation (console.x.ai DPoP, free tier) — dev branch
- **Route:** `POST /v1/images/generations` (OpenAI Images API shape: `{model, prompt, n, size|aspect_ratio, response_format: b64_json|url}`) — dispatched in main.go catch-all.
- **PURE GO — no sidecar.** console.x.ai accepts Go's plain net/http **as long as the Cookie header is complete** (`sso` + `sso-rw`). The earlier "TLS fingerprint block" conclusion was WRONG: root cause was a missing `sso-rw` value (pool JSON key is `sso-rw` DASH — struct tags with `sso_rw` underscore silently drop it → mint 401). Always double-check cookie/JSON key spelling before blaming TLS.
- **LAZY SSO AUTH (operator directive, Aug 2026):** cookies CACHED in Redis, NEVER proactively refreshed. On 401 → `RefreshSSO(email)` = pure-HTTP re-login in-gateway (`LoginSSO`: local Turnstile solver `127.0.0.1:8742` sitekey `0x4AAAAAAAhr9JGVDZbrZOo0` + `POST /api/auth/sign-in`, ~3s, NO browser) → retry once. On 429 → `MarkImgExhausted` (lazy local count, limit 5) + rotate. NO `/v1/usage` round-trip worker — `GrokImageQuotaWorker` REMOVED (cookies die too often); quota counted locally (`IncrImgUsed` on 200). `POST /grok/quota/sync` = best-effort on-demand only.
- **Account fields (Redis `grok:account:<email>`):** `sso`, `sso_rw` (console SSO cookies, JWT), `password` (console login pwd for lazy re-login; `json:"-"` — never serialized), `img_cooldown_until` (unix). Import accepts `sso`/`sso_rw`/`password` (single + bulk).
- **Rotation:** `PickImageAccount(start)` = RR over accounts with SSO, not disabled, cooldown passed, quota not exhausted (lazy count). All exhausted → 429.
- **DPoP impl:** `internal/upstream/grok_dpop.go` — `GenerateImage(sso, ssoRW, prompt, aspect)` does mint (`POST /v1/dpop/token` `{jwk}` + Cookie) → ES256 proof (`typ: dpop+jwt`, claims jti/htm/htu/iat/ath, raw r||s) → `POST /v1/images/generations` (`grok-imagine-image`, `b64_json`). `SyncUsage(sso,ssoRW)` = GET /v1/usage (diagnostics).
- Verified end-to-end (dev gateway native + dev redis): direct 200 AND corrupt-SSO → 401 → auto re-login → 200.

### Media Studio — Chat tab (LLM prompt engineer)
- First tab = **Chat**: chat with any gateway model (default glm-5.2) using an image-prompt-engineer system prompt (MEDIA_SYS_PROMPT). Multi-turn refine; last assistant reply becomes "current image prompt" (auto-fills the Generate prompt box).
- **Generate Image →** runs the generate with the LLM-refined prompt; **Use in Edit** copies it to the edit prompt.
- Verified live: "red panda with sunglasses" → 100-word detailed prompt → photorealistic image matching all details.
### Media Studio (image gen / edit / video)
- Nav **Media** → **Media Studio** (#/media). 3 tabs:
  - **Generate** — POST /v1/images/generations (b64 preview, aspect ratio, n). Shares image quota 5/account (lazy local counter).
  - **Edit** — POST /v1/images/edits ({model, prompt, image(b64)} → {data:[{url}]} imgen.x.ai). Source = file upload or "Send to Edit" from Generate.
  - **Video** — POST /v1/videos/generations ({model, prompt} → {id}, model MUST be `grok-imagine-video`) + GET /v1/videos/:id (202 {status,progress} → 200 {data:[{url}]} vidgen.x.ai). Auto-poll 5s. Video quota 2/account.
- All media endpoints: lazy SSO auth (401 → LoginSSO via Settings-configured Turnstile solver → retry), 429 → MarkImgExhausted / MarkVidExhausted (24h cooldown) + rotate.
- Video owner mapping persisted to Redis (`grok:video:<id>`, 24h TTL) — polling survives gateway restarts.
- console.x.ai DPoP: proof(htm, htu, at) — htm must match HTTP method (GET for polls).
### Settings page (Turnstile Solver config)
- Nav **Settings** (#/settings) → `GET/PUT /settings/turnstile` (solver_url + sitekey, persisted Redis `gw:config`, env `TURNSTILE_SOLVER_URL`/`TURNSTILE_SITEKEY` fallback at startup) + `POST /settings/turnstile/test` (live solve → token_len + elapsed_ms + reachable status cards).
- Containers reach the host solver via `host.docker.internal` — compose `extra_hosts: ["host.docker.internal:host-gateway"]` + `TURNSTILE_SOLVER_URL=http://host.docker.internal:8742/cloudflare` default; dev.sh passes the same. Native (non-Docker) default stays `127.0.0.1:8742/cloudflare`.
- `LoginSSO` (lazy 401 refresh) reads the runtime config — no restart needed after Settings save.

### Grok aliases
`grok-4.5-{high,medium,low,xhigh,auto,none}` → `grok-4.5` + `reasoning_effort` (client wins if set).

**Grok chat headers** (`grokHeaders`/credential_probe): `x-grok-client-version: 1.0.0`, `x-grok-client-identifier: grok-shell`, `X-XAI-Token-Auth: xai-grok-cli`, `x-authenticateresponse: authenticate-response`, `x-grok-client-mode: tui`, `User-Agent: grok-shell/1.0.0 (linux; x86_64)`, `x-userid`/`x-email` (JWT sub + email). Mirrors shipped CLI (grok-build source, `xai_grok_version::VERSION` = `GROK_VERSION` stamp). Version source: npm `@xai-official/grok` dist-tags.latest == GCS pointer `storage.googleapis.com/grok-build-public-artifacts/cli/stable`.

### Freebuff provider (`fb/` prefix)
- **Upstream:** `www.codebuff.com/api/v1/chat/completions` (OpenAI-compatible, Bearer auth)
- **Models:** dynamic — fetched every 6h from `freebuff-models.json` (freebuff2api project, mirrors CodebuffAI/freebuff official source). Static fallback: `fb/deepseek-v4-flash`, `fb/mimo-v2.5` (limited mode), `fb/deepseek-v4-pro`, `fb/minimax-m3`, `fb/gpt-5.6-luna`, `fb/glm-5.2` (full mode only — US/EU residential IP). See **Dynamic Model Registry** below.
- **Auth:** Device-code flow → authToken UUID (no expiry). `POST /fb/oauth/device/start` → login URL → `GET /fb/oauth/device/poll` (auto-import on ready). Bulk import: `POST /fb/import/bulk` (pipe format `token|email|userid`, email+userid optional).
- **Session:** 1hr TTL, model-locked. `fbGetOrCreateSession` caches in-memory (L1) + Redis (L2). Switch model = DELETE + 5s wait + POST new.
- **Quota + tier:** `GET /api/v1/freebuff/session` (header `x-freebuff-include-unused-rate-limits: 1`) → `rateLimitsByModel.{model}.{limit, recentCount, resetAt}` + `accessTier` (`full`/`limited`/`blocked`) + `entitlementBreakdown`. `SyncQuota()` per account, `FbQuotaSyncWorker` every 5min. Auto-cooldown when `recentCount >= limit` until `resetAt`. Quota-aware `Next(model)` skips exhausted, prefers lowest `QuotaRecent`, skips limited-tier for premium models + blocked accounts always. Full-tier account detected ≤5min → premium models (deepseek-v4-pro/minimax-m3/gpt-5.6-luna/glm-5.2) auto-unlock, no restart.
- **Streak worker:** `FBStreakWorker` — fires ads impression + streak check-in (`fbFireAdsAndStreak`) for every account on a timer (default 24h, `FREEBUFF_STREAK_INTERVAL` env, min 1h) so daily streaks never break. First run 20s after startup. Manual trigger `POST /fb/streak/checkin` → `{checked, failed}`. Dashboard "Streak check-in" button.
- **Credential probe (Test):** `POST /fb/accounts/test` `{"token":"..."}` — probes chat upstream directly (`fb/deepseek-v4-flash`, always-available limited-tier model), warms session cache. Dashboard Test button per row.
- **Buffy prefix:** `fbTransform` auto-prepends `"You are Buffy, the strategic coding assistant.\n\n"` to system prompt. Client system prompt appended after.
- **Tool calling:** `end_turn` dummy tool injected to bypass foreign client detection.
- **Max tokens:** Auto-default 384K, auto-clamp to fit 1M combined context (prompt+completion ≤ 1,048,576).
- **Redis keys:** `fb:account:<token>` (account state + quota), `fb:session:<token>:<model>` (session cache), `fb:run:<token>:<agent>` (run cache).
- **Env gate:** `FREEBUFF_DISABLED=1` skips provider.
- **Dashboard:** Freebuff tab in Accounts page (5 buttons: +Add Token, Bulk Import, +Add OAuth, Sync Quota, Refresh). Overview cards: FB count+quota, FB circuit, FB latency, FB errors.
- **Endpoints:** `POST /fb/import`, `POST /fb/import/bulk`, `POST /fb/quota/sync`, `GET /fb/accounts`, `DELETE /fb/accounts/:token`, `POST /fb/oauth/device/start`, `GET /fb/oauth/device/poll`.

### Dynamic Model Registry (`internal/upstream/model_registry.go`)
Refreshes Freebuff + Grok model lists from upstream sources every **6h** so new
models appear WITHOUT code changes / rebuilds. Static fallback on any
fetch/parse failure (zero downtime). CodeBuddy is always static (no models
endpoint — verified).

| Upstream | Source | What's fetched |
|----------|--------|----------------|
| Freebuff | `pingmike2/freebuff2api-wokers` releases `freebuff-models.json` (daily generated, mirrors `CodebuffAI/freebuff`) | Full model table `{id, session, agent, upstream}` + `pools {premium, glm, standard}` → `FullMode` auto-detected (premium+glm = full-mode-only) |
| Grok | upstream `GET /v1/models` (live account) | Base model + `reasoning_efforts[]` → aliases generated dynamically (`auto`/`none` kept as gateway-internal) |
| CodeBuddy | — (none) | static |

- `/v1/models` reads the registry (Grok + Freebuff sections), static fallback.
- Workers: `FbModelsWorker(ctx)`, `GrokModelsWorker(ctx, grokAM)` — gated by `WORKERS_DISABLED=1` (dev).
- Manual trigger: `POST /models/refresh` (admin) → per-upstream `{source, synced_at, count, error}`.
- `fbModelConfig()` / `fbIsPremiumModel()` read the dynamic list (registry → static fallback).
- Accessors: `GetFBModels()`, `FBModelsInfo()`, `GetGrokModels()`, `GrokModelsInfo()`.
- GatewayID convention: `fb/<short-name>` — provider/ prefix stripped (`deepseek/deepseek-v4-flash` → `fb/deepseek-v4-flash`).

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
| `internal/db/logstore_sqlite.go` | modernc.org/sqlite backend (default; the only backend since v1.6.13 — ClickHouse removed) |
| `handlers.go` | health, accounts, history, keys, dashboard static |
| `auth.go` / `ratelimit.go` / `health.go` | Auth, RPM, circuit |
| `dashboard/` | Embedded SPA (go:embed dir) — modular parts: `head.html`/`body.html`/`modals.html` (HTML+CSS) + `core.js` (block 1: auth/routing/helpers/accounts) + `pages.js` (block 2: custom/combos/proxies/tunnel/settings/media + INIT). Assembled byte-identical in `main.go assembleDashboard()`. Nav: Dashboard/Accounts/Keys/Models/Proxies/Tunnel/Settings/Media |
| `internal/auth/session_store.go` | Session token → API key map (P3-3, 256-bit random tokens) |
| `internal/proxy/validate.go` | `validateName()` regex for id/alias/combo (P3-5) |
| `internal/proxy/combo.go` | ComboRegistry — fallback + round_robin strategies |
| `internal/proxy/pool.go` | ProxyPool — HTTP/SOCKS5 proxy pool, round-robin, per-upstream scoping, transport cache, auto-disable |
| `internal/handlers/combos.go` | Combos CRUD endpoints |
| `internal/handlers/custom.go` | Custom models + aliases CRUD endpoints |
| `internal/handlers/proxies.go` | Proxy pool CRUD + test + toggle endpoints |
| `proxy_pool_test.go` | Proxy pool tests (CRUD, validation, masking, round-robin, scoping) |
| `internal/upstream/codebuddy.go` | CB dual pool (api_key + oauth), OAuth refresh, meter SyncCredits, credit worker |
| `internal/upstream/codebuddy_device.go` | OAuth device/login URL: StartDeviceAuth, PollDeviceAuth, JWT email helpers |
| `internal/upstream/codebuddy_oauth_test.go` | OAuth import / EnsureValid / refresh tests |
| `internal/upstream/codebuddy_credit_sync_test.go` | Meter sync tests (API key + OAuth, Status==3 disable) |
| `internal/upstream/freebuff.go` | Freebuff pool, quota sync, session/run cache, Buffy prefix, fbTransform, ProxyFreebuff |
| `internal/upstream/model_registry.go` | Dynamic model registry — Freebuff (freebuff-models.json) + Grok (/v1/models) refresh workers, accessors, static fallback |
| `internal/handlers/freebuff_handlers.go` | FB import (single+bulk pipe format), quota sync, accounts list, delete, OAuth device flow |
| `internal/handlers/models.go` | `POST /models/refresh` — manual model-registry refresh (per-upstream source/count/error) |
| `internal/upstream/credential_probe.go` | Direct upstream Test for CB key/OAuth + Grok account |
| `internal/handlers/credential_probe.go` | `POST /cb/keys/test`, `POST /accounts/test` |
| `internal/proxy/filters.go` | Pudidil content filter — strips Claude/Anthropic identity + billing headers before upstream |
| `internal/proxy/filters_test.go` | Filter tests (billing, identities, tool_result nested, end-to-end) |
| `docs/OPENCODE.md` | OpenCode CLI integration guide (custom provider, reasoningEffort, tool calling) |
| `docs/HERMES.md` | Hermes reasoning_content display fix reference |
| `CHANGELOG.md` | Version history (v1.4.0 → v1.6.2) |
| `.gateway.env` | Secrets (chmod 600, gitignored) |

## Env (essentials)
```
REDIS_ADDR / REDIS_PASSWORD / REDIS_DB
LOG_BACKEND=sqlite               # sqlite (default) | clickhouse
LOG_SQLITE_PATH=/var/lib/foxrouters/logs.db  # only used when LOG_BACKEND=sqlite
CLICKHOUSE_ADDR=127.0.0.1:9000   # only used when LOG_BACKEND=clickhouse
CLICKHOUSE_DB=gateway
GATEWAY_KEY_FILE / CB_KEY_FILE
CB_SELECTOR_MODE=sticky          # rr | sticky (default) | content-hash | hybrid
GROK_SELECTOR_MODE=sticky        # rr | sticky (default) | content-hash | hybrid
PORT=20130
COOKIE_SECURE=0  # dev HTTP; omit for prod (defaults to HTTPS-only)
```

## Build / test / deploy
```bash
cd /root/nexus-workspace/foxrouters
export PATH=$PATH:/usr/local/go/bin
go test -count=1 -race ./... && go vet ./...   # REQUIRED first
docker compose up -d --build foxrouters        # rebuild + restart gateway
curl -s http://127.0.0.1:20130/health
```

## API (auth Bearer unless noted)
| Endpoint | Notes |
|----------|--------|
| `POST /v1/chat/completions` | Main proxy |
| `GET /v1/models` | Includes Grok aliases |
| `GET /health` | Public |
| `GET /history?hours=24` | Log stats |
| `GET /history/recent?limit=50` | Previews; `id` is **string** |
| `GET /history/detail/:id` | Full request/response JSON |
| `GET/POST /accounts` … | Grok import/delete/refresh |
| `POST /cb/import` | CB API key (`ck_*`) single |
| `POST /cb/import/bulk` | CB API keys bulk |
| `GET /cb/selector-mode` | Current key-selection mode + sticky count |
| `PUT /cb/selector-mode` | `{"mode":"hybrid"}` → switch + Redis-persist |
| `POST /cb/oauth/import` | CB OAuth single (email+AT+RT+expires_in?) — eager refresh if AT near-expiry |
| `POST /cb/oauth/import/bulk` | CB OAuth bulk (`accounts[]` JSON array) — idempotent by email |
| `POST /cb/oauth/device/start` | OAuth login URL — `{state, auth_url}` (platform default CLI) |
| `GET /cb/oauth/device/poll` | Poll after browser login — `pending` \| `ready` (tokens) \| `error` |
| `POST /cb/keys/test` | Probe one CB credential upstream (`{key}` or `{email}`) |
| `POST /accounts/test` | Probe one Grok account upstream (`{email}`) |
| `POST /cb/credits/sync` | Realtime meter sync (all `{}` or one by `email`/`key`) |
| `GET /grok/selector-mode` | Current Grok account selection mode + sticky count |
| `PUT /grok/selector-mode` | `{"mode":"hybrid"}` → switch + Redis-persist |
| `POST /accounts/billing/sync` | Manual billing sync (all `{}` or one by email) |
| `POST /models/refresh` | Manual dynamic model-registry refresh (Freebuff + Grok; per-upstream source/count/error) |
| `GET /cb-stats` | CB credits + `cred_type` / remain / package / meter_* |
| `GET/POST /api/keys` … | Gateway key CRUD |
| `GET/POST /api/proxies` … | Proxy pool CRUD + test + toggle |
| `GET /dashboard` | Public HTML; cookie session via `/login` |

## Dashboard UX prefs
- I/O text: modal only, never table columns  
- Total tokens: top stats card  
- History full JSON: tabs Request/Response (lazy detail)  
- Grok table: client-side pagination  
- **CB tab buttons (6):** `+ Add Key`, `+ Add OAuth`, `Bulk OAuth`, `Bulk Import`, `Sync credits`, `Cleanup Disabled`  
- **CB table:** Type badge (OAuth purple / API Key blue), Expires column, meter remain, **Test** + Delete per row  
- **Grok table:** Period End + Usage (tokens/1M) + **Test** + Delete per row, **Sync Billing** button  
- **Add OAuth modal:** segmented **Manual | Login URL** (generate auth URL → auto-poll → import)  


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
