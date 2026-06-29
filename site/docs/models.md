# Bring your own model

Beyond Claude Code and Codex, drydock can run **any OpenAI-compatible endpoint**
— Google Gemini (via its OpenAI-compatible API), OpenRouter, or a local server
(Ollama, LM Studio, vLLM) — through the same sandbox. The agent is **`opencode`**,
and as with every drydock task the real key stays host-side: the VM only ever
sees a per-task token and the gateway's address.

## How it works

`opencode` runs in the throwaway VM pointed at drydock's credential gateway. The
gateway holds your real key, forwards the request to your configured endpoint,
and meters the response — exactly like the Claude and Codex lanes. You configure
*which* endpoint; drydock handles the isolation.

## Configure

Add an `openai_compat` block to `~/.drydock/config.yaml` (or let the setup wizard
write it — see below). The real key is referenced by the **name** of a host env
var; it is never stored in the file.

```yaml
openai_compat:
  base_url:    "https://generativelanguage.googleapis.com"   # endpoint host (empty = disabled)
  base_path:   "/v1beta/openai"                              # joined onto the request path
  api_key_env: "GEMINI_API_KEY"                              # NAME of the env var holding your key
  model:       "gemini-2.5-pro"                              # model id passed to the agent
```

Then export the key host-side and start:

```bash
export GEMINI_API_KEY=...
drydock start
```

`base_url` must be `https` (plain `http` is allowed only for `localhost`, for a
local model server).

### …or use the wizard

`drydock setup` asks **"Configure a bring-your-own OpenAI-compatible endpoint
(e.g. Gemini, OpenRouter, local)?"** and prompts for the base URL, model, and the
key's env-var name, writing the block for you.

## Run a task

```bash
drydock submit --repo … --instruction "…" --agent opencode
```

The model comes from `openai_compat.model`; pass `--model <id>` to override it
per task. Everything else — the approval gate, the diff, egress — works exactly
as it does for Claude and Codex.

## Worked examples

**Google Gemini** (its OpenAI-compatible endpoint):

```yaml
openai_compat:
  base_url:    "https://generativelanguage.googleapis.com"
  base_path:   "/v1beta/openai"
  api_key_env: "GEMINI_API_KEY"
  model:       "gemini-2.5-pro"
```

**OpenRouter** (one endpoint, hundreds of models):

```yaml
openai_compat:
  base_url:    "https://openrouter.ai/api"
  base_path:   "/v1"
  api_key_env: "OPENROUTER_API_KEY"
  model:       "google/gemini-2.5-pro"
```

**Local server** (Ollama / LM Studio / vLLM on the host):

```yaml
openai_compat:
  base_url:    "http://localhost:11434"   # http allowed for localhost
  base_path:   "/v1"
  api_key_env: "LOCAL_KEY"                # any value your server accepts (or a placeholder)
  model:       "llama3.1"
```

## The one constraint: chat/completions

The lane speaks the OpenAI **chat/completions** wire format — which is why it
uses `opencode` (Claude Code talks Anthropic's format; Codex talks the OpenAI
*Responses* API). Any endpoint that serves `/chat/completions` works: Gemini's
OpenAI endpoint, OpenRouter, and most local servers all do. Models reachable
*only* via the Responses API or a vendor-native format aren't on this lane — use
`--agent codex` or `--agent claude` for those.

## Budget

USD budgeting needs prices, which drydock can't know for an arbitrary endpoint.
Two options:

- **Set prices** (USD per 1M tokens) and the per-task `task_budget_usd` ceiling
  applies as usual:

  ```yaml
  openai_compat:
    base_url: …
    # …
    prices:
      gemini-2.5-pro: { input: 1.25, output: 10.0 }   # use your provider's published rates
  ```

- **Or rely on the request cap.** With no prices, the lane meters by round-trip
  count: set `task_max_requests` in `config.yaml` to bound a runaway task (the
  same control subscription mode uses).
