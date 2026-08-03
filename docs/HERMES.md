# Hermes Agent Integration — Reasoning Content Display Fix

> **Applies to:** Hermes Agent ≤ current version (Aug 2026) connecting to FoxRouters
> via custom provider (`base_url: http://localhost:<port>/v1`)
> **Components affected:** Hermes `agent/turn_finalizer.py`, `gateway/run.py`,
> `run_agent.py`
> **FoxRouters side:** No changes needed — SSE passthrough already forwards
> `delta.reasoning_content` correctly.

---

## Background

### Reasoning field conventions across the ecosystem

| Provider / Platform | Response Field | Streaming Delta Field | Notes |
|---|---|---|---|
| **DeepSeek** (R1, V3) | `message.reasoning_content` | `delta.reasoning_content` | Pioneer; defines the convention |
| **Qwen / DashScope** | `message.reasoning_content` | `delta.reasoning_content` | Follows DeepSeek |
| **GLM / Zhipu** | `message.reasoning_content` | `delta.reasoning_content` | Follows DeepSeek |
| **Kimi / Moonshot** | `message.reasoning_content` | `delta.reasoning_content` | Follows DeepSeek |
| **vLLM** | `message.reasoning_content` | `delta.reasoning_content` | Adopted DeepSeek convention |
| **SGLang** | `message.reasoning_content` | `delta.reasoning_content` | Adopted DeepSeek convention |
| **TGI (HuggingFace)** | `message.reasoning_content` | `delta.reasoning_content` | Adopted DeepSeek convention |
| **CodeBuddy (Tencent)** | `message.reasoning_content` | `delta.reasoning_content` | Follows DeepSeek |
| **OpenRouter** | `message.reasoning` + `reasoning_details` | `delta.reasoning` | Unified wrapper (different) |
| **Anthropic** | `thinking` blocks in content array | `thinking` content blocks | Native, different format |
| **OpenAI** (o1/o3) | Not exposed | Not exposed | Only `reasoning_effort` request param |

**`reasoning_content` is the de facto standard** for OpenAI-compatible APIs —
all Chinese providers (DeepSeek, Qwen, GLM, Kimi) and all major open-source
serving engines (vLLM, SGLang, TGI) use it. `reasoning` is OpenRouter's
unified wrapper name only.

FoxRouters proxies CodeBuddy + Grok upstreams. CodeBuddy returns
`reasoning_content`; Grok returns `reasoning_content` in streaming deltas.
Both are forwarded as-is via SSE passthrough.

---

## FoxRouters: No Changes Needed

FoxRouters already handles reasoning correctly:

### Streaming path (`codebuddy.go`, `grok.go`)

```go
// sseChunk struct — shared between Grok + CodeBuddy stream paths
type sseChunk struct {
    Error   any `json:"error"`
    Choices []struct {
        Delta struct {
            Content          string `json:"content"`
            ReasoningContent string `json:"reasoning_content"`
        } `json:"delta"`
    } `json:"choices"`
    Usage map[string]any `json:"usage"`
}
```

- **SSE passthrough**: raw `data:` lines are forwarded to the client verbatim,
  including `delta.reasoning_content` chunks. No transformation, no stripping.
- **History accumulation**: `streamContent` + `streamReasoning` are accumulated
  separately and stored in `response_body.message`:
  ```go
  msg := gin.H{"role": "assistant", "content": streamContent.String()}
  if r := streamReasoning.String(); r != "" {
      msg["reasoning_content"] = r
  }
  ```
- **Non-streaming path** (`cbCollectStream`): also accumulates
  `reasoning_content` and attaches it to the response message.

### Verification

Send a streaming request with `reasoning_effort: high` and check the raw SSE:

```bash
curl -N http://127.0.0.1:20130/v1/chat/completions \
  -H "Authorization: Bearer <key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "glm-5.2",
    "messages": [{"role":"user","content":"What is 8*7?"}],
    "reasoning_effort": "high",
    "stream": true
  }' 2>/dev/null | grep '"reasoning_content"'
```

Each SSE chunk with reasoning will contain `"reasoning_content":"..."` in the
delta — this is what the client (Hermes) receives.

