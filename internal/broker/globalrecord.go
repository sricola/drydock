package broker

import (
	"log/slog"
	"time"

	"drydock/internal/audit"
)

// This file is the GLOBAL CEILING's RECORDING half (docs/superpowers/plans/
// 2026-07-31-global-ceiling.md, Task 3): the one place a finished task becomes
// a durable ledger entry. globalledger.go is the store, globalcap.go is the
// enforcement, and cmd/brokerd's reconcileGlobalLedger is the boot-time
// cross-check that repairs whatever a crash lost between the two.
//
// ---------------------------------------------------------------------------
// THE F-07 DISCIPLINE, APPLIED TO THE LEDGER: every terminal path records, and
// that is STRUCTURAL rather than a checklist.
// ---------------------------------------------------------------------------
//
// appendBrokerResult carries a rule the audit trail depends on — the
// broker-authored result row must be the LAST result line on EVERY exit path,
// because a path that skips it erases that task's metered spend from the
// restart-seeded aggregate cap. The ledger needs the same rule for a stronger
// reason: a terminal that skips its ledger write UNDER-COUNTS the ceiling, and
// under-counting is the one direction G2/G3 forbid outright.
//
// appendBrokerResult satisfies its rule by enumeration — five call sites in
// runSandbox, one in setup.go, one in verify.go, plus the synthetic
// planned/policy_blocked/push_failed rows and reconcile.go's resumed terminal.
// Enumeration is exactly what a new terminal path forgets. So this one does not
// enumerate. recordGlobalUsage is registered as a DEFER ON THE FIRST LINE of
// taskRun.runLifecycle, the single lifecycle both live paths share (HandleTask
// and runQueued), plus once in resumePush, the only task terminal that does not
// run through runLifecycle. There is no exit path out of a function that skips
// a defer registered as its first statement — return, panic, or a terminal
// emitted six calls deep — so every present and future terminal is covered by
// construction, including the ones a checklist would miss.
//
// WHY NOT IN appendMetrics, the existing "runs once at the very end on every
// path" hook (metrics.go, deferred at broker.go's audit-log open). Because it
// does NOT run on every path — it runs on every path FROM THE AUDIT LOG OPEN
// ONWARD, and returns immediately when tr.logf is nil. Everything that
// terminates a task before that point (a denied egress widen, a clone failure,
// a stage-quota failure, the low-disk preflight, the push preflight, an
// unresolvable agent, a credential-mint failure, the audit open itself failing)
// would silently record nothing. Those are REAL TASK STARTS: each one consumed
// a concurrency slot and an in-flight ceiling claim, and G1's task-start limb
// counts starts, not audit files. A retry storm whose every attempt died at
// clone would otherwise be completely invisible to the ceiling — precisely the
// unattended loop the ceiling exists to stop. So the ledger write sits ALONGSIDE
// appendMetrics, earlier and unconditional, not inside it.
//
// ORDERING, and why it is not load-bearing. Defers run LIFO, so the exit order
// from runLifecycle is: appendMetrics -> audit Sync -> audit Close -> ... ->
// recordGlobalUsage, and then the CALLER's releaseGlobalStart. That gives two
// properties worth stating:
//
//   - The audit trace is flushed and closed BEFORE the ledger says "counted",
//     so a crash in that gap leaves the evidence boot reconciliation needs to
//     add the entry itself. The reverse order would leave a counted task with
//     no trace to verify it against.
//   - The in-flight claim is released AFTER the entry lands, so the task is
//     briefly both claimed and recorded. globalcap.go's tally is keyed on task
//     IDENTITY and skips a claimed id the ledger already has, so that instant
//     is counted exactly once either way (a bare counter would have counted it
//     twice, silently tightening the ceiling). The ordering is verified by test
//     rather than assumed, but nothing breaks if it is ever reversed.
//
// ---------------------------------------------------------------------------
// G4: THE USD FIGURE COMES FROM THE LEASE, NEVER FROM audit.TotalCost.
// ---------------------------------------------------------------------------
//
// audit.TotalCost returns the last result row's total_cost_usd WITHOUT
// filtering Src, so it will happily return a number a compromised agent CLI
// printed to its own stdout. Recording that as ceiling-relevant spend would let
// an agent DEFLATE the ceiling by under-reporting — the single way an agent
// could influence a control that exists to bound it. The only trustworthy
// figure is the gateway lease's SpentUSD, which the broker parses out of the
// proxied response bodies itself, and it is the only figure this file records.
//
// The lease is torn down by `defer grant.Revoke()` (registered at mint time,
// so it fires BEFORE this defer), after which grant.Spent() answers 0 for an
// unknown token. Reading it from here would therefore record $0 for every task
// — a silent, total deflation of the USD limb that no test of the write path
// would have caught. So the figure is SNAPSHOTTED at the one instant it is both
// final and still readable: immediately after the agent run returns in
// runSandbox (captureLeaseSpend). That anchor is semantic, not defer-ordering
// luck — the agent VM is the only VM a bearer is ever injected into (setup and
// verify VMs are bearer-free by construction), so no spend can occur before it
// starts or after it exits.
//
// ---------------------------------------------------------------------------
// METERED vs UNMETERED, and why an unmetered lane's $0 is not recorded as $0.
// ---------------------------------------------------------------------------
//
// A subscription lane, or an openai-compat lane configured with no prices,
// meters at $0 BY CONSTRUCTION rather than by measurement — writeBrief already
// treats that as a distinct fact (b.UnmeteredVendors flips BudgetUnbounded and
// zeroes the reported budget rather than pretending a cap applied). The ledger
// uses the SAME signal, so the two can never disagree about which lanes carry
// real dollars, and records such a task as USDTrusted:false. That keeps its $0
// out of the USD limb's sum instead of averaging the limb downward with
// fictional zeroes — while still recording it as a TASK START, which is the
// limb that actually bounds subscription mode (G1) and the one thing about an
// unmetered task that is measurable at all.

