# Security Audit FoxRouters — 2026-08-15 (claude-opus-5 via deleg_8152cf75)

Mode: READ-ONLY static audit + verifikasi parent ke code actual. Repo `/root/nexus-workspace/foxrouters` branch dev (uncommitted tree, 18 files).

## Ringkasan

| Severity | Jumlah | Status |
|---|---|---|
| P0 | 0 | — |
| P1 | 4 | ✅ 3 fixed, 1 turun ke P2 (sink dashboard aman) |
| P2 | 8 | ✅ 6 fixed, 2 verified aman |

## Temuan P1

### A1 — `/ali/accounts` bocor API key penuh (FIXED)
- File: `internal/handlers/alibaba_handlers.go` L88-96
- Komentar mengklaim "never the full secret over the wire" tapi `key` asli cuma ditambah `key_masked`, tidak pernah dihapus.
- Fix: `ListAccounts()` hanya kirim `key_masked` + `key_hash` (SHA-256 24 hex). Dashboard + aksi (test/disable/enable/delete) resolve via hash server-side. Verified: response tidak ada field `key`.

### A2 — `DELETE /ali/accounts/:key` key penuh di URL path → gin access log (FIXED)
- File: `internal/handlers/alibaba_handlers.go` L101-110, route di `main.go`
- `gin.Default()` logger mencetak method+path ke stdout/log shipper → secret permanen di log.
- Fix: route diganti `POST /ali/accounts/delete` body `{key_hash}`. Verified: log dev 0 hit "sk-ws-".

### A3 — Nil-pointer panic / DoS di `AliModelUsageList` (FIXED)
- File: `internal/upstream/ali_quota.go` L206, 223
- `am.db.Redis()` tanpa nil-check (inkonsisten dengan `RecordUsageModel` yang punya guard).
- Fix: guard `am == nil || am.db == nil || rdb == nil`.

### A4 — Supply-chain → XSS source dari registry pihak ketiga (TURUN KE P2)
- File: `internal/upstream/model_registry.go` L32, 165-184 — Freebuff model list dari GitHub release pihak ketiga (`pingmike2/freebuff2api-wokers`), `m.ID` dipakai mentah jadi GatewayID tanpa validasi charset.
- **U1 verified**: sink dashboard AMAN — `escHtml` escape lengkap (`& < > " '`), `quotaCell` cuma interpolasi angka (formatNum), `renderModelsTable` pakai `escHtml(id)` + event delegation data-* (bukan inline onclick). Jadi bukan XSS executable.
- Fix (defense-in-depth): `validModelID` regex `^[A-Za-z0-9._/\-]{1,80}$` di Ali + FB registry, drop fail-closed.

## Temuan P2

| ID | Deskripsi | Status |
|---|---|---|
| B1 | `filterModelsByKey` fail-open saat key unknown | ✅ FIXED — fail-closed (empty list) |
| B2 | Data race `am.idx` (RR cursor) | ✅ FIXED — `atomic.AddUint64` |
| B3 | Redis keyspace pollution via model name | ✅ FIXED — sanitize `[a-z0-9._-]` |
| B4 | Dated-variant fallback `LastIndex("-20")` rapuh + lolos zero-quota | ✅ FIXED — regex `-\d{4}(-\d{2})?$` + zero-check |
| B5 | Per-key limit scaling fluktuasi (ActiveKeyCount vs cooldown) | ⚠️ PARTIAL — clamp Remaining ≥0; scaling 1M×keys per user request (bukan bug) |
| B6 | HGetAll error silent `continue` | ✅ FIXED — `slog.Warn` + clamp |
| B7 | `err.Error()` passthrough bocor detail upstream | ✅ FIXED — generic "upstream probe failed" |
| B8 | Telemetry pool di `/ali/models/usage` | ✅ Verified aman — sudah `adminAuth` |

## Verified aman (tidak perlu re-check)