---

## Hermes: The Bug

On streaming platforms (Telegram, Discord, etc.), reasoning was **never
displayed** even when `display.platforms.<platform>.show_reasoning: true`
was set in `config.yaml`.

### Symptom

- Gateway history shows `reasoning_content` in `response_body` ✅
- Raw SSE from gateway contains `delta.reasoning_content` ✅
- Hermes `show_reasoning: true` in config ✅
- But reasoning **never appears** in the Telegram/Discord message ❌

### Root Cause: Two bugs

#### Bug 1 — `turn_finalizer.py` only checked `msg["reasoning"]`

```python
# agent/turn_finalizer.py line 569 (BEFORE fix)
if msg.get("role") == "assistant" and msg.get("reasoning"):
    last_reasoning = msg["reasoning"]
```

`build_assistant_message()` stores reasoning in TWO fields:
- `msg["reasoning"]` — unified field (result of `_extract_reasoning()`, which
  checks `reasoning` + `reasoning_content` + `reasoning_details`)
- `msg["reasoning_content"]` — raw `reasoning_content` from the provider

The extraction chain (`_extract_reasoning`) does check `reasoning_content`,
so `msg["reasoning"]` should normally contain the data. But as
defense-in-depth, `turn_finalizer` should also fall back to
`msg["reasoning_content"]`.

#### Bug 2 — `gateway/run.py` reasoning prepend + `already_sent=True`

When streaming is enabled (default on Telegram), the response text is
streamed live to the user, setting `already_sent = True`. The reasoning
prepend modified the `response` string:

```python
# BEFORE fix: reasoning prepended to `response` string
response = f"💭 **Reasoning:**\n```\n{display_reasoning}\n```\n\n{response}"
```

But when `already_sent=True`, the `response` string is **never re-sent** —
only `MEDIA:` files are extracted from it, then `return None`:

```python
if agent_result.get("already_sent") and not agent_result.get("failed"):
    # Only extracts MEDIA: files from response, then returns None
    # The prepended reasoning is LOST!
    return None
```

Compare with the runtime footer, which has an explicit
`not already_sent` check and sends itself as a **separate trailing message**
when streaming already delivered the body. Reasoning had no such fallback.

---

## Hermes: The Fix (3 patches)

### Patch 1 — `agent/turn_finalizer.py` (field fallback)

**File:** `/usr/local/lib/hermes-agent/agent/turn_finalizer.py`

```diff
-        if msg.get("role") == "assistant" and msg.get("reasoning"):
-            last_reasoning = msg["reasoning"]
+        if msg.get("role") == "assistant" and (msg.get("reasoning") or msg.get("reasoning_content")):
+            last_reasoning = msg.get("reasoning") or msg.get("reasoning_content")
             break
```

### Patch 2 — `gateway/run.py` (build reasoning as separate string)

**File:** `/usr/local/lib/hermes-agent/gateway/run.py`

Refactor the reasoning prepend block to build `_reasoning_display` as a
standalone string instead of prepending to `response`:

```diff
+            _reasoning_display = None  # rendered reasoning block (without body)
             if _show_reasoning_effective and response and not _intentional_silence:
                 last_reasoning = agent_result.get("last_reasoning")
                 if last_reasoning:
                     # ... build display_reasoning ...
                     if _reasoning_style == "subtext":
-                        response = f"-# 💭 Reasoning\n{_quoted}\n\n{response}"
+                        _reasoning_display = f"-# 💭 Reasoning\n{_quoted}"
                     elif _reasoning_style == "blockquote":
-                        response = f"> 💭 **Reasoning:**\n{_quoted}\n\n{response}"
+                        _reasoning_display = f"> 💭 **Reasoning:**\n{_quoted}"
                     else:
-                        response = f"💭 **Reasoning:**\n```\n{display_reasoning}\n```\n\n{response}"
+                        _reasoning_display = f"💭 **Reasoning:**\n```\n{display_reasoning}\n```"
+                    # Non-streaming: prepend reasoning to response body (single message)
+                    if not agent_result.get("already_sent"):
+                        response = f"{_reasoning_display}\n\n{response}"
```

