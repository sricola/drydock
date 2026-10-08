package broker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	rtID1 = "0123456789abcdef0123456789abcdef"
	rtID2 = "fedcba9876543210fedcba9876543210"
	rtID3 = "00000000000000000000000000000001"
)

func rtEntry(kind, id, repoKey, instrSHA, issueURL, diffSHA string) RejectionEntry {
	return RejectionEntry{Kind: kind, AtMs: 1000, TaskID: id, RepoKey: repoKey,
		InstructionSHA256: instrSHA, IssueURL: issueURL, DiffSHA256: diffSHA, Path: "live"}
}

func TestRejectionKeys_CanonicalLowercaseRepoAndRootInstruction(t *testing.T) {
	r1, i1, k1 := RejectionKeys("https://github.com/O/R.git", "", "do x", "")
	r2, i2, k2 := RejectionKeys("git@github.com:o/r", "do x", "do x\n\n[ci evidence]", "")
	if r1 != r2 || r1 != "github.com/o/r" {
		t.Fatalf("repo keys differ or are not canonical lowercase: %q vs %q", r1, r2)
	}
	if i1 != i2 || len(i1) != 64 {
		t.Fatalf("instruction hashes differ or are not sha256 hex: %q vs %q", i1, i2)
	}
	if k1 != "" || k2 != "" {
		t.Fatalf("issue key must be empty without an issue url: %q %q", k1, k2)
	}
	_, _, k3 := RejectionKeys("https://github.com/o/r.git", "", "anything", "https://github.com/o/r/issues/42")
	_, _, k4 := RejectionKeys("git@github.com:O/R", "", "something else", "https://github.com/o/r/issues/42")
	if k3 == "" || k3 != k4 {
		t.Fatalf("issue key must be repo+url regardless of instruction: %q vs %q", k3, k4)
	}
}

