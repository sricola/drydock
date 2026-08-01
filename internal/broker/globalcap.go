package broker

import (
	"fmt"
	"log/slog"
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
// Either may trip on its own; both default to 0 = OFF, and with both off no
// decision path here does anything — it does not resolve an agent, does not read
// the ledger, and never refuses. That identity is asserted directly
// (TestGlobalCap_OffIsIdentity), including with a missing, corrupt or
// unreadable ledger, because "off" has to mean off even when the store is
// broken.
//
// releaseGlobalStart is the ONE function that still runs with the limbs off, and
// deliberately: a claim taken while a limb was on must be released even if the
// limb is turned off before the task ends, or the claim leaks for the life of
// the process and counts against the ceiling forever once a limb is turned back
// on. It takes an uncontended mutex and deletes from a map.
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
//	terminates.
//
//	That gap is NOT bounded by max_concurrent_tasks, and an earlier version of
//	this comment claimed it was. ResumeAwaiting spawns one goroutine per
//	surviving gate marker with no bound at all, and resumePush takes neither a
//	concurrency slot nor a ceiling claim. What actually bounds it is that a
//	resume is not a spend decision: resumePush replays a diff that a previous
//	process already produced and paid for — it never runs an agent, never mints
//	a lease, and cannot spend — so the tasks in the gap can add to the RECORDED
//	total (Task 3 records their terminal, and boot reconciliation against the
//	audit converges the rest) but cannot consume budget while they sit there.
//	The correct statement of the residual is therefore: for the interval
//	between boot and a resumed task's terminal, its start is invisible to the
//	start limb, and the number of such tasks is bounded by how many were parked
//	at the diff gate when the previous process died.
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
	blocked, reason, _ = b.globalCeilingExceededDetail(agent)
	return blocked, reason
}

