package broker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const glID = "0123456789abcdef0123456789abcde0"

func glTaskID(n int) string { return fmt.Sprintf("%032x", n+1) }

// mustOpenGL opens a ledger that TRUSTS THE nowMs IT IS HANDED — the write
// clock's monotonic guard is off (OpenGlobalLedgerWithClock's nil mono).
//
// That is what a test driving time synthetically needs: these tests record at
// T and then compact or query at T+1e12 on purpose, and the production guard
// would correctly call that a forward clock jump and refuse to let the window
// advance. The guard itself is covered by the tests that inject a monotonic
// source and move the two apart deliberately — see
// TestGlobalLedgerWriteClockGuard* — not by leaving it armed here, where it
// would only make the rolling window untestable.
func mustOpenGL(t *testing.T, root string, window time.Duration, nowMs int64) *GlobalLedger {
	t.Helper()
	l, err := OpenGlobalLedgerWithClock(root, window, nowMs, nil)
	if err != nil {
		t.Fatalf("OpenGlobalLedger: %v", err)
	}
	return l
}

func glEntry(id string, endMs int64, usd float64) GlobalEntry {
	return GlobalEntry{
		TaskID:      id,
		StartedAtMs: endMs - 1000,
		EndedAtMs:   endMs,
		Vendor:      "anthropic",
		Agent:       "claude",
		Auth:        "api_key",
		Metered:     true,
		USD:         usd,
		USDTrusted:  true,
		Outcome:     "pushed",
		Src:         GlobalSrcTerminal,
	}
}

func mustRecord(t *testing.T, l *GlobalLedger, nowMs int64, e GlobalEntry) {
	t.Helper()
	if err := l.Record(nowMs, e); err != nil {
		t.Fatalf("Record(%s): %v", e.TaskID, err)
	}
}

// rawLines returns the ledger file's non-empty lines (none if it does not
// exist yet — a rejected Record must not create one).
func rawLines(t *testing.T, path string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var out [][]byte
	for _, ln := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(ln)) > 0 {
			out = append(out, ln)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Layout, permissions, and the "nothing agent-writable feeds the ledger" rule.
// ---------------------------------------------------------------------------

// The ledger must NOT sit directly in AuditRoot as a *.jsonl file: five
// independent scanners (seedAggregateFromAudit, drydock tasks/status/prune,
// stats, the web UI's audit routes) glob *.jsonl there and would parse the
// ledger as a task trace. Every one of them skips directories, so a
// subdirectory is the collision-free home.
func TestGlobalLedgerLivesInItsOwnSubdirectory(t *testing.T) {
	root := t.TempDir()
	path := GlobalLedgerPath(root)
	if filepath.Dir(path) == root {
		t.Fatalf("ledger path %q sits directly in the audit root; *.jsonl scanners would parse it", path)
	}
	l := mustOpenGL(t, root, time.Hour, 1000)
	mustRecord(t, l, 1000, glEntry(glID, 1000, 1))

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			t.Errorf("audit root gained a non-directory entry %q; audit scanners would see it", e.Name())
		}
	}
}

