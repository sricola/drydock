package audit

import (
	"os"
	"path/filepath"
	"testing"
)

func writeJSONL(t *testing.T, lines ...string) (path string, size int64) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "id.jsonl")
	data := ""
	for _, l := range lines {
		data += l + "\n"
	}
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	return p, fi.Size()
}

func TestOutcomeAndCost(t *testing.T) {
	meta := `{"type":"drydock_meta","subscription":false,"sensitive":false}`
	subMeta := `{"type":"drydock_meta","subscription":true,"sensitive":false}`
	sens := `{"type":"drydock_meta","subscription":false,"sensitive":true}`

	cases := []struct {
		name        string
		lines       []string
		wantOutcome string
		wantCost    string
		wantDur     bool
	}{
		{"success with turns", []string{meta, `{"type":"result","subtype":"success","is_error":false,"duration_ms":12,"total_cost_usd":0.0731,"num_turns":2}`}, "ok (2 turn)", "$0.0731", true},
		{"success no turns", []string{meta, `{"type":"result","subtype":"success","is_error":false,"duration_ms":12,"total_cost_usd":0,"num_turns":0}`}, "ok", "$0.0000", true},
		{"error", []string{meta, `{"type":"result","subtype":"error","is_error":true,"duration_ms":5,"total_cost_usd":0.01,"num_turns":1}`}, "error", "$0.0100", true},
		{"interrupted", []string{meta, `{"type":"result","subtype":"interrupted","is_error":false,"duration_ms":0,"total_cost_usd":0,"num_turns":0}`}, "interrupted", "$0.0000", false},
		{"subscription", []string{subMeta, `{"type":"result","subtype":"success","is_error":false,"duration_ms":3,"total_cost_usd":0,"num_turns":1}`}, "ok (1 turn)", "subscription", true},
		{"sensitive suffix", []string{sens, `{"type":"result","subtype":"success","is_error":false,"duration_ms":3,"total_cost_usd":0,"num_turns":1}`}, "ok (1 turn) · sensitive", "$0.0000", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, sz := writeJSONL(t, tc.lines...)
			last, ok := LastResult(p, sz)
			if !ok {
				t.Fatal("expected a result line")
			}
			m := ReadMeta(p)
			if got := Outcome(last, ok, m); got != tc.wantOutcome {
				t.Errorf("Outcome = %q, want %q", got, tc.wantOutcome)
			}
			if got := Cost(m, last, ok); got != tc.wantCost {
				t.Errorf("Cost = %q, want %q", got, tc.wantCost)
			}
			if got := HasDuration(last, ok); got != tc.wantDur {
				t.Errorf("HasDuration = %v, want %v", got, tc.wantDur)
			}
		})
	}
}

func TestNoResultLine(t *testing.T) {
	p, sz := writeJSONL(t, `{"type":"drydock_meta","subscription":false,"sensitive":false}`)
	last, ok := LastResult(p, sz)
	if ok {
		t.Fatal("expected ok=false with no result line")
	}
	if Outcome(last, ok, ReadMeta(p)) != "running?" {
		t.Errorf("Outcome should be running? when no result")
	}
	if Cost(ReadMeta(p), last, ok) != "-" {
		t.Errorf("Cost should be - when no result")
	}
}

// --- TotalCost ---

func TestTotalCost_LastResultLine(t *testing.T) {
	p, _ := writeJSONL(t,
		`{"type":"result","total_cost_usd":0.05}`,
		`{"type":"result","total_cost_usd":0.11}`,
	)
	if c := TotalCost(p); c != 0.11 {
		t.Errorf("TotalCost = %v, want 0.11", c)
	}
}

func TestTotalCost_NoResult(t *testing.T) {
	p, _ := writeJSONL(t, `{"type":"drydock_meta"}`)
	if c := TotalCost(p); c != 0 {
		t.Errorf("TotalCost with no result = %v, want 0", c)
	}
}

// --- HasResultLine ---

func TestHasResultLine_Present(t *testing.T) {
	p, _ := writeJSONL(t,
		`{"type":"stream_event"}`,
		`{"type":"result","subtype":"success","is_error":false}`,
	)
	has, err := HasResultLine(p)
	if err != nil || !has {
		t.Errorf("HasResultLine = (%v,%v), want (true,nil)", has, err)
	}
}