// globalCeilingExceededDetail is globalCeilingExceeded plus WHY, reduced to the
// one distinction a caller can act on: did the ceiling measure this start and
// find it over a limb (`unmeasured` false), or could it not measure at all
// (`unmeasured` true)?
//
// The two are the same refusal but not the same FACT, and treating them alike
// destroys work. "Over cap" is a real, self-clearing condition — spend ages out
// of the window, in-flight tasks finish, an operator raises the limb. "Cannot
// measure" is a fault: an agent that would not resolve this second, a ledger
// that could not be read, a store that has not been opened yet. Dead-lettering
// an unattended retry for a fault makes a transient failure into irreversible
// work loss, with nothing left to retry from.
func (b *Broker) globalCeilingExceededDetail(agent string) (blocked bool, reason string, unmeasured bool) {
	if !b.globalCeilingOn() {
		return false, "", false
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
	blocked, reason, _ = b.admitGlobalStartDetail(taskID, agent)
	return blocked, reason
}

// admitGlobalStartDetail is admitGlobalStart with the over-cap vs cannot-measure
// distinction described on globalCeilingExceededDetail.
func (b *Broker) admitGlobalStartDetail(taskID, agent string) (blocked bool, reason string, unmeasured bool) {
	if !b.globalCeilingOn() {
		return false, "", false
	}
	b.capMu.Lock()
	defer b.capMu.Unlock()
	if blocked, reason, unmeasured := b.globalCeilingExceededLocked(agent, taskID); blocked {
		return true, reason, unmeasured
	}
	if b.capInFlight == nil {
		b.capInFlight = make(map[string]struct{})
	}
	b.capInFlight[taskID] = struct{}{}
	return false, "", false
}

// releaseGlobalStart drops taskID's in-flight claim. Idempotent, safe for an id
// that never claimed and safe for the empty string, because it runs from
// lifecycle defers that also fire on paths which never reached the claim.
//
// It deliberately does NOT gate on globalCeilingOn. A claim held by a task
// whose limbs were turned off between its admission and its terminal would
// otherwise never be released, and would then count permanently against the
// ceiling the moment a limb was turned back on — a unit of budget leaked for
// the life of the process, per task. Deleting from a map is cheap enough that
// buying that risk back for it is not a trade worth making, and Task 4's admin
// surface is expected to make a limb changeable at runtime.
func (b *Broker) releaseGlobalStart(taskID string) {
	if taskID == "" {
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
func (b *Broker) globalCeilingExceededLocked(agent, selfID string) (blocked bool, reason string, unmeasured bool) {
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
			globalCeilingPrefix, safeErr(err), b.enabledLimbs()), true
	}
	// FAIL-CLOSED #2: an empty vendor. Unreachable through resolveAgent, which
	// rejects an unknown agent first — this is the explicit mirror of
	// vendorExceeded's `v != ""` fail-open, kept so the two files can be read
	// against each other and the difference is visible.
	if v, _ := provider.VendorForAgent(agentName); v == "" {
		return true, fmt.Sprintf(
			"%sagent %q resolves to no vendor, so the ceiling cannot be evaluated; refusing the task start because %s is enforced (fail-closed, plan G2)",
			globalCeilingPrefix, safeStr(agentName), b.enabledLimbs()), true
	}

	// ONE non-destructive pass over the in-memory ledger, which also answers
	// which in-flight claims the ledger has not counted yet — a single call, and
	// therefore a single hold of the ledger's mutex, because the two halves of
	// this decision must come from ONE view of the store.
	//
	// Two calls would not merely be slower, they would be WRONG. Reading usage
	// and then asking about each claim drops and re-takes the ledger's lock, and
	// a task terminal recording in that gap counts ZERO times: the usage
	// snapshot predates the write so it excludes the task, and the claim walk
	// then sees the id present and skips it as "already counted". capMu does not
	// close that hole — the race is against the LEDGER's lock, which capMu does
	// not hold. LoadErr rides along in the same snapshot for the same reason.
	//
	// A nil store answers Degraded with an "unavailable" reason, which both
	// limbs below turn into a refusal — a configured limb with no store to
	// measure it is exactly the "I don't know" G2 defines as "no".
	usage, inflight := b.GlobalLedger.UsageWithClaims(b.ceilingNowMs(), b.capInFlight, selfID)
	loadErr := usage.LoadErr
	window := globalWindowLabel(usage.WindowMs)

	// --- the USD limb ---
	if b.GlobalBudgetUSD > 0 {
		if usage.Degraded {
			return true, fmt.Sprintf(
				"%sglobal_budget_usd is enforced and the spend total cannot be trusted, so the task start is refused (fail-closed, plan G2)%s Detail: %s",
				globalCeilingPrefix, b.degradedRemedy(usage), safeStr(usage.DegradedReason)), true
		}
		if usage.USD >= b.GlobalBudgetUSD {
			return true, fmt.Sprintf(
				"%sglobal_budget_usd is exhausted: $%.2f of $%.2f broker-metered in %s (headroom $%.2f, across %d recorded task starts). The task start is refused; it is admitted again when spend ages out of the window, or raise global_budget_usd.",
				globalCeilingPrefix, usage.USD, b.GlobalBudgetUSD, window,
				headroomUSD(b.GlobalBudgetUSD, usage.USD), usage.Starts), false
		}
	}

	// --- the task-start limb ---
	if b.GlobalMaxTasks > 0 {
		// An ordinary quarantined line does NOT degrade this limb:
		// globalledger.go counts such a tombstone as one task start, which is
		// not a guess (the line exists because an entry was written), so the
		// count is still complete and one corrupt byte must not brick a
		// deployment that is not even using the USD limb.
		//
		// StartsDegraded is where that identity actually breaks, and it is a
		// refusal: bytes we never read (their starts are simply missing), a
		// destroyed CHECKPOINT whose every copy was lost (it carried the folded
		// start count of the entire history, so scoring it as one start would
		// reset the limb by everything before it — permanently, in total mode,
		// because the repair rewrite persists the reset), or a destroyed region
		// that took its newlines with it (one tombstone standing for N entries).
		// The ledger reports it; this refuses on it, because a smaller number we
		// cannot justify is exactly the under-count G2 forbids.
		if loadErr != "" {
			return true, fmt.Sprintf(
				"%sglobal_max_tasks is enforced and the ledger could not be read in full, so the start count is only a lower bound and the task start is refused (fail-closed, plan G2). Detail: %s",
				globalCeilingPrefix, safeStr(loadErr)), true
		}
		if usage.StartsDegraded {
			return true, fmt.Sprintf(
				"%sglobal_max_tasks is enforced and the recorded start count is only a lower bound, so the task start is refused (fail-closed, plan G2).%s Detail: %s",
				globalCeilingPrefix, b.degradedRemedyStarts(usage), safeStr(usage.StartsDegradedReason)), true
		}
		starts := usage.Starts + inflight
		if starts >= b.GlobalMaxTasks {
			return true, fmt.Sprintf(
				"%sglobal_max_tasks is exhausted: %d of %d task starts in %s (%d recorded, %d in flight and not yet recorded). The task start is refused; it is admitted again when a start ages out of the window or an in-flight task finishes, or raise global_max_tasks.",
				globalCeilingPrefix, starts, b.GlobalMaxTasks, window, usage.Starts, inflight), false
		}
	}
	return false, "", false
}

// ---------------------------------------------------------------------------
// THE HOST CLOCK, and why the ceiling does not measure against it directly.
// ---------------------------------------------------------------------------
//
// Both limbs are windowed, so the whole control rests on "now". A host clock
// that jumps FORWARD by more than the window moves the query cutoff past every
// entry the ledger holds: usage reads {USD 0, Starts 0} and the ceiling admits
// freely, silently, for a full window. That is a fail-OPEN in a control whose
// contract is that "I don't know" means "no", and the trigger is mundane — an
// NTP correction on a host that booted with a bad RTC, a VM resumed from a
// snapshot, an operator running `date`.
//
// The pruning path already defends itself by anchoring to the ledger's own
// newest event (pruneLocked), because deleting the record is unrecoverable. The
// QUERY cannot use that anchor: an anchored query would never let anything age
// out on an idle install, so a deployment that once hit its cap would refuse
// forever. "Every entry is older than the window" is the SAME picture for a
// clock that jumped and for a daemon that has simply been idle, and nothing in
// the ledger can tell them apart.
//
// So the ceiling asks a source the wall clock cannot corrupt: MONOTONIC time.
// It anchors a (wall, monotonic) pair once, and the amount by which wall time
// has outrun monotonic time since then is, by definition, the clock having been
// moved rather than having passed. The window is then measured against a
// jump-corrected instant. This is exact, not heuristic, and it CANNOT misfire
// on an idle daemon: an idle hour advances both clocks equally, the skew stays
// zero, and entries age out normally.
//
// The correction only ever widens the window (skew is floored at zero and never
// decreases), so its error direction is over-counting — the direction a ceiling
// is allowed to take.
//
// RESIDUAL, stated plainly: a clock change that happens while brokerd is DOWN
// is not detectable this way, because a monotonic reading does not survive a
// reboot and every timestamp on disk came from the same wall clock that moved.
// That case is logged at open (globalledger.go's boot check) rather than
// refused, because refusing it would also refuse every legitimately idle
// install, and an unattended install that cannot start a task after a quiet
// week is a worse failure than a one-window reset after an operator moved the
// clock across a restart.
//
// THE SECOND RESIDUAL, and it is the price of the fixed tolerance below rather
// than a bug in it: movement UNDER the tolerance is discarded, not banked, so a
// wall clock that gains just under ceilingClockToleranceMs between every two
// CONSECUTIVE observations walks the query cutoff forward without ever being
// corrected. Measured, so the shape is on the record rather than guessed at:
// 4000 samples of +1.9 s wall against +1 ms monotonic bought 2106 admissions
// against an exhausted 1-hour window.
//
// What that costs an attacker is the reason it is a documented trade and not a
// finding. The rate it requires is ~1900x real time SUSTAINED — every sample,
// for the length of the window — where a slewing NTP client is capped at 500 ppm
// (1.0005x) and a step correction is a JUMP the tolerance catches on the first
// sample. It is not reachable by drift, not reachable from inside a task VM
// (nothing in a VM can set the host clock), and reachable on the host only by
// root, who can move the ledger instead. Widening the tolerance would make it
// harder and the far likelier failure — a real 2-second scheduler stall or NTP
// step charged to skew, widening the window forever — more common; a
// rate-proportional tolerance is the WRITE clock's shape (see
// ledgerClockDriftDivisor) and is wrong here, where observations are frequent
// and the interval between them carries no information.

// ceilingClockToleranceMs is how much wall-vs-monotonic movement BETWEEN TWO
// CONSECUTIVE OBSERVATIONS is treated as drift rather than as a clock change.
// Ordinary NTP slew and a slow crystal move the two apart by milliseconds;
// scheduler jitter adds a few more. A jump worth correcting for is orders of
// magnitude larger, and the ceiling's window is measured in hours.
//
// It is FIXED, and sub-tolerance movement is discarded rather than banked, which
// is a real (measured) residual and a deliberate trade. See "THE SECOND
// RESIDUAL" above for the rate it takes to walk and why widening it would cost
// more than it bought.
const ceilingClockToleranceMs = 2000

// ceilingNowMs is the instant the ceiling measures its window against: the host
// clock, less however far the host clock has jumped forward since this process
// anchored itself. See the discussion above.
//
// Detection is DISABLED for a broker with an injected wall clock and no
// injected monotonic source — that is a test driving time deliberately, and
// calling a deliberate advance a "jump" would make the ledger's window untestable
// while telling the operator nothing. Production installs inject neither seam
// and get the real pair.
// CeilingNowMs is ceilingNowMs for callers outside this package — cmd/brokerd's
// boot reconciliation, which is the ledger's other WRITER and must measure
// against the same corrected instant the enforcement path does. Calling it also
// ANCHORS the correction, which is why the ceiling's wiring calls it once at
// configuration time: an anchor taken lazily on the first admission would treat
// a jump that happened between boot and that admission as ordinary elapsed
// time, and every entry a previous process recorded would fall out of the
// window unnoticed.
func (b *Broker) CeilingNowMs() int64 { return b.ceilingNowMs() }

func (b *Broker) ceilingNowMs() int64 {
	now := b.nowMs()
	if b.now != nil && b.mono == nil {
		return now
	}
	mono := b.monoMs()

	b.capClockMu.Lock()
	defer b.capClockMu.Unlock()
	if !b.capClockAnchored {
		b.capClockAnchored, b.capClockWall, b.capClockMono = true, now, mono
		return now
	}
	// The delta is measured against the PREVIOUS observation, not against a
	// fixed anchor, and only a delta over the tolerance is accumulated. That
	// distinguishes a JUMP (seconds or hours appearing between two consecutive
	// observations) from ordinary DRIFT — NTP slewing the wall clock's rate, or
	// a crystal that runs slow — which would otherwise accumulate into the skew
	// over a long uptime and widen the window for no reason.
	//
	// A negative delta is a BACKWARD wall jump. It is deliberately not
	// corrected: a backward clock moves the cutoff back and counts MORE, which
	// is the over-counting direction a ceiling is allowed to take.
	delta := (now - b.capClockWall) - (mono - b.capClockMono)
	b.capClockWall, b.capClockMono = now, mono
	if delta >= ceilingClockToleranceMs {
		b.capClockSkew += delta
		slog.Warn("global ceiling: the host wall clock jumped forward relative to monotonic time",
			"jump_ms", delta, "total_skew_ms", b.capClockSkew,
			"note", "the ceiling's rolling window is corrected by this amount so the jump cannot silently reset either limb")
	}
	return now - b.capClockSkew
}

// monoMs is a monotonic millisecond reading. It is a seam only so a test can
// drive a clock jump deterministically; nothing else overrides it.
func (b *Broker) monoMs() int64 {
	if b.mono != nil {
		return b.mono()
	}
	return int64(time.Since(processStartMono) / time.Millisecond)
}

// processStartMono is the monotonic anchor for monoMs. time.Now carries a
// monotonic reading, and time.Since uses it, so this is immune to every later
// wall-clock change.
var processStartMono = time.Now()

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
// It does NOT name the ledger's path, and that omission is deliberate. This
// string is rendered into a 402 body for whoever submitted the task; the host's
// audit-root layout is not theirs to learn, and a refusal is exactly the moment
// a prober would go looking. The operator gets the path from the broker log,
// which names the file on every degrade (globalledger.go warns with "path" on
// quarantine, repair and preservation).
func (b *Broker) degradedRemedy(u GlobalUsage) string {
	if u.WindowMs > 0 {
		return " It clears when the affected entries age out of the window; set global_budget_usd to 0 to disable the USD limb."
	}
	return " global_window is total mode, so this does NOT age out: repair or remove the global ledger file (the broker log names it), or set global_budget_usd to 0 to disable the USD limb."
}

// degradedRemedyStarts is degradedRemedy's task-start-limb twin. It is a
// separate string because the remedy differs: the start limb is what bounds a
// subscription lane, so "turn the limb off" is a much heavier suggestion and the
// file repair is the real answer.
func (b *Broker) degradedRemedyStarts(u GlobalUsage) string {
	if u.WindowMs > 0 {
		return " It clears when the affected entries age out of the window; repairing the global ledger file (the broker log names it) clears it immediately."
	}
	return " global_window is total mode, so this does NOT age out: repair or remove the global ledger file (the broker log names it), or set global_max_tasks to 0 to disable the task-start limb."
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
