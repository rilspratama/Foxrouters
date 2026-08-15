# Dashboard

## Dashboard

Served at `GET /dashboard` (public HTML; XHRs still require a gateway key via
session cookie from `/login`). The SPA has five+ nav routes:

| Route | Page |
|---|---|
| `#/` | **Dashboard** — health, request counts, token totals, recent history preview. |
| `#/accounts` | **Accounts & Keys** — Grok + CodeBuddy. CB: Type badge, Expires, meter remain; buttons `+ Add Key`, `+ Add OAuth` (Manual\|Login URL), `Bulk OAuth`, `Bulk Import`, `Sync credits`, `Cleanup Disabled`. Per-row **Test** + Delete (Grok & CB). |
| `#/keys` | **Gateway API Keys** — key CRUD, role picker, allowed-models selector, RPM/burst/quota inputs. |
| `#/models` | **Models** — 3 tabs: **Models** (usage stats) \| **Custom** (custom models + aliases) \| **Combos** (group models under virtual alias). |
| `#/proxies` | **Proxies** — HTTP/SOCKS5 pool with upstream scoping. |
| `#/tunnel` | **Tunnel** — Cloudflare Tunnel enable/disable + config. |

Live gateway keys are **never** rendered into the HTML server-side. Delete buttons
use `data-*` attributes + event delegation (XSS-safe, no inline `onclick`).

