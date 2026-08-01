package broker

import "time"

// HEADROOM: the global usage ceiling's read-only operator surface (plan G6).
//
// The plan states the problem plainly: before this file there was no endpoint,
// command or UI reporting aggregate ledger state, so an operator could not
// answer "how close am I to the cap?" A ceiling nobody can see is a ceiling
// nobody trusts — and worse, its first observable symptom is a 402 or a parked
// queue, which reads as a broken daemon rather than a working control.
//
// Two properties of this file are deliberate:
//
//  1. IT REUSES THE ENFORCEMENT PATH FOR ITS VERDICT. Blocked/Reason come from
//     globalCeilingExceededLocked — the same function the three admission
//     points call — rather than from a second comparison written against the
//     same numbers. A re-implementation would be free to drift, and the way it
//     would drift is "the status page says fine, the dispatcher parks", which
//     is precisely the confusion the surface exists to remove.
//
//  2. IT COUNTS IN-FLIGHT STARTS. The ledger is written at task TERMINAL, so a
//     status built from the ledger alone would report a smaller number than the
//     ceiling is actually enforcing, and an operator watching it would see the
//     count jump when tasks finish. Starts is recorded + in-flight, the same sum
//     admission compares.
//
// It takes capMu (never the reverse order), reads the ledger non-destructively,
// and mutates nothing.

// GlobalCeilingStatus is the operator-facing snapshot of the global usage
// ceiling: the configured limbs, what has been used against them in the current
// window, the honesty flags, and the verdict a task start would receive right
// now. Served by GET /admin/ceiling and rendered by `drydock stats`.
//
// A zero limb means that limb is OFF, and its Headroom* field is meaningless
// (reported as 0). Enabled false means neither limb is configured, in which
// case every other field is zero and NO ledger was consulted — that is the
// off-by-default identity, not a reading of an empty ledger.
type GlobalCeilingStatus struct {
	// Enabled is globalCeilingOn(): at least one limb is configured.
	Enabled bool `json:"enabled"`

	// The configured limbs. 0 = that limb is off.
	BudgetUSD float64 `json:"budget_usd"`
	MaxTasks  int     `json:"max_tasks"`
	// WindowMs is the window the LEDGER was opened with, read back from the
	// store rather than from config, so the surface reports what is actually
	// being measured. 0 = total mode. Window is its human rendering.
	WindowMs int64  `json:"window_ms"`
	Window   string `json:"window"`

	// SpentUSD is trustworthy BROKER-METERED spend in window. It never includes
	// an agent-reported total_cost_usd (G4) and never includes UntrustedUSD.
	SpentUSD float64 `json:"spent_usd"`
	// HeadroomUSD is BudgetUSD - SpentUSD, floored at 0. Meaningless (0) when
	// the USD limb is off.
	HeadroomUSD float64 `json:"headroom_usd"`
	// UntrustedUSD is in-window spend the ledger declines to count: unmetered
	// lanes (subscription, an openai_compat lane with no prices) and figures it
	// could not recover. Shown so an operator can see that dollars moved without
	// the ceiling claiming to have measured them.
	UntrustedUSD float64 `json:"untrusted_usd"`

	// RecordedStarts is task starts durably recorded in window; InFlightStarts
	// is starts admitted by THIS process that have not reached their terminal
	// (and so are not in the ledger yet). Starts is the sum — the number the
	// task limb is actually compared against.
	RecordedStarts int `json:"recorded_starts"`
	InFlightStarts int `json:"in_flight_starts"`
	Starts         int `json:"starts"`
	// HeadroomStarts is MaxTasks - Starts, floored at 0. Meaningless (0) when
	// the task limb is off.
	HeadroomStarts int `json:"headroom_starts"`

	// Entries is how many ledger records the window covers (a total-mode
	// checkpoint is one record covering many starts); Damaged is how many of
	// them are quarantined.
	Entries int `json:"entries"`
	Damaged int `json:"damaged"`
	// OldestMs/NewestMs bracket the in-window entries (0 when empty).
	OldestMs int64 `json:"oldest_ms"`
	NewestMs int64 `json:"newest_ms"`

	// The honesty flags, surfaced separately because they mean different things
	// and gate different limbs. Degraded says SpentUSD is a lower bound;
	// StartsDegraded says Starts is; LoadError says the file could not be read
	// in full, which degrades both.
	Degraded             bool   `json:"degraded"`
	DegradedReason       string `json:"degraded_reason,omitempty"`
	StartsDegraded       bool   `json:"starts_degraded"`
	StartsDegradedReason string `json:"starts_degraded_reason,omitempty"`
	LoadError            string `json:"load_error,omitempty"`

	// Blocked is the verdict a task start would receive RIGHT NOW, produced by
	// the enforcement path itself. Agent names which agent it was evaluated for
	// (the daemon's default_agent) — the ceiling is cross-vendor, but its
	// fail-closed rules include "the agent must resolve", so the verdict is
	// agent-scoped even though the limbs are not.
	Blocked bool   `json:"blocked"`
	Reason  string `json:"reason,omitempty"`
	Agent   string `json:"agent,omitempty"`
}

