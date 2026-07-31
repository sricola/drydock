package broker

import (
	"fmt"
	"time"

	"drydock/internal/provider"
)

// This file is the GLOBAL USAGE CEILING's ENFORCEMENT (docs/superpowers/plans/
// 2026-07-31-global-ceiling.md, Task 2). globalledger.go is the durable store
// it measures against; this is the one place that turns a measurement into a
// refusal, and it is consulted at all three admission points:
//
//	POST /tasks     (broker.go HandleTask)   -> 402 with the reason
//	the dispatcher  (queue.go takeDispatchable) -> park a human item, DROP a retry
//	the CI retry    (ciretryloop.go gate 8)  -> refuse, reason on the audit row
//
// ---------------------------------------------------------------------------
// G1. TWO LIMBS, because a subscription lane has no enforceable USD.
// ---------------------------------------------------------------------------
//
//	GlobalBudgetUSD  windowed, cross-vendor, BROKER-METERED spend. Meaningful
//	                 only where metering is real (api_key lanes, priced
//	                 openai-compat lanes).
//	GlobalMaxTasks   windowed TASK STARTS, across every vendor and every auth
//	                 mode. This is the limb that actually bounds subscription
//	                 mode, and it works everywhere because it counts an event
//	                 the broker itself causes rather than a number a vendor
//	                 reports.
//
// Either may trip on its own; both default to 0 = OFF, and with both off this
// whole file is inert — it does not resolve an agent, does not read the ledger
// and never takes a lock. That identity is asserted directly
// (TestGlobalCap_OffIsIdentity), including with a missing, corrupt or
// unreadable ledger, because "off" has to mean off even when the store is
// broken.
//
// ---------------------------------------------------------------------------
// G2. FAIL-CLOSED, which is the substantive break with every existing check.
// ---------------------------------------------------------------------------
//
// vendorExceeded and HandleTask's per-vendor pre-check both silently ADMIT when
// they cannot determine the answer: a nil hook, an agent that will not resolve,
// an empty vendor string. For a ceiling that is backwards — an unattended loop
// that cannot be measured is exactly the thing to stop — so every one of those
// conditions REFUSES here. The two checks coexist and the stricter answer wins;
// nothing in this file changes what the per-vendor cap does.
//
// The refusal is gated PER LIMB, and that qualifier is load-bearing rather than
// pedantic. GlobalUsage.Degraded means "the USD figure is a LOWER BOUND", and
// it is set by a quarantined ledger line — which globalledger.go still counts
// as a task start, so the START limb has lost nothing. Refusing on Degraded
// when GlobalBudgetUSD is 0 would let one corrupt byte brick a deployment that
// is not even using the USD limb, permanently in total mode. So:
//
//	quarantined line (Usage.Degraded, ledger readable) -> USD limb refuses,
//	                                                      START limb unaffected
//	whole file unreadable (LoadError)                  -> BOTH limbs refuse,
//	                                                      because the starts we
//	                                                      never read are missing
//	                                                      from the count too
//
// Every refusal carries an honest, actionable reason: which limb tripped, the
// numbers on both sides of it, the window, and — for a degradation — whether it
// clears on its own and which file to repair if it does not.
//
// ---------------------------------------------------------------------------
// G5. THE CEILING REFUSES STARTS. IT NEVER KILLS A RUNNING TASK.
// ---------------------------------------------------------------------------
//
// Nothing here cancels a context, deletes a container, or touches a live task.
// A task that was admitted keeps its own per-task lease as its bound and runs
// to its natural terminal even if the ceiling slams shut behind it — the money
// is already spent, and terminating in-flight work would buy a half-finished
// tree and a task killed after its diff was staged for no saving.
//
// ---------------------------------------------------------------------------
// THE ORDERING PROBLEM, and the in-flight claim that closes it.
// ---------------------------------------------------------------------------
//
// The ledger is written at task TERMINAL (Task 3). Between a task's admission
// and its terminal, its start is INVISIBLE to Usage(). A check that read only
// the ledger would therefore let N tasks race through one limb of N-1: they all
// read the same count, they all pass, and the ceiling is a suggestion.
//
// The fix is an IN-FLIGHT CLAIM taken in the SAME critical section as the
// check. admitGlobalStart resolves the agent, reads the ledger, adds the
// uncounted in-flight claims, compares — and, if it admits, records the claim
// before releasing capMu. Two admissions therefore cannot both observe the same
// pre-state, and the task-start limb has NO overshoot at all
// (TestGlobalCap_ConcurrentAdmissionsCannotOvershoot: 64 racing admissions
// against a limb of 3 admit exactly 3).
//
// The claim is keyed by TASK ID and counted only while the LEDGER DOES NOT
// ALREADY HAVE THAT ID. That is what makes it safe against Task 3's write
// ordering: a terminal records the entry and then the lifecycle's defer
// releases the claim, so for one instant the task is both claimed and recorded.
// Counting it twice would silently tighten the ceiling by up to
// max_concurrent_tasks; counting it zero times would loosen it. Keying on
// identity makes the answer exact in either order, so Task 3 does not have to
// get an ordering right that nothing would have caught if it got it wrong.
//
// RESIDUAL, stated precisely, because a ceiling with an undocumented hole is
// worse than no ceiling:
//
//	START LIMB: no overshoot. A start is counted from the instant it is
//	admitted, whether or not it has reached the ledger. The one gap is a task
//	RESUMED at the diff-approval gate after a restart (resumePush /
//	ResumeAwaiting): it was admitted in a previous process, so this process
//	holds no claim for it and its ledger entry does not exist until it
//	terminates. That is bounded by max_concurrent_tasks, is not a fresh spend
//	decision (the task already ran), and Task 3's boot reconciliation against
//	the audit is what makes it converge.
//
//	USD LIMB: bounded overshoot, unavoidably. An admitted task's spend is not
//	knowable until it ends, so the ceiling can be crossed by at most
//	  (uncounted in-flight tasks) x (per-task lease budget)
//	which is bounded by max_concurrent_tasks x task_budget_usd — the same
//	quantity that already bounds one dispatch pass today. It is NOT unbounded
//	and a retry storm cannot drive through it, because every retry is itself a
//	task start that must first pass the start limb and must first claim a
//	concurrency slot. Reserving each in-flight task's full lease budget against
//	the limb would remove even that overshoot, and is deliberately not done: it
//	would refuse every task on any install whose global_budget_usd is smaller
//	than max_concurrent_tasks x task_budget_usd, which is a far likelier and
//	far worse failure than an overshoot of one concurrency window. G7 already
//	names the task-start limb as the backstop for exactly the cases where the
//	USD figure cannot be trusted, and this is one of them.

