package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"drydock/internal/broker"
)

// jsonUnmarshal keeps the ledger-line reader below to one line.
func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// These tests drive the global ceiling's BOOT RECONCILIATION (globalreconcile.go,
// plan G3). The property under test throughout is that the sweep converges the
// durable ledger toward the audit trail WITHOUT EVER UNDER-COUNTING and without
// ever double-counting on replay.

const recNow = int64(1_700_000_000_000)

func taskID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

// traceOpt mutates a trace under construction.
type trace struct {
	meta       string
	agent      string
	rows       []string
	mtime      time.Time
	endedAtMs  int64
	outcome    string
	noMetrics  bool
	metricsSrc string
}

// writeTrace lays down one <id>.jsonl audit trace shaped like a real one:
// drydock_meta, drydock_task, the agent's own rows, then the broker-authored
// terminal rows.
func writeTrace(t *testing.T, root, id string, tr trace) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	agent := tr.agent
	if agent == "" {
		agent = "claude"
	}
	meta := tr.meta
	if meta == "" {
		meta = `{"type":"drydock_meta","subscription":false,"sensitive":false}`
	}
	var sb strings.Builder
	sb.WriteString(meta + "\n")
	fmt.Fprintf(&sb, `{"type":"drydock_task","agent":%q}`+"\n", agent)
	for _, r := range tr.rows {
		sb.WriteString(r + "\n")
	}
	if !tr.noMetrics {
		src := tr.metricsSrc
		if src == "" {
			src = "broker"
		}
		fmt.Fprintf(&sb,
			`{"type":"metrics","src":%q,"task_id":%q,"agent":%q,"vendor":"anthropic","auth":"api_key","outcome":%q,"ended_at_ms":%d,"repo":"r","stage_ms":{"preparing":1,"running":1,"pushing":1},"egress_gate_wait_ms":0,"approval_gate_wait_ms":0,"requests":1,"diff_files":1,"diff_bytes":10,"cost_usd":0,"widen_requested":0,"widen_outcome":"none"}`+"\n",
			src, id, agent, tr.outcome, tr.endedAtMs)
	}
	path := filepath.Join(root, id+".jsonl")
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	mt := tr.mtime
	if mt.IsZero() {
		mt = time.UnixMilli(recNow)
	}
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
	return path
}