// captureLeaseSpend snapshots this task's BROKER-METERED spend off the gateway
// lease. See the G4 discussion above: it must be called while the lease is
// alive (before `defer grant.Revoke()` fires) and after the agent run has
// ended, and runSandbox is the single place both hold.
//
// Idempotent and nil-safe: a task whose grant was never minted has provably
// spent nothing, and leaves the snapshot uncaptured so recordGlobalUsage can
// tell "no lease existed" from "the lease metered zero".
func (tr *taskRun) captureLeaseSpend() {
	if tr.grant == nil || tr.leaseCaptured {
		return
	}
	tr.leaseSpentUSD = tr.grant.Spent()
	tr.leaseCaptured = true
}

// recordGlobalUsage writes this task's durable global-ledger entry. It is the
// task-terminal half of the ceiling, deferred on runLifecycle's first line (and
// in resumePush) so no terminal path can skip it.
//
// It is a no-op when the ceiling has no store, which is the stock install: the
// ledger is nil until it is configured, so a deployment that has not enabled
// the ceiling writes nothing, fsyncs nothing, and behaves exactly as it did
// before this file existed.
//
// A durable write failure is logged, never fatal, and never retried here: the
// store keeps the entry in memory (so this process's ceiling stays correct and
// over-counts rather than under-counts) and boot reconciliation re-derives it
// from the audit trail if the process dies before it reaches disk.
func (tr *taskRun) recordGlobalUsage() {
	b := tr.b
	if b == nil || b.GlobalLedger == nil {
		return
	}
	now := b.nowMs()
	e := GlobalEntry{
		Kind:      GlobalEntryTask,
		TaskID:    tr.id,
		EndedAtMs: now,
		Vendor:    tr.taskVendor,
		Agent:     tr.agentName,
		Auth:      "api_key",
		Outcome:   tr.outcome,
		Attempt:   tr.attempt,
		RetryOf:   tr.retryOf,
		Src:       GlobalSrcTerminal,
	}
	if tr.subscription {
		e.Auth = "subscription"
	}
	// Derived from the SAME clock as EndedAtMs (b.now), not from taskStart's
	// wall clock, so the two ends of an entry cannot come from two different
	// clocks. Zero when the task never reached a start anchor — an early
	// terminal has no meaningful run window, and 0 reads as "unknown".
	if !tr.taskStart.IsZero() {
		if started := now - time.Since(tr.taskStart).Milliseconds(); started > 0 {
			e.StartedAtMs = started
		}
	}
	// The same signal writeBrief uses (see the file header): a vendor in
	// UnmeteredVendors carries no USD metering at all. An unresolved vendor is
	// treated as unmetered too — we cannot claim a lane meters when we do not
	// know which lane it was.
	e.Metered = tr.taskVendor != "" && !b.UnmeteredVendors[tr.taskVendor]
	e.USD, e.USDTrusted = tr.brokerMeteredSpendUSD()
	// The figure is parsed by the broker out of a PROXIED VENDOR RESPONSE BODY,
	// so "a float" is not the same as "a number we can put in a total". A NaN
	// makes `usage.USD >= budget` false forever — the USD limb would admit
	// without bound — and a non-finite float also fails json.Marshal, which
	// would wedge every later compaction and let the ledger grow unchecked. The
	// store defends itself too (sanitizeEntryUSD); this catches it where the
	// figure is PRODUCED, so the operator's warning names the real cause and the
	// store's error stays about durability.
	if !usableUSD(e.USD) {
		slog.Warn("global ledger: the broker-metered spend figure for this task is not a usable number; the start is counted and its spend is recorded as unknown",
			"task_id", tr.id, "usd", e.USD, "vendor", tr.taskVendor)
		e.USD, e.USDTrusted = 0, false
	}
	if !e.Metered {
		// $0 by construction is not a measured $0. Recording it as trustworthy
		// would let an unmetered lane's zeroes look like real spend data.
		e.USDTrusted = false
	}
	if err := b.GlobalLedger.Record(now, e); err != nil {
		slog.Warn("global ledger: could not durably record a task terminal; it is counted in memory and boot reconciliation re-derives it from the audit",
			"task_id", tr.id, "outcome", tr.outcome, "err", err)
	}
}

