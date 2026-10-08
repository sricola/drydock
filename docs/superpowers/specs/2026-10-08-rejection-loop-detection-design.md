# Rejection-loop detection (roadmap 5C, first item)

Date: 2026-10-08
Status: approved design, revised after independent review, pre-implementation

## Goal

An unattended daemon fed a queue of work must not keep spending on a change
the operator has already rejected. Today a diff denied at the approval gate
ends that task and nothing inside brokerd re-enqueues it, so the loop is
driven from outside: a feeder script re-adding the same issue with
`drydock queue add --issue`, or a hand resubmit of the same instruction.
This increment gives the broker a durable memory of human diff-gate verdicts
and uses it in two places: before any spend, to refuse a queue add for work
that has been denied repeatedly, and after a run, to stop a queued task from
re-posing a diff the operator has already turned down. Closes the first of
the four 5C refinements in `docs/ROADMAP.md`.

A bounded CI retry chain is not one of the loops this closes: a retry child
exists only after an observed CI failure, which needs a push, which needs an
approval, and a denied child never pushes. A chain therefore never
accumulates more than one denial. Chains still key on their original
instruction (below) so a child's denial lands on the same key a feeder would
hit, and so the diff backstop sees the chain's denied diffs.

## Decisions taken during brainstorming

- **Two guards, identity first.** A pre-spend identity guard at enqueue is
  the bound; a post-run same-diff backstop catches a reworded resubmission
  that produces the same change.
- **Queue path only for the identity guard.** `POST /queue` and
  `drydock queue add` are refused. The synchronous `drydock submit` and
  `drydock retry` never consult it: a human typing them is spending
  deliberately, and they are the override path. There is no override flag.
- **Refuse at the API, persist nothing.** A tripped enqueue returns HTTP 409
  with a structured reason. A feeder retrying every few minutes cannot grow
  the audit dir.
- **Reset by a changed instruction or a later human approval.** The key
  includes a hash of the original instruction, so a reworded task is a new
  key by construction; a human-approved push on a key resets its count. No
  rolling window, no clear command.
- **Issue-driven work is also keyed on the issue URL** (added after review).
  The issue's title and body are the author's text, and the author may be
  the adversary; editing one byte would otherwise reset the bound. The URL
  is set by the operator's CLI and the author cannot change it.
- **Only human verdicts move the count** (sharpened after review). A human
  `deny` counts; a human `approve` resets. Auto-approve writes nothing.
- **Backstop: auto-deny on the queue path, warn on the sync path.** A queued
  task whose diff matches a denied diff for the repository terminates before
  the gate. A synchronous task still poses the gate, with the match shown to
  the reviewer.
- **History lives in a dedicated ledger**, not derived from the audit dir or
  from queue records (see "Why a ledger").
- **Default on, `max_denials: 2`.** This deliberately breaks Phase 5's
  opt-in rule. The guard fires only after two human denials of the same work
  on the unattended path, which is exactly the loop the phase exists to stop;
  `0` turns it off, including its fail-closed behavior.

## Why a ledger

Three places could hold "this key was denied N times":

1. **The audit dir, scanned at decision time.** Every enqueue would scan
   every task's result row and trust brief: O(audit dir) per enqueue,
   coupled to two on-disk formats, and `drydock prune` erases the evidence
   and silently reopens the loop.
2. **Queue records.** Miss every synchronous-path denial, and die with the
   terminal-record prune sweep that is itself a 5C item.
3. **A dedicated append-only ledger under `audit_root`.** O(1) decisions
   from an in-memory index, independent of pruning, written by every gate
   path (live, queued, and resumed after a restart). It follows the global
   usage ceiling's ledger (`internal/broker/globalledger.go`) in stance and
   layout, without that ledger's rolling window, compaction, or clock
   correction.

Option 3 is the design.

## The keys, and what counts

Every verdict carries two keys; the guard trips on either.

**Instruction key** = `repo_key` + SHA-256 of the **original instruction**:
`Task.RootInstruction` when non-empty (a CI retry child, where the broker is
the only writer of that field), otherwise `Task.Instruction`. `repo_key` is
`repokey.Normalize(repo_ref)` with the path lowercased as well as the host,
so `github.com/O/R` and `github.com/o/r` are one key. The hash is
`trustbrief.HashInstruction` over the root text. For a retry child that is a
different value from the brief's `instruction_sha256`, which hashes the
assembled instruction; same function, different input, and the ledger never
stores the text.