// globalCeilingPrefix opens every refusal this file authors. It is a stable,
// greppable marker for operators and for the tests, and it is the reason an
// operator can tell a ceiling refusal from the per-vendor cap's at a glance.
const globalCeilingPrefix = "global ceiling: "

// globalCeilingOn reports whether either limb is configured. It is the single
// gate on the whole feature: false means every entry point below returns
// immediately, taking no lock, resolving no agent and reading no ledger, so a
// stock install behaves exactly as it did before this file existed.
func (b *Broker) globalCeilingOn() bool {
	return b.GlobalBudgetUSD > 0 || b.GlobalMaxTasks > 0
}

// globalCeilingExceeded is the CHECK without the claim: does a task start
// authored right now pass the ceiling?
//
// It is what the CI-retry gate asks, because enqueuing is not starting — the
// child becomes an ordinary queued item and the dispatcher re-asks (and does
// claim) when it actually dispatches. Admission points that are about to START
// a task must use admitGlobalStart instead, or two of them can pass the same
// limb.
func (b *Broker) globalCeilingExceeded(agent string) (blocked bool, reason string) {
	if !b.globalCeilingOn() {
		return false, ""
	}
	b.capMu.Lock()
	defer b.capMu.Unlock()
	return b.globalCeilingExceededLocked(agent, "")
}

// admitGlobalStart is CHECK-AND-CLAIM under one lock: it answers the same
// question globalCeilingExceeded does and, when it admits, records taskID as an
// in-flight start so a concurrent admission counts it.
//
// The caller MUST pair an admitted call with releaseGlobalStart(taskID) on
// every exit path — a leaked claim is a permanent unit of ceiling. Both live
// callers do it from a defer registered immediately after the claim.
//
// Re-admitting an id that is already claimed is harmless and does not
// double-count (the claim is a set, and the id excludes itself from its own
// in-flight tally).
func (b *Broker) admitGlobalStart(taskID, agent string) (blocked bool, reason string) {
	if !b.globalCeilingOn() {
		return false, ""
	}
	b.capMu.Lock()
	defer b.capMu.Unlock()
	if blocked, reason := b.globalCeilingExceededLocked(agent, taskID); blocked {
		return true, reason
	}
	if b.capInFlight == nil {
		b.capInFlight = make(map[string]struct{})
	}
	b.capInFlight[taskID] = struct{}{}
	return false, ""
}

