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