func TestGlobalLedgerPermissions(t *testing.T) {
	root := t.TempDir()
	l := mustOpenGL(t, root, time.Hour, 1000)
	mustRecord(t, l, 1000, glEntry(glID, 1000, 1))

	fi, err := os.Stat(l.Path())
	if err != nil {
		t.Fatalf("stat ledger: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("ledger perm = %o, want 0600", perm)
	}
	di, err := os.Stat(filepath.Dir(l.Path()))
	if err != nil {
		t.Fatalf("stat ledger dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("ledger dir perm = %o, want 0700", perm)
	}
}

// Mechanically, not by convention: nothing in the store may read a wall clock.
// Every timestamp arrives through the broker's b.now/nowMs seam, which is what
// makes the window deterministic in tests and auditable in production. Parsed
// rather than grepped so the file may DISCUSS time.Now in a comment.
func TestGlobalLedgerNoTimeNow(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "globalledger.go", nil, 0)
	if err != nil {
		t.Fatalf("parse source: %v", err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "time" && sel.Sel.Name == "Now" {
			t.Errorf("%s: globalledger.go calls time.Now; every timestamp must come from the caller's clock seam",
				fset.Position(sel.Pos()))
		}
		return true
	})
}

// ---------------------------------------------------------------------------
// The two limbs (G1): a rolling-window USD sum and a rolling-window start count.
// ---------------------------------------------------------------------------

func TestGlobalLedgerUsageSumsBothLimbs(t *testing.T) {
	root := t.TempDir()
	l := mustOpenGL(t, root, time.Hour, 0)
	mustRecord(t, l, 1000, glEntry(glTaskID(1), 1000, 0.25))
	mustRecord(t, l, 2000, glEntry(glTaskID(2), 2000, 0.75))
	mustRecord(t, l, 3000, glEntry(glTaskID(3), 3000, 1.00))

	u := l.Usage(3000)
	if u.USD != 2.0 {
		t.Errorf("USD = %v, want 2.0", u.USD)
	}
	if l.WindowMs() != int64(time.Hour/time.Millisecond) || u.WindowMs != l.WindowMs() {
		t.Errorf("WindowMs = %d / %d, want %d", l.WindowMs(), u.WindowMs, int64(time.Hour/time.Millisecond))
	}
	if u.Starts != 3 {
		t.Errorf("Starts = %d, want 3", u.Starts)
	}
	if u.Degraded {
		t.Errorf("Degraded = true on a clean ledger: %s", u.DegradedReason)
	}
	if u.OldestMs != 1000 || u.NewestMs != 3000 {
		t.Errorf("Oldest/Newest = %d/%d, want 1000/3000", u.OldestMs, u.NewestMs)
	}
}

// An unmetered (subscription) lane contributes NOTHING to the USD limb but is a
// full task start — that is exactly why G1 has two limbs.
func TestGlobalLedgerUntrustedUSDIsNotSummedButStillCounts(t *testing.T) {
	root := t.TempDir()
	l := mustOpenGL(t, root, time.Hour, 0)

	sub := glEntry(glTaskID(1), 1000, 9.99)
	sub.Auth = "subscription"
	sub.Metered = false
	sub.USDTrusted = false
	mustRecord(t, l, 1000, sub)
	mustRecord(t, l, 2000, glEntry(glTaskID(2), 2000, 0.5))

	u := l.Usage(2000)
	if u.USD != 0.5 {
		t.Errorf("USD = %v, want 0.5 (the subscription lane's figure is not trustworthy)", u.USD)
	}
	if u.UntrustedUSD != 9.99 {
		t.Errorf("UntrustedUSD = %v, want 9.99", u.UntrustedUSD)
	}
	if u.Starts != 2 {
		t.Errorf("Starts = %d, want 2 — the task-count limb must bound subscription mode", u.Starts)
	}
}

// A retry is a task start like any other (G1).
func TestGlobalLedgerRetriesCountAsStarts(t *testing.T) {
	root := t.TempDir()
	l := mustOpenGL(t, root, time.Hour, 0)
	parent := glEntry(glTaskID(1), 1000, 0.1)
	mustRecord(t, l, 1000, parent)
	for i := 2; i <= 4; i++ {
		child := glEntry(glTaskID(i), int64(i)*1000, 0.1)
		child.Attempt = i - 1
		child.RetryOf = glTaskID(i - 1)
		mustRecord(t, l, int64(i)*1000, child)
	}
	if u := l.Usage(4000); u.Starts != 4 {
		t.Errorf("Starts = %d, want 4 (parent + 3 retries)", u.Starts)
	}
}

// ---------------------------------------------------------------------------
// Window semantics.
// ---------------------------------------------------------------------------

// Coherent with gateway.spendLedger.windowed: the cutoff is STRICTLY after.
func TestGlobalLedgerWindowBoundaryIsStrictlyAfterCutoff(t *testing.T) {
	root := t.TempDir()
	const window = 1000 * time.Millisecond
	l := mustOpenGL(t, root, window, 0)
	// now = 10000, cutoff = 9000.
	mustRecord(t, l, 10000, glEntry(glTaskID(1), 9000, 1)) // exactly at the cutoff
	mustRecord(t, l, 10000, glEntry(glTaskID(2), 9001, 2)) // one ms after

	u := l.Usage(10000)
	if u.USD != 2 {
		t.Errorf("USD = %v, want 2 — the entry exactly at the cutoff is out of window", u.USD)
	}
	if u.Starts != 1 {
		t.Errorf("Starts = %d, want 1", u.Starts)
	}
}

func TestGlobalLedgerAgedOutEntriesDoNotCount(t *testing.T) {
	root := t.TempDir()
	l := mustOpenGL(t, root, time.Hour, 0)
	mustRecord(t, l, 1000, glEntry(glTaskID(1), 1000, 5))
	if u := l.Usage(1000 + int64(2*time.Hour/time.Millisecond)); u.USD != 0 || u.Starts != 0 {
		t.Errorf("usage = %+v, want an empty window", u)
	}
}

// window == 0 is total mode: no decay at all, and (unlike the gateway's
// in-memory ledger) it must survive a restart — G3.
func TestGlobalLedgerTotalModeHasNoDecay(t *testing.T) {
	root := t.TempDir()
	l := mustOpenGL(t, root, 0, 0)
	mustRecord(t, l, 1000, glEntry(glTaskID(1), 1000, 3))
	u := l.Usage(1000 + int64(365*24*time.Hour/time.Millisecond))
	if u.USD != 3 || u.Starts != 1 {
		t.Errorf("usage = %+v, want the running total to persist forever in total mode", u)
	}
}

// ---------------------------------------------------------------------------
// G3: durability across restart in BOTH window modes.
// ---------------------------------------------------------------------------

func TestGlobalLedgerDurableAcrossRestart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window time.Duration
	}{
		{"windowed", time.Hour},
		{"total", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			l := mustOpenGL(t, root, tc.window, 0)
			mustRecord(t, l, 1000, glEntry(glTaskID(1), 1000, 0.5))
			mustRecord(t, l, 2000, glEntry(glTaskID(2), 2000, 1.5))
			before := l.Usage(2000)

			// "Restart": a brand new store over the same directory.
			l2 := mustOpenGL(t, root, tc.window, 2000)
			after := l2.Usage(2000)
			if after.USD != before.USD || after.Starts != before.Starts {
				t.Errorf("after restart usage = %+v, want %+v", after, before)
			}
			if after.Degraded {
				t.Errorf("restart reported degraded: %s", after.DegradedReason)
			}
			if !l2.Has(glTaskID(1)) || !l2.Has(glTaskID(2)) {
				t.Error("Has lost a task id across restart; boot reconciliation would double-count")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tolerant scan vs never-under-count. This is the sharpest property.
// ---------------------------------------------------------------------------

// A garbage line must not fail the boot scan AND must not vanish: it becomes a
// quarantined "damaged" entry that still counts as a task start and flags the
// USD limb as a lower bound.
func TestGlobalLedgerDamagedLineIsQuarantinedNotDropped(t *testing.T) {
	root := t.TempDir()
	l := mustOpenGL(t, root, time.Hour, 0)
	mustRecord(t, l, 1000, glEntry(glTaskID(1), 1000, 0.5))
	appendRawGL(t, l.Path(), "{not json at all\n")
	mustRecordDirect(t, l.Path(), glEntry(glTaskID(2), 2000, 0.25))

	l2 := mustOpenGL(t, root, time.Hour, 2500)
	u := l2.Usage(2500)
	if u.Starts != 3 {
		t.Errorf("Starts = %d, want 3 — the damaged line is evidence a task ran and must not be dropped", u.Starts)
	}
	if u.Damaged != 1 {
		t.Errorf("Damaged = %d, want 1", u.Damaged)
	}
	if u.USD != 0.75 {
		t.Errorf("USD = %v, want 0.75 (the readable entries)", u.USD)
	}
	if !u.Degraded {
		t.Error("Degraded = false with a damaged entry in window; the USD limb is only a lower bound")
	}
	if u.DegradedReason == "" {
		t.Error("DegradedReason is empty; the operator must be told why the ceiling refuses")
	}

	// The repair rewrite is IDEMPOTENT: a second restart must not mint a
	// second tombstone for the same damage.
	l3 := mustOpenGL(t, root, time.Hour, 2600)
	if u3 := l3.Usage(2600); u3.Damaged != 1 || u3.Starts != 3 {
		t.Errorf("second restart usage = %+v, want Damaged=1 Starts=3 (repair must be idempotent)", u3)
	}
	// And the raw bytes survive for forensics.
	found := false
	for _, ln := range rawLines(t, l3.Path()) {
		var e GlobalEntry
		if json.Unmarshal(ln, &e) == nil && e.Kind == GlobalEntryDamaged {
			found = true
			if !bytes.Contains(e.Raw, []byte("not json at all")) {
				t.Errorf("tombstone lost the original bytes: %q", e.Raw)
			}
		}
	}
	if !found {
		t.Error("no damaged tombstone was persisted")
	}
}

// The crash signature: a torn trailing line from an interrupted append.
func TestGlobalLedgerTornTailLineIsQuarantined(t *testing.T) {
	root := t.TempDir()
	l := mustOpenGL(t, root, time.Hour, 0)
	mustRecord(t, l, 1000, glEntry(glTaskID(1), 1000, 0.5))
	appendRawGL(t, l.Path(), `{"kind":"task","task_id":"0123456`) // no newline: torn

	l2 := mustOpenGL(t, root, time.Hour, 2000)
	u := l2.Usage(2000)
	if u.Starts != 2 || u.Damaged != 1 {
		t.Errorf("usage = %+v, want Starts=2 Damaged=1", u)
	}
	if !u.Degraded {
		t.Error("a torn tail must degrade the USD limb rather than silently under-count")
	}
}

// The cimarker lesson: a body that fails its own self-consistency check is not
// trusted. Here the id shape is the check, and failing it is damage, not a
// silent skip.
func TestGlobalLedgerBadTaskIDOnDiskIsDamagedNotDropped(t *testing.T) {
	root := t.TempDir()
	l := mustOpenGL(t, root, time.Hour, 0)
	mustRecord(t, l, 1000, glEntry(glTaskID(1), 1000, 0.5))
	appendRawGL(t, l.Path(), `{"kind":"task","task_id":"../../etc/passwd","ended_at_ms":1500,"usd":1,"usd_trusted":true}`+"\n")

	l2 := mustOpenGL(t, root, time.Hour, 2000)
	u := l2.Usage(2000)
	if u.Damaged != 1 {
		t.Errorf("Damaged = %d, want 1 for a body with an invalid task id", u.Damaged)
	}
	if u.Starts != 2 {
		t.Errorf("Starts = %d, want 2 — the bad-id line still evidences a start", u.Starts)
	}
	if u.USD != 0.5 {
		t.Errorf("USD = %v, want 0.5 — an unvalidatable body's dollars are never summed", u.USD)
	}
}

// An entry with no timestamp has no position in the window; it must not be
// admitted as a well-formed entry that silently never ages out.
func TestGlobalLedgerZeroTimestampOnDiskIsDamaged(t *testing.T) {
	root := t.TempDir()
	l := mustOpenGL(t, root, time.Hour, 0)
	appendRawGL(t, l.Path(), `{"kind":"task","task_id":"`+glTaskID(1)+`","ended_at_ms":0,"usd":4,"usd_trusted":true}`+"\n")

	l2 := mustOpenGL(t, root, time.Hour, 5000)
	u := l2.Usage(5000)
	if u.Damaged != 1 || u.Starts != 1 || u.USD != 0 {
		t.Errorf("usage = %+v, want Damaged=1 Starts=1 USD=0", u)
	}
}

// A symlinked ledger must be refused outright (parity with the queuestore /
// cimarker reads), and refusal is FAIL-CLOSED: the store still exists, but it
// reports itself degraded so the ceiling refuses rather than admitting.
func TestGlobalLedgerSymlinkIsRefusedAndDegrades(t *testing.T) {
	root := t.TempDir()
	path := GlobalLedgerPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	elsewhere := filepath.Join(t.TempDir(), "planted.jsonl")
	if err := os.WriteFile(elsewhere, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write planted: %v", err)
	}
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	l, err := OpenGlobalLedger(root, time.Hour, 1000)
	if err == nil {
		t.Error("OpenGlobalLedger accepted a symlinked ledger")
	}
	if l == nil {
		t.Fatal("OpenGlobalLedger returned a nil store on error; the caller cannot fail closed on nil")
	}
	u := l.Usage(1000)
	if !u.Degraded {
		t.Error("a symlinked ledger must report Degraded so the ceiling refuses")
	}
	// The planted file must not have been clobbered or read through.
	if b, _ := os.ReadFile(elsewhere); string(b) != "{}\n" {
		t.Errorf("planted file was written through the symlink: %q", b)
	}
}

// A hard read failure must never be papered over by a rewrite that discards
// the bytes we could not read.
func TestGlobalLedgerDegradedStoreNeverRewritesTheFile(t *testing.T) {
	root := t.TempDir()
	path := GlobalLedgerPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	elsewhere := filepath.Join(t.TempDir(), "planted.jsonl")
	if err := os.WriteFile(elsewhere, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write planted: %v", err)
	}
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	l, _ := OpenGlobalLedger(root, time.Hour, 1000)
	if err := l.Compact(2000); err == nil {
		t.Error("Compact on a degraded store must refuse rather than replace unread bytes")
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the degraded store replaced the symlink; unread bytes were discarded")
	}
}

// The nil store is the shape Task 2 sees when the ledger could not be built at
// all. It must answer "I don't know", which is fail-closed.
func TestGlobalLedgerNilStoreFailsClosed(t *testing.T) {
	var l *GlobalLedger
	u := l.Usage(1000)
	if !u.Degraded || u.DegradedReason == "" {
		t.Errorf("nil store usage = %+v, want Degraded with a reason", u)
	}
	if l.Has(glID) {
		t.Error("nil store claimed to have a task id")
	}
	if err := l.Record(1000, glEntry(glID, 1000, 1)); err == nil {
		t.Error("nil store accepted a Record")
	}
}

// ---------------------------------------------------------------------------
// Recording rules.
// ---------------------------------------------------------------------------

func TestGlobalLedgerRecordDedupesByTaskID(t *testing.T) {
	root := t.TempDir()
	l := mustOpenGL(t, root, time.Hour, 0)
	mustRecord(t, l, 1000, glEntry(glID, 1000, 0.5))
	mustRecord(t, l, 1000, glEntry(glID, 1000, 0.5)) // replayed after a crash
	if u := l.Usage(1000); u.Starts != 1 || u.USD != 0.5 {
		t.Errorf("usage = %+v, want a single start (task ids are unique; a replay must not double-count)", u)
	}
	if n := len(rawLines(t, l.Path())); n != 1 {
		t.Errorf("ledger has %d lines, want 1", n)
	}
}

func TestGlobalLedgerRecordRejectsInvalidTaskID(t *testing.T) {
	root := t.TempDir()
	l := mustOpenGL(t, root, time.Hour, 0)
	for _, id := range []string{"", "../escape", "ABCDEF0123456789abcdef0123456789", strings.Repeat("a", 31)} {
		if err := l.Record(1000, glEntry(id, 1000, 1)); err == nil {
			t.Errorf("Record accepted task id %q", id)
		}
	}
	if n := len(rawLines(t, l.Path())); n != 0 {
		t.Errorf("a rejected Record wrote %d lines", n)
	}
}

// The caller's clock fills a missing stamp, so an entry is never dropped for
// want of a timestamp — but nothing in the store reads a wall clock.
func TestGlobalLedgerRecordStampsFromCallerClock(t *testing.T) {
	root := t.TempDir()
	l := mustOpenGL(t, root, time.Hour, 0)
	e := glEntry(glID, 0, 1)
	e.EndedAtMs = 0
	mustRecord(t, l, 7777, e)
	if u := l.Usage(7777); u.NewestMs != 7777 {
		t.Errorf("NewestMs = %d, want 7777 (stamped from the caller's clock)", u.NewestMs)
	}
}

// A durable write that fails must not erase the fact that the task ran: the
// entry stays in memory (over-counting for this process's life is the safe
// direction) and the error is reported.
func TestGlobalLedgerRecordKeepsEntryWhenTheDiskWriteFails(t *testing.T) {
	root := t.TempDir()
	l := mustOpenGL(t, root, time.Hour, 0)
	mustRecord(t, l, 1000, glEntry(glTaskID(1), 1000, 0.5))
	if err := os.Chmod(l.Path(), 0o400); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(l.Path(), 0o600) })
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only file is still writable")
	}
	err := l.Record(2000, glEntry(glTaskID(2), 2000, 1.5))
	if err == nil {
		t.Fatal("Record reported success against a read-only ledger")
	}
	if u := l.Usage(2000); u.Starts != 2 || u.USD != 2.0 {
		t.Errorf("usage = %+v, want the un-persisted entry still counted (never under-count)", u)
	}
}

