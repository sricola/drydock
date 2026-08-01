# A global, durable, fail-closed usage ceiling

**Status:** planned 2026-07-31. Follows the orchestration engine increments A/B (#215, #216, #217).

**Why now.** B2 made drydock spend money unattended: an observed CI failure enqueues a retry, up to
`ci.max_attempts` times, per chain. The reviews kept returning the same answer to "what stops a
retry storm?" — *nothing global*. Today's only cross-task bound is `aggregate_budget_usd`, and it is:

- **per-vendor**, so with N vendors the real ceiling is `N × aggregate_budget_usd`;
- **off by default** (`0`);
- **api_key-mode only** — deliberately excluded for subscription lanes;
- **in-memory**, and in total mode (`aggregate_window: 0`) it resets to $0 on every restart;
- **fail-open at every broker-side check** (`vendorExceeded` returns `false` on a nil hook, an
  agent-resolution error, or an empty vendor string).

The gap that matters most: **in subscription mode there is no cross-task bound at all.** A
subscription lane's only limits are per-task (1000 requests, 1 in-flight, 30 minutes) plus
`max_concurrent_tasks: 2`. Nothing bounds cumulative usage over a day. That is the likeliest
configuration for an unattended install, and it is the one with no ceiling.

---

## Locked design decisions

### G1. Two currencies, because subscription lanes have no enforceable USD

A USD-only ceiling would miss precisely the mode that motivated this work. The ceiling therefore
has two independent limbs, either of which can trip:

- **`global_budget_usd`** — cumulative broker-metered USD across **all vendors**, rolling window.
  Meaningful only where metering is real (api_key and priced openai-compat lanes).
- **`global_max_tasks`** — cumulative **task starts** across all vendors and all auth modes,
  rolling window. This is the limb that actually bounds subscription mode, and it works
  everywhere because it counts an event the broker itself causes.

Both count **retries and their parents alike** — a retry is a task start like any other.

### G2. Fail-CLOSED, unlike every existing spend check

This is the substantive break with the current code. `vendorExceeded` and the `HandleTask`
pre-check silently admit when they cannot determine the answer. For a *ceiling*, "I don't know"
must mean "no" — an unattended loop that cannot be measured is exactly the thing to stop.

If the durable ledger cannot be read, is corrupt, or the vendor cannot be resolved, the global
check **refuses the task start** with an honest, actionable message. The existing per-vendor cap
keeps its current fail-open behavior (changing it is a separate, riskier change); the two
coexist, and the stricter answer wins.

### G3. Durable, crash-safe, and window-honest

The ledger persists under `AuditRoot` using the established marker idiom (`atomicfile.Write`,
0600, `O_NOFOLLOW` reads, tolerant boot scan). It survives restart in **both** window modes —
the current total-mode amnesia is itself a hole, since a crash loop resets the ceiling to zero.

Entries are appended at **task terminal** (the natural unit, matching how spend is already
reconciled) and carry a broker-authored timestamp, so the window is computed from when the task
*ran*, not from a file's mtime. Today's `seedAggregateFromAudit` keys on mtime for both the cutoff
and the entry timestamp — which the CI work already had to paper over with `appendPreservingMTime`.

**Reconciliation, not replacement:** the durable ledger is authoritative, and the boot scan
cross-checks it against the audit. A task present in the audit but missing from the ledger (killed
between terminal and ledger write) is counted from the audit; the direction is always
*over*-counting, never under.

### G4. Only broker-observed cost counts, everywhere

`audit.TotalCost` does **not** filter `Src`, so an agent-forgeable `total_cost_usd` currently
reaches `drydock stats` and the web UI's push-gate display. The gateway's `lease.SpentUSD` — parsed
by the broker from the proxied response body — is the only trustworthy number.

The global ledger uses broker-metered spend exclusively. And since this work is about making spend
trustworthy, it also fixes the two places where an agent-reported number is shown to an operator as
if it were fact — most importantly `app.js`'s push-gate display, which renders an untrusted cost
next to a human security decision.

### G5. The ceiling refuses task *starts*; it never kills a running task

A ceiling that terminated in-flight work would create a new failure mode (half-finished trees, a
task killed after its diff was staged but before its gate) for no safety gain — the money is
already spent. It blocks at admission: `POST /tasks` (402), the dispatcher, and the CI-retry gate.
A running task remains bounded by its own per-task lease as it is today.

### G6. Headroom is visible

There is currently **no endpoint, command, or UI that reports aggregate ledger state.** An
operator cannot answer "how close am I to the cap?" A ceiling nobody can see is a ceiling nobody
trusts, so this ships with the numbers exposed on `GET /admin/policy`-adjacent surfaces and in
`drydock stats`.

### G7. Off by default, and honest about what it does not cover

Both limbs default to `0 = off`. Documented non-coverage, plainly: spend metered after a task's
broker result row (a late in-flight completion); responses whose usage block exceeds the 1 MiB
parse buffer; `/v1/messages/batches`-style routes where usage is not in the response; and
openai-compat lanes configured with no prices, which meter at $0 by construction. The task-count
limb is the backstop for exactly these cases — it counts events, not dollars, so it cannot be
under-reported by a metering gap.

---

## Delivery

### Task 1: the durable global ledger

**Files:** `internal/broker/globalledger.go` (new); tests.

Append-only durable record under `AuditRoot`, the queuestore idiom exactly. Entry: task id,
broker-authored start/end ms, vendor, agent, broker-metered USD, auth mode (metered vs unmetered),
and whether the USD figure is trustworthy. Rolling-window sum + count. Tolerant boot scan, atomic
writes, id validation before path construction, compaction of entries older than the window
(so the file cannot grow without bound — the accumulation mistake Increment A made with queue
records).

### Task 2: enforcement, fail-closed

**Files:** `internal/broker/globalcap.go` (new), `broker.go` (`HandleTask` pre-check), `queue.go`
(dispatcher), `ciretryloop.go` (a new gate); tests.

One `globalCeilingExceeded(agent) (bool, reason string)` consulted at all three admission points.
Fail-closed on every error path. Retries are refused (never parked — matching B2's reasoning that
an unattended item parked at a ceiling dispatches hours later at a gate nobody is waiting at).
Human submissions get a 402 with the reason and the current headroom.

### Task 3: recording + reconciliation

**Files:** `metrics.go` / the task-terminal path, `cmd/brokerd/main.go` (boot reconcile); tests.

Write the ledger entry on every task terminal, on **every** exit path (the F-07 discipline — a
path that skips it under-counts the ceiling). Boot reconciliation against the audit, over-counting
on ambiguity. Must handle: crash before the entry, a task in the audit but not the ledger, an
entry with no matching audit, and clock movement.

### Task 4: config, provenance, operator surface

**Files:** `internal/config/{config,explain}.go`, `config/config.yaml`, `admin.go`,
`cmd/drydock/stats.go`, `internal/webui/`, docs.

`global_budget_usd`, `global_max_tasks`, `global_window` (default 24h) with defaults, env
overrides, validation, provenance entries whose guards mirror `applyEnvOverrides` exactly, and the
seed template. Headroom on an admin surface + `drydock stats`. Fix G4's two agent-reported-cost
displays. THREAT_MODEL: N4 gains the global ceiling and its documented non-coverage.

### Task 5: adversarial tests + docs

A retry storm across N chains stops at the ceiling; subscription mode is genuinely bounded by the
task limb; a corrupt/missing/hand-edited ledger refuses rather than admits; the ledger cannot be
inflated or deflated by an agent (it is host-only and never reads agent-reported cost); window
boundary behavior; restart durability in both window modes; and a test that the ceiling never
kills a running task.

## Explicitly out of scope

The `SetAggregateCap` unsynchronized-publish race, the unrestricted `AllowedRoutes` on
openai-compat vendors, `/v1/messages/batches` metering blindness, and strict YAML decoding. Each is
real and separately filed; none is created or worsened here.

## Verification gate (every task)

`go vet ./...`; `go test -race -count=1 ./...`; `gofmt -l internal/ cmd/` silent;
`staticcheck@v0.7.0 ./...`; `node --check internal/webui/assets/app.js`;
`go test ./cmd/docs-build/`; `go vet -tags integration ./tests/integration/`; `make redteam`.
