# Rejection-loop detection (roadmap 5C, first item)

Date: 2026-10-08
Status: approved design, pre-implementation

## Goal

An unattended daemon fed a queue of work must not keep spending on a change
the operator has already rejected. Today a diff denied at the approval gate
ends that task and nothing inside brokerd re-enqueues it, so the loop is
driven from outside: a feeder script re-adding the same issue with
`drydock queue add --issue`, a hand resubmit, or a bounded CI retry chain
whose every attempt is denied. This increment gives the broker a durable
memory of human denials and uses it in two places: before any spend, to
refuse a queue add for work that has been denied repeatedly, and after a
run, to stop a queued task from re-posing a diff the operator has already
turned down. Closes the first of the four 5C refinements in
`docs/ROADMAP.md`.

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
- **Reset by a changed instruction or a later approval.** The key includes a
  hash of the original instruction, so an edited issue or reworded task is a
  new key by construction; an approved push on a key resets its count. No
  rolling window, no clear command.
- **Backstop: auto-deny on the queue path, warn on the sync path.** A queued
  task whose diff matches a denied diff for the repository terminates as
  `denied` before the gate. A synchronous task still poses the gate, with the
  match shown to the reviewer.
- **History lives in a dedicated ledger**, not derived from the audit dir or
  from queue records: both of those are pruned, and neither covers every
  gate (see "Why a ledger").
- **Default on, `max_denials: 2`.** This deliberately breaks Phase 5's
  opt-in rule. The guard fires only after two human denials of the same work
  on the unattended path, which is exactly the loop the phase exists to stop;
  `0` turns it off.

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
   path (live, queued, and resumed after a restart). It is the idiom the
   global usage ceiling already trusts (`internal/broker/globalledger.go`),
   without that ledger's rolling window, compaction, or clock correction.

Option 3 is the design.

## The key, and what counts

**Key** = `repokey.Normalize(repo_ref)` + `"\n"` + SHA-256 of the **original
instruction**: `Task.RootInstruction` when non-empty (a CI retry child),
otherwise `Task.Instruction`. A retry chain therefore shares one key with
its parent, and the chain's denials accumulate on it. The hash is the one
`trustbrief.HashInstruction` already computes for the brief; the text itself
is never stored in the ledger.

