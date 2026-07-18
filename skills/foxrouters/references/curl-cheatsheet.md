# FoxRouters — curl Cheatsheet

## Setup
```bash
GW=http://127.0.0.1:20130
KEY=$(head -1 /root/nexus-workspace/foxrouters/gateway-key.txt)
AUTH="Authorization: Bearer $KEY"
```

## Fresh Deploy / Wiped Redis (v5.11.6+)

On first boot with empty Redis, the gateway auto-generates a bootstrap admin key:
```bash
# Get the auto-generated bootstrap key (written once on first boot)
KEY=$(cat /root/nexus-workspace/foxrouters/bootstrap-key.txt)
# Or grep from startup log:
journalctl -u foxrouters --since "5 min ago" | grep "Key: gw-" | head -1

# Verify it works (should return admin role)
curl -s $GW/api/keys -H "Authorization: Bearer $KEY" | python3 -m json.tool

# After first login, delete the file (Redis now owns the key)
rm /root/nexus-workspace/foxrouters/bootstrap-key.txt
```

To disable auto-bootstrap (prod hardening): set `GATEWAY_NO_AUTOBOOTSTRAP=1` in `.gateway.env`.

## Inference

```bash
# Chat completion
curl -s $GW/v1/chat/completions -H "$AUTH" -H "Content-Type: application/json" -d '{
  "model": "grok-4.5",
  "messages": [{"role":"user","content":"Hello"}]
}'

# Streaming
curl -N $GW/v1/chat/completions -H "$AUTH" -d '{
  "model":"cb/gpt-5.5",
  "messages":[{"role":"user","content":"Write a poem"}],
  "stream": true
}'

# List models
curl -s $GW/v1/models -H "$AUTH"
```

## Gateway Keys

```bash
# List
curl -s $GW/api/keys -H "$AUTH"

# Create inference key (model-restricted)
curl -X POST $GW/api/keys -H "$AUTH" -d '{
  "name":"app",
  "role":"inference",
  "allowed_models":["grok-*"],
  "rpm":60
}'

# Create admin key
curl -X POST $GW/api/keys -H "$AUTH" -d '{"name":"ops","role":"admin"}'

# Update key
curl -X PUT $GW/api/keys/gw-xxx -H "$AUTH" -d '{"rpm":120}'

# Delete key
curl -X DELETE $GW/api/keys/gw-xxx -H "$AUTH"

# Key usage
curl -s $GW/api/keys/gw-xxx/usage -H "$AUTH"
```

## Grok Accounts

```bash
# List
curl -s $GW/accounts -H "$AUTH"

# Add single
curl -X POST $GW/accounts/import -H "$AUTH" -d '{
  "email":"u@x.com",
  "access_token":"eyJ...",
  "refresh_token":"..."
}'

# Bulk add
curl -X POST $GW/accounts/import/bulk -H "$AUTH" -d '{"accounts":[
  {"email":"a@x.com","access_token":"...","refresh_token":"..."},
  {"email":"b@x.com","access_token":"...","refresh_token":"..."}
]}'

# Refresh all tokens
curl -X POST $GW/accounts/refresh -H "$AUTH"

# Delete account
curl -X DELETE $GW/accounts/user@x.com -H "$AUTH"
```

## CodeBuddy Keys

```bash
# Add single
curl -X POST $GW/cb/import -H "$AUTH" -d '{"api_key":"ck_..."}'

# Bulk add (array)
curl -X POST $GW/cb/import/bulk -H "$AUTH" -d '{"api_keys":["ck_1","ck_2","ck_3"]}'

# Bulk add (raw paste)
curl -X POST $GW/cb/import/bulk -H "$AUTH" -d '{"raw":"ck_1\nck_2,ck_3"}'

# CB stats (credits, usage per key)
curl -s $GW/cb-stats -H "$AUTH"
```

## Health & History

```bash
# Health (public minimal)
curl -s $GW/health

# Health (authed = full telemetry)
curl -s $GW/health -H "$AUTH" | python3 -m json.tool

# HEAD /health (liveness)
curl -sI $GW/health | head -1

# Check X-Request-ID (every response has one)
curl -sI $GW/health | grep -i x-request

# History stats
curl -s "$GW/history?hours=24" -H "$AUTH"

# Recent requests
curl -s "$GW/history/recent?limit=20" -H "$AUTH"

# Request detail (full JSON)
curl -s "$GW/history/detail/12345" -H "$AUTH"
```

## Dashboard Login (cookie auth, v5.11.6+)

```bash
# Login page (GET)
curl -s $GW/login | head -5

# Login with key → sets HttpOnly cookie + redirects
curl -si -X POST $GW/login -d "key=$KEY" | grep -iE "set-cookie|location"

# Access dashboard with cookie (no Bearer needed)
curl -s $GW/dashboard -H "Cookie: foxrouters_session=$KEY" -o /dev/null -w "%{http_code}\n"

# API also works with cookie (browser-style)
curl -s $GW/api/keys -H "Cookie: foxrouters_session=$KEY"

# Logout → clears cookie + redirects to /login
curl -si $GW/logout | grep -iE "set-cookie|location"
```

## Service Management

```bash
systemctl status foxrouters
systemctl restart foxrouters
systemctl stop foxrouters
systemctl start foxrouters
journalctl -u foxrouters -f --since "5 min ago"
journalctl -u foxrouters --since "1 hour ago" --no-pager | grep -E '\[cb\]|\[auth\]|\[grok\]|\[server\]'
```

## Python (requests)

```python
import requests

GW = "http://127.0.0.1:20130"
KEY = open("/root/nexus-workspace/foxrouters/gateway-key.txt").readline().strip()
H = {"Authorization": f"Bearer {KEY}"}

# Chat
r = requests.post(f"{GW}/v1/chat/completions", headers=H, json={
    "model": "grok-4.5",
    "messages": [{"role": "user", "content": "Hello"}]
}, timeout=300)
print(r.json()["choices"][0]["message"]["content"])

# List models
models = requests.get(f"{GW}/v1/models", headers=H).json()["data"]
print([m["id"] for m in models])
```
