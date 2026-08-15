# Architecture

## Architecture

```
Client
  │  Bearer <gateway-key>
  ▼
AuthMiddleware           ── validate key, load role + limits
  ▼
RateLimitMiddleware      ── RPM / burst / token quota (Redis)
  ▼
proxyRequest             ── inspect "model", expand aliases
  ├── grok-*  → proxyGrok         (O(k) RR, refresh, 401 retry, 403 ban)
  └── cb/*    → proxyCodeBuddy    (dual pool api_key+oauth, meter credits, stream transform)
  ▼
async LogRequest → SQLite (full request + response, TTL 90d)
```

**Storage split**

| Layer | Engine | Contents |
|---|---|---|
| Hot | **Redis** | Tokens, CB credits, disabled flags, gateway keys, rate/quota counters. |
| Cold | **SQLite** | `request_logs` (full bodies), refresh events, disable events. |

**Hot-path invariants**

1. `Next()` is O(k) round-robin — re-enable/refresh happens in background workers.
2. Counts come from `Len()`, never `len(GetAll())`.
3. Refresh uses singleflight and never holds `acc.mu` across a network call.
4. Any disable/enable/token mutation calls `Save*()` **after** the lock is
   released.
5. History writes are async; credentials never land in the log store.

---

