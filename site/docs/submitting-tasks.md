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
drydock reports it as **pushed** with a hint to open the PR manually. It never
loses your work to a failed PR step.

## Operator surface

```bash
drydock status             # brokerd up?, breakdown (running · egress · diff · pushing)
drydock tasks              # recent runs: id, age, duration, cost, outcome
drydock logs <id> [-f]     # stream-json audit (use -f to follow)
drydock kill <id>          # cancel the in-flight task (VM down + gate unblocked)
drydock doctor             # smoke-test the sandbox setup (no API spend)
drydock redteam            # run live containment attacks on your own sandbox (no API spend)
```

Prefer a browser? `drydock ui` puts the board, the diff/approve gate, and
history in a local web app. See [Web UI](web-ui.html).

## Variations

```bash
# Pick the agent for this task: claude (default) | codex | gemini | opencode
drydock submit --repo … --instruction "…" --agent codex
# opencode runs any OpenAI-compatible model; see Bring your own model
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
  `gitea.com` / `codeberg.org`; else push-only (covers Bitbucket and
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