**Issue key** = `repo_key` + `Task.IssueURL`, present only when the task
carries an issue URL. The URL comes from the operator's `--issue` flag; the
issue author cannot change it by editing the issue.

**A denial** is a human `drydock deny` (or the web UI's deny) resolving the
**diff** gate: `gateCause == gateDenied` out of `gatePushMarked`, on the live
path (`pushAndOpenPR`, which serves both the synchronous and the queued run)
and on the resume path (`reconcile.go: resumePush`). Nothing else counts:

- a timeout auto-deny (`gateTimeout`) is the daemon's absence, not a verdict;
- a kill or shutdown (`gateKilled`, `gateShutdown`) is not a verdict;
- an egress-widening denial is about reach, not the change;
- `policy_blocked`, `verify_failed`, and `push_failed` are broker findings,
  not operator rejections;
- the backstop's own auto-deny is a broker decision and writes nothing.

**An approval** is a human `approve` resolving the diff gate (`gateApproved`
from a real `awaitGate` wait, never the auto-approve short-circuit). It is
recorded when the human decides, before the push runs: the human accepted
the change, landed or not, and that is what resets the count. Auto-approve
writes nothing, so a feeder running with `--auto-approve` cannot erase a
human's denials by producing a diff one byte different.

**The count** for a key is the number of denials recorded after its most
recent approval. There is no time window. Both of a verdict's keys move
together: a denial increments both, an approval resets both.

## The rejection ledger

Path: `<audit_root>/rejections/ledger.jsonl`, in its own `0700` directory,
file `0600`, created on first write. It is NOT at the audit root: five
consumers glob `*.jsonl` there and parse each hit as a task trace, and the
boot sweep `TerminateStuckAudits` would append an `interrupted` result line
into any file it found without a broker result row. All of them skip
directories (the global ledger lives in `<audit_root>/global/` for the same
reason). `drydock prune` only removes `<hex-id><suffix>` files and never
touches it.

Append-only, one JSON object per line, every field broker-authored:

| Field | Type | Meaning |
|---|---|---|
| `kind` | `"denied"` \| `"approved"` | the human verdict |
| `at_ms` | int64 | broker clock (`b.nowMs()`), never the wall clock inside the store |
| `task_id` | string | the task the verdict was for (32 hex, validated on read and write) |
| `repo_key` | string | canonical lowercase `host/owner/repo` |
| `instruction_sha256` | string | hash of the original instruction (see "The keys") |
| `issue_url` | string | the task's issue URL, or `""` |
| `diff_sha256` | string | `trustbrief.HashDiff` of the gated diff |
| `path` | `"live"` \| `"queue"` \| `"resume"` | which gate path wrote it; display only |

No instruction text, no diff text, no agent text, no CI text.

**Write.** `O_APPEND` + `fsync` per line, `O_NOFOLLOW`, under the ledger's
own mutex (never `queueMu`, which the dispatcher holds while it consults the
index). The in-memory index is updated only after the line is durably on
disk. A failed append is logged at warn and the verdict is lost, which can
only under-count: a lost denial weakens the bound by one and a lost approval
leaves a count one too high, where the sync path remains the override.
Neither failure changes the task's own recorded outcome. The append happens
before the terminal `result` event is emitted to the submit stream, so a
feeder reacting to the stream cannot enqueue ahead of the count moving.

**Read.** Loaded once at boot into the index. A missing file is an empty
ledger, not a fault. A torn trailing line (a crash mid-append) is dropped
with a warning. Any other unparseable or invalid line (bad `kind`, a
`task_id` that is not 32 hex) marks the ledger **degraded** with a path-free
reason naming the line number; see "Failure direction". There is no
quarantine or self-repair in this increment: the reason tells the operator
which line to fix or remove, and the next boot re-reads.

**Index** (in memory, rebuilt at boot, updated on every append, its own
mutex):

- per instruction key and per issue key: `denials` (count since the last
  approval) and `denied_ids` (newest first, capped at 10 for display);
- per `repo_key`: `denied_diffs`, a map from `diff_sha256` to the task id
  that was denied with it. An `approved` entry removes its own
  `diff_sha256` and resets both of its keys' counts.

**Growth.** One line per human gate verdict. No compaction in this
increment; the terminal-record prune sweep (a later 5C item) is the natural
home for a rewrite, and the format (self-contained lines) leaves it that
option.

## The identity guard

`Broker.Enqueue` consults the index after its existing validation and before
it mints an id. With `max_denials > 0`: if the ledger is degraded it returns
a degraded error; else if the larger of the two keys' counts has reached
`max_denials` it returns a loop error carrying the repo key, the issue URL
(if any), the count, the bound, and the merged `denied_ids`. `HandleQueueAdd`
maps the loop error to **HTTP 409** with a JSON body:

```json
{"error":"rejection_loop","repo":"github.com/o/r","issue_url":"https://github.com/o/r/issues/42",
 "denials":2,"max_denials":2,"denied_task_ids":["<id>","<id>"],
 "hint":"this work was denied 2 times at the diff gate; change the instruction, or run it synchronously with drydock submit to override"}
```

Nothing is persisted and no VM argv is ever built. brokerd logs one warn line
per refusal with the same fields. `drydock queue add` prints the hint, the
repo, and the denied ids, and exits non-zero.

**Dispatch-time re-check.** `takeDispatchable` applies the same guard to each
still-queued item before it touches the spend cap, the global ceiling claim,
or the slot, so a refusal leaks no claim. It exists for two reasons: an item
enqueued before its key's last denial landed must not run on the strength of
having beaten the bound to the queue, and `ResumeQueue` re-appends surviving
`queued` items at boot without going through `Enqueue`. A tripped item is
dropped `queued -> dead_letter` with `last_error` = `"dropped before
dispatch: this work has been denied N times at the diff gate since its last
approval (queue.max_denials M; denied tasks: ...)"`, so it shows in
`drydock queue list`'s REASON column. This is a second writer of the
`queued -> dead_letter` edge (today only the spend-capped CI retry drop uses
it); the comment on `validTransition` names both.

**Stated residual: same-key items already in flight.** The guard sees the
ledger, not the live task table. N same-key items enqueued at count 0 all
pass and up to `max_concurrent_tasks` of them run before any gate resolves,
each reaching its own gate. The real bound on spend per key is therefore
`max_denials + max_concurrent_tasks` runs, and a denial never kills a task
that is already running.

**CI retry children.** `maybeEnqueueCIRetry` consults the guard as a gate
of its own, **before** it writes the enqueue-once mark: a loop error refuses
the retry terminally (`retry_detail` records it); a degraded ledger parks the
decision exactly as the ceiling's unmeasured branch does (`CIRetryDeferred`,
bounded by the same park bound), so a transient fault does not end a chain.

