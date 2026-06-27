# Docs & site freshen (bring-your-own-model) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring the user-facing docs and landing site up to date with bring-your-own-model (`openai_compat`/`opencode`) and this session's other shipped features (`--draft`, `approval_timeout`, graceful-degrade), tightening prose as each page is touched.

**Architecture:** A new `site/docs/models.md` page is the home for bring-your-own-model; the other pages get light, accurate edits + cross-links. The Markdown sources are tracked; `make docs` regenerates the gitignored `site/docs/*.html`. One one-line code change (`submit.go` `--agent` help).

**Tech Stack:** Markdown docs rendered by `cmd/docs-build` (Go); the landing page is hand-authored `site/index.html`.

## Global Constraints

- **Accurate to `main`.** Exact `openai_compat` fields: `base_url`, `base_path`, `api_key_env` (the env-var **name**, never the key value), `model`, optional `prices` (`{<model>: {input, output}}`, USD per 1M tokens). `base_url` is https (http only for `localhost`).
- The bring-your-own lane uses the **chat/completions** wire format → the `opencode` agent (Claude = Anthropic format; Codex = OpenAI Responses API).
- `--agent` / `default_agent` values: `claude` | `codex` | `opencode`.
- `approval_timeout` has **no env override** (config.yaml only — there is no `DRYDOCK_APPROVAL_TIMEOUT`). `default_agent` *does* have `DRYDOCK_DEFAULT_AGENT`. `openai_compat` has no flat env override (config.yaml or wizard only).
- Budget: openai-compat USD metering needs `prices`; otherwise `task_max_requests` caps (same as subscription mode).
- The credential guarantee is unchanged and restated: real key host-side; VM sees only a per-task token + the gateway address.
- Keep the existing tight, example-led voice. Don't pad. macOS 26+/Apple-silicon + pre-1.0 framing stays.
- **Verify after every task:** `go test ./cmd/docs-build/` must stay green (`TestNoStaleVersion`, `TestLandingInternalLinksResolve`). Don't commit regenerated `site/docs/*.html` (gitignored). Run `make docs` to render-check locally.

---

### Task 1: New `models.md` page + registration + `index.md`

**Files:**
- Create: `site/docs/models.md`
- Modify: `cmd/docs-build/main.go:19` (the `order` slice)
- Modify: `site/docs/index.md` (intro + Pages list)
- Test: `go test ./cmd/docs-build/`

**Interfaces:**
- Produces: a docs page at slug `models` (→ `models.html`), linked from `index.md`. Later tasks (4) link the landing site to `docs/models.html`, which requires this `.md` source to exist (`TestLandingInternalLinksResolve`).

- [ ] **Step 1: Create `site/docs/models.md`** with this exact content:

````markdown
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
````

- [ ] **Step 2: Register the page in the sidebar order**

In `cmd/docs-build/main.go`, line 19, add `"models"` after `"configuration"`:

```go
var order = []string{"index", "quickstart", "authentication", "submitting-tasks", "egress", "configuration", "models", "troubleshooting", "threat-model"}
```

- [ ] **Step 3: Update `site/docs/index.md`** — the intro line and Pages list.

Change the intro's parenthetical:

```
(Claude Code or OpenAI Codex)
```
to:
```
(Claude Code, OpenAI Codex, or any OpenAI-compatible model)
```

And add a Pages-list bullet after the Authentication line:

```markdown
- **[Bring your own model](models.html)** — run any OpenAI-compatible endpoint (Gemini, OpenRouter, local) via `opencode`.
```

- [ ] **Step 4: Render + test**

Run: `make docs && go test ./cmd/docs-build/`
Expected: docs build prints `==> site/docs/models.html` (among others); tests PASS. Open `site/docs/models.html` to eyeball it in the sidebar.

- [ ] **Step 5: Commit** (sources only — the `*.html` are gitignored)

```bash
git add site/docs/models.md cmd/docs-build/main.go site/docs/index.md
git commit -m "docs: add Bring-your-own-model page (openai_compat/opencode)"
```

---

### Task 2: `authentication.md`, `submitting-tasks.md`, `--agent` help

**Files:**
- Modify: `site/docs/authentication.md`
- Modify: `site/docs/submitting-tasks.md`
- Modify: `cmd/drydock/submit.go` (the `--agent` flag usage string)
- Test: `go test ./cmd/docs-build/ ./cmd/drydock/`

