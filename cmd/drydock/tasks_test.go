package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A failing task whose entrypoint dies before claude can write a `result`
// event used to leave `drydock tasks` showing "running?" forever. The broker
// now appends a synthetic result row; this asserts the consumer side reads
// it as "error" so the UI no longer hangs the row in a never-resolved state.
func TestSummarize_SyntheticErrorIsReadAsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-x.jsonl")
	// The same shape brokerd writes (broker.go).
	body := `{"type":"stream_event"}` + "\n" +
		`{"type":"result","subtype":"error","is_error":true,"duration_ms":420,"total_cost_usd":0,"num_turns":0}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	got := summarize("task-x", path, info)
	if got.outcome != "error" {
		t.Errorf("outcome = %q, want %q (synthetic terminal event must resolve to error, not running?)", got.outcome, "error")
	}
	if got.dur == "-" || got.dur == "" {
		t.Errorf("dur = %q, want a duration parsed from duration_ms", got.dur)
	}
}

// Without a terminal `result` event the row stays "running?" — this is the
// regression guard for the case the synthetic event is added to address.
func TestSummarize_NoResultStaysRunning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-y.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"stream_event"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	got := summarize("task-y", path, info)
	if got.outcome != "running?" {
		t.Errorf("outcome = %q, want %q", got.outcome, "running?")
	}
}
