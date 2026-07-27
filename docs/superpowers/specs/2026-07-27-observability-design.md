# Observability (roadmap 4.7): audit metrics enrichment + `drydock stats`

Date: 2026-07-27
Status: approved design, pre-implementation

## Goal

Answer "what is drydock doing across many runs" (durations, gate latencies,
egress-widen frequency, budget burn) from the terminal, without grepping
per-task audit JSONL by hand. Closes roadmap item 4.7.

## Decisions taken during brainstorming

- **Consumer: a CLI summary command**, `drydock stats`. No web UI charts, no
  Prometheus/OTel export in this iteration; the aggregation layer is written
  so the web UI can reuse it later.
- **Data source: enrich the existing audit JSONL**, not a second metrics
  stream. Two additive changes: a `ts` timestamp on event rows, and one
  terminal `type:"metrics"` row per task. Old files keep working; timing
  columns render "-" for pre-upgrade tasks.
- **Grouping dimensions:** agent, vendor/auth lane, repo, and time bucket
  (day/week).

## Why the audit rows cannot answer this today

The per-task JSONL carries the `drydock_meta` and `drydock_task` head rows,
the agent's stream output, and a terminal `result` row (`duration_ms`,
`total_cost_usd`, `subtype`), but:

- the broker's lifecycle events (accepted, stage transitions, gates,
  outcomes) go only to the submit client's NDJSON response stream; they are
  never written to the audit file, so no stage timing is persisted at all;
- gate wait durations (egress widen, approval) are never persisted;
- per-lease request counts stay inside the gateway (`num_turns` in the
  result row is reserved and always 0);
- egress-widen outcomes live in `<id>.widen.json`, and a task denied at the
  egress gate dies before the audit file is even created, so it leaves a
  `.widen.json` with no `.jsonl` at all.

## Recording design (write side)

### 1. `ts` on stream events

Every broker-emitted lifecycle event on the submit NDJSON stream
(`accepted`, `stage`, `error`, interim `result`) gains `ts` (RFC 3339 UTC).
Near-free (one line in `stream.emit`), and gives live clients a timeline.
These events are not persisted (they never were); all *persistent* timing
comes from the terminal metrics row below.

### 2. Terminal `metrics` row

Written by the broker at task end, adjacent to the terminal `result` row,
for every terminal path that today gets a real result row. An `interrupted`
task (brokerd died under it) gets no metrics row, exactly as it gets only a
synthetic result.

Fields (all broker-observed; `src:"broker"` required, and the aggregator
takes only the *last* metrics row in the file, mirroring `LastResult`:
the broker writes it after the agent's stream has ended, so an in-VM
agent printing a forged row, even one carrying `src:"broker"`, is
superseded):

| Field | Type | Source |
|---|---|---|
| `type` | `"metrics"` | constant |
| `src` | `"broker"` | constant; trust marker |
| `task_id` | string | task |
| `agent` | string | task (claude/codex/gemini/opencode) |
| `vendor` | string | provider registry (anthropic/openai/google/...) |
| `auth` | string | `api_key` \| `subscription` |
| `repo` | string | same redacted form the trust brief records |
| `model` | string | effective model (may be empty) |
| `stage_ms` | object | `{preparing, running, pushing}` wall-clock ms |
| `egress_gate_wait_ms` | int64 | widen gate wall-clock wait (0 if no gate) |
| `approval_gate_wait_ms` | int64 | approval gate wall-clock wait (0 if none) |
| `requests` | int | requests admitted through the gateway lease |
| `diff_files` | int | from the brief's DiffFacts |
| `diff_bytes` | int64 | from the brief's DiffFacts |
| `cost_usd` | float64 | broker-metered, same source as result row |
| `widen_requested` | int | count of requested extra egress domains |
| `widen_outcome` | string | `approved` \| `denied` \| `none` |

Gate waits are wall-clock on purpose: queued human review time is the
latency being measured.