## The diff backstop

In `pushAndOpenPR`, the broker computes the diff facts once, and if the diff
is not truncated looks `facts.SHA256` up in the repository's `denied_diffs`
**before** the brief is written, so the brief on disk at gate time carries
the match. The capture fails closed on an oversized diff rather than truncating, so a truncated diff cannot reach this point; the `Truncated` check stays as a defensive guard.

**Queue path** (the run was dispatched by `runQueued`, marked by a
`fromQueue` flag on the run; `onAwaitingReview` is not used as the
discriminator) with `max_denials > 0`: the task terminates after the
diff-policy caps and before the second-look computation, the auto-approve
branch, and the gate. The broker appends a synthetic result row, mirroring
the `policy_blocked` pattern:

```
{"type":"result","subtype":"denied","repeat_of":"<earlier task id>","is_error":false,...,"src":"broker"}
```

sets the run's outcome to `denied`, emits the stream `result` event with
`repeat_of`, and records `repeat_of` on the metrics row
(`internal/audit.Metrics`). The queue item terminates `dead_letter` (not
`cancelled`: nobody cancelled it, and the repo's own rule is that a broker
decision nobody asked for is `dead_letter` with the reason) with
`last_error` = `"auto-denied: identical to the diff denied in task <id>"`.
`drydock stats` and `drydock tasks` classify it as `denied` from the audit.
This runs even when the queued task carries `auto_approve`: a diff a human
already rejected is not something a headless flag may push.

**Synchronous path**: the gate is posed as today. The match is surfaced as
`Diff.RepeatOfDenied` in the trust brief, as `repeat_of` on the
`awaiting_approval` stream event, as a `REPEAT` line in `drydock review` and
`drydock inspect`, and as a chip in the web UI brief panel. If the operator
approves it anyway, the `approved` entry removes the hash from
`denied_diffs`. With `--auto-approve` on the sync path the diff pushes and
nothing is written (auto-approve is not a human verdict), so the hash stays
denied.

