package broker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assertLastLineInterrupted(t *testing.T, path string) {
	t.Helper()
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	last := lines[len(lines)-1]
	var x struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal([]byte(last), &x); err != nil {
		t.Fatalf("last line not JSON: %q", last)
	}
	if x.Type != "result" || x.Subtype != "interrupted" || !x.IsError {
		t.Errorf("last line = %+v, want type=result subtype=interrupted is_error=true", x)
	}
}

func TestTerminateStuckAudits_AppendsInterruptedAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-a.jsonl")
	body := `{"type":"drydock_meta","subscription":false}` + "\n" +
		`{"type":"stream_event"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := TerminateStuckAudits(dir)
	if err != nil || n != 1 {
		t.Fatalf("first pass = (%d,%v), want (1,nil)", n, err)
	}
	assertLastLineInterrupted(t, path)

	// Second pass: the interrupted line is itself a result line → no-op.
	after1, _ := os.ReadFile(path)
	n2, _ := TerminateStuckAudits(dir)
	after2, _ := os.ReadFile(path)
	if n2 != 0 || string(after1) != string(after2) {
		t.Errorf("second pass modified the trace (n=%d)", n2)
	}
}

// Guards the detection rule: a stream event whose TEXT payload contains the
// literal `"type":"result"` must NOT be mistaken for a real result line —
// otherwise a genuinely-crashed task would be skipped and stay "running?".
func TestTerminateStuckAudits_SubstringInPayloadIsNotAResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-b.jsonl")
	body := `{"type":"stream_event","text":"emitted {\"type\":\"result\"} as text"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := TerminateStuckAudits(dir)
	if err != nil || n != 1 {
		t.Fatalf("got (%d,%v), want (1,nil) — substring must not count as a result", n, err)
	}
	assertLastLineInterrupted(t, path)
}

func TestTerminateStuckAudits_LeavesCompletedTraceUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-c.jsonl")
	body := `{"type":"stream_event"}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	n, err := TerminateStuckAudits(dir)
	if err != nil || n != 0 {
		t.Fatalf("completed trace = (%d,%v), want (0,nil)", n, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("completed trace modified:\nbefore=%q\nafter=%q", before, after)
	}
}

func TestTerminateStuckAudits_MissingRootIsNoop(t *testing.T) {
	if n, err := TerminateStuckAudits(filepath.Join(t.TempDir(), "nope")); err != nil || n != 0 {
		t.Errorf("missing root = (%d,%v), want (0,nil)", n, err)
	}
}

// A trace larger than the 16KB tail window with NO result line must still be
// detected as stuck and terminated — exercises the hasResultLine seek branch.
func TestTerminateStuckAudits_LargeTraceNoResultIsTerminated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-big.jsonl")
	var b strings.Builder
	for b.Len() < 64*1024 { // well past the 16KB tail window
		b.WriteString(`{"type":"stream_event","delta":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}` + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := TerminateStuckAudits(dir)
	if err != nil || n != 1 {
		t.Fatalf("large no-result trace = (%d,%v), want (1,nil)", n, err)
	}
	assertLastLineInterrupted(t, path)
}

// A trace larger than the tail window WITH a result line as its last line must
// be left untouched — the seek must land such that the final result line is
// within the tail and is parsed.
func TestTerminateStuckAudits_LargeTraceWithResultIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-big2.jsonl")
	var b strings.Builder
	for b.Len() < 64*1024 {
		b.WriteString(`{"type":"stream_event","delta":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}` + "\n")
	}
	b.WriteString(`{"type":"result","subtype":"success","is_error":false}` + "\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	n, err := TerminateStuckAudits(dir)
	if err != nil || n != 0 {
		t.Fatalf("large completed trace = (%d,%v), want (0,nil)", n, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("large completed trace must be left untouched")
	}
}

// TestAppendLine_RefusesSymlink verifies the O_NOFOLLOW hardening: a symlink
// planted at the audit path must not let the boot-time interrupted-marker write
// pass through to another file.
func TestAppendLine_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.jsonl")
	if err := os.WriteFile(target, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := appendLine(link, "x\n"); err == nil {
		t.Fatal("appendLine should refuse a symlinked path (O_NOFOLLOW)")
	}
	if b, _ := os.ReadFile(target); len(b) != 0 {
		t.Errorf("write leaked through the symlink to the target: %q", b)
	}
}
