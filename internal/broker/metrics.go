package broker

import (
	"encoding/json"
	"fmt"

	"drydock/internal/audit"
	"drydock/internal/trustbrief"
)

// stageMs is the per-stage wall-clock breakdown carried by the metrics row.
type stageMs struct {
	Preparing int64 `json:"preparing"`
	Running   int64 `json:"running"`
	Pushing   int64 `json:"pushing"`
}

// taskMetrics is the terminal broker-authored metrics row: one per task,
// written as the LAST line of the audit stream on every exit path once the
// audit log exists. Readers take the last such row (last-wins), mirroring
// the result-row trust rule: the broker writes it after the agent's output
// ends, so an in-VM agent printing a forged row is superseded.
type taskMetrics struct {
	Type               string  `json:"type"` // "metrics"
	Src                string  `json:"src"`  // "broker"
	TaskID             string  `json:"task_id"`
	Agent              string  `json:"agent"`
	Vendor             string  `json:"vendor"`
	Auth               string  `json:"auth"` // "api_key" | "subscription"
	Repo               string  `json:"repo"` // redacted, same form as the brief
	Model              string  `json:"model,omitempty"`
	StageMs            stageMs `json:"stage_ms"`
	EgressGateWaitMs   int64   `json:"egress_gate_wait_ms"`
	ApprovalGateWaitMs int64   `json:"approval_gate_wait_ms"`
	Requests           int     `json:"requests"`
	DiffFiles          int     `json:"diff_files"`
	DiffBytes          int64   `json:"diff_bytes"`
	CostUSD            float64 `json:"cost_usd"`
	WidenRequested     int     `json:"widen_requested"`
	WidenOutcome       string  `json:"widen_outcome"` // "none" | "approved"
}

// appendMetrics writes the terminal metrics row. Registered as a defer right
// after the audit log opens, so it runs on every exit path and lands after
// the terminal result row (defers registered later run first, so it still
// precedes the log Sync/Close pair). Cost is re-read from the audit's own
// last result row so live and resume paths agree with the displayed cost.
func (tr *taskRun) appendMetrics() {
	if tr.logf == nil {
		return
	}
	m := taskMetrics{
		Type: "metrics", Src: "broker", TaskID: tr.id,
		Agent: tr.agentName, Vendor: tr.taskVendor,
		Auth:               "api_key",
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
	if !tr.prepStart.IsZero() && !tr.taskStart.IsZero() {
		m.StageMs.Preparing = tr.taskStart.Sub(tr.prepStart).Milliseconds()
	}
	if !tr.taskStart.IsZero() && !tr.runEnd.IsZero() {
		m.StageMs.Running = tr.runEnd.Sub(tr.taskStart).Milliseconds()
	}
	m.StageMs.Pushing = tr.pushDur.Milliseconds()
	if rc, ok := tr.grant.(interface{ Requests() int }); ok {
		m.Requests = rc.Requests()
	}
	if b, err := json.Marshal(m); err == nil {
		_, _ = fmt.Fprintf(tr.logf, "%s\n", b)
	}
}
