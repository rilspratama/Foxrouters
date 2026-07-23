# CodeBuddy OAuth import (operator quick ref)

See full RE + implementation: skill `ai-gateway-development` → `references/codebuddy-oauth-dual-pool.md`.

## Endpoints
- `POST /cb/import` — API key `ck_*`
- `POST /cb/import/bulk` — API keys bulk only
- `POST /cb/oauth/import` — OAuth single (`{email, access_token, refresh_token, expires_in?}`)
- `POST /cb/oauth/import/bulk` — OAuth bulk (`{accounts:[{email, access_token, refresh_token, expires_in?}, ...]}`) — idempotent by email
- `POST /cb/credits/sync` — meter sync one/all (`{}` or `{email|key}`)
- `GET /cb-stats` — `cred_type`, `email`, `expires_at`, `credits_remain`, package, meter_*

## Eager refresh on import
If the supplied access token is expired or near-expiry and the refresh token is valid, the gateway calls `Refresh()` via `POST /v2/plugin/auth/token/refresh` **before** the credential enters the pool. Fresh AT is stored; failed eager refresh still stores as-is (logged as warning).

## Upstream auth (verified)
| Type | Header | Refresh |
|------|--------|---------|
| api_key | `Authorization: Bearer ck_*` or `X-API-Key` | none |
| oauth | `Authorization: Bearer <AT>` only (`X-API-Key` → 401) | `POST /v2/plugin/auth/token/refresh` + `X-Refresh-Token` (cli\|plugin); no `/plugin` → 404 |

Gateway auto-refresh: pre-warm 30s/30m + EnsureValid + 401 retry. TTL often ~365d. Dual pool: `api_key` + `oauth` mixed RR on same chat endpoint.

## Dashboard
- Type badge (OAuth purple / API Key blue), Expires column, meter remain
- Buttons: `+ Add Key`, `+ Add OAuth` (single modal), **Bulk OAuth** modal, Bulk Import, **Sync credits**, Cleanup Disabled
- `dashboard.html` is `//go:embed` — rebuild Docker after UI edits
- Bare curl `/dashboard` without login cookie can look empty

## Credits
Realtime meter: `POST www.codebuddy.ai/v2/billing/meter/get-user-resource` works for **API key and OAuth**. Worker every 5m + `POST /cb/credits/sync`. Permanent disable on `Status==3`. Fallback `CB_CREDIT_LIMIT=240` if never synced. See `references/codebuddy-credit-meter.md`.

## Ops
- Local often ahead of GHCR (e.g. v1.6.1-oauth)
- Compose ports must be `127.0.0.1:PORT:PORT`
- Keep `xai-*.json` / `cpa_auths/` in `.dockerignore`
- Deploy: `go test -race ./... && go vet ./...` then `docker compose up -d --build foxrouters`