The broker already holds each of these at task end (taskStart, stage
transition times, DiffFacts, widen gate outcome). The row is written by a
deferred hook registered when the audit log opens, so it runs on every
exit path and is guaranteed to be the file's last row; the boot-resume
path (`resumePush`) appends one too. The gateway lease already counts
admitted requests (`Lease.Requests`); it is surfaced on the grant via an
optional-capability interface (the codebase's `BaseCommit` idiom), so the
`creds.Grant` interface and its test fakes stay untouched, and it also
finally populates `num_turns` in the result row.

Known visibility limits, accepted: a task denied (or cancelled) at the
egress gate never creates an audit file, so it appears in stats only via
its orphan `.widen.json` (counted as "widen requested, task never ran");
denied and cancelled are indistinguishable there. Auto-approved tasks
record a 0 approval-gate wait and are excluded from gate-wait percentiles.

## `drydock stats` (read side)

```
drydock stats [--since 30d] [--by agent|vendor|repo|day|week] [--json]
```

- Reads `~/.drydock/audit/*.jsonl` directly, tail-first (the metrics and
  result rows are the last rows), following the `drydock tasks` pattern.
  Works with brokerd down.
- `--since` filters by result-file mtime; default `30d`.
- Default report, plain text: task count; outcome breakdown with rates
  (success / error / push_failed / no_diff / cancelled / interrupted);
  duration p50/p95; gate-wait p50/p95 (egress and approval separately);
  total spend and spend/day; egress-widen count and approval ratio.
- `--by X` appends a table grouped on that dimension with the same columns
  (compact). `day`/`week` bucket on the task's result mtime.
- Percentiles are computed exactly over the retained sample; task counts
  are small enough that no estimation is warranted.
- Subscription-lane tasks are unmetered: spend lines aggregate api_key
  tasks only and report the subscription task count as "unmetered"
  alongside, never as $0.
- `--json` emits the same aggregates as a single JSON object.

The aggregator lives in a new `internal/stats` package (parse rows,
filter, group, summarize) so the web UI can call it later; the CLI command
is a thin renderer over it.

## Compatibility and error handling

- **Old audit files** (no `ts`, no metrics row): outcomes, durations, and
  cost still aggregate from the existing result row; timing/requests
  columns show "-"; a footnote reports how many tasks predate metrics.
- **Malformed or truncated files:** skipped and counted in a warning line,
  never fatal (matches existing audit reader behavior).
- **Existing readers are unaffected:** `LastResult`, boot spend seeding,
  and the web UI history key on `type:"result"` and ignore unknown rows.
  No migration, no format version bump.
- **Security posture unchanged:** the metrics row is broker-written
  host-side; nothing new crosses the sandbox boundary; agent-forgeable
  rows without `src:"broker"` are ignored by the aggregator.

## Testing

- Unit, write side: metrics row emitted on each terminal path (success,
  no_diff, cancelled, error, push_failed); none on interrupted; gate-wait
  and stage-duration computation; request counter propagation from the
  gateway lease; `ts` present on event rows.
- Unit, read side: aggregator math (percentiles, rates, grouping by all
  four dimensions, `--since` filtering, day/week bucketing); old-format
  fallback; forged-row (missing `src`) rejection; malformed-file skip.
- CLI: output tests for `stats` and `stats --json` over a fixture audit
  dir mixing old-format and new-format files.
- No red-team impact expected; `make redteam` runs as the standard gate
  anyway.

## Out of scope (deliberate)

- Web UI charts (aggregator is reusable when wanted).
- Prometheus/OpenTelemetry export.
- Per-request latency percentiles (only the per-task request count ships).
- Retention changes: `drydock prune` already governs the audit dir.

## Docs to update on landing

- Audit format documentation (new `ts` field, `metrics` row).
- CLI docs / README command list (`drydock stats`).
- ROADMAP 4.7 marked landed with a summary paragraph.
