# FoxRouters Changelog (this VPS)

**Service:** Docker Compose (`foxrouters` container) · port **20130** · image local / GHCR  
**Repo:** `/root/nexus-workspace/foxrouters/`  
**Live version:** `const Version` in `main.go` (currently **v1.6.1-oauth** local; GHCR may still be v1.6.0)

Policy: **test (`go test -race`) before build/restart**. Secrets only via `.gateway.env` (gitignored).

---

## v1.6.1 — CodeBuddy OAuth + Realtime Credits (2026-07-23)

### Added

| Feature | Description |
|---------|-------------|
| **CB OAuth dual pool** | `api_key` (ck_*) + OAuth JWT in same CBKey pool, mixed round-robin. OAuth: Bearer AT only, refresh via /v2/plugin/auth/token/refresh. |
| **CB OAuth import** | `POST /cb/oauth/import` (single) + `POST /cb/oauth/import/bulk` (JSON array). Idempotent by email. |
| **Eager refresh on import** | If supplied AT is expired/near-expiry, Refresh() via RT before pool entry. Fresh AT enters pool. |
| **Realtime credit meter** | `POST /v2/billing/meter/get-user-resource` (works for API key + OAuth). Worker every 5m + `POST /cb/credits/sync`. Persist limit/remain/package/cycle/status. Permanent disable on Status==3. |
| **Dashboard OAuth UI** | Type badge (OAuth purple / API Key blue), Expires column, + Add OAuth modal, Bulk OAuth modal, Sync credits button. |
| **CB OAuth auto-refresh** | Pre-warm worker (30s tick, 30m window) + EnsureValid before chat + 401 refresh-retry. Singleflight + lock-split. |

### Fixed

| Fix | Description |
|-----|-------------|
| **CB credit accuracy** | Meter API is source of truth (was SSE usage.credit interim only). Fallback CB_CREDIT_LIMIT=240 if never synced. |

---

## v1.6.0 — SQLite Default + Cloudflare Tunnel + Anthropic Tools + CB Disable Guards (2026-07-21)

### ⚠️ Breaking Change

**Default log backend changed from ClickHouse to SQLite.** Existing ClickHouse users must set `LOG_BACKEND=clickhouse` in `.env` before upgrading. The installer auto-detects existing ClickHouse installs (via `.env` or running container) and preserves the setting.

### Added

| Feature | Description |
|---------|-------------|
| **SQLite log backend (default)** | `modernc.org/sqlite` (pure Go, no CGO). `LOG_BACKEND=sqlite` (default) → single file at `LOG_SQLITE_PATH`, ~9 MiB RAM. `LOG_BACKEND=clickhouse` still fully supported. `NewLogStore()` factory switches on env. |
| **Cloudflare Tunnel — first-class Go feature** | `internal/tunnel/manager.go` with `cloudflare-go/v7` SDK for named tunnel management (control plane). Embedded `cloudflared` binary (data plane). 1 container for gateway + tunnel. Dashboard UI: Tunnel page with enable/disable + config modal. API: `/api/tunnel/{status,enable,disable,restart}`. Redis: `fr:tunnel:config` for persistence. Auto-start on boot. Modes: quick (random URL), named (custom domain), hybrid (both). |
| **Anthropic Messages API tool calling** | Full bidirectional tool translation on `POST /v1/messages`. Request: Anthropic `tools`/`tool_choice`/`tool_result` → OpenAI `tools`/`tool_choice`/`role:tool`. Response (stream + non-stream): OpenAI `tool_calls` → Anthropic `tool_use` blocks with `input_json_delta`. |
| **Anthropic model list fix** | `AnthropicAuthMiddleware` now covers all `/v1/*` paths (was `/v1/messages` only) — `x-api-key` accepted for `/v1/models`. `/v1/models` detects Anthropic clients and adds `display_name`, `created_at`, `type` fields alongside OpenAI fields. |
| **CB key disable guards** | 429 code 14017 (trial not activated) → permanent disable. 403 code 11140 (request illegal/banned) → permanent disable. Matching existing 401 and 14018 (credits exhausted) permanent disable behavior. |

### Fixed

