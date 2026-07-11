# Push partial-failure contract + recovery (ROADMAP 4.2): design

Status: proposed (2026-07-10)

## Problem

The approved-diff push is a multi-step git sequence run by `Stage.Push`:
`git checkout -b <branch>` then `add`, `commit`, `push origin <branch>`. When it
fails (most often at `push`), `broker.pushAndOpenPR` emits a bare
`{"event":"error",...}` line and returns. That is NOT a terminal `result` line,
so `audit.LastResult` finds no outcome and `drydock tasks` shows the task in an
ambiguous state. The operator cannot tell from the audit row whether anything
landed on the remote or whether a retry is safe.

A single-ref `git push` is atomic: the remote branch either receives the whole
commit or is left unchanged. So there is no corrupted remote to roll back. The
real gap is (a) the outcome is not reported as a clean terminal state, and (b)
recoverable failures (a flaky network, or the rare branch-name collision) are
not retried at all.

## Goal

Define the push failure contract and make it observable, and recover
automatically from the recoverable failure classes, reporting exactly one clean
terminal outcome either way.

## Decisions

1. **Split commit from push.** `Stage.Commit` runs the one-time
   `checkout -b`/`add`/`commit`; `Stage.PushBranch(local, remote)` runs
   `git push origin <local>:<remote>` and is the retryable unit. The refspec
   lets recovery push the SAME local commit to a fresh remote branch name
   without re-committing (a re-invocation of the old combined `Push` would find
   nothing to commit).
2. **Classify failures from git stderr.** A pure classifier maps git's combined
   output (already carried in the error via `runGit`) into a reason. Unknown
   reasons are treated as terminal: never retry a failure we do not understand.
3. **Recover the recoverable classes.** Transient failures retry the same push
   with exponential backoff; branch-name collisions retry to a fresh remote
   name; auth/protected/unknown stop immediately.
4. **One terminal outcome.** Success at any attempt yields `outcome=pushed` with
   the ACTUAL branch used; exhaustion or a terminal class yields
   `outcome=push_failed` with the classified reason. Both are `result` lines, so
   the audit row is always unambiguous.
5. **Config-tunable bounds.** Three fields with env overrides and `0`-disables
   semantics, matching the existing per-task config pattern.

## Components

### Stage primitives (internal/stage)

- `Commit(branch, message string) error`: `checkout -b <branch>`, `stageAll`,
  `commit -m <message>`. Runs once per task.
- `PushBranch(localBranch, remoteBranch string) error`:
  `git push origin <localBranch>:<remoteBranch>`. Retryable; supports pushing
  the committed local branch to a differently-named remote branch.
- `Push(branch, message string) error` stays as a thin wrapper
  (`Commit(branch, msg)` then `PushBranch(branch, branch)`) so existing callers
  and tests keep working.

The `stagePort` interface the broker uses (currently `Push` + `PushEnv`) gains
`Commit` and `PushBranch` so white-box broker tests can drive recovery with a
fake stage. `realStage` forwards to the new `Stage` methods.

### Push-failure classifier (new internal/broker/pushfail.go)

`classifyPushError(errText string) pushReason` where `pushReason` is one of:

- `reasonNonFastForward`: "! [rejected]", "non-fast-forward", "fetch first",
  "Updates were rejected".
- `reasonTransient`: "Could not resolve host", "Connection timed out",
  "Connection reset", "Could not read from remote", "RPC failed", "early EOF",
  "TLS", "503"/"502"/"500", "timed out".
- `reasonAuth`: "Authentication failed", "could not read Username",
  "Permission denied", "403", "access denied", "Invalid username or password".
- `reasonProtected`: "protected branch", "GH006", "pre-receive hook declined".
- `reasonUnknown`: anything else.

Pure and table-tested. It knows nothing about the stage, config, or broker: it
takes a string and returns a reason.

### Recovery orchestration (broker)

`pushWithRecovery(ctx, st stagePort, taskID, message string, cfg pushRetry) (branch string, attempts int, reason pushReason, err error)`:

