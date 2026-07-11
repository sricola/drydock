# Resume awaiting-approval across restart (ROADMAP 4.14): design

Status: proposed (2026-07-11). Tracks issue #140.

## Problem

A task blocked at the diff-approval push gate holds its live state (the work
tree with the agent's uncommitted changes, the separated host-only git dir) only
in the on-disk stage dir at `<StageRoot>/<taskID>`. On brokerd restart,
`pruneOrphanTasks` -> `stage.ReapOrphans` unconditionally `RemoveAll`s every
stage dir, so:

- the work tree is destroyed while the persisted `<id>.diff` survives
  unpushable (there is no code path to push it afterward); and
- the audit's terminal `result` line is the agent's `ok` (the agent writes its
  result line BEFORE `CaptureDiff` and the gate), so `drydock tasks` reads a
  false success even though nothing was pushed.

Human-gated tasks are exactly the ones expected to sit for minutes-to-hours, so
this is the common case, and the launchd daemon's restart-on-crash makes it
worse. Note the push does NOT need the model credential: it uses git remote auth
via `PushEnv`, so a push after restart is feasible without re-minting a lease.

## Goal

Persist a durable gate marker; preserve the awaiting-approval stage dir across a
restart; at boot re-register the task as pending and resume, so `drydock approve
<id>` after a restart pushes the surviving branch (no agent re-run, no re-spend).
Fall back to an honest `interrupted` audit outcome when the stage did not
survive. Never show a false `ok` for a task that did not push.

Scope: the PUSH (diff-approval) gate only. The egress-widen gate runs before the
agent, so an interrupted egress-gate task has no completed work and already reads
`interrupted` via `TerminateStuckAudits` (no result line yet).

## Decisions

1. **Durable gate marker.** When a task enters the push gate, write
   `<AuditRoot>/<id>.gate.json` with everything a resume needs. Remove it the
   moment the gate resolves definitively (approve, deny, kill, timeout). Leave it
   on brokerd shutdown so boot resumes the task.
2. **Distinguish shutdown from kill.** The per-task context becomes a
   `context.WithCancelCause`. `drydock kill` cancels with `errTaskKilled`
   (definitive drop); brokerd shutdown's `CancelAll` cancels with `errShutdown`
   (resume later). `awaitGate` inspects `context.Cause(ctx)` to decide whether to
   remove the marker.
3. **Skip-reap marked stages.** `pruneOrphanTasks` reads the live markers and
   passes their ids to `ReapOrphans` as a keep-set, so awaiting-approval stage
   dirs survive the boot reap.
4. **Resume, else honest-interrupt.** After the broker is built, boot
   reconciliation scans markers: if the stage dir reopens, spawn a headless
   resume that re-registers the task as pending and runs the gate+push tail
   (approve -> push the surviving branch + open PR; deny/timeout -> terminal
   outcome); if the stage is gone, append an `interrupted` terminal line to the
   audit and drop the marker.
5. **Honest terminal audit line.** Every resume resolution appends a terminal
   `result` line to the audit (reusing the 4.2 synthetic-line pattern, carrying
   the agent's metered cost forward so cost and the aggregate-cap seed stay
   correct), so `drydock tasks` reflects the true final state.

## Components

### Gate marker (internal/broker/gates.go)

`gatePush`'s `onReady` already persists `<id>.diff`. Add: write
`<AuditRoot>/<id>.gate.json`:

```json
{"repo_ref":"...","instruction":"...","platform":"...","draft":false,
 "agent":"claude","task_start_ms":1720}
```

After `awaitGate` returns, `gatePush` removes the marker UNLESS the cause was
`errShutdown` (leave it for boot resume). A new `gateMarker` struct + `writeGateMarker`/`readGateMarker`/`removeGateMarker`/`listGateMarkers(auditRoot)` helpers live in a small `internal/broker/gatemarker.go` so the shape and the boot reconciler share one definition.

`awaitGate` returns a richer result so callers can act on the cause. Change its
return to `(approved bool, cause gateCause)` where `gateCause` is one of
`gateApproved`, `gateDenied`, `gateKilled`, `gateTimeout`, `gateShutdown`
(derived from the channel value, `context.Cause(ctx)`, and
`ctx.Err()==DeadlineExceeded`). `gateEgressWiden` ignores the cause (its
behavior is unchanged).

### Context cause (internal/broker/broker.go, admin.go, cmd/brokerd/main.go)

`taskCtx, cancel := context.WithCancelCause(context.Background())`;
`registerTask` stores a `context.CancelCauseFunc`. `HandleKill`'s stored-cancel
call passes `errTaskKilled`; `CancelAll` passes `errShutdown`. `defer cancel(nil)`
on the normal exit path. Two package errors: `errTaskKilled`, `errShutdown`.

### Stage reopen + reap skip (internal/stage/stage.go)

- `Reopen(root string) (*Stage, error)`: reconstruct `{Root, WorkDir, gitDir}`
  from the same layout `Prepare` builds (no clone), returning an error if the
  work tree or git dir is missing.
- `ReapOrphans(root string, keep map[string]bool) (int, error)`: gains a keep
  set; a stage dir whose base name is in `keep` is skipped. (Existing callers
  pass `nil`.)

### Boot skip + resume wiring (cmd/brokerd/main.go)

- `pruneOrphanTasks` reads `broker.ListGateMarkers(auditRoot)` -> `keep` id set,
  and calls `stage.ReapOrphans(stageRoot, keep)`.
- After the broker `b` is constructed and BEFORE the serve loop starts,
  `b.ResumeAwaiting(stageRoot)` runs (so pending is registered before operators
  can connect).

### Resume reconciliation (internal/broker/reconcile.go)

`(b *Broker) ResumeAwaiting(stageRoot string)`: for each `<id>.gate.json`:

- Reopen `<stageRoot>/<id>` via `stage.Reopen`. On success: read `<id>.diff`,
  reopen the audit log for append, `registerTask` a fresh cancel-cause ctx, and
  spawn a goroutine running `(b *Broker) resumePush(id, marker, st, diff)`.
- On reopen failure (stage gone / partial): append an `interrupted` terminal
  line to `<id>.jsonl`, remove the marker, and log. (No false `ok` remains.)

`resumePush` reconstructs a minimal `taskRun` (the reopened stage, a discard
stream since the original submit client is gone, the marker's fields, the
reopened `logf`, the fresh ctx) and runs the gate+push tail:

- Re-register pending via the same `gatePush` path (so `drydock approve <id>`
  works), then on `gateApproved` run `pushWithRecovery` + open the PR + append a
  terminal `result` line (`subtype: pushed` or `push_failed`); on `gateDenied`
  append `subtype: denied`; on `gateTimeout` append `subtype: interrupted`; on
  `gateShutdown` (brokerd going down again) leave the marker for the next boot
  and exit without a terminal line. Remove the marker + `st.Cleanup()` on a
  definitive resolution.

To share the gate+push tail between the live path (`pushAndOpenPR`) and
`resumePush`, extract the post-gate push+PR+outcome sequence into a helper
`(tr *taskRun) finishPush(diff string, files, insertions, deletions int)` that
both call after the gate returns approved. The live `pushAndOpenPR` keeps
computing the gate; `resumePush` calls the gate then `finishPush`.

### Terminal audit rendering (internal/audit/audit.go)

`Outcome` already renders `interrupted`, `success`, `push_failed`. Add a
`denied` case rendering `"denied"` (so a resume-denied task shows "denied", not
the default bland string). `pushed` remains implicit via the agent `ok` line on
a successful resume (the push succeeded, so `ok` is honest), but `resumePush`
appends a `pushed` subtype line for clarity; add a `pushed -> "pushed"` case too.

## Data flow

```
gatePush (live): onReady writes <id>.diff + <id>.gate.json ; awaitGate blocks
  approve/deny/kill/timeout -> remove marker, finishPush or drop
  shutdown (errShutdown)    -> LEAVE marker ; task goroutine exits

brokerd boot:
  pruneOrphanTasks: keep = ListGateMarkers(auditRoot) ; ReapOrphans(stageRoot, keep)
  ... broker built ...
  b.ResumeAwaiting(stageRoot):
     per <id>.gate.json:
       stage reopens -> registerTask + resumePush goroutine (re-register pending,
                         gate -> approve: pushWithRecovery + PR + terminal line;
                                  deny: terminal denied; timeout: terminal interrupted)
       stage gone    -> append interrupted terminal line ; remove marker
```

## Error handling

- A resume whose push fails classifies + reports `push_failed` (via the 4.2
  path); the marker is removed and the diff preserved, so `drydock retry <id>`
  still works.
- If brokerd goes down again while a resumed task is re-awaiting, its marker
  persists and it resumes on the next boot (idempotent).
- `ResumeAwaiting` never blocks boot: each task is a goroutine; a reopen or read
  error for one task is logged and does not stop the others.

## Testing (TDD)

- `stage.Reopen`: reopens a prepared-then-persisted stage and can `Commit`/`PushBranch`; errors when the git dir is absent.
- `ReapOrphans` keep-set: a dir in `keep` survives; others are removed.
- gate marker helpers: write/read/list/remove round-trip; `listGateMarkers`
  ignores non-marker files.
- `awaitGate` cause: returns `gateShutdown` when ctx cancelled with `errShutdown`,
  `gateKilled` with `errTaskKilled`, `gateTimeout` on deadline, `gateApproved`/`gateDenied` on the channel.
- gatePush marker lifecycle: marker present while blocked; removed on
  approve/deny/kill/timeout; LEFT on shutdown.
- `ResumeAwaiting`: given a marker + a surviving fake stage, registers pending and
  an approve pushes the surviving branch (a fake `PushBranch` records the call)
  and writes a terminal line; given a marker + no stage, appends `interrupted`
  and removes the marker; the audit no longer reads `ok`.
- `audit.Outcome`: renders `denied` and `pushed`.

## Non-goals (YAGNI)

- The egress-widen gate is out of scope (no completed work to lose; already
  reads interrupted).
- No live-path change to how denied/pushed are reported for NON-restart tasks
  (that pre-existing display question is separate); the resume path writes the
  honest terminal line where it matters most (after a restart).
- No re-run of the agent on resume (the whole point is to avoid re-spending); a
  stage that did not survive falls back to interrupted + `drydock retry`.
- No cross-host resume (single-host daemon).