| Fix | Description |
|-----|-------------|
| **Tunnel named modal CSS** | `.modal-overlay` used `.show` class but JS added `.active` → added `.active` to display rule. |
| **Installer auto-detection** | `install.sh` detects existing ClickHouse config/container and auto-sets `LOG_BACKEND=clickhouse` (upgrade safety). |

### Tunnel modes

| Mode | URL | Persistent? | Needs CF account? |
|------|-----|-------------|-------------------|
| quick | random `*.trycloudflare.com` | no | no |
| named | `gateway.example.com` | yes | yes (API token + account/zone ID) |
| hybrid | both simultaneously | quick=no, named=yes | yes for named |

Named tunnel is fully automated via Cloudflare API — no manual `cloudflared login` or cert.pem. User provides API token + account ID + zone ID + domain via dashboard form.

---

## v1.5.0 — Proxy Pool Manager + Security Audit Fixes (2026-07-20)

### Added

| Feature | Description |
|---------|-------------|
| **Proxy pool manager** | Dashboard-managed HTTP/SOCKS5 proxy pool with round-robin rotation. All upstream calls (Grok chat, CodeBuddy chat, token refresh, health checks) route through enabled proxies. Redis-backed CRUD, auto-disable after 5 consecutive failures, proxy test endpoint. |
| **Per-upstream proxy scoping** | Each proxy carries an `upstreams` list (`all` / `grok` / `codebuddy`). `ProxyPool.Next(upstream)` filters by scope — assign a proxy to Grok only, CodeBuddy only, or both. Backward compatible (old entries default `["all"]`). |
| **Dashboard Proxies page** | New `#/proxies` nav route — stats cards, proxy pool table with upstream badges, add/edit modal with upstream checkboxes, test/toggle/delete actions. |

### Fixed (security audit — 17 bugs across 3 tracks)

| Track | Fixes |
|-------|-------|
| **P0 data races (3)** | C1: `saveGrokAccount` reads fields after `Unlock()` (7 call sites) → `toDTO()` snapshot under RLock. C2: `saveCBKey` reads fields after `Unlock()` (5 sites) → capture locals inside lock. C3: `Manager.Get()` returns `*GatewayKeyInfo` pointer → changed to value-type `GatewayKeySnapshot` + per-key RWMutex. C5: `IncrementTokens` reads `info.Disabled` after unlock → capture inside lock. |
| **P1 concurrency (5)** | C6: SSE stream no client-cancel check → `ctx.Err()` + writer error check. C8: 401 double-fault after refresh → permanent disable. C9: `bufferedWriter` header copy clobbers middleware headers → `Add()` merge. C10: combo fallback no ctx propagation → `NewRequestWithContext`. |
| **P1+P2 dashboard+logs (7)** | D1: stored XSS via custom model id in `onclick` → `data-*` + delegated handler. D2: login redirect loop for inference-role users → admin-only gate. D3: dead Test button in cookie mode → conditional Bearer header. D7: `apiFetch` returns undefined on 401 → throw sentinel. D8: uncleared poll intervals → `_stopped` guard. S1: full API keys in Redis error logs → `maskRedisKey()`. S2: bootstrap admin key in logs → `MaskKey()`. |
| **Dashboard fixes** | `loadProxies` ReferenceError (typeof guard), scroll-to-top on page change, page-proxies rendered outside `div.content` (broken div nesting), proxies page UI (inline form → modal). |

### Security audit summary

| Round | Scope | Findings | Fixed |
|-------|-------|----------|-------|
| v1.4.6–v1.4.8 | Auth, session, CSRF, XSS, limiter | 12 items | 12/12 ✅ |
| v1.5.0 (3-track) | Data races, concurrency, dashboard, logs | 17 bugs (3 P0 + 6 P1 + 8 P2) | 17/17 ✅ |
| v1.5.0 (proxy pool) | SSRF, credential leak, injection, authz, races | 3 P3 defense-in-depth | 0 (accepted risk) |

### Files changed (v1.5.0)

