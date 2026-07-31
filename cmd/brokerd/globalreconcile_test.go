package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
// unterminated: no src, zero cost.
const interruptedRow = `{"type":"result","subtype":"interrupted","is_error":true,"duration_ms":0,"total_cost_usd":0,"num_turns":0}`

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
	if e.USD != 3.5 {
		t.Errorf("usd = %v, want the BROKER-authored 3.5 (never the agent's 0.0001) — plan G4", e.USD)
	}
	if !e.USDTrusted || !e.Metered {
		t.Errorf("metered=%v usd_trusted=%v, want both true", e.Metered, e.USDTrusted)
	}
	if e.EndedAtMs != recNow-60_000 {
		t.Errorf("ended_at_ms = %d, want the metrics row's broker-authored %d", e.EndedAtMs, recNow-60_000)
	}
	if e.Outcome != "pushed" {
		t.Errorf("outcome = %q, want pushed", e.Outcome)
	}
	u := l.Usage(recNow)
	if u.Starts != 1 || u.USD != 3.5 {
		t.Errorf("usage: starts=%d usd=%v, want 1 and 3.5", u.Starts, u.USD)
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
	if u.USD != 3 {
		t.Errorf("usd = %v, want 3 (1 from the terminal path + 2 reconciled); the audit's 99 must not overwrite a recorded entry", u.USD)
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
	if u := reopened.Usage(recNow); u.Starts != 2 || u.USD != 3 {
		t.Errorf("after restart + sweep: starts=%d usd=%v, want 2 and 3", u.Starts, u.USD)
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

// TestReconcileGlobalLedger_CrashedTaskKeepsItsMeteredSpend is the
// seedAggregateFromAudit weakness, fixed. A task the daemon died under gets a
// synthetic `interrupted` row appended by TerminateStuckAudits — no src, zero
// cost — which lands AFTER the broker's own metered row. A "last result row must
// be src==broker" check therefore sees the interrupted row, rejects it, and the
// task's real spend becomes invisible. The sweep scans back to the last broker
// row instead, so the crash case is exactly the case it reads correctly.
func TestReconcileGlobalLedger_CrashedTaskKeepsItsMeteredSpend(t *testing.T) {
	b, l := reconcileBroker(t, 24*time.Hour)
	id := taskID(t)
	writeTrace(t, b.AuditRoot, id, trace{
		rows:      []string{agentRow(0.02), brokerRow(4.25), interruptedRow},
		endedAtMs: recNow - 30_000,
		outcome:   "error",
	})
	reconcileGlobalLedger(b, recNow)

	e := entryFor(t, l, id)
	if e.USD != 4.25 || !e.USDTrusted {
		t.Errorf("usd=%v trusted=%v, want 4.25 and true: the interrupted row must not hide the broker-metered spend behind it",
			e.USD, e.USDTrusted)
	}
	if l.Usage(recNow).Starts != 1 {
		t.Error("the crashed task was not counted as a start")
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

// TestReconcileGlobalLedger_UsesTheBrokerAuthoredTimestampNotMtime pins the
// improvement over seedAggregateFromAudit, which keys on mtime for BOTH its
// cutoff and its entry timestamp.
func TestReconcileGlobalLedger_UsesTheBrokerAuthoredTimestampNotMtime(t *testing.T) {
	b, l := reconcileBroker(t, time.Hour)

	// (1) A task that RAN two days ago but whose file was touched a second ago
	//     (a CI observation row, a `touch`, a backup restore). mtime says
	//     in-window; the broker-authored instant says otherwise, and the
	//     broker-authored instant wins.
	stale := taskID(t)
	writeTrace(t, b.AuditRoot, stale, trace{
		rows:      []string{brokerRow(100)},
		endedAtMs: recNow - (48 * time.Hour).Milliseconds(),
		mtime:     time.UnixMilli(recNow),
		outcome:   "pushed",
	})
	// (2) A task that ran a minute ago, in window.
	fresh := taskID(t)
	writeTrace(t, b.AuditRoot, fresh, trace{
		rows:      []string{brokerRow(2)},
		endedAtMs: recNow - 60_000,
		mtime:     time.UnixMilli(recNow),
		outcome:   "pushed",
	})

	reconcileGlobalLedger(b, recNow)

	u := l.Usage(recNow)
	if u.Starts != 1 || u.USD != 2 {
		t.Fatalf("starts=%d usd=%v, want 1 and 2: only the task that actually RAN in the window may count", u.Starts, u.USD)
	}
	if e := entryFor(t, l, fresh); e.EndedAtMs != recNow-60_000 {
		t.Errorf("ended_at_ms = %d, want the broker-authored %d, not the file's mtime %d",
			e.EndedAtMs, recNow-60_000, recNow)
	}
	for _, e := range ledgerLines(t, l) {
		if e.TaskID == stale {
			t.Error("a task whose broker-authored end time is outside the window was reconciled in on mtime alone")
		}
	}
}

// TestReconcileGlobalLedger_MTimeIsTheLastResortForAPreUpgradeTrace: a trace
// written before ended_at_ms existed has no broker-authored instant at all.
// mtime is then the only signal there is, and using it is honest — but it must
// be the fallback, never the preference (asserted above).
func TestReconcileGlobalLedger_MTimeIsTheLastResortForAPreUpgradeTrace(t *testing.T) {
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
		t.Errorf("ended_at_ms = %d, want the mtime fallback %d", e.EndedAtMs, recNow-120_000)
	}
	if e.USD != 1.5 {
		t.Errorf("usd = %v, want 1.5", e.USD)
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
	if u.USD != 11 {
		t.Errorf("usd = %v, want 11", u.USD)
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
	if !strings.Contains(le, notADir) {
		t.Errorf("degrade reason %q does not name the directory it could not read", le)
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

// TestReconcileGlobalLedger_UnreadableTraceCountsAStartAndDegrades: the
// per-trace version. The file's existence under a valid task id is itself
// evidence a task ran, so the START is recorded (never under-count the limb
// that bounds subscription mode) while the dollars are declared unknown.
func TestReconcileGlobalLedger_UnreadableTraceCountsAStartAndDegrades(t *testing.T) {
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
	if l.LoadError() == "" {
		t.Error("the ledger was not degraded after a trace could not be read; the USD limb would silently report a lower bound as fact")
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
	if u := l.Usage(recNow); u.Starts != 1 || u.USD != 1 {
		t.Errorf("starts=%d usd=%v, want 1 and 1: only <32-hex>.jsonl is a task trace", u.Starts, u.USD)
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
	if u.USD != float64(n)*0.5 {
		t.Errorf("usd = %v, want %v", u.USD, float64(n)*0.5)
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
