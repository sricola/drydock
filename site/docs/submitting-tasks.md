# Submitting tasks

A task is one unit of work: a repo, an instruction, and an agent. drydock clones
the repo into a throwaway VM, runs the agent, captures a `git diff`, and waits
for your approval before anything reaches origin.

## The basic flow

In one shell, fire the task. **It blocks until the agent runs and you approve
the diff** (typically a few seconds to a few minutes, plus your review time):

```bash
drydock submit \
  --repo git@github.com:your-org/your-repo \
  --instruction "Add a one-line comment to README.md explaining the project."
```

A macOS notification fires when the diff is ready (opt out with
`DRYDOCK_NO_NOTIFY=1`).

Before the first submit against an unfamiliar repo, `drydock doctor --repo
<path>` preflights a local checkout with no API spend and no container boot:
size vs the stage cap, toolchains vs what the image ships, registry hosts vs
your egress allowlist, and `diff_policy` collisions (see
[Troubleshooting](troubleshooting.html#check-a-repo-before-submitting)).

## From a GitHub issue

A GitHub issue can be the task instruction directly — no copy-pasting:

```bash
# 1. Plan first: the agent produces an implementation plan, changes nothing.
drydock submit --issue https://github.com/o/r/issues/42 --plan
#    (sugar: drydock plan https://github.com/o/r/issues/42)

# 2. Review the plan.
drydock inspect <id>            # renders the plan inline
less ~/.drydock/audit/<id>.plan.md

# 3. Happy with the plan? Run the same issue for real.
drydock submit --issue https://github.com/o/r/issues/42
```

`--issue` fetches the issue **host-side** via your authenticated `gh` (with a
curated environment, not a full copy of yours) and derives `--repo` from the
URL when you omit it. Your GitHub credentials never enter the sandbox — only
the issue *text* does, folded into the prompt with the title, labels, and a
size-capped body.

Treat that text as untrusted input: anyone who can write an issue on the repo
is now writing instructions to your agent, exactly like a hostile
`AGENTS.md` already could (see the
[threat model](https://github.com/sricola/drydock/blob/main/THREAT_MODEL.md)).
The human gates are the boundary — the plan you review, and the diff gate
every implementing run still passes through.

A `--plan` run is scope-gated in the broker, not just prompted: it terminates
with outcome `planned` **before** the verify/push logic, so nothing is ever
pushed from a plan run regardless of what the agent did in its VM. Any stray
diff is still captured beside the audit log for your review, and
`drydock retry <id>` of a plan run re-plans (it never silently escalates to
an implementing run).

## Queue (durable, unattended)

`drydock submit` blocks your shell until the task finishes (and until you
approve the diff). `drydock queue add` takes the **same flags** — `--issue`,
`--plan`, `--instruction-file`, `--egress-extra`, all of it — but returns the
moment brokerd has durably persisted the task:

```bash
drydock queue add --repo git@github.com:o/r --instruction "fix the flaky test"
drydock queue add --issue https://github.com/o/r/issues/42 --auto-approve

drydock queue list           # ID / STATE / AGE / ATTEMPTS / REPO
drydock queue cancel <id>    # dequeue a waiting item, or kill it if running
```

Each item moves through a broker-enforced state machine:

```
queued → preparing → running → verifying → awaiting_review → completed
                                                           ↘ dead_letter
        (cancelled is reachable from every non-terminal state)
```

- **Same lifecycle, same gates.** A dispatched item runs the exact code path
  a synchronous submit runs — setup profiles, verification, the diff-approval
  gate (unless `--auto-approve`), diff policy. Nothing is weaker because it
  was queued.
- **Shared concurrency cap.** Queued and synchronous tasks draw from the one
  `max_concurrent` pool; the dispatcher never over-commits it.
- **Spend-ceiling parking.** When a vendor's aggregate spend cap is
  exhausted, that vendor's items stay `queued` — parked, not failed, and a
  park never counts as an attempt. They dispatch when the spend window
  slides.
- **Survives restarts.** Queue items are atomically-written files in the
  audit dir. At boot, brokerd re-dispatches anything still `queued`, resumes
  items parked at the approval gate, and **never re-runs** an item that was
  mid-flight when the daemon died — an interrupted `running`/`verifying`
  item is dead-lettered (with its audit trail intact) rather than executed a
  second time.
- **Terminals.** A clean finish (pushed, no-diff, or a plan) lands
  `completed`; any failure lands `dead_letter` with the reason in
  `last_error`. Terminal items stay in `drydock queue list` as history.

A queued task's terminal metrics row records the wait as `stage_ms.queued`
(enqueue → dispatch); `drydock stats` aggregates it as the queue-wait
p50/p95 line. Synchronous tasks omit the field entirely.

## Push-credential preflight

Before the sandbox boots, drydock runs `git push --dry-run` against your repo
with the exact branch it would later push (`agent/<id>`). A dry-run push
authenticates and computes ref updates without sending objects or moving
refs: one remote round trip, before the sandbox boots. A missing local
credential (no helper, no key, no agent) fails even faster, in milliseconds,
without touching the network at all. Either way, if it fails, the task ends
immediately with a classified reason and nothing spent: no VM, no agent run.

The consequence is plain: a task against a repo you cannot push to fails at
submit, not after a long agent run. There is no opt-out.

git never prompts during this check. HTTPS needs a credential helper
(`gh auth setup-git`); SSH runs in BatchMode unless you've set
`GIT_SSH_COMMAND` yourself.

## The approval gate

Nothing reaches your real repo until you say so. Review and act on a pending
task from any shell:

```bash
drydock pending            # tasks awaiting you (egress + diff gates both shown)
drydock review <id>        # diff in $PAGER, then prompt y/N (the one-shot path)
# … or step by step:
less ~/.drydock/audit/<id>.diff
drydock approve <id>       # … or: drydock deny <id>
```

`drydock review` is the fast path; `approve` / `deny` are the explicit controls.
A denied task keeps its diff in the audit dir but never pushes.

### Diff policy: caps and second-look acknowledgments

The optional `diff_policy` block in `config.yaml` (see
[Configuration](configuration.html)) shapes what reaches this gate:

- **Caps and blocked paths** (`max_files_changed`, `max_lines_changed`,
  `blocked_paths`): a violating diff fails the task closed with outcome
  `policy_blocked` **before review** — nothing to approve, and
  `--auto-approve` does not bypass it. `drydock tasks` / `drydock stats`
  show it as **policy blocked**; the diff and trust brief stay in the audit
  dir.
- **Second-look paths** (`second_look_paths`): the diff still reaches this
  gate, but the broker computes the touched risk-flag categories (the trust
  brief's FLAG kinds, e.g. `ci-workflow`, `lockfile`) and refuses any
  approve that doesn't acknowledge all of them. `drydock pending` marks such
  tasks with `SECOND-LOOK[...]`; `drydock review` prompts
  `acknowledge <category> change? [y/N]` per category after the diff (any
  refusal denies); the explicit path is:

  ```bash
  drydock approve <id> --acknowledge ci-workflow --acknowledge lockfile
  ```

  (`--ack` is an alias; repeat per category.) An approve with missing
  acknowledgments is refused — the CLI prints the missing categories and the
  corrected command, and the task stays pending. In the web UI the review
  overlay renders a checkbox per required category and keeps the Approve
  button disabled until every box is checked. The refusal is enforced by
  brokerd itself, so no client can approve an under-acknowledged diff.

If the branch pushes but the PR can't be opened (e.g. `gh` isn't authenticated),
drydock reports it as **pushed** with a hint to open the PR manually; it never
loses your work to a failed PR step.

## Execution profiles (setup, per repo)

Some repos need preparation before an agent can work: dependencies
installed, code generated. An **execution profile** in
`~/.drydock/config.yaml` (see
[Configuration](configuration.html#execution-profiles-setup-and-readiness-per-repo))
gives a repo `setup` commands plus `readiness` commands that gate the run:

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

The commands run in the sandbox against the task's **live work tree, before
the agent starts** — what setup installs is exactly what the agent sees.
They live in host config only, so the sandboxed agent can never edit its own
setup phase. Setup VMs get egress through the squid proxy (registries on
your allowlist are reachable) but **no model gateway and no credentials**.
Repos that opt in with `cache: true` (see
[Configuration](configuration.html#persistent-dependency-cache-opt-in-per-repo))
reuse dependency downloads across tasks: setup VMs populate a host-side,
content-addressed cache mounted at `/deps` — read-write in setup, strictly
**read-only** in the agent VM, keyed per repo + lockfile (no cross-repo
sharing, and no cache at all for a repo without a lockfile). Without the
opt-in the per-task workspace is wiped at cleanup and setup runs from
scratch on every task. Each command is a
self-contained argv in its own VM run — shell state (`cd`, exports,
virtualenv activation) does not carry between commands.

Setup is always enforced; there is no advisory mode. Verdicts are the exit
codes the broker observes (nothing a command prints can flip a status);
commands run in order and stop at the first non-pass (later ones record
`skipped`). Any failure, timeout, or infrastructure error **fails the task
closed before any API spend**: the terminal outcome is `setup_failed`, the
agent VM never boots, and no credential is ever injected into any VM — a
broken workspace costs you $0 of model budget.

While it runs, the task shows a `setting_up` stage — between `preparing` and
`running` — in the submit stream, `drydock status`, and the web UI. The
evidence lands in the trust brief (`drydock inspect <id>`): overall status,
the setup VMs' egress posture, the dependency-cache line when the repo opts
in (`hit`/`miss`/`disabled: no lockfile` plus the cache entry's key prefix
— all broker-observed), per-command exit codes and durations. The
commands' combined output is kept (display-only, size-capped, never parsed)
at `~/.drydock/audit/<id>.setup.log`.

## Verification (optional, per repo)

drydock can run your own checks — build, tests, lint — against the agent's
work before the diff reaches the approval gate. Configure them per repository
in `~/.drydock/config.yaml`; keys must be the canonical `host/owner/repo`
form (a non-canonical key is a config error, not a silent never-match):

```yaml
verify:
  repos:
    "github.com/you/yourrepo":
      commands:
        - ["go", "build", "./..."]
        - ["go", "test", "./..."]
      timeout: 10m      # per command; 0 = the default (10m)
      required: false   # true = anything but "passed" blocks the push
```

Each command gets exactly this, no more:

- **A fresh VM per command** (`--rm`), separate from the agent's VM.
- **A sealed export of the staged tree** mounted at `/work`: the same tree
  whose diff you review. Commands may write to their copy; nothing they do
  changes what would be pushed, and at push time drydock re-hashes the
  staged tree and refuses to push unless it still equals the verified tree.
- **No network**: root installs a deny-all firewall pin (loopback only —
  strictly tighter than the agent's allowlist; not even the egress proxy is
  reachable) before dropping to the unprivileged agent user via the same
  mechanism the agent VM uses, so repo code cannot remove the pin.
- **No credentials**: no gateway lease, no API keys, no push credentials.

Verdicts are the process exit codes the broker observes from each VM —
nothing a command prints can change a status. Commands run in order and stop
at the first non-pass (later ones record `skipped`). The overall status is
`passed` only when every command passed; a timeout or infrastructure error
makes it `inconclusive`, which is never treated as a pass.

- `required: false` (the default): verification is **advisory**. Results
  land in the trust brief for your review, and the diff still reaches the
  approval gate whatever the status.
- `required: true`: **fail closed**. Any status but `passed` (including
  `inconclusive`) ends the task with outcome `verify_failed` before the
  approval gate, and nothing is pushed. The evidence survives: the trust
  brief (`drydock inspect <id>`), the captured diff (`<id>.diff`), and — when
  any verify VM actually ran — the verification log are all still persisted.

While it runs, the task shows a `verifying` stage — between `running` and
`awaiting_approval` — in the submit stream, `drydock status`, and the web
UI. Review the evidence with `drydock inspect <id>`: overall status, the
VM's capability posture, per-command exit codes and durations, and the
verified tree hash. The commands' combined output is kept (display-only,
size-capped, never parsed) at `~/.drydock/audit/<id>.verify.log`.

## Push outcomes

After you approve a diff, drydock pushes the single commit to a new remote
branch (`agent/<id>`) and optionally opens a PR. A single-ref `git push` is
atomic: the remote branch either receives the whole commit or is left unchanged.
`push_failed` therefore guarantees nothing landed on the remote for that task,
and the captured diff is always preserved in the audit `.diff` file for every
outcome that produced a diff — `verify_failed` included; only tasks that never
captured one (`no_diff`, or a failure before diff capture) have no `.diff`.

drydock classifies push failures and recovers from the recoverable ones:

- **Transient** (network errors: timeouts, connection resets, TLS errors): retried
  with exponential backoff, up to `push_max_retries` attempts (default `3`; `0`
  disables). Exhausted transient retries result in `push_failed`.
- **Branch-name collision** (`non_fast_forward` on the initial push): retried to a
  fresh remote name (`agent/<id>-2`, `-3`, ...), up to `push_fresh_branch_tries`
  alternates (default `2`; `0` disables). On success, the `pushed` event reports
  the actual branch used.
- **Auth / protected / unknown**: stopped immediately; no retry.

When every recovery path is exhausted, drydock reports `outcome=push_failed` with
the classified reason (`transient`, `auth`, `protected`, `non_fast_forward`, or
`unknown`). The `drydock tasks` row shows **push failed** for that task.

`push_failed` is retry-safe: `drydock retry <id>` re-runs the task under a new
id (a new `agent/<newid>` branch), so a retry never collides with the failed
attempt.

Not every task reaches a push attempt. A diff denied at the approval gate
reports `outcome=denied` and never pushes; a task killed mid-run reports
`outcome=cancelled`; a run that produces no diff reports `outcome=no_diff`
and still displays as `ok` (a clean no-op isn't a failure); a task whose
execution profile does not pass reports `outcome=setup_failed` before the
agent ever runs (no diff exists — nothing ran to produce one); a task whose
**required** verification does not pass reports `outcome=verify_failed` and
never reaches the approval gate; a diff that violates `diff_policy` (caps or
`blocked_paths`) reports `outcome=policy_blocked` — shown as **policy
blocked** — and likewise never reaches the gate. `drydock tasks`, `drydock
stats`, and the web UI all show these outcomes distinctly from `pushed` and
`push_failed`.

## Operator surface

```bash
drydock status             # brokerd up?, breakdown (setting up · running · verifying · egress · diff · pushing)
drydock inspect <id>       # trust brief: broker-observed evidence incl. setup + verification
drydock tasks              # recent runs: id, age, duration, cost, outcome
drydock logs <id> [-f]     # stream-json audit (use -f to follow)
drydock stats [--since 30d] [--by agent|vendor|repo|day|week] [--json]
                           # aggregate run metrics across tasks
drydock kill <id>          # cancel the in-flight task (VM down + gate unblocked)
drydock queue add|list|cancel
                           # durable unattended queue (see Queue above)
drydock doctor [--repo <path>]
                           # smoke-test the sandbox setup, or preflight a local
                           # repo with --repo (no API spend, no container)
drydock redteam            # run live containment attacks on your own sandbox (no API spend)
```

Prefer a browser? `drydock ui` puts the board, the diff/approve gate, and
history in a local web app; see [Web UI](web-ui.html).

### Run metrics: `drydock stats`

```bash
drydock stats [--since 30d] [--by agent|vendor|repo|day|week] [--json]
```

- `--since <duration>`: only include tasks whose audit file was modified in
  the last `<duration>` (e.g. `30d`, `12h`); defaults to `30d` if omitted,
  use `--since 0` for all history.
- `--by agent|vendor|repo|day|week`: break the summary out by that
  dimension instead of one aggregate.
- `--json`: emit the report as JSON instead of a table.

`drydock stats` aggregates outcome rates, duration and gate-wait
percentiles, spend, and egress-widen frequency straight from the audit dir
(no brokerd needed). Tasks from before this feature shipped still count
toward outcomes, durations, and cost, but report their gate-wait and
request-count fields as absent rather than zero.

The audit result row alone cannot tell a diff denied at the approval gate
apart from a normal success, a mid-run kill apart from a plain agent error,
or a fail-closed diff-capture failure (an oversized or unreadable staged
diff) apart from a normal success: the terminal metrics row's `outcome`
field carries the distinction (`denied`, `cancelled`, alongside `pushed`,
`push_failed`, `error`, and `no_diff`), and `drydock tasks`, `drydock
stats`, and the web UI all read it. `no_diff` still displays as `ok`
(unchanged operator muscle memory: a clean no-op run isn't a failure).
Audit files from before the `outcome` field shipped fall back to the result
row alone: denied and fail-closed diff-capture failures read as `ok`,
cancelled reads as `error`, same as today.

## Audit stream format

Each task's audit file (`~/.drydock/audit/<id>.jsonl`) is newline-delimited
JSON: one event per line, in the order they happened. Separately, the live
`drydock submit --json` / `POST /tasks` NDJSON stream now stamps every event
with a `ts` field (RFC 3339); that timestamp is on the submit stream only,
not persisted into the audit file. The audit file ends with a
broker-authored row, `{"type":"metrics","src":"broker"}`, holding stage
durations (`stage_ms.queued`, `.preparing`, `.setup`, `.running`,
`.verifying` — `.queued`/`.setup`/`.verifying` omitted when the task never
ran those stages, so a synchronous submit's row has no `queued` at all — and
`.pushing`; the stages partition the task's wall-clock, so `preparing` ends
where `setup` begins), egress/approval gate waits, the
admitted request count, spend, the terminal `outcome` (`pushed`, `denied`,
`cancelled`, `push_failed`, `setup_failed`, `verify_failed`, `policy_blocked`, `error`, or `no_diff`), and the
egress-widen outcome; if brokerd ever
appends more than one (e.g. after a resumed task), the last such row wins,
so readers should take the last `metrics` line, not the first.

## Variations

```bash
# Pick the agent for this task: claude (default) | codex | gemini | opencode
drydock submit --repo … --instruction "…" --agent codex
# opencode runs any OpenAI-compatible model (see Bring your own model)
drydock submit --repo … --instruction "…" --agent opencode

# Long prompt from a file
drydock submit --repo … --instruction-file ./task.md

# Pipe from stdin
echo "Refactor the egress compiler" | drydock submit --repo … -

# Pick a specific model (overrides default_model in config)
drydock submit --repo … --instruction "…" --model claude-sonnet-4-6

# Skip the approval gate (trusted batch run; see the threat model first)
drydock submit --repo … --instruction "…" --auto-approve

# Open the resulting PR/MR as a draft
drydock submit --repo … --instruction "…" --draft

# Request additional egress (host:port[,port], repeatable; human-gated)
drydock submit --repo … --instruction "…" \
  --egress-extra internal.example.com:443

# Suppress progress; print only the final outcome line (useful in scripts)
drydock submit --repo … --instruction "…" --quiet

# Mark the task sensitive in the audit trail
drydock submit --repo … --instruction "…" --sensitive

# Stream raw NDJSON events (one JSON object per line)
drydock submit --repo … --instruction "…" --json | jq -c 'select(.event=="result")'
```

See [Egress & widening](egress.html) for `--egress-extra`.

## Platform selection (PR/MR)

`--repo` must be a git URL (`https://`, `git@`, or `ssh://`); local paths are
rejected. The PR/MR adapter is chosen by `--platform`:

- `github` → `gh pr create --head <branch> --fill` (needs `gh` authed)
- `gitlab` → `glab mr create --fill --yes` (needs `glab` authed)
- `gitea` (alias `forgejo`) → `tea pr create --head <branch>` (needs `tea` authed)
- `none` → push only; no PR/MR
- *omitted* → hostname autodetect (`github.com`, `gitlab.com`,
  `gitea.com` / `codeberg.org`; else push-only, covering Bitbucket and
  self-hosted)

Self-hosted GitLab and Gitea need an explicit `--platform`.

## HTTP API

If you'd rather hit the broker directly:

```bash
SOCK=$TMPDIR/drydock-$(id -u)/drydock.sock
curl --unix-socket "$SOCK" http://_/tasks \
  -H 'content-type: application/json' \
  -d '{ "repo_ref": "git@github.com:o/r", "instruction": "..." }'
```
