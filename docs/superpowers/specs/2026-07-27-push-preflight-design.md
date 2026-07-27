# Git push-credential preflight

Date: 2026-07-27
Status: approved design, pre-implementation

## Problem

Push credentials are only exercised at the very end of the task lifecycle,
after the VM ran, the budget was spent, and the diff was approved. Worse,
no host-side git invocation disables interactive prompting: with an HTTPS
remote and no credential helper, git under launchd fails (no TTY), but
under a foreground `drydock start` it can sit waiting on stdin for a
username the operator never sees. The expensive failure case is a
public-read/authenticated-write repo: the anonymous clone succeeds, the
agent runs, and only the final push discovers there are no credentials.

## Design (three pieces)

### 1. Submit-time write-auth probe (fail-closed, no opt-out)

After `stage.Prepare` clones the repo, and before task files are written,
credentials are minted, or any VM work happens, the broker probes write
auth against the actual remote:

    git push --dry-run origin HEAD:refs/heads/agent/<task-id>

using the same refspec the real push would use, through the stage's
hardened `git` wrapper (separated git dir, hooks neutralized, cancellable
ctx). A dry-run push authenticates and computes ref updates without
sending objects or moving refs.

On failure the task ends at accept time: the reason comes from the
existing `classifyPushError` (`auth`, `transient`, `protected`, ...), the
submit stream gets an error event with an actionable hint (configure
`gh auth setup-git`, check the SSH agent), and nothing was spent. Single
attempt, no retry loop: a transient blip fails the task the same way a
failed clone already does, and `drydock retry` covers it.

There is deliberately no config opt-out.

**Stated consequence:** a task against a repo the operator cannot push to
now fails at submit. The previously-possible pattern of running the agent
and reviewing the diff against a read-only remote (never approving the
push) stops working. The docs state this plainly.

**Interface:** the probe is an optional capability on the stage,
`PushPreflight(branch string) error`, discovered by type assertion (the
`BaseCommit` idiom), so existing broker test fakes are untouched.

### 2. Fail-fast environment on every host-side git operation

`runGit` currently inherits the parent env untouched. A single shared
helper adds:

- `GIT_TERMINAL_PROMPT=0` (always): git never prompts on stdin;
- `GCM_INTERACTIVE=never` (always): git-credential-manager never pops UI;
- `GIT_SSH_COMMAND=ssh -oBatchMode=yes` **only when the operator has not
  set their own `GIT_SSH_COMMAND`** (a custom transport is never
  clobbered).

The helper applies to: `runGit` (all stage git calls including clone and
push), the direct `exec.CommandContext` git invocation in stage.go
(`gitDiffCapped`, the diff-capture path), and `PushEnv` (the curated env handed to gh/glab adapters,
which run their own git under the hood). Result: a credential gap
anywhere in the host git path fails in milliseconds with a classifiable
error instead of prompting or hanging.

### 3. `drydock doctor` credentials check (heuristic)

Doctor knows no target repo, so this step is generic and says so in its
output. Pass when either:

- an HTTPS credential helper is configured (`git config --get
  credential.helper`, any scope, non-empty), or
- SSH looks usable: `SSH_AUTH_SOCK` is set, or a private key exists under
  `~/.ssh` (id_* files).

Fail with an actionable hint (run `gh auth setup-git`, or set up an SSH
key / `ssh-add`) when neither exists. The per-repo probe (piece 1) is the
real gate; this is early warning during setup.

## Error handling

- Probe failures classify via `classifyPushError` and surface as a
  terminal error event on the submit stream: reason
  `push preflight failed (<class>): <first line of git output>` plus a
  hint keyed on the class (`auth` gets the credential-helper/SSH hints).
- No audit `.jsonl` exists yet at probe time (it opens later), matching
  the current clone-failure behavior: the task appears only in the submit
  stream, like other pre-accept failures.
- The probe runs under the task ctx, so kill/shutdown cancels it; the
  stage's existing git WaitDelay bounds a black-holing remote.

## Testing

- Stage: `PushPreflight` runs the dry-run against a local
  `git init --bare` remote (success) and a nonexistent remote path
  (failure), no network needed; env helper unit tests assert
  `GIT_TERMINAL_PROMPT=0`/`GCM_INTERACTIVE=never` always present and
  `GIT_SSH_COMMAND` preserved when pre-set, on `runGit`, the direct
  `gitDiffCapped` call, and `PushEnv`.
- Broker: a fake stage implementing `PushPreflight` with an auth error
  asserts the task fails at accept (no WriteTaskFiles, no mint, no run),
  with the classified reason in the terminal event; a fake without the
  capability runs unchanged (fakes untouched proves it).
- Doctor: both outcomes of the credentials step via its existing
  seam/output conventions.
- Red-team suites run as the standard gate; no containment surface
  changes (everything here is host-side pre-VM).

## Out of scope (YAGNI)

- Retrying the probe or wiring it into the push retry/backoff config.
- Branch-protection or permission-level detection (dry-run auth only).
- Per-repo or per-task probe configuration.
- Any change to the end-of-task push path itself.

## Docs to update on landing

- site/docs/submitting-tasks.md: the probe, its timing, and the stated
  consequence for read-only remotes.
- site/docs/troubleshooting.md: the auth failure hints.
- Doctor docs (where doctor's checks are listed) and README if it
  enumerates doctor checks.
- CHANGELOG Unreleased: Added (probe, doctor check) + Changed (git
  operations never prompt; SSH runs BatchMode unless GIT_SSH_COMMAND is
  set; read-only-remote tasks now fail at submit).
