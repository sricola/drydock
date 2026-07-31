package stats

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeAudit drops an audit fixture file with a given mtime.
func writeAudit(t *testing.T, dir, id, content string, mtime time.Time) {
	t.Helper()
	p := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

const newFormat = `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":60000,"total_cost_usd":0.10,"num_turns":4,"src":"broker"}
{"type":"metrics","src":"broker","task_id":"ID","agent":"claude","vendor":"anthropic","auth":"api_key","repo":"github.com/o/r","stage_ms":{"preparing":5000,"running":60000,"pushing":800},"egress_gate_wait_ms":0,"approval_gate_wait_ms":30000,"requests":4,"diff_files":2,"diff_bytes":512,"cost_usd":0.10,"widen_requested":0,"widen_outcome":"none"}
`

const oldFormat = `{"type":"drydock_meta","subscription":true,"sensitive":false}
{"type":"drydock_task","agent":"codex"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":30000,"total_cost_usd":0,"num_turns":0,"src":"broker"}
`

func TestCollect_MixedFormatsAndSinceFilter(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeAudit(t, dir, "new1", newFormat, now.Add(-1*time.Hour))
	writeAudit(t, dir, "old1", oldFormat, now.Add(-2*time.Hour))
	writeAudit(t, dir, "ancient", oldFormat, now.Add(-90*24*time.Hour))
	// A malformed file must be skipped, not fatal.
	writeAudit(t, dir, "garbled", "not json at all\n", now.Add(-1*time.Hour))
	// Orphan widen: task denied at the egress gate, no .jsonl ever existed.
	if err := os.WriteFile(filepath.Join(dir, "gone.widen.json"), []byte(`[{"host":"x.example.com","ports":[443]}]`), 0o600); err != nil {
		t.Fatal(err)
	}

	samples, orphans, _ := Collect(dir, now.Add(-30*24*time.Hour))
	if len(samples) != 3 { // new1, old1, garbled(as running/unknown), ancient filtered
		t.Fatalf("samples=%d, want 3 (ancient filtered by since)", len(samples))
	}
	if orphans != 1 {
		t.Errorf("orphanWidens=%d, want 1", orphans)
	}
	var newS, oldS *Sample
	for i := range samples {
		switch samples[i].ID {
		case "new1":
			newS = &samples[i]
		case "old1":
			oldS = &samples[i]
		}
	}
	if newS == nil || !newS.HasMetrics || newS.Vendor != "anthropic" || newS.Auth != "api_key" {
		t.Fatalf("new-format sample wrong: %+v", newS)
	}
	if oldS == nil || oldS.HasMetrics || oldS.Agent != "codex" || oldS.Metered {
		t.Fatalf("old-format sample wrong: %+v (agent from drydock_task, unmetered from meta)", oldS)
	}
	if oldS.Vendor != "openai" {
		t.Errorf("old-format vendor=%q, want openai derived from agent codex", oldS.Vendor)
	}
}

func TestSummarize_SpendGatesAndFallbacks(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeAudit(t, dir, "a", newFormat, now.Add(-1*time.Hour))
	writeAudit(t, dir, "b", oldFormat, now.Add(-25*time.Hour))
	samples, _, _ := Collect(dir, time.Time{})
	s := Summarize(samples)
	if s.Tasks != 2 || s.Outcomes["ok"] != 2 {
		t.Fatalf("summary counts wrong: %+v", s)
	}
	if s.SpendUSD != 0.10 || s.UnmeteredTasks != 1 {
		t.Errorf("spend must cover metered tasks only: %+v", s)
	}
	if s.ApprovalWaitP50Ms != 30000 {
		t.Errorf("approval wait p50=%d, want 30000 (zero-wait tasks excluded)", s.ApprovalWaitP50Ms)
	}
	if s.PreMetricsTasks != 1 {
		t.Errorf("pre_metrics_tasks=%d, want 1", s.PreMetricsTasks)
	}
	if s.DurP95Ms != 60000 {
		t.Errorf("dur p95=%d, want 60000", s.DurP95Ms)
	}
}

// TestOutcomeFor_DeniedPassthrough is the regression test for the
// deny-after-restart path: reconcile.go writes a broker-authored
// {"subtype":"denied","is_error":false,...} result row, and outcomeFor must
// pass that subtype through (mirroring audit.Outcome's default case)
// rather than collapsing it into "ok".
func TestOutcomeFor_DeniedPassthrough(t *testing.T) {
	dir := t.TempDir()
	denied := `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"denied","is_error":false,"duration_ms":0,"total_cost_usd":0.02,"num_turns":0,"src":"broker"}
`
	writeAudit(t, dir, "denied1", denied, time.Now())
	samples, _, _ := Collect(dir, time.Time{})
	if len(samples) != 1 {
		t.Fatalf("samples=%d, want 1", len(samples))
	}
	if samples[0].Outcome != "denied" {
		t.Errorf("Outcome=%q, want %q", samples[0].Outcome, "denied")
	}
}

// cancelledFixture is a new-format audit file where the result row alone is
// indistinguishable from a real agent error (subtype:"error"), but the
// metrics row's outcome field records what actually happened: the task was
// killed mid-run. This is the new-format fixture for the terminal outcome
// taxonomy (#200).
const cancelledFixture = `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"error","is_error":true,"duration_ms":5000,"total_cost_usd":0.01,"num_turns":1,"src":"broker"}
{"type":"metrics","src":"broker","task_id":"ID","agent":"claude","vendor":"anthropic","auth":"api_key","repo":"github.com/o/r","outcome":"cancelled","stage_ms":{"preparing":1000,"running":4000,"pushing":0},"egress_gate_wait_ms":0,"approval_gate_wait_ms":0,"requests":1,"diff_files":0,"diff_bytes":0,"cost_usd":0.01,"widen_requested":0,"widen_outcome":"none"}
`

// TestOutcomeFor_MetricsOutcomeOverridesCancelled is the regression test for
// the bug this field fixes: a task killed mid-run wrote a generic
// subtype:"error" result row, so stats bucketed it under "error"; the
// metrics row's outcome must win and bucket it under "cancelled" instead.
func TestOutcomeFor_MetricsOutcomeOverridesCancelled(t *testing.T) {
	dir := t.TempDir()
	writeAudit(t, dir, "cancelled1", cancelledFixture, time.Now())
	samples, _, _ := Collect(dir, time.Time{})
	if len(samples) != 1 {
		t.Fatalf("samples=%d, want 1", len(samples))
	}
	if samples[0].Outcome != "cancelled" {
		t.Errorf("Outcome=%q, want %q (metrics row must override the result row's generic error subtype)",
			samples[0].Outcome, "cancelled")
	}
}

// TestOutcomeFor_PreOutcomeFieldFallsBackToResultRow is the old-format
// fixture: newFormat's metrics row predates the outcome field entirely (no
// "outcome" key), so classification must fall back to the result row exactly
// as it did before this field existed: the fallback IS the unmodified key,
// not a separate code path.
func TestOutcomeFor_PreOutcomeFieldFallsBackToResultRow(t *testing.T) {
	dir := t.TempDir()
	writeAudit(t, dir, "old-metrics", newFormat, time.Now())
	samples, _, _ := Collect(dir, time.Time{})
	if len(samples) != 1 || samples[0].Outcome != "ok" {
		t.Fatalf("Outcome=%q, want %q (pre-outcome-field metrics row must not affect classification)",
			samples[0].Outcome, "ok")
	}
}

// queuedFixture is a queued task's audit file: stage_ms.queued records the
// time the task waited on the durable queue before dispatch.
const queuedFixture = `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":60000,"total_cost_usd":0.10,"num_turns":4,"src":"broker"}
{"type":"metrics","src":"broker","task_id":"ID","agent":"claude","vendor":"anthropic","auth":"api_key","repo":"github.com/o/r","outcome":"pushed","stage_ms":{"queued":1500,"preparing":5000,"running":60000,"pushing":800},"egress_gate_wait_ms":0,"approval_gate_wait_ms":30000,"requests":4,"diff_files":2,"diff_bytes":512,"cost_usd":0.10,"widen_requested":0,"widen_outcome":"none"}
`

// TestSummarize_QueueWaitPercentiles: stage_ms.queued aggregates into the
// queue-wait percentiles; a task that never sat on the queue (no queued key,
// i.e. every synchronous task) is excluded from the sample set rather than
// dragging the percentiles toward zero — same rule as the gate waits.
func TestSummarize_QueueWaitPercentiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeAudit(t, dir, "q1", queuedFixture, now.Add(-1*time.Hour))
	writeAudit(t, dir, "sync1", newFormat, now.Add(-1*time.Hour))
	samples, _, _ := Collect(dir, time.Time{})
	s := Summarize(samples)
	if s.QueueWaitSamples != 1 {
		t.Fatalf("QueueWaitSamples = %d, want 1 (synchronous task excluded)", s.QueueWaitSamples)
	}
	if s.QueueWaitP50Ms != 1500 || s.QueueWaitP95Ms != 1500 {
		t.Errorf("queue wait p50/p95 = %d/%d, want 1500/1500", s.QueueWaitP50Ms, s.QueueWaitP95Ms)
	}
}