// GlobalCeilingStatus builds the headroom snapshot. Safe on a Broker with no
// ceiling configured (returns Enabled:false having touched nothing) and on one
// whose ledger failed to open (every GlobalLedger method is nil-safe).
func (b *Broker) GlobalCeilingStatus() GlobalCeilingStatus {
	s := GlobalCeilingStatus{
		Enabled:   b.globalCeilingOn(),
		BudgetUSD: b.GlobalBudgetUSD,
		MaxTasks:  b.GlobalMaxTasks,
	}
	if !s.Enabled {
		// The off-by-default identity: no lock, no agent resolution, no ledger
		// read. An operator asking about a ceiling that is off gets "off", not a
		// reading of a store that was never opened.
		return s
	}

	b.capMu.Lock()
	defer b.capMu.Unlock()

	// ONE ledger pass for the numbers, and it is the same call admission makes,
	// so the usage snapshot and the uncounted-claim tally come from a single
	// hold of the ledger's mutex.
	usage, inflight := b.GlobalLedger.UsageWithClaims(b.ceilingNowMs(), b.capInFlight, "")

	s.WindowMs = usage.WindowMs
	s.Window = globalWindowLabel(usage.WindowMs)
	s.SpentUSD = usage.USD
	s.UntrustedUSD = usage.UntrustedUSD
	s.RecordedStarts = usage.Starts
	s.InFlightStarts = inflight
	s.Starts = usage.Starts + inflight
	s.Entries = usage.Entries
	s.Damaged = usage.Damaged
	s.OldestMs = usage.OldestMs
	s.NewestMs = usage.NewestMs
	s.Degraded = usage.Degraded
	s.DegradedReason = safeStr(usage.DegradedReason)
	s.StartsDegraded = usage.StartsDegraded
	s.StartsDegradedReason = safeStr(usage.StartsDegradedReason)
	s.LoadError = safeStr(usage.LoadErr)

	if s.BudgetUSD > 0 {
		s.HeadroomUSD = headroomUSD(s.BudgetUSD, s.SpentUSD)
	}
	if s.MaxTasks > 0 {
		if h := s.MaxTasks - s.Starts; h > 0 {
			s.HeadroomStarts = h
		}
	}

	// The verdict from the enforcement path itself (see the file header). A
	// second ledger pass, but this is a read-only diagnostic endpoint, not an
	// admission point, and correctness-by-reuse is worth more here than one
	// avoided walk of an in-memory slice.
	s.Agent = b.DefaultAgent
	blocked, reason, _ := b.globalCeilingExceededLocked(b.DefaultAgent, "")
	s.Blocked, s.Reason = blocked, reason
	return s
}

// GlobalWindowDuration renders WindowMs as a Duration for callers that want to
// format it themselves. 0 stays 0 (total mode).
func (s GlobalCeilingStatus) GlobalWindowDuration() time.Duration {
	return time.Duration(s.WindowMs) * time.Millisecond
}
