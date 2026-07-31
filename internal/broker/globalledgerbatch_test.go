package broker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These cover the three ADDITIVE store methods Task 3 needed: the batched write
// boot reconciliation uses instead of N fsyncs, the reconcile's lookback anchor,
// and the external degrade a failed reconcile uses to fail closed.

const batchNow = int64(1_700_000_000_000)

func batchLedger(t *testing.T, window time.Duration) (*GlobalLedger, string) {
	t.Helper()
	root := t.TempDir()
	l, err := OpenGlobalLedger(root, window, batchNow)
	if err != nil {
		t.Fatalf("OpenGlobalLedger: %v", err)
	}
	return l, root
}

func taskEntry(id string, endedAt int64, usd float64) GlobalEntry {
	return GlobalEntry{
		Kind: GlobalEntryTask, TaskID: id, EndedAtMs: endedAt,
		Vendor: "anthropic", Agent: "claude", Auth: "api_key", Metered: true,
		USD: usd, USDTrusted: true, Outcome: "pushed",
	}
}

// TestGlobalLedger_RecordBatch_MatchesRecordSemantics: the batch path must be
// Record with one fsync, not Record with different rules. Same validation, same
// dedupe, same durability.
func TestGlobalLedger_RecordBatch_MatchesRecordSemantics(t *testing.T) {
	l, root := batchLedger(t, 24*time.Hour)
	dup := newID()
	if err := l.Record(batchNow, taskEntry(dup, batchNow, 1)); err != nil {
		t.Fatal(err)
	}
	ids := []string{newID(), newID(), newID()}
	batch := []GlobalEntry{
		taskEntry(ids[0], batchNow-1, 2),
		taskEntry(dup, batchNow, 99),            // already recorded: suppressed
		taskEntry("not-a-task-id", batchNow, 5), // invalid: rejected, batch continues
		taskEntry(ids[1], batchNow-2, 3),
		{Kind: GlobalEntryCheckpoint, TaskID: ids[2], EndedAtMs: batchNow}, // wrong kind
	}
	added, err := l.RecordBatch(batchNow, batch)
	if added != 2 {
		t.Errorf("added = %d, want 2 (the two valid, non-duplicate task entries)", added)
	}
	if err == nil {
		t.Error("RecordBatch returned no error despite an invalid id and a non-task kind")
	}
	u := l.Usage(batchNow)
	if u.Starts != 3 || u.USD != 6 {
		t.Fatalf("starts=%d usd=%v, want 3 and 6 (1+2+3; the duplicate's 99 must be suppressed)", u.Starts, u.USD)
	}
	// Durable, and readable back as clean entries — a batch of lines written in
	// one write must parse exactly like lines written one at a time.
	reopened, err := OpenGlobalLedger(root, 24*time.Hour, batchNow)
	if err != nil {
		t.Fatal(err)
	}
	ru := reopened.Usage(batchNow)
	if ru.Starts != 3 || ru.USD != 6 || ru.Damaged != 0 {
		t.Errorf("after restart: starts=%d usd=%v damaged=%d, want 3, 6, 0", ru.Starts, ru.USD, ru.Damaged)
	}
	for _, id := range ids[:2] {
		if !reopened.Has(id) {
			t.Errorf("task %s did not survive the restart", id)
		}
	}
	if reopened.Has(ids[2]) {
		t.Error("a non-task entry was recorded through RecordBatch")
	}
}

// TestGlobalLedger_RecordBatch_IsOneAppend: the reason RecordBatch exists.
// Record fsyncs per call (~4ms), which a boot sweep of thousands of recovered
// terminals cannot afford. The observable proxy for "one write" is that the
// entries land contiguously as one block of lines with nothing else between.
func TestGlobalLedger_RecordBatch_IsOneAppend(t *testing.T) {
	l, _ := batchLedger(t, 24*time.Hour)
	const n = 500
	batch := make([]GlobalEntry, 0, n)
	for i := 0; i < n; i++ {
		batch = append(batch, taskEntry(newID(), batchNow-int64(i)-1, 0.01))
	}
	added, err := l.RecordBatch(batchNow, batch)
	if err != nil || added != n {
		t.Fatalf("RecordBatch: added=%d err=%v, want %d and nil", added, n, err)
	}
	data, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Count(strings.TrimRight(string(data), "\n"), "\n") + 1
	if lines != n {
		t.Errorf("ledger holds %d lines after one batch of %d, want %d", lines, n, n)
	}
	if u := l.Usage(batchNow); u.Starts != n {
		t.Errorf("starts = %d, want %d", u.Starts, n)
	}
}

