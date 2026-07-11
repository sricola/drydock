# Precise gateway metering: in-flight reservation + per-request ceiling (ROADMAP 4.15): design

Status: proposed (2026-07-11). Tracks issue #139.

## Problem

The gateway enforces the per-task USD budget post-hoc: `admit` gates on
`SpentUSD >= BudgetUSD`, but `SpentUSD` is only updated at stream end
(`usageReader.onDone` in `gateway.go`). A single per-task bearer allows
pipelined concurrent requests, so N requests all pass `admit` while `SpentUSD`
is still 0. The effective ceiling is `budget x in-flight-count`, not `budget`.

A truncated stream is already charged its input cost (Anthropic's `message_start`
arrives early, carries input tokens, and is kept by `usageReader`; `ReverseProxy`
always `Close()`s the body, firing `onDone`), so the input side is covered. The
remaining gap is the concurrent bypass: nothing bounds how many requests admit
before any of them meters.

The v0.6.0 per-task request cap bounds the worst case to
`cap x per-request-max`. This is the precise fix.

## Goal

Bound the per-task budget under concurrency by reserving a per-request cost
ceiling at admission and reconciling it at completion, so concurrent admits
cannot all pass at spend=0. Backward compatible: off by default.

## Decisions

1. **In-flight reservation.** A configured per-request cost ceiling `R`
   (`max_request_cost_usd`) is reserved against the lease at `admit` and
   released at `onDone`. `admit` rejects when
   `SpentUSD + Reserved + R > BudgetUSD`, so N concurrent requests each hold `R`
   and the budget cannot be overrun by in-flight count.
2. **Config-gated, off by default.** `max_request_cost_usd` defaults to `0`,
   which disables reservation entirely (exact current post-hoc behavior). An
   operator opts into precise metering by setting a per-request ceiling. This
   keeps the change a no-op by default (a billing behavior change should be
   explicit).
3. **Truncation charges what was metered (at least input).** `onDone` charges
   the actual metered cost, which already includes input from `message_start`
   even on a truncated stream. We do NOT charge the full reservation on
   truncation: that would over-bill a legitimately interrupted request. The
   reservation's job is to bound concurrency during flight; at completion the
   real (at-least-input) cost is committed and `R` released.
4. **Per-vendor unchanged, aggregate-cap unchanged.** The reservation is a
   per-lease mechanism layered onto the existing per-lease budget check; the
   aggregate cap (4.3) and request cap are untouched.

## Config surface

One field on `config.Config`, defaulting to disabled (fully backward
compatible):

- `max_request_cost_usd` (float, default `0`): the worst-case USD a single
  request may cost, reserved against the lease budget while the request is
  in flight. `0` disables reservation (post-hoc metering only). Env override
  `DRYDOCK_MAX_REQUEST_COST_USD`. Validation: `>= 0`. SeedTemplate + the on-disk
  `config/config.yaml` mirror document it. It sits with the other per-task
  limits (`task_budget_usd`, `task_max_requests`).

The operator sizes `R` from their model's worst case, roughly
`max_output_tokens x output_price_per_1M / 1e6` plus a typical input; a value
like `0.50` bounds a Claude request. Setting `R` larger than `BudgetUSD` makes
the first request reject (fail-closed), which is a sizing error the operator
sees immediately.

## Components

### Lease fields (internal/gateway/gateway.go)

`Lease` gains:

- `MaxRequestCostUSD float64`: the per-request reservation `R` (0 = disabled),
  set at `Mint`.
- `Reserved float64`: the sum of `R` for admitted-but-unmetered requests,
  guarded by `g.mu` like `SpentUSD`.

### admit (gateway.go)

After the existing `SpentUSD >= BudgetUSD` 402 check and before `Requests++`,
add the reservation gate (only when `R > 0`):

```
if l.MaxRequestCostUSD > 0 {
    if l.SpentUSD + l.Reserved + l.MaxRequestCostUSD > l.BudgetUSD {
        return nil, 402
    }
    l.Reserved += l.MaxRequestCostUSD
}
```