**Stated limit.** The hash is over the captured unified diff, so a one-byte
change evades the backstop. The identity guard is the bound; the backstop
exists for the common case of a resubmission that reproduces the previous
change exactly.

## Failure direction

- **Ledger degraded at boot** (unparseable or invalid line, unreadable
  file, file over the 64 MiB read cap) **with `max_denials > 0`**: `Enqueue`
  refuses with a typed error that `HandleQueueAdd` maps to **HTTP 503**
  `{"error":"rejection_ledger_degraded","reason":"line 7 is not a ledger
  entry"}`; `takeDispatchable` parks every queued item (no drop) and logs the
  park once per process, not per item per tick. The backstop looks nothing
  up; the human gate still stands. brokerd logs the degraded state at boot,
  `GET /healthz` carries it as `rejection_ledger_error`, and `drydock status`
  prints it with the file path. Only a repair clears it: fix or remove the
  named line (or the file) and restart. 503 rather than the ceiling's 402
  because this is not a budget refusal and the CLI keys on the JSON `error`
  field, not the status.
- **`max_denials: 0`**: off means off, even with a degraded ledger (the
  ceiling's "off is identity" rule). No 503, no park, no auto-deny. The
  ledger is still written and the sync-path warning still shows, so turning
  the knob back on later has history to work from.
- **Append fails**: logged, verdict lost, task outcome unchanged (above).
- **Crash between gate resolution and the append.** `gatePushMarked`
  removes the gate marker before it returns the cause, and the append
  follows, so a SIGKILL in that window loses the verdict with nothing to
  resume. This is the under-count direction and is accepted; recording
  before the marker is removed would make a resumed gate double-count the
  same change.

## Configuration

One new block, mirroring `ci:`:

```yaml
queue:
  max_denials: 2   # refuse a queue add whose work has been denied this many times at the diff gate since its last approval; 0 = off; max 10
```

Env override `DRYDOCK_QUEUE_MAX_DENIALS`. The config package's `validate`
rejects values outside 0..10. `drydock policy explain` lists it with
provenance; the shipped `config/config.yaml` and the strict known-fields
loader both gain the block.

## Resume path

`gateMarker` gains `root_instruction` and `issue_url` (both `omitempty`),
written beside `instruction`, so a task resumed after a restart computes the
same keys the live path did. A marker without them (written by an older
brokerd) keys on `instruction` alone, which is right for every non-retry,
non-issue task. `resumePush` records the verdict from the marker's fields
plus the hash of the diff it re-read.

## Surfaces touched

- `internal/broker`: `rejections.go` (ledger, index, errors, guard),
  `Enqueue`, `takeDispatchable`, `runQueued`, `maybeEnqueueCIRetry`,
  `pushAndOpenPR`, `writeBrief` (takes precomputed facts), `resumePush`,
  `gateMarker`, `gatePushMarked`, `HandleTask` (zeroes `root_instruction`
  like `HandleQueueAdd`), `HandleQueueAdd`, `HandleHealth`, the `taskRun`
  fields `rootInstruction`/`fromQueue`/`repeatOf`/`autoDenied`, the
  `validTransition` comment.
- `internal/audit`: `Metrics.RepeatOf`.
- `internal/trustbrief`: `DiffFacts.RepeatOfDenied` (additive, `omitempty`),
  `HashDiff`.
- `internal/config`: `QueueConfig{MaxDenials}`, env override, validation,
  defaults, explain row, the shipped `config/config.yaml` block.
- `cmd/brokerd`: open the ledger at boot (before `ResumeAwaiting`), wire
  `MaxDenials`, log degraded.
- `cmd/drydock`: `queue add` 409/503 rendering; `review`/`inspect` `REPEAT`
  line; `status` degraded-ledger line (from `/healthz`); `policy explain`
  row via config.
- `internal/webui`: brief panel chip for `repeat_of_denied`.
- Docs (`site/docs/*.md`, HTML regenerated on deploy): `configuration.md`
  (new section), `submitting-tasks.md` (deny semantics, `repeat_of`),
  `daemon.md` (the unattended loop bound and its residual), `troubleshooting.md`
  (409/503 from `queue add`, repairing a degraded ledger). `THREAT_MODEL.md`
  gains a note under N4 stating what the ledger holds, that an issue author
  cannot trip or clear it, and the two stated limits (exact-hash backstop;
  same-key in-flight residual). `docs/ROADMAP.md` 5C status. `CHANGELOG.md`
  is stamped at release time, not in this PR.

## Tests

Named `TestRejectionLoop_*` and `TestRejectionLedger_*`. The `TestRedteam_`
prefix is reserved for the A-claims `make redteam` collects, and this is an
N4 bound, so it stays out of that namespace. In `internal/broker`:

- `TestRejectionLoop_ThirdEnqueueRefusedBeforeSpend`: deny the same key
  twice through the real gate path, assert the third `POST /queue` is 409
  with both ids, that no queue file exists for it, and that the agent runner
  was invoked exactly twice.
- `TestRejectionLoop_IssueKeyTripsAcrossEditedInstruction`: two denials of
  tasks sharing an issue URL but with different instruction text; a third
  with yet another text and the same URL is refused.
- `TestRejectionLoop_RepeatDiffNeverReposesGate`: a queued task whose
  captured diff hashes to a denied diff terminates `denied` with `repeat_of`,
  writes no gate marker, registers nothing pending, lands `dead_letter` with
  the auto-denied reason, the brief on disk carries `repeat_of_denied`, and
  this holds with `auto_approve: true`.
- `TestRejectionLoop_TruncatedDiffIsNotMatched`: a truncated capture is
  never auto-denied.
- `TestRejectionLoop_DispatchRecheckDropsLateItem`: item enqueued at one
  denial, second denial lands, dispatch dead-letters it with the reason and
  leaks no global-ceiling claim.
- `TestRejectionLoop_RunningItemIsNotKilledByALaterDenial`: the in-flight
  residual, pinned.
- `TestRejectionLoop_HumanApprovalResetsAutoApproveDoesNot`: deny, deny,
  auto-approved sync push (count stays 2, enqueue still refused); human
  approve (count 0, enqueue admitted, that diff freed).
- `TestRejectionLoop_OnlyHumanDiffDenialsCount`: timeout auto-deny, kill,
  shutdown, egress denial, policy_blocked each leave the count unchanged.
- `TestRejectionLoop_SyncPathIgnoresClientRootInstruction` and
  `TestRejectionLoop_RetryChainSharesParentKey` (live write from a
  `runQueued` task built from `QueueItem.Task`, and the resume marker with
  and without the new fields).
- `TestRejectionLoop_RetryChildRefusedOrParked`: `maybeEnqueueCIRetry` under
  a tripped key refuses with `retry_detail` and leaves `ci_retry_enqueued`
  unset; under a degraded ledger it parks.
- `TestRejectionLedger_RoundTripTornTailAndDegraded`: round trip, missing
  file is empty, torn trailing line dropped, malformed middle line and bad
  `task_id` degrade with a line number, degraded index makes `Enqueue`
  return the 503-mapped error and `takeDispatchable` park.
- `TestRejectionLedger_InvisibleToAuditConsumers`: with a ledger present,
  `TerminateStuckAudits` does not touch it, and the audit-dir listings used
  by `drydock tasks`, `drydock status`, `drydock stats`, and the aggregate
  seed see no task named `ledger` or `rejections`.
- `TestRejectionLoop_OffAtZero`: `max_denials: 0` admits and never
  auto-denies, even with a degraded ledger, but the ledger is still written
  and the sync warning shows.
- `TestQueueStateMachineIsForwardOnly` continues to pass unchanged.
- Config: validation bounds, env override, explain row, shipped config
  loads strict. CLI: `queue add` renders 409 and 503 bodies; `inspect`
  prints the `REPEAT` line; `status` prints the degraded line. Web UI: the
  asset carries the chip.

## Out of scope

Stale-base handling, `needs_input` and notifications, the terminal
queue-record prune sweep and any ledger compaction or self-repair (each a
separate 5C item); a rolling denial window; a `drydock queue forget`
command; fuzzy or per-file diff similarity; declining dispatch while a
same-key task is live; applying the identity guard to the synchronous path
or to `drydock retry` (whose resubmission of a retry child records under the
child's assembled-instruction key, since the CLI carries no root field).