1. `st.Commit("agent/"+taskID, message)`. A commit-step failure is terminal
   (reason `reasonUnknown`, attempts 0) since nothing was pushed.
2. Attempt `st.PushBranch("agent/"+taskID, remote)` starting with
   `remote = "agent/"+taskID`. On error, classify:
   - `reasonTransient`: sleep `backoff * 2^n` (ctx-cancellable), retry the SAME
     remote, up to `cfg.MaxRetries` times.
   - `reasonNonFastForward`: set `remote = "agent/"+taskID+"-"+k` for
     `k = 2..cfg.FreshBranchTries+1`, retry each once.
   - `reasonAuth`, `reasonProtected`, `reasonUnknown`: stop; return the reason.
3. Return the remote branch that succeeded (nil err), or the last reason + error
   on exhaustion.

Backoff waits on `select { case <-time.After(d): case <-ctx.Done(): }` so a
`drydock kill` during backoff cancels promptly. `cfg` is derived from config;
`push_retry_backoff = 0` makes tests deterministic with no real sleeps.

The orchestrator lives in a new `internal/broker/push.go` so `broker.go` does
not grow; `pushAndOpenPR` calls it and maps the result onto the terminal event.

### Terminal outcome / audit shape

Important reality of the current audit: the outcome `drydock tasks` shows comes
from the terminal `{"type":"result","subtype":...}` line, which the AGENT writes
(or drydock synthesizes for non-claude agents and for task failure, see
`broker.go` around lines 530 and 552). The push outcome is NOT in the audit at
all today: `pushAndOpenPR` only streams `{"event":"result","outcome":...}` to
the client via `tr.sw`. So a task whose agent succeeded but whose push then
failed currently shows a misleading `subtype:success` -> "ok" in the audit row.
That is the ambiguity to close.

The fix reuses the existing synthetic-result-line pattern. On a terminal push
failure, `pushAndOpenPR` appends one synthetic result line to the audit log
(`tr.logf`), carrying the agent's already-metered cost forward so cost and the
aggregate-cap seed stay correct:

```
{"type":"result","subtype":"push_failed","is_error":false,"duration_ms":N,
 "total_cost_usd":<audit.TotalCost(auditPath)>,"num_turns":0}
```

`is_error` is false so `audit.Outcome` reaches a new explicit case
`r.Subtype == "push_failed" -> "push failed"` (added before the `IsError`
branch, which would otherwise render it as a bland "error"). `LastResult` reads
this as the terminal line, so `drydock tasks` shows "push failed" with the
preserved cost, unambiguously.

The live client still gets a richer STREAM event (replacing today's bare
`errorEvent`):

```
{"event":"result","outcome":"push_failed","task_id":...,"reason":"transient",
 "push_attempts":3,"branch":"agent/<id>","error":"<safeErr>",
 "files":N,"insertions":N,"deletions":N,"duration_ms":N,"cost_usd":N,
 "hint":"nothing was pushed to the remote; the diff is preserved; retry with `drydock retry <id>`"}
```

On success, the existing `outcome=pushed` STREAM event reports `branch` = the
ACTUAL remote branch used (e.g. `agent/<id>-2` after a fresh-branch recovery)
and adds `push_attempts`. No new audit line is written on success: the agent's
result line stands (the row shows the agent outcome, e.g. "ok"), and a
successful push is not ambiguous. `pr_opened`/`pr_error` stay as they are (PR
creation remains best-effort and never downgrades a successful push). Denied and
cancelled outcomes are unchanged (out of scope: they are a separate, pre-existing
display question, not the push-failure ambiguity 4.2 targets).

### Config surface

Three fields on `config.Config`. The defaults enable recovery; setting any
field to `0` disables that specific path (falling back toward today's
single-attempt behavior):

- `push_max_retries` (int, default `3`): transient-failure retries. `0` disables
  transient retry.
- `push_retry_backoff` (duration, default `1s`): base for exponential backoff
  (`backoff * 2^n`). `0` = no delay between retries.