// meteredCostUSD is the task's broker-observed spend for DISPLAY and for the
// broker's own audit rows: brokerMeteredSpendUSD without the trust flag.
//
// It exists because of a G4 hole that was worse than the two the plan names.
// Every synthetic broker result row — `planned`, `policy_blocked`,
// `push_failed`, `verify_failed`, `setup_failed`, the resumed terminal — used
// to fill its total_cost_usd from audit.TotalCost, which reads the last result
// line WITHOUT filtering Src. On any path where the agent's own result line was
// the last one, an agent-authored number was copied into a row stamped
// src:"broker" — LAUNDERING it. That row is what the aggregate-cap restart seed
// reads, what boot reconciliation reads, what a resumed task's spend is
// recovered from, and now what `drydock stats` and `drydock tasks` read. A
// src=="broker" filter downstream is worth nothing if the broker itself will
// copy an agent's number into a broker row, so the fix has to be here, at the
// writer.
//
// The figure is the gateway lease's, snapshotted while it was alive
// (captureLeaseSpend), or — for a task resumed after a restart — the PREVIOUS
// process's own src=="broker" row. A terminal that never minted a lease records
// 0, which is not an estimate: nothing could have spent.
func (tr *taskRun) meteredCostUSD() float64 {
	usd, _ := tr.brokerMeteredSpendUSD()
	if !usableUSD(usd) || usd < 0 {
		// The same sanitisation recordGlobalUsage applies: a figure parsed out
		// of a proxied response body is not automatically a number that can be
		// printed into JSON. A non-finite value would make the audit row
		// unparseable for every later reader.
		return 0
	}
	return usd
}

// brokerMeteredSpendUSD answers "what did the BROKER meter for this task, and
// do we believe it?" — the only spend question the ceiling is allowed to ask.
// It never reads audit.TotalCost (G4); see the file header.
func (tr *taskRun) brokerMeteredSpendUSD() (usd float64, trusted bool) {
	switch {
	case tr.leaseCaptured:
		// The gateway lease, snapshotted while it was alive. The strongest
		// source there is, and the one every live task uses.
		return tr.leaseSpentUSD, true
	case tr.resumed:
		// A task resumed at the diff-approval gate after a restart: its agent
		// ran in a PREVIOUS process, whose lease is long gone. The previous
		// process's own broker-authored result row is the surviving record of
		// what that lease metered — the same src=="broker" row the aggregate
		// cap is reseeded from — so it is broker-observed, just at one remove.
		// Untrusted when there is no such row: we then have no broker-authored
		// figure at all, and inventing one would be worse than saying so.
		res, ok := audit.LastBrokerResult(tr.auditPath)
		if !ok || res.TotalCostUSD < 0 {
			return 0, false
		}
		return res.TotalCostUSD, true
	default:
		// No lease was ever captured, which means the agent VM never ran in
		// this process (a terminal before the mint, or during setup — setup VMs
		// are bearer-free by construction). $0 is not an estimate here, it is
		// the only value the lane could possibly have produced.
		return 0, true
	}
}