**A denial** is a human `drydock deny` (or the web UI's deny) resolving the
**diff** gate: `gateCause == gateDenied` out of `gatePushMarked`, on the live
path (`pushAndOpenPR`) and on the resume path (`reconcile.go: resumePush`).
Nothing else counts:

- a timeout auto-deny (`gateTimeout`) is the daemon's absence, not a verdict;
- a kill or shutdown (`gateKilled`, `gateShutdown`) is not a verdict;
- an egress-widening denial is about reach, not the change;
- `policy_blocked`, `verify_failed`, and `push_failed` are broker findings,
  not operator rejections.

**An approval** is `gateApproved` on the same two paths, including the
auto-approve branch: the work landed, so the key's count resets. On the
queue path a diff identical to a denied one never reaches the auto-approve
branch (see "The diff backstop").

**The count** for a key is the number of denials recorded after its most
recent approval. There is no time window.

## The rejection ledger

Path: `<audit_root>/rejections.jsonl`, `0600`, created on first write.
Append-only, one JSON object per line, every field broker-authored:

| Field | Type | Meaning |
|---|---|---|
| `kind` | `"denied"` \| `"approved"` | the gate verdict |
| `at_ms` | int64 | broker clock (`b.nowMs()`), never the wall clock inside the store |
| `task_id` | string | the task the verdict was for (32 hex, validated on read) |
| `repo_key` | string | `repokey.Normalize(repo_ref)` |
| `instruction_sha256` | string | hash of the original instruction (see "The key") |
| `diff_sha256` | string | `trustbrief.DiffFacts.SHA256` of the gated diff |
| `path` | `"live"` \| `"queue"` \| `"resume"` | which gate path wrote it; display only |

No instruction text, no diff text, no agent text, no CI text.

**Write.** `O_APPEND` + `fsync` per line, under the broker's existing
`O_NOFOLLOW` discipline. A failed append is logged at warn and the verdict
is lost, which can only under-count: a lost denial weakens the bound by one
and a lost approval leaves a count one too high, where the sync path remains
the override. Neither failure changes the task's own recorded outcome.

**Read.** Loaded once at boot into the in-memory index below. A torn
trailing line (a crash mid-append) is dropped with a warning; any other
unparseable line, or a file that cannot be opened at all, marks the ledger
**degraded** with the error text and the guards fail closed (see "Failure
direction"). Both guards read only the index; nothing on a task path reads
the file.

**Index** (in memory, rebuilt at boot, updated on every append):

- per key: `denials` (count since the last approval) and `denied_ids` (the
  task ids of those denials, newest first, capped at 10 for display);
- per `repo_key`: `denied_diffs`, a map from `diff_sha256` to the task id
  that was denied with it. An `approved` entry removes its `diff_sha256`
  from the map and resets its key's count.

**Growth.** One line per human gate verdict. There is no compaction in this
increment; the terminal-record prune sweep (a later 5C item) is the natural
home for a rewrite that folds superseded entries, and the format above
(self-contained lines, no checkpoints) leaves it that option.

## The identity guard

`Broker.Enqueue` consults the index after its existing validation and before
it mints an id. When the key's `denials >= max_denials`, it returns a typed
error carrying the repo key, the count, the bound, and `denied_ids`.
`HandleQueueAdd` maps that error to **HTTP 409** with a JSON body:

```json
{"error":"rejection_loop","repo":"github.com/o/r","denials":2,"max_denials":2,
 "denied_task_ids":["<id>","<id>"],
 "hint":"this instruction was denied 2 times at the diff gate; change the instruction, or run it synchronously with drydock submit to override"}
```

Nothing is persisted and no VM argv is ever built. brokerd logs one warn line
per refusal with the same fields. `drydock queue add` prints the hint and the
denied ids and exits non-zero.

**Dispatch-time re-check.** An item enqueued before the key's second denial
landed must not run on the strength of having beaten the bound to the queue.
`takeDispatchable` applies the same check before claiming a slot. A tripped
item is dropped `queued -> dead_letter` with `last_error` =
`"dropped before dispatch: this instruction has been denied N times at the
diff gate since it was enqueued"`, so it shows in `drydock queue list`'s
REASON column. This is a second writer of the `queued -> dead_letter` edge
(today only the spend-capped CI retry drop uses it); the comment on
`validTransition` is updated to name both.

A bounded CI retry child goes through `Enqueue` like any item, so a chain
whose attempts keep being denied stops at the bound with the refusal
recorded in the parent's `ci_observation` row via the existing
`retry_detail` field.

## The diff backstop

In `pushAndOpenPR`, after `checkDiffCaps` and before the second-look
computation and the auto-approve branch, the broker looks up
`facts.SHA256` in the repository's `denied_diffs`.

**Queue path** (the run was dispatched by `runQueued`): the task terminates
before the gate. The broker appends a synthetic result row, mirroring the
`policy_blocked` pattern:

```
{"type":"result","subtype":"denied","repeat_of":"<earlier task id>","is_error":false,...,"src":"broker"}
```

sets `tr.outcome = "denied"`, emits the stream `result` event with
`repeat_of`, and records `repeat_of` on the metrics row. The queue item
takes the existing `denied -> cancelled` terminal mapping, with
`last_error` = `"auto-denied: identical to the diff denied in task <id>"`.
No ledger entry is written for an auto-deny: the count is a count of human
verdicts, and the earlier denial already holds the hash. `drydock stats`
counts it under `denied` unchanged. This check runs even when the queued
task carries `auto_approve`: a diff a human already rejected is not
something a headless flag may push.

**Synchronous path**: the gate is posed as today. The match is surfaced as
`Diff.RepeatOfDenied` (the earlier task id) in the trust brief, as
`repeat_of` on the `awaiting_approval` stream event, as a warning line in
`drydock review` and `drydock inspect`, and in the web UI brief panel. If
the operator approves it anyway, the `approved` entry removes the hash from
`denied_diffs`.

**Stated limit.** The hash is over the whole unified diff, so a one-byte
change evades the backstop. That is why the identity guard, not the backstop,
is the bound. The backstop exists for the common case of a resubmission that
reproduces the agent's previous change exactly.

## Failure direction

- **Ledger unreadable at boot** (open error, or a malformed line that is not
  a torn tail): the index is marked degraded. `Enqueue` refuses with a typed
  error that `HandleQueueAdd` maps to **HTTP 503** carrying the load error,
  and `takeDispatchable` parks every queued item (no drop) until the next
  boot clears it. This matches the global ceiling's stance: a bound that
  cannot be measured refuses rather than admits. The backstop, reading the
  same degraded index, auto-denies nothing and shows no match; the human
  gate still stands. brokerd logs the degraded state at boot and
  `drydock doctor` reports it.
- **Append fails**: logged, verdict lost, task outcome unchanged (above).
- **`max_denials: 0`**: the identity guard and the queue-path auto-deny are
  off. The ledger is still written and the sync-path warning still shows, so
  turning the knob back on later has history to work from.

## Configuration

One new block, mirroring `ci:`:

```yaml
queue:
  max_denials: 2   # refuse a queue add whose repo+instruction has been denied this many times at the diff gate since its last approval; 0 = off; max 10
```

Env override `DRYDOCK_QUEUE_MAX_DENIALS`. `config.Validate` rejects values
outside 0..10. `drydock policy explain` lists it with provenance like every
other knob. The daemon-divergence check needs no change.

## Resume path

`gateMarker` gains `root_instruction` (`omitempty`), written beside
`instruction`, so a retry child resumed after a restart computes the chain's
key rather than its own assembled instruction's. `resumePush` writes the
ledger entry from the marker's fields plus the diff it re-read. A marker
without the field (written by an older brokerd) falls back to `instruction`,
which is the right key for every non-retry task.

## Surfaces touched

- `internal/broker`: `rejections.go` (ledger + index), `Enqueue`,
  `takeDispatchable`, `pushAndOpenPR`, `resumePush`, `gateMarker`,
  `HandleQueueAdd`, the metrics row (`repeat_of`), `validTransition` comment.
- `internal/trustbrief`: `DiffFacts.RepeatOfDenied` (additive, `omitempty`).
- `internal/config`: `QueueConfig{MaxDenials}`, env override, validation,
  defaults, the shipped `config/config.yaml` comment block.
- `cmd/brokerd`: open the ledger at boot, wire `b.MaxDenials`, log degraded.
- `cmd/drydock`: `queue add` 409/503 rendering; `review`/`inspect` warning
  line; `doctor` degraded-ledger check; `policy explain` row.
- `internal/webui`: brief panel line for `repeat_of_denied`.
- Docs (`site/docs/*.md`, HTML regenerated on deploy): `configuration.md`
  (new section), `submitting-tasks.md` (deny semantics, `repeat_of`),
  `daemon.md` (the unattended loop bound), `troubleshooting.md` (409/503
  from `queue add`, degraded ledger). `THREAT_MODEL.md` gains a note under
  the N-series that ledger entries are broker-authored only and that the
  backstop hash is evadable by design. `docs/ROADMAP.md` 5C status.
  `CHANGELOG.md` is stamped at release time, not in this PR.

## Tests

Red-team style, named to the claim, in `internal/broker`:

- `TestRedteam_RejectionLoop_ThirdEnqueueRefusedBeforeSpend`: deny the same
  key twice through the real gate path (one live, one queued), assert the
  third `POST /queue` is 409 with the two ids, that no queue file exists for
  it, and that the runner's argv builder was never reached.
- `TestRedteam_RejectionLoop_RepeatDiffNeverReposesGate`: a queued task whose
  captured diff hashes to a denied diff terminates `denied` with `repeat_of`,
  writes no gate marker, registers nothing pending, and this holds with
  `auto_approve: true`.
- `TestRejectionLoop_DispatchRecheckDropsLateItem`: item enqueued at one
  denial, second denial lands, dispatch dead-letters it with the reason.
- `TestRejectionLoop_ApprovalResetsKeyAndFreesDiff`: two denials, one
  approval, enqueue admitted; approving a previously denied diff removes it
  from `denied_diffs`.
- `TestRejectionLoop_OnlyHumanDiffDenialsCount`: timeout auto-deny, kill,
  shutdown, egress denial, policy_blocked each leave the count unchanged.
- `TestRejectionLoop_RetryChainSharesParentKey`: a CI retry child's denial
  counts on the parent's key; the resume path with and without
  `root_instruction` in the marker computes the same key.
- `TestRejectionLedger_RoundTripTornTailAndDegraded`: round trip, torn
  trailing line dropped with a warning, a malformed middle line degrades,
  degraded index makes `Enqueue` return the 503-mapped error and
  `takeDispatchable` park.
- `TestRejectionLoop_OffAtZero`: `max_denials: 0` admits and never
  auto-denies, but the ledger is still written and the sync warning shows.
- `TestQueueStateMachineIsForwardOnly` continues to pass unchanged.
- Config: validation bounds and env override; `policy explain` row.
- CLI: `queue add` renders 409 and 503 bodies; `review` prints the warning.

## Out of scope

Stale-base handling, `needs_input` and notifications, the terminal
queue-record prune sweep (each a separate 5C item); a rolling denial window;
a `drydock queue forget` command; fuzzy or per-file diff similarity; applying
the identity guard to the synchronous path or to `drydock retry`.