func TestGlobalLedgerHas(t *testing.T) {
	root := t.TempDir()
	l := mustOpenGL(t, root, time.Hour, 0)
	mustRecord(t, l, 1000, glEntry(glID, 1000, 1))
	if !l.Has(glID) {
		t.Error("Has = false for a recorded id")
	}
	if l.Has(glTaskID(99)) {
		t.Error("Has = true for an unrecorded id")
	}
}

// ---------------------------------------------------------------------------
// Compaction.
// ---------------------------------------------------------------------------

// Compaction must fire off the APPEND path too, not just at boot: a broker
// that never restarts must still bound its ledger file.
func TestGlobalLedgerRecordPathCompacts(t *testing.T) {
	root := t.TempDir()
	const window = time.Hour
	windowMs := int64(window / time.Millisecond)
	l := mustOpenGL(t, root, window, 0)

	base := int64(1_000_000_000_000)
	for i := 0; i < 10; i++ {
		mustRecord(t, l, base, glEntry(glTaskID(i), base+int64(i), 1))
	}
	recentBase := base + 10*windowMs
	total := globalLedgerMinCompactEntries + 20
	for i := 10; i < total; i++ {
		mustRecord(t, l, recentBase+int64(i), glEntry(glTaskID(i), recentBase+int64(i), 0.01))
	}
	now := recentBase + int64(total)
	wantStarts := total - 10
	if u := l.Usage(now); u.Starts != wantStarts {
		t.Errorf("Starts = %d, want %d", u.Starts, wantStarts)
	}
	// The aged-out entries must be gone from the FILE, not merely from the sum.
	lines := rawLines(t, l.Path())
	if len(lines) >= total {
		t.Errorf("ledger still has %d lines for %d entries; compaction never ran on the append path", len(lines), total)
	}
	if len(lines) < wantStarts {
		t.Errorf("ledger has %d lines but %d entries are still in window; compaction lost data", len(lines), wantStarts)
	}
}

