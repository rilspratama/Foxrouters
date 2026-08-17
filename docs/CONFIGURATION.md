# Configuration

## Configuration

All configuration is read from environment variables (or a `.env` file loaded at
startup).

| Variable | Default | Description |
|---|---|---|
| `PORT` | `20130` | HTTP listen port. |
| `REDIS_ADDR` | `127.0.0.1:6379` | Redis host:port for hot state. |
| `REDIS_PASSWORD` | *(empty)* | Redis password if AUTH is enabled. |
| `REDIS_DB` | `0` | Redis logical DB index. |
| `GATEWAY_KEY_FILE` | `./gateway-key.txt` | Path to the seed admin bearer token file. |
| `CB_KEY_FILE` | `./cb-keys.json` | Path to a JSON file of CodeBuddy keys to seed. |
| `CPA_AUTH_DIR` | `./` | Directory scanned for `xai-*.json` Grok credential files at boot. |
| `GATEWAY_AUTH_DISABLE` | `false` | **Dev only.** When `true`, bypasses auth on all routes. Never enable in production. |
| `FREEBUFF_DISABLED` | `false` | When `true`, disables the Freebuff provider entirely. |
| `FREEBUFF_BASE_URL` | `https://www.codebuff.com` | Relay base URL for session/chat/ads/streak/run calls. Set to a Railway/CF Worker relay URL for full-access tier (exit IP not VPN-flagged). Device flow (OAuth login) always hits codebuff.com directly. **Runtime-overridable**: `GET/PUT /fb/config` (dashboard Freebuff tab) persists to Redis (`fb:config`) and wins over this env default after boot. |
| `LOG_BODY_CAP_BYTES` | `1048576` | Max bytes for a single stored request/response body in SQLite history. Default 1 MiB keeps full bodies for normal traffic while bounding DB growth (multi-MB rows from huge-context requests previously bloated logs.db to >20GB and caused `/history/recent` timeouts). |
| `COOKIE_SECURE` | `1` | Session cookie `Secure` flag. Set to `0` for dev HTTP (localhost). Default `1` = HTTPS-only. |

> **Do not** commit secrets. Put the `.env` outside the repo or use `chmod 600
> .gateway.env` alongside `.gitignore`.