func TestGroupBy_Dimensions(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeAudit(t, dir, "a", newFormat, now.Add(-1*time.Hour))
	writeAudit(t, dir, "b", oldFormat, now.Add(-1*time.Hour))
	samples, _, _ := Collect(dir, time.Time{})

	for _, dim := range []string{"agent", "vendor", "day", "week"} {
		gs, err := GroupBy(samples, dim)
		if err != nil || len(gs) == 0 {
			t.Fatalf("GroupBy(%s): %v groups=%d", dim, err, len(gs))
		}
	}
	gs, _ := GroupBy(samples, "agent")
	if len(gs) != 2 {
		t.Fatalf("agent groups=%d, want 2 (claude, codex)", len(gs))
	}
	if _, err := GroupBy(samples, "flavor"); err == nil {
		t.Fatal("unknown dimension must error")
	}
}

func TestPercentile_Exact(t *testing.T) {
	vals := []int64{10, 20, 30, 40}
	if p := percentile(vals, 50); p != 20 {
		t.Errorf("p50=%d, want 20", p)
	}
	if p := percentile(vals, 95); p != 40 {
		t.Errorf("p95=%d, want 40", p)
	}
	if p := percentile(nil, 50); p != 0 {
		t.Errorf("p50(empty)=%d, want 0", p)
	}
}

