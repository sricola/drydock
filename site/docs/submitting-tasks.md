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

If the branch pushes but the PR can't be opened (e.g. `gh` isn't authenticated),
drydock reports it as **pushed** with a hint to open the PR manually; it never
loses your work to a failed PR step.

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
  approval gate, and nothing is pushed.

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
outcome.

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
**required** verification does not pass reports `outcome=verify_failed` and
never reaches the approval gate. `drydock tasks`, `drydock stats`, and the
web UI all show these outcomes distinctly from `pushed` and `push_failed`.

## Operator surface

```bash
drydock status             # brokerd up?, breakdown (running · verifying · egress · diff · pushing)
drydock inspect <id>       # trust brief: broker-observed evidence incl. verification
drydock tasks              # recent runs: id, age, duration, cost, outcome
drydock logs <id> [-f]     # stream-json audit (use -f to follow)
drydock stats [--since 30d] [--by agent|vendor|repo|day|week] [--json]
                           # aggregate run metrics across tasks
drydock kill <id>          # cancel the in-flight task (VM down + gate unblocked)
drydock doctor             # smoke-test the sandbox setup (no API spend)
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
durations (`stage_ms.preparing`, `.running`, `.verifying` — omitted when the
task never verified — and `.pushing`), egress/approval gate waits, the
admitted request count, spend, the terminal `outcome` (`pushed`, `denied`,
`cancelled`, `push_failed`, `verify_failed`, `error`, or `no_diff`), and the
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
