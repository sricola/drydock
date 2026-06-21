# drydock submit — streaming UX redesign

- **Date:** 2026-06-21
- **Status:** Design approved; implementation plan pending
- **Scope:** `cmd/drydock/submit.go`, `internal/broker/` (`broker.go` + new `stream.go`)

## Problem

`drydock submit` POSTs a task to brokerd and blocks on a single long-lived HTTP
request until the *entire* task completes — boot → agent run → approval gate →
push — then prints a one-line summary. Four consequences:

1. **Silent block.** No output for the whole run (often tens of minutes). The
   operator can't tell whether the task is booting, running, waiting for their
   approval, or wedged.
2. **Opaque failures.** A sandbox that fails to boot surfaces as
   `HTTP 500: task failed: exit status 1`; the real reason (e.g.
   `entrypoint.sh: line 5: DRYDOCK_GW_IP: missing gateway ip`) lives only in the
   audit `.jsonl`. This exact failure cost real debugging time during the v0.2.0
   image migration.
3. **Invisible approval gate.** When the task reaches the diff-approval gate the
   submit shell shows nothing; the operator must already know to switch shells
   and run `drydock approve <id>`.
4. **Thin summary.** The completion line shows only
   `task <id>: pushed <branch> (<platform>)` — no duration, cost, or diffstat.

## Goals

Address all four with one mechanism, while:

- **Preserving current safety semantics** — the task's lifetime is tied to the
  submit connection, and `^C` is a clean abort (`gatePush`/`gateEgressWiden`
  return `false` on `ctx.Done()`).
- **Remaining backward-compatible** across brokerd/CLI version skew.

## Non-goals

- Changing the approval mechanism itself (`/admin/approve`, `drydock approve`).
- Making tasks outlive the submit client (async/detached execution).
- Streaming raw agent tokens to the client.

## Approach: server-streamed NDJSON over the existing POST

brokerd writes newline-delimited JSON *events* to the `POST /tasks` response as
the task advances, flushing each. The submit client consumes the stream and
renders live progress, the approval prompt, and a rich final summary.

Chosen over two alternatives:

- **Client polls `GET /admin/tasks`.** The client doesn't learn its own
  `task_id` until the POST returns, forcing a correlation hack to match its task
  among concurrent ones; the rich result/error still needs separate work.
- **Async submit + poll.** POST returns `202` immediately and the task runs
  detached. This breaks the "submit owns the task, `^C` aborts" model and makes
  tasks outlive the client — fighting the existing design.

Streaming reuses the connection submit already holds, carries the `task_id` in
the first event (solving correlation), and naturally signals every phase.

## Wire protocol

**Format:** `Content-Type: application/x-ndjson` — one JSON object per line,
each with an `event` field. Streaming begins the moment a task is *accepted*;
failures *before* acceptance keep normal HTTP status codes.

**Event sequence** (a task emits a subset, always ending in exactly one terminal
event — `result` or `error`):

```jsonc
{"event":"accepted","task_id":"7f3a…","repo":"…"}                       // always first
{"event":"stage","stage":"awaiting_egress","extras":"host:443",
   "approve":"drydock approve 7f3a…","deny":"drydock deny 7f3a…"}        // only if widening
{"event":"stage","stage":"running","agent":"claude","model":"…"}        // container up, agent working
{"event":"stage","stage":"awaiting_approval","diff_bytes":1234,"files":4,
   "diff_path":"…","approve":"drydock approve 7f3a…","deny":"drydock deny 7f3a…"}
{"event":"stage","stage":"pushing","branch":"agent/7f3a…"}
// …then exactly ONE terminal event:
{"event":"result","outcome":"pushed","branch":"…","platform":"github",
   "files":4,"insertions":120,"deletions":8,"duration_ms":138000,"cost_usd":0.11}
{"event":"result","outcome":"no_diff|denied|cancelled","duration_ms":…,"cost_usd":…}
{"event":"error","reason":"entrypoint.sh: line 5: DRYDOCK_GW_IP: missing gateway ip",
   "hint":"run `drydock doctor` to check the sandbox image","audit":"…/7f3a….jsonl","duration_ms":707}
```

**HTTP semantics:** once `accepted` is emitted, the status is `200`; the
*outcome* is read from the terminal event, not the status code. Pre-accept
failures (bad request, bad repo URL, slot full → `503`, invalid egress) stay as
`4xx/503` + plain body.

**Exit codes:** `error` → `1`; every other outcome (`pushed`, `no_diff`,
`denied`, `cancelled`) → `0`. (`denied` deliberately exits `0` — an operator
decision, not a failure.)

**Backward compatibility (free):** today's response is a single compact JSON
object (`json.NewEncoder().Encode` → one newline-terminated line) with **no**
`event` field. The client reads line-by-line; a line lacking `event` is treated
as the legacy final result and rendered through the existing `printPretty`. So
new-client↔old-brokerd still works; new↔new gets the full stream.

## Server changes (`internal/broker/`)

**New `stream.go`** — a tiny helper wrapping the writer + flusher:

```go
type stream struct { enc *json.Encoder; f http.Flusher }
func newStream(w http.ResponseWriter) *stream  // NDJSON header, WriteHeader(200), grab Flusher
func (s *stream) emit(ev map[string]any)        // enc.Encode(ev); if s.f != nil { s.f.Flush() }
```

