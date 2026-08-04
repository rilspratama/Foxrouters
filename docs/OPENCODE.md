# OpenCode Integration — FoxRouters as OpenAI-Compatible Provider

> **Applies to:** OpenCode CLI ≥ 1.18.x connecting to FoxRouters via a custom
> `@ai-sdk/openai-compatible` provider (`base_url: http://127.0.0.1:20130/v1`)
> **Components affected:** `~/.config/opencode/opencode.json`,
> `~/.local/share/opencode/auth.json`
> **FoxRouters side:** No changes needed — standard `/v1/chat/completions`
> with tool calling + reasoning passthrough.

---

## Background

OpenCode (opencode.ai) is a provider-agnostic, open-source AI coding agent
with a TUI + CLI. It connects to any OpenAI-compatible endpoint through the
Vercel AI SDK `@ai-sdk/openai-compatible` provider. FoxRouters exposes
CodeBuddy + Grok models as a single OpenAI-compatible endpoint, so OpenCode
can use all gateway models (`glm-5.2`, `gpt-5.6-sol`, `claude-opus-4.7-1m`,
`kimi-k3`, `grok-4.5`, …) with zero provider changes.

**Verified working (Aug 2026):**

| Feature | Status | Evidence |
|---|---|---|
| Chat completions | ✅ | `opencode run` → 200 OK via gateway |
| Tool calling | ✅ | 10 tools registered (`bash`, `edit`, `glob`, `read`, …), multi-turn `tool_calls` + `tool_result` in gateway history |
| Reasoning (`reasoning_content`) | ✅ | Requires `options.reasoningEffort` in model config (see below) |
| Content filter (pudidil) | ✅ | Gateway `rewriteAgentIdentity()` strips Claude/Anthropic identity strings before upstream |

---

## Installation

```bash
npm i -g opencode-ai@latest        # or: brew install anomalyco/tap/opencode
opencode --version                 # ≥ 1.18.x
```

Binary location on this VPS: `/root/.opencode/bin/opencode`
(symlinked to `/usr/local/bin/opencode`).

---

## Auth

FoxRouters requires a Bearer gateway key. Register it in OpenCode:

```bash
# One-time: store the gateway key in OpenCode's credential store
opencode auth login   # choose "API key" → paste gateway key → name it "myrouter"
```

Credentials live at `~/.local/share/opencode/auth.json`:

```json
{
  "myrouter": {
    "type": "api",
    "key": "gw-..."
  }
}
```

Alternatively set `FOXROUTERS_API_KEY` env var and reference it in the config
(`"{env:FOXROUTERS_API_KEY}"`) — the config below uses the env-var form.

---

## Provider Config

`~/.config/opencode/opencode.json`:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "myrouter": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "My Router (FoxRouters)",
      "options": {
        "baseURL": "http://127.0.0.1:20130/v1",
        "apiKey": "{env:FOXROUTERS_API_KEY}"
      },
      "models": {
        "glm-5.2": {
          "name": "GLM 5.2",
          "reasoning": true,
          "options": { "reasoningEffort": "high" },
          "limit": { "context": 1000000, "output": 64000 }
        }
      }
    }
  },
  "model": "myrouter/glm-5.2"
}
```

### Model naming

- Model IDs are the **bare gateway names** — `glm-5.2`, `gpt-5.5`,
  `claude-opus-4.7-1m` — **not** the `cb/`-prefixed aliases.
- CLI model selector format: `myrouter/<bare-name>`
  (`--model myrouter/gpt-5.6-sol`).
- Gateway `/v1/models` returns both bare names and `cb/` aliases; OpenCode
  resolves models only from the config's `models` map, so list what you use.

### Full model list (18 reasoning models configured)

`glm-5.0/5.1/5.2` · `gpt-5.6-sol/terra/luna` · `gpt-5.5/5.4/5.3-codex` ·
`claude-opus-4.7-1m/4.6` · `claude-sonnet-4.6` · `gemini-3.1-pro` ·
`deepseek-v3` · `kimi-k2.5/k3` · `grok-4.5` / `grok-4.5-high`

---

## Reasoning — CRITICAL: `options.reasoningEffort`

OpenCode **does NOT send a reasoning parameter by default**. CodeBuddy gates
`reasoning_content` on the presence of `reasoning_effort` (or
`thinking`/`enable_thinking`) in the request body. Without it, the model
responds **without** emitting reasoning_content (and without paying thinking
tokens), even though the model is reasoning-capable.

**Fix:** set `options.reasoningEffort` per model in `opencode.json`:

```json
"glm-5.2": {
  "reasoning": true,
  "options": { "reasoningEffort": "high" }
}
```

This is forwarded by the AI SDK as `reasoning_effort: "high"` in the request
body → gateway `cbTransform` passes it upstream → CodeBuddy returns
`reasoning_content` (verified in gateway history).

### Verified request/response

```
REQ reasoning_effort: high
RESP reasoning_content: 'The user is asking a math question. Let me calculate...'
RESP completion_thinking_tokens: 60
```

### Reasoning effort levels

| Value | Use case | Credit cost |
|---|---|---|
| `high` | Hard problems, code review, architecture | Highest (thinking tokens billed) |
| `medium` | Balanced default | Medium |
| `low` / remove key | Simple Q&A, quick edits | Lowest |

---

## Tool Calling

OpenCode registers its built-in tools (`bash`, `edit`, `glob`, `read`, `grep`,
`web`…) in the request `tools` array. FoxRouters passes them through
untouched — no transformation needed. Multi-turn tool loops work normally:

```
msg[2] role=assistant tool_calls: ['bash']
msg[3] role=tool          (tool_result)
msg[4] role=assistant tool_calls: ['read']
msg[5] role=tool          (tool_result)
```

---

## Usage

```bash
# Set key for the session (one-time per shell)
export FOXROUTERS_API_KEY=$(head -1 /root/nexus-workspace/foxrouters/gateway-key.txt)

