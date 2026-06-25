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
	got := summarize("task-x", path, info)
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
	got := summarize("task-big", path, info)
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
	got := summarize("task-y", path, info)
	if got.outcome != "running?" {
		t.Errorf("outcome = %q, want %q", got.outcome, "running?")
	}
}

// readMeta reads the per-task drydock_meta line so the cost column reflects how
// the task ACTUALLY ran (subscription) and whether it was marked sensitive.
func TestReadMeta(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	sub := write("sub.jsonl", `{"type":"drydock_meta","subscription":true,"sensitive":false}`+"\n"+`{"type":"result","subtype":"success"}`+"\n")
	if m := readMeta(sub); !m.Subscription || m.Sensitive {
		t.Errorf("subscription meta = %+v, want Subscription=true Sensitive=false", m)
	}
	sens := write("sens.jsonl", `{"type":"drydock_meta","subscription":false,"sensitive":true}`+"\n")
	if m := readMeta(sens); m.Subscription || !m.Sensitive {
		t.Errorf("sensitive meta = %+v, want Subscription=false Sensitive=true", m)
	}
	legacy := write("legacy.jsonl", `{"type":"stream_event"}`+"\n")
	if m := readMeta(legacy); m.Subscription || m.Sensitive {
		t.Errorf("legacy task (no meta line) should report zero value, got %+v", m)
	}
	if m := readMeta(filepath.Join(dir, "missing.jsonl")); m.Subscription || m.Sensitive {
		t.Errorf("missing file should report zero value, got %+v", m)
	}
}

// A task recorded as subscription shows "subscription" regardless of current
// config — the label is now per-task, not display-time.
func TestSummarize_SubscriptionTaskShowsSubscription(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub.jsonl")
	body := `{"type":"drydock_meta","subscription":true}` + "\n" +
		`{"type":"result","subtype":"success","duration_ms":1000,"total_cost_usd":0.0237,"num_turns":3}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if got := summarize("sub", path, info); got.cost != "subscription" {
		t.Errorf("cost = %q, want subscription (per-task meta must drive the label)", got.cost)
	}
}
