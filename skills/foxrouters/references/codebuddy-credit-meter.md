# CodeBuddy realtime credit meter

Verified July 2026 (CLI RE + live curl with OAuth AT and `ck_*` API key).

## Endpoint (use this only)

```
POST https://www.codebuddy.ai/v2/billing/meter/get-user-resource
Authorization: Bearer <accessToken OR ck_*>
Content-Type: application/json
Body: {}
```

Works for **both** OAuth access tokens and API keys.

| Path | OAuth Bearer | API key Bearer / X-API-Key |
|------|--------------|----------------------------|
| `/v2/billing/meter/get-user-resource` | ✅ full meter | ✅ full meter |
| `/billing/meter/get-user-resource` (no v2) | ✅ full meter | ❌ **401** |
| `/v2/billing/meter/get-dosage-notify` | ✅ notify only | ✅ notify only |

CLI `BillingService` only calls `get-dosage-notify` and **skips** when `tokenType === "ApiKey"` — that is a CLI filter, not an API restriction. Full meter via `/v2/...` still works for API keys.

## Response fields (Accounts[0])

| Field | Meaning |
|-------|---------|
| `CapacitySize` / `CapacitySizePrecise` | total credits |
| `CapacityUsed` / `CapacityUsedPrecise` | used (prefer Precise float) |
| `CapacityRemain` / `CapacityRemainPrecise` | remaining |
| `CycleStartTime` / `CycleEndTime` | trial window |
| `PackageName` | plan name |
| `Status` | `0` = active, `3` = exhausted |

Example: used `0.08` after a short chat that billed `usage.credit: 0.08` in SSE.

## FoxRouters integration notes

- Prefer meter sync as source of truth; SSE `usage.credit` is interim until next sync.
- **Shipped** (`010ac96` local): `CBKey.SyncCredits()`, `CBCreditSyncWorker` every 5m + boot stagger, permanent disable on Status==3 or remain≤0.
- Gateway: `POST /cb/credits/sync` admin — body `{}` (all) or `{email|key}` (one). Persist limit/remain/package/cycle/meter_status/synced_at. Fallback `CB_CREDIT_LIMIT=240` if never synced.
- Dashboard: **Sync credits** button; show `credits_remain` / real `credit_limit` from meter.
- Auth for meter: same as chat — OAuth `Bearer <AT>`; API key `Bearer ck_*` or `X-API-Key`. Always use **`/v2/...`** path.

## Related

- OAuth dual pool: `references/codebuddy-oauth-import.md`
- Upstream RE: `ai-api-trial-farming/references/codebuddy-api-reference.md`
