package main

import (
	"log/slog"
	"time"

	"drydock/internal/broker"
	"drydock/internal/config"
)

// CONFIG WIRING for the global usage ceiling (docs/superpowers/plans/
// 2026-07-31-global-ceiling.md, Task 4). This is the file that takes the
// feature out of the dark: until it existed, Broker.GlobalBudgetUSD,
// GlobalMaxTasks and GlobalLedger were fields nothing ever set, so every
// admission point short-circuited on globalCeilingOn() == false.
//
// THE OFF-BY-DEFAULT IDENTITY (plan G7) IS ENFORCED HERE, and it is stronger
// than "the numbers are zero". With both limbs at 0 this function opens NO
// ledger, so:
//
//   - no <audit_root>/global/ directory is created and no ledger file is
//     written — a stock install's audit root gains nothing at all;
//   - reconcileGlobalLedger stays a no-op (it returns immediately on a nil
//     store), so boot does not walk the audit dir a second time;
//   - every task terminal's recordGlobalUsage returns on the nil check, so
//     nothing is fsynced on the task path.
//
// Task 2 tested that identity against a nil ledger and Task 3 asserted the
// directory never appears; this is the only place that could break it, which is
// why the guard is the same expression globalCeilingOn() uses rather than a
// separate one that could drift.
// The limbs and the store are armed TOGETHER, here, and nowhere else. Setting
// the limbs in the Broker literal and opening the ledger separately would allow
// a state the ceiling has no defence against being *configured* into: limbs on
// with no store. (That state is fail-closed at runtime — every start would be
// refused — so the failure would be loud rather than dangerous, but it would
// still be a wiring bug an operator experiences as a dead daemon.)
func applyGlobalCeiling(b *broker.Broker, cfg *config.Config) {
	if cfg.GlobalBudgetUSD <= 0 && cfg.GlobalMaxTasks <= 0 {
		return
	}
	b.GlobalBudgetUSD = cfg.GlobalBudgetUSD
	b.GlobalMaxTasks = cfg.GlobalMaxTasks

	// The store is kept EVEN ON ERROR. OpenGlobalLedger returns a usable handle
	// whenever the audit root is non-empty, and a handle that failed to load
	// reports itself degraded — which globalcap.go turns into a refusal on every
	// enforced limb (G2). Dropping to nil would do the opposite: a nil store is
	// also fail-closed today, but it hides WHICH failure happened and it throws
	// away the in-memory entries a partial load did recover. Keep it and let the
	// ceiling refuse with the real reason.
	lg, err := broker.OpenGlobalLedger(cfg.AuditRoot, cfg.GlobalWindow, time.Now().UnixMilli())
	if err != nil {
		slog.Error("global usage ceiling: the durable ledger could not be opened; task starts will be REFUSED until this is fixed (fail-closed, plan G2)",
			"err", err, "audit_root", cfg.AuditRoot)
	}
	b.GlobalLedger = lg
	// ANCHOR THE CLOCK CORRECTION NOW, at the same instant the ledger anchored
	// its own. ceilingNowMs takes its (wall, monotonic) reference on its first
	// call, and every later forward jump is measured against it. Left to anchor
	// lazily on the first admission, a jump between boot and that admission
	// would read as ordinary elapsed time — and every entry a previous process
	// recorded would silently fall out of the rolling window. The return value
	// is deliberately unused; the side effect is the point.
	_ = b.CeilingNowMs()

	slog.Info("global usage ceiling enabled",
		"global_budget_usd", cfg.GlobalBudgetUSD,
		"global_max_tasks", cfg.GlobalMaxTasks,
		"global_window", cfg.GlobalWindow,
		"ledger", broker.GlobalLedgerPath(cfg.AuditRoot))

	// TOTAL MODE is a supported configuration, not a mistake — the ledger folds
	// history into a checkpoint precisely so it can be bounded without decaying
	// — but it behaves unlike every other window in drydock and the difference
	// costs money to discover. aggregate_window: 0 is in-memory and resets on
	// every restart; global_window: 0 is DURABLE and deliberately survives one,
	// so an exhausted ceiling stays exhausted across reboots until an operator
	// acts. Say so once, at boot, where an operator will see it.
	if cfg.GlobalWindow <= 0 {
		slog.Warn("global usage ceiling: global_window is 0 (TOTAL mode), so nothing ever ages out — unlike aggregate_window this is durable across restarts, and once a limb is exhausted every task start is refused until you raise the limb or remove the ledger file",
			"ledger", broker.GlobalLedgerPath(cfg.AuditRoot))
	}

	// A budget smaller than ONE task's budget cannot be enforced the way an
	// operator reading it would expect: spend is metered post-hoc, so the first
	// task admitted against an empty window can overshoot the whole global
	// ceiling on its own before anything is recorded. This is the same post-hoc
	// bound task_budget_usd already carries, restated at global scale — and the
	// task-start limb is the honest answer for anyone who wants a hard one.
	if cfg.GlobalBudgetUSD > 0 && cfg.GlobalBudgetUSD < cfg.TaskBudgetUSD {
		slog.Warn("global usage ceiling: global_budget_usd is below task_budget_usd, so a single task can exhaust (and overshoot) the whole global budget before its spend is recorded; set global_max_tasks for a bound that is enforced at admission",
			"global_budget_usd", cfg.GlobalBudgetUSD, "task_budget_usd", cfg.TaskBudgetUSD)
	}

	// A USD-ONLY INSTALL HAS NO CRASH-RECOVERY BACKSTOP, and that is worth a line
	// at boot because the documented reasoning for the ceiling's biggest
	// deliberate blind spot ASSUMES the task limb is on.
	//
	// Boot reconciliation records every crash-lost task as USD 0 / USDTrusted
	// false — it will not read a dollar figure out of an agent-writable trace — and
	// deliberately does NOT raise the degraded flag, because degrading is sticky
	// for the daemon's life and would brick an unattended install after an ordinary
	// crash. What makes that trade sound is that THE START LIMB COUNTS EVERY ONE OF
	// THOSE TASKS EXACTLY, so the blind spot is bounded even though the dollars are
	// gone. With global_max_tasks at 0 that sentence is simply false: nothing counts
	// them, the under-count is real money, and it repeats without bound across
	// repeated crashes — ten crash-recovery cycles with four tasks in flight leaves
	// forty invisible starts and a ceiling that admits freely.
	//
	// It is a WARNING rather than a refusal, and rather than degrading the USD limb
	// on reconciled entries, for the reason above: a control that bricks an
	// unattended install after a routine crash is a worse failure than a bounded,
	// stated blind spot. The remedy is one line of config.
	if cfg.GlobalBudgetUSD > 0 && cfg.GlobalMaxTasks <= 0 {
		slog.Warn("global usage ceiling: global_budget_usd is set but global_max_tasks is 0, so crash recovery has NO backstop — a task killed before its ledger write is recorded with unknown spend (boot reconciliation will not read a dollar figure out of an agent-writable trace), and with no task-start limb nothing counts it at all; the under-count is unbounded across repeated crashes. Set global_max_tasks to bound it.",
			"global_budget_usd", cfg.GlobalBudgetUSD)
	}
}
