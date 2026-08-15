# FoxRouters

[![Go Version](https://img.shields.io/badge/go-1.25.12%2B-00ADD8?logo=go)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](#license)
[![Version](https://img.shields.io/badge/version-v1.6.14-blue)](./CHANGELOG.md)
[![Security](https://img.shields.io/badge/security-audited%203x-brightgreen)](./CHANGELOG.md)
[![Tests](https://img.shields.io/badge/tests-62%2F62%20PASS%20(%2Brace)-success)](./)

Unified **OpenAI-compatible** API gateway that fronts **Grok**, **CodeBuddy**,
**Freebuff**, and **Alibaba DashScope** behind one `/v1/chat/completions`
endpoint. Route by model prefix, round-robin across many upstream
accounts/keys, refresh tokens automatically, enforce per-key quotas, and log
every request/response to SQLite — all behind a single Bearer token.

Also exposes the **Anthropic Messages API** (`/v1/messages`) so the assistant
CLI can use it as a backend proxy.

---

## Quick Start (One-Liner)

```bash
curl -fsSL https://raw.githubusercontent.com/rilspratama/Foxrouters/master/install.sh | bash
```

Auto-installs Docker if missing, starts Redis + FoxRouters, prints the gateway
key. Open `http://<host-ip>:20130/login`, paste the key, done.

---

## Features

- **4 upstreams, one endpoint** — model-prefix routing: `grok-*`, `cb/*`, `fb/*`, `ali/*`.
- **Multi-account / multi-key pools** — O(k) round-robin, auto token refresh
  (singleflight + pre-warm), circuit-breaker disable on auth/credit errors.
- **Selector modes** — `rr` | `sticky` | `content-hash` | `hybrid` (sticky session
  pinning for prompt-cache locality, ~3x cheaper cached tokens).
- **Per-gateway-key limits** — RPM, burst, token quota, model whitelist, role
  (`admin` vs `inference`).
- **Redis hot state + SQLite cold history** — full request/response bodies, 90-day TTL.
- **Dynamic model registry** — Freebuff + Grok model lists refresh from upstream
  every 6h; new models appear without code changes.
- **Embedded web dashboard** — stats, accounts & keys, gateway keys, models,
  proxies, tunnel.
- **Security hardened** (3x audited) — XSS-safe event delegation, CSRF guard,
  Secure/HttpOnly cookies, per-key key masking, fail-closed model visibility.

---

## Documentation

| Doc | Contents |
|---|---|
| [Installation](docs/INSTALL.md) | One-liner, Docker Compose, manual build, `.env` table |
| [CLI & Native Binary](docs/CLI.md) | GitHub release binaries, checksums, `install/update/config` subcommands |
| [Credentials](docs/CREDENTIALS.md) | Import Grok / CodeBuddy (API key + OAuth) / Freebuff / Alibaba |
| [Configuration](docs/CONFIGURATION.md) | All environment variables |
| [API Reference](docs/API.md) | Endpoints, auth, examples (chat, Anthropic, custom models, combos) |
| [Dashboard](docs/DASHBOARD.md) | SPA routes and features |
| [Architecture](docs/ARCHITECTURE.md) | Request flow, storage split, invariants |
| [Development](docs/DEVELOPMENT.md) | Isolated dev stack, local build, project layout |

**Other docs:** [Hermes integration](docs/HERMES.md) · [OpenCode via gateway](docs/OPENCODE.md)

---

## Examples

```bash
# Chat completion → Grok (alias expands to grok-4.5 + reasoning_effort=high)
curl -s http://127.0.0.1:20130/v1/chat/completions \
  -H "Authorization: Bearer $GATEWAY_KEY" -H "Content-Type: application/json" \
  -d '{"model":"grok-4.5-high","stream":true,"messages":[{"role":"user","content":"hello"}]}'

# Anthropic Messages API (the assistant CLI backend)
curl -s http://127.0.0.1:20130/v1/messages \
  -H "x-api-key: $GATEWAY_KEY" -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet-4","max_tokens":100,"messages":[{"role":"user","content":"Hello"}]}'
```

More in [API Reference](docs/API.md).

---

## License

Released under the **MIT License**. See [`LICENSE`](./LICENSE) for the full text.
