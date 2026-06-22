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
2. **API-key handling:** the wizard can persist the key host-side, but only with
   **explicit consent** — never as a silent side-effect. Today's README repeats
   "API keys never go to disk" as a selling point, so a silent write would
   change a documented property out from under a user who chose drydock partly
   for it. The API-key step prompts: *"store this key at `~/.drydock/api-keys.env`
   (0600) so the broker finds it across shells? [Y/n] — or keep it in your shell
   env only."* Storing is the recommended default (better UX; fixes the
   broker-can't-see-the-shell-env-key class of bug), but **env-only is a
   first-class, preserved path**. The file is a **dedicated 0600 file**, NOT
   `config.yaml`. The broker loads it at start; an exported
   `ANTHROPIC_API_KEY`/`OPENAI_API_KEY` still overrides it (CI/advanced).

   Honest security framing: the load-bearing, unchanged invariant is **the
   credential never enters the VM** (the A1 red-team test). On-disk exposure is
   **comparable** to the OAuth token already at 0600 — both are long-lived
   host-side secrets; they differ in how each is revoked (API key in the vendor
   console / by rotation; OAuth token by re-login). It is **not** "strictly
   less" exposure: a raw `ANTHROPIC_API_KEY` is often org-wide and doesn't
   auto-expire, and SECURITY.md already notes it isn't per-task-revocable.
   README/SECURITY/THREAT_MODEL retire "API keys never go to disk" and reframe
   to: credentials stay host-side (env or `~/.drydock/` 0600) and **never enter
   the VM**; the wizard *offers* to persist the key, and you may decline to keep
   it env-only.
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
| `cmd/drydock/wizard.go` (new) | The `configure` orchestration (`runWizard`) + **pure** prompt helpers: `promptChoice(in io.Reader, out io.Writer, q string, opts []string, dflt int) int`, `promptYesNo(in, out, q string, dflt bool) bool`, `promptSecret(...)` (reads the API key, echo disabled when stdin is a TTY via stdlib termios — macOS-only, **no new dependency**). Helpers read/write injected streams so they're unit-testable. |
| `renderConfig(choices) string` (new, in wizard.go or config seam) | Renders the full comment-rich `config.yaml` body with the chosen `default_agent`/`anthropic_auth`/`openai_auth`; all other keys keep the existing template defaults. |
| API-key store (new) | `~/.drydock/api-keys.env` — a 0600 `KEY=value` file. A small **loader** (in `internal/config` or a tiny `creds` helper): `LoadAPIKeys(path) map[string]string`, parsing `ANTHROPIC_API_KEY`/`OPENAI_API_KEY` lines (ignore blanks/`#` comments). A **writer** the wizard uses to upsert a single key at 0600 (atomic temp+rename), preserving any other key already in the file. |
| `cmd/brokerd/main.go` (modify) | Before reading the API keys, load `~/.drydock/api-keys.env` as defaults; an exported env var overrides a file value. The downstream backend wiring is unchanged (`StaticKey(key)` → gateway). |
| `runSetup` (modify) | Decide interactive vs non-interactive (TTY + first-run / `--reconfigure`); run prereqs + infra; on interactive, call `runWizard`; else today's path. |
| auth cores (modify) | Extract the callable core of `drydock auth claude` / `auth codex` into functions the wizard can invoke and branch on (e.g. `bootstrapClaudeCred() error`, `bootstrapCodexCred() error`). The existing `drydock auth claude|codex` subcommands call the same core. |
| `cmd/drydock/doctor.go` (modify) | Report the active API-key source per vendor (`env` / `~/.drydock/api-keys.env` / `none`), so the key's provenance is never a silent surprise. |
| docs (modify) | `README.md` / `SECURITY.md` / `THREAT_MODEL.md`: retire "API keys never go to disk"; reframe to "credentials stay host-side (env or `~/.drydock/` 0600), never in the VM"; document `~/.drydock/api-keys.env` and that the wizard *offers* (consented) to persist the key. |

**Implementation sequencing (two stages).** The security-reviewable core — the
`api-keys.env` store + `LoadAPIKeys` + the brokerd precedence + the
SECURITY/THREAT_MODEL/README reframe — is independently testable and shippable,
and is where the real security review belongs. Build and ship it **first**
(stage 1); the wizard (stage 2) is UX that consumes it. This de-risks the
posture change (it gets reviewed on its own) and unblocks the wizard from the
docs discussion. The implementation plan should reflect these as two stages
(and likely two PRs).

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
   - **API key:** prompt for consent — *"store at `~/.drydock/api-keys.env`
     (0600) so the broker finds it across shells? [Y/n] — or keep it env-only."*
     If yes: if the env var is already exported, persist that value (no
     re-entry — avoids the key landing in shell scrollback); else read it with
     `promptSecret` (no terminal echo) and write it. The key is upserted (any
     other key in the file is preserved); the value is never printed. If the
     user declines (env-only): if the env var is set → `✓`; else print the exact
     `export ANTHROPIC_API_KEY=…` reminder. Nothing is written to disk.