func TestGlobalLedgerBootCompactionPrunesAgedOutEntriesOnly(t *testing.T) {
	root := t.TempDir()
	const window = time.Hour
	windowMs := int64(window / time.Millisecond)
	base := int64(1_000_000_000_000)

	var seed []GlobalEntry
	for i := 0; i < 10; i++ { // aged out
		seed = append(seed, glEntry(glTaskID(i), base+int64(i), 1))
	}
	recentBase := base + 10*windowMs
	total := globalLedgerMinCompactEntries + 20
	for i := 10; i < total; i++ { // in window
		seed = append(seed, glEntry(glTaskID(i), recentBase+int64(i), 0.01))
	}
	seedGL(t, root, seed)

	now := recentBase + int64(total)
	l := mustOpenGL(t, root, window, now)
	wantStarts := total - 10
	u := l.Usage(now)
	if u.Starts != wantStarts {
		t.Errorf("Starts = %d, want %d", u.Starts, wantStarts)
	}
	if lines := rawLines(t, l.Path()); len(lines) != wantStarts {
		t.Errorf("ledger has %d lines, want exactly the %d in-window entries", len(lines), wantStarts)
	}
	// The surviving dollars are exactly the in-window ones — pruning must not
	// have clipped an entry that is still inside the window.
	if want := 0.01 * float64(wantStarts); !glClose(u.USD, want) {
		t.Errorf("USD = %v, want %v", u.USD, want)
	}
	// And a second restart is stable.
	l2 := mustOpenGL(t, root, window, now)
	if u2 := l2.Usage(now); u2.Starts != wantStarts {
		t.Errorf("after a second restart Starts = %d, want %d", u2.Starts, wantStarts)
	}
}

