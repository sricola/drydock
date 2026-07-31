# Orchestration Engine — Durable Queue + Core State Machine (Increment A)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** A durable task queue with a per-task state machine that survives daemon restart. Submissions to a new detached `POST /queue` persist to disk, run when a concurrency slot frees (respecting `MaxConcurrent` and the aggregate-spend ceiling), and resume on boot. States: `queued → preparing → running → verifying → awaiting_review → completed | dead_letter` (+ `cancelled`). The **existing synchronous `POST /tasks` path is unchanged**; the queue is additive.

**Scope note:** This is Increment A of the orchestration engine. The CI-feedback bounded-retry loop and the refinements (rejection-loop detection, stale-base handling, needs-input, escalation) are Increments B and C. The `dead_letter` state and the attempt-count field are wired here so B/C need no schema change, but auto-retry itself is B.

**Architecture:** A new `internal/broker/queue.go` holds an in-memory ordered queue backed by a durable `<AuditRoot>/<id>.queue.json` (atomicfile + fsync, the full `Task` + state + timestamps + attempts). A dispatcher goroutine drains it: blocking-acquire a slot, park (don't dispatch) when `AggregateExceeded(vendor)`, then run the existing lifecycle body **headlessly** (a discard stream, exactly like `resumePush`). State transitions are persisted. Boot-resume scans the queue files and — the correctness crux — **only re-dispatches genuinely-`queued` items**; `running`/`verifying`/`awaiting_review` items are reconciled against on-disk evidence (never blindly re-run — they may have spent budget or pushed).

**Tech Stack:** Go stdlib, the existing broker lifecycle + atomicfile + reconcile patterns.

## Decision record (locked — from the scout)

- **Additive.** `POST /tasks` keeps its synchronous stream + 503-on-saturation. `POST /queue` is the durable, detached path (returns `{queued, task_id}` immediately, no stream). Both share the same `slots` semaphore → both honor `MaxConcurrent`.
- **Idempotent resume (the crux).** A `<id>.queue.json` in state `queued` (never started) is safe to re-dispatch. A `running`/`verifying` item at crash time must NOT be re-run — reconcile against the audit trace (`TerminateStuckAudits` already writes a synthetic `interrupted` terminal for a crashed run) → mark `dead_letter` (or a terminal state), never re-dispatch. An `awaiting_review` item defers to the existing `ResumeAwaiting` (gate-marker path); the queue file just records the state. Only `queued` re-dispatches.
- **Spend-ceiling parks, doesn't fail.** The dispatcher checks `AggregateExceeded(vendor)` per-vendor before dispatch; if exceeded, the item stays `queued` (parked) and is retried on the next tick (the window slides). A spend-park is NOT an attempt and NOT a dead-letter.
- **Durable-file integrity.** Queue files are `atomicfile.Write` (temp+rename) + fsync, 0600, `O_NOFOLLOW` on read. The queue file is the authoritative requeue record (superset of the gate marker); it persists the FULL Task incl. `AutoApprove` (which the gate marker omits).
- **Cancellation.** `POST /queue/cancel/{id}`: if still `queued` → dequeue + terminal `cancelled` + remove the file (no VM). If running → delegate to the existing kill (canceller map). Restart-safe.

## Global Constraints

- Never double-run a task on resume. A queued item is re-dispatched at most once; a task that reached `running`/`awaiting_review` is reconciled, not re-run. Test this explicitly (a `running` queue file at boot → dead_letter/interrupted, NOT a second VM).
- Queue files: `atomicfile.Write` + fsync, 0600, `O_NOFOLLOW` read, id-shape validated (`^[0-9a-f]{32}$`) before any path use.
- The dispatcher must respect `MaxConcurrent` (never exceed N concurrent lifecycle runs) — reuse the existing `slots` semaphore (blocking acquire in the dispatcher instead of the non-blocking 503). `-race` clean.
- Aggregate-spend parking is per-vendor and re-checked; a parked item never counts as a failed attempt.
- The queue dispatcher runs the SAME lifecycle body as `HandleTask` (no fork of the run logic) — invoke it headlessly with a discard stream, mirroring `resumePush`.
- Boot order: queue-resume runs AFTER `pruneOrphanTasks`/`TerminateStuckAudits` and coordinates with the stage `keep` map (a queued task with a surviving stage isn't reaped).
- Go gate before each commit: `go vet ./...`, `go test -race ./...` (scope per task), `gofmt -l internal/ cmd/` silent, `staticcheck ./...` clean.
- Commit `type(scope): summary`; trailer `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`; no PR footer.

---

### Task 1: `internal/broker/queuestore.go` — durable queue-item file + state machine

**Files:** Create `internal/broker/queuestore.go` + `queuestore_test.go`.

**Interfaces:**
- `type QueueState string` with consts `QueueQueued="queued"`, `QueuePreparing="preparing"`, `QueueRunning="running"`, `QueueVerifying="verifying"`, `QueueAwaitingReview="awaiting_review"`, `QueueCompleted="completed"`, `QueueDeadLetter="dead_letter"`, `QueueCancelled="cancelled"`. A `func (s QueueState) Terminal() bool` (completed/dead_letter/cancelled). A `validTransition(from,to)` table + `func (s QueueState) CanTransitionTo(to) bool` (queued→preparing/cancelled; preparing→running/dead_letter/cancelled; running→verifying/awaiting_review/dead_letter/cancelled; verifying→awaiting_review/dead_letter/cancelled; awaiting_review→completed/dead_letter/cancelled; terminals→nothing).
- `type QueueItem struct { ID string; Task Task; State QueueState; EnqueuedAtMs, StartedAtMs, UpdatedAtMs int64; Attempts int; LastError string }` — the full Task (incl. AutoApprove/PlanOnly/IssueURL) + state + timestamps + attempts.
- `queueMarkerPath(auditRoot, id) string` = `<auditRoot>/<id>.queue.json`.
- `writeQueueItem(auditRoot string, it QueueItem) error` — `atomicfile.Write` MarshalIndent 0600 (+ an fsync of the file via reopening or accept atomicfile's rename durability; if fsync-of-dir is needed for crash-safety, note it — atomicfile's rename is the primary guarantee).
- `readQueueItem(auditRoot, id) (QueueItem, error)` — id-shape validated, `O_NOFOLLOW` read.
- `removeQueueItem(auditRoot, id) error` — idempotent on NotExist.
- `listQueueItems(auditRoot) ([]QueueItem, error)` — scan `.queue.json`, skip unparseable with a warn (don't fail the whole scan), sorted by EnqueuedAtMs.

- [ ] **Step 1: Failing tests** — `queuestore_test.go`: round-trip write/read (full Task fields incl. AutoApprove preserved); id-shape rejection on read; list scans + sorts + tolerates a garbage file; `CanTransitionTo` table (valid + invalid transitions, terminals are sinks); Terminal(). Since timestamps come from the caller (pass them in — no time.Now in the store funcs so tests are deterministic; the broker passes the clock), assert the stored ms values round-trip.
- [ ] **Step 2:** fail. **Step 3:** implement. **Step 4:** `go test -race ./internal/broker/ -run Queue`. **Step 5:** commit `feat(broker): durable queue-item store + state machine`.

---

### Task 2: the queue + dispatcher

**Files:** Modify `internal/broker/broker.go` (Broker gains the queue + dispatcher; a `runQueued` that invokes the lifecycle headlessly), create `internal/broker/queue.go` (the queue struct + dispatcher loop) + `queue_test.go`.

**Interfaces / behavior:**
- `Broker` gains: `queueMu sync.Mutex`, `queue []QueueItem` (in-memory ordered, the durable file is the backing store), a `queueTick` interval, a `queueStop chan struct{}`, and the dispatcher.
- `(b *Broker) Enqueue(t Task) (id string, err error)` — mints an ID, builds a `QueueItem{State: QueueQueued, EnqueuedAtMs: now}`, `writeQueueItem`, appends to the in-memory queue under `queueMu`, signals the dispatcher. Returns the id. (Same validation as HandleTask: RepoRef regex, egress domains.)
- `(b *Broker) StartDispatcher()` / a dispatcher goroutine: loops on a ticker + a wakeup channel; on each tick, under `queueMu` scan for the oldest `queued` item whose vendor is NOT `AggregateExceeded` and for which a slot is available; to dispatch: `acquireSlot()` (blocking-wait variant OR the existing non-blocking + retry-next-tick — simplest: try the non-blocking acquireSlot; if it fails, leave queued for the next tick), transition the item `queued→preparing` (persist), and `go b.runQueued(item)`. Never exceed MaxConcurrent (the slot semaphore guarantees it). Parked (spend-exceeded) items stay queued.
- `(b *Broker) runQueued(it QueueItem)` — the headless lifecycle: build a `taskRun` from `it.Task` with `newDiscardStream()`, register the task, run the SAME lifecycle steps `HandleTask` runs after the accept (egress gate → prepare → setup → run → verify → gate → push), persisting the queue state at each major transition (preparing→running→verifying→awaiting_review→completed/dead_letter) via a small `b.setQueueState(id, state)` helper. On a terminal outcome, set `completed` (for pushed/no_diff/planned — a normal finish) or `dead_letter` (for error/setup_failed/push_failed/policy_blocked/verify_failed — a failure that in Increment B would trigger retry, but in A terminates as dead_letter with LastError). `releaseSlot` on exit. Remove/retain the queue file per terminal policy: keep the file with the terminal state for `drydock queue list` history (a prune sweep can clean terminals later — or remove on completed, keep on dead_letter; decide and test).
   - IMPORTANT: to avoid forking the lifecycle, factor the post-accept body of `HandleTask` into a reusable `(tr *taskRun) runLifecycle()` (or similar) that BOTH `HandleTask` (with the real stream) and `runQueued` (with the discard stream) call. This refactor must be behavior-preserving for the existing synchronous path — assert existing HandleTask tests still pass unchanged.
- `(b *Broker) cancelQueued(id) bool` — under queueMu: if the item is `queued` (not yet dispatched), transition `cancelled`, persist, remove from the in-memory queue, return true. If already running (has a canceller), return false (caller delegates to kill).

- [ ] **Step 1: Failing tests** (`queue_test.go`, reuse the handle_task harness seams): 
  - `TestQueue_DrainsRespectingConcurrency` — enqueue K > MaxConcurrent; assert at most MaxConcurrent lifecycle runs are in-flight at once (use a fake runAgent that blocks on a barrier and counts concurrent entries), the rest stay queued, all eventually complete.
  - `TestQueue_ParksOnAggregateExceeded` — stub `AggregateExceeded` to return true for the task's vendor → the item stays queued (not dispatched, not failed); flip it false → it dispatches. Assert the parked item's Attempts stays 0.
  - `TestQueue_CompletedAndDeadLetterStates` — a task that pushes → `completed`; a task whose runAgent errors → `dead_letter` with LastError.
  - `TestQueue_CancelQueuedBeforeDispatch` — cancel a queued item → `cancelled`, never dispatched (fake runAgent never called for it).
  - `TestHandleTask_LifecycleUnchanged` — the refactor didn't change the synchronous path (an existing HandleTask test still passes; add an explicit assertion the accept→terminal event sequence is identical).
  - Run with `-race`.
- [ ] **Step 2:** fail. **Step 3:** implement (the `runLifecycle` refactor first, behavior-preserving, then the queue). **Step 4:** `go test -race -count=1 ./internal/broker/`. **Step 5:** commit `feat(broker): durable queue + dispatcher (concurrency + spend-ceiling parking)`.

---

### Task 3: boot resume + idempotency (the correctness crux)

**Files:** Modify `internal/broker/reconcile.go` (a `ResumeQueue`), `cmd/brokerd/main.go` (call it at boot in the right order); tests in `reconcile_test.go`.

**Behavior:**
- `(b *Broker) ResumeQueue(stageRoot string) error` — scan `listQueueItems(auditRoot)`; for each:
  - `QueueQueued` → re-append to the in-memory queue (the dispatcher will run it). Safe: never started.
  - `QueuePreparing`/`QueueRunning`/`QueueVerifying` → **do NOT re-dispatch.** Reconcile against evidence: the audit `.jsonl` — if it has a terminal `result` line (or `TerminateStuckAudits` wrote `interrupted`), mark the queue item `dead_letter` with LastError "interrupted by restart" and persist; the VM/stage are reaped by the existing `pruneOrphanTasks`. (Increment B will convert this to a bounded retry; A dead-letters.) Never boot a second VM for a task that may have already run/spent/pushed.
  - `QueueAwaitingReview` → defer to `ResumeAwaiting` (the gate marker drives the resume); the queue item stays `awaiting_review`. Do NOT re-dispatch. (Coordinate: don't double-handle — `ResumeAwaiting` already re-drives the gate; the queue file is just the state record. Ensure the gate resolution updates the queue state to completed/etc. — thread a queue-state update into the resume path, or accept that the queue file lags for awaiting_review items and is reconciled next boot. Simplest correct: on `awaiting_review`, ResumeQueue does nothing (ResumeAwaiting owns it); a later gate resolution updates the queue file. Document the coordination.)
  - Terminal states (`completed`/`dead_letter`/`cancelled`) → leave as-is (history).
- `cmd/brokerd/main.go`: call `b.ResumeQueue(cfg.StageRoot)` at boot AFTER `pruneOrphanTasks`/`TerminateStuckAudits` and after/around `ResumeAwaiting` (order so the `keep` map protects queued/awaiting stages). Then `b.StartDispatcher()`.

- [ ] **Step 1: Failing tests** (`reconcile_test.go`, mirror `ResumeAwaiting` tests):
  - `TestResumeQueue_QueuedItemRedispatched` — a `queued` queue file at boot → after ResumeQueue + a dispatcher tick, the task runs (fake runAgent called once). Exactly once (idempotency: run ResumeQueue twice → still one dispatch, or the second finds it already in-memory/terminal).
  - `TestResumeQueue_RunningItemDeadLettersNeverReRuns` (THE crux test) — a `running` queue file at boot whose audit has no terminal line → ResumeQueue marks it `dead_letter` (interrupted), and the fake runAgent is NEVER called (no second VM). Assert the dead_letter state + no dispatch.
  - `TestResumeQueue_AwaitingReviewDefersToResumeAwaiting` — an `awaiting_review` queue file → ResumeQueue does not re-dispatch it (ResumeAwaiting owns it); no double-run.
  - `TestResumeQueue_TerminalItemsUntouched` — completed/cancelled files are left as-is.
- [ ] **Step 2:** fail. **Step 3:** implement. **Step 4:** `go test -race ./internal/broker/ ./cmd/brokerd/`. **Step 5:** commit `feat(broker): idempotent queue boot-resume — re-dispatch queued, dead-letter interrupted, defer awaiting-review`.

---

### Task 4: HTTP endpoints + CLI

**Files:** Modify `cmd/brokerd/main.go` (routes), `internal/broker/admin.go` (`HandleQueueAdd`/`HandleQueueList`/`HandleQueueCancel`), `cmd/drydock/queue.go` (new) + `cmd/drydock/main.go` (dispatch/subHelp/usage), `cmd/drydock/submit.go` (share the taskRequest builder); tests.

**Interfaces:**
- `POST /queue` → `HandleQueueAdd`: decode a `Task` (same body as `/tasks`, `MaxBytesReader` cap), validate, `id := b.Enqueue(t)`, `writeJSON({event:"queued", task_id:id})`. No stream.
- `GET /queue` → `HandleQueueList`: `writeJSON(listQueueItems(...))` (or a projected view: id/repo/state/enqueued/attempts). Behind the same auth as `/admin/*`.
- `POST /queue/cancel/{id}` → `HandleQueueCancel`: `if b.cancelQueued(id) { 204 }` else `if canceller exists { delegate to HandleKill logic } else 404`.
- CLI `drydock queue add <same flags as submit>` — reuse the submit flag parsing + issue ingestion + taskRequest builder; POST to `/queue`; print the queued id + "drydock queue list" hint. `drydock queue list` — GET /queue, render a table (ID/STATE/AGE/ATTEMPTS/REPO). `drydock queue cancel <id>` — POST /queue/cancel.
- Factor the submit-flag→taskRequest logic so `queue add` and `submit` share it (avoid duplicating --issue/--plan/--egress-extra parsing).

- [ ] **Step 1: Failing tests** — broker admin tests (HandleQueueAdd enqueues + returns id; HandleQueueList returns items; HandleQueueCancel dequeues a queued item / 404 unknown); CLI queue_test (queue add posts the task body; list renders; the submit-flag sharing). Add "queue" to `dispatchedCommands`.
- [ ] **Step 2:** fail. **Step 3:** implement. **Step 4:** `go test -race ./internal/broker/ ./cmd/brokerd/ ./cmd/drydock/`. **Step 5:** commit `feat(cli,brokerd): queue add/list/cancel endpoints + subcommands`.

---

### Task 5: metrics, web UI, docs

**Files:** `internal/audit/audit.go` (`StageMs.Queued` omitempty + `completed`/`dead_letter` outcomeString), `internal/broker/metrics.go` (QueuedMs), `internal/stats/stats.go` (queue-latency percentile), `internal/webui/assets/app.js` (`queued`/`dead_letter` stage/outcome labels), `cmd/drydock/tasks.go`/`stats.go` (render new outcomes), `site/docs/submitting-tasks.md` + a queue section, `CHANGELOG.md`; tests.

**Behavior:**
- `StageMs.Queued int64 \`json:"queued,omitempty"\`` = time from enqueue to run start (0 for non-queued tasks — back-compat). `appendMetrics` sets it from the queue timestamps when the task came from the queue.
- `audit.outcomeString`: `completed`→"completed", `dead_letter`→"dead-letter". `stats` fixed list gains them; queue-latency P50/P95 in the Summary.
- Web UI: `queued` in the stage label map + active-stages; `dead_letter`/`completed` outcome rendering.
- Docs: a "Queue (durable, unattended)" section in `submitting-tasks.md` (`drydock queue add` vs `submit`; the state machine; concurrency + spend parking; survives restart; cancel). CHANGELOG. Docs-drift sentinel check.

- [ ] **Step 1: Failing tests** — QueuedMs in the metrics row for a queued task; outcomeString for completed/dead_letter; a stats queue-latency assertion. **Step 2:** fail. **Step 3:** implement; `node --check app.js`. **Step 4:** FULL gate: `go vet ./...`; `go test -race -count=1 ./...`; `gofmt -l internal/ cmd/` silent; `staticcheck ./...` clean; `go test ./cmd/docs-build/`; `node --check internal/webui/assets/app.js`. **Step 5:** commit `feat(metrics,webui,docs): queue latency + completed/dead_letter surfacing`.

---

## Final verification (whole branch)

- Full gate green.
- **No-double-run trace:** a `running`/`verifying` queue file at boot never boots a second VM (dead-letters); only `queued` re-dispatches, at most once.
- **Concurrency trace:** the dispatcher never exceeds MaxConcurrent (shares the slot semaphore); `-race` clean.
- **Spend-park trace:** an aggregate-exceeded vendor's items stay queued (parked), don't fail, don't count as attempts, re-dispatch when the window slides.
- **Additive trace:** `POST /tasks` synchronous path byte-identical (the `runLifecycle` refactor is behavior-preserving; existing HandleTask tests pass unchanged).
- **Durability:** queue files atomicfile+fsync, 0600, O_NOFOLLOW, id-validated.
- PR notes: Increment A of the orchestration engine; CI-feedback retry (B) and rejection-loop/stale-base/escalation (C) are the next increments; `dead_letter` + attempts are wired for B.
