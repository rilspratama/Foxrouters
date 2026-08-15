# Credentials

Bootstrapping accounts / keys:

```bash
# Import a Grok account credential file (admin only)
curl -X POST http://127.0.0.1:20130/accounts/import \
     -H "Authorization: Bearer $ADMIN_KEY" \
     -H "Content-Type: application/json" \
     --data @path/to/grok-account.json

# Import a CodeBuddy API key
curl -X POST http://127.0.0.1:20130/cb/import \
     -H "Authorization: Bearer $ADMIN_KEY" \
     -H "Content-Type: application/json" \
     -d '{"api_key":"ck_YOUR_CB_KEY_HERE"}'

# Import a CodeBuddy OAuth credential (dual pool — same chat path as API keys)
curl -X POST http://127.0.0.1:20130/cb/oauth/import \
     -H "Authorization: Bearer $ADMIN_KEY" \
     -H "Content-Type: application/json" \
     -d '{
       "email": "user@example.com",
       "access_token": "eyJ...",
       "refresh_token": "eyJ...",
       "expires_in": 31535929
     }'
# If AT is expired/near-expiry and RT is valid, gateway eagerly refreshes before pool entry.

# Bulk OAuth import (idempotent by email)
curl -X POST http://127.0.0.1:20130/cb/oauth/import/bulk \
     -H "Authorization: Bearer $ADMIN_KEY" \
     -H "Content-Type: application/json" \
     -d '{"accounts":[
       {"email":"u1@example.com","access_token":"eyJ...","refresh_token":"eyJ...","expires_in":31535929},
       {"email":"u2@example.com","access_token":"eyJ...","refresh_token":"eyJ..."}
     ]}'

# Sync credit meters (all keys, or one by email/key)
curl -X POST http://127.0.0.1:20130/cb/credits/sync \
     -H "Authorization: Bearer $ADMIN_KEY" \
     -H "Content-Type: application/json" \
     -d '{}'
# one: -d '{"email":"user@example.com"}'  or  -d '{"key":"ck_..."}'

# OAuth login URL (device flow — no tokens to paste)
curl -s -X POST http://127.0.0.1:20130/cb/oauth/device/start \
     -H "Authorization: Bearer $ADMIN_KEY" -H "Content-Type: application/json" -d '{}'
# → {state, auth_url} — open auth_url in browser, then poll:
curl -s "http://127.0.0.1:20130/cb/oauth/device/poll?state=$STATE" \
     -H "Authorization: Bearer $ADMIN_KEY"
# ready → import tokens via /cb/oauth/import

# Test a credential (direct upstream probe)
curl -s -X POST http://127.0.0.1:20130/cb/keys/test \
     -H "Authorization: Bearer $ADMIN_KEY" -H "Content-Type: application/json" \
     -d '{"email":"user@example.com"}'
curl -s -X POST http://127.0.0.1:20130/accounts/test \
     -H "Authorization: Bearer $ADMIN_KEY" -H "Content-Type: application/json" \
     -d '{"email":"acct@x.ai"}'
```

### CodeBuddy credentials (v1.6.1+)

| Mode | Pool field | Upstream auth | Refresh |
|------|------------|---------------|---------|
| API key | `cred_type=api_key` | Bearer `ck_*` or `X-API-Key` | none |
| OAuth | `cred_type=oauth` | `Authorization: Bearer <AT>` only | `POST /v2/plugin/auth/token/refresh` |

Both modes share `www.codebuddy.ai/v2/chat/completions` and mixed RR. Credits come from
`POST /v2/billing/meter/get-user-resource` (worker every 5m + manual `/cb/credits/sync`).
Dashboard: Type badge, Expires, Add OAuth Manual|Login URL, Bulk OAuth, Sync credits, per-row Test.

### Freebuff credentials (v1.6.4+)

| Method | Endpoint | Input |
|--------|----------|-------|
| Single token | `POST /fb/import` | `{"token":"UUID","email":"...","user_id":"..."}` |
| Bulk (pipe format) | `POST /fb/import/bulk` | `{"raw":"token1\|email1\|userid1\ntoken2\|email2\|userid2"}` |
| OAuth device flow | `POST /fb/oauth/device/start` | generates login URL → `GET /fb/oauth/device/poll` auto-imports |
| Quota sync | `POST /fb/quota/sync` | `{"token":"optional"}` (empty = all) |
| Model refresh | `POST /models/refresh` | Manual dynamic model-registry refresh (Freebuff + Grok; per-upstream source/count/error) |

Pipe format: `token|email|userid` — token required, email+userid optional, bare UUID also works.
Buffy prefix auto-injected. Quota auto-synced every 5min. Auto-cooldown when exhausted.

### Alibaba credentials (v1.6.14+)

| Method | Endpoint | Input |
|--------|----------|-------|
| Single key | `POST /ali/import` | `{"key":"sk-ws-…","email":"…"}` |
| Bulk (api_keys.txt format) | `POST /ali/import/bulk` | `{"raw":"email \| password \| sk-ws-… \| ts\n…"}` (cap 500/batch) |
| Test | `POST /ali/keys/test` | `{"key_hash":"…"}` (or full key, server-side) |
| Disable / Enable | `POST /ali/keys/disable` / `enable` | `{"key_hash":"…","reason":"…"}` |
| Delete (last resort — prefer disable) | `POST /ali/accounts/delete` | `{"key_hash":"…"}` |
| Quota view | `GET /ali/models/usage` | admin — per-model used/limit table |

**Security:** full `sk-ws-*` secrets NEVER leave the server — `/ali/accounts` returns
only `key_masked` + opaque `key_hash` (SHA-256 24 hex). All key actions resolve
hash→key server-side; unknown ids return 404 (no credential-validation oracle).

Freebuff + Grok + Alibaba model lists auto-refresh every 6h from upstream sources (static fallback on failure).

