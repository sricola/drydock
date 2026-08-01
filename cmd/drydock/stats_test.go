package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"drydock/internal/stats"
)

const statsNewFixture = `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":60000,"total_cost_usd":0.10,"num_turns":4,"src":"broker"}
{"type":"metrics","src":"broker","task_id":"a","agent":"claude","vendor":"anthropic","auth":"api_key","repo":"github.com/o/r","stage_ms":{"preparing":5000,"running":60000,"pushing":800},"egress_gate_wait_ms":0,"approval_gate_wait_ms":30000,"requests":4,"diff_files":2,"diff_bytes":512,"cost_usd":0.10,"widen_requested":0,"widen_outcome":"none"}
`

const statsOldFixture = `{"type":"drydock_meta","subscription":true,"sensitive":false}
{"type":"drydock_task","agent":"codex"}
{"type":"result","subtype":"error","is_error":true,"duration_ms":5000,"total_cost_usd":0,"num_turns":0,"src":"broker"}
`

func statsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for id, content := range map[string]string{"a": statsNewFixture, "b": statsOldFixture} {
		p := filepath.Join(dir, id+".jsonl")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestWriteStats_Text(t *testing.T) {
	var buf bytes.Buffer
	if err := writeStats(&buf, statsDir(t), 30*24*time.Hour, "agent", false, nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"tasks: 2", "ok: 1", "error: 1", "$0.10", "claude", "codex", "1 task predates metrics"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "$0.00") {
		t.Errorf("unmetered subscription task rendered as $0:\n%s", out)
	}
}

// TestWriteStats_AllOldFormat is the regression test for the duration
// fallback: a dir of only pre-upgrade files (result rows with real
// duration_ms, no metrics rows) must still render the real duration
// percentiles, not "-". PreMetricsTasks tracks the metrics row, not the
// result row, so it must not gate the duration display.
func TestWriteStats_AllOldFormat(t *testing.T) {
	dir := t.TempDir()
	old1 := `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":5000,"total_cost_usd":0.05,"num_turns":2,"src":"broker"}
`
	old2 := `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":60000,"total_cost_usd":0.20,"num_turns":6,"src":"broker"}
`
	for id, content := range map[string]string{"c": old1, "d": old2} {
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := writeStats(&buf, dir, 30*24*time.Hour, "agent", false, nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// p50 of [5000,60000] is 5000 (nearest-rank), p95 is 60000.
	if !strings.Contains(out, "dur p50/p95: 5.0s/1m00s") {
		t.Errorf("output missing real duration percentiles:\n%s", out)
	}
	if strings.Contains(out, "dur p50/p95: -") {
		t.Errorf("real durations rendered as absent:\n%s", out)
	}
	// The single claude group row must carry the real dur p50 too, not "-":
	// one "5.0s" from the overall line, a second from the group table.
	if strings.Count(out, "5.0s") < 2 {
		t.Errorf("group row lost its duration:\n%s", out)
	}
}

// TestWriteStats_DeniedOutcome is the regression test for the deny-after-
// restart path: a broker-authored {"subtype":"denied","is_error":false,...}
// result row must render as its own outcome line, not be folded into "ok".
func TestWriteStats_DeniedOutcome(t *testing.T) {
	dir := t.TempDir()
	denied := `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"denied","is_error":false,"duration_ms":0,"total_cost_usd":0.02,"num_turns":0,"src":"broker"}
`
	if err := os.WriteFile(filepath.Join(dir, "denied1.jsonl"), []byte(denied), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := writeStats(&buf, dir, 30*24*time.Hour, "", false, nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "denied: 1") {
		t.Errorf("output missing %q:\n%s", "denied: 1", out)
	}
}

// setup_failed is a first-class outcome in the fixed display list — it
// renders its own line and sorts with the fixed outcomes (before any
// alphabetical passthrough like "denied"), so its position is stable.
func TestWriteStats_SetupFailedInFixedOrder(t *testing.T) {
	dir := t.TempDir()
	failed := `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"setup_failed","is_error":false,"duration_ms":8000,"total_cost_usd":0,"num_turns":0,"src":"broker"}
`
	denied := `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"denied","is_error":false,"duration_ms":0,"total_cost_usd":0.02,"num_turns":0,"src":"broker"}
`
	for id, content := range map[string]string{"setupfail1": failed, "denied1": denied} {
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := writeStats(&buf, dir, 30*24*time.Hour, "", false, nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "setup_failed: 1") {
		t.Fatalf("output missing %q:\n%s", "setup_failed: 1", out)
	}
	sf, dn := strings.Index(out, "setup_failed: 1"), strings.Index(out, "denied: 1")
	if dn < 0 || sf > dn {
		t.Errorf("setup_failed must render in the fixed list, before the sorted passthrough outcomes:\n%s", out)
	}
}

// TestWriteStats_PolicyBlockedInFixedOrder: policy_blocked is a first-class
// outcome in the fixed display list — it renders its own line and sorts with
// the fixed outcomes (before any alphabetical passthrough like "denied"),
// so its position is stable across reports.
func TestWriteStats_PolicyBlockedInFixedOrder(t *testing.T) {
	dir := t.TempDir()
	blocked := `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"policy_blocked","is_error":false,"duration_ms":4000,"total_cost_usd":0.03,"num_turns":0,"src":"broker"}
`
	denied := `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"denied","is_error":false,"duration_ms":0,"total_cost_usd":0.02,"num_turns":0,"src":"broker"}
`
	for id, content := range map[string]string{"blocked1": blocked, "denied1": denied} {
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := writeStats(&buf, dir, 30*24*time.Hour, "", false, nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "policy_blocked: 1") {
		t.Fatalf("output missing %q:\n%s", "policy_blocked: 1", out)
	}
	pb, dn := strings.Index(out, "policy_blocked: 1"), strings.Index(out, "denied: 1")
	if dn < 0 || pb > dn {
		t.Errorf("policy_blocked must render in the fixed list, before the sorted passthrough outcomes:\n%s", out)
	}
}

// TestWriteStats_QueueWaitAndOutcomes: a queued task's stage_ms.queued renders
// as the queue-wait percentile line, and the queue's terminal vocabulary
// (completed, dead_letter) renders in the fixed outcome list — before any
// alphabetical passthrough outcome, same rule as policy_blocked.
func TestWriteStats_QueueWaitAndOutcomes(t *testing.T) {
	dir := t.TempDir()
	queued := `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":60000,"total_cost_usd":0.10,"num_turns":4,"src":"broker"}
{"type":"metrics","src":"broker","task_id":"q1","agent":"claude","vendor":"anthropic","auth":"api_key","repo":"github.com/o/r","outcome":"pushed","stage_ms":{"queued":1500,"preparing":5000,"running":60000,"pushing":800},"egress_gate_wait_ms":0,"approval_gate_wait_ms":30000,"requests":4,"diff_files":2,"diff_bytes":512,"cost_usd":0.10,"widen_requested":0,"widen_outcome":"none"}
`
	completed := `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"completed","is_error":false,"duration_ms":1000,"total_cost_usd":0.01,"num_turns":1,"src":"broker"}
`
	deadLetter := `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"dead_letter","is_error":false,"duration_ms":1000,"total_cost_usd":0.01,"num_turns":1,"src":"broker"}
`
	denied := `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"denied","is_error":false,"duration_ms":0,"total_cost_usd":0.02,"num_turns":0,"src":"broker"}
`
	for id, content := range map[string]string{
		"q1": queued, "c1": completed, "dl1": deadLetter, "denied1": denied,
	} {
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := writeStats(&buf, dir, 30*24*time.Hour, "", false, nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "queue wait p50/p95: 1.5s/1.5s") {
		t.Errorf("output missing queue wait percentiles:\n%s", out)
	}
	for _, want := range []string{"completed: 1", "dead_letter: 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	dn := strings.Index(out, "denied: 1")
	if dn < 0 || strings.Index(out, "completed: 1") > dn || strings.Index(out, "dead_letter: 1") > dn {
		t.Errorf("completed/dead_letter must render in the fixed list, before sorted passthrough outcomes:\n%s", out)
	}
}

func TestWriteStats_JSON(t *testing.T) {
	var buf bytes.Buffer
	if err := writeStats(&buf, statsDir(t), 30*24*time.Hour, "", true, nil); err != nil {
		t.Fatal(err)
	}
	var rep map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, buf.String())
	}
	overall, _ := rep["overall"].(map[string]any)
	if overall == nil || overall["tasks"] != float64(2) {
		t.Fatalf("overall.tasks wrong: %v", rep)
	}
}

func TestWriteStats_BadDimension(t *testing.T) {
	var buf bytes.Buffer
	if err := writeStats(&buf, statsDir(t), 0, "flavor", false, nil); err == nil {
		t.Fatal("unknown --by dimension must error")
	}
}

// A shop that only runs subscription (unmetered) tasks must never see a
// dollar amount in the overall spend line; renderGroups already held this
// rule, renderSpend regressed it.
func TestWriteStats_AllUnmeteredSpendNotZeroDollar(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(statsOldFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := writeStats(&buf, dir, 0, "", false, nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "$0.00") {
		t.Errorf("all-subscription dir rendered a dollar amount:\n%s", out)
	}
	if !strings.Contains(out, "spend: -") {
		t.Errorf("expected 'spend: -' for all-unmetered dir:\n%s", out)
	}
}

// planned is a first-class outcome in the fixed display list — it renders
// its own line and sorts with the fixed outcomes (before any alphabetical
// passthrough like "denied"), so its position is stable across reports.
func TestWriteStats_PlannedInFixedOrder(t *testing.T) {
	dir := t.TempDir()
	planned := `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"planned","is_error":false,"duration_ms":9000,"total_cost_usd":0.05,"num_turns":0,"src":"broker"}
`
	denied := `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"denied","is_error":false,"duration_ms":0,"total_cost_usd":0.02,"num_turns":0,"src":"broker"}
`
	for id, content := range map[string]string{"planned1": planned, "denied1": denied} {
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := writeStats(&buf, dir, 30*24*time.Hour, "", false, nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "planned: 1") {
		t.Fatalf("output missing %q:\n%s", "planned: 1", out)
	}
	pl, dn := strings.Index(out, "planned: 1"), strings.Index(out, "denied: 1")
	if dn < 0 || pl > dn {
		t.Errorf("planned must render in the fixed list, before the sorted passthrough outcomes:\n%s", out)
	}
}

// --- the global ceiling's headroom line (plan G6) ---

// The section must actually FIRE with real values — the B1 lesson is that a
// surface nobody can observe is worse than none. Both limbs, both headrooms,
// and the in-flight breakdown.
func TestRenderCeiling_ShowsBothLimbs(t *testing.T) {
	var buf bytes.Buffer
	renderCeiling(&buf, &ceilingStatus{
		Enabled: true, Window: "the last 24h0m0s", WindowMs: 86400000,
		BudgetUSD: 50, SpentUSD: 12.5, HeadroomUSD: 37.5,
		MaxTasks: 20, Starts: 7, RecordedStarts: 6, InFlightStarts: 1, HeadroomStarts: 13,
	})
	out := buf.String()
	for _, want := range []string{
		"global ceiling (the last 24h0m0s)",
		"$12.50 of $50.00 broker-metered — $37.50 left",
		"starts: 7 of 20 — 13 left (6 recorded, 1 in flight)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("headroom output missing %q:\n%s", want, out)
		}
	}
}

// Off, and unreachable, both render NOTHING: `drydock stats` on a stock install
// (or with brokerd stopped) is byte-identical to what it printed before.
func TestRenderCeiling_SilentWhenOffOrAbsent(t *testing.T) {
	for name, s := range map[string]*ceilingStatus{
		"brokerd unreachable": nil,
		"ceiling off":         {Enabled: false},
	} {
		var buf bytes.Buffer
		renderCeiling(&buf, s)
		if buf.Len() != 0 {
			t.Errorf("%s: rendered %q, want nothing", name, buf.String())
		}
	}
}

// A degraded ledger, a blocked verdict and quarantined entries must be visible
// beside the numbers: an operator seeing "$3 of $50" AND a refusal needs both
// facts, or the daemon just looks broken.
func TestRenderCeiling_ShowsDegradedAndBlocked(t *testing.T) {
	var buf bytes.Buffer
	renderCeiling(&buf, &ceilingStatus{
		Enabled: true, Window: "the last 1h0m0s",
		BudgetUSD: 50, SpentUSD: 3, HeadroomUSD: 47,
		Degraded: true, DegradedReason: "a line was quarantined",
		StartsDegraded: true, StartsDegradedReason: "a checkpoint was lost",
		LoadError: "the ledger could not be read",
		Damaged:   2,
		Blocked:   true, Reason: "global ceiling: refused",
	})
	out := buf.String()
	for _, want := range []string{
		"DEGRADED: the spend total is a lower bound — a line was quarantined",
		"DEGRADED: the start count is a lower bound — a checkpoint was lost",
		"DEGRADED: the ledger could not be read in full",
		"2 quarantined ledger entr(ies) in window",
		"BLOCKED: global ceiling: refused",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("headroom output missing %q:\n%s", want, out)
		}
	}
}

// Only the configured limb is reported. A USD-only install must not see a
// "starts: 0 of 0" line that reads like an exhausted cap.
func TestRenderCeiling_OmitsAnUnconfiguredLimb(t *testing.T) {
	var buf bytes.Buffer
	renderCeiling(&buf, &ceilingStatus{Enabled: true, Window: "total", MaxTasks: 5, Starts: 1, HeadroomStarts: 4})
	if out := buf.String(); strings.Contains(out, "spend:") {
		t.Errorf("USD limb off but a spend line was printed:\n%s", out)
	}
	buf.Reset()
	renderCeiling(&buf, &ceilingStatus{Enabled: true, Window: "total", BudgetUSD: 5, SpentUSD: 1, HeadroomUSD: 4})
	if out := buf.String(); strings.Contains(out, "starts:") {
		t.Errorf("task limb off but a starts line was printed:\n%s", out)
	}
}

// --- G4: the spend line ---

// An agent-reported figure must not be inside the "spend: $X total" number, and
// must not vanish either — it gets its own labelled line.
func TestRenderSpend_AgentReportedIsSeparateAndLabelled(t *testing.T) {
	var buf bytes.Buffer
	renderSpend(&buf, stats.Summary{
		Tasks: 2, SpendUSD: 1.5, SpendPerDayUSD: 1.5,
		AgentReportedUSD: 9999.99, AgentReportedTasks: 1,
	})
	out := buf.String()
	if !strings.Contains(out, "spend: $1.50 total") {
		t.Errorf("total must be the broker-metered $1.50 only:\n%s", out)
	}
	if !strings.Contains(out, "not included: $9999.99 self-reported by 1 task(s)") {
		t.Errorf("the agent-reported figure must be shown and labelled:\n%s", out)
	}
	// And nothing extra when there is none.
	buf.Reset()
	renderSpend(&buf, stats.Summary{Tasks: 1, SpendUSD: 1.5, SpendPerDayUSD: 1.5})
	if strings.Contains(buf.String(), "not included") {
		t.Errorf("no agent-reported tasks, but a line was printed:\n%s", buf.String())
	}
}

// --json gains global_ceiling ADDITIVELY: stats.Report is embedded, so every
// existing key stays exactly where it was and a consumer parsing today's shape
// is unaffected. Omitted entirely when the daemon could not be asked.
func TestWriteStats_JSONCarriesTheCeilingAdditively(t *testing.T) {
	var buf bytes.Buffer
	if err := writeStats(&buf, statsDir(t), 30*24*time.Hour, "", true,
		&ceilingStatus{Enabled: true, MaxTasks: 9, Starts: 2, HeadroomStarts: 7}); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(buf.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, buf.String())
	}
	if _, ok := body["overall"]; !ok {
		t.Error("the existing top-level `overall` key moved or disappeared")
	}
	gc, ok := body["global_ceiling"].(map[string]any)
	if !ok {
		t.Fatalf("global_ceiling missing or not an object: %s", buf.String())
	}
	if gc["max_tasks"] != float64(9) || gc["headroom_starts"] != float64(7) {
		t.Errorf("global_ceiling = %v, want the limb and headroom", gc)
	}

	buf.Reset()
	if err := writeStats(&buf, statsDir(t), 30*24*time.Hour, "", true, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "global_ceiling") {
		t.Errorf("brokerd unreachable but global_ceiling was emitted: %s", buf.String())
	}
}