Held under `g.mu` (same lock that guards `SpentUSD`/`Reserved`). When `R == 0`,
admit is unchanged.

### meter onDone (gateway.go)

The `onDone` closure reconciles: release the reservation and commit the actual
metered cost. When `R > 0`, `Reserved -= R` must happen whether or not usage
parsed (so a stream with no usage still releases its hold):

```
g.mu.Lock()
if rc.lease.MaxRequestCostUSD > 0 {
    rc.lease.Reserved -= rc.lease.MaxRequestCostUSD
    if rc.lease.Reserved < 0 { rc.lease.Reserved = 0 } // floor, defense in depth
}
rc.lease.SpentUSD += delta // delta is the parsed cost (0 if no usage)
g.mu.Unlock()
```

The `delta` and the `ledger.add` (aggregate cap) stay as they are; only the
reservation release is added, and it runs even when `ParseUsage` returns
`ok == false` (so the release is not inside the `if ok` block). The aggregate
`ledger.add` stays gated on a parsed delta.

Reservation-leak note: if a request admits (reserves `R`) but the upstream
fails before a response body exists, `meter`/`onDone` never runs and `R` stays
reserved until the lease expires (short TTL). This is fail-closed
(over-conservative: it only makes the budget stricter, never looser) and is
documented as a known bound, not a correctness bug. `Reserved` can never make
the budget looser, so it cannot cause an over-admit.

### Mint + Provider + brokerd wiring

- `Gateway.Mint` gains a `maxRequestCostUSD float64` parameter, written into the
  new `Lease.MaxRequestCostUSD`.
- `gateway.Provider` gains a `MaxRequestCost float64` field; `Provider.Mint`
  passes it to `GW.Mint`.
- `cmd/brokerd/main.go` sets `MaxRequestCost: cfg.MaxRequestCostUSD` when
  building each provider.

## Data flow

```
admit (per request, under g.mu):
  ... existing 401/expiry/402(SpentUSD)/429(requests)/aggregate checks ...
  if R > 0 and SpentUSD + Reserved + R > Budget -> 402
  else Reserved += R ; Requests++

meter onDone (per request end, under g.mu):
  if R > 0: Reserved -= R (floored at 0)
  SpentUSD += delta        # delta includes input from message_start even on truncation
  (aggregate ledger.add on a parsed delta, unchanged)
```

With `R > 0`, K concurrent requests hold `K x R` in `Reserved`, so the
`(SpentUSD + Reserved + R > Budget)` gate rejects once the in-flight commitments
would exceed the budget, closing the concurrent bypass.

## Testing (TDD)

- admit reservation: with `R` set and `BudgetUSD` small, the first admit passes
  and reserves; a second concurrent admit (before any meter) is rejected once
  `SpentUSD + Reserved + R > Budget`. With `R == 0`, admit is unchanged
  (existing tests still pass).
- meter reconciliation: after a metered request, `Reserved` returns to 0 and
  `SpentUSD` reflects the actual cost; a request whose stream yielded no usage
  still releases its reservation (`Reserved` back to 0), `SpentUSD` unchanged.
- truncation: a stream with only `message_start` (input, no `message_delta`)
  charges the input cost and releases the reservation (verifies "at least input
  cost").
- concurrency under `-race`: interleave admit and onDone on one lease; no data
  race on `Reserved`/`SpentUSD` (both under `g.mu`).
- config: `max_request_cost_usd` parses, env override, validate `>= 0`,
  SeedTemplate matches the on-disk mirror (existing drift test).
- wiring: brokerd builds providers with `MaxRequestCost` from config; a minted
  lease carries `MaxRequestCostUSD`.

## Non-goals (YAGNI)

- No mid-stream progressive output metering (counting tokens from
  `content_block_delta`): heavy, and the reservation + request cap already bound
  the worst case.
- No charging the full reservation on truncation (would over-bill legitimate
  interruptions); truncation charges the metered at-least-input cost.
- No deriving `R` from the request body's `max_tokens` (config value is simpler
  and does not require parsing/rewriting the request); a future refinement.
- No change to the aggregate cap, the request cap, or subscription handling.