**Emit points** map onto existing transitions in `HandleTask` — the `setStage`
calls are already in the right places; each gains a paired `emit`. No structural
rewrite.

| Current code (broker.go) | Becomes |
|---|---|
| after validation (~L330), before egress gate / clone | `newStream` + `accepted` |
| `setStage(StageAwaitingEgress)` L334 | + `stage:awaiting_egress` (extras summary, approve/deny hints) |
| before run (~L437) | + `stage:running` (resolved agent + model) |
| run error L445–457 | terminal `error` (distilled reason + hint) or `cancelled` result |
| no diff L477 | terminal `result outcome=no_diff` |
| `setStage(StagePending)` L480 | + `stage:awaiting_approval` (diff_bytes, files, diff_path, hints) |
| `setStage(StagePushing)` L492 | + `stage:pushing` (branch) |
| push fail L496 | terminal `error` (reason=safeErr, hint="check the remote and push credentials") |
| pushed L499 | terminal `result outcome=pushed` (diffstat, duration, cost) |

Pre-accept failures (L294–329: bad request, bad repo, slot full, invalid egress)
stay as `http.Error` — they're before the stream starts.

**Failure distillation** (the actionable-error facet): for the boot-fail case,
set `reason` from the **last non-progress line of the audit `.jsonl`**, reusing
the `lastLine`/`claudeVersionLine` pattern from `cmd/drydock/doctor.go` (strips
the `[n/6]` image-pull preamble). That turns `exit status 1` into the real
entrypoint message. Set `hint = "run `drydock doctor` to check the sandbox
image"` when the container died before emitting a real `result` success event
(the broker already synthesizes a synthetic error `result` at L454, so "never
produced a real result" is a known signal). `safeErr` bounds are preserved —
last line only, never the whole log.

**Data sources, all already present:**

- `duration_ms` = `time.Since(taskStart)` (taskStart at L437).
- `cost_usd` = `grant.Spent()` (the gateway-metered spend; codex already uses it
  at L468 — authoritative and uniform across agents).
- `files`/`insertions`/`deletions` = a small, testable `diffStat(diff string)`
  over the diff string already captured by `CaptureDiff` (L471).

The raw agent stream-json is **not** teed to the client in core (verbose); the
client shows phase + locally-tracked elapsed time.

## Client changes (`cmd/drydock/submit.go`)

`submit.go` becomes a stream consumer — replace the `ReadAll` + `printPretty`
block (L230–257) with a `bufio.Scanner` loop decoding each line and dispatching
on `event`. The dispatch core is a **pure function**
`render(ev map[string]any, mode) (lines []string, done bool, exit int)` so it
unit-tests without a TTY; the spinner/carriage-return is a thin TTY-only wrapper.

**Render modes:**

- **TTY (interactive):** an in-place updating status line + spinner + local
  elapsed time; the approval block printed persistently; the final summary on
  the terminal event. Reuses init.go's `tty`/`NO_COLOR` detection and the
  existing `step()` color style.
- **Piped (non-TTY):** one clean line per event, no spinner/CR — CI/log friendly.
- **`--json`:** raw NDJSON passthrough to stdout (a superset of today's `--json`;
  old brokerd → the single legacy object, unchanged).
- **`--quiet`** (new): suppress progress, print only the terminal summary line.

**Approval block** (rendered on `awaiting_approval`; hints come from the event,
not hardcoded):

```
⏸ awaiting approval · 1.2 KB diff (4 files)
   review:  drydock review 7f3a…
   approve: drydock approve 7f3a…     deny: drydock deny 7f3a…     (^C aborts)
```

**Backward-compat branch:** a decoded line with no `event` key → existing
`printPretty`, stop. A non-200 response with a plain body → render an improved
error, exit 1.

## Testing

- **Broker:** extend `internal/broker/handle_task_test.go`, injecting the
  existing `b.runAgent` fake to drive each path and asserting the emitted NDJSON
  sequence (httptest recorder, split body on `\n`): push / no_diff / denied /
  cancelled / auto-approve (skips the approval stage) / boot-failure (asserts the
  `error` event carries the distilled reason + the `drydock doctor` hint).
- **Client:** table-test the pure `render`: event sequences → asserted piped
  output; a legacy single object → `printPretty` path; an `error` event →
  message + exit 1.
- **Helper:** unit-test `diffStat` on a known unified diff.

## Scope

**Core (this spec):** the protocol; the stream helper + all emit points; failure
distillation; client rendering (TTY / piped / `--json` / `--quiet`); the approval
block; the rich summary (diffstat / duration / cost / branch / platform);
backward compatibility; tests.

**Stretch (noted, not built now — both additive, protocol unchanged):**

- Live `heartbeat`/`progress` events with turn count & running cost — needs the
  broker to intercept and parse the agent stream-json mid-run.
- A clickable remote PR/branch URL in the summary — needs the remote adapter
  (`remote.AdapterFor`) to construct a compare/branch URL.

## Resolved decisions

- **`--quiet` flag:** included.
- **`denied` exit code:** `0` (operator decision, not an error).