4. **Write config.** Render `~/.drydock/config.yaml` with the choices. If a
   config already exists and the user is reconfiguring, overwrite; the egress
   file is left untouched (seeded by init as today).
5. **Finish.** Print a one-line summary of what was written, then
   `start: drydock start` and a `drydock submit` example. Recommend
   `drydock doctor` to verify.

## API-key store (`~/.drydock/api-keys.env`)

A dedicated secret file, deliberately separate from `config.yaml`:

- **Format:** plain `KEY=value` lines, e.g. `ANTHROPIC_API_KEY=sk-ant-…` /
  `OPENAI_API_KEY=sk-…`. Blank lines and `#` comments ignored. Only the two
  vendor keys are recognized.
- **Mode:** `0600`, written atomically (temp + rename), like the OAuth json.
- **Precedence (broker):** load the file first as defaults; a **non-empty**
  exported env var of the same name overrides it. An env var set to `""` does
  NOT override a good file value (it falls through to the file), symmetric with
  ignoring a blank line in the file. So scripted/CI use (`export …`) is
  unchanged, and an interactive operator gets persistence for free.
- **Visibility:** `drydock doctor` reports which source is active per vendor —
  `key: env` / `key: ~/.drydock/api-keys.env` / `key: none` — so a stale file or
  an unset env never causes silent surprise.
- **Security:** the broker reads it host-side and still mints a per-task token —
  the raw key never enters the VM (A1 invariant unchanged). It is NOT in
  `config.yaml` (the file people edit/screenshot/paste into issues). It is the
  api_key-mode peer of the OAuth-token json files; both live host-side at 0600.

## Error handling

- **Invalid prompt input** (out-of-range / non-numeric) → re-prompt with a short
  "enter 1–N"; empty input → the default.
- **Not a TTY** → never enter the wizard (the gate); EOF on stdin can't hang it.
- **Credential not ready** (subscription CLI missing / not logged in; or the
  user skips pasting an API key) → clear, **non-fatal** guidance; the config is
  still written so the user can finish out-of-band (`claude login` /
  `drydock auth claude`, or add the key to `~/.drydock/api-keys.env`) and
  `drydock start`.
- **Config / api-keys.env write failure** (permissions) → surfaced via
  `step(..., false, err)` and a non-zero exit, consistent with the rest of
  `init`. `promptSecret` reads with terminal echo disabled when stdin is a TTY;
  on a non-TTY it never runs (the wizard gate).

## Testing

- **`promptChoice` / `promptYesNo`** — table tests over an `io.Reader` of
  scripted input: valid selection, Enter→default, invalid-then-valid retry,
  out-of-range rejection.
- **`renderConfig`** — for each agent×auth combination, assert the rendered
  `config.yaml` parses (via `config.Load` on a temp file) and carries the right
  `default_agent` / `anthropic_auth` / `openai_auth`, with other keys at their
  defaults.
- **API-key store** — `LoadAPIKeys` parses valid lines / ignores blanks+comments;
  the writer upserts one key while preserving the other and writes 0600 (mode
  asserted). Brokerd precedence: a non-empty env var overrides the file; an env
  var set to `""` falls through to the file value; the file value is used when
  env is unset. Doctor source-reporting: returns `env` / file-path / `none` for
  each of the three states.
- **`runWizard`** — driven with a scripted stdin reader + captured stdout and
  the infra/credential steps stubbed (injected funcs), asserting the flow
  selects the right config and prints the expected summary; subscription
  bootstrap and "not logged in" branches both covered.
- **Vendor-CLI / live auth** — leans on the existing `drydock auth` parser tests
  plus a manual first-run on a real machine before release.

## Out of scope

- A full-screen / arrow-key TUI (explicitly rejected — dependency cost).
- Putting secrets in `config.yaml` (the API key goes in the separate
  `~/.drydock/api-keys.env`; OAuth tokens keep their own json files).
- Prompting for egress / budget / model / timeouts (template defaults; advanced
  users edit `config.yaml`).
- Changing `drydock init`'s non-interactive behavior or `drydock auth`'s CLI
  surface (the wizard reuses their cores).
- Installing the vendor CLIs (`claude` / `codex`) or running their `login` flows
  for the user (the wizard guides; the user runs the vendor login).