### Patch 3 — `gateway/run.py` (trailing message delivery)

In the `if agent_result.get("already_sent")` block, send reasoning as a
separate trailing message (same pattern as the footer):

```diff
             if agent_result.get("already_sent") and not agent_result.get("failed"):
                 if response:
                     # ... media extraction ...
+                # Streaming already delivered the body text, but the reasoning
+                # block was held back. Send it now as a leading message.
+                if _reasoning_display:
+                    try:
+                        _reasoning_adapter = self._adapter_for_source(source)
+                        if _reasoning_adapter:
+                            await _reasoning_adapter.send(
+                                source.chat_id,
+                                _reasoning_display,
+                                metadata=self._thread_metadata_for_source(source, self._reply_anchor_for_event(event)),
+                            )
+                    except Exception as _e:
+                        logger.debug("trailing reasoning send failed: %s", _e)
                 # ... footer trailing message ...
                 return None
```

### Patch 4 — `run_agent.py` (localhost reasoning forwarding, prior session)

**File:** `/usr/local/lib/hermes-agent/run_agent.py`

`_supports_reasoning_extra_body()` must check localhost BEFORE the
OpenRouter gate, otherwise localhost URLs return `False` before reaching
the localhost allowlist:

```diff
     def _supports_reasoning_extra_body(self) -> bool:
         # ... nousresearch, github, lmstudio, ollama checks ...
+        # Localhost / private gateway (FoxRouters, 9router, etc.) — trust
+        # the gateway to handle reasoning_effort passthrough. Must be BEFORE
+        # the openrouter gate below.
+        if (
+            "localhost" in self._base_url_lower
+            or "127.0.0.1" in self._base_url_lower
+            or "0.0.0.0" in self._base_url_lower
+        ):
+            return True
         if "openrouter" not in self._base_url_lower:
             return False
```

---

## Re-applying After Hermes Upgrade

These patches are in `/usr/local/lib/hermes-agent/` and are **lost on
`pip install --upgrade hermes-agent`**. Re-apply all 4 patches after every
upgrade.

Quick check if patches are present:

```bash
# Patch 1
grep -c 'reasoning_content' /usr/local/lib/hermes-agent/agent/turn_finalizer.py
# Should print 2

# Patch 2+3
grep -c '_reasoning_display' /usr/local/lib/hermes-agent/gateway/run.py
# Should print 6+

# Patch 4
grep -c 'localhost.*_base_url_lower' /usr/local/lib/hermes-agent/run_agent.py
# Should print 1
```

---

## Hermes Config

```yaml
# ~/.hermes/config.yaml
model:
  default: glm-5.2              # bare name (matches models.dev registry)
  provider: custom
  base_url: http://localhost:20130/v1
  api_key: gw-...

display:
  show_reasoning: true          # global default
  platforms:
    telegram:
      show_reasoning: true      # platform override
      streaming: true
  # reasoning_style: code       # code (default) | blockquote | subtext
```

### Config resolution chain

`display_config.py:resolve_display_setting()` priority:

1. `display.platforms.<platform>.<setting>` — explicit per-platform override
2. `display.<setting>` — global user setting
3. `_PLATFORM_DEFAULTS[<platform>][<setting>]` — built-in platform default
4. `_GLOBAL_DEFAULTS[<setting>]` — built-in global default

> ⚠️ Built-in defaults are `show_reasoning: False` for ALL platforms.
> Must set explicitly in `config.yaml` to enable.

### models.dev registry

Hermes uses `~/.hermes/models_dev_cache.json` (NOT the gateway `/v1/models`
endpoint) to determine model reasoning capability. The model name in config
must match the registry:

| Config model name | Registry match | Reasoning detected? |
|---|---|---|
| `glm-5.2` | `zhipuai/glm-5.2` → `reasoning: True` | ✅ Yes |
| `cb/glm-5.2` | Not in registry (gateway prefix) | ❌ No |

**Use bare model names** (no `cb/` prefix) in Hermes config to match the
registry. FoxRouters routes bare names to CodeBuddy upstream by default
(non-`grok-*` = CodeBuddy catch-all).

