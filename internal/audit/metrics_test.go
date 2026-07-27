package audit

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenRead(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestLastMetricsFile_ParsesBrokerRow(t *testing.T) {
	f := writeTemp(t, `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"result","subtype":"success","is_error":false,"duration_ms":1200,"total_cost_usd":0.05,"num_turns":3,"src":"broker"}
{"type":"metrics","src":"broker","task_id":"abc","agent":"claude","vendor":"anthropic","auth":"api_key","repo":"github.com/o/r","stage_ms":{"preparing":100,"running":1200,"pushing":50},"egress_gate_wait_ms":0,"approval_gate_wait_ms":900,"requests":3,"diff_files":2,"diff_bytes":512,"cost_usd":0.05,"widen_requested":0,"widen_outcome":"none"}
`)
	m, ok := LastMetricsFile(f)
	if !ok {
		t.Fatal("no metrics row found")
	}
	if m.Agent != "claude" || m.Vendor != "anthropic" || m.Auth != "api_key" {
		t.Errorf("dims: %+v", m)
	}
	if m.StageMs.Running != 1200 || m.ApprovalGateWaitMs != 900 || m.Requests != 3 {
		t.Errorf("timings: %+v", m)
	}
}

func TestLastMetricsFile_ForgedRowSuperseded(t *testing.T) {
	// An in-VM agent prints a forged metrics row (even with src:broker);
	// the broker's real row comes after, and last-wins must pick it.
	f := writeTemp(t, `{"type":"metrics","src":"broker","task_id":"abc","cost_usd":0.000001,"agent":"forged"}
{"type":"metrics","src":"broker","task_id":"abc","agent":"claude","cost_usd":0.05}
`)
	m, ok := LastMetricsFile(f)
	if !ok || m.Agent != "claude" {
		t.Fatalf("last-wins violated: %+v ok=%v", m, ok)
	}
}

func TestLastMetricsFile_AbsentOnOldFiles(t *testing.T) {
	f := writeTemp(t, `{"type":"drydock_meta","subscription":true,"sensitive":false}
{"type":"result","subtype":"success","is_error":false,"duration_ms":10,"total_cost_usd":0,"num_turns":0,"src":"broker"}
`)
	if _, ok := LastMetricsFile(f); ok {
		t.Fatal("metrics reported on a pre-metrics file")
	}
}

func TestLastMetricsFile_IgnoresNonBrokerSrc(t *testing.T) {
	f := writeTemp(t, `{"type":"metrics","src":"agent","task_id":"abc","agent":"evil"}
`)
	if _, ok := LastMetricsFile(f); ok {
		t.Fatal("non-broker metrics row must be ignored")
	}
}
