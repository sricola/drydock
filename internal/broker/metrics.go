package broker

import (
	"encoding/json"
	"fmt"

	"drydock/internal/audit"
	"drydock/internal/trustbrief"
)

// appendMetrics writes the terminal broker-authored metrics row: one per
// task, the LAST line of the audit stream on every exit path once the audit
// log exists. Readers take the last such row (last-wins), mirroring the
// result-row trust rule: the broker writes it after the agent's output
// ends, so an in-VM agent printing a forged row is superseded. The row's
// shape IS audit.Metrics, marshalled directly, so the writer and the reader
// cannot drift.
//
// Registered as a defer right after the audit log opens, so it runs on
// every exit path and lands after the terminal result row (defers
// registered later run first, so it still precedes the log Sync/Close
// pair). Cost is re-read from the audit's own last result row so live and
// resume paths agree with the displayed cost.
//
// NOTE for the global usage ceiling: this hook runs on every exit path FROM
// THE AUDIT-LOG OPEN ONWARD, and returns immediately when tr.logf is nil.
// That is why the ceiling's durable ledger write is NOT folded in here —
// every terminal before the audit log exists (a denied egress widen, a clone
// or quota failure, the disk/push preflights, an unresolvable agent, a mint
// failure) is still a real task START and must be counted. See
// globalrecord.go, which defers its write on runLifecycle's first line
// instead. CostUSD below is likewise display-only and deliberately unused by
// the ceiling: audit.TotalCost does not filter Src (G4).
func (tr *taskRun) appendMetrics() {
	if tr.logf == nil {
		return
	}
	m := audit.Metrics{
		Type: "metrics", Src: "broker", TaskID: tr.id,
		Agent: tr.agentName, Vendor: tr.taskVendor,
		Auth:    "api_key",
		Outcome: tr.outcome,
		// The one BROKER-AUTHORED absolute instant in the whole trace. Boot
		// reconciliation (cmd/brokerd) reads it so the ceiling's rolling
		// window is computed from when the task RAN, instead of falling back
		// to the file's mtime the way seedAggregateFromAudit still does.
		// Taken from the b.now clock seam, like every other broker timestamp.
		EndedAtMs:          tr.b.nowMs(),
		Repo:               trustbrief.RedactRepoRef(tr.repoRef),
		Model:              tr.model,
		EgressGateWaitMs:   tr.egressGateWait.Milliseconds(),
		ApprovalGateWaitMs: tr.approvalGateWait.Milliseconds(),
		DiffFiles:          tr.diffFiles,
		DiffBytes:          tr.diffBytes,
		CostUSD:            audit.TotalCost(tr.auditPath),
		WidenRequested:     len(tr.egressExtra),
		WidenOutcome:       tr.widenOutcome,
	}
	if tr.subscription {
		m.Auth = "subscription"
	}
	if m.WidenOutcome == "" {
		m.WidenOutcome = "none"
	}
	// Stage times partition the task's wall-clock: preparing ends at the
	// first stage that follows it — setup when an execution profile ran
	// (setupStart), else the agent start (taskStart). Without the setupStart
	// anchor, preparing would span prep+setup while Setup is also recorded
	// below, double-counting the setup phase.
	prepEnd := tr.taskStart
	if !tr.setupStart.IsZero() {
		prepEnd = tr.setupStart
	}
	if !tr.prepStart.IsZero() && !prepEnd.IsZero() {
		m.StageMs.Preparing = prepEnd.Sub(tr.prepStart).Milliseconds()
	}
	if !tr.taskStart.IsZero() && !tr.runEnd.IsZero() {
		m.StageMs.Running = tr.runEnd.Sub(tr.taskStart).Milliseconds()
	}
	// Queued is set only on the dispatcher path (runQueued): time from
	// enqueue to dispatch. Synchronous /tasks tasks leave it 0, and
	// omitempty keeps their row shape exactly as before the queue existed.
	m.StageMs.Queued = tr.queuedMs
	m.StageMs.Setup = tr.setupDur.Milliseconds()
	m.StageMs.Verifying = tr.verifyDur.Milliseconds()
	m.StageMs.Pushing = tr.pushDur.Milliseconds()
	if rc, ok := tr.grant.(interface{ Requests() int }); ok {
		m.Requests = rc.Requests()
	}
	if b, err := json.Marshal(m); err == nil {
		_, _ = fmt.Fprintf(tr.logf, "%s\n", b)
	}
}