- VC1: SSRF registry BERSIH — URL konstanta hardcoded, tidak ada input user
- VC2: API key di header (bukan query string) pada fetch registry
- VC3: Body-size cap 1MB pada semua fetch registry
- VC4: `/v1/models/:id` di-filter via `filterModelsByKey` — key restricted dapat 404
- VC5: Registry swap build-lokal-lalu-lock — pola benar, no shared append
- VC6: `LoadFromRedis` lock benar
- VC7: `ActiveKeyCount`/`QuotaSummary`/`ListAccounts` RLock + per-entry lock
- VC8: Worker shutdown via ctx.Done (no goroutine leak)
- VC9: Redis command injection tidak mungkin (go-redis RESP framed)
- VC10: SQL injection — Alibaba/registry tidak sentuh logstore
- VC11: Header injection — konstanta + Go net/http tolak CR/LF
- VC12: Logging key sudah termask (`keyPrefix` 12 char)
- VC13: csrfGuard coverage route lama BERSIH — semua mutating route punya `csrfGuard()+adminAuth`
- U2: route baru `/ali/*` + `/models/refresh` — semua mutating ada csrfGuard+adminAuth, GET ada adminAuth
- U3: `auth.Manager.Get` return `info.Snapshot()` by-value — no pointer race
- U4: `RecordUsageModel` hanya dipanggil saat HTTP 200 dari upstream — model arbitrer ditolak upstream dulu

## Catatan

- Tidak ada file yang dimodifikasi oleh subagent (READ-ONLY).
- Semua fix dilakukan parent setelah verifikasi temuan ke code actual.
- 18 files modified (12 tracked, 510 insertions), BELUM commit.

---

# Re-audit #2 — 2026-08-15 (claude-opus-5 via deleg_3f349570, 183s)

Verdict: **8/10 fix FIXED-OK, 2 FIXED-TAPI-ISSUE, + 5 temuan baru (1 P1, 3 P2, 1 P3)** — semua sudah difix parent.

## Verdict fix lama

| ID | Verdict | Keterangan |
|---|---|---|
| A1 | ✅ FIXED-OK | DTO cuma key_masked + key_hash (SHA-256 96-bit) |
| A2 | ✅ FIXED-OK | POST body, gak ada :key di path |
| A3 | ✅ FIXED-OK | Nil-guard lengkap |
| A4 | ✅ FIXED-OK | validModelID regex; 0 false-positive (qwen-vl-max, deepseek/deepseek-v4-flash, z-ai/glm-5.2, dated variant semua lolos) |
| B1 | ✅ FIXED-OK | fail-closed; dashboard cookie-session gak regresi |
| B2 | ⚠️ FIXED-TAPI-ISSUE → **FIXED** | read `am.idx` masih plain → `atomic.LoadUint64` |
| B3 | ✅ FIXED-OK | sanitize konsisten write+read |
| B4 | ✅ FIXED-OK | regex anchored + zero-check di 2 titik |
| B6 | ✅ FIXED-OK | Warn + clamp |
| B7 | ⚠️ FIXED-TAPI-ISSUE → **FIXED** | Test generic tapi import path masih `err.Error()` → di-generic-kan juga |

## Temuan baru (semua difix)

| ID | Sev | Temuan | Fix |
|---|---|---|---|
| P1-1 | 🔴 P1 | **TOCTOU panic di `Next()`** — `n := len(am.keys)` di RLock #1 dilepas, `am.keys[idx]` diakses di RLock #2. `RemoveAccount` (Lock) di antara → slice mengecil → index out of range → **crash seluruh gateway**. | Snapshot `keys := append([]*AlibabaKey(nil), am.keys...)` sekali di awal, loop dari snapshot |
| P2-1 | 🟠 P2 | `resolveAliKey` cabang non-hash return id verbatim → `HandleAliTest` jadi **oracle validasi key DashScope pihak ketiga** (kirim key curian → 200 vs 502) | resolve wajib lewat pool: `aliAM.Get(id)` → nil kalau gak ada → 404 |
| P2-2 | 🟠 P2 | `err.Error()` masih bocor di import ("detail") + bulk (errors array) | Generic message, detail server-side |
| P2-3 | 🟠 P2 | Bulk import unbounded — body 1MB ≈ 15k key → O(n²) sort + 15k Redis round-trip sambil hold `am.mu.Lock()` → **Next() ter-block** | Cap 500 (`aliBulkImportCap`), `truncated: true` |
| P3-1 | 🟡 P3 | Email dibuang di bulk import (`AddAccount(k, "")`) → dashboard blank | Parse email (field 0) + simpan |

## Verified live (dev :20131)

- test via hash → OK (masih jalan)
- test key acak `sk-ws-fake-...` → **404** (bukan oracle) ✅
- bulk 510 keys → `added 500, truncated: true` ✅
- email ke-save: 532/532 accounts punya email ✅
- `go test -race` ALL PASS · `go vet` clean ✅

## Belum diverifikasi (putaran berikutnya)

dashboard/core.js key_hash migration, main.go route table `/ali/*`, internal/auth session flow, proxy pool getClient scope, freebuff.go/grok_dpop.go/grok_images.go, SSE ctx.Done, grep global secret leak.