---

## Extraction Chain (end-to-end trace)

How `reasoning_content` flows from FoxRouters through Hermes to the user:

```
FoxRouters SSE passthrough
  → data: {"choices":[{"delta":{"reasoning_content":"..."}}]}
  → forwarded verbatim to client (Hermes)

Hermes streaming (chat_completion_helpers.py:2966)
  → getattr(delta, "reasoning_content", None) or getattr(delta, "reasoning", None)
  → reasoning_parts.append(reasoning_text)
  → _fire_reasoning_delta() [CLI display only — gateway doesn't set this callback]

Hermes post-stream (chat_completion_helpers.py:3200)
  → mock_message = SimpleNamespace(reasoning_content=full_reasoning)

Hermes build_assistant_message (chat_completion_helpers.py:1313)
  → _extract_reasoning(mock_message)
  → hasattr(mock_message, 'reasoning_content') → True
  → reasoning_text = reasoning_content
  → msg["reasoning"] = reasoning_text        (unified field)
  → msg["reasoning_content"] = raw_value      (preserved separately)

Hermes turn_finalizer (turn_finalizer.py:569)
  → last_reasoning = msg.get("reasoning") or msg.get("reasoning_content")
  → result["last_reasoning"] = last_reasoning

Hermes gateway delivery (run.py:14331)
  → _reasoning_display = render(last_reasoning)
  → if not already_sent: prepend to response (single message)
  → if already_sent: send as trailing message (separate delivery)
```

---

## Audit: Other `msg.get("reasoning")` spots in Hermes — all SAFE

10 other code spots access `msg.get("reasoning")` without checking
`reasoning_content`. All are safe because `build_assistant_message()` stores
the extracted (unified) reasoning into `msg["reasoning"]`:

| Spot | Why safe |
|---|---|
| `conversation_loop.py:5568` | Dedup compare — reads `msg["reasoning"]` which already contains extracted reasoning_content |
| `agent_runtime_helpers.py:136` | Trajectory storage — reads unified `msg["reasoning"]` |
| `agent_runtime_helpers.py:218` | Trajectory storage — same function, different branch |
| `gateway/session.py:3080` | Message restore — line 3081 also restores `reasoning_content` separately |
| `gateway/slash_commands.py:4615` | Display — line 4616 also passes `reasoning_content` |
| `codex_responses_adapter.py:929` | Request-side (reasoning config param, not response field) |
| `auxiliary_client.py:1028` | Request-side (extra_body reasoning config) |
| `auxiliary_client.py:1430` | Request-side (extra_body reasoning config) |
| `gateway/platforms/api_server.py:204` | Request-side (model options) |
| `gateway/platforms/api_server.py:2029` | Request-side (model options) |

---

## FAQ

### Why not fix this upstream in Hermes?

The patches are in `/usr/local/lib/hermes-agent/` (pip-installed). They can
be submitted as a PR to [NousResearch/hermes-agent](https://github.com/NousResearch/hermes-agent),
but until merged, local patches are required.

### Does FoxRouters need any changes?

No. FoxRouters correctly:
1. Forwards `delta.reasoning_content` in SSE passthrough
2. Accumulates `reasoning_content` in history `response_body`
3. Accepts `reasoning_effort` parameter and forwards to upstream

The bug is entirely in Hermes' delivery path (how it displays reasoning
to the user on streaming platforms).

### Does this affect non-streaming mode?

No. In non-streaming mode (`already_sent=False`), reasoning is prepended to
the response body as a single message — this always worked. The bug only
affects streaming mode (Telegram, Discord default).

### What about the empty `content=""` entries in gateway history?

Those are caused by a separate Hermes behavior: "Thinking-only response"
retry. When a reasoning model (e.g. GLM-5.2 with `reasoning_effort: high`)
returns `reasoning_content` without `content`, Hermes detects it via
`_is_thinking_only_assistant()` and triggers a prefill retry (max 2x).
Each retry creates a gateway history entry with `content=""`. This is
expected behavior, not a bug.