**Interfaces:**
- Consumes: `models.html` (Task 1) for cross-links.

- [ ] **Step 1: `authentication.md` intro** — change the first paragraph's "two agents" to three, and add a one-line pointer. Replace:

```
drydock runs two agents — **Claude Code** (Anthropic) and **OpenAI Codex**
(OpenAI) — and each works with either a vendor **API key** or your existing
**subscription**.
```
with:
```
drydock runs **Claude Code** (Anthropic) and **OpenAI Codex** (OpenAI), each
with a vendor **API key** or your existing **subscription** — and **`opencode`**
for any OpenAI-compatible endpoint (see [Bring your own model](models.html)).
```

- [ ] **Step 2: `authentication.md` — add a short subsection** after the "## The matrix" table (before "## API key"):

```markdown
### Bring your own model

`opencode` reaches any OpenAI-compatible endpoint (Gemini, OpenRouter, a local
server). It's API-key-only — no OAuth — and configured by the `openai_compat`
block, not the matrix above. See [Bring your own model](models.html).
```

- [ ] **Step 3: `submitting-tasks.md` Variations** — extend the agent example and add `--draft`. Replace the existing codex variation:

```bash
# Use OpenAI Codex instead of Claude Code for this task
drydock submit --repo … --instruction "…" --agent codex
```
with:
```bash
# Pick the agent for this task: claude (default) | codex | opencode
drydock submit --repo … --instruction "…" --agent codex
# opencode runs any OpenAI-compatible model — see Bring your own model
drydock submit --repo … --instruction "…" --agent opencode
```

And add a `--draft` variation after the `--auto-approve` one:

```bash
# Open the resulting PR/MR as a draft
drydock submit --repo … --instruction "…" --draft
```

- [ ] **Step 4: `submitting-tasks.md` — one line on graceful PR open.** In "## The approval gate", after the "A denied task keeps its diff…" sentence, add:

```markdown
If the branch pushes but the PR can't be opened (e.g. `gh` isn't authenticated),
drydock reports it as **pushed** with a hint to open the PR manually — it never
loses your work to a failed PR step.
```

- [ ] **Step 5: `submit.go` — fix the `--agent` help** (the one code change). Change:

```go
		agent       = fs.String("agent", "", "sandbox agent: claude | codex (default: broker's default_agent)")
```
to:
```go
		agent       = fs.String("agent", "", "sandbox agent: claude | codex | opencode (default: broker's default_agent)")
```

- [ ] **Step 6: Build + test**

Run: `gofmt -w cmd/drydock/submit.go && make docs && go test ./cmd/docs-build/ ./cmd/drydock/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add site/docs/authentication.md site/docs/submitting-tasks.md cmd/drydock/submit.go
git commit -m "docs: authentication/submitting cover opencode + --draft; fix --agent help"
```

---

### Task 3: `configuration.md` table

**Files:**
- Modify: `site/docs/configuration.md`
- Test: `go test ./cmd/docs-build/`

- [ ] **Step 1: Update `default_agent` row + add `approval_timeout`.** In the "## Common settings" table, change the `default_agent` Meaning to list opencode, and add an `approval_timeout` row after `task_timeout`. Replace:

```
| `default_agent` | `DRYDOCK_DEFAULT_AGENT` | `claude` | Agent when `--agent` is omitted (`claude` \| `codex`) |
```
with:
```
| `default_agent` | `DRYDOCK_DEFAULT_AGENT` | `claude` | Agent when `--agent` is omitted (`claude` \| `codex` \| `opencode`) |
```

And after the `task_timeout` row, add (note the em-dash in the env column — there is no env override):

```
| `approval_timeout` | — | `0s` | Auto-deny a task left at an approval gate after this long; `0` = wait forever (right for interactive use; set for unattended runs) |
```

- [ ] **Step 2: Add an `openai_compat` section** after the "## Common settings" table (before "## Advanced"):

