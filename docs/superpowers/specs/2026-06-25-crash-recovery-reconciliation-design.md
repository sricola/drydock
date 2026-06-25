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

The sweeps `RemoveAll` stage dirs and rewrite audit rows, so they are only safe
if **no task is live** when they run. A single `brokerd` per host has accepted
zero tasks at startup, so every leftover stage dir and every audit row lacking
a result line provably belongs to a dead prior life — nothing live to
misclassify, and the sweeps are idempotent (a second boot finds nothing to do).

That proof depends on single-instance — and today it is **not enforced**:
`pruneOrphanTasks()` runs before brokerd binds its socket/ports (the de-facto
exclusivity), with no lock in between. A second `brokerd` started while the
first is mid-task would, at boot, force-delete the first's live VM (existing
reaper behavior) and — once these sweeps land — `RemoveAll` its live stage dir
and corrupt its in-flight audit row, *then* fail to bind and exit. This spec
therefore promotes single-instance from an assumption to an **enforced
invariant** (Component 0) as a prerequisite for the sweeps, closing the
pre-existing live-VM-kill race as a side effect.

## Scope

In scope: enforce single-instance with a boot lock (prerequisite for the
sweeps' safety); sweep stale stage dirs; terminate stuck audit rows with a
distinct `interrupted` outcome; wire all into the existing boot reconciler. Out
of scope: any mid-run (non-boot) reconciliation, container/squid reaping
(already done), and concurrency-slot recovery (not a gap — a restarted brokerd
rebuilds the semaphore at full capacity).

## Architecture

Boot-time only. A single-instance lock is acquired first; then
`pruneOrphanTasks()` gains two more sweeps after its existing container/squid
reaping. Each new sweep unit lives where its knowledge already does and is pure
filesystem logic, independently unit-testable with `t.TempDir()`:

- `cmd/brokerd/main.go` acquires the boot lock (via `config.LockPath()`) before
  any reaping, and stays thin wiring for the rest.
- `internal/stage` owns stage dirs and the `StageRoot` safety guard → it gets
  the stage-dir sweep.
- `internal/broker` owns the audit-result line format (it already writes the
  entrypoint-died synthetic line) → it gets the audit-row terminator.
- `cmd/drydock/tasks.go` renders the new outcome.

## Components

### 0. Single-instance lock (prerequisite)

New `config.LockPath()` returning `~/.drydock/brokerd.lock` (mirrors
`DefaultPath()`), and a small guard in `cmd/brokerd/main.go` run **before**
`pruneOrphanTasks`:

- A small helper `acquireLock(path) (*os.File, error)` opens (create, `0o600`)
  the lock file and `syscall.Flock(fd, LOCK_EX|LOCK_NB)` (stdlib, already
  imported; verified on darwin). On success it returns the open `*os.File`,
  which `main` holds for the process's whole life (a package var so it isn't
  GC'd/closed); the descriptor staying open is what keeps the lock held.
- On a non-nil error from `acquireLock` (`EWOULDBLOCK` = another `brokerd` holds
  it) → `die("another brokerd is already running", ...)` and exit non-zero
  **without** running any reaper.
- The lock is advisory and released automatically when the process exits
  (including SIGKILL — the kernel drops the flock), so no stale-lock cleanup is
  needed. A crashed brokerd leaves the file but not the lock.

This makes "no task is live at boot" true by construction: the reconciler only
runs once the lock is held, and the lock is held only by the sole live brokerd,
which at that point has accepted no tasks.

### 1. `stage.ReapOrphans(root string) (int, error)`

New function in `internal/stage/stage.go`.

- Apply the same guard as `Cleanup`: compute `clean := filepath.Clean(root)`;
  if `clean == "" || clean == "/" || clean == "." || !filepath.IsAbs(clean)`
  return `(0, error)` and remove nothing. The caller `slog.Warn`s this error
  so a relative/unset `STAGE_ROOT` (e.g. an env override `STAGE_ROOT=stage`,
  which `expandHome` does not make absolute) surfaces as a logged refusal
  rather than reaping mysteriously not happening.
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
- **Ordering is load-bearing, not stylistic:** the container delete must
  precede the stage reap, because the VM mounts the work tree out of the stage
  dir — reaping first could `RemoveAll` a path a still-terminating VM holds. A
  code comment must state this so a later refactor can't reorder it. (Apple
  `container delete --force` returns after teardown; the boot lock further
  guarantees no *other* brokerd is concurrently starting a VM here.)

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

`config` test:
- `LockPath()` returns an absolute `~/.drydock/brokerd.lock`-shaped path.

Lock contention is unit-testable in-process and should be a real test (no
subprocess, no manual-only path). `flock` locks attach to the open file
*description*, not the process, so two separate `os.OpenFile` handles to the
same path within one test contend: lock the first → succeeds; lock the second
(`LOCK_EX|LOCK_NB`) → returns `EWOULDBLOCK`. Verified empirically on darwin.
The lock acquisition is therefore factored into a small testable helper
(e.g. `acquireLock(path) (*os.File, error)`) that the `cmd/brokerd` guard
calls, and the test asserts: first acquire on a `t.TempDir()` path succeeds,
second acquire on the same path returns an error.

## Done when

A `brokerd` restart after a crash leaves no orphaned stage dir and no audit row
stuck at `running?`; crash-interrupted tasks display the distinct `interrupted`
outcome; a second concurrent `brokerd` refuses to start (and runs no reaper)
instead of clobbering the first's live task; all new functions are unit-tested;
and roadmap 4.1 reflects that crash recovery now covers VMs, squid, stage dirs,
and audit rows (with single-instance enforced). A one-line SECURITY.md residual
update is optional, only if the stale-stage-dir leak is judged worth noting.
