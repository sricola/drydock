# Crash-recovery reconciliation — design

**Roadmap item:** Phase 4.1 (Reliability & hardening).

## Problem

When `brokerd` dies mid-task (SIGKILL, panic, host reboot) the per-task
`defer`s that clean up never run. The existing boot reconciler
`pruneOrphanTasks()` (`cmd/brokerd/main.go`, called at startup) already reaps
the *dangerous* leftovers — orphan `task-*` containers (a VM that would
otherwise keep holding a credential grant and egress) and orphan squid
processes. Two **hygiene** leaks remain, both low-danger but real:

1. **Stale stage dirs.** Each task's host scratch lives at
   `StageRoot/<taskID>/` (a cloned work tree + the host-only git dir) and is
   removed by `defer st.Cleanup()` in `HandleTask`. A crash skips that defer,
   leaving the dir — a disk leak that also retains repo contents.
2. **Stuck audit rows.** The task's `<id>.jsonl` never receives a terminal
   `{"type":"result",...}` line, so `drydock tasks` renders it as `running?`
   forever. This is the same UI-hang class the existing synthetic-result line
   fixes for the entrypoint-died path, but the crash path has no live broker
   left to write it.

The in-memory concurrency semaphore is **not** a gap here: a restarted
`brokerd` rebuilds it at full capacity, so no slot is held across a crash.
(The "wedged slot" scenario was the approval-gate-without-timeout bug, fixed
separately in PR #71.)

### Why boot-time reconciliation is safe

At startup the new `brokerd` has accepted zero tasks. Therefore *every*
leftover stage dir and *every* audit row lacking a result line provably
belongs to a dead prior life — there is no live task to misclassify. The
sweeps can run unconditionally at boot, and they are idempotent (a second boot
finds nothing left to do).

## Scope

In scope: sweep stale stage dirs; terminate stuck audit rows with a distinct
`interrupted` outcome; wire both into the existing boot reconciler. Out of
scope: any mid-run (non-boot) reconciliation, container/squid reaping (already
done), and concurrency-slot recovery (not a gap).

## Architecture

Boot-time only. `pruneOrphanTasks()` gains two more sweeps after its existing
container/squid reaping. Each new unit lives where its knowledge already does
and is pure filesystem logic, independently unit-testable with `t.TempDir()`:

- `internal/stage` owns stage dirs and the `StageRoot` safety guard → it gets
  the stage-dir sweep.
- `internal/broker` owns the audit-result line format (it already writes the
  entrypoint-died synthetic line) → it gets the audit-row terminator.
- `cmd/brokerd/main.go` stays thin wiring.
- `cmd/drydock/tasks.go` renders the new outcome.

## Components

### 1. `stage.ReapOrphans(root string) (int, error)`

New function in `internal/stage/stage.go`.

- Apply the same guard as `Cleanup`: compute `clean := filepath.Clean(root)`;
  if `clean == "" || clean == "/" || clean == "." || !filepath.IsAbs(clean)`
  return `(0, error)` and remove nothing.
- `os.ReadDir(root)`. If the root does not exist, return `(0, nil)` — a no-op,
  not an error (a fresh install has no stage root yet).
- For each entry that `IsDir()`, `os.RemoveAll(filepath.Join(root, name))`.
  A per-entry removal error is collected and logged by the caller but does not
  abort the sweep; return the count successfully reaped and the first error (or
  nil). Non-directory entries are left untouched.

### 2. `broker.TerminateStuckAudits(auditRoot string) (int, error)`

New function in a new file `internal/broker/reconcile.go` (broker package).

- `os.ReadDir(auditRoot)`. Missing root → `(0, nil)`.
- For each `*.jsonl` entry: read the file's tail (16 KB, mirroring
  `lastResult` in `cmd/drydock/tasks.go`) and scan its lines for one where
  `"type":"result"`. If such a line exists, skip the file (already terminal —
  this is what makes the sweep idempotent).
- If no result line exists, append exactly one line:

  ```json
  {"type":"result","subtype":"interrupted","is_error":true,"duration_ms":0,"total_cost_usd":0,"num_turns":0}
  ```

  `duration_ms:0` because the death time is unknown; the `interrupted` label
  carries the signal rather than a fabricated duration. Open the file with
  `O_APPEND|O_WRONLY`.
- Per-file errors are collected/logged, not fatal. Return the count terminated
  and the first error (or nil).

The result-line detection is a small local tail-scan, intentionally not shared
with `cmd/drydock`'s `lastResult`: the two live in different binaries
(`brokerd` vs `drydock`), and sharing would mean a new internal package for ~15
lines. The audit-result JSON shape is already a stable contract duplicated
between the broker writer and the `drydock` reader; one more writer with a
distinct subtype is consistent with that.

### 3. `summarize` (`cmd/drydock/tasks.go`)

Add a case **before** the `IsError` case so an interrupted task reads
`interrupted` rather than `error`:

```go
switch {
case last.Subtype == "interrupted":
    r.outcome = "interrupted"
case last.IsError:
    r.outcome = "error"
case last.Subtype == "success":
    // ... unchanged
default:
    r.outcome = last.Subtype
}
```

`is_error:true` is preserved on the line so any exit/scripting semantics that
key off it still treat an interrupted task as a non-success terminal state; the
display simply distinguishes *why*.

### 4. `pruneOrphanTasks` (`cmd/brokerd/main.go`)

- Change signature to `pruneOrphanTasks(stageRoot, auditRoot string)`; pass
  `cfg.StageRoot` and `cfg.AuditRoot` at the existing call site.
- After the container/squid reaping, call `stage.ReapOrphans(stageRoot)` and
  `broker.TerminateStuckAudits(auditRoot)`, each guarded by an `err != nil`
  `slog.Warn`, and `slog.Info` the non-zero counts (`"orphan prune: reaped N
  stage dirs"`, `"orphan prune: terminated N stuck audit rows"`). Mirror the
  best-effort, log-and-continue style the function already uses.

## Error handling

Every sweep is best-effort and isolated: a failure on one dir or file is
logged and skipped, never aborting boot — consistent with the existing
container reaper that swallows per-item errors. The single hard stop is the
`StageRoot` guard: an unsafe root (`""`, `"/"`, `"."`, non-absolute) refuses
the entire stage sweep rather than risk a catastrophic `RemoveAll`.

## Testing

`internal/stage/stage_test.go`:
- `ReapOrphans` over a temp root holding 3 child dirs + 1 stray file → returns
  3, all dirs gone, stray file untouched.
- Unsafe roots (`""`, `"/"`, `"."`, a relative path) → error, nothing removed.
- Missing root → `(0, nil)`.

`internal/broker/reconcile_test.go`:
- A `.jsonl` with stream lines but no result line → exactly one `interrupted`
  line appended; `is_error` decodes true, `subtype` is `interrupted`.
- A `.jsonl` that already has a `result` line → byte-identical after the call
  (idempotency).
- Running `TerminateStuckAudits` twice → the second call appends nothing.
- Missing audit root → `(0, nil)`.

`cmd/drydock/tasks_test.go`:
- A synthetic `interrupted` result line → `summarize` yields outcome
  `interrupted` (not `error`).

## Done when

A `brokerd` restart after a crash leaves no orphaned stage dir and no audit row
stuck at `running?`; crash-interrupted tasks display the distinct `interrupted`
outcome; all new functions are unit-tested; and `THREAT_MODEL.md` / roadmap
4.1 reflects that crash recovery now covers VMs, squid, stage dirs, and audit
rows.