- `push_fresh_branch_tries` (int, default `2`): alternate remote branch names to
  try on a collision. `0` disables fresh-branch recovery.

Env overrides `DRYDOCK_PUSH_MAX_RETRIES`, `DRYDOCK_PUSH_RETRY_BACKOFF`,
`DRYDOCK_PUSH_FRESH_BRANCH_TRIES`, matching the existing per-field pattern.
Validation: each `>= 0`. SeedTemplate and the on-disk `config/config.yaml`
mirror document all three (the drift test enforces they match).

## Data flow

```
pushAndOpenPR(diff):
  gate (approve/deny/cancel)  -- unchanged
  -> pushWithRecovery(ctx, st, id, msg, cfg):
       Commit(agent/<id>, msg)
       loop: PushBranch(agent/<id>, remote)
         ok            -> return (remote, attempts)
         transient     -> backoff (ctx-aware), retry same remote  (<= MaxRetries)
         non_fast_fwd  -> remote = agent/<id>-2, -3 ...           (<= FreshBranchTries)
         auth/protected/unknown -> return reason (terminal)
  -> success: open PR on the returned branch; emit result outcome=pushed (branch, attempts)
  -> failure: emit result outcome=push_failed (reason, attempts, hint)
```

## The contract (documented in site/docs)

- A single-ref `git push` is atomic: the remote branch either receives the whole
  commit or is unchanged. `push_failed` therefore guarantees nothing landed on
  the remote for that task. A fresh-branch success lands exactly one branch: the
  one named in the `pushed` event.
- The captured diff is preserved in the audit `.diff` file for every outcome, so
  no approved work is ever lost to a push failure.
- `push_failed` is retry-safe: `drydock retry <id>` re-runs the task under a new
  id (and a new `agent/<newid>` branch), so a retry never collides with the
  failed attempt.
- Reason semantics: `transient` (retried, exhausted) means try again later;
  `auth`/`protected` mean fix credentials or branch protection; `non_fast_forward`
  exhausted means the target branches already exist (retry gives a fresh id);
  `unknown` is reported verbatim with the git error for the operator to judge.

## Testing (TDD)

- Classifier: a table of real git stderr samples maps to each reason, including
  `unknown` for an unrecognized string.
- Recovery orchestrator: a fake `stagePort` that (a) fails `transient` N times
  then succeeds, (b) always returns `non_fast_forward` and walks the alt
  branches, (c) returns `auth` and stops after one attempt, (d) fails `Commit`.
  With `push_retry_backoff = 0` the test runs without real sleeps; assert the
  attempt count, the final branch, and the returned reason. A ctx cancelled
  mid-backoff returns promptly.
- Stage: `Commit` then `PushBranch` against a bare origin (real git), including
  pushing the local branch to a differently-named remote via the refspec; the
  `Push` wrapper still passes `TestPush_CommitsAndPushes`.
- Config: negative values rejected; env overrides parsed; SeedTemplate matches
  the on-disk mirror (existing drift test).
- Audit: `Outcome` renders a `push_failed` subtype as "push failed" (new case,
  before the `IsError` branch); a task with an agent-success line followed by a
  synthetic `push_failed` line reads as "push failed" with the carried cost.
- Broker: a push failure appends the synthetic `push_failed` result line to the
  audit AND streams a `push_failed` event with the reason (not a bare error
  event); a fresh-branch success streams `pushed` with the alt branch and
  `push_attempts > 1`.

## Non-goals (YAGNI)

- No robustness changes to the pre-push steps: a `Commit` failure is terminal
  with a clear outcome (it is rare, and nothing was pushed).
- No auto-retry of the whole task: recovery is scoped to the push. Re-running a
  task is the operator's call via `drydock retry` (4.4).
- No partial-remote cleanup: single-ref push atomicity means there is nothing
  half-written to undo.
- No PR-creation retry: opening the PR stays best-effort and never downgrades a
  successful push.