// releaseGlobalStart drops taskID's in-flight claim. Idempotent, safe for an id
// that never claimed and safe for the empty string, because it runs from
// lifecycle defers that also fire on paths which never reached the claim.
func (b *Broker) releaseGlobalStart(taskID string) {
	if taskID == "" || !b.globalCeilingOn() {
		return
	}
	b.capMu.Lock()
	delete(b.capInFlight, taskID)
	b.capMu.Unlock()
}

// inFlightStarts is how many admitted-but-unreleased starts this process holds.
// Diagnostics and tests; Task 4's headroom surface will want it too, since a
// number an operator reads should include the starts that have not settled.
func (b *Broker) inFlightStarts() int {
	b.capMu.Lock()
	defer b.capMu.Unlock()
	return len(b.capInFlight)
}

// globalCeilingExceededLocked is the whole decision. Caller holds capMu.
//
// selfID, when non-empty, is the id of the start being decided: it is excluded
// from the in-flight tally so a re-admission of an already-claimed task does
// not count itself.
//
// Lock order is capMu -> GlobalLedger.mu, and it is the only nesting in the
// file. Nothing takes capMu while holding the ledger's lock, and nothing here
// takes queueMu (the dispatcher calls in while holding it, never the reverse).
func (b *Broker) globalCeilingExceededLocked(agent, selfID string) (bool, string) {
	// FAIL-CLOSED #1: the agent must resolve. Neither limb actually needs the
	// vendor — the ceiling is cross-vendor and cross-auth-mode by construction
	// — but G2 is explicit that an unresolvable identity is a refusal, and the
	// alternative is a ceiling that admits precisely the tasks it understands
	// least. vendorExceeded returns false here; this returns the opposite, on
	// purpose, and only ever when a limb is enforced.
	agentName, _, err := b.resolveAgent(agent)
	if err != nil {
		return true, fmt.Sprintf(
			"%sthe agent could not be resolved (%s), so the ceiling cannot be evaluated for it; refusing the task start because %s is enforced (fail-closed, plan G2)",
			globalCeilingPrefix, safeErr(err), b.enabledLimbs())
	}
	// FAIL-CLOSED #2: an empty vendor. Unreachable through resolveAgent, which
	// rejects an unknown agent first — this is the explicit mirror of
	// vendorExceeded's `v != ""` fail-open, kept so the two files can be read
	// against each other and the difference is visible.
	if v, _ := provider.VendorForAgent(agentName); v == "" {
		return true, fmt.Sprintf(
			"%sagent %q resolves to no vendor, so the ceiling cannot be evaluated; refusing the task start because %s is enforced (fail-closed, plan G2)",
			globalCeilingPrefix, safeStr(agentName), b.enabledLimbs())
	}

	// One non-destructive pass over the in-memory ledger. A nil store answers
	// Degraded with an "unavailable" reason, which both limbs below turn into a
	// refusal — a configured limb with no store to measure it is exactly the
	// "I don't know" G2 defines as "no".
	usage := b.GlobalLedger.Usage(b.nowMs())
	loadErr := b.GlobalLedger.LoadError()
	window := globalWindowLabel(usage.WindowMs)

	// --- the USD limb ---
	if b.GlobalBudgetUSD > 0 {
		if usage.Degraded {
			return true, fmt.Sprintf(
				"%sglobal_budget_usd is enforced and the spend total cannot be trusted, so the task start is refused (fail-closed, plan G2)%s Detail: %s",
				globalCeilingPrefix, b.degradedRemedy(usage), safeStr(usage.DegradedReason))
		}
		if usage.USD >= b.GlobalBudgetUSD {
			return true, fmt.Sprintf(
				"%sglobal_budget_usd is exhausted: $%.2f of $%.2f broker-metered in %s (headroom $%.2f, across %d recorded task starts). The task start is refused; it is admitted again when spend ages out of the window, or raise global_budget_usd.",
				globalCeilingPrefix, usage.USD, b.GlobalBudgetUSD, window,
				headroomUSD(b.GlobalBudgetUSD, usage.USD), usage.Starts)
		}
	}

	// --- the task-start limb ---
	if b.GlobalMaxTasks > 0 {
		// Only a WHOLE-FILE read failure degrades this limb. A quarantined line
		// does not: globalledger.go counts a tombstone as one task start, which
		// is not a guess (the line exists because an entry was written), so the
		// count is still complete. Bytes we never read are a different matter —
		// the starts in them are simply missing.
		if loadErr != "" {
			return true, fmt.Sprintf(
				"%sglobal_max_tasks is enforced and the ledger could not be read in full, so the start count is only a lower bound and the task start is refused (fail-closed, plan G2). Detail: %s",
				globalCeilingPrefix, safeStr(loadErr))
		}
		inflight := b.uncountedInFlightLocked(selfID)
		starts := usage.Starts + inflight
		if starts >= b.GlobalMaxTasks {
			return true, fmt.Sprintf(
				"%sglobal_max_tasks is exhausted: %d of %d task starts in %s (%d recorded, %d in flight and not yet recorded). The task start is refused; it is admitted again when a start ages out of the window or an in-flight task finishes, or raise global_max_tasks.",
				globalCeilingPrefix, starts, b.GlobalMaxTasks, window, usage.Starts, inflight)
		}
	}
	return false, ""
}

