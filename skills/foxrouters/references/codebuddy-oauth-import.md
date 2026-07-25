# CodeBuddy OAuth import (operator quick ref)

See full RE + implementation: skill `ai-gateway-development` → `references/codebuddy-oauth-dual-pool.md`.

## Endpoints
- `POST /cb/import` — API key `ck_*`
- `POST /cb/import/bulk` — API keys bulk only
- `POST /cb/oauth/import` — OAuth single (`{email, access_token, refresh_token, expires_in?}`)
- `POST /cb/oauth/import/bulk` — OAuth bulk (`{accounts:[{email, access_token, refresh_token, expires_in?}, ...]}`) — idempotent by email
- `POST /cb/oauth/device/start` — Login URL flow → `{state, auth_url}` (upstream `POST /v2/plugin/auth/state?platform=CLI`)
- `GET /cb/oauth/device/poll?state=` — `pending` | `ready` (tokens + email?) | `error`
- `POST /cb/keys/test` — probe one CB credential upstream (`{key}` or `{email}`) — model `gpt-5.5`
- `POST /accounts/test` — probe one Grok account upstream (`{email}`) — model `grok-4.5`
- `POST /cb/credits/sync` — meter sync one/all (`{}` or `{email|key}`)
- `GET /cb-stats` — `cred_type`, `email`, `expires_at`, `credits_remain`, package, meter_*

## Eager refresh on import
If the supplied access token is expired or near-expiry and the refresh token is valid, the gateway calls `Refresh()` via `POST /v2/plugin/auth/token/refresh` **before** the credential enters the pool. Fresh AT is stored; failed eager refresh still stores as-is (logged as warning).

## OAuth Login URL (device flow)
1. Dashboard **+ Add OAuth** → tab **Login URL** (or curl `/cb/oauth/device/start`)
2. Open `auth_url` in browser → GitHub/Google login on CodeBuddy
3. Poll `/cb/oauth/device/poll?state=` every ~3s (dashboard auto-polls, ~5 min max)
4. On `ready` → client POSTs tokens to `/cb/oauth/import` (eager refresh still applies)
5. Email resolved from JWT claim / nickname / uid fallback if not provided

No server-side session store — each poll is a fresh upstream call. Tokens never logged full.

## Upstream auth (verified)
| Type | Header | Refresh |
|------|--------|---------|
| api_key | `Authorization: Bearer ck_*` or `X-API-Key` | none |
| oauth | `Authorization: Bearer <AT>` only (`X-API-Key` → 401) | `POST /v2/plugin/auth/token/refresh` + `X-Refresh-Token` (cli\|plugin); no `/plugin` → 404 |

Gateway auto-refresh: pre-warm 30s/30m + EnsureValid + 401 retry. TTL often ~365d. Dual pool: `api_key` + `oauth` mixed RR on same chat endpoint.

## Credential Test
- Per-row **Test** on Grok + CodeBuddy tables
- Hits **that credential** against upstream (not pool RR)
- CB: `gpt-5.5` stream mini-chat; Grok: `grok-4.5` non-stream
- OAuth: `EnsureValid()` first; disabled credentials still probeable (recovery check)
- Response: `{ok, status, latency_ms, model, content?, credit?, error?}`

## Dashboard
- Type badge (OAuth purple / API Key blue), Expires column, meter remain
- Buttons: `+ Add Key`, `+ Add OAuth` (**Manual | Login URL**), **Bulk OAuth**, Bulk Import, **Sync credits**, Cleanup Disabled
- Per-row: **Test** + Delete (Grok + CB)
- `dashboard.html` is `//go:embed` — rebuild Docker after UI edits
- Bare curl `/dashboard` without login cookie can look empty

## Credits
Realtime meter: `POST www.codebuddy.ai/v2/billing/meter/get-user-resource` works for **API key and OAuth**. Worker every 5m + `POST /cb/credits/sync`. Permanent disable on `Status==3`. Fallback `CB_CREDIT_LIMIT=240` if never synced. See `references/codebuddy-credit-meter.md`.

## Ops
- Local often ahead of GHCR (post-v1.6.1: device OAuth + Test still local until next tag)
- Compose ports must be `127.0.0.1:PORT:PORT`
- Keep `xai-*.json` / `cpa_auths/` / `migration-backup/` out of image (`.dockerignore`)
- Deploy: `go test -race ./... && go vet ./...` then `docker compose up -d --build foxrouters`
