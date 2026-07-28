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
	if got.outcome != "ok (3 turns)" {
		t.Errorf("outcome = %q, want %q (tail read must find the final result line)", got.outcome, "ok (3 turns)")
	}
	if got.cost != "$0.0500" {
		t.Errorf("cost = %q, want $0.0500", got.cost)
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

// A denied task's on-disk result row is the AGENT's own pre-gate success
// line (broker.pushAndOpenPR streams outcome=denied to the live client but
// never rewrites the audit log's result row), so before the metrics row
// gained an outcome field a denied task showed "ok (N turns)" here. The
// metrics row is the only place "denied" survives to a later `drydock tasks`
// read.
func TestSummarize_DeniedTaskUsesMetricsOutcomeNotResultRow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-denied.jsonl")
	body := `{"type":"drydock_meta","subscription":false}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":1000,"total_cost_usd":0.05,"num_turns":3}` + "\n" +
		`{"type":"metrics","src":"broker","task_id":"task-denied","outcome":"denied"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	got := summarize("task-denied", path, info)
	if got.outcome != "denied" {
		t.Errorf("outcome = %q, want %q", got.outcome, "denied")
	}
}

// A fail-closed diff-capture failure (V-01: an oversized or unreadable
// staged diff) also leaves the on-disk result row as the AGENT's own
// pre-failure success line: HandleTask's CaptureDiff error branch sets
// tr.outcome = "error" on the metrics row but, like the denied case, appends
// no broker result row. Before OutcomeKeyWithMetrics honored outcome=error
// against an "ok" result-row key, this task showed "ok (N turns)" here
// despite the broker recording the run as failed closed.
func TestSummarize_FailClosedDiffCaptureErrorUsesMetricsOutcomeNotResultRow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-diffcap-error.jsonl")
	body := `{"type":"drydock_meta","subscription":false}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":1000,"total_cost_usd":0.05,"num_turns":3}` + "\n" +
		`{"type":"metrics","src":"broker","task_id":"task-diffcap-error","outcome":"error"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	got := summarize("task-diffcap-error", path, info)
	if got.outcome != "error" {
		t.Errorf("outcome = %q, want %q", got.outcome, "error")
	}
}

// A pre-upgrade metrics row (no outcome field) must not change today's
// classification: "ok (N turns)" stays the fallback for a plain success row.
func TestSummarize_PreOutcomeFieldMetricsRowUnaffected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-old-metrics.jsonl")
	body := `{"type":"drydock_meta","subscription":false}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":1000,"total_cost_usd":0.05,"num_turns":3}` + "\n" +
		`{"type":"metrics","src":"broker","task_id":"task-old-metrics"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	got := summarize("task-old-metrics", path, info)
	if got.outcome != "ok (3 turns)" {
		t.Errorf("outcome = %q, want %q", got.outcome, "ok (3 turns)")
	}
}

// A brokerd crash leaves the boot reconciler's synthetic interrupted line.
// It must read as a distinct "interrupted" outcome (not "error"), and DUR
// must stay "-" (unknown) rather than the synthetic 0ms.
func TestSummarize_InterruptedShowsInterruptedAndUnknownDur(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-i.jsonl")
	body := `{"type":"drydock_meta","subscription":false}` + "\n" +
		`{"type":"result","subtype":"interrupted","is_error":true,"duration_ms":0,"total_cost_usd":0,"num_turns":0}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	got := summarize("task-i", path, info)
	if got.outcome != "interrupted" {
		t.Errorf("outcome = %q, want interrupted", got.outcome)
	}
	if got.dur != "-" {
		t.Errorf("dur = %q, want %q (unknown, not synthetic 0ms)", got.dur, "-")
	}
}
