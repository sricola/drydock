package main

import (
	"os"
	"path/filepath"
	"strings"
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
	got := summarize("task-x", path, info, false)
	if got.outcome != "error" {
		t.Errorf("outcome = %q, want %q (synthetic terminal event must resolve to error, not running?)", got.outcome, "error")
	}
	if got.dur == "-" || got.dur == "" {
		t.Errorf("dur = %q, want a duration parsed from duration_ms", got.dur)
	}
}

// summarize now reads only the file tail to find the result line. Guard the
// seek path: a large (>16KB) preamble of stream events followed by the result
// line at the very end must still resolve — the tail read starts mid-line, but
// the final result line is fully within the tail.
func TestSummarize_FindsResultPastTailSeek(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-big.jsonl")
	var b strings.Builder
	for b.Len() < 64*1024 { // well past the 16KB tail window
		b.WriteString(`{"type":"stream_event","delta":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}` + "\n")
	}
	b.WriteString(`{"type":"result","subtype":"success","duration_ms":1000,"total_cost_usd":0.0500,"num_turns":3}` + "\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	got := summarize("task-big", path, info, false)
	if got.outcome != "ok (3 turn)" {
		t.Errorf("outcome = %q, want %q (tail read must find the final result line)", got.outcome, "ok (3 turn)")
	}
	if got.cost != "$0.0500" {
		t.Errorf("cost = %q, want $0.0500", got.cost)
	}
}

// TestCostCell_Subscription asserts that costCell returns the literal string
// "subscription" when the subscription flag is true, regardless of the USD value.
func TestCostCell_Subscription(t *testing.T) {
	if got := costCell(true /*subscription*/, 0); got != "subscription" {
		t.Errorf("costCell=%q want subscription", got)
	}
}

// TestCostCell_APIKey asserts that when subscription is false, costCell formats
// the USD value as "$x.xxxx" (four decimal places).
func TestCostCell_APIKey(t *testing.T) {
	got := costCell(false, 0.0338)
	want := "$0.0338"
	if got != want {
		t.Errorf("costCell=%q, want %q", got, want)
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
	got := summarize("task-y", path, info, false)
	if got.outcome != "running?" {
		t.Errorf("outcome = %q, want %q", got.outcome, "running?")
	}
}
