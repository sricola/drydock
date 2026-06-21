# First-run setup wizard — design spec

**Date:** 2026-06-21
**Status:** Approved (brainstorming) — ready for implementation plan

## Goal

Make a new user's first run of drydock simple: turn `drydock setup` into a
guided, wizard-style flow that takes them from a fresh install to a working
configuration — choosing an agent, choosing an auth mode, bootstrapping the
credential, and writing `~/.drydock/config.yaml` — with minimal friction and no
manual config editing required.

## Background

Today the first-run path is:

- `drydock setup` installs the two prerequisites (Apple `container`, squid) via
  Homebrew with y/N prompts, then hands off to `drydock init`.
- `drydock init` checks platform/container/squid/git, starts the container
  system, creates the network, builds the sandbox + anchor images, and seeds
  `~/.drydock/{config,egress}.yaml` from a **static** template. It then prints a
  static "ready. next:" block: `export ANTHROPIC_API_KEY=…`, edit config,
  `drydock start`, etc.

The gap: there is **no guided choice of agent or auth mode and no credential
bootstrap**. The user is left to know that they must pick `default_agent`, set
`anthropic_auth`/`openai_auth`, and either export an API key or run
`drydock auth claude|codex`. drydock supports four ways to run (Claude Code /
OpenAI Codex × API key / subscription); none of that is surfaced interactively.

## Decisions (locked during brainstorming)

1. **Entry point:** the wizard *is* `drydock setup`. `drydock init` stays the
   low-level, non-interactive primitive (scriptable; unchanged behavior).
2. **API-key handling:** env-only. The wizard records the *choice* (`api_key`)
   and verifies/reminds about the env var; it **never writes an API key to
   disk** (upholds drydock's "real key stays host-side, never persisted"
   stance). Only the subscription OAuth token is stored (as today, 0600).
3. **Implementation style:** plain numbered stdin prompts reusing the existing
   color/`step()` formatting. **No TUI framework / no new dependency** — keeps
   the supply-chain surface (SBOM + cosign-signed releases) clean. The "wizard"
   value is the flow and defaults, not a full-screen UI.
4. **Re-run defaults to preserving config:** re-running `setup` with an existing
   `config.yaml` keeps it (reconfigure is opt-in via a prompt / `--reconfigure`).
5. **Not-logged-in is non-fatal:** if a chosen subscription's vendor CLI isn't
   logged in, the wizard writes the config anyway and prints the exact
   `claude login` / `codex login` command, rather than blocking.

## Architecture

`runSetup` becomes the orchestrator:

```
drydock setup
  ├─ prereqs            (existing: Homebrew, container cask, squid)
  ├─ infra              (existing: runInit's container-system/network/images steps)
  └─ configure          (NEW, only when TTY + first-run or --reconfigure)
       ├─ choose agent(s)                  → default_agent + which backends
       ├─ per agent: choose auth           → anthropic_auth / openai_auth
       ├─ per agent: bootstrap credential  → subscription auth, or env reminder
       ├─ write ~/.drydock/config.yaml     → chosen keys, template defaults elsewhere
       └─ verify + next steps              → token check (subscription), start hint
```

Non-TTY or existing-config-without-`--reconfigure`: skip the `configure` block
and behave exactly as today (so CI and `drydock init` are unaffected).

### Components (small, isolated, testable)

| Unit | Responsibility |
|---|---|
| `cmd/drydock/wizard.go` (new) | The `configure` orchestration (`runWizard`) + **pure** prompt helpers: `promptChoice(in io.Reader, out io.Writer, q string, opts []string, dflt int) int`, `promptYesNo(in, out, q string, dflt bool) bool`. Helpers read/write injected streams so they're unit-testable. |
| `renderConfig(choices) string` (new, in wizard.go or config seam) | Renders the full comment-rich `config.yaml` body with the chosen `default_agent`/`anthropic_auth`/`openai_auth`; all other keys keep the existing template defaults. |
| `runSetup` (modify) | Decide interactive vs non-interactive (TTY + first-run / `--reconfigure`); run prereqs + infra; on interactive, call `runWizard`; else today's path. |
| auth cores (modify) | Extract the callable core of `drydock auth claude` / `auth codex` into functions the wizard can invoke and branch on (e.g. `bootstrapClaudeCred() error`, `bootstrapCodexCred() error`). The existing `drydock auth claude|codex` subcommands call the same core. |

## Wizard flow (detail)

1. **Choose agent.** `promptChoice`: `[1] Claude Code (default)  [2] OpenAI Codex
   [3] both`. Sets `default_agent` (claude or codex; for "both", default to
   claude and configure both backends).
2. **Per chosen agent, choose auth.** `[1] subscription (default)  [2] API key`.
   Sets `anthropic_auth` / `openai_auth` accordingly.
3. **Per chosen agent, bootstrap the credential:**
   - **subscription:** verify the vendor CLI exists and is logged in; run the
     auth core (reads Keychain / `~/.codex/auth.json`, writes
     `~/.drydock/{claude,codex}-oauth.json` 0600); then a light token-validity
     check (the same `OAuthCred.Current()` the doctor uses) → `✓ token valid`.
     If the CLI is missing or not logged in → print the exact
     `claude login` / `codex login` line; **non-fatal**.
   - **API key:** if `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` is set → `✓`; else
     print `export ANTHROPIC_API_KEY=sk-ant-…` (the exact var) as a reminder to
     run before `drydock start`. **Never stores the key.**
4. **Write config.** Render `~/.drydock/config.yaml` with the choices. If a
   config already exists and the user is reconfiguring, overwrite; the egress
   file is left untouched (seeded by init as today).
5. **Finish.** Print a one-line summary of what was written, then
   `start: drydock start` and a `drydock submit` example. Recommend
   `drydock doctor` to verify.

## Error handling

- **Invalid prompt input** (out-of-range / non-numeric) → re-prompt with a short
  "enter 1–N"; empty input → the default.
- **Not a TTY** → never enter the wizard (the gate); EOF on stdin can't hang it.
- **Credential not ready** (CLI missing / not logged in / env var unset) →
  clear, **non-fatal** guidance; config is still written so the user can finish
  out-of-band and `drydock start`.
- **Config write failure** (permissions) → surfaced via `step(..., false, err)`
  and a non-zero exit, consistent with the rest of `init`.

## Testing

- **`promptChoice` / `promptYesNo`** — table tests over an `io.Reader` of
  scripted input: valid selection, Enter→default, invalid-then-valid retry,
  out-of-range rejection.
- **`renderConfig`** — for each agent×auth combination, assert the rendered
  `config.yaml` parses (via `config.Load` on a temp file) and carries the right
  `default_agent` / `anthropic_auth` / `openai_auth`, with other keys at their
  defaults.
- **`runWizard`** — driven with a scripted stdin reader + captured stdout and
  the infra/credential steps stubbed (injected funcs), asserting the flow
  selects the right config and prints the expected summary; subscription
  bootstrap and "not logged in" branches both covered.
- **Vendor-CLI / live auth** — leans on the existing `drydock auth` parser tests
  plus a manual first-run on a real machine before release.

## Out of scope

- A full-screen / arrow-key TUI (explicitly rejected — dependency cost).
- Persisting API keys anywhere (explicitly rejected — never to disk).
- Prompting for egress / budget / model / timeouts (template defaults; advanced
  users edit `config.yaml`).
- Changing `drydock init`'s non-interactive behavior or `drydock auth`'s CLI
  surface (the wizard reuses their cores).
- Installing the vendor CLIs (`claude` / `codex`) or running their `login` flows
  for the user (the wizard guides; the user runs the vendor login).
