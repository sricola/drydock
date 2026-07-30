# Configuration

`drydock init` creates `~/.drydock/` (mode `0700`) and seeds two files:

| Path | What |
|---|---|
| `~/.drydock/config.yaml` | Operator settings (network, gateway IP, budget, timeout, concurrency, paths, listener, behavior flags) |
| `~/.drydock/egress.yaml` | The allowlist: hosts and ports the sandbox may reach (see [Egress](egress.html)) |

Both are seeded from defaults the first time; `drydock init` never overwrites
them. **Env vars win over file values**, so existing scripts keep working. Edit
`config.yaml` and re-run `drydock start`.

The vendor keys (`ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `GEMINI_API_KEY`) are
intentionally **not** in these files; they live in your shell env, or at
`~/.drydock/api-keys.env` (mode `0600`), read host-side and never passed into
the VM. All three keys are recognized automatically; no extra config is needed
to declare them.

## Common settings

| Field (`config.yaml`) | Env override | Default | Meaning |
|---|---|---|---|
| `anthropic_auth` | `DRYDOCK_ANTHROPIC_AUTH` | `api_key` | `api_key` uses `ANTHROPIC_API_KEY`; `subscription` uses `~/.drydock/claude-oauth.json` |
| `openai_auth` | `DRYDOCK_OPENAI_AUTH` | `api_key` | `api_key` uses `OPENAI_API_KEY`; `subscription` uses `~/.drydock/codex-oauth.json` |
| `default_agent` | `DRYDOCK_DEFAULT_AGENT` | `claude` | Agent when `--agent` is omitted (`claude` \| `codex` \| `gemini` \| `opencode`) |
| `default_model` | `DRYDOCK_DEFAULT_MODEL` | *(empty)* | `--model` fallback for **Claude Code and Codex only**; empty = the agent picks. Not applied to `gemini` (uses its own `gemini-2.5-pro` default) or `opencode` (uses `openai_compat.model`). |
| `task_budget_usd` | `DRYDOCK_TASK_BUDGET_USD` | `2.0` | Per-task USD soft cap, metered post-hoc; overshoot bounded to `task_max_inflight` in-flight requests (default 1); set `max_request_cost_usd` for a reservation-backed bound (`api_key` mode only; unused in subscription mode) |
| `task_max_inflight` | `DRYDOCK_TASK_MAX_INFLIGHT` | `1` | Concurrent gateway requests admitted per task lease; bounds budget overshoot to this many in-flight requests; `0` disables the cap |
| `max_request_cost_usd` | `DRYDOCK_MAX_REQUEST_COST_USD` | `0` (disabled) | Worst-case USD reserved per in-flight request so concurrent requests cannot admit past the budget; `0` disables (post-hoc metering only) |
| `task_max_requests` | `DRYDOCK_TASK_MAX_REQUESTS` | `0` (falls closed to a built-in default of 1000) | Hard cap on API round-trips per task; the primary runaway control in subscription mode |
| `aggregate_budget_usd` | `DRYDOCK_AGGREGATE_BUDGET_USD` | `0` (disabled) | Cross-task USD ceiling per `api_key` provider over `aggregate_window`; `0` disables the cap; subscription mode is out of scope (bounded per-task by `task_max_requests`) |
| `aggregate_window` | `DRYDOCK_AGGREGATE_WINDOW` | `24h` | Rolling window for the aggregate cap; `0` = total since brokerd boot, resets on restart |
| `task_timeout` | n/a | `30m` | Wall-clock per task |
| `approval_timeout` | n/a | `0s` | Auto-deny a task left at an approval gate after this long; `0` = wait forever (right for interactive use; set for unattended runs) |
| `max_concurrent_tasks` | `DRYDOCK_MAX_CONCURRENT_TASKS` | `2` | Excess POSTs to `/tasks` get HTTP 503 |
| `notifications` | `DRYDOCK_NO_NOTIFY=1` (off) | `true` | macOS notifications on pending approval |
| `push_max_retries` | `DRYDOCK_PUSH_MAX_RETRIES` | `3` | Transient push failures (network errors) to retry with exponential backoff before giving up; `0` disables transient retry |
| `push_retry_backoff` | `DRYDOCK_PUSH_RETRY_BACKOFF` | `1s` | Base delay for push retry backoff (`backoff * 2^n`); `0` disables the delay between retries |
| `push_fresh_branch_tries` | `DRYDOCK_PUSH_FRESH_BRANCH_TRIES` | `2` | Alternate remote branch names (`agent/<id>-2`, `-3`, ...) to try when a branch-name collision is detected; `0` disables fresh-branch recovery |

## Diff policy: caps, blocked paths, second-look

The optional `diff_policy` block in `config.yaml` constrains the diff a task
may propose. It is enforced host-side by brokerd against the broker-computed
diff facts (the same analysis the trust brief reports), so nothing the agent
prints can influence it. All checks are off by default (zero values / empty
lists).

```yaml
diff_policy:
  max_files_changed: 0        # fail closed when the diff changes more files than this (0 = no cap)
  max_lines_changed: 0        # fail closed when added+deleted lines exceed this (0 = no cap)
  blocked_paths: []           # e.g. ["**/*.pem", ".github/workflows/**"] — touching one fails the task
  second_look_paths: []       # e.g. ["**/Dockerfile"] — approver must acknowledge each flagged category
```

Two distinct mechanisms:

- **Caps and blocked paths fail closed, before review.** A diff that exceeds
  `max_files_changed` / `max_lines_changed`, or touches any `blocked_paths`
  pattern, ends the task with outcome `policy_blocked` **before it reaches
  the approval gate** — there is nothing to approve, and `--auto-approve`
  does not bypass it. The captured diff and trust brief are preserved in the
  audit dir for inspection. A diff too large to fully analyze (files omitted
  past the tracking bound) also fails closed whenever a content-based policy
  (`blocked_paths` / `max_lines_changed`) is configured, rather than letting
  unanalyzed files slip past it.
- **Second-look paths still reach review, but approving requires
  acknowledgment.** When a diff touches a `second_look_paths` pattern, the
  broker computes the affected risk-flag categories (the trust brief's FLAG
  kinds, e.g. `ci-workflow`, `lockfile`, `exec-bit`) and refuses any approve
  that does not explicitly acknowledge every one of them: `drydock approve
  <id> --acknowledge <category>` (repeatable; alias `--ack`), the per-category
  prompts in `drydock review`, or the checkboxes in the web UI. An approve
  with missing acknowledgments is refused (HTTP 422 naming the missing
  categories) and **the task stays pending** — there is no path on which an
  under-acknowledged approve pushes. Acknowledgments are a human-gate
  feature: `--auto-approve` tasks skip the gate entirely, so
  `second_look_paths` does not apply to them — anything that must be
  impossible to push belongs in `blocked_paths`.

Path patterns are `**`-aware repo-relative globs: `*` matches within a path
segment, `**` crosses segments. Write `dir/**` to cover everything under
`dir` — a trailing-slash pattern like `dir/` matches nothing and is rejected
at config load. There is no env override for this block; configure it in
`config.yaml` (it participates in `drydock policy explain`'s divergence
check like any other field).

## Execution profiles: setup and readiness (per repo)

The optional `profiles` block in `config.yaml` gives a repository a **setup
phase**: commands drydock runs against the task's live work tree **before
the agent starts** — dependency install, code generation — plus readiness
commands that gate the run. Keys must be the canonical `host/owner/repo`
form (a non-canonical key is a config error, not a silent never-match):

```yaml
profiles:
  repos:
    "github.com/you/yourrepo":
      setup:
        - ["npm", "ci"]
      readiness:
        - ["node", "--version"]
      timeout: 10m      # per command; 0 = the default (10m)
```

What the phase guarantees:

- **Host config only.** The commands live in this file on your machine; the
  sandboxed agent can never edit its own setup phase.
- **Fail closed before any API spend.** Setup and readiness run before the
  agent VM boots. Any command that fails, times out, or errors ends the task
  with outcome `setup_failed` — the agent VM never boots, no credential is
  ever injected into any VM, and zero API budget is spent.
- **No credentials, egress-only network.** Setup VMs get the squid egress
  proxy (so `npm ci` can reach your allowlisted registries) but no model
  gateway and no API bearer.
- **Same workspace the agent gets.** Commands run in fresh sandbox VMs (the
  agent's image) against the live per-task work tree, so what setup installs
  is exactly what the agent sees. The per-task stage is wiped at cleanup —
  there is **no persistent cache yet**; setup runs from scratch each task.
- **Self-contained commands.** Each command is its own argv in its own VM
  run: shell state (`cd`, exported variables, activated virtualenvs) does
  **not** carry between commands. Write each entry to stand alone.

Verdicts are the process exit codes the broker observes — nothing a command
prints can flip a status. The task shows a `setting_up` stage while the
phase runs, and the per-command evidence (status, exit codes, durations)
lands in the trust brief (`drydock inspect <id>`); the combined output is
kept display-only at `~/.drydock/audit/<id>.setup.log`. See
[Submitting tasks](submitting-tasks.html#execution-profiles-setup-per-repo)
for the full behavior.

## Bring your own model

`opencode` reaches any OpenAI-compatible endpoint via the `openai_compat` block
in `config.yaml` (or the `drydock setup` wizard). There is **no env override**;
configure it in the file. The real key is referenced by env-var **name**, never
stored here.

| Key (under `openai_compat:`) | Meaning |
|---|---|
| `base_url` | Endpoint host, e.g. `https://generativelanguage.googleapis.com` (empty = disabled; https, or http only for `localhost`) |
| `base_path` | Path joined onto the request, e.g. `/v1beta/openai` |
| `api_key_env` | **Name** of the host env var holding the real key (e.g. `GEMINI_API_KEY`) |
| `model` | Model id passed to the agent, e.g. `gemini-2.5-pro` |
| `prices` | Optional `{<model>: {input, output}}` USD per 1M tokens; enables USD budgeting, omit to rely on `task_max_requests` |

**Streaming and USD metering.** Streaming `chat/completions` responses commonly
omit token usage unless the client explicitly requests it (via
`stream_options.include_usage`). drydock does not inject that option, so a
*streamed* task against a priced `openai_compat` endpoint may be metered at $0
against `task_budget_usd`: the response completes but carries no usage to bill.
The usage-independent backstop is `task_max_requests`: it counts every API
round-trip regardless of whether the upstream reports usage. Set
`task_max_requests` for any `openai_compat` lane where streaming is expected.

**`prices` and the `"default"` row.** The `prices` map is keyed by model id. If
a task uses a model not explicitly listed and no `"default"` row exists, drydock
has no price to apply and meters that call at $0, so the USD budget will never
trip for that model. Add a `"default"` entry to catch unlisted models:

```yaml
openai_compat:
  prices:
    my-model: {input: 1.00, output: 3.00}
    default:  {input: 1.00, output: 3.00}  # fallback for any unlisted model
```

See [Bring your own model](models.html) for worked examples.

## Native Gemini

`--agent gemini` (`default_agent: gemini`) uses Google's native Gemini API
directly. No `openai_compat:` block is needed; just set `GEMINI_API_KEY` in
your env or `~/.drydock/api-keys.env`. `GEMINI_API_KEY` is a recognized key
automatically. There is no subscription mode for Gemini; API key is the only
auth path.

```yaml
default_agent: gemini          # make Gemini the default (defaults to gemini-2.5-pro)
```

`default_model` does not affect Gemini; pick a non-default Gemini model per task
with `--model gemini-2.5-flash`.

## Advanced: runtime, paths, listener

| Field (`config.yaml`) | Env override | Default | Meaning |
|---|---|---|---|
| `network` | `DRYDOCK_NETWORK` | `drydock-egress` | vmnet network name |
| `gateway_ip` | `DRYDOCK_GW_IP` | `192.168.66.1` | Gateway + squid bind here |
| `sandbox_image` | `SANDBOX_IMAGE` | `drydock-sandbox:latest` | Per-task agent VM image |
| `anchor_image` | `DRYDOCK_ANCHOR_IMAGE` | `drydock-anchor:latest` | Minimal image holding the vmnet gateway IP |
| `stage_root` / `audit_root` / `squid_run_dir` | `STAGE_ROOT` / `AUDIT_ROOT` / `SQUID_RUN_DIR` | `~/.drydock/{stage,audit,squid}` | Per-task scratch (audit dir `0700`; log + diff `0600`) |
| `broker.socket` | `BROKER_SOCKET` | `$TMPDIR/drydock-$UID/drydock.sock` | Unix socket (parent dir `0700`, socket `0600`) |
| `broker.addr` | `BROKER_ADDR` | *(empty)* | `host:port` to expose over TCP (**no auth**; see [SECURITY.md § TCP exposure](https://github.com/sricola/drydock/blob/main/SECURITY.md#tcp-exposure-brokeraddr--broker_addr)) |
| `log_json` | `DRYDOCK_LOG_JSON=1` | `false` | Force JSON logs even on a TTY |
| `strict_container_version` | `DRYDOCK_STRICT_CONTAINER_VERSION=1` | `false` | Fail closed when `container`'s major drifts from the tested range |
| n/a | `EGRESS_CONFIG` | `~/.drydock/egress.yaml` | Path override for the egress YAML |

Gateway port `8088` and squid port `3128` are hard-coded in
`cmd/brokerd/main.go` and `image/entrypoint.sh`; change both together.

## Inspecting the effective policy

With env vars layered over `config.yaml` layered over built-in defaults, it is
easy to lose track of which value actually won. `drydock policy explain`
resolves the config exactly like `drydock start` does and prints every setting
with the layer that supplied it:

```text
SETTING          VALUE             SOURCE
Network          drydock-egress    default
GatewayIP        192.168.66.1      default
TaskBudgetUSD    5                 config.yaml
MaxConcurrent    4                 env:DRYDOCK_MAX_CONCURRENT_TASKS
...

daemon: in sync — the running brokerd resolved this same policy
```

The `SOURCE` column is one of three values, resolved per field (env >
`config.yaml` > default):

- `default` — the built-in default is in effect.
- `config.yaml` — the file set this field to a non-default value.
- `env:VAR` — the named env var overrode it (the row tells you exactly what
  to unset).

**The daemon verdict.** brokerd resolves its policy once, at boot. When it is
reachable, `policy explain` compares your shell's resolution against the
daemon's:

- `in sync` — the running daemon resolved this same policy.
- `DIVERGENT` — the daemon is running an older resolution (you edited
  `config.yaml` or changed env since it started); the differing fields are
  listed as `local=... live=...`. Restart brokerd to pick up the changes.
- `LIVE POLICY UNVERIFIED` — brokerd isn't reachable; only the local table is
  shown, and no sync claim is made.

`--json` emits the machine-readable form: `{"local": {fields, hash}, "live":
{fields, hash} | null, "in_sync": bool | null}` (`live`/`in_sync` are `null`
when the daemon could not be asked). Both `hash` values cover only the divergence-comparison subset (connection fields `broker.socket` / `broker.addr` are excluded), so clients should compare the two `hash` values to each other or check `in_sync` rather than recomputing a hash over the full `fields` list.

Two semantics worth knowing:

- **Source attribution is value-based.** A `config.yaml` line that sets a
  field to its built-in default shows as source `default` — the layers agree,
  so the lowest one is credited. Only a non-default file value reads
  `config.yaml`.
- **Connection settings don't count as divergence.** `broker.socket` /
  `broker.addr` (`BROKER_SOCKET` / `BROKER_ADDR`) describe how the CLI
  *reaches* the daemon, not policy the daemon enforces. They appear in the
  table like any other field but are excluded from the in-sync/DIVERGENT
  verdict, so dialing a specific daemon via `BROKER_ADDR` doesn't trip a
  spurious divergence.
