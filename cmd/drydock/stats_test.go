package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if err := writeStats(&buf, statsDir(t), 30*24*time.Hour, "agent", false); err != nil {
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

func TestWriteStats_JSON(t *testing.T) {
	var buf bytes.Buffer
	if err := writeStats(&buf, statsDir(t), 30*24*time.Hour, "", true); err != nil {
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
	if err := writeStats(&buf, statsDir(t), 0, "flavor", false); err == nil {
		t.Fatal("unknown --by dimension must error")
	}
}