# One-shot task (default model glm-5.2)
opencode run 'Add retry logic to API calls and update tests'

# Pick a model
opencode run 'Refactor auth module' --model myrouter/gpt-5.6-sol
opencode run 'Review this PR' --model myrouter/claude-opus-4.7-1m

# Interactive TUI
opencode                      # then Ctrl+X M to switch model
```

### CLI flags (relevant subset)

| Flag | Use |
|------|-----|
| `run '<prompt>'` | One-shot execution and exit |
| `--model myrouter/<name>` | Force a model |
| `--thinking` | Show model thinking blocks |
| `--variant high` | Reasoning effort override |
| `-f <path>` | Attach context file |
| `-c` / `-s <id>` | Continue last / specific session |

---

## Verification

```bash
# 1. Provider resolves
opencode models | grep myrouter

# 2. Smoke test
opencode run 'Respond with exactly: OPENCODE_SMOKE_OK' --model myrouter/glm-5.2

# 3. Reasoning (should show thinking + reasoning_content in gateway history)
opencode run 'What is 17 * 23? Show your reasoning.' --model myrouter/glm-5.2

# 4. Tool calling (should create/edit files and run commands)
cd /tmp && opencode run 'Create a Go file that prints hello and run it' --model myrouter/glm-5.2

# 5. Confirm in gateway history (input/output, reasoning_content, thinking tokens)
curl -s "http://127.0.0.1:20130/history/recent?limit=3" \
  -H "Authorization: Bearer $(head -1 /root/nexus-workspace/foxrouters/gateway-key.txt)"
```

---

## Pitfalls

1. **Model not found in OpenCode** — the model must be in the `models` map of
   `opencode.json`; OpenCode does not auto-discover gateway models from
   `/v1/models`. Add it to the config.
2. **No reasoning_content** — almost always a missing `options.reasoningEffort`
   (OpenCode default sends none). Add `"options": {"reasoningEffort": "high"}`.
3. **`cb/` prefix model errors** — use bare names (`glm-5.2`), not `cb/glm-5.2`.
4. **`muyrouter` typo** — older auth entries may contain a misspelled
   provider name (`muyrouter`); harmless but unused. Clean with
   `opencode auth logout`.
5. **Env var not set** — `FOXROUTERS_API_KEY` must be exported in the shell
   before `opencode run`, or hardcode the key in `opencode.json`
   (`"apiKey": "gw-..."` — but prefer env var to avoid leaking secrets).

---

## Files

| Path | Purpose |
|---|---|
| `~/.config/opencode/opencode.json` | Provider + model config |
| `~/.local/share/opencode/auth.json` | Credential store |
| `~/.opencode/bin/opencode` | Binary (v1.18.12) |
| `/usr/local/bin/opencode` | Symlink for PATH |
