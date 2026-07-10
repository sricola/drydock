# Aggregate budget cap (ROADMAP 4.3): design

Status: proposed (2026-07-10)

## Problem

`task_budget_usd` caps a single task, and v0.6.0 added a per-task request cap
for uncapped (subscription / priceless) lanes. Nothing bounds spend *across*
tasks: a runaway submission loop, each task individually within budget, can
still drain an API key in aggregate. This is the last major unattended-daemon
safety gap (4.11 unattended operation is live; the daemon restarts on crash and
accepts queued submissions, so a loop can run unattended).

## Goal

A gateway-enforced ceiling on cross-task USD spend, per provider, over a
configurable window, that survives a brokerd restart.

## Decisions

1. **Window model.** Rolling window by default (`aggregate_window`, e.g. 24h);
   `aggregate_window: 0` means a session total since brokerd boot. Rolling
   auto-recovers as old spend ages out (right for an always-on daemon); the
   session-total mode is a simpler bound with no time decay.
2. **Ledger source: the audit trail.** No new persisted file. The audit is
   already the source of truth for per-task cost. The gateway keeps a live
   in-memory per-vendor ledger during operation and, in rolling mode, seeds it
   at boot from audit files within the window.
3. **Scope: per-vendor.** The cap applies independently per provider
   (anthropic, openai, google, and the openai_compat lane). One config value,
   enforced per-vendor: draining one key never affects another, which is what
   "can't drain a key" means.
4. **Subscription is out of scope for the USD cap.** Subscription is a flat
   plan, not metered money, so the USD aggregate applies only to `api_key`-mode
   vendors. Subscription runaway stays bounded per-task by the request cap. This
   is a stated limit, documented in the daemon/config docs. (An aggregate
   *request* cap for subscription is a possible future item, not this one.)
5. **Two enforcement layers.** The gateway `admit()` is the hard boundary: when
   a vendor's windowed spend is at or over the cap, the next request is rejected
   (`402 Payment Required`), which also halts an already-running task on its
   next request. The broker additionally pre-checks at `POST /tasks` so an
   over-budget submit fails cleanly at submit time ("aggregate budget
   exhausted") rather than starting a doomed task.

## Config surface

Two new fields on `config.Config`, both defaulting to disabled (fully backward
compatible):

- `aggregate_budget_usd` (float, default `0`): USD ceiling on cross-task spend
  per provider over the window. `0` disables the cap.
- `aggregate_window` (duration, default `24h`): rolling window length. `0` means
  total since brokerd boot (no time decay, resets on restart).

Env overrides `DRYDOCK_AGGREGATE_BUDGET_USD` and `DRYDOCK_AGGREGATE_WINDOW`,
matching the existing per-field override pattern. Validation in
`config.validate()`: `aggregate_budget_usd >= 0` and `aggregate_window >= 0`.
The SeedTemplate and its on-disk mirror (`config/config.yaml`) document both.

## Components

### `spendledger` (new, in internal/gateway)

A small, independently testable unit.

- State: `map[vendor] -> []entry{ts time.Time, usd float64}`, guarded by a
  mutex. `window time.Duration` (0 = total mode).
- `Add(vendor string, usd float64, ts time.Time)`: append an entry.
- `Windowed(vendor string, now time.Time) float64`: in rolling mode, drop
  entries older than `now - window` and return the sum of the rest; in total
  mode (`window == 0`), return the sum of all entries (no pruning).
- `Seed(entries ...seedEntry)`: bulk-add historical entries at boot (rolling
  mode only).

It knows nothing about the gateway, config, or audit. What it does: track and
sum time-stamped per-vendor spend. How you use it: `Add` on metering, `Windowed`
on admit. What it depends on: nothing but `time`.

### Boot seed (rolling mode)

At brokerd startup, before serving: scan the audit dir for `<id>.jsonl` files
with `mtime` within the window (mtime prunes the scan cheaply). For each
non-subscription task (`drydock_meta.subscription == false`), read
`total_cost_usd` from the terminal result line and resolve the vendor from the
`drydock_task` line's `agent` via `provider.VendorForAgent`; an empty `agent`
(the task took the default) resolves through the configured `default_agent`
first. Add `(mtime, total_cost_usd)` for that vendor. Tasks with no determinable
vendor (pre-v0.6.0 traces without a `drydock_task` line) are skipped:
conservative undercount, never a miscount. The whole seed is skipped in total
mode (`window == 0`), a since-boot session cap.

mtime is the timestamp proxy for a task's spend, close enough for a coarse
hours-scale window; the design does not need per-request precision here.

### Gateway `admit()` check

Alongside the existing per-lease budget check, before admitting a request for a
vendor whose auth mode is `api_key` and where `aggregate_budget_usd > 0`:
`if ledger.Windowed(vendor, now) >= aggregate_budget_usd { return 402 }`.
Subscription vendors and a disabled cap skip the check.

### Live metering

Where the gateway meters a task's spend (the `meter` / usage-finalize path that
already updates `lease.SpentUSD`), also `ledger.Add(vendor, delta, now)` so the
aggregate tracks in-flight and just-completed spend, not only what the boot seed
captured.

### Broker submit-time pre-check

The gateway and broker share the brokerd process. At `POST /tasks`, before
minting a lease, the broker asks the gateway/ledger whether the vendor is at or
over its aggregate cap; if so, reject with a clear "aggregate budget exhausted
for <vendor>" (HTTP 402/429) so the task never starts. This is UX on top of the
hard gateway boundary, not a replacement for it.

## Data flow

```
POST /tasks
  -> broker pre-check: ledger.Windowed(vendor) >= cap ?  yes -> 402 (task never starts)
  -> mint lease, run task
       -> each model request -> gateway.admit():
            per-lease budget check (existing)
            aggregate check: ledger.Windowed(vendor) >= cap ? yes -> 402 (halts task)
       -> gateway.meter(): lease.SpentUSD += delta ; ledger.Add(vendor, delta, now)

brokerd boot (rolling mode):
  scan audit dir (mtime in window) -> per non-subscription task:
     ledger.Seed(vendor, total_cost_usd, mtime)
```

## Testing (TDD)

- `spendledger`: Add/Windowed prune correctness; `window == 0` total mode sums
  all; per-vendor isolation; concurrent Add/Windowed under `-race`.
- Boot seed: given synthetic audit files (with `drydock_meta`, `drydock_task`,
  and result lines) at controlled mtimes, seeds the correct per-vendor windowed
  sum; excludes subscription tasks; skips vendor-less traces.
- `admit`: rejects at/over cap, allows under, skips subscription vendors, skips
  when cap is 0; per-vendor isolation (anthropic at cap does not block openai).
- Config: negative budget/window rejected; env overrides parsed; SeedTemplate
  matches the on-disk mirror (existing drift test).
- Broker pre-check: `POST /tasks` returns the aggregate-exhausted error when the
  vendor is over cap; allows when under.

## Non-goals (YAGNI)

- No aggregate USD cap for subscription mode (bounded per-task by the request
  cap; noted as a stated limit).
- No cross-provider global cap (per-vendor only).
- No manual reset command (rolling window auto-decays; total mode resets on
  restart).
- No multi-host / distributed aggregation (single-host daemon).

## Docs

Update `site/docs/configuration.md` (the two new fields), `site/docs/daemon.md`
(the no-aggregate-cap caveat becomes "aggregate cap available; here is how"),
and the CHANGELOG. Mark ROADMAP 4.3 landed.