func TestRejectionLedger_RoundTripCountsAndResets(t *testing.T) {
	root := t.TempDir()
	l, err := OpenRejectionLedger(root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	repo, instr, _ := RejectionKeys("https://github.com/o/r.git", "", "do x", "")
	if err := l.Record(rtEntry(RejectionKindDenied, rtID1, repo, instr, "", "aaa")); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(rtEntry(RejectionKindDenied, rtID2, repo, instr, "", "bbb")); err != nil {
		t.Fatal(err)
	}
	if n, ids := l.Denials(repo, instr, ""); n != 2 || len(ids) != 2 || ids[0] != rtID2 {
		t.Fatalf("denials=%d ids=%v, want 2 newest-first", n, ids)
	}
	if id, ok := l.DeniedDiff(repo, "aaa"); !ok || id != rtID1 {
		t.Fatalf("denied diff aaa not indexed: %q %v", id, ok)
	}
	// Reload from disk: the index must rebuild identically.
	l2, err := OpenRejectionLedger(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if n, _ := l2.Denials(repo, instr, ""); n != 2 {
		t.Fatalf("reloaded denials=%d, want 2", n)
	}
	// An approval resets the key's count and frees its own diff hash only.
	if err := l2.Record(rtEntry(RejectionKindApproved, rtID3, repo, instr, "", "aaa")); err != nil {
		t.Fatal(err)
	}
	if n, ids := l2.Denials(repo, instr, ""); n != 0 || len(ids) != 0 {
		t.Fatalf("after approval denials=%d ids=%v, want 0", n, ids)
	}
	if _, ok := l2.DeniedDiff(repo, "aaa"); ok {
		t.Fatal("approved diff aaa still indexed as denied")
	}
	if _, ok := l2.DeniedDiff(repo, "bbb"); !ok {
		t.Fatal("approval of aaa also freed bbb; it must free only its own hash")
	}
	if fi, err := os.Stat(RejectionLedgerPath(root)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("ledger file mode: %v %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Dir(RejectionLedgerPath(root))); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("ledger dir mode: %v %v", fi, err)
	}
}

func TestRejectionLedger_IssueKeyCountsAcrossInstructions(t *testing.T) {
	l, _ := OpenRejectionLedger(t.TempDir())
	const url = "https://github.com/o/r/issues/42"
	repo, i1, issue := RejectionKeys("https://github.com/o/r.git", "", "title v1", url)
	_, i2, _ := RejectionKeys("https://github.com/o/r.git", "", "title v2 (edited by the author)", url)
	_ = l.Record(rtEntry(RejectionKindDenied, rtID1, repo, i1, url, "aaa"))
	_ = l.Record(rtEntry(RejectionKindDenied, rtID2, repo, i2, url, "bbb"))
	if n, ids := l.Denials(repo, i2, issue); n != 2 || len(ids) != 2 {
		t.Fatalf("issue key did not accumulate across edited instructions: n=%d ids=%v", n, ids)
	}
	if n, _ := l.Denials(repo, i2, ""); n != 1 {
		t.Fatalf("instruction key alone should be 1, got %d", n)
	}
	// An approval on the issue resets both keys.
	_ = l.Record(rtEntry(RejectionKindApproved, rtID3, repo, i2, url, "bbb"))
	if n, _ := l.Denials(repo, i2, issue); n != 0 {
		t.Fatalf("after approval of i2, Denials(repo, i2, issue)=%d, want 0", n)
	}
	if n, _ := l.Denials(repo, i1, issue); n != 1 {
		t.Fatalf("after approval of i2, Denials(repo, i1, issue)=%d, want 1 (i1 never approved; max(1, 0) = 1)", n)
	}
}

func TestRejectionLedger_DenialsReturnsMaxOfBothKeys(t *testing.T) {
	l, _ := OpenRejectionLedger(t.TempDir())
	const url = "https://github.com/o/r/issues/99"
	repo, i1, issue := RejectionKeys("https://github.com/o/r.git", "", "instruction v1", url)
	// Add denial to instruction key only.
	_ = l.Record(rtEntry(RejectionKindDenied, rtID1, repo, i1, "", "d1"))
	// Add denial to issue key only.
	_ = l.Record(rtEntry(RejectionKindDenied, rtID2, repo, i1, url, "d2"))
	// After two denials under same instruction but one with issue:
	// i1 alone: 2 denials (both affect instruction key)
	// i1+issue: max(2, 1) = 2
	if n, _ := l.Denials(repo, i1, ""); n != 2 {
		t.Fatalf("instruction key: %d denials, want 2", n)
	}
	if n, _ := l.Denials(repo, i1, issue); n != 2 {
		t.Fatalf("max of i1 and issue: %d, want 2", n)
	}
	// Now approve the instruction key only.
	_ = l.Record(rtEntry(RejectionKindApproved, rtID3, repo, i1, "", "d1"))
	// i1 alone: 0 (approved)
	// issue alone: 1 (second denial still pending)
	// i1+issue: max(0, 1) = 1 (issue key takes higher value)
	if n, _ := l.Denials(repo, i1, ""); n != 0 {
		t.Fatalf("after approval of i1, instruction key=%d, want 0", n)
	}
	if n, _ := l.Denials(repo, i1, issue); n != 1 {
		t.Fatalf("after approval of i1, max(0, 1)=%d, want 1", n)
	}
}

func TestRejectionLedger_TornTailDroppedMalformedMiddleDegrades(t *testing.T) {
	root := t.TempDir()
	path := RejectionLedgerPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	good := `{"kind":"denied","at_ms":1,"task_id":"` + rtID1 + `","repo_key":"github.com/o/r","instruction_sha256":"` + strings.Repeat("a", 64) + `","issue_url":"","diff_sha256":"d","path":"live"}` + "\n"
	// Torn tail: a crash mid-append. Dropped with a warning, not degraded.
	if err := os.WriteFile(path, []byte(good+`{"kind":"den`), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := OpenRejectionLedger(root)
	if err != nil || l.LoadError() != "" {
		t.Fatalf("torn tail must not degrade: err=%v load=%q", err, l.LoadError())
	}
	if n, _ := l.Denials("github.com/o/r", strings.Repeat("a", 64), ""); n != 1 {
		t.Fatalf("denials=%d, want 1 from the intact line", n)
	}
	// A malformed MIDDLE line is not a torn tail: degraded, naming the line.
	if err := os.WriteFile(path, []byte(`not json`+"\n"+good), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err = OpenRejectionLedger(root)
	if err == nil || l == nil || !strings.Contains(l.LoadError(), "line 1") {
		t.Fatalf("malformed middle line must degrade naming the line: err=%v load=%q", err, l.LoadError())
	}
	// A bad task id (not 32 hex) degrades too, and never reaches the index.
	bad := strings.Replace(good, rtID1, "../../etc/passwd", 1)
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err = OpenRejectionLedger(root)
	if err == nil || l.LoadError() == "" {
		t.Fatal("bad task_id must degrade the ledger")
	}
	if _, ok := l.DeniedDiff("github.com/o/r", "d"); ok {
		t.Fatal("a line with a bad task_id was indexed")
	}
	if strings.Contains(l.LoadError(), root) {
		t.Fatalf("load error leaks the audit path: %q", l.LoadError())
	}
}

func TestRejectionLedger_RecordValidatesAndAppendsOneLine(t *testing.T) {
	root := t.TempDir()
	l, _ := OpenRejectionLedger(root)
	if err := l.Record(rtEntry("maybe", rtID1, "r", "i", "", "d")); err == nil {
		t.Fatal("Record accepted an unknown kind")
	}
	if err := l.Record(rtEntry(RejectionKindDenied, "short", "r", "i", "", "d")); err == nil {
		t.Fatal("Record accepted a malformed task id")
	}
	if err := l.Record(rtEntry(RejectionKindDenied, rtID1, "r", "i", "", "d")); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(RejectionLedgerPath(root))
	if lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n"); len(lines) != 1 {
		t.Fatalf("ledger has %d lines, want 1: %q", len(lines), data)
	}
}

func TestRejectionLedger_MissingFileIsEmptyNotDegraded(t *testing.T) {
	l, err := OpenRejectionLedger(t.TempDir())
	if err != nil || l.LoadError() != "" {
		t.Fatalf("missing ledger must open empty: %v %q", err, l.LoadError())
	}
	if n, _ := l.Denials("r", "i", ""); n != 0 {
		t.Fatal("empty ledger reports denials")
	}
}

// Review Focus 5: the ledger is invisible to every audit-root *.jsonl consumer.
// TerminateStuckAudits must not append an interrupted line into it, and the
// audit listing the CLIs share must not report a task named "ledger".
func TestRejectionLedger_InvisibleToAuditConsumers(t *testing.T) {
	root := t.TempDir()
	l, _ := OpenRejectionLedger(root)
	if err := l.Record(rtEntry(RejectionKindDenied, rtID1, "github.com/o/r", strings.Repeat("a", 64), "", "d")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(RejectionLedgerPath(root))
	if _, err := TerminateStuckAudits(root); err != nil {
		t.Fatalf("TerminateStuckAudits: %v", err)
	}
	after, _ := os.ReadFile(RejectionLedgerPath(root))
	if string(before) != string(after) {
		t.Fatalf("TerminateStuckAudits modified the rejection ledger:\n%s", after)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			t.Fatalf("a .jsonl landed at the audit root: %s", e.Name())
		}
	}
	// Reopen: still one clean entry, not degraded.
	l2, err := OpenRejectionLedger(root)
	if err != nil || l2.LoadError() != "" {
		t.Fatalf("ledger degraded after the boot sweep: %v %q", err, l2.LoadError())
	}
}

// TestRejectionLedger_TornFragmentFixedBeforeAppend verifies that when a
// previous Record was torn (an unparseable fragment on disk without trailing
// newline), the next Record truncates the file back to the last complete line
// before appending, so the fragment cannot fuse with the new line.
func TestRejectionLedger_TornFragmentFixedBeforeAppend(t *testing.T) {
	root := t.TempDir()
	path := RejectionLedgerPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	good := `{"kind":"denied","at_ms":1,"task_id":"` + rtID1 + `","repo_key":"github.com/o/r","instruction_sha256":"` + strings.Repeat("a", 64) + `","issue_url":"","diff_sha256":"d1","path":"live"}` + "\n"
	// Write good line plus a torn fragment (no trailing newline).
	torn := `{"kind":"denied","at_ms":2,"task_id":"` + rtID2
	if err := os.WriteFile(path, []byte(good+torn), 0o600); err != nil {
		t.Fatal(err)
	}
	// Open: torn line dropped.
	l, err := OpenRejectionLedger(root)
	if err != nil || l.LoadError() != "" {
		t.Fatalf("torn tail must not degrade: err=%v load=%q", err, l.LoadError())
	}
	if n, _ := l.Denials("github.com/o/r", strings.Repeat("a", 64), ""); n != 1 {
		t.Fatalf("denials=%d, want 1 from the intact line", n)
	}
	// Record a new entry. Record must detect the missing newline and fix it.
	if err := l.Record(rtEntry(RejectionKindDenied, rtID3, "github.com/o/r", strings.Repeat("b", 64), "", "d2")); err != nil {
		t.Fatal(err)
	}
	// Reopen: both entries indexed, no degradation.
	l2, err := OpenRejectionLedger(root)
	if err != nil || l2.LoadError() != "" {
		t.Fatalf("ledger degraded after recovery: err=%v load=%q", err, l2.LoadError())
	}
	if n, _ := l2.Denials("github.com/o/r", strings.Repeat("a", 64), ""); n != 1 {
		t.Fatalf("original entry lost: %d denials", n)
	}
	if n, _ := l2.Denials("github.com/o/r", strings.Repeat("b", 64), ""); n != 1 {
		t.Fatalf("new entry lost: %d denials", n)
	}
}

func rtLine(t *testing.T, e RejectionEntry) string {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A newline-less tail that parses and validates is a real verdict: Record
// must keep it (write a separating newline) rather than truncate it.
func TestRejectionLedger_ParseableNewlinelessTailSurvives(t *testing.T) {
	root := t.TempDir()
	path := RejectionLedgerPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	body := rtLine(t, rtEntry(RejectionKindDenied, rtID1, "github.com/o/r", a, "", "d1")) + "\n" +
		rtLine(t, rtEntry(RejectionKindDenied, rtID2, "github.com/o/r", a, "", "d2")) // no newline
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := OpenRejectionLedger(root)
	if err != nil || l.LoadError() != "" {
		t.Fatalf("open: %v %q", err, l.LoadError())
	}
	if n, _ := l.Denials("github.com/o/r", a, ""); n != 2 {
		t.Fatalf("denials=%d, want 2", n)
	}
	if err := l.Record(rtEntry(RejectionKindDenied, rtID3, "github.com/o/r", b, "", "d3")); err != nil {
		t.Fatal(err)
	}
	l2, err := OpenRejectionLedger(root)
	if err != nil || l2.LoadError() != "" {
		t.Fatalf("reopen degraded: %v %q", err, l2.LoadError())
	}
	if n, _ := l2.Denials("github.com/o/r", a, ""); n != 2 {
		t.Fatalf("after reopen a-denials=%d, want 2 (tail verdict lost)", n)
	}
	if n, _ := l2.Denials("github.com/o/r", b, ""); n != 1 {
		t.Fatalf("after reopen b-denials=%d, want 1", n)
	}
}

// A read error during the torn-tail repair must write nothing and must not
// truncate the ledger.
func TestRejectionLedger_RepairReadErrorWritesNothing(t *testing.T) {
	root := t.TempDir()
	l, _ := OpenRejectionLedger(root)
	path := RejectionLedgerPath(root)
	a := strings.Repeat("a", 64)
	body := rtLine(t, rtEntry(RejectionKindDenied, rtID1, "github.com/o/r", a, "", "d1")) + "\n" + `{"kind":"den`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Skip("cannot chmod:", err)
	}
	defer os.Chmod(path, 0o600)
	if f, err := os.Open(path); err == nil {
		f.Close()
		t.Skip("file still readable (running as root?)")
	}
	if err := l.Record(rtEntry(RejectionKindDenied, rtID2, "github.com/o/r", a, "", "d2")); err == nil {
		t.Fatal("Record must return the read error")
	}
	os.Chmod(path, 0o600)
	got, _ := os.ReadFile(path)
	if string(got) != body {
		t.Fatalf("ledger modified after a failed repair read: %q", got)
	}
	if n, _ := l.Denials("github.com/o/r", a, ""); n != 0 {
		t.Fatalf("index counted a verdict that was not written: %d", n)
	}
}

// A newline-terminated garbage last line is not a torn append: it degrades.
func TestRejectionLedger_NewlineTerminatedGarbageTailDegrades(t *testing.T) {
	root := t.TempDir()
	path := RejectionLedgerPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	good := rtLine(t, rtEntry(RejectionKindDenied, rtID1, "github.com/o/r", strings.Repeat("a", 64), "", "d1"))
	if err := os.WriteFile(path, []byte(good+"\nnot json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := OpenRejectionLedger(root)
	if err == nil || !strings.Contains(l.LoadError(), "line 2") {
		t.Fatalf("want degraded naming line 2, got err=%v load=%q", err, l.LoadError())
	}
}

func TestRejectionKeys_IssueURLCanonicalized(t *testing.T) {
	spell := []string{
		"github.com/o/r/issues/42",
		"https://github.com/O/R/issues/42/",
		"https://github.com/o/r/issues/42#issuecomment-1",
		"  https://www.github.com/o/R/issues/42  ",
	}
	_, _, want := RejectionKeys("https://github.com/o/r.git", "", "x", spell[0])
	if want == "" {
		t.Fatal("canonical spelling produced no issue key")
	}
	for _, s := range spell[1:] {
		if _, _, k := RejectionKeys("https://github.com/o/r.git", "", "x", s); k != want {
			t.Errorf("%q: key %q, want %q", s, k, want)
		}
	}
	for _, bad := range []string{"#abc123", "https://gitlab.com/o/r/issues/1", "junk", ""} {
		if _, _, k := RejectionKeys("https://github.com/o/r.git", "", "x", bad); k != "" {
			t.Errorf("%q: issue key %q, want none", bad, k)
		}
	}
}