func TestHasResultLine_Absent(t *testing.T) {
	p, _ := writeJSONL(t, `{"type":"stream_event"}`)
	has, err := HasResultLine(p)
	if err != nil || has {
		t.Errorf("HasResultLine = (%v,%v), want (false,nil)", has, err)
	}
}

func TestHasResultLine_SubstringInPayloadNotCounted(t *testing.T) {
	// A line whose text payload contains "type":"result" must NOT be mistaken
	// for a real terminal result line.
	p, _ := writeJSONL(t, `{"type":"stream_event","text":"emitted {\"type\":\"result\"} as text"}`)
	has, err := HasResultLine(p)
	if err != nil || has {
		t.Errorf("HasResultLine with substring-only = (%v,%v), want (false,nil)", has, err)
	}
}

func TestHasResultLine_MissingFile(t *testing.T) {
	_, err := HasResultLine("/nonexistent/path/task.jsonl")
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

// --- Reason ---

func TestReason_LastInfraLine(t *testing.T) {
	p, _ := writeJSONL(t,
		`{"type":"drydock_meta","subscription":true}`,
		"[6/6] Starting container [0s]",
		"/usr/local/bin/entrypoint.sh: line 5: DRYDOCK_GW_IP: missing gateway ip",
	)
	got, ok := Reason(p)
	want := "/usr/local/bin/entrypoint.sh: line 5: DRYDOCK_GW_IP: missing gateway ip"
	if !ok || got != want {
		t.Errorf("Reason = %q,%v; want %q,true", got, ok, want)
	}
}

func TestReason_NoMeaningfulLine(t *testing.T) {
	p, _ := writeJSONL(t, `{"type":"result"}`, "[1/6] x")
	if _, ok := Reason(p); ok {
		t.Error("want ok=false when only JSON/progress lines are present")
	}
}

func TestReason_SkipsJSONArray(t *testing.T) {
	p, _ := writeJSONL(t,
		"container failed to start: no route to host",
		`["progress","starting"]`,
	)
	got, ok := Reason(p)
	want := "container failed to start: no route to host"
	if !ok || got != want {
		t.Errorf("Reason = %q,%v; want %q,true", got, ok, want)
	}
}

func TestReason_PrefersErrorOverTrailingNoise(t *testing.T) {
	// codex prints incidental output (a token count) after its real error
	// line; Reason must surface the error, not the trailing count.
	p, _ := writeJSONL(t,
		`{"type":"drydock_meta","subscription":true}`,
		"ERROR: exceeded retry limit, last status: 429 Too Many Requests",
		"1,633",
		`{"type":"result","subtype":"error","is_error":true}`,
	)
	got, ok := Reason(p)
	want := "ERROR: exceeded retry limit, last status: 429 Too Many Requests"
	if !ok || got != want {
		t.Errorf("Reason = %q,%v; want %q,true", got, ok, want)
	}
}

// TestTaskAgent reads the agent from the drydock_task line, and returns "" when
// no such line is present.
func TestTaskAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	os.WriteFile(path, []byte(
		`{"type":"drydock_meta","subscription":false,"sensitive":false}`+"\n"+
			`{"type":"drydock_task","repo_ref":"r","instruction":"i","agent":"codex"}`+"\n"+
			`{"type":"result","subtype":"success","total_cost_usd":0.5}`+"\n"), 0o600)
	if got := TaskAgent(path); got != "codex" {
		t.Errorf("TaskAgent = %q, want codex", got)
	}
	// No drydock_task line -> "".
	p2 := filepath.Join(dir, "old.jsonl")
	os.WriteFile(p2, []byte(`{"type":"result","subtype":"success"}`+"\n"), 0o600)
	if got := TaskAgent(p2); got != "" {
		t.Errorf("TaskAgent(no task line) = %q, want empty", got)
	}
}

// TestHasResultLine_RefusesSymlink verifies audit reads use O_NOFOLLOW: a
// planted symlink in the audit dir can't redirect the outcome read.
func TestHasResultLine_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(target, []byte(`{"type":"result","subtype":"success"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "l.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := HasResultLine(link); err == nil {
		t.Error("HasResultLine should refuse a symlinked audit path (O_NOFOLLOW)")
	}
}

func TestOutcome_PushFailed(t *testing.T) {
	r := Result{Type: "result", Subtype: "push_failed", IsError: false, NumTurns: 0}
	if got := Outcome(r, true, Meta{}); got != "push failed" {
		t.Errorf("Outcome(push_failed) = %q, want \"push failed\"", got)
	}
}
