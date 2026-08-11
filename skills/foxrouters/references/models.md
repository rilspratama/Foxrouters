# FoxRouters — Quick Model Reference

> **Dynamic model registry** (v1.6.4+): Freebuff + Grok model lists refresh
> from upstream sources every 6h (`FbModelsWorker` / `GrokModelsWorker`),
> so new models appear without code changes. `/v1/models` serves the dynamic
> list with static fallback. Manual refresh: `POST /models/refresh` (admin).
> CodeBuddy has no models endpoint — its list is static.

## Grok Models (upstream: cli-chat-proxy.grok.com)

| Model ID | Notes |
|----------|-------|
| `grok-4.5` | Base model |
| `grok-4.5-high` | High reasoning effort |
| `grok-4.5-medium` | Medium reasoning |
| `grok-4.5-low` | Low reasoning |
| `grok-4.5-xhigh` | Extra-high reasoning (slowest, most thorough) |
| `grok-4.5-auto` | Auto reasoning level |
| `grok-4.5-none` | No reasoning (fastest) |

**Alias mechanism:** `grok-4.5-{level}` → `grok-4.5` + `reasoning_effort` param. Client-set `reasoning_effort` wins.
**Dynamic:** when the registry has been refreshed, aliases are generated from
the upstream `reasoning_efforts[]` list (`auto`/`none` stay gateway-internal).

## Freebuff Models (upstream: www.codebuff.com — Codebuff rebrand)

**Dynamic** — fetched from `freebuff-models.json` (freebuff2api project releases,
mirrors `CodebuffAI/freebuff` official source). Static fallback:

| Model ID | Pool | Notes |
|----------|------|-------|
| `fb/deepseek-v4-flash` | standard | Limited mode (default) |
| `fb/mimo-v2.5` | standard | Limited mode |
| `fb/deepseek-v4-pro` | premium | Full mode only (US/EU residential IP) |
| `fb/minimax-m3` | premium | Full mode only |
| `fb/gpt-5.6-luna` | premium | Full mode only |
| `fb/glm-5.2` | glm (referral) | Referral-unlock only |

Newer models appear automatically after a registry refresh, e.g.
`fb/laguna-s-2.1`, `fb/kimi-k3-eco`, `fb/claude-fable-5`,
`fb/muse-spark-1.2-contributor`. Premium/glm pool membership → `FullMode`
(full-access accounts only) — limited-tier accounts are skipped for those.

## CodeBuddy Models (upstream: www.codebuddy.ai/v2)

42 models total. Common ones:

| Model ID | Family |
|----------|--------|
| `cb/gpt-5.6-sol` | OpenAI GPT-5.6 (newest) |
| `cb/gpt-5.6-terra` | OpenAI GPT-5.6 (newest) |
| `cb/gpt-5.6-luna` | OpenAI GPT-5.6 (newest) |
| `cb/gpt-5.5` | OpenAI GPT |
| `cb/gpt-5.2` | OpenAI GPT |
| `cb/claude-opus-4.7` | Anthropic Claude |
| `cb/claude-sonnet-4` | Anthropic Claude |
| `cb/gemini-3.1-pro` | Google Gemini |
| `cb/gemini-3.1-flash` | Google Gemini (fast) |
| `cb/deepseek-v3` | DeepSeek |
| `cb/llama-4-405b` | Meta Llama |
| `cb/qwen-3-235b` | Alibaba Qwen |

## Model Selection Guide

| Use case | Recommended model |
|----------|-------------------|
| Fast chat, cheap | `cb/gpt-5.5`, `cb/gemini-3.1-flash` |
| Code generation | `cb/claude-sonnet-4`, `grok-4.5` |
| Complex reasoning | `grok-4.5-high`, `grok-4.5-xhigh` |
| Long context | `cb/gemini-3.1-pro` (1M context) |
| Creative writing | `cb/claude-opus-4.7` |
| Math/coding hard problems | `grok-4.5-xhigh` |

## List all available models

```bash
KEY=$(head -1 /root/nexus-workspace/foxrouters/gateway-key.txt)
curl -s http://127.0.0.1:20130/v1/models \
  -H "Authorization: Bearer $KEY" | python3 -m json.tool
```
