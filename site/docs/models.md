# Models

## Gemini (native): experimental

drydock has a **native `gemini` agent** that speaks Google's own wire format:
Google's auth header (`x-goog-api-key`) and a `usageMetadata` token-metering
parser, rather than the OpenAI-compatibility shim.

> **Experimental: not yet verified end-to-end.** The gateway, usage parser, and
> price table have CI unit coverage, but the full in-sandbox run (the CLI making
> a real metered call through the gateway under deny-by-default egress, plus the
> A1/A2 red-team) is macOS-gated and has not been executed. Until
> `make test-integration` passes on macOS with a real `GEMINI_API_KEY`, treat
> this lane as experimental. If you just need Gemini working today, the
> [OpenAI-compatible lane](#bring-your-own-model-openai-compatible-lane) routes to
> Gemini's `/v1beta/openai` endpoint and is a proven path.

### Requirements

- **`GEMINI_API_KEY`**: set in your shell env or stored at
  `~/.drydock/api-keys.env`. This is the only auth mode: no OAuth, no Google
  subscription lane.
- A sandbox image built with `@google/gemini-cli` installed
  (`drydock init` handles this).

### Run a task

```bash
export GEMINI_API_KEY=...
drydock submit --repo … --instruction "…" --agent gemini
```

Or set `default_agent: gemini` in `~/.drydock/config.yaml` to make it the
default.

### Models

| Model | Notes |
|---|---|
| `gemini-2.5-pro` | Default, best for coding tasks |
| `gemini-2.5-flash` | Faster, lower cost |
| `gemini-2.5-flash-lite` | Lightest, lowest cost |

Override per task with `--model gemini-2.5-flash`. With no explicit model the
agent defaults to `gemini-2.5-pro`. Note `default_model` in `config.yaml` does
**not** apply to Gemini (it is Claude/Codex-only). Use `--model` per task.

### Gemini via the OpenAI-compat lane vs. the native lane

Gemini is reachable two ways. The `opencode` + `openai_compat` lane (see below)
is the **proven** path today; the native `gemini` agent is **experimental**
(see the note above) but speaks Google's own wire format:

| | `--agent gemini` (native) | `--agent opencode` + `openai_compat` |
|---|---|---|
| Wire protocol | Google's native Gemini API | OpenAI chat/completions compat |
| Auth header | `x-goog-api-key` | `Authorization: Bearer` |
| Token metering | Native `usageMetadata` | Depends on endpoint reporting usage |
| Config needed | Just `GEMINI_API_KEY` | `openai_compat:` block in config.yaml |

---

## Bring your own model (OpenAI-compatible lane)

Beyond Claude Code, Codex, and the native Gemini lane, drydock can run **any
OpenAI-compatible endpoint** (OpenRouter, or a local server: Ollama, LM
Studio, vLLM) through the same sandbox. The agent is **`opencode`**, and as
with every drydock task the real key stays host-side: the VM only ever
sees a per-task token and the gateway's address.

## How it works

`opencode` runs in the throwaway VM pointed at drydock's credential gateway. The
gateway holds your real key, forwards the request to your configured endpoint,
and meters the response, exactly like the Claude and Codex lanes. You configure
*which* endpoint; drydock handles the isolation.

## Configure

Add an `openai_compat` block to `~/.drydock/config.yaml` (or let the setup wizard
write it; see below). The real key is referenced by the **name** of a host env
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
per task. Everything else (the approval gate, the diff, egress) works exactly
as it does for Claude and Codex.

## Worked examples

**Google Gemini via the compat lane,** the proven path today (the native
`--agent gemini` above is experimental, see [Gemini (native)](#gemini-native-experimental)):

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

The lane speaks the OpenAI **chat/completions** wire format, which is why it
uses `opencode` (Claude Code talks Anthropic's format; Codex talks the OpenAI
*Responses* API). Any endpoint that serves `/chat/completions` works: Gemini's
OpenAI endpoint, OpenRouter, and most local servers all do. Models reachable
*only* via the Responses API or a vendor-native format aren't on this lane. Use
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
