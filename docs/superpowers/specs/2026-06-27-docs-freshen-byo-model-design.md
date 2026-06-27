# Docs & site freshen (bring-your-own-model + this session's features) — design

**Goal:** Bring the user-facing docs and landing site up to date with what
shipped this session — chiefly **bring-your-own-model** (`openai_compat` /
`opencode`), plus `--draft`, `approval_timeout`, and the graceful-degrade /
`interrupted` behaviors — and tighten prose for concision/consistency as each
page is touched.

**Scope:** Content accuracy first; polish folded into the same edits. No code
logic changes beyond a one-line page-registration in `cmd/docs-build`. The
landing site gets copy updates (capabilities), not a visual redesign.

## Constraints (bind every change)

- **Accurate to the shipped code.** Every documented flag, config key, and
  behavior must match `main`. Specifics to get right:
  - `openai_compat` fields: `base_url`, `base_path`, `api_key_env`, `model`,
    optional `prices` (`{<model>: {input, output}}`, USD per 1M tokens). The key
    is referenced by **env-var name** (`api_key_env`), never stored in the file.
    `base_url` must be https (http only for `localhost`).
  - The openai-compat lane uses the **chat/completions** wire format → the
    `opencode` agent (not codex/claude). Gemini via `/v1beta/openai`, OpenRouter,
    local Ollama/LM Studio.
  - `--agent` values: `claude` | `codex` | `opencode`. `default_agent` likewise.
  - `--draft` opens the PR/MR as a draft. A pushed branch whose PR can't open
    reports `pushed` (open it manually), never `push failed`.
  - `approval_timeout` (config): auto-deny a task waiting at an approval gate
    after this long; `0` = wait forever.
  - Budget: in openai-compat mode, USD metering needs `prices`; otherwise the
    `task_max_requests` cap applies (same pattern as subscription mode).
- **The credential guarantee is unchanged** and must be restated for the new
  lane: the real upstream key stays host-side; the VM sees only a per-task
  `tok_` lease + the gateway base URL.
- **macOS 26+ / Apple silicon** requirement and the pre-1.0/alpha framing stay.
- Concision: prefer the existing pages' tight, example-led voice. Don't pad.

## Components (page-by-page)

### 1. New page — `site/docs/models.md` (slug `models`, H1 "Bring your own model")
The headline addition. Sections:
- **What** — any OpenAI-compatible endpoint through the gateway, run by the
  `opencode` agent; the real key stays host-side.
- **Configure** — the `openai_compat` block (all five fields, with the
  `api_key_env` "name not value" note), or the `drydock setup` wizard prompt.
- **Run** — `drydock submit --agent opencode …`; model comes from
  `openai_compat.model` (or `--model`).
- **Worked examples** — Gemini (`base_url: https://generativelanguage.googleapis.com`,
  `base_path: /v1beta/openai`), OpenRouter, a local server.
- **The constraint** — chat/completions only; Responses-API-only or
  Anthropic-native models are out (those are codex/claude lanes).
- **Budget** — `prices` for USD metering, else the `task_max_requests` cap.

Register in `cmd/docs-build/main.go`'s `order` slice (after `configuration`,
before `troubleshooting`): `"…", "configuration", "models", "troubleshooting", …`.
The H1 becomes the sidebar label automatically.

### 2. `site/docs/authentication.md`
- Intro: "two agents" → three (Claude Code, OpenAI Codex, and **opencode** for
  bring-your-own OpenAI-compatible models).
- After the matrix, a short **"Bring your own model"** subsection: opencode is
  API-key + endpoint (no OAuth), config-driven — link to `models.html` for the
  full setup. Keep it to a few lines; the dedicated page carries the detail.

### 3. `site/docs/submitting-tasks.md`
- Variations: add `--agent opencode` (one line, link to models.html) and
  `--draft` (open the PR/MR as a draft).
- One line on the graceful PR behavior near the approval/push content: a branch
  that pushes but whose PR can't open is reported as `pushed` with a manual-PR
  hint, not a failure.

### 4. `site/docs/configuration.md`
Config table gains rows (matching the existing table format):
- `approval_timeout` — `DRYDOCK_APPROVAL_TIMEOUT` — `0s` — auto-deny a task
  waiting at an approval gate after this long; `0` waits forever.
- `default_agent` row updated to `claude | codex | opencode`.
- An `openai_compat` block reference: a compact sub-table or rows for
  `base_url` / `base_path` / `api_key_env` / `model` / `prices`, with a pointer
  to `models.html`. (Nested keys — present them clearly; the table's one-key-
  per-row format may warrant a short prose block + a pointer instead of cramming
  nested keys into the env-override column.)

### 5. `site/docs/index.md`
- Intro framing: "Claude Code or OpenAI Codex" → "Claude Code, OpenAI Codex, or
  any OpenAI-compatible model".
- Pages list: add **Bring your own model** → `models.html`.

### 6. `README.md`
- Top pitch + feature bullets: mention bring-your-own-model (Gemini/OpenRouter/
  local), `--draft`, `approval_timeout`. Light touch — don't restructure.

### 7. `site/index.html` (landing)
- Capabilities/feature copy: add bring-your-own-model alongside the
  claude/codex framing. Keep the existing layout/structure; copy only.
- If a `docs/models.html` link is added, the `models.md` source must exist (it
  does — component 1) so `TestLandingInternalLinksResolve` passes.

## Build & verify

- Edit `.md` / `README.md` / `site/index.html` sources, add `models` to the
  `order` slice, then `make docs` regenerates `site/docs/*.html` (gitignored;
  CI rebuilds on deploy).
- `go test ./cmd/docs-build/` must stay green:
  `TestNoStaleVersion` (no stale version strings) and
  `TestLandingInternalLinksResolve` (every landing link to `docs/*.html`
  resolves to a `.md` source) — the latter is why the new `models.md` must exist
  before linking to `models.html`.
- A local preview (`make docs` then open `site/docs/models.html`) confirms the
  page renders and appears in the sidebar.

## Error handling / edge cases

- Don't document env overrides that don't exist. `openai_compat` is a nested
  YAML block; if there's no flat `DRYDOCK_OPENAI_COMPAT_*` env override, say so
  (configure via `config.yaml` or the wizard) rather than inventing one.
- Keep version/requirement strings consistent so `TestNoStaleVersion` passes.

## Done when

The docs and landing site accurately describe bring-your-own-model and the
session's other shipped features; a new `models.html` page exists and is in the
sidebar; `go test ./cmd/docs-build/` is green; and the touched pages read
tighter, in the existing voice.
