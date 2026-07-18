# FoxRouters — Quick Model Reference

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

## CodeBuddy Models (upstream: www.codebuddy.ai/v2)

39 models total. Common ones:

| Model ID | Family |
|----------|--------|
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