// TestGlobalLedger_RecordBatch_KeepsEntriesWhenTheDiskWriteFails: identical to
// Record's rule and for the identical reason — the tasks really ran, and
// dropping them because the disk was full would UNDER-count the ceiling, the
// one direction G2/G3 forbid.
func TestGlobalLedger_RecordBatch_KeepsEntriesWhenTheDiskWriteFails(t *testing.T) {
	l, root := batchLedger(t, 24*time.Hour)
	// Make the ledger path unwritable by replacing its directory with a
	// read-only one after the handle exists.
	dir := filepath.Dir(GlobalLedgerPath(root))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot make the ledger dir read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	added, err := l.RecordBatch(batchNow, []GlobalEntry{
		taskEntry(newID(), batchNow, 4), taskEntry(newID(), batchNow, 6),
	})
	if added != 2 {
		t.Fatalf("added = %d, want 2 even when the durable write fails", added)
	}
	if err == nil {
		t.Error("RecordBatch hid a durable-write failure from its caller")
	}
	if u := l.Usage(batchNow); u.Starts != 2 || u.USD != 10 {
		t.Errorf("starts=%d usd=%v, want 2 and 10: entries stay in memory so this process over-counts rather than under-counts",
			u.Starts, u.USD)
	}
}

// TestGlobalLedger_NewestEventMs is the reconcile's lookback anchor: the newest
// REAL event, ignoring quarantine tombstones (whose timestamps are repair-time
// stamps, not events — letting one set the anchor would hand a bad clock exactly
// the influence the anchor exists to deny it).
func TestGlobalLedger_NewestEventMs(t *testing.T) {
	var nilLedger *GlobalLedger
	if got := nilLedger.NewestEventMs(); got != 0 {
		t.Errorf("nil ledger NewestEventMs = %d, want 0", got)
	}
	l, root := batchLedger(t, 24*time.Hour)
	if got := l.NewestEventMs(); got != 0 {
		t.Errorf("empty ledger NewestEventMs = %d, want 0", got)
	}
	if err := l.Record(batchNow, taskEntry(newID(), batchNow-5000, 1)); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(batchNow, taskEntry(newID(), batchNow-1000, 1)); err != nil {
		t.Fatal(err)
	}
	if got := l.NewestEventMs(); got != batchNow-1000 {
		t.Errorf("NewestEventMs = %d, want %d", got, batchNow-1000)
	}

	// A quarantine tombstone stamped far in the future must NOT move the anchor.
	f, err := os.OpenFile(GlobalLedgerPath(root), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("this line is not json\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	future := batchNow + (365 * 24 * time.Hour).Milliseconds()
	repaired, err := OpenGlobalLedger(root, 24*time.Hour, future)
	if err != nil {
		t.Fatal(err)
	}
	if got := repaired.NewestEventMs(); got != batchNow-1000 {
		t.Errorf("NewestEventMs = %d after a future-stamped tombstone, want the newest REAL event %d", got, batchNow-1000)
	}
}

// TestGlobalLedger_Degrade: the external fail-closed signal a failed boot
// reconciliation raises. It must set the same signal a failed load sets (so
// globalcap.go refuses on BOTH limbs), be sticky and first-wins, ignore an empty
// reason, be nil-safe, and — inherited, deliberately — stop the store rewriting
// itself from a picture it knows is incomplete.
func TestGlobalLedger_Degrade(t *testing.T) {
	var nilLedger *GlobalLedger
	nilLedger.Degrade("x") // must not panic

	l, _ := batchLedger(t, 24*time.Hour)
	if err := l.Record(batchNow, taskEntry(newID(), batchNow, 1)); err != nil {
		t.Fatal(err)
	}
	if l.LoadError() != "" || l.Usage(batchNow).Degraded {
		t.Fatal("a healthy ledger reported itself degraded")
	}
	l.Degrade("")
	if l.LoadError() != "" {
		t.Error("an empty reason degraded the ledger; a refusal with no explanation is worse than none")
	}

	l.Degrade("the audit could not be read")
	if l.LoadError() != "the audit could not be read" {
		t.Errorf("LoadError = %q, want the degrade reason", l.LoadError())
	}
	u := l.Usage(batchNow)
	if !u.Degraded || u.DegradedReason != "the audit could not be read" {
		t.Errorf("Usage degraded=%v reason=%q, want true and the degrade reason", u.Degraded, u.DegradedReason)
	}
	// Sticky and first-wins: the first reason is the root cause.
	l.Degrade("a later symptom")
	if l.LoadError() != "the audit could not be read" {
		t.Errorf("LoadError = %q, want the FIRST reason to win", l.LoadError())
	}
	// And a degraded store refuses to rewrite itself, because replacing bytes
	// from an incomplete picture would turn "I could not see everything" into
	// "this is everything".
	if err := l.Compact(batchNow); err == nil {
		t.Error("a degraded ledger rewrote itself")
	}
	// The starts it does know about are still reported: the sweep's job is to
	// add information, and degrading never discards any.
	if u := l.Usage(batchNow); u.Starts != 1 {
		t.Errorf("starts = %d after degrading, want 1", u.Starts)
	}
}