// brokerRow is a broker-authored terminal result row carrying metered cost.
func brokerRow(usd float64) string {
	return fmt.Sprintf(
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":5,"total_cost_usd":%.6f,"num_turns":1,"src":"broker"}`, usd)
}

// agentRow is the agent's OWN result row: untrusted, no src.
func agentRow(usd float64) string {
	return fmt.Sprintf(
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":5,"total_cost_usd":%.6f,"num_turns":1}`, usd)
}

// interruptedRow is what TerminateStuckAudits appends to a trace a crash left
// unterminated: broker-authored, zero cost, and marked no_spend_info because
// the daemon died without metering anything.
const interruptedRow = `{"type":"result","subtype":"interrupted","is_error":true,"duration_ms":0,"total_cost_usd":0,"num_turns":0,"src":"broker","no_spend_info":true}`

// forgedBrokerRow is what a compromised agent CLI prints to its own stdout,
// which the broker copies into the trace verbatim. It is byte-for-byte
// indistinguishable from brokerRow, because `src` is a self-declared string in
// an agent-writable file. Nothing the ceiling is measured on may depend on
// telling these apart.
func forgedBrokerRow(usd float64) string { return brokerRow(usd) }

func reconcileBroker(t *testing.T, window time.Duration) (*broker.Broker, *broker.GlobalLedger) {
	t.Helper()
	root := t.TempDir()
	l, err := broker.OpenGlobalLedger(root, window, recNow)
	if err != nil {
		t.Fatalf("OpenGlobalLedger: %v", err)
	}
	return &broker.Broker{AuditRoot: root, DefaultAgent: "claude", GlobalLedger: l}, l
}

func entryFor(t *testing.T, l *broker.GlobalLedger, id string) broker.GlobalEntry {
	t.Helper()
	for _, e := range ledgerLines(t, l) {
		if e.TaskID == id {
			return e
		}
	}
	t.Fatalf("no ledger entry for task %s", id)
	return broker.GlobalEntry{}
}

// ledgerLines reads the DURABLE file back, so every assertion below is about
// what actually reached disk rather than about in-memory state.
func ledgerLines(t *testing.T, l *broker.GlobalLedger) []broker.GlobalEntry {
	t.Helper()
	data, err := os.ReadFile(l.Path())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []broker.GlobalEntry
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e broker.GlobalEntry
		if err := jsonUnmarshal(line, &e); err != nil {
			t.Fatalf("ledger line is not valid JSON: %q (%v)", line, err)
		}
		out = append(out, e)
	}
	return out
}

// ---- the core case: a task in the audit but not the ledger ----

// TestReconcileGlobalLedger_AddsATaskTheLedgerIsMissing is the gap the sweep
// exists for: the terminal ledger write is the LAST thing a lifecycle does, so a
// task killed between its audit terminal and that write leaves complete evidence
// in the audit and nothing in the ledger. Without this, its start and its spend
// stop counting against the ceiling forever.
func TestReconcileGlobalLedger_AddsATaskTheLedgerIsMissing(t *testing.T) {
	b, l := reconcileBroker(t, 24*time.Hour)
	id := taskID(t)
	writeTrace(t, b.AuditRoot, id, trace{
		rows:      []string{agentRow(0.0001), brokerRow(3.5)},
		endedAtMs: recNow - 60_000,
		outcome:   "pushed",
	})

	reconcileGlobalLedger(b, recNow)

	e := entryFor(t, l, id)
	if e.Src != broker.GlobalSrcReconcile {
		t.Errorf("src = %q, want %q", e.Src, broker.GlobalSrcReconcile)
	}
	// THE START IS THE RECOVERY. The dollars are not: every figure in the trace
	// is in a file the agent's stdout is copied into, so "the broker-authored
	// 3.5" is a claim about a `src` string, not a measurement. See
	// globalreconcile.go's header.
	if e.USD != 0 || e.USDTrusted {
		t.Errorf("usd=%v trusted=%v, want 0 and false: no reconciled entry may carry a trusted dollar figure", e.USD, e.USDTrusted)
	}
	if !e.Metered {
		t.Errorf("metered = %v, want true: the lane resolves from the broker-written header lines", e.Metered)
	}
	// The timestamp is filesystem metadata, never the trace's own ended_at_ms.
	if e.EndedAtMs != recNow {
		t.Errorf("ended_at_ms = %d, want the file's mtime %d", e.EndedAtMs, recNow)
	}
	if e.Outcome != "unknown" {
		t.Errorf("outcome = %q, want unknown: the trace's outcome row is agent-writable", e.Outcome)
	}
	u := l.Usage(recNow)
	if u.Starts != 1 || u.USD != 0 {
		t.Errorf("usage: starts=%d usd=%v, want 1 and 0", u.Starts, u.USD)
	}
}

// TestReconcileGlobalLedger_NeverDoubleAdds: the sweep must be safe to run on
// every boot, forever. A ledger that already has the task is left exactly as it
// is, and a second sweep in the same process adds nothing.
func TestReconcileGlobalLedger_NeverDoubleAdds(t *testing.T) {
	b, l := reconcileBroker(t, 24*time.Hour)
	already := taskID(t)
	missing := taskID(t)
	// The one the terminal path already recorded, with ITS figure.
	if err := l.Record(recNow, broker.GlobalEntry{
		Kind: broker.GlobalEntryTask, TaskID: already, EndedAtMs: recNow - 1000,
		Vendor: "anthropic", Agent: "claude", Metered: true, USD: 1, USDTrusted: true,
		Outcome: "pushed", Src: broker.GlobalSrcTerminal,
	}); err != nil {
		t.Fatal(err)
	}
	// The audit says something different about it; the LEDGER IS AUTHORITATIVE.
	writeTrace(t, b.AuditRoot, already, trace{rows: []string{brokerRow(99)}, endedAtMs: recNow - 1000, outcome: "pushed"})
	writeTrace(t, b.AuditRoot, missing, trace{rows: []string{brokerRow(2)}, endedAtMs: recNow - 1000, outcome: "pushed"})

	for i := 0; i < 3; i++ {
		reconcileGlobalLedger(b, recNow)
	}

	u := l.Usage(recNow)
	if u.Starts != 2 {
		t.Fatalf("starts = %d after 3 sweeps over 2 tasks, want 2", u.Starts)
	}
	if u.USD != 1 {
		t.Errorf("usd = %v, want 1 (the terminal path's own figure, alone); the audit's 99 must neither overwrite a "+
			"recorded entry nor contribute a dollar of its own", u.USD)
	}
	if got := entryFor(t, l, already).Src; got != broker.GlobalSrcTerminal {
		t.Errorf("the existing entry's src became %q; reconciliation must not rewrite what the ledger already knows", got)
	}
	// And across a restart, which is how it will actually be replayed.
	reopened, err := broker.OpenGlobalLedger(b.AuditRoot, 24*time.Hour, recNow)
	if err != nil {
		t.Fatal(err)
	}
	b2 := &broker.Broker{AuditRoot: b.AuditRoot, DefaultAgent: "claude", GlobalLedger: reopened}
	reconcileGlobalLedger(b2, recNow)
	if u := reopened.Usage(recNow); u.Starts != 2 || u.USD != 1 {
		t.Errorf("after restart + sweep: starts=%d usd=%v, want 2 and 1", u.Starts, u.USD)
	}
}

// TestReconcileGlobalLedger_LedgerEntryWithNoAuditIsKept: the ledger is
// AUTHORITATIVE and the sweep only ever ADDS. A trace can be pruned, moved, or
// simply predate the retention an operator chose; removing a counted start
// because its evidence aged out would hand an operator a way to reset the
// ceiling by clearing the audit dir.
func TestReconcileGlobalLedger_LedgerEntryWithNoAuditIsKept(t *testing.T) {
	b, l := reconcileBroker(t, 24*time.Hour)
	orphan := taskID(t)
	if err := l.Record(recNow, broker.GlobalEntry{
		Kind: broker.GlobalEntryTask, TaskID: orphan, EndedAtMs: recNow - 5000,
		Vendor: "anthropic", Metered: true, USD: 7, USDTrusted: true, Src: broker.GlobalSrcTerminal,
	}); err != nil {
		t.Fatal(err)
	}
	reconcileGlobalLedger(b, recNow)
	u := l.Usage(recNow)
	if u.Starts != 1 || u.USD != 7 {
		t.Fatalf("starts=%d usd=%v, want 1 and 7: an entry with no audit trace must survive reconciliation", u.Starts, u.USD)
	}
}

// TestReconcileGlobalLedger_CrashedTaskCountsAStartWithUnknownSpend is the
// crash case, and it is where the honest answer costs something. The trace
// really does hold a broker-written figure — but a trace is an append-only file
// the agent's stdout flows into, so "broker-written" is not a property the
// reader can verify. The start is recovered exactly; the dollars are declared
// unknown rather than believed.
func TestReconcileGlobalLedger_CrashedTaskCountsAStartWithUnknownSpend(t *testing.T) {
	b, l := reconcileBroker(t, 24*time.Hour)
	id := taskID(t)
	writeTrace(t, b.AuditRoot, id, trace{
		rows:      []string{agentRow(0.02), brokerRow(4.25), interruptedRow},
		endedAtMs: recNow - 30_000,
		outcome:   "error",
	})
	reconcileGlobalLedger(b, recNow)

	e := entryFor(t, l, id)
	if e.USD != 0 || e.USDTrusted {
		t.Errorf("usd=%v trusted=%v, want 0 and false", e.USD, e.USDTrusted)
	}
	if l.Usage(recNow).Starts != 1 {
		t.Error("the crashed task was not counted as a start")
	}
	// And the USD limb is not degraded by it: the start limb is the backstop
	// (G7), and degrading on every crash-recovered task would refuse every start
	// for the daemon's life after an ordinary crash.
	if l.LoadError() != "" {
		t.Errorf("a reconciled entry degraded the ledger: %s", l.LoadError())
	}
}

// TestReconcileGlobalLedger_NoBrokerRowIsAStartWithUnknownSpend: a genuine
// mid-run kill leaves no broker-authored row at all. The START still counts —
// that is the limb that cannot be under-reported by a metering gap (G7) — and
// the dollars are declared unknown rather than invented.
func TestReconcileGlobalLedger_NoBrokerRowIsAStartWithUnknownSpend(t *testing.T) {
	b, l := reconcileBroker(t, 24*time.Hour)
	id := taskID(t)
	writeTrace(t, b.AuditRoot, id, trace{
		rows:      []string{agentRow(50), interruptedRow},
		endedAtMs: recNow - 1000,
		outcome:   "",
		noMetrics: true,
	})
	reconcileGlobalLedger(b, recNow)

	e := entryFor(t, l, id)
	if e.USD != 0 || e.USDTrusted {
		t.Errorf("usd=%v trusted=%v, want 0 and false: with no broker row the agent's 50 must never be believed (G4)",
			e.USD, e.USDTrusted)
	}
	u := l.Usage(recNow)
	if u.Starts != 1 {
		t.Errorf("starts = %d, want 1: a task with unknowable spend is still a task start", u.Starts)
	}
	if u.USD != 0 {
		t.Errorf("usd limb = %v, want 0: an untrusted figure is never summed", u.USD)
	}
}

// ---- timestamps ----

// TestReconcileGlobalLedger_TimestampIsFilesystemMetadataNotTraceContent is the
// inversion of what this file used to assert, and the inversion is the fix.
//
// Preferring the trace's own `ended_at_ms` looked like an improvement over
// seedAggregateFromAudit's mtime — a broker-authored instant beats a file
// timestamp — right up until you notice the row carrying it is in an
// agent-writable file. It gave an agent a dial on WHERE in the rolling window
// its entry landed, in both directions. mtime is a worse measurement of when a
// task ran and a far better one of what the ceiling can rely on: nothing in a
// sandbox can set it.
func TestReconcileGlobalLedger_TimestampIsFilesystemMetadataNotTraceContent(t *testing.T) {
	b, l := reconcileBroker(t, time.Hour)

	// A trace whose metrics row claims the task ran two days ago while the file
	// itself was written a second ago. The claim is ignored.
	claimsOld := taskID(t)
	writeTrace(t, b.AuditRoot, claimsOld, trace{
		rows:      []string{brokerRow(100)},
		endedAtMs: recNow - (48 * time.Hour).Milliseconds(),
		mtime:     time.UnixMilli(recNow),
		outcome:   "pushed",
	})
	fresh := taskID(t)
	writeTrace(t, b.AuditRoot, fresh, trace{
		rows:      []string{brokerRow(2)},
		endedAtMs: recNow - 60_000,
		mtime:     time.UnixMilli(recNow),
		outcome:   "pushed",
	})

	reconcileGlobalLedger(b, recNow)

	u := l.Usage(recNow)
	if u.Starts != 2 {
		t.Fatalf("starts=%d, want 2: an ended_at_ms claiming to be outside the window must not hide a start", u.Starts)
	}
	if u.USD != 0 {
		t.Errorf("usd = %v, want 0: no reconciled entry carries a trusted figure", u.USD)
	}
	for _, id := range []string{claimsOld, fresh} {
		if e := entryFor(t, l, id); e.EndedAtMs != recNow {
			t.Errorf("ended_at_ms = %d, want the file's mtime %d", e.EndedAtMs, recNow)
		}
	}
}

// TestReconcileGlobalLedger_MTimeIsTheTimestampForAPreUpgradeTrace: a trace
// with no metrics row at all is now no different from any other — mtime was
// always going to be the answer.
func TestReconcileGlobalLedger_MTimeIsTheTimestampForAPreUpgradeTrace(t *testing.T) {
	b, l := reconcileBroker(t, 24*time.Hour)
	id := taskID(t)
	writeTrace(t, b.AuditRoot, id, trace{
		rows:      []string{brokerRow(1.5)},
		noMetrics: true, // a pre-metrics-row trace
		mtime:     time.UnixMilli(recNow - 120_000),
	})
	reconcileGlobalLedger(b, recNow)
	e := entryFor(t, l, id)
	if e.EndedAtMs != recNow-120_000 {
		t.Errorf("ended_at_ms = %d, want the mtime %d", e.EndedAtMs, recNow-120_000)
	}
	if e.USD != 0 || e.USDTrusted {
		t.Errorf("usd=%v trusted=%v, want 0 and false", e.USD, e.USDTrusted)
	}
}

// ---- clock movement ----

// TestReconcileGlobalLedger_ForwardClockJumpStillReconciles: the failure mode
// this guards is silent. A forward clock jump (NTP correction, misconfigured
// host) would put the whole lookback window in the future, the sweep would find
// nothing, and a crash-lost terminal would be gone for good — an UNDER-count,
// the one direction G3 forbids. The anchor is the earlier of now and the
// ledger's own newest event, so it cannot happen.
func TestReconcileGlobalLedger_ForwardClockJumpStillReconciles(t *testing.T) {
	b, l := reconcileBroker(t, time.Hour)
	// The ledger's own history says "now" is around recNow.
	anchorTask := taskID(t)
	if err := l.Record(recNow, broker.GlobalEntry{
		Kind: broker.GlobalEntryTask, TaskID: anchorTask, EndedAtMs: recNow,
		Vendor: "anthropic", Metered: true, USDTrusted: true, Src: broker.GlobalSrcTerminal,
	}); err != nil {
		t.Fatal(err)
	}
	missed := taskID(t)
	writeTrace(t, b.AuditRoot, missed, trace{
		rows: []string{brokerRow(5)}, endedAtMs: recNow - 60_000,
		mtime: time.UnixMilli(recNow), outcome: "pushed",
	})

	// The host's clock is now a year ahead of everything the ledger has seen.
	jumped := recNow + (365 * 24 * time.Hour).Milliseconds()
	reconcileGlobalLedger(b, jumped)

	found := false
	for _, e := range ledgerLines(t, l) {
		if e.TaskID == missed {
			found = true
		}
	}
	if !found {
		t.Error("a forward clock jump made the sweep skip a task it should have reconciled; " +
			"the lookback anchor must fall back to the ledger's own newest event")
	}
}

// TestReconcileGlobalLedger_BackwardClockJumpOverCounts: the safe direction
// needs no defence, and this pins that it stays the safe direction.
func TestReconcileGlobalLedger_BackwardClockJumpOverCounts(t *testing.T) {
	b, l := reconcileBroker(t, time.Hour)
	id := taskID(t)
	writeTrace(t, b.AuditRoot, id, trace{
		rows: []string{brokerRow(1)}, endedAtMs: recNow, mtime: time.UnixMilli(recNow), outcome: "pushed",
	})
	// The clock went backwards by a day: the floor moves back too, so MORE is
	// reconciled, never less.
	reconcileGlobalLedger(b, recNow-(24*time.Hour).Milliseconds())
	if len(ledgerLines(t, l)) != 1 {
		t.Errorf("ledger holds %d entries, want 1 after a backwards clock jump", len(ledgerLines(t, l)))
	}
}

// ---- the total-mode lookback bound ----

// TestReconcileGlobalLedger_TotalModeLookbackIsBounded is Task 1's fold-horizon
// concern. Total mode folds all but the newest entries into a checkpoint that
// preserves their sums and DESTROYS their ids, so Has answers false for them; a
// sweep that walked the whole audit history would re-add tasks already inside
// the checkpoint on EVERY boot — an over-count that repeats and grows without
// bound. The floor is therefore clamped to the ledger's oldest ADDRESSABLE entry.
func TestReconcileGlobalLedger_TotalModeLookbackIsBounded(t *testing.T) {
	b, l := reconcileBroker(t, 0) // total mode: no decay
	// The ledger's oldest addressable entry. In a real total-mode ledger this
	// is the checkpoint, positioned at the newest event it folded.
	horizon := recNow - (2 * time.Hour).Milliseconds()
	if err := l.Record(recNow, broker.GlobalEntry{
		Kind: broker.GlobalEntryTask, TaskID: taskID(t), EndedAtMs: horizon,
		Vendor: "anthropic", Metered: true, USD: 10, USDTrusted: true, Src: broker.GlobalSrcTerminal,
	}); err != nil {
		t.Fatal(err)
	}
	// One task from BEFORE the horizon: it is either already inside the folded
	// sums or predates the ledger, and replaying it would double-count forever.
	old := taskID(t)
	writeTrace(t, b.AuditRoot, old, trace{
		rows: []string{brokerRow(100)}, endedAtMs: horizon - 60_000,
		mtime: time.UnixMilli(horizon - 60_000), outcome: "pushed",
	})
	// One from after it: genuinely recoverable.
	recent := taskID(t)
	writeTrace(t, b.AuditRoot, recent, trace{
		rows: []string{brokerRow(1)}, endedAtMs: horizon + 60_000,
		mtime: time.UnixMilli(horizon + 60_000), outcome: "pushed",
	})

	reconcileGlobalLedger(b, recNow)

	u := l.Usage(recNow)
	if u.Starts != 2 {
		t.Fatalf("starts = %d, want 2 (the seeded entry + the recoverable one); anything older than the ledger's "+
			"oldest addressable entry must not be replayed in total mode", u.Starts)
	}
	if u.USD != 10 {
		t.Errorf("usd = %v, want 10 (the seeded entry alone; a reconciled entry contributes no dollars)", u.USD)
	}
	for _, e := range ledgerLines(t, l) {
		if e.TaskID == old {
			t.Error("a task older than the total-mode fold horizon was replayed; on a folded ledger that double-counts on every boot")
		}
	}
}

// TestReconcileFloorMs pins the floor arithmetic directly, including the
// windowed case and the empty-ledger total-mode case where only the fixed
// lookback bounds the walk.
func TestReconcileFloorMs(t *testing.T) {
	t.Run("windowed stops at the window", func(t *testing.T) {
		_, l := reconcileBroker(t, 6*time.Hour)
		if got, want := reconcileFloorMs(l, recNow), recNow-(6*time.Hour).Milliseconds(); got != want {
			t.Errorf("floor = %d, want %d", got, want)
		}
	})
	t.Run("total mode on an empty ledger uses the fixed lookback", func(t *testing.T) {
		_, l := reconcileBroker(t, 0)
		if got, want := reconcileFloorMs(l, recNow), recNow-globalReconcileTotalLookback.Milliseconds(); got != want {
			t.Errorf("floor = %d, want %d (a fresh total-mode ledger has no fold horizon to clamp to)", got, want)
		}
	})
}

// ---- read failures must not fail open ----

// TestReconcileGlobalLedger_UnreadableAuditDirDegradesRatherThanFailsOpen:
// seedAggregateFromAudit returns silently on a ReadDir error, which seeds the
// cap from nothing and ADMITS freely. For a ceiling that is backwards (G2): a
// sweep that cannot enumerate the evidence does not know how many starts OR how
// many dollars it is missing, so it degrades the ledger, which globalcap.go
// turns into a refusal on every enforced limb.
func TestReconcileGlobalLedger_UnreadableAuditDirDegradesRatherThanFailsOpen(t *testing.T) {
	b, l := reconcileBroker(t, 24*time.Hour)
	if l.LoadError() != "" {
		t.Fatalf("the ledger started degraded: %s", l.LoadError())
	}
	// Point the broker at a path that is a FILE, so ReadDir fails with
	// something other than not-exist (an absent audit root is a legitimate
	// never-run install, and must stay benign).
	notADir := filepath.Join(t.TempDir(), "audit")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.AuditRoot = notADir

	reconcileGlobalLedger(b, recNow)

	le := l.LoadError()
	if le == "" {
		t.Fatal("the ledger was not degraded after the audit directory could not be read; the ceiling would admit freely")
	}
	// PATH-FREE: this reason is rendered into a 402 body for whoever submitted
	// the task. The operator gets the path from the log line instead.
	if strings.Contains(le, notADir) {
		t.Errorf("degrade reason leaks the host's audit path into a client-facing 402 body: %q", le)
	}
	if u := l.Usage(recNow); !u.Degraded {
		t.Error("Usage does not report Degraded after the reconcile failed to read the audit")
	}
}

// TestReconcileGlobalLedger_MissingAuditDirIsBenign: an install that has never
// run a task has no audit root. That is not a failure and must not brick
// admission.
func TestReconcileGlobalLedger_MissingAuditDirIsBenign(t *testing.T) {
	b, l := reconcileBroker(t, 24*time.Hour)
	b.AuditRoot = filepath.Join(t.TempDir(), "never-created")
	reconcileGlobalLedger(b, recNow)
	if le := l.LoadError(); le != "" {
		t.Errorf("a never-created audit root degraded the ledger: %s", le)
	}
}

// TestReconcileGlobalLedger_UnreadableTraceCountsAStartWithoutDegrading: the
// per-trace version. The file's existence under a valid task id is itself
// evidence a task ran, so the START is recorded (never under-count the limb
// that bounds subscription mode) while the dollars are declared unknown.
//
// It does NOT degrade, and that is a deliberate change. Reading a trace now
// contributes nothing to either limb — the start comes from the file's
// existence and the dollars are unknown either way — so one unreadable trace
// leaves the ceiling in exactly the state a readable one does. Degrading would
// refuse every task start for the daemon's life over a permissions glitch on a
// single file.
func TestReconcileGlobalLedger_UnreadableTraceCountsAStartWithoutDegrading(t *testing.T) {
	b, l := reconcileBroker(t, 24*time.Hour)
	id := taskID(t)
	// A symlinked trace: every audit read is O_NOFOLLOW, so this is unreadable
	// by construction — and it is the exact shape an attacker would plant.
	target := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	if err := os.WriteFile(target, []byte(brokerRow(999)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(b.AuditRoot, id+".jsonl")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	reconcileGlobalLedger(b, recNow)

	e := entryFor(t, l, id)
	if e.USDTrusted || e.USD != 0 {
		t.Errorf("usd=%v trusted=%v, want 0 and false: an unreadable trace tells us nothing about dollars", e.USD, e.USDTrusted)
	}
	if u := l.Usage(recNow); u.Starts != 1 {
		t.Errorf("starts = %d, want 1: an unreadable trace is still evidence that a task ran", u.Starts)
	}
	if l.LoadError() != "" {
		t.Errorf("one unreadable trace degraded the ledger for the daemon's life: %s", l.LoadError())
	}
}

// ---- lanes, filtering, and durability ----

// TestReconcileGlobalLedger_SubscriptionTraceIsRecordedUnmetered: a
// subscription run meters at $0 by construction, and the trace records that
// fact itself — so a config change since the run cannot make its zeroes look
// like measured spend.
func TestReconcileGlobalLedger_SubscriptionTraceIsRecordedUnmetered(t *testing.T) {
	b, l := reconcileBroker(t, 24*time.Hour)
	id := taskID(t)
	writeTrace(t, b.AuditRoot, id, trace{
		meta:      `{"type":"drydock_meta","subscription":true,"sensitive":false}`,
		rows:      []string{brokerRow(0)},
		endedAtMs: recNow - 1000,
		outcome:   "pushed",
	})
	reconcileGlobalLedger(b, recNow)
	e := entryFor(t, l, id)
	if e.Metered || e.USDTrusted {
		t.Errorf("metered=%v trusted=%v, want both false on a subscription trace", e.Metered, e.USDTrusted)
	}
	if e.Auth != "subscription" {
		t.Errorf("auth = %q, want subscription", e.Auth)
	}
	if u := l.Usage(recNow); u.Starts != 1 {
		t.Errorf("starts = %d, want 1: the task limb is what bounds subscription mode (G1)", u.Starts)
	}
}

// TestReconcileGlobalLedger_IgnoresWhatIsNotATaskTrace: the audit root holds
// .diff/.brief.json/.queue.json/.ci.json files and the ledger's own `global/`
// subdirectory. Only <32-hex>.jsonl is a task trace, and a name is never
// trusted before it is matched.
func TestReconcileGlobalLedger_IgnoresWhatIsNotATaskTrace(t *testing.T) {
	b, l := reconcileBroker(t, 24*time.Hour)
	real := taskID(t)
	writeTrace(t, b.AuditRoot, real, trace{rows: []string{brokerRow(1)}, endedAtMs: recNow - 1000, outcome: "pushed"})
	for _, name := range []string{
		"notanid.jsonl", "../escape.jsonl", strings.Repeat("z", 32) + ".jsonl",
		real + ".diff", real + ".queue.json", "README.md",
	} {
		p := filepath.Join(b.AuditRoot, filepath.Base(name))
		if err := os.WriteFile(p, []byte(brokerRow(500)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reconcileGlobalLedger(b, recNow)
	if u := l.Usage(recNow); u.Starts != 1 || u.USD != 0 {
		t.Errorf("starts=%d usd=%v, want 1 and 0: only <32-hex>.jsonl is a task trace", u.Starts, u.USD)
	}
	// The ledger's own subdirectory must never be mistaken for one either.
	if l.LoadError() != "" {
		t.Errorf("the sweep degraded the ledger over its own storage directory: %s", l.LoadError())
	}
}

// TestReconcileGlobalLedger_BulkRecoveryIsDurable exercises the batched write
// (one fsync for the whole sweep rather than one per entry) and proves it is
// really durable, not just in memory.
func TestReconcileGlobalLedger_BulkRecoveryIsDurable(t *testing.T) {
	b, l := reconcileBroker(t, 24*time.Hour)
	const n = 200
	for i := 0; i < n; i++ {
		writeTrace(t, b.AuditRoot, taskID(t), trace{
			rows: []string{brokerRow(0.5)}, endedAtMs: recNow - int64(i) - 1, outcome: "pushed",
		})
	}
	reconcileGlobalLedger(b, recNow)
	if u := l.Usage(recNow); u.Starts != n {
		t.Fatalf("starts = %d, want %d", u.Starts, n)
	}
	reopened, err := broker.OpenGlobalLedger(b.AuditRoot, 24*time.Hour, recNow)
	if err != nil {
		t.Fatal(err)
	}
	u := reopened.Usage(recNow)
	if u.Starts != n {
		t.Errorf("after restart: starts = %d, want %d — the batched write must be as durable as %d Records", u.Starts, n, n)
	}
	if u.Damaged != 0 {
		t.Errorf("%d damaged entries after reopening a batch-written ledger, want 0", u.Damaged)
	}
	if u.USD != 0 {
		t.Errorf("usd = %v, want 0: a reconciled entry never carries a trusted figure", u.USD)
	}
}

// TestReconcileGlobalLedger_NoLedgerIsANoOp: the stock install. With the
// ceiling unconfigured the sweep reads nothing and writes nothing.
func TestReconcileGlobalLedger_NoLedgerIsANoOp(t *testing.T) {
	root := t.TempDir()
	writeTrace(t, root, taskID(t), trace{rows: []string{brokerRow(3)}, endedAtMs: recNow, outcome: "pushed"})
	b := &broker.Broker{AuditRoot: root, DefaultAgent: "claude"}
	reconcileGlobalLedger(b, recNow) // must not panic
	if _, err := os.Stat(broker.GlobalLedgerPath(root)); !os.IsNotExist(err) {
		t.Errorf("a ledger file appeared with no ledger configured (err=%v)", err)
	}
}

// ---------------------------------------------------------------------------
// THE INVARIANT: NO AGENT-WRITABLE BYTE MAY RAISE OR LOWER EITHER LIMB.
//
// Everything below is one property stated four ways. A task trace is an
// append-only file the agent's own stdout is copied into (broker.go's
// io.MultiWriter), so `type`, `src`, `total_cost_usd` and `ended_at_ms` are all
// attacker-supplied text. The ceiling may read a trace's EXISTENCE — the broker
// created it — and nothing else that any limb is measured on.
// ---------------------------------------------------------------------------

// TestReconcile_ForgedBrokerRowCannotRaiseTheUSDLimb is the inflating half. An
// agent prints one src:"broker" result line at the START of its run and then
// arranges to be killed; brokerd restarts. That row used to become the ledger's
// TRUSTED spend for the task, and a single task then refused every task start on
// the install — all vendors, both auth modes — until it aged out. In total mode,
// forever.
func TestReconcile_ForgedBrokerRowCannotRaiseTheUSDLimb(t *testing.T) {
	b, l := reconcileBroker(t, 24*time.Hour)
	id := taskID(t)
	writeTrace(t, b.AuditRoot, id, trace{
		// The forgery is FIRST, exactly as an agent that prints at startup and
		// then dies would leave it, and there is no honest broker row at all.
		rows:      []string{forgedBrokerRow(999999)},
		noMetrics: true,
		mtime:     time.UnixMilli(recNow - 1000),
	})
	reconcileGlobalLedger(b, recNow)

	e := entryFor(t, l, id)
	if e.USD != 0 || e.USDTrusted {
		t.Fatalf("usd=%v trusted=%v: a forged src:\"broker\" row entered the ledger's trusted USD limb", e.USD, e.USDTrusted)
	}
	u := l.Usage(recNow)
	if u.USD != 0 {
		t.Errorf("USD limb = %v, want 0", u.USD)
	}
	if u.Starts != 1 {
		t.Errorf("starts = %d, want 1: the forgery must not cost us the start either", u.Starts)
	}
	// And the practical consequence, which is the whole reason this matters: a
	// $10 budget is not exhausted by one task that claimed to spend $999,999.
	if u.USD >= 10 {
		t.Errorf("one forged row exhausted a $10 global budget: usd=%v", u.USD)
	}
}

// TestReconcile_ForgedBrokerRowCannotLowerTheUSDLimb is the deflating half:
// a $0 forgery must not be recorded as a TRUSTED zero, which would let an agent
// launder a real spend into "measured nothing".
func TestReconcile_ForgedBrokerRowCannotLowerTheUSDLimb(t *testing.T) {
	b, l := reconcileBroker(t, 24*time.Hour)
	id := taskID(t)
	writeTrace(t, b.AuditRoot, id, trace{
		rows:      []string{forgedBrokerRow(0)},
		noMetrics: true,
		mtime:     time.UnixMilli(recNow - 1000),
	})
	reconcileGlobalLedger(b, recNow)
	if e := entryFor(t, l, id); e.USDTrusted {
		t.Error("a forged $0 row was recorded as a TRUSTED zero; an unmeasured task must read as unknown, not as measured-nothing")
	}
}

// TestReconcile_FutureDatedTraceCannotParkSpendInTheWindow is C3's inflating
// dial. A metrics row dated a year ahead used to place the entry a year into
// the future, where a 24h rolling window can never age it out.
func TestReconcile_FutureDatedTraceCannotParkSpendInTheWindow(t *testing.T) {
	b, l := reconcileBroker(t, 24*time.Hour)
	id := taskID(t)
	aYearOn := recNow + (365 * 24 * time.Hour).Milliseconds()
	writeTrace(t, b.AuditRoot, id, trace{
		rows:      []string{forgedBrokerRow(500000)},
		endedAtMs: aYearOn,
		mtime:     time.UnixMilli(recNow - 1000),
		outcome:   "pushed",
	})
	reconcileGlobalLedger(b, recNow)

	e := entryFor(t, l, id)
	if e.EndedAtMs > recNow {
		t.Fatalf("ended_at_ms = %d, which is after now (%d): an agent-supplied timestamp placed an entry in the future",
			e.EndedAtMs, recNow)
	}
	if u := l.Usage(recNow); u.USD != 0 {
		t.Errorf("USD limb = %v, want 0", u.USD)
	}
	// It ages out on schedule, like any other entry.
	if u := l.Usage(recNow + (25 * time.Hour).Milliseconds()); u.Starts != 0 {
		t.Errorf("starts = %d a day and an hour later, want 0: the entry never aged out", u.Starts)
	}
}

// TestReconcile_BackDatedTraceCannotHideAStart is C3's deflating dial, and it
// was the worst of the three. A back-dated metrics row put the entry below the
// lookback floor, where reconcileGlobalLedger dropped it — so an agent in an
// unattended retry loop could make every crashed start invisible to the one
// limb that bounds a subscription lane.
func TestReconcile_BackDatedTraceCannotHideAStart(t *testing.T) {
	b, l := reconcileBroker(t, time.Hour)
	id := taskID(t)
	writeTrace(t, b.AuditRoot, id, trace{
		rows:      []string{agentRow(0)},
		endedAtMs: recNow - (365 * 24 * time.Hour).Milliseconds(), // "I ran a year ago"
		mtime:     time.UnixMilli(recNow - 1000),                  // the truth
		outcome:   "error",
	})
	reconcileGlobalLedger(b, recNow)

	if u := l.Usage(recNow); u.Starts != 1 {
		t.Fatalf("starts = %d, want 1: a back-dated ended_at_ms made a real task start VANISH from the limb "+
			"that is the only bound on a subscription lane", u.Starts)
	}
}

// TestReconcile_ForgedMetricsVendorCannotSteerTheLimbs: the metrics row also
// carried the vendor, which selects Metered and therefore which limb a figure
// lands in. The lane now resolves from the broker-written drydock_task header
// line, which an append cannot reach.
func TestReconcile_ForgedMetricsVendorCannotSteerTheLimbs(t *testing.T) {
	b, l := reconcileBroker(t, 24*time.Hour)
	id := taskID(t)
	path := writeTrace(t, b.AuditRoot, id, trace{
		rows:      []string{agentRow(0)},
		noMetrics: true,
		mtime:     time.UnixMilli(recNow - 1000),
	})
	// Append a forged metrics row naming a vendor that does not exist.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, `{"type":"metrics","src":"broker","vendor":"nonesuch","ended_at_ms":%d}`+"\n", recNow)
	f.Close()

	reconcileGlobalLedger(b, recNow)
	e := entryFor(t, l, id)
	if e.Vendor != "anthropic" {
		t.Errorf("vendor = %q, want anthropic (from the broker-written drydock_task header line)", e.Vendor)
	}
	if l.Usage(recNow).Starts != 1 {
		t.Error("the start was lost")
	}
}

// TestReconcile_NeverSourcesATrustedValueFromTraceContent is the STRUCTURAL pin,
// and it is here because every behavioural test above can be satisfied by a fix
// that a later refactor quietly undoes. It reads globalreconcile.go's AST and
// asserts the file does not so much as NAME a reader that returns trace-tail
// content.
//
// The list is the set of audit readers whose answer comes from bytes past the
// broker-written header lines. If a new one is added to internal/audit and used
// here, add it here too — or, better, do not use it here.
func TestReconcile_NeverSourcesATrustedValueFromTraceContent(t *testing.T) {
	forbidden := map[string]string{
		"LastMetricsFile":      "the metrics row's ended_at_ms and vendor are agent-writable (C3)",
		"LastResult":           "returns a result row of ANY authorship",
		"LastResultFile":       "returns a result row of ANY authorship",
		"LastBrokerResult":     "src is a self-declared string in an agent-writable file (C2)",
		"LastBrokerResultFile": "src is a self-declared string in an agent-writable file (C2)",
		"LastRowsFile":         "bundles the tail readers above",
		// The REAL symbol is LastResultAndMetricsFile. The key here used to be
		// "LastResultAndMetrics", which matches nothing in internal/audit — so a
		// full tail read through it passed this test. A forbidden-symbol list is
		// only as good as its spelling; every key here is a name that actually
		// exists (or, for TotalCost, one that deliberately does not).
		"LastResultAndMetricsFile": "bundles the tail readers above",
		"LastResultAndMetrics":     "kept so a future rename to the shorter form is caught too",
		"TotalCost":                "deliberately absent from internal/audit; never reintroduce it here",
		"HasBrokerResultLine":      "a tail scan; the sweep must not branch on trace content at all",
		"scanTailForResult":        "a tail scan",
		"AllowedSrcCostFile":       "any future tail reader belongs on this list",
	}
	// VACUITY GUARD ON THE LIST ITSELF. Every key that names a real audit reader
	// must exist in internal/audit, or it is a key that can never match — which is
	// exactly how LastResultAndMetricsFile went unguarded.
	assertAuditSymbolsExist(t, forbidden, map[string]bool{
		"TotalCost":            true, // deliberately deleted; see the tombstone test
		"LastResultAndMetrics": true, // the shorter spelling, guarded pre-emptively
		"AllowedSrcCostFile":   true, // a placeholder for future readers
	})
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "globalreconcile.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		why, bad := forbidden[sel.Sel.Name]
		if !bad {
			return true
		}
		pos := fset.Position(sel.Pos())
		t.Errorf("globalreconcile.go:%d calls %s: %s.\n"+
			"Boot reconciliation may read a trace's EXISTENCE and its broker-written header lines, "+
			"never its tail. See this file's header.", pos.Line, sel.Sel.Name, why)
		return true
	})

	// The other half of the same rule, stated over the values rather than the
	// readers: nothing in this file may set USDTrusted to true.
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := as.Key.(*ast.Ident)
		if !ok || key.Name != "USDTrusted" {
			return true
		}
		if lit, ok := as.Value.(*ast.Ident); !ok || lit.Name != "false" {
			pos := fset.Position(as.Pos())
			t.Errorf("globalreconcile.go:%d sets USDTrusted to something other than the literal false; "+
				"a reconciled entry's dollars are unknown by construction", pos.Line)
		}
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "USDTrusted" {
				continue
			}
			if id, ok := as.Rhs[i].(*ast.Ident); !ok || id.Name != "false" {
				pos := fset.Position(as.Pos())
				t.Errorf("globalreconcile.go:%d assigns a non-literal-false to USDTrusted", pos.Line)
			}
		}
		return true
	})

	// THE THIRD HALF, and C3's own vector: the TIMESTAMP. USDTrusted was pinned
	// and EndedAtMs was not, so reintroducing `e.EndedAtMs = m.EndedAtMs` — the
	// exact line C3 was about — passed this test. An ended_at_ms read out of the
	// trace controls WHERE in the rolling window an entry lands: future-dated it
	// parks spend for a year, back-dated it drops the entry below the lookback
	// floor and THE START VANISHES.
	//
	// The only permitted source is reconcileEventMs, which takes filesystem mtime
	// and clamps it at both ends.
	endedAtSources := 0
	checkEndedAt := func(pos token.Pos, value ast.Expr) {
		endatSrc := ""
		if call, ok := value.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok {
				endatSrc = id.Name
			}
		}
		endedAtSources++
		if endatSrc != "reconcileEventMs" {
			p := fset.Position(pos)
			t.Errorf("globalreconcile.go:%d sets EndedAtMs from something other than reconcileEventMs(...); "+
				"a reconciled entry's timestamp comes from filesystem metadata, never from trace content (C3). "+
				"Future-dating parks spend in the window for a year; back-dating makes a real start vanish.", p.Line)
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.KeyValueExpr:
			if key, ok := v.Key.(*ast.Ident); ok && key.Name == "EndedAtMs" {
				checkEndedAt(v.Pos(), v.Value)
			}
		case *ast.AssignStmt:
			for i, lhs := range v.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "EndedAtMs" && i < len(v.Rhs) {
					checkEndedAt(v.Pos(), v.Rhs[i])
				}
			}
		}
		return true
	})
	if endedAtSources == 0 {
		t.Fatal("found no EndedAtMs assignment in globalreconcile.go; the timestamp half of the pin is vacuous")
	}
}

// assertAuditSymbolsExist fails if a key in a forbidden-symbol map names nothing
// in internal/audit. A misspelled key matches no AST node and silently disables
// that entry — which is how a full tail read through LastResultAndMetricsFile
// passed a test whose list said "LastResultAndMetrics".
//
// exempt names entries that are deliberately not real symbols (a deleted
// function whose return is the point, or a pre-emptive spelling).
func assertAuditSymbolsExist(t *testing.T, forbidden map[string]string, exempt map[string]bool) {
	t.Helper()
	fset := token.NewFileSet()
	names, err := filepath.Glob("../../internal/audit/*.go")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	parsed := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++
		for _, decl := range af.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				have[fn.Name.Name] = true
			}
		}
	}
	if parsed == 0 {
		t.Fatal("parsed no files from internal/audit; the spelling guard would be vacuous")
	}
	for name := range forbidden {
		if exempt[name] || have[name] {
			continue
		}
		t.Errorf("the forbidden-symbol list names %q, which does not exist in internal/audit. "+
			"A key that matches nothing disables its own entry — check the spelling against the real "+
			"function name, or add it to the exempt set with a reason.", name)
	}
}