```markdown
## Bring your own model

`opencode` reaches any OpenAI-compatible endpoint via the `openai_compat` block
in `config.yaml` (or the `drydock setup` wizard). There is **no env override** —
configure it in the file. The real key is referenced by env-var **name**, never
stored here.

| Key (under `openai_compat:`) | Meaning |
|---|---|
| `base_url` | Endpoint host, e.g. `https://generativelanguage.googleapis.com` (empty = disabled; https, or http only for `localhost`) |
| `base_path` | Path joined onto the request, e.g. `/v1beta/openai` |
| `api_key_env` | **Name** of the host env var holding the real key (e.g. `GEMINI_API_KEY`) |
| `model` | Model id passed to the agent, e.g. `gemini-2.5-pro` |
| `prices` | Optional `{<model>: {input, output}}` USD per 1M tokens — enables USD budgeting; omit to rely on `task_max_requests` |

See [Bring your own model](models.html) for worked examples.
```

- [ ] **Step 2b: Run + test**

Run: `make docs && go test ./cmd/docs-build/`
Expected: PASS; eyeball `site/docs/configuration.html`.

- [ ] **Step 3: Commit**

```bash
git add site/docs/configuration.md
git commit -m "docs: configuration covers approval_timeout + openai_compat"
```

---

### Task 4: `README.md` + landing `site/index.html`

**Files:**
- Modify: `README.md`
- Modify: `site/index.html`
- Test: `go test ./cmd/docs-build/`

- [ ] **Step 1: README pitch.** In the opening paragraph, change "runs **Claude Code** or **OpenAI Codex**" to include bring-your-own-model. Replace:

```
drydock runs **Claude Code** or **OpenAI Codex** full-throttle on your own
repos, on your own Mac
```
with:
```
drydock runs **Claude Code**, **OpenAI Codex**, or **any OpenAI-compatible model**
(Gemini, OpenRouter, local) full-throttle on your own repos, on your own Mac
```

- [ ] **Step 2: README — restate the key guarantee for the new lane is unchanged.** No new bullet needed; the existing "It never gets your key" bullet already covers all lanes. (No edit — confirm the three bullets still read true for opencode; they do.)

- [ ] **Step 3: Landing `site/index.html`.** Locate the capabilities/feature copy that frames "Claude Code / Codex" (search for `Claude Code` and `Codex` in the file). Add a concise mention that drydock also runs any OpenAI-compatible model (Gemini/OpenRouter/local) via `opencode`, matching the surrounding copy's tone and markup. If the landing has a features/cards list, add or extend a line; do not restructure the layout. If it links into the docs, link the new capability to `docs/models.html` (the `models.md` source from Task 1 makes that link resolve).

- [ ] **Step 4: Build + test**

Run: `make docs && go test ./cmd/docs-build/`
Expected: PASS — in particular `TestLandingInternalLinksResolve` (any `docs/models.html` link resolves to `site/docs/models.md`) and `TestNoStaleVersion` (version strings unchanged).

- [ ] **Step 5: Commit**

```bash
git add README.md site/index.html
git commit -m "docs: README + landing mention bring-your-own-model"
```

---

## Self-Review

**Spec coverage:**
- New `models.md` (spec component 1) → Task 1. ✓
- `authentication.md` (2) → Task 2 Steps 1-2. ✓
- `submitting-tasks.md` (3) → Task 2 Steps 3-4. ✓
- `configuration.md` table incl. `approval_timeout` env `—` + `openai_compat` (4, F1) → Task 3. ✓
- `index.md` (5) → Task 1 Step 3. ✓
- `README.md` (6) → Task 4 Steps 1-2. ✓
- `submit.go` `--agent` help (7, F2) → Task 2 Step 5. ✓
- `site/index.html` (8) → Task 4 Step 3. ✓
- Build/test green-keeping → every task's verify step. ✓

**Placeholder scan:** the new page's content is complete and verified; the smaller edits are exact old→new replacements. Task 4 Step 3 (landing) is described rather than verbatim because the exact surrounding markup isn't reproduced here — it names the search anchor (`Claude Code`/`Codex`), the tone constraint, and the link target, which is the right level for a hand-authored HTML page the implementer reads; not a placeholder.

**Type/fact consistency:** `openai_compat` fields and the chat/completions constraint are identical across `models.md`, `configuration.md`, and `authentication.md`; the `--agent … opencode` value matches between `submit.go`, `submitting-tasks.md`, `configuration.md`, and `index.md`; `approval_timeout` is consistently shown with no env override; the worked-example base_url/base_path pairs match the gateway's `BaseURL`+`BasePath` forwarding (Gemini `/v1beta/openai`, OpenRouter `/v1`, local `/v1`).