// uncountedInFlightLocked counts admitted starts the ledger does not yet know
// about. Keying on ledger presence rather than on a bare counter is what makes
// the tally exact regardless of whether Task 3's terminal write lands before or
// after the claim is released; see the ordering discussion above. Caller holds
// capMu.
//
// It is O(in-flight), which is bounded by max_concurrent_tasks — single digits
// — on an admission path that already does a full ledger pass.
func (b *Broker) uncountedInFlightLocked(selfID string) int {
	n := 0
	for id := range b.capInFlight {
		if id == selfID {
			continue
		}
		if b.GlobalLedger.Has(id) {
			continue // already counted by Usage(); counting it again would tighten the ceiling
		}
		n++
	}
	return n
}

// enabledLimbs names the configured limbs, so a refusal an operator did not
// expect says which setting produced it.
func (b *Broker) enabledLimbs() string {
	switch {
	case b.GlobalBudgetUSD > 0 && b.GlobalMaxTasks > 0:
		return "global_budget_usd and global_max_tasks"
	case b.GlobalBudgetUSD > 0:
		return "global_budget_usd"
	case b.GlobalMaxTasks > 0:
		return "global_max_tasks"
	}
	return "no global limb"
}

// degradedRemedy is the actionable half of a degraded-USD refusal, and it is
// different in the two window modes — Task 1's first concern. Windowed, the
// damage ages out on its own and the refusal is temporary. In TOTAL mode
// nothing ever ages out, so the install stays refused until an operator repairs
// the file, and the message has to name the file and say so plainly rather than
// implying it will clear.
func (b *Broker) degradedRemedy(u GlobalUsage) string {
	if u.WindowMs > 0 {
		return " It clears when the affected entries age out of the window; set global_budget_usd to 0 to disable the USD limb."
	}
	// A store that never opened has no path to name, so do not offer a file to
	// repair that the operator cannot find.
	if p := b.GlobalLedger.Path(); p != "" {
		return fmt.Sprintf(
			" global_window is total mode, so this does NOT age out: repair or remove %s, or set global_budget_usd to 0 to disable the USD limb.",
			p)
	}
	return " global_window is total mode, so this does NOT age out until the ledger is readable again; set global_budget_usd to 0 to disable the USD limb."
}

// globalWindowLabel renders the window for an operator-facing message.
func globalWindowLabel(windowMs int64) string {
	if windowMs <= 0 {
		return "total (global_window is 0, so nothing ages out)"
	}
	return "the last " + (time.Duration(windowMs) * time.Millisecond).String()
}

// headroomUSD is what is left under the budget, floored at zero: a negative
// headroom is arithmetic, not information.
func headroomUSD(budget, spent float64) float64 {
	if h := budget - spent; h > 0 {
		return h
	}
	return 0
}