// Total mode has no decay, so compaction folds the tail into a checkpoint
// instead of dropping it. The sums must be preserved EXACTLY.
func TestGlobalLedgerTotalModeFoldsIntoCheckpoint(t *testing.T) {
	root := t.TempDir()
	total := globalLedgerTotalKeep + globalLedgerMinCompactEntries + 50
	var seed []GlobalEntry
	for i := 0; i < total; i++ {
		seed = append(seed, glEntry(glTaskID(i), int64(i)+1, 0.5))
	}
	seedGL(t, root, seed)

	now := int64(total) + 1
	l := mustOpenGL(t, root, 0, now)
	u := l.Usage(now)
	if u.Starts != total {
		t.Errorf("Starts = %d, want %d", u.Starts, total)
	}
	if want := 0.5 * float64(total); !glClose(u.USD, want) {
		t.Errorf("USD = %v, want %v", u.USD, want)
	}
	// The fold's copies + the retained tail. The checkpoint is written
	// globalLedgerCheckpointCopies times on purpose: it carries the folded
	// start count of the entire history, so it must not hinge on one byte.
	if want := globalLedgerTotalKeep + globalLedgerCheckpointCopies; len(rawLines(t, l.Path())) != want {
		t.Errorf("ledger has %d lines, want %d (the fold's copies + the retained tail) — total mode must stay bounded",
			len(rawLines(t, l.Path())), want)
	}
	// Restart agrees, and folding is idempotent.
	l2 := mustOpenGL(t, root, 0, now)
	u2 := l2.Usage(now)
	if u2.Starts != u.Starts || !glClose(u2.USD, u.USD) {
		t.Errorf("after restart usage = %+v, want %+v", u2, u)
	}
	// The most recent ids stay individually addressable so boot reconciliation
	// cannot re-add them.
	if !l2.Has(glTaskID(total - 1)) {
		t.Error("the newest task id was folded away; reconciliation would double-count it")
	}
	// The oldest were folded — Has is false for them, which is why the retained
	// tail has to be much larger than any crash window.
	if l2.Has(glTaskID(0)) {
		t.Error("expected the oldest id to have been folded into the checkpoint")
	}
}

// The append path in total mode stays bounded too: the compaction threshold
// doubles off the retained tail, so the file oscillates between the tail size
// and twice it, never growing with the number of tasks ever run.
func TestGlobalLedgerTotalModeStaysBoundedOnTheAppendPath(t *testing.T) {
	root := t.TempDir()
	var seed []GlobalEntry
	for i := 0; i < 2*globalLedgerTotalKeep+200; i++ {
		seed = append(seed, glEntry(glTaskID(i), int64(i)+1, 0.5))
	}
	seedGL(t, root, seed)
	l := mustOpenGL(t, root, 0, int64(len(seed))+1)
	if want := 2*globalLedgerTotalKeep + globalLedgerCheckpointCopies + 1; len(rawLines(t, l.Path())) > want {
		t.Errorf("ledger has %d lines, want <= %d", len(rawLines(t, l.Path())), want)
	}
	if err := l.Compact(int64(len(seed)) + 1); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if want := globalLedgerTotalKeep + globalLedgerCheckpointCopies; len(rawLines(t, l.Path())) != want {
		t.Errorf("after an explicit Compact the ledger has %d lines, want %d",
			len(rawLines(t, l.Path())), want)
	}
}

// A checkpoint written in total mode and then read back under a window keeps
// its whole historical total in window until its own newest folded event ages
// out — over-counting, which is the required direction.
func TestGlobalLedgerCheckpointReadUnderAWindowOverCounts(t *testing.T) {
	root := t.TempDir()
	total := globalLedgerTotalKeep + globalLedgerMinCompactEntries + 50
	var seed []GlobalEntry
	for i := 0; i < total; i++ {
		seed = append(seed, glEntry(glTaskID(i), int64(i)+1, 0.5))
	}
	seedGL(t, root, seed)
	now := int64(total) + 1
	// Fold it in total mode first.
	mustOpenGL(t, root, 0, now)
	// Now an operator switches global_window from 0 to 1h.
	l2 := mustOpenGL(t, root, time.Hour, now)
	u := l2.Usage(now)
	if u.Starts != total {
		t.Errorf("Starts = %d, want %d — a fold read under a window must not lose starts", u.Starts, total)
	}
}

// Compaction goes through atomicfile (temp + rename), so a crash can only ever
// leave the whole old file or the whole new one — never a mix — and no stray
// .tmp survives a clean run.
func TestGlobalLedgerCompactionLeavesNoTempFile(t *testing.T) {
	root := t.TempDir()
	l := mustOpenGL(t, root, time.Hour, 0)
	mustRecord(t, l, 1000, glEntry(glTaskID(1), 1000, 1))
	if err := l.Compact(2000); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if _, err := os.Stat(l.Path() + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("a .tmp survived compaction (err=%v)", err)
	}
}

// A .tmp left by a crashed atomic write is swept at boot, exactly as the queue
// store sweeps its own.
func TestGlobalLedgerBootSweepsStrayTemp(t *testing.T) {
	root := t.TempDir()
	l := mustOpenGL(t, root, time.Hour, 0)
	mustRecord(t, l, 1000, glEntry(glTaskID(1), 1000, 1))
	if err := os.WriteFile(l.Path()+".tmp", []byte("half a lin"), 0o600); err != nil {
		t.Fatalf("write stray tmp: %v", err)
	}
	l2 := mustOpenGL(t, root, time.Hour, 2000)
	if _, err := os.Stat(l2.Path() + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("boot did not sweep the stray .tmp (err=%v)", err)
	}
	if u := l2.Usage(2000); u.Starts != 1 || u.Damaged != 0 {
		t.Errorf("usage = %+v; the stray .tmp must not be read as ledger content", u)
	}
}

// ---------------------------------------------------------------------------
// Clock movement.
// ---------------------------------------------------------------------------

// Backwards clock: the cutoff moves back too, so MORE entries are in window.
// Over-counting is the safe direction, and nothing may be pruned.
func TestGlobalLedgerClockMovingBackwardsNeverPrunes(t *testing.T) {
	root := t.TempDir()
	const window = time.Hour
	base := int64(1_000_000_000_000)
	var seed []GlobalEntry
	for i := 0; i < globalLedgerMinCompactEntries+5; i++ {
		seed = append(seed, glEntry(glTaskID(i), base+int64(i), 0.01))
	}
	seedGL(t, root, seed)
	l := mustOpenGL(t, root, window, base+10000)
	want := l.Usage(base + 10000)
	// The clock jumps a week backwards; a compaction runs against it.
	past := base - int64(7*24*time.Hour/time.Millisecond)
	if err := l.Compact(past); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got := l.Usage(base + 10000); got.Starts != want.Starts || got.USD != want.USD {
		t.Errorf("after a backwards clock jump usage = %+v, want %+v", got, want)
	}
}