**New:** `internal/proxy/pool.go`, `internal/handlers/proxies.go`, `proxy_pool_test.go`  
**Modified:** `internal/db/db.go`, `internal/upstream/{upstream,grok,codebuddy,health}.go`, `internal/auth/auth.go`, `internal/handlers/handlers.go`, `internal/proxy/{buffered_writer,proxy}.go`, `main.go`, `dashboard.html`, `go.mod`

---

## v1.4.8 — /health session cookie fix (2026-07-19)

### What changed

| Area | Before | After |
|------|--------|-------|
| `/health` endpoint | cookie = raw API key (pre-P3-3) → `am.Get(ck)` worked | cookie = session token (P3-3) → `am.Get(ck)` returned nil → minimal response |
| Dashboard stats | "CB KEYS: undefined active", circuit cards "—" | proper counts (976 keys, 502 grok accounts, upstreams populated) |
| JS console | `Cannot read properties of undefined (reading 'grok')` | 0 errors |

### Fix

`HandleHealth` now accepts a `sessions` parameter and resolves `cookie → session token → API key` via `sessions.Lookup()` before `am.Get(key)`.

### Files

- `internal/handlers/handlers.go` — `HandleHealth` +1 param
- `handlers_adapter.go` — `handleHealth`/`handleLogin`/`handleLogout` → function wrappers (var aliases don't match new signatures)
- `main.go` — pass `sessions` to `handleHealth`

---

## v1.4.7 — Security re-audit fixes (2026-07-19)

7 bugs fixed from second security audit (commit `a49df94`):

| # | Bug | Fix |
|---|-----|-----|
| P2-1 | Login rate-limit bypass via `X-Forwarded-For` spoofing | `r.SetTrustedProxies(nil)` — don't trust XFF |
| P2-2 | CSRF on admin mutations (cookie session + no Origin check) | New `csrf_guard.go` — Origin/Referer check on cookie-authed POST/PUT/DELETE |
| P3-1 | Admin can delete last admin key → self-DoS | `CountAdmins()` + 409 refuse if last admin |
| P3-2 | Unbounded combo `models` array → Redis DoS | Cap `len(c.Models) <= 32` |
| P3-3 | Session fixation (cookie = raw API key) | New `internal/auth/session_store.go` — 256-bit random session tokens, 7-day TTL |
| P4-1 | `loginLimiter.cleanup()` trimmed minuteWindow by hourCutoff | 1-char fix: `hourCutoff` → `minCutoff` |
| P4-2 | Legacy inline `onclick` on pagination buttons (10 occurrences) | Migrated to `data-page`/`data-action` + event delegation |

### New files

- `csrf_guard.go` — Origin/Referer check middleware
- `internal/auth/session_store.go` — `SessionStore` (in-memory token → key map)

### Compliance

12/12 security checklist items ✅ (was 9/12 after v1.4.6)

---

## v1.4.6 — Security audit fixes (2026-07-19)

9 bugs fixed from first security audit (commit `3f0c82e`):

| # | Bug | Fix |
|---|-----|-----|
| P1-1 | Stored XSS in dashboard (6 `onclick` handlers) | `data-*` attributes + global event delegation |
| P2-2 | 26 Go stdlib CVEs (Go 1.25.0 stale) | `go.mod` → `go 1.25.12` (auto-upgrade) |
| P2-3 | Cookie missing `Secure` flag | `Secure: true` default + `COOKIE_SECURE=0` dev override |
| P3-4 | `key_full` leak in `/accounts` + `/cb-stats` | Removed; `ResolveKey(masked)` server-side on delete |
| P3-5 | No input validation (id/alias/combo) | `validateName()` regex `^[A-Za-z0-9._/\-]{1,64}$` |
| P3-6 | No rate limit on `/login` | `login_limiter.go` — 5/min + 20/hour per IP |
| P3-7 | Upstream `requestId` leak in errors | Generic error message only |
| P3-8 | Empty role → admin fallback | Default → `RoleInference` (least privilege) |
| P4-9 | Dead path check (`path != "/api/"`) | `!strings.HasPrefix(path, "/api/")` |

### New files

- `internal/proxy/validate.go` — `validateName()` helper
- `login_limiter.go` — IP-based rate limiter for `/login`

### Verification

- `govulncheck ./...` → 0 vulnerabilities (26 CVEs gone)
- `go test -race` → 62/62 PASS

---

## v1.4.5 — Custom Models tab + Combos selector (2026-07-19)

### UI restructure

| Area | Before | After |
|------|--------|-------|
| Nav bar | 5 items (Dashboard, Accounts, Keys, Models, Custom) | 4 items (Dashboard, Accounts, Keys, Models) |
| Custom Models | Separate page (`#/custom`) | Tab inside page Models |
| Combos form | Textarea (manual model input) | Checkbox selector (mirip API keys) |
| Models page | Single view | 3 tabs: **Models** \| **Custom** \| **Combos** |

### Combos model selector

- `loadComboModelSelector()` — fetch `/v1/models`, cache, render grouped checkboxes
- `toggleAllComboModels(check)` / `toggleComboGroup(prefix)` — bulk select
- `getSelectedComboModels()` — return checked values
- `updateComboSelectedCount()` — real-time counter

---

## v1.4.3 — Combos moved to tab in Models page (2026-07-19)

- Removed nav item "Combos" + route `#/combos` + `page-combos`
- Added tab selector: `[Models] [Combos]` at top of page-models
- Combo content moved to `#mtab-pane-combos` (default hidden)
- `switchMTab('models'|'combos')` function for tab switching
- `loadCombos()` auto-triggered on tab switch

---

## v1.4.2 — CB key masked fix in /cb-stats (2026-07-19)

**Root cause:** Fix sebelumnya (`36b5602`) cuma `/accounts` yang dapat `key_full`, padahal dashboard `refresh()` pakai `/cb-stats` buat render CB table → key masih masked → delete 404.

**Fix:** Tambah `key_full` ke `/cb-stats` response juga (main.go inline handler). *(Note: `key_full` removed again in v1.4.6 in favor of server-side `ResolveKey(masked)`.)*

---

## v1.4.1 — CB key delete 404 fix (2026-07-19)

**Bug:** CB key delete return 404 karena dashboard pass masked key (`ck_abcde...wxyz`) ke DELETE endpoint, tapi backend expect full key.

**Fix:** Backend return `key_full` field di `/accounts` response (admin-only via `adminAuth`). Dashboard pakai `k.key_full || k.key` buat delete (backward compat). *(Note: `key_full` removed in v1.4.6; delete now resolves masked → full server-side.)*

---

## v1.4.0 — Combos (fallback + round-robin strategies) (2026-07-19)

### What changed

| Area | Before | After |
|------|--------|-------|
| Multi-model routing | One model per request | + **combos**: `combo/<name>` groups N models under one virtual alias |
| Reliability | Retry within a single upstream | + **fallback strategy**: try `models[0]`, on 5xx buffer response + retry `models[1]`, `models[2]`, … up to list end |
| Load-spreading | Manual per-request model choice | + **round_robin strategy**: atomic `INCR combo:counter:<name>` rotates models across requests (cluster-safe) |
| Model catalog | Hardcoded + custom models | + combos appear in `/v1/models` as `combo/<name>` with `owned_by: foxrouters` |
| Dashboard | 5 pages | + **Combos** page (`#/combos`, admin) — create form + table with delete |

### New endpoints (admin-only, Bearer auth)

- `GET  /api/combos` — list every combo
- `POST /api/combos` — `{name, strategy, models[], description?}` (strategy: `fallback` | `round_robin`)
- `GET  /api/combos/*name` — fetch one combo
- `DELETE /api/combos/*name` — remove combo + its round-robin counter

### Example

```bash
# Create a Fallback combo (GPT-5.5 → Claude → Grok-4.5)
curl -X POST http://127.0.0.1:20130/api/combos \
  -H "Authorization: Bearer $ADMIN_KEY" -H "content-type: application/json" \
  -d '{"name":"smart-fallback","strategy":"fallback",
       "models":["cb/gpt-5.5","cb/claude-sonnet-4.6","grok-4.5"],
       "description":"GPT then Claude then Grok"}'

# Call it — client sees the concrete backend response, retries are transparent
curl -X POST http://127.0.0.1:20130/v1/chat/completions \
  -H "Authorization: Bearer $CLIENT_KEY" -H "content-type: application/json" \
  -d '{"model":"combo/smart-fallback","messages":[{"role":"user","content":"hi"}]}'
```

### Redis schema

- `combos` HASH — `field=<name>`, `value=Combo JSON`
- `combo:counter:<name>` STRING — atomic INCR for round-robin (auto-deleted on combo delete)

### Implementation notes

- Fallback retry is non-streaming-only: streaming requests use `models[0]` (once bytes hit the wire we can't un-send). SSE clients keep the head-of-list model without retry.
- 4xx responses aren't retried (client error, not upstream). Only 5xx walks the chain.
- Resolution order: custom-alias → combo → grok-alias → default prefix routing.
- Combos of aliases work: the retry loop re-runs `CustomRegistry.Resolve` on each candidate so alias-of-model in a combo still resolves correctly.

### Files

- New: `internal/proxy/combo.go`, `internal/proxy/buffered_writer.go`, `internal/handlers/combos.go`, `combo_test.go`
- Modified: `internal/db/db.go` (Combo type + Load/Save/Delete/IncrCounter), `internal/proxy/proxy.go` (combo resolution + fallback retry loop), `main.go` (registry init + 4 routes), adapters, `dashboard.html` (nav + page + JS)

**Tests:** 62 total (52 pre-existing + 10 new). `go vet ./...` clean, `go test -count=1 -race` green.

---

## v1.3.0 — Custom Models + Aliases (runtime-configurable) (2026-07-19)

### What changed

| Area | Before | After |
|------|--------|-------|
| Model catalog | Hardcoded ~42 entries in `internal/proxy/proxy.go` | + **runtime custom models** via `POST /api/models/custom` (Redis-backed, no rebuild) |
| Anthropic aliases | Hardcoded `claude-*` → `cb/claude-sonnet-4.6` | + **user-defined aliases** checked first (`mapAnthropicModel` walks the alias table before the built-in rules) |
| Model routing | Prefix-based (`grok-*` / `cb/*`) | + **custom model override** — declare upstream (`codebuddy`/`grok`) + upstream `model_name` per id |
| Dashboard | 4 pages (Dashboard, Accounts, Keys, Models) | + **Custom Models & Aliases** page (`#/custom`, admin) with CRUD forms |
| Tests | 38 tests | **52 tests** — 14 new covering registry Resolve, CRUD, `/v1/models` append, concurrency |

### New endpoints (all admin)

| Method | Path | Body / Response |
|--------|------|----------------|
| `GET` | `/api/models/custom` | `{models: {id: {upstream, model_name, owned_by}}}` |
| `POST` | `/api/models/custom` | `{id, upstream, model_name, owned_by?}` — validates upstream ∈ {codebuddy, grok} |
| `DELETE` | `/api/models/custom/*id` | slash-tolerant path (id may be `cb/kimi-k3`) |
| `GET` | `/api/aliases` | `{aliases: {alias: target}}` |
| `POST` | `/api/aliases` | `{alias, target}` — rejects self-loops |
| `DELETE` | `/api/aliases/:alias` | |

### Redis schema

- `custom_models` HASH: field=model_id (e.g. `cb/kimi-k3`), value=JSON `{upstream, model_name, owned_by}`
- `custom_aliases` HASH: field=alias, value=target model_id

### New files

- `internal/proxy/custom.go` — `CustomRegistry` (sync.RWMutex-guarded in-memory cache backed by Redis)
- `internal/handlers/custom.go` — 6 admin handlers
- `custom_test.go` — 14 tests (Resolve, ListModels, CRUD, validation, concurrency, /v1/models append)

### Modified files

- `internal/db/db.go` — `CustomModel` struct + 6 methods (`Load/Save/DeleteCustomModel`, `Load/Save/DeleteCustomAlias`); new consts `RK_CUSTOM_MODELS`, `RK_CUSTOM_ALIASES`
- `internal/proxy/proxy.go` — Resolve step before Grok-alias expansion; routing switch honours `customUpstream`; `/v1/models` appends `registry.ListModels()`; ClickHouse `Upstream` field now uses `upstreamName`
- `internal/handlers/anthropic.go` — `mapAnthropicModel(m, reg)` consults aliases first, then falls back to hardcoded rules; signature threading for `buildOpenAIBody` + `HandleMessages`
- `main.go` — startup `customReg := NewCustomRegistry(db); customReg.Load()`; 6 new routes; registry threaded to `proxyRequest` + `handleMessages`
- `handlers_adapter.go`, `db_adapter.go`, `proxy_adapter.go` — new re-exports
- `dashboard.html` — new page `#/custom` (Custom Models table + Alias table + inline forms), nav item, route wiring, 6 JS functions

### Design notes

- **Load once, refresh on mutation**: `NewCustomRegistry(db).Load()` at boot; every Add/Delete mutates Redis first, then updates the map under `sync.RWMutex`. Hot-path Resolve is a single RLock.
- **Alias resolution is single-hop** — no recursion, no cycles. Documented in `Resolve()` godoc.
- **Custom models override prefix routing** — Grok alias expansion (`grok-4.5-high` → `grok-4.5`) is skipped when a custom model has already claimed the id, so the operator can register `grok-4.5-high` as a custom entry without the alias table stealing it.
- **cbTransform integration**: for `codebuddy` upstream we set `bodyMap["model"] = "cb/" + customModelName` so the existing `stripCBPrefix` in `cbTransform` unwraps it to the intended model_name.
- **Nil-safe store**: registry accepts a `nil` `*db.Store` (used by unit tests) — mutations touch the in-memory map only.

### Commits

_(see git log for `feat: custom models + aliases`)_

---

## v1.2.0 — Anthropic Messages API + GPT-5.6 + cleanup tooling (2026-07-19)

### What changed

| Area | Before | After |
|------|--------|-------|
| API format | OpenAI-only (`/v1/chat/completions`) | + **Anthropic Messages API** (`POST /v1/messages`) — Claude Code compatible |
| Auth header | `Authorization: Bearer` only | + `x-api-key` (Anthropic standard) — both accepted |
| Model catalog | 39 models | **42 models** — added `cb/gpt-5.6-sol`, `cb/gpt-5.6-terra`, `cb/gpt-5.6-luna` |
| Key management | Grok delete + auth key delete only | + **CB key delete** (`DELETE /cb/keys/:key`) + **cleanup disabled** (`POST /cleanup/disabled?type=all\|grok\|cb`) |
| Dashboard | View-only for CB keys | **Delete button** per CB key + per Grok account + **Cleanup Disabled** button |
| Logging | `log.Printf` ad-hoc | **slog structured logging** (86 calls migrated, `LOG_LEVEL=debug\|warn\|error`) |
| Metrics | None | **Prometheus `/metrics`** — `foxrouters_requests_total`, `request_duration_seconds`, `active_keys`, `disabled_keys`, `circuit_state` |
| Version | Hardcoded `const Version = "5.11.2"` | **`-ldflags -X main.Version=<tag>`** — fallback `dev`, injected via Dockerfile + CI |
| Code structure | Flat `package main` (5,441 LOC) | **7 `internal/` packages** — `metrics`, `ratelimit`, `db`, `auth`, `upstream`, `proxy`, `handlers` |
| Tests | 22 unit tests | **38 tests** (22 unit + 16 integration) |
| Shutdown drain | `time.Sleep(500ms)` | **`sync.WaitGroup`** with 10s timeout (no log loss) |
| CB 429 handling | Permanent ban | **Cooldown 10min** (401 still permanent) |

### New endpoints

| Method | Path | Purpose |
|--------|------|---------|
| `POST` | `/v1/messages` | Anthropic Messages API (Claude Code compatible) |
| `DELETE` | `/cb/keys/:key` | Delete a CodeBuddy key |
| `POST` | `/cleanup/disabled?type=all\|grok\|cb` | Bulk-remove permanently disabled keys/accounts |
| `GET` | `/metrics` | Prometheus metrics (public) |

### Claude Code integration

```bash
export ANTHROPIC_BASE_URL=http://localhost:20130
export ANTHROPIC_API_KEY=gw-xxx
claude
```

Model mapping: `claude-*` → `cb/claude-sonnet-4` (default), `*-grok` → `grok-4.5`, explicit `cb/*` / `grok-*` passthrough.

### Commits
```
106a4d1 feat: add GPT-5.6 models + Anthropic Messages API adapter
bd1975b feat: dashboard UI for delete CB key + cleanup disabled
cc715b7 feat: add CB key delete + cleanup disabled endpoints
3f37406 refactor: complete package split — main.go slim
170b91f refactor: extract internal/handlers
abaff4a refactor: extract internal/proxy
8f2ca67 refactor: extract internal/upstream
ffbe6a3 refactor: extract internal/auth
63f3a4d refactor: extract internal/db
c52e2cd refactor: extract internal/ratelimit
dfce981 refactor: extract internal/metrics
a7f5291 feat: version ldflags, slog structured logging, prometheus metrics, integration tests
706bbbf fix: P1 audit issues (CH port, gin ctx, CB load error, CB 429 cooldown, shutdown drain)
```

---

## v5.11.2 — Security hardening + admin scope split (2026-07-18)

### What changed
| Area | Before | After |
|------|--------|-------|
| Auth scope | Flat — any bearer = full admin | **Role-based** — `inference` (default) vs `admin` |
| Admin endpoints | All keys access `/api/keys`, `/accounts`, `/history`, `/cb-stats` | **AdminMiddleware** gates these — inference keys get 403 |
| Auth fail mode | Fail-open (no keys = allow all) | **Fail-closed** — reject if no keys loaded (override: `GATEWAY_AUTH_DISABLE=1`) |
| http.Server | No timeouts (Slowloris risk) | `ReadHeaderTimeout=10s`, `IdleTimeout=120s`, `MaxHeaderBytes=1MB` |
| HEAD /health | Hung (Gin HEAD→GET handler path issue) | Explicit `handleHealthMinimal()` — instant 200 |
| /v1/responses | Dead references in ratelimit + log path | Cleaned — only `/v1/chat/completions` is valid |
| CH error capture | `error_msg` + `response_body` empty on 400/503 | All non-2xx branches now set both fields |
| systemd | Root, no sandbox | `NoNewPrivileges`, `ProtectSystem=strict`, `PrivateTmp`, `ProtectKernel*`, `RestrictAddressFamilies`, `CapabilityBoundingSet` |

### Commits
```
(this patch set)
```

### Migration notes
- Existing bootstrap keys (from `gateway-key.txt` / env) auto-assigned `role=admin` — no action needed.
- Redis-stored keys created before this version default to `admin` (backward compat in `parseGatewayKeyFromRedis`).
- New keys created via `POST /api/keys` default to `role=inference` unless `"role":"admin"` is specified.
- To create an admin key: `POST /api/keys {"name":"ops","role":"admin"}`.
- systemd hardening requires `ProtectSystem=strict` — gateway only writes to `WorkingDirectory` + Redis/CH sockets. If you add file writes outside `/root/nexus-workspace/foxrouters/`, loosen `ProtectSystem` to `full` or `true`.

---

## v5.10.0 — ClickHouse history + full body + dashboard JSON fix (2026-07-17)

### What changed
| Area | Before | After |
|------|--------|--------|
| History store | PostgreSQL JSONB (64KB body cap after v5.9) | **ClickHouse** `gateway.*` full body **unlimited** (ZSTD) |
| Credentials | Redis | Redis (**unchanged**) |
| Body policy | Cap 64KB then briefly 16MB soft-cap | **Unlimited** passthrough (`bodyString` no truncate) |
| Dashboard Request/Response JSON tabs | Often empty | Fixed — **log `id` as JSON string** (UInt64 > JS `MAX_SAFE_INTEGER`) |
| Stats `/history` | PG | CH flat aggregates; token totals summed in Go (no nested `sum`) |

### Commits
```
2b2edcd  fix: dashboard JSON tabs — string IDs + unlimited body
4e4e452  fix: CH stats — sum tokens in Go
2850887  fix: CH nested aggregate error 184
03100b2  feat: migrate history PG → ClickHouse
```

### Ops
- ClickHouse **26.x**, native **`127.0.0.1:9001`** (9000 taken), HTTP **8123**
- Env: `CLICKHOUSE_ADDR`, `CLICKHOUSE_DB`, `CLICKHOUSE_USER`, `CLICKHOUSE_PASSWORD`
- Schema auto-ensure on boot; TTL **90 days** on history tables
- Legacy PG data may remain on disk — **gateway no longer reads/writes it**

### Verified
- Full bodies ~90KB–900KB stored and returned via `/history/detail/:id`
- Compression ~**4.7×** on `request_logs` parts
- Dashboard list previews + lazy full JSON tabs after hard refresh

### Docs
- Skill `foxrouters-development` + `references/clickhouse-history-migration.md`
- `references/dashboard-history-json-tabs-uint64.md`

### Explicit non-goals (user decision)
- No further “Tier A / client-side” optimisations (context trim, model routing, reasoning defaults) — those are **client/Hermes**, not gateway.
- Gateway optimisations already shipped (v5.9 hot path) considered **enough** for now.

---

## v5.9.0 — Hot-path performance (2026-07-17)

**Commit:** `ae41b31`

| Optimisation | Detail |
|--------------|--------|
| `Len()` O(1) | Replace `len(GetAll())` on hot path |
| Re-enable off `Next()` | `reenableWorker` / `reenableCBWorker` every 1m |
| Quiet logs | No success-path spam |
| `AccountID` | Set from upstream account/key |
| Body log (then) | Cap 64KB toward PG (superseded by v5.10 unlimited CH) |
| Refresh | `singleflight` + lock-split (no mutex across HTTP) |
| SSE | Single unmarshal + line carry + buffer pool |
| Managers | `RWMutex` for readers |
| Version | Single `const Version` |

See `references/v5.9-performance-optimizations.md` in skill.

---

## v5.8.x — P0/P1 correctness (2026-07-17)

| Commit | Scope |
|--------|--------|
| `94ccb19` | Dashboard no live key inject; MaxBytesReader 10MB; unlock-before-save re-enable; graceful shutdown 15s |
| `465a549` | Auth RLock; import race; circuit no false-open on pool exhaust; health 2xx/3xx only |
| `ab57e8b` | 401 retry rebuild body; env DB secrets; (intermediate dashboard inject — later removed) |
| `972957b` | Persist Grok/CB disable + invalid_grant; gzip writer create/close once |

See `references/p0-p1-correctness-audit.md`.

---

## Architecture (current)

```
Client → Auth → RateLimit → proxyRequest
  ├── grok-* (+ aliases) → proxyGrok → cli-chat-proxy.grok.com
  └── cb/*               → proxyCodeBuddy → www.codebuddy.ai/v2
        ↓
  memory Next() O(k)
  Redis: tokens / credits / disabled / gw keys
  ClickHouse async: full request_logs (unlimited body, ZSTD)
```

| Store | Role |
|-------|------|
| **Redis** | Hot credentials & serve state |
| **ClickHouse** | Cold history + full JSON bodies |
| **PostgreSQL** | Retired for gateway history |

---

## Performance notes (observed)

- Gateway `/health` ~1–4 ms; `/history/recent` ~10 ms; chat latency = **upstream** (Grok p50 ~30s on large contexts; CB simple ~1–3s).
- Full body @ ~0.6 MB/req is fine at current traffic; **1k RPS full-body** is disk/network bound, not CH engine; chat 1k RPS dies at LLM/pool first.
- Remaining latency wins for “faster LLM feel” are mostly **client** (context size, model choice, reasoning effort) — deferred by operator.

---

## Quick ops

```bash
cd /root/nexus-workspace/foxrouters
export PATH=$PATH:/usr/local/go/bin
go test -count=1 -race ./... && go vet ./...
go build -o foxrouters . && systemctl restart foxrouters.service
curl -s http://127.0.0.1:20130/health
clickhouse-client --port 9001 -q 'SELECT count(), max(length(request_body)) FROM gateway.request_logs'
```

Dashboard: `http://<host>:20130/dashboard?key=<gw-key>` once (localStorage). **Never** re-inject live keys into HTML.
