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

The per-task JSONL carries a terminal `result` row (`duration_ms`,
`total_cost_usd`, `subtype`) and interim `event` rows (accepted, stage
transitions, errors, outcomes), but:

- event rows carry no timestamps, so stage durations are not derivable;
- gate wait durations (egress widen, approval) are never persisted;
- per-lease request counts stay inside the gateway (`num_turns` in the
  result row is reserved and always 0);
- egress-widen outcomes live in `<id>.widen.json` and interim events, not
  in any aggregatable summary.

## Recording design (write side)

### 1. `ts` on event rows

Every broker-written event row (`accepted`, `stage`, `error`, interim
`result`) gains `ts` (RFC 3339 UTC, second precision). Near-free, and makes
single-task debugging read like a timeline. No reader depends on its
absence; rows remain NDJSON-additive.

### 2. Terminal `metrics` row

Written by the broker at task end, adjacent to the terminal `result` row,
for every terminal path that today gets a real result row. An `interrupted`
task (brokerd died under it) gets no metrics row, exactly as it gets only a
synthetic result.

Fields (all broker-observed; `src:"broker"` required, forged rows without
it are ignored, same trust rule as cost today):

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
transition times, `grant.Spent()`, DiffFacts, widen gate outcome). One
gateway addition is needed: a per-lease admitted-request counter surfaced
on the grant (also used to finally populate `num_turns` in the result row).

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