// Forwards clock: a bogus far-future `now` must not be able to prune a ledger
// whose own newest event is recent. Pruning is destructive, so it uses the
// EARLIER of the caller's clock and the ledger's own newest event.
func TestGlobalLedgerForwardClockJumpDoesNotDestroyEntries(t *testing.T) {
	root := t.TempDir()
	const window = time.Hour
	l := mustOpenGL(t, root, window, 0)
	base := int64(1_000_000_000_000)
	for i := 0; i < 20; i++ {
		mustRecord(t, l, base+int64(i), glEntry(glTaskID(i), base+int64(i), 0.01))
	}
	future := base + int64(3650*24*time.Hour/time.Millisecond)
	if err := l.Compact(future); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if n := len(rawLines(t, l.Path())); n != 20 {
		t.Errorf("ledger has %d lines after a far-future compaction, want 20", n)
	}
	if u := l.Usage(base + 20); u.Starts != 20 {
		t.Errorf("Starts = %d, want 20", u.Starts)
	}
}

// ---------------------------------------------------------------------------
// Concurrency and scale.
// ---------------------------------------------------------------------------

// The dispatcher, the synchronous /tasks path and the CI watcher can all
// terminal a task, so every entry point is shared-goroutine reachable.
func TestGlobalLedgerConcurrentWritersAndReaders(t *testing.T) {
	root := t.TempDir()
	l := mustOpenGL(t, root, time.Hour, 0)
	const writers, per = 8, 60

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				n := w*per + i
				now := int64(1_000_000_000_000 + n)
				if err := l.Record(now, glEntry(glTaskID(n), now, 0.01)); err != nil {
					t.Errorf("Record: %v", err)
					return
				}
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = l.Usage(1_000_000_000_000)
				_ = l.Has(glTaskID(i))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := l.Compact(1_000_000_000_000 + int64(i)); err != nil {
				t.Errorf("Compact: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	now := int64(1_000_000_000_000 + writers*per)
	if u := l.Usage(now); u.Starts != writers*per {
		t.Errorf("Starts = %d, want %d", u.Starts, writers*per)
	}
	l2 := mustOpenGL(t, root, time.Hour, now)
	if u := l2.Usage(now); u.Starts != writers*per || u.Damaged != 0 {
		t.Errorf("after restart usage = %+v, want %d clean starts", u, writers*per)
	}
}

// Usage is on the admission path, so it must stay cheap with a large ledger,
// and the boot scan must be linear rather than quadratic.
func TestGlobalLedgerScalesToManyEntries(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	root := t.TempDir()
	const window = time.Hour
	base := int64(1_000_000_000_000)
	const n = 50000
	seed := make([]GlobalEntry, 0, n)
	for i := 0; i < n; i++ {
		seed = append(seed, glEntry(glTaskID(i), base+int64(i), 0.001))
	}
	seedGL(t, root, seed)

	now := base + n
	bootStart := time.Now()
	l := mustOpenGL(t, root, window, now)
	if boot := time.Since(bootStart); boot > 10*time.Second {
		t.Errorf("boot scan of %d entries took %v", n, boot)
	}
	start := time.Now()
	u := l.Usage(now)
	elapsed := time.Since(start)
	if u.Starts != n {
		t.Errorf("Starts = %d, want %d", u.Starts, n)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("Usage over %d entries took %v; too slow for an admission check", n, elapsed)
	}
	// One more record on top of a large ledger must not be O(n).
	recStart := time.Now()
	mustRecord(t, l, now, glEntry(glTaskID(n), now, 0.001))
	if d := time.Since(recStart); d > 2*time.Second {
		t.Errorf("Record onto a %d-entry ledger took %v", n, d)
	}
}

// ---------------------------------------------------------------------------
// helpers that write to the ledger file behind the store's back
// ---------------------------------------------------------------------------

func appendRawGL(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open ledger for raw append: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatalf("raw append: %v", err)
	}
}

// seedGL writes a whole ledger file in one shot, so a large-scale test does not
// pay one fsync per entry for state it only needs to exist on disk.
func seedGL(t *testing.T, root string, entries []GlobalEntry) {
	t.Helper()
	path := GlobalLedgerPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var buf bytes.Buffer
	for _, e := range entries {
		if e.Kind == "" {
			e.Kind = GlobalEntryTask
		}
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
}

// glClose compares accumulated float sums, which are order-dependent at the
// last bit.
func glClose(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

// mustRecordDirect appends a well-formed entry without going through the store,
// so a test can build an on-disk file the store has never seen.
func mustRecordDirect(t *testing.T, path string, e GlobalEntry) {
	t.Helper()
	if e.Kind == "" {
		e.Kind = GlobalEntryTask
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	appendRawGL(t, path, string(b)+"\n")
}

// ---------------------------------------------------------------------------
// Security-review regressions: the checkpoint's integrity, the damage
// classification behind "one damaged line is one start", and the dollar figure
// the ledger is willing to believe.
// ---------------------------------------------------------------------------

// glFoldedLedger builds a total-mode ledger past the fold threshold, so its
// file starts with the checkpoint copies.
func glFoldedLedger(t *testing.T, root string, n int, usd float64) *GlobalLedger {
	t.Helper()
	l := mustOpenGL(t, root, 0, capNow)
	for i := 0; i < n; i++ {
		mustRecord(t, l, capNow, glEntry(glTaskID(i), capNow-int64(i), usd))
	}
	if err := l.Compact(capNow); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if u := l.Usage(capNow); u.Starts != n {
		t.Fatalf("seeded Starts = %d, want %d", u.Starts, n)
	}
	return l
}

// A CHECKPOINT carries the folded start count of the entire history, so damage
// to it must not be scored as "one damaged line, one start". One corrupted byte
// used to turn 2600 starts into 2001 — silently, with LoadError empty so the
// start limb did not even refuse — and the forced repair rewrite then replaced
// the checkpoint on disk, making the loss permanent.
//
// The copies make ordinary damage RECOVERABLE rather than merely detectable.
func TestGlobalLedgerCheckpointSurvivesAOneByteEdit(t *testing.T) {
	root := t.TempDir()
	l := glFoldedLedger(t, root, globalLedgerTotalKeep+600, 1)
	before := l.Usage(capNow)

	path := GlobalLedgerPath(root)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte(`{"kind":"checkpoint"`)) {
		t.Fatalf("the file does not start with a checkpoint: %.60s", raw)
	}
	raw[3] = 'X' // one byte, inside the first copy
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	l2 := mustOpenGL(t, root, 0, capNow)
	after := l2.Usage(capNow)
	if after.Starts != before.Starts {
		t.Errorf("Starts = %d after a one-byte edit, want %d: the folded history was lost",
			after.Starts, before.Starts)
	}
	if !glClose(after.USD, before.USD) {
		t.Errorf("USD = %v after a one-byte edit, want %v", after.USD, before.USD)
	}
	if after.StartsDegraded {
		t.Errorf("the start limb degraded on damage that was fully recovered: %s", after.StartsDegradedReason)
	}
	// The repair rewrite must restore the full set of copies, not persist the
	// loss, and a third open must still agree.
	third := mustOpenGL(t, root, 0, capNow).Usage(capNow)
	if third.Starts != before.Starts {
		t.Errorf("Starts = %d after the repair rewrite, want %d", third.Starts, before.Starts)
	}
	if got := len(rawLines(t, path)); got != globalLedgerTotalKeep+globalLedgerCheckpointCopies {
		t.Errorf("the repaired file has %d lines, want %d: the copies were not restored",
			got, globalLedgerTotalKeep+globalLedgerCheckpointCopies)
	}
}

// A corruption that lands INSIDE A DIGIT still parses as JSON. Without the
// checksum the ledger would believe the smaller number forever.
func TestGlobalLedgerCheckpointRejectsATamperedCount(t *testing.T) {
	root := t.TempDir()
	l := glFoldedLedger(t, root, globalLedgerTotalKeep+600, 1)
	before := l.Usage(capNow)

	path := GlobalLedgerPath(root)
	lines := rawLines(t, path)
	var cp GlobalEntry
	if err := json.Unmarshal(lines[0], &cp); err != nil {
		t.Fatal(err)
	}
	cp.Starts = 1 // the tamper: valid JSON, valid shape, wrong number
	tampered, _ := json.Marshal(cp)
	lines[0] = tampered
	var buf bytes.Buffer
	for _, ln := range lines {
		buf.Write(ln)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	after := mustOpenGL(t, root, 0, capNow).Usage(capNow)
	if after.Starts != before.Starts {
		t.Errorf("Starts = %d after a tampered count, want %d (recovered from a surviving copy)",
			after.Starts, before.Starts)
	}
}

// When EVERY copy is destroyed the counts are genuinely gone. The only honest
// answer is to say the start count is a lower bound, which the ceiling turns
// into a refusal — never to report the smaller number as fact.
func TestGlobalLedgerLosingEveryCheckpointCopyDegradesTheStartLimb(t *testing.T) {
	root := t.TempDir()
	glFoldedLedger(t, root, globalLedgerTotalKeep+600, 1)
	path := GlobalLedgerPath(root)
	lines := rawLines(t, path)
	var buf bytes.Buffer
	for i, ln := range lines {
		if i < globalLedgerCheckpointCopies {
			buf.WriteString("{")
		}
		buf.Write(ln)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	u := mustOpenGL(t, root, 0, capNow).Usage(capNow)
	if !u.StartsDegraded {
		t.Fatal("every checkpoint copy was destroyed and the start count was still reported as fact")
	}
	if u.StartsDegradedReason == "" {
		t.Error("a degraded start limb with no reason is a refusal with no explanation")
	}
}

// A repair overwrites the damaged bytes with tombstones. The pre-repair file is
// the only record of what they said, so it is preserved rather than destroyed.
func TestGlobalLedgerRepairPreservesTheDamagedBytes(t *testing.T) {
	root := t.TempDir()
	path := GlobalLedgerPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	const junk = "{corrupted beyond recovery"
	if err := os.WriteFile(path, []byte(junk+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenGlobalLedger(root, 24*time.Hour, capNow); err != nil {
		t.Fatalf("OpenGlobalLedger: %v", err)
	}
	hits, err := filepath.Glob(path + ".damaged-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("found %d preserved copies of the damaged ledger, want 1", len(hits))
	}
	kept, err := os.ReadFile(hits[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(kept), junk) {
		t.Errorf("the preserved copy does not hold the damaged bytes: %q", kept)
	}
	fi, err := os.Stat(hits[0])
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("the preserved copy is mode %v, want 0600", fi.Mode().Perm())
	}
}

// "One damaged line is one task start" is a LOWER BOUND. A torn line that still
// identifies itself as one task entry is worth exactly one; anything else may
// be worth more, and the difference has to reach the start limb.
func TestGlobalLedgerDamageClassification(t *testing.T) {
	id := glTaskID(7)
	cases := []struct {
		name        string
		line        string
		wantOneTask bool
	}{
		{"a torn task line keeps kind and task_id", `{"kind":"task","task_id":"` + id + `","started_at_ms":170000`, true},
		{"a byte flip in the middle of a task line", `{"kind":"task","task_id":"` + id + `","ended_at_msX":1700000000000}`, true},
		{"a damaged checkpoint", `{"kind":"checkpoint","starts":2600,"usdX":12}`, false},
		{"a byte flip inside the kind", `{"kind":"tXsk","task_id":"` + id + `","ended_at_ms":1}`, false},
		{"two entries whose separating newline was destroyed",
			`{"kind":"task","task_id":"` + id + `","ended_at_ms":1}{"kind":"task","task_id":"` + id + `","ended_at_ms":2}`, false},
		{"unrecognisable junk", `{not json at all`, false},
		{"a region longer than any single entry", `{"kind":"task","task_id":"` + id + `",` + strings.Repeat("x", globalLedgerMaxTaskLineBytes), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := quarantine([]byte(tc.line), capNow)
			if q.StartsUnknown == tc.wantOneTask {
				t.Errorf("StartsUnknown = %v, want %v", q.StartsUnknown, !tc.wantOneTask)
			}
		})
	}
}

// A NaN spends the ceiling in the one direction it must never fail: every
// comparison against NaN is false, so `usage.USD >= budget` is false forever and
// the USD limb admits without bound. It also breaks json.Marshal, which would
// wedge every later compaction and let the file grow without limit.
func TestGlobalLedgerRejectsAnUnusableSpendFigure(t *testing.T) {
	for _, tc := range []struct {
		name string
		usd  float64
	}{
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
		{"negative", -100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			l := mustOpenGL(t, root, time.Hour, capNow)
			e := glEntry(glTaskID(0), capNow, 0)
			e.USD = tc.usd
			if err := l.Record(capNow, e); err == nil {
				t.Error("Record accepted the figure silently; the operator learns nothing")
			}
			u := l.Usage(capNow)
			if math.IsNaN(u.USD) || math.IsInf(u.USD, 0) || u.USD < 0 {
				t.Errorf("USD = %v reached the running total", u.USD)
			}
			// The task still ran: the start must be counted, only the money
			// disclaimed.
			if u.Starts != 1 {
				t.Errorf("Starts = %d, want 1", u.Starts)
			}
			// And compaction must still work, or the file grows forever.
			if err := l.Compact(capNow); err != nil {
				t.Errorf("compaction wedged on the bad entry: %v", err)
			}
			// The durable file must round-trip to the same answer.
			if got := mustOpenGL(t, root, time.Hour, capNow).Usage(capNow); got.Starts != 1 || got.USD != 0 {
				t.Errorf("after a reopen usage = %+v, want 1 start and $0", got)
			}
		})
	}
}

// The same rule on the READ side: a hand-edited or replayed line whose dollars
// are unusable is damage, not data. A negative figure is the sharp case — it
// SUBTRACTS from the limb and buys back headroom.
func TestGlobalLedgerDecodeRejectsUnusableSpendFigures(t *testing.T) {
	cases := map[string]GlobalEntry{
		"a negative task usd":             {Kind: GlobalEntryTask, TaskID: glTaskID(1), EndedAtMs: capNow, USD: -5},
		"a negative checkpoint usd":       {Kind: GlobalEntryCheckpoint, EndedAtMs: capNow, Starts: 3, USD: -5000},
		"a negative checkpoint untrusted": {Kind: GlobalEntryCheckpoint, EndedAtMs: capNow, Starts: 3, UntrustedUSD: -5000},
		"a checkpoint with no checksum":   {Kind: GlobalEntryCheckpoint, EndedAtMs: capNow, Starts: 3, USD: 5},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			line, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := decodeGlobalEntry(line); ok {
				t.Errorf("decoded %s as data", name)
			}
		})
	}
	// ...and a well-formed checkpoint still decodes, or the copies would be
	// unreadable and every fold would be damage.
	good := GlobalEntry{Kind: GlobalEntryCheckpoint, Src: GlobalSrcCompact, EndedAtMs: capNow, Starts: 3, USD: 5}
	good.Sum = checkpointSum(good)
	line, _ := json.Marshal(good)
	if _, ok := decodeGlobalEntry(line); !ok {
		t.Error("a well-formed checkpoint did not decode")
	}
}

// TestGlobalLedgerFoldCarriesStartsUnknown is the I1 failure the file header
// argues this store defends against, found INSIDE the defence.
//
// A destroyed region that took its separating newlines with it is quarantined
// as ONE tombstone standing for an unknown number of entries, and marked
// StartsUnknown so the start limb refuses rather than believing a count it
// cannot justify. In TOTAL mode that tombstone is eventually FOLDED into a
// checkpoint — and the checkpoint had nowhere to carry the flag, so the fold
// silently converted "I do not know how many starts these bytes were" into
// "exactly one", wrote that to disk, and the limb stopped refusing. Forever:
// total mode never ages anything out.
func TestGlobalLedgerFoldCarriesStartsUnknown(t *testing.T) {
	root := t.TempDir()
	const T = int64(1_700_000_000_000)
	l := mustOpenGL(t, root, 0, T) // total mode
	for n := 0; n < 10; n++ {
		mustRecord(t, l, T+int64(n), glEntry(glTaskID(n), T+int64(n), 1))
	}
	// A damaged region holding two concatenated entries — the case StartsUnknown
	// exists for.
	f, err := os.OpenFile(l.Path(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, `{"kind":"task","task_id":%q,"ended_at_ms":1}{"kind":"task","task_id":%q,"ended_at_ms":1}`+"\n",
		glTaskID(500), glTaskID(501))
	f.Close()

	l2 := mustOpenGL(t, root, 0, T+100)
	if u := l2.Usage(T + 100); !u.StartsDegraded {
		t.Fatalf("precondition: a multi-entry damaged region must set StartsDegraded, got %+v", u)
	}

	// The install keeps running until the tombstone is pushed past the fold
	// horizon.
	for n := 1000; n < 1000+globalLedgerTotalKeep+10; n++ {
		mustRecord(t, l2, T+int64(n), glEntry(glTaskID(n), T+int64(n), 1))
	}
	if err := l2.Compact(T + 999999); err != nil {
		t.Fatal(err)
	}
	if u := l2.Usage(T + 999999); !u.StartsDegraded {
		t.Errorf("FAIL-OPEN: folding erased StartsUnknown. starts=%d, and the start limb — the only bound on a "+
			"subscription lane — stopped refusing over a region whose true start count is unknown", u.Starts)
	}
	// And it survives the restart, because the fold was written to disk.
	l3 := mustOpenGL(t, root, 0, T+1_000_000)
	if u := l3.Usage(T + 1_000_000); !u.StartsDegraded {
		t.Errorf("the erased StartsUnknown was PERSISTED: after a restart starts=%d and the limb still does not refuse", u.Starts)
	}
}

// A checkpoint's StartsUnknown is covered by its checksum, so it cannot be
// cleared by a hand edit that leaves the rest of the line intact.
func TestGlobalLedgerCheckpointStartsUnknownIsChecksummed(t *testing.T) {
	cp := GlobalEntry{Kind: GlobalEntryCheckpoint, EndedAtMs: 1, Starts: 5, Src: GlobalSrcCompact, StartsUnknown: true}
	cp.Sum = checkpointSum(cp)
	cleared := cp
	cleared.StartsUnknown = false
	if cleared.Sum == checkpointSum(cleared) {
		t.Error("clearing StartsUnknown left the checkpoint's checksum valid; the flag is outside the digest")
	}
	line, err := json.Marshal(cleared)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decodeGlobalEntry(line); ok {
		t.Error("a checkpoint with StartsUnknown cleared under the original checksum was accepted as data")
	}
}