// --- G4: the spend total is BROKER-OBSERVED ---

// THE DEFECT, stated as a test: a task's agent prints its own result line with
// whatever total_cost_usd it likes. buildSample used to take the last result
// row of ANY src, so that number went straight into Sample.CostUSD and
// Summarize summed it into SpendUSD — the figure `drydock stats` prints as
// "spend: $X total". An agent could inflate or deflate the operator's spend
// report at will.
//
// After the fix the forged number is not in SpendUSD at all. It is not silently
// dropped either: a trace with NO broker row still has its figure carried, in
// AgentReportedUSD, where the renderer labels it.
func TestSummarize_AgentReportedCostIsNeverBrokerSpend(t *testing.T) {
	const meta = `{"type":"drydock_meta","subscription":false,"sensitive":false}` + "\n" +
		`{"type":"drydock_task","agent":"claude"}` + "\n"
	forged := `{"type":"result","subtype":"success","is_error":false,"duration_ms":10,"total_cost_usd":9999.99,"num_turns":1}` + "\n"
	brokered := `{"type":"result","subtype":"success","is_error":false,"duration_ms":10,"total_cost_usd":1.50,"num_turns":1,"src":"broker"}` + "\n"

	dir := t.TempDir()
	now := time.Now()
	// A real task: the broker metered $1.50, and the agent then claimed $9999.99
	// AFTER it — the ordering that used to win, since the read was last-wins.
	writeAudit(t, dir, "real", meta+brokered+forged, now.Add(-time.Hour))
	// A task with no broker row at all (still running, or killed before its
	// terminal): all we have is the agent's claim.
	writeAudit(t, dir, "agentonly", meta+forged, now.Add(-time.Hour))

	samples, _, _ := Collect(dir, time.Time{})
	if len(samples) != 2 {
		t.Fatalf("collected %d samples, want 2", len(samples))
	}
	s := Summarize(samples)

	if s.SpendUSD != 1.50 {
		t.Errorf("SpendUSD = %v, want exactly 1.50 — only the src==\"broker\" figure may be summed", s.SpendUSD)
	}
	if s.AgentReportedTasks != 1 || s.AgentReportedUSD != 9999.99 {
		t.Errorf("agent-reported = $%v over %d task(s), want $9999.99 over 1 — the number must be carried, not dropped",
			s.AgentReportedUSD, s.AgentReportedTasks)
	}
	for _, sm := range samples {
		if sm.ID == "real" && (sm.CostUSD != 1.50 || sm.HasAgentReportedUSD) {
			t.Errorf("real task: CostUSD=%v agentReported=%v, want 1.50/false", sm.CostUSD, sm.HasAgentReportedUSD)
		}
		if sm.ID == "agentonly" && (sm.CostUSD != 0 || !sm.HasAgentReportedUSD) {
			t.Errorf("agent-only task: CostUSD=%v agentReported=%v, want 0/true", sm.CostUSD, sm.HasAgentReportedUSD)
		}
	}
}

// A subscription lane meters no dollars at all, so an agent-reported figure
// there is not even a candidate: the task counts as unmetered and contributes
// nothing to either total.
func TestSummarize_UnmeteredLaneIgnoresAgentReportedCost(t *testing.T) {
	dir := t.TempDir()
	writeAudit(t, dir, "sub", `{"type":"drydock_meta","subscription":true,"sensitive":false}`+"\n"+
		`{"type":"drydock_task","agent":"claude"}`+"\n"+
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":10,"total_cost_usd":500,"num_turns":1}`+"\n",
		time.Now().Add(-time.Hour))
	s := Summarize(mustCollect(t, dir))
	if s.SpendUSD != 0 || s.AgentReportedUSD != 0 || s.AgentReportedTasks != 0 || s.UnmeteredTasks != 1 {
		t.Errorf("summary = spend $%v / agent $%v over %d / unmetered %d, want 0/0/0/1",
			s.SpendUSD, s.AgentReportedUSD, s.AgentReportedTasks, s.UnmeteredTasks)
	}
}

func mustCollect(t *testing.T, dir string) []Sample {
	t.Helper()
	samples, _, _ := Collect(dir, time.Time{})
	return samples
}
