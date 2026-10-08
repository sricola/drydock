# Rejection-Loop Detection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give brokerd a durable memory of human diff-gate verdicts and use it to refuse a queue add for work denied `max_denials` times (before any spend) and to auto-deny a queued task whose diff is identical to one already denied.

**Architecture:** A new append-only ledger `<audit_root>/rejections/ledger.jsonl` (its own subdirectory, so the audit-root `*.jsonl` consumers never see it) is written by every human diff-gate verdict (live, queued, resumed) and loaded at boot into an in-memory index keyed on repo+instruction-hash and, for issue-driven work, repo+issue-URL. `Enqueue`, the dispatcher, and the CI-retry decision consult the index (the identity guard); `pushAndOpenPR` consults it before the brief is written (the same-diff backstop). The synchronous `POST /tasks` path never consults the guard and only surfaces a diff match to the reviewer.

**Tech Stack:** Go stdlib only (no new module dependencies). Existing idioms: `queuestore.go` for durable files, `globalledger.go` for the fail-closed ledger stance and subdirectory layout, `ciretryloop.go` for broker-authored reason strings and park-on-fault.

**Spec:** `docs/superpowers/specs/2026-10-08-rejection-loop-detection-design.md`

## Global Constraints

- Go `1.27.1` (`go.mod`); no new module dependencies.
- Every ledger field is broker-authored: no instruction text, no diff text, no agent text, no CI text, ever. `issue_url` is the operator CLI's value, copied as-is.
- Agent-produced text never decides a state transition (repo-wide rule). The only inputs here are gate causes, hashes, URLs the operator supplied, and ids.
- Only human verdicts move the count: `gateDenied` increments, a human `gateApproved` (not auto-approve) resets. The backstop's auto-deny writes nothing.
- No em dashes (`—`) in any new doc, YAML comment, or commit message. Code comments follow the surrounding file.
- `drydock submit` and `drydock retry` never consult the identity guard. There is no override flag.
- `max_denials` default `2`, range `0..10`; `0` means off even with a degraded ledger (no 503, no park, no auto-deny), but the ledger is still written and the sync-path warning still shows.
- A degraded ledger with `max_denials > 0` fails closed on the queue path only: `Enqueue` refuses (503), the dispatcher parks (one log line per process), the CI-retry decision parks, the backstop looks nothing up.
- The queue state machine stays forward-only and acyclic (`TestQueueStateMachineIsForwardOnly` must keep passing).
- Test names use `TestRejectionLoop_*` / `TestRejectionLedger_*`, never `TestRedteam_*` (reserved for A-claims).
- Docs live in `site/docs/*.md`; the `.html` files are gitignored. `CHANGELOG.md` is stamped at release time, not here.

## Review Focus

1. **A `root_instruction` on a synchronous `POST /tasks` body** (or forwarded by the web UI). The sync path must key on `instruction` alone. Test in Task 3.
2. **A denial recorded while the same key's next item is already dispatched.** That item runs to its own gate; the guard must not kill a running task. Test in Task 4.
3. **An auto-approved push (sync or queued) at count 1 or 2.** It must not reset the count or free a denied hash. Test in Task 3.
4. **A truncated diff capture.** Two different large changes sharing a prefix hash equal; the backstop must not auto-deny a truncated diff. Test in Task 5.
5. **A ledger file present at the audit root's subdirectory while the boot sweep and the CLI listings run.** None of them may parse or append to it. Test in Task 1.

---

### Task 1: The rejection ledger store and index

**Files:**
- Create: `internal/broker/rejections.go`
- Create: `internal/broker/rejections_test.go`
- Modify: `internal/trustbrief/difffacts.go:118-121` (extract `HashDiff`)

**Interfaces:**
- Consumes: `repokey.Normalize(string) string`, `trustbrief.HashInstruction(string) string`, `queueIDRE` (queuestore.go), `pathFreeErr` (globalledger.go), `TerminateStuckAudits` (reconcile.go).
- Produces:
  - `func RejectionKeys(repoRef, rootInstruction, instruction, issueURL string) (repoKey, instructionSHA, issueKey string)` (`issueKey == ""` when `issueURL == ""`)
  - `type RejectionEntry struct{ Kind string; AtMs int64; TaskID, RepoKey, InstructionSHA256, IssueURL, DiffSHA256, Path string }`
  - `const RejectionKindDenied = "denied"`, `RejectionKindApproved = "approved"`
  - `func RejectionLedgerPath(auditRoot string) string` = `<auditRoot>/rejections/ledger.jsonl`
  - `func OpenRejectionLedger(auditRoot string) (*RejectionLedger, error)` (non-nil ledger even on error)
  - `func (l *RejectionLedger) LoadError() string`
  - `func (l *RejectionLedger) Record(e RejectionEntry) error`
  - `func (l *RejectionLedger) Denials(repoKey, instructionSHA, issueKey string) (n int, deniedIDs []string)` (max of the two keys; ids merged newest first, deduped)
  - `func (l *RejectionLedger) DeniedDiff(repoKey, diffSHA string) (taskID string, ok bool)`
  - `func trustbrief.HashDiff(diff string) string`

- [ ] **Step 1: Extract `HashDiff` in trustbrief**

In `internal/trustbrief/difffacts.go`, change the first two lines of `Analyze` to:

```go
func Analyze(diff string) DiffFacts {
	facts := DiffFacts{SHA256: HashDiff(diff), Bytes: len(diff), Files: []FileChange{}, Flags: []Flag{}}
```

and add above `Analyze`:

```go
// HashDiff returns the hex SHA-256 of a unified diff: the exact value Analyze
// records as DiffFacts.SHA256, for callers that need the hash without the
// structural pass (the rejection ledger keys denied diffs on it).
func HashDiff(diff string) string {
	sum := sha256.Sum256([]byte(diff))
	return hex.EncodeToString(sum[:])
}
```

Run: `go test ./internal/trustbrief/`
Expected: PASS (behavior unchanged).

- [ ] **Step 2: Write the failing ledger tests**

Create `internal/broker/rejections_test.go`:

```go
package broker

import (
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
	if n, _ := l.Denials(repo, i1, issue); n != 0 {
		t.Fatalf("approval did not reset the issue key: %d", n)
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
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/broker/ -run 'TestRejection' -v`
Expected: FAIL with "undefined: RejectionKeys" and friends.

- [ ] **Step 4: Implement the ledger**

Create `internal/broker/rejections.go`:

```go
package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"drydock/internal/repokey"
	"drydock/internal/trustbrief"
)

// This file is the REJECTION LEDGER (spec: docs/superpowers/specs/
// 2026-10-08-rejection-loop-detection-design.md): the durable, append-only
// record of HUMAN diff-gate verdicts that the rejection-loop guards are
// enforced against, plus the typed refusals and the guard itself. Who writes
// a verdict (recordGateVerdict, broker.go), who consults the guard (Enqueue,
// takeDispatchable, maybeEnqueueCIRetry) and who auto-denies a repeat diff
// (pushAndOpenPR) live beside their callers.
//
// Every field is broker-authored: a gate cause, two hashes, a canonical repo
// key, the operator-supplied issue URL, a task id, a clock reading. No
// instruction text, no diff text, nothing an agent or an issue author wrote.
//
// LAYOUT. The file lives in its OWN SUBDIRECTORY, <audit_root>/rejections/,
// for the reason globalledger.go gives for <audit_root>/global/: five
// consumers glob *.jsonl at the audit root and parse each hit as a task
// trace, and TerminateStuckAudits would APPEND an interrupted result line into
// any root-level file without a broker result row, which would degrade this
// ledger on the next boot. All of them skip directories.
//
// FAILURE DIRECTION. A torn trailing line (a crash mid-append) is dropped. Any
// other unreadable or invalid line, or an unreadable file, marks the ledger
// DEGRADED (LoadError names the line): the guards then refuse rather than
// admit, matching globalledger.go's stance, but ONLY while max_denials > 0.
// A failed append is the caller's to log; a lost verdict can only under-count.

const (
	rejectionLedgerDirName  = "rejections"
	rejectionLedgerFileName = "ledger.jsonl"
	rejectionLedgerMaxBytes = 64 << 20
	// rejectionDeniedIDsKeep bounds the per-key id list kept for display.
	rejectionDeniedIDsKeep = 10

	RejectionKindDenied   = "denied"
	RejectionKindApproved = "approved"
)

// RejectionEntry is one ledger line.
type RejectionEntry struct {
	Kind              string `json:"kind"`
	AtMs              int64  `json:"at_ms"`
	TaskID            string `json:"task_id"`
	RepoKey           string `json:"repo_key"`
	InstructionSHA256 string `json:"instruction_sha256"`
	IssueURL          string `json:"issue_url"`
	DiffSHA256        string `json:"diff_sha256"`
	Path              string `json:"path"`
}

func (e RejectionEntry) validate() error {
	if e.Kind != RejectionKindDenied && e.Kind != RejectionKindApproved {
		return fmt.Errorf("unknown kind %q", e.Kind)
	}
	if !queueIDRE.MatchString(e.TaskID) {
		return errors.New("task_id is not a 32-hex task id")
	}
	if e.RepoKey == "" || e.InstructionSHA256 == "" {
		return errors.New("empty repo_key or instruction_sha256")
	}
	if len(e.IssueURL) > 2048 || strings.ContainsAny(e.IssueURL, "\n\r") {
		return errors.New("issue_url is not a single-line url")
	}
	return nil
}

// RejectionKeys derives the two identities a verdict is counted under.
//
//   - repoKey: repokey.Normalize with the PATH lowercased too (the hosts
//     drydock targets are case-insensitive, and feeder spelling drift must
//     not reset a count).
//   - instructionSHA: the hash of the ORIGINAL instruction, rootInstruction
//     when set (a CI retry child; the broker is that field's only writer),
//     else instruction.
//   - issueKey: repoKey + "\n" + issueURL, or "" when there is no issue. The
//     URL is the operator's --issue value; the issue AUTHOR cannot change it
//     by editing the issue, which is the whole point of the second key.
func RejectionKeys(repoRef, rootInstruction, instruction, issueURL string) (repoKey, instructionSHA, issueKey string) {
	root := rootInstruction
	if root == "" {
		root = instruction
	}
	repoKey = strings.ToLower(repokey.Normalize(repoRef))
	instructionSHA = trustbrief.HashInstruction(root)
	if u := strings.TrimSpace(issueURL); u != "" {
		issueKey = repoKey + "\n" + u
	}
	return repoKey, instructionSHA, issueKey
}

type rejectionKeyState struct {
	denials   int
	deniedIDs []string // newest first, capped at rejectionDeniedIDsKeep
}

// RejectionLedger is the store plus its index. Safe for concurrent use; it
// has its own mutex and never takes queueMu.
type RejectionLedger struct {
	path    string
	mu      sync.Mutex
	loadErr string
	byKey   map[string]*rejectionKeyState // instruction keys AND issue keys
	diffs   map[string]map[string]string  // repoKey -> diffSHA -> task id
}

func RejectionLedgerPath(auditRoot string) string {
	return filepath.Join(auditRoot, rejectionLedgerDirName, rejectionLedgerFileName)
}

func instructionKey(repoKey, instructionSHA string) string { return repoKey + "\n#" + instructionSHA }

// OpenRejectionLedger loads the ledger under auditRoot. It ALWAYS returns a
// usable ledger; a non-nil error means it is degraded (LoadError says why,
// path-free) and the guards must fail closed while max_denials > 0.
func OpenRejectionLedger(auditRoot string) (*RejectionLedger, error) {
	l := &RejectionLedger{
		path:  RejectionLedgerPath(auditRoot),
		byKey: map[string]*rejectionKeyState{},
		diffs: map[string]map[string]string{},
	}
	if auditRoot == "" {
		l.loadErr = "empty audit root"
		return l, errors.New("rejection ledger: empty audit root")
	}
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		l.loadErr = "the ledger directory is unavailable: " + pathFreeErr(err)
		return l, fmt.Errorf("rejection ledger: %w", err)
	}
	if fi, err := os.Stat(dir); err == nil && fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			slog.Warn("rejection ledger: could not tighten the ledger directory to 0700", "path", dir, "err", err)
		}
	}
	f, err := os.OpenFile(l.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		l.loadErr = "cannot open: " + pathFreeErr(err)
		return l, fmt.Errorf("rejection ledger: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, rejectionLedgerMaxBytes+1))
	if err != nil {
		l.loadErr = "cannot read: " + pathFreeErr(err)
		return l, fmt.Errorf("rejection ledger: %w", err)
	}
	if len(data) > rejectionLedgerMaxBytes {
		l.loadErr = fmt.Sprintf("exceeds %d bytes", rejectionLedgerMaxBytes)
		return l, errors.New("rejection ledger: " + l.loadErr)
	}
	lines := bytes.Split(data, []byte("\n"))
	// Index of the last non-empty line: the only line a crash can tear.
	last := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if len(bytes.TrimSpace(lines[i])) > 0 {
			last = i
			break
		}
	}
	for i, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var e RejectionEntry
		if err := json.Unmarshal(line, &e); err != nil {
			if i == last {
				slog.Warn("rejection ledger: dropping a torn trailing line (crash mid-append)", "path", l.path)
				continue
			}
			l.loadErr = fmt.Sprintf("line %d is not a ledger entry; fix or remove it and restart brokerd", i+1)
			return l, errors.New("rejection ledger: " + l.loadErr)
		}
		if err := e.validate(); err != nil {
			l.loadErr = fmt.Sprintf("line %d: %s; fix or remove it and restart brokerd", i+1, err.Error())
			return l, errors.New("rejection ledger: " + l.loadErr)
		}
		l.applyLocked(e)
	}
	return l, nil
}

// LoadError is "" for a healthy ledger, else the path-free reason it is
// degraded (rendered into HTTP bodies and operator terminals).
func (l *RejectionLedger) LoadError() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.loadErr
}

// Record appends e (O_APPEND + fsync, 0600, O_NOFOLLOW) and updates the index
// only once the line is durably on disk, so a failed append leaves memory and
// disk agreeing that the verdict was lost.
func (l *RejectionLedger) Record(e RejectionEntry) error {
	if err := e.validate(); err != nil {
		return fmt.Errorf("rejection ledger: %w", err)
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	l.applyLocked(e)
	return nil
}

func (l *RejectionLedger) applyLocked(e RejectionEntry) {
	keys := []string{instructionKey(e.RepoKey, e.InstructionSHA256)}
	if u := strings.TrimSpace(e.IssueURL); u != "" {
		keys = append(keys, e.RepoKey+"\n"+u)
	}
	for _, k := range keys {
		st := l.byKey[k]
		if st == nil {
			st = &rejectionKeyState{}
			l.byKey[k] = st
		}
		switch e.Kind {
		case RejectionKindDenied:
			st.denials++
			st.deniedIDs = append([]string{e.TaskID}, st.deniedIDs...)
			if len(st.deniedIDs) > rejectionDeniedIDsKeep {
				st.deniedIDs = st.deniedIDs[:rejectionDeniedIDsKeep]
			}
		case RejectionKindApproved:
			st.denials = 0
			st.deniedIDs = nil
		}
	}
	switch e.Kind {
	case RejectionKindDenied:
		if e.DiffSHA256 != "" {
			m := l.diffs[e.RepoKey]
			if m == nil {
				m = map[string]string{}
				l.diffs[e.RepoKey] = m
			}
			m[e.DiffSHA256] = e.TaskID
		}
	case RejectionKindApproved:
		if m := l.diffs[e.RepoKey]; m != nil {
			delete(m, e.DiffSHA256)
		}
	}
}

// Denials reports the LARGER of the instruction key's and the issue key's
// counts since their last approval, with the denied task ids of both merged
// newest first (deduped, capped for display). issueKey may be "".
func (l *RejectionLedger) Denials(repoKey, instructionSHA, issueKey string) (int, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	var ids []string
	seen := map[string]bool{}
	for _, k := range []string{instructionKey(repoKey, instructionSHA), issueKey} {
		if k == "" {
			continue
		}
		st := l.byKey[k]
		if st == nil {
			continue
		}
		if st.denials > n {
			n = st.denials
		}
		for _, id := range st.deniedIDs {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if len(ids) > rejectionDeniedIDsKeep {
		ids = ids[:rejectionDeniedIDsKeep]
	}
	return n, ids
}

// DeniedDiff reports the task whose denied diff hashed to diffSHA for repoKey.
func (l *RejectionLedger) DeniedDiff(repoKey, diffSHA string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	id, ok := l.diffs[repoKey][diffSHA]
	return id, ok
}
```

`pathFreeErr` already exists in `globalledger.go` (same package).

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race ./internal/broker/ -run 'TestRejection' -v`
Expected: PASS (8 tests).

- [ ] **Step 6: Commit**

```bash
git add internal/broker/rejections.go internal/broker/rejections_test.go internal/trustbrief/difffacts.go
git commit -m "feat(broker): rejection ledger store and index (5C rejection-loop, task 1)"
```

---

### Task 2: The `queue.max_denials` config knob

**Files:**
- Modify: `internal/config/config.go` (struct after `CI CIConfig` at line 309; `Defaults()` at 369; env overrides after the `DRYDOCK_CI_MAX_ATTEMPTS` block at ~641; `validate` after the `ci.max_attempts` checks at ~859)
- Modify: `internal/config/explain.go:411` (add a row after `CI.MaxAttempts`)
- Modify: `internal/config/config_test.go:142` (`TestEnvOverrides_AllOperatorKnobs`), `internal/config/ci_test.go:260` (provenance table), `internal/config/explain_test.go:85` (env list)
- Create: `internal/config/queue_test.go`
- Modify: `config/config.yaml` (new block after the `ci:` block, before `# --- Where state lives ---`); `TestLoad_ShippedConfigLoadsStrict` (knownfields_test.go) covers it.

**Interfaces:**
- Produces: `config.QueueConfig{MaxDenials int}`, `Config.Queue QueueConfig`, `config.DefaultQueueMaxDenials = 2`, `config.MaxQueueMaxDenials = 10`, env `DRYDOCK_QUEUE_MAX_DENIALS`.

- [ ] **Step 1: Write the failing config tests**

Create `internal/config/queue_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeQueueCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestQueueMaxDenials_DefaultIsTwo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Queue.MaxDenials != 2 {
		t.Fatalf("default queue.max_denials=%d, want 2", c.Queue.MaxDenials)
	}
}

func TestQueueMaxDenials_YAMLAndEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := writeQueueCfg(t, "queue:\n  max_denials: 4\n")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Queue.MaxDenials != 4 {
		t.Fatalf("yaml queue.max_denials=%d, want 4", c.Queue.MaxDenials)
	}
	t.Setenv("DRYDOCK_QUEUE_MAX_DENIALS", "0")
	c, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Queue.MaxDenials != 0 {
		t.Fatalf("env override not applied: %d", c.Queue.MaxDenials)
	}
	t.Setenv("DRYDOCK_QUEUE_MAX_DENIALS", "-3")
	c, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Queue.MaxDenials != 4 {
		t.Fatalf("negative env must be ignored, got %d", c.Queue.MaxDenials)
	}
}

func TestQueueMaxDenials_ValidateBounds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, bad := range []string{"-1", "11"} {
		_, err := Load(writeQueueCfg(t, "queue:\n  max_denials: "+bad+"\n"))
		if err == nil || !strings.Contains(err.Error(), "queue.max_denials") {
			t.Errorf("max_denials=%s: want a queue.max_denials validation error, got %v", bad, err)
		}
	}
	if _, err := Load(writeQueueCfg(t, "queue:\n  max_denials: 10\n")); err != nil {
		t.Errorf("max_denials=10 must be accepted: %v", err)
	}
}

func TestQueueMaxDenials_ExplainRow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DRYDOCK_QUEUE_MAX_DENIALS", "5")
	fields, _, err := Explain(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fields {
		if f.YAMLKey == "queue.max_denials" {
			if f.Value != "5" || f.Source != SourceEnv || f.EnvVar != "DRYDOCK_QUEUE_MAX_DENIALS" {
				t.Fatalf("explain row: %+v", f)
			}
			return
		}
	}
	t.Fatal("policy explain has no queue.max_denials row")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestQueueMaxDenials' -v`
Expected: FAIL with "c.Queue undefined".

- [ ] **Step 3: Implement the knob**

In `internal/config/config.go`, after the `CIConfig` type (ends around line 147) add:

```go
// QueueConfig is the `queue:` block: knobs that apply only to the durable
// queue path (POST /queue, drydock queue add), never to a synchronous submit.
type QueueConfig struct {
	// MaxDenials is the rejection-loop bound: a queue add whose canonical
	// repo + original instruction (or repo + issue URL) has been DENIED by a
	// human at the diff gate this many times since its last human approval
	// is refused (HTTP 409) before any VM boots, and a queued task whose diff
	// is identical to one already denied for the repo is auto-denied before
	// the gate. 0 turns both off, including the fail-closed refusal on an
	// unreadable ledger; the ledger is still written. Max 10.
	MaxDenials int `yaml:"max_denials"`
}

const (
	// DefaultQueueMaxDenials deliberately ships ON (Phase 5's opt-in rule
	// is broken here on purpose): it fires only after two human denials of
	// the same work on the unattended path.
	DefaultQueueMaxDenials = 2
	MaxQueueMaxDenials     = 10
)
```

In the `Config` struct, directly after `CI CIConfig \`yaml:"ci"\``:

```go
	Queue QueueConfig `yaml:"queue"`
```

In `Defaults()`, directly after the `CI: CIConfig{...},` literal:

```go
		Queue: QueueConfig{MaxDenials: DefaultQueueMaxDenials},
```

In the env-override function, directly after the `DRYDOCK_CI_MAX_ATTEMPTS` block:

```go
	if v := os.Getenv("DRYDOCK_QUEUE_MAX_DENIALS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Queue.MaxDenials = n
		}
	}
```

In `(*Config).validate`, directly after the `ci.max_attempts` upper-bound check:

```go
	if c.Queue.MaxDenials < 0 {
		return fmt.Errorf("config: queue.max_denials must be >= 0, got %d", c.Queue.MaxDenials)
	}
	if c.Queue.MaxDenials > MaxQueueMaxDenials {
		return fmt.Errorf("config: queue.max_denials must be <= %d, got %d", MaxQueueMaxDenials, c.Queue.MaxDenials)
	}
```

In `internal/config/explain.go`, directly after the `CI.MaxAttempts` row:

```go
		{name: "Queue.MaxDenials", yamlKey: "queue.max_denials", envVar: "DRYDOCK_QUEUE_MAX_DENIALS",
			guardedEnv: envIntNonNegative("DRYDOCK_QUEUE_MAX_DENIALS"),
			value:      func(c *Config) string { return renderInt(c.Queue.MaxDenials) }},
```

Extend the three existing tables so their completeness checks keep passing:
- `config_test.go` `TestEnvOverrides_AllOperatorKnobs`: add `"DRYDOCK_QUEUE_MAX_DENIALS": "6",` to `values` and the assertion `if c.Queue.MaxDenials != 6 { t.Errorf("queue env override not applied: %+v", c) }`.
- `ci_test.go` (~line 260) provenance table: add `{"Queue.MaxDenials", "queue.max_denials", "DRYDOCK_QUEUE_MAX_DENIALS", "3", SourceEnv},` and `t.Setenv("DRYDOCK_QUEUE_MAX_DENIALS", "3")` in that test's env block.
- `explain_test.go` (~line 85) env list: add `"DRYDOCK_QUEUE_MAX_DENIALS",`.

In `config/config.yaml`, after the `ci:` block's last line and before `# --- Where state lives ---`:

```yaml

# --- Rejection-loop detection (queue path only; ON by default) ---
# brokerd keeps a ledger of HUMAN diff-gate verdicts (audit_root/rejections/
# ledger.jsonl: hashes, ids, and the issue URL only). A `drydock queue add`
# whose repo + original instruction, or repo + issue URL, has been denied
# max_denials times since its last human approval is refused (HTTP 409) before
# any VM boots; a queued task whose diff is byte-identical to one already
# denied for the repo is auto-denied before the gate. `drydock submit` never
# consults it and is the override: change the instruction, or run it by hand.
# Only `drydock deny` at the DIFF gate counts, and only `drydock approve` (not
# --auto-approve) resets. Timeouts, kills, and egress-gate denials do not count.
queue:
  max_denials: 2                  # 0 = off (the ledger is still written); max 10
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/config/`
Expected: PASS, including the three extended table tests and `TestLoad_ShippedConfigLoadsStrict`.

- [ ] **Step 5: Commit**

```bash
git add internal/config/ config/config.yaml
git commit -m "feat(config): queue.max_denials knob (5C rejection-loop, task 2)"
```

---

### Task 3: Record human gate verdicts on every diff-gate path

**Files:**
- Modify: `internal/broker/broker.go` (Broker struct near `CIMaxAttempts` ~line 261; `taskRun` struct ~line 632; `HandleTask` after the body decode ~line 769; `pushAndOpenPR` ~line 1648; `writeBrief` ~line 1466)
- Modify: `internal/broker/gates.go:168-200` (`gatePushMarked` writes the two new marker fields)
- Modify: `internal/broker/gatemarker.go:13-20`
- Modify: `internal/broker/reconcile.go:451-560` (`resumePush`)
- Modify: `internal/broker/queue.go:470-490` (`runQueued` taskRun literal)
- Modify: `internal/broker/metrics.go` and `internal/audit/audit.go:80-118`
- Modify: `internal/trustbrief/difffacts.go` (`DiffFacts`)
- Modify: `cmd/brokerd/main.go` (after `applyCIConfig(b, cfg.CI)` at line 600)
- Create: `internal/broker/rejections_gate_test.go`

**Interfaces:**
- Consumes: Task 1's ledger API, `gateCause` constants (`gateApproved`, `gateDenied`, `gateTimeout`, `gateKilled`, `gateShutdown`), `trustbrief.HashDiff`.
- Produces:
  - `Broker.Rejections *RejectionLedger`, `Broker.MaxDenials int`
  - `taskRun.rootInstruction string`, `taskRun.fromQueue bool`, `taskRun.repeatOf string`, `taskRun.autoDenied bool`
  - `func (b *Broker) recordGateVerdict(tr *taskRun, cause gateCause, diffSHA, path string)` (human verdicts only)
  - `func (tr *taskRun) gateVerdictPath() string`
  - `gateMarker.RootInstruction`, `gateMarker.IssueURL` (both `omitempty`)
  - `func (b *Broker) writeBrief(tr *taskRun, diff string, diffFacts trustbrief.DiffFacts) trustbrief.DiffFacts` (signature change: callers pass precomputed facts)
  - `trustbrief.DiffFacts.RepeatOfDenied string \`json:"repeat_of_denied,omitempty"\``
  - `audit.Metrics.RepeatOf string \`json:"repeat_of,omitempty"\``

- [ ] **Step 1: Write the failing tests**

Create `internal/broker/rejections_gate_test.go`:

```go
package broker

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"drydock/internal/trustbrief"
)

const rtRepo = "https://github.com/o/r.git"

func mustOpenRejections(t *testing.T, auditRoot string) *RejectionLedger {
	t.Helper()
	l, err := OpenRejectionLedger(auditRoot)
	if err != nil {
		t.Fatalf("open rejection ledger: %v", err)
	}
	return l
}

// runSync submits body on the synchronous path, resolves its gate with
// decide (approve/deny) once pending, and returns the task id.
func runSync(t *testing.T, b *Broker, body string, decide func(*testing.T, *Broker, string)) string {
	t.Helper()
	done := make(chan struct{})
	go func() {
		b.HandleTask(httptest.NewRecorder(), httptest.NewRequest("POST", "/tasks", strings.NewReader(body)))
		close(done)
	}()
	id := waitForPending(t, b)
	decide(t, b, id)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("HandleTask did not return")
	}
	return id
}

func TestRejectionLoop_OnlyHumanDiffDenialsCount(t *testing.T) {
	const diff = "diff --git a/x b/x\n+y\n"
	st := &fakeStage{workDir: t.TempDir(), diff: diff}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, writesResult(`{"type":"result","subtype":"success"}`))
	b.Rejections = mustOpenRejections(t, b.AuditRoot)
	b.MaxDenials = 2
	body := `{"repo_ref":"` + rtRepo + `","instruction":"do x","agent":"claude"}`

	id := runSync(t, b, body, deny)
	repo, instr, _ := RejectionKeys(rtRepo, "", "do x", "")
	if n, ids := b.Rejections.Denials(repo, instr, ""); n != 1 || len(ids) != 1 || ids[0] != id {
		t.Fatalf("after one human deny: denials=%d ids=%v", n, ids)
	}
	if got, ok := b.Rejections.DeniedDiff(repo, trustbrief.HashDiff(diff)); !ok || got != id {
		t.Fatalf("denied diff not indexed: %q %v", got, ok)
	}
	// Non-human causes record nothing.
	tr := &taskRun{b: b, id: "0123456789abcdef0123456789abcdef", repoRef: rtRepo, instruction: "do x"}
	for _, c := range []gateCause{gateTimeout, gateKilled, gateShutdown} {
		b.recordGateVerdict(tr, c, "zzz", "live")
	}
	if n, _ := b.Rejections.Denials(repo, instr, ""); n != 1 {
		t.Fatalf("a non-human cause changed the count: %d", n)
	}
	// A human approve resets the key and frees the hash.
	runSync(t, b, body, approve)
	if n, _ := b.Rejections.Denials(repo, instr, ""); n != 0 {
		t.Fatalf("human approval did not reset the key: %d", n)
	}
	if _, ok := b.Rejections.DeniedDiff(repo, trustbrief.HashDiff(diff)); ok {
		t.Fatal("human approval did not free the denied diff hash")
	}
}

// Review Focus 3: auto-approve is not a human verdict. It neither resets a
// count nor frees a denied hash, on the sync path.
func TestRejectionLoop_HumanApprovalResetsAutoApproveDoesNot(t *testing.T) {
	const diff = "diff --git a/x b/x\n+y\n"
	st := &fakeStage{workDir: t.TempDir(), diff: diff}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, writesResult(`{"type":"result","subtype":"success"}`))
	b.Rejections = mustOpenRejections(t, b.AuditRoot)
	b.MaxDenials = 2
	body := `{"repo_ref":"` + rtRepo + `","instruction":"do x","agent":"claude"}`
	runSync(t, b, body, deny)
	runSync(t, b, body, deny)
	_, _, term := submit(b, `{"repo_ref":"`+rtRepo+`","instruction":"do x","agent":"claude","auto_approve":true}`)
	if term["outcome"] != "pushed" {
		t.Fatalf("auto-approved sync push: %v", term)
	}
	repo, instr, _ := RejectionKeys(rtRepo, "", "do x", "")
	if n, _ := b.Rejections.Denials(repo, instr, ""); n != 2 {
		t.Fatalf("auto-approve reset the human count: %d", n)
	}
	if _, ok := b.Rejections.DeniedDiff(repo, trustbrief.HashDiff(diff)); !ok {
		t.Fatal("auto-approve freed a human-denied diff hash")
	}
}

// Review Focus 1: the sync path keys on `instruction` alone.
func TestRejectionLoop_SyncPathIgnoresClientRootInstruction(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, writesResult(`{"type":"result","subtype":"success"}`))
	b.Rejections = mustOpenRejections(t, b.AuditRoot)
	b.MaxDenials = 2
	runSync(t, b, `{"repo_ref":"`+rtRepo+`","instruction":"do x","root_instruction":"someone else's chain","agent":"claude"}`, deny)
	repo, own, _ := RejectionKeys(rtRepo, "", "do x", "")
	_, forged, _ := RejectionKeys(rtRepo, "", "someone else's chain", "")
	if n, _ := b.Rejections.Denials(repo, own, ""); n != 1 {
		t.Fatalf("sync deny not keyed on instruction: %d", n)
	}
	if n, _ := b.Rejections.Denials(repo, forged, ""); n != 0 {
		t.Fatalf("client root_instruction reached the key: %d", n)
	}
}

// A retry child's live write keys on its chain, and a resumed gate marker
// reproduces the same keys, with a fallback for markers from older builds.
func TestRejectionLoop_RetryChainSharesParentKey(t *testing.T) {
	const url = "https://github.com/o/r/issues/7"
	// Live write from a queued run built from QueueItem.Task.
	b := queueBroker(t, 1, writesResult(`{"type":"result","subtype":"success"}`))
	b.Rejections = mustOpenRejections(t, b.AuditRoot)
	b.MaxDenials = 2
	child := Task{RepoRef: "git@github.com:O/R", Instruction: "root text\n\n<<ci evidence>>",
		RootInstruction: "root text", IssueURL: url, RetryOf: "0123456789abcdef0123456789abcdef", Attempt: 1}
	id, err := b.Enqueue(child)
	if err != nil {
		t.Fatal(err)
	}
	b.StartDispatcher()
	defer b.StopDispatcher()
	if !waitFor(5*time.Second, func() bool {
		b.pendingMu.Lock()
		defer b.pendingMu.Unlock()
		_, ok := b.pending[id]
		return ok
	}) {
		t.Fatal("child never reached the gate")
	}
	deny(t, b, id)
	waitForQueueState(t, b, id, QueueCancelled)
	repo, instr, issue := RejectionKeys(rtRepo, "", "root text", url)
	if n, _ := b.Rejections.Denials(repo, instr, issue); n != 1 {
		t.Fatalf("child's denial did not land on the chain key: %d", n)
	}
	// Resume-path keys: new marker fields, and the old-marker fallback.
	newM := gateMarker{RepoRef: "git@github.com:o/r", Instruction: "root text\n\n<<ci evidence>>", RootInstruction: "root text", IssueURL: url}
	oldM := gateMarker{RepoRef: rtRepo, Instruction: "root text"}
	_, fromNew, issueNew := RejectionKeys(newM.RepoRef, newM.RootInstruction, newM.Instruction, newM.IssueURL)
	_, fromOld, issueOld := RejectionKeys(oldM.RepoRef, oldM.RootInstruction, oldM.Instruction, oldM.IssueURL)
	if fromNew != instr || issueNew != issue || fromOld != instr || issueOld != "" {
		t.Fatal("resume-path keys do not match the live keys")
	}
}

// The resume path records too, from the marker's fields.
func TestRejectionLoop_ResumePathRecordsDenial(t *testing.T) {
	const diff = "diff --git a/x b/x\n+y\n"
	st := &fakeStage{workDir: t.TempDir(), diff: diff}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, writesResult(`{"type":"result","subtype":"success"}`))
	b.Rejections = mustOpenRejections(t, b.AuditRoot)
	b.MaxDenials = 2
	b.reopenStage = func(string) (taskStage, error) { return st, nil }
	id := "0123456789abcdef0123456789abcdef"
	b.persistDiff(id, diff)
	if err := writeGateMarker(b.AuditRoot, id, gateMarker{RepoRef: rtRepo,
		Instruction: "root\n\nevidence", RootInstruction: "root", Agent: "claude",
		TaskStartMs: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	b.ResumeAwaiting(b.StageRoot)
	deny(t, b, waitForPending(t, b))
	repo, instr, _ := RejectionKeys(rtRepo, "", "root", "")
	if !waitFor(2*time.Second, func() bool { n, _ := b.Rejections.Denials(repo, instr, ""); return n == 1 }) {
		t.Fatal("resume-path deny was not recorded on the chain key")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/broker/ -run 'TestRejectionLoop_' -v`
Expected: FAIL with "b.Rejections undefined" / "unknown field RootInstruction".

- [ ] **Step 3: Add the fields and the recorder**

In `internal/broker/broker.go`, in the `Broker` struct directly after `CIMaxAttempts int`:

```go
	// Rejections is the durable rejection ledger (rejections.go) every HUMAN
	// diff-gate verdict is recorded to. nil disables recording and both
	// guards (tests that build a Broker literal). MaxDenials is config
	// queue.max_denials, the identity guard's bound; 0 = guards off, even
	// with a degraded ledger.
	Rejections *RejectionLedger
	MaxDenials int
```

In the `taskRun` struct directly after `issueURL string`:

```go
	// rootInstruction is the chain's ORIGINAL instruction for a CI retry child
	// (Task.RootInstruction, broker-owned); "" otherwise. The rejection keys
	// are computed from it when set, so a chain's denials land on one key.
	// Never populated from a synchronous POST /tasks body (HandleTask zeroes
	// the field exactly as HandleQueueAdd does).
	rootInstruction string
	// fromQueue marks a run dispatched by runQueued. Only the queue path is
	// subject to the rejection guards; a synchronous submit is the override.
	// Deliberately a flag of its own rather than onAwaitingReview != nil.
	fromQueue bool
	// repeatOf is the task id whose DENIED diff this task's diff is
	// byte-identical to ("" = no match, or a truncated capture that is never
	// matched). Surfaced in the brief and the awaiting_approval event on
	// every path; on the queue path with MaxDenials > 0 it also auto-denies
	// (autoDenied), which lands the queue item in dead_letter.
	repeatOf   string
	autoDenied bool
```

In `HandleTask`, directly after the `if !gitURLRef.MatchString(t.RepoRef) {...}` check, add:

```go
	// Broker-owned chain fields are zeroed here exactly as HandleQueueAdd
	// does: a synchronous body must not choose the rejection key its verdict
	// records under, nor render as a link in someone else's chain.
	t.Attempt = 0
	t.RetryOf = ""
	t.RootInstruction = ""
```

Add to `internal/broker/broker.go` (near `writeBrief`):

```go
// recordGateVerdict appends a HUMAN diff-gate verdict to the rejection ledger:
// gateDenied, or gateApproved from a real gate wait. Auto-approve, a timeout,
// a kill, and a shutdown are not verdicts and write nothing. It is called
// before the terminal result event is emitted, so a feeder reacting to the
// stream cannot enqueue ahead of the count moving. A failed append is logged
// and lost: a lost denial weakens the bound by one, a lost approval leaves a
// count one too high, and the synchronous path remains the override for both.
func (b *Broker) recordGateVerdict(tr *taskRun, cause gateCause, diffSHA, path string) {
	if b.Rejections == nil {
		return
	}
	var kind string
	switch cause {
	case gateDenied:
		kind = RejectionKindDenied
	case gateApproved:
		if tr.autoApprove {
			return // a flag, not a human
		}
		kind = RejectionKindApproved
	default:
		return
	}
	repoKey, instrSHA, _ := RejectionKeys(tr.repoRef, tr.rootInstruction, tr.instruction, tr.issueURL)
	if err := b.Rejections.Record(RejectionEntry{
		Kind: kind, AtMs: b.nowMs(), TaskID: tr.id, RepoKey: repoKey,
		InstructionSHA256: instrSHA, IssueURL: strings.TrimSpace(tr.issueURL),
		DiffSHA256: diffSHA, Path: path,
	}); err != nil {
		slog.Warn("rejection ledger: could not record a gate verdict; the rejection-loop bound is one verdict weaker",
			"task_id", tr.id, "kind", kind, "err", err)
	}
}

// gateVerdictPath labels which gate path wrote a ledger entry (display only).
func (tr *taskRun) gateVerdictPath() string {
	if tr.fromQueue {
		return "queue"
	}
	return "live"
}
```

In `pushAndOpenPR`, directly after `approved, cause := b.gatePushMarked(tr.ctx, tr, diff)` (and before anything is emitted):

```go
	b.recordGateVerdict(tr, cause, facts.SHA256, tr.gateVerdictPath())
```

In `internal/broker/gatemarker.go`, add to `gateMarker` after `Instruction string`:

```go
	// RootInstruction and IssueURL let a task resumed after a restart compute
	// the same rejection keys the live path did. omitempty: markers from older
	// builds lack them, and the resume path then keys on Instruction alone,
	// which is right for every non-retry, non-issue task.
	RootInstruction string `json:"root_instruction,omitempty"`
	IssueURL        string `json:"issue_url,omitempty"`
```

In `internal/broker/gates.go` `gatePushMarked`, in the `writeGateMarker` literal add `RootInstruction: tr.rootInstruction, IssueURL: tr.issueURL,` after `Instruction: tr.instruction,`.

In `internal/broker/reconcile.go` `resumePush`: in the `taskRun` literal add `rootInstruction: m.RootInstruction, issueURL: m.IssueURL,` after `instruction: m.Instruction,`. Then directly after the `if cause == gateShutdown { ...; return }` block that follows `ok, cause := b.gatePushMarked(ctx, tr, diff)`:

```go
	b.recordGateVerdict(tr, cause, trustbrief.HashDiff(diff), "resume")
```

In `internal/broker/queue.go` `runQueued`, in the `taskRun` literal add after `issueURL: t.IssueURL,`:

```go
		rootInstruction: t.RootInstruction,
		fromQueue:       true,
```

In `internal/trustbrief/difffacts.go` add to `DiffFacts` after `FilesOmitted`:

```go
	// RepeatOfDenied is the task id whose DENIED diff this diff is
	// byte-identical to (broker-looked-up in the rejection ledger); omitted
	// when there is no match or the capture was truncated.
	RepeatOfDenied string `json:"repeat_of_denied,omitempty"`
```

In `internal/audit/audit.go` add to `Metrics` after `WidenOutcome`:

```go
	// RepeatOf is the task id whose denied diff this task's diff matched
	// (rejection-loop backstop); omitted when it matched nothing.
	RepeatOf string `json:"repeat_of,omitempty"`
```

In `internal/broker/metrics.go` `appendMetrics`, add `RepeatOf: tr.repeatOf,` after `WidenOutcome: tr.widenOutcome,`.

Change `writeBrief`'s signature to `func (b *Broker) writeBrief(tr *taskRun, diff string, diffFacts trustbrief.DiffFacts) trustbrief.DiffFacts`, replace `Diff: trustbrief.Analyze(diff),` with `Diff: diffFacts,`, and add before the `brief := trustbrief.Brief{` literal:

```go
	diffFacts.RepeatOfDenied = tr.repeatOf
```

Update every caller: run `grep -rn "writeBrief(" internal/broker` and pass `trustbrief.Analyze(diff)` as the third argument in each (the `pushAndOpenPR` call site is rewritten in Task 5; for now pass `trustbrief.Analyze(diff)` there too).

In `cmd/brokerd/main.go`, directly after `applyCIConfig(b, cfg.CI)`:

```go
	// The rejection ledger (5C rejection-loop detection). Opened before
	// ResumeAwaiting so a gate resumed after a restart records its verdict.
	// A degraded ledger is logged here and surfaced on /healthz; with
	// queue.max_denials > 0 the guards then fail closed (503 on POST /queue,
	// dispatch parks) until the named line is repaired and brokerd restarts.
	b.MaxDenials = cfg.Queue.MaxDenials
	rejections, rerr := broker.OpenRejectionLedger(cfg.AuditRoot)
	if rerr != nil {
		slog.Warn("rejection ledger unreadable; queue adds are refused and dispatch parks until it is repaired",
			"path", broker.RejectionLedgerPath(cfg.AuditRoot), "err", rerr)
	}
	b.Rejections = rejections
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go build ./... && go test -race ./internal/broker/ -run 'TestRejectionLoop_|TestRedteam_A5|TestResume|TestHandleTask' -v`
Expected: PASS; existing resume, A5, and lifecycle tests unchanged. (The enqueue-side consequence of a non-resetting auto-approve is asserted in Task 4's `TestRejectionLoop_AutoApproveDoesNotReopenEnqueue`, once the guard exists.)

- [ ] **Step 5: Commit**

```bash
git add internal/broker internal/trustbrief internal/audit cmd/brokerd
git commit -m "feat(broker): record human diff-gate verdicts to the rejection ledger (5C, task 3)"
```

---

### Task 4: The identity guard at enqueue, dispatch, and the CI-retry decision

**Files:**
- Modify: `internal/broker/rejections.go` (errors + guard)
- Modify: `internal/broker/queue.go` (`Enqueue` at line 61; `takeDispatchable` loop at ~line 219; `dropSpendCappedRetryLocked` at line 345)
- Modify: `internal/broker/queuestore.go:70-76` (the `validTransition` comment on `queued -> dead_letter`)
- Modify: `internal/broker/admin.go:176-181` (`HandleQueueAdd` error mapping) and `admin.go:371` (`writeJSONStatus`)
- Modify: `internal/broker/ciretryloop.go` (a new gate before `markCIRetryEnqueued`, ~line 313)
- Create: `internal/broker/rejections_guard_test.go`

**Interfaces:**
- Consumes: Task 1 ledger, Task 3 `Broker.Rejections`/`MaxDenials`, `ciRetryParkExpired`, `ciRetryParkBoundMs`, `dropSpendCappedRetryLocked`.
- Produces:
  - `type RejectionLoopError struct{ RepoKey, IssueURL string; Denials, MaxDenials int; DeniedTaskIDs []string }` (implements `error`; `Hint() string`)
  - `type RejectionLedgerDegradedError struct{ Reason string }` (implements `error`)
  - `func (b *Broker) rejectionGuard(t Task) error`
  - `func (b *Broker) dropQueuedLocked(it QueueItem, why, logMsg string)`
  - `func writeJSONStatus(w http.ResponseWriter, code int, v any)`
  - HTTP: `POST /queue` returns 409 `{"error":"rejection_loop",...}` or 503 `{"error":"rejection_ledger_degraded","reason":...}`.

- [ ] **Step 1: Write the failing tests**

Create `internal/broker/rejections_guard_test.go`:

```go
package broker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func rejectionQueueBroker(t *testing.T, maxConcurrent int, runs *atomic.Int32) *Broker {
	t.Helper()
	b := queueBroker(t, maxConcurrent, func(ctx context.Context, args []string, stdout, stderr io.Writer) error {
		if runs != nil {
			runs.Add(1)
		}
		_, _ = io.WriteString(stdout, `{"type":"result","subtype":"success"}`+"\n")
		return nil
	})
	b.Rejections = mustOpenRejections(t, b.AuditRoot)
	b.MaxDenials = 2
	return b
}

func waitPending(t *testing.T, b *Broker, id string) {
	t.Helper()
	if !waitFor(5*time.Second, func() bool {
		b.pendingMu.Lock()
		defer b.pendingMu.Unlock()
		_, ok := b.pending[id]
		return ok
	}) {
		t.Fatalf("%s never reached the gate", id)
	}
}

// Enqueue task, deny it at the gate, and wait for its terminal.
func enqueueAndDeny(t *testing.T, b *Broker, task Task) string {
	t.Helper()
	id, err := b.Enqueue(task)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitPending(t, b, id)
	deny(t, b, id)
	waitForQueueState(t, b, id, QueueCancelled)
	return id
}

func degradedLedger(reason string) *RejectionLedger {
	return &RejectionLedger{loadErr: reason, byKey: map[string]*rejectionKeyState{}, diffs: map[string]map[string]string{}}
}

// THE BOUND. Two human denials of the same repo+instruction, and the third
// queue add is refused with 409 before anything is persisted or run.
func TestRejectionLoop_ThirdEnqueueRefusedBeforeSpend(t *testing.T) {
	var runs atomic.Int32
	b := rejectionQueueBroker(t, 1, &runs)
	b.StartDispatcher()
	defer b.StopDispatcher()

	id1 := enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "add a backdoor"})
	id2 := enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "add a backdoor"})

	_, err := b.Enqueue(Task{RepoRef: "git@github.com:O/R", Instruction: "add a backdoor"})
	var loop *RejectionLoopError
	if !errors.As(err, &loop) {
		t.Fatalf("LOOP BREACH: third enqueue admitted (err=%v)", err)
	}
	if loop.Denials != 2 || loop.MaxDenials != 2 || len(loop.DeniedTaskIDs) != 2 ||
		loop.DeniedTaskIDs[0] != id2 || loop.DeniedTaskIDs[1] != id1 {
		t.Fatalf("refusal detail: %+v", loop)
	}
	if items, _ := listQueueItems(b.AuditRoot); len(items) != 2 {
		t.Fatalf("refused enqueue persisted an item: %d items", len(items))
	}
	if runs.Load() != 2 {
		t.Fatalf("LOOP BREACH: agent ran %d times, want 2", runs.Load())
	}

	rec := httptest.NewRecorder()
	b.HandleQueueAdd(rec, httptest.NewRequest("POST", "/queue",
		strings.NewReader(`{"repo_ref":"`+rtRepo+`","instruction":"add a backdoor"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d body=%s, want 409", rec.Code, rec.Body)
	}
	var body struct {
		Error         string   `json:"error"`
		Repo          string   `json:"repo"`
		Denials       int      `json:"denials"`
		MaxDenials    int      `json:"max_denials"`
		DeniedTaskIDs []string `json:"denied_task_ids"`
		Hint          string   `json:"hint"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("409 body is not JSON: %s", rec.Body)
	}
	if body.Error != "rejection_loop" || body.Repo != "github.com/o/r" || body.Denials != 2 ||
		len(body.DeniedTaskIDs) != 2 || !strings.Contains(body.Hint, "drydock submit") {
		t.Fatalf("409 body: %+v", body)
	}
	// A different instruction is a different key and is admitted.
	if _, err := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "add a backdoor, but tested"}); err != nil {
		t.Fatalf("new instruction refused: %v", err)
	}
}

// The issue key: two denials under one issue URL with different instruction
// text (the author edited the issue), and a third edit is still refused.
func TestRejectionLoop_IssueKeyTripsAcrossEditedInstruction(t *testing.T) {
	b := rejectionQueueBroker(t, 1, nil)
	b.StartDispatcher()
	defer b.StopDispatcher()
	const url = "https://github.com/o/r/issues/42"
	enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "# Issue #42: v1", IssueURL: url})
	enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "# Issue #42: v2", IssueURL: url})
	_, err := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "# Issue #42: v3", IssueURL: url})
	var loop *RejectionLoopError
	if !errors.As(err, &loop) || loop.IssueURL != url {
		t.Fatalf("edited issue evaded the bound: %v", err)
	}
	// The same text WITHOUT the issue url is a fresh instruction key: admitted
	// (that is a hand resubmit, and the sync path is the override anyway).
	if _, err := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "# Issue #42: v3"}); err != nil {
		t.Fatalf("instruction-only key wrongly tripped: %v", err)
	}
}

// Moved from Task 3: an auto-approved push must not reopen the queue path.
func TestRejectionLoop_AutoApproveDoesNotReopenEnqueue(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, writesResult(`{"type":"result","subtype":"success"}`))
	b.Rejections = mustOpenRejections(t, b.AuditRoot)
	b.MaxDenials = 2
	body := `{"repo_ref":"` + rtRepo + `","instruction":"do x","agent":"claude"}`
	runSync(t, b, body, deny)
	runSync(t, b, body, deny)
	submit(b, `{"repo_ref":"`+rtRepo+`","instruction":"do x","agent":"claude","auto_approve":true}`)
	if _, err := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "do x"}); err == nil {
		t.Fatal("enqueue admitted after an auto-approve that must not reset the count")
	}
}

// An item enqueued at one denial, whose key reaches the bound before it
// dispatches, is dead-lettered at dispatch with the reason, and no global
// ceiling claim is left behind.
func TestRejectionLoop_DispatchRecheckDropsLateItem(t *testing.T) {
	b := rejectionQueueBroker(t, 1, nil)
	b.StartDispatcher()
	defer b.StopDispatcher()
	id1, err := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "x"})
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "x"})
	id3, _ := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "x"})
	for _, id := range []string{id1, id2} {
		waitPending(t, b, id)
		deny(t, b, id)
		waitForQueueState(t, b, id, QueueCancelled)
	}
	waitForQueueState(t, b, id3, QueueDeadLetter)
	it := queueItemState(t, b, id3)
	if !strings.Contains(it.LastError, "denied 2 times") || !strings.Contains(it.LastError, "since its last approval") ||
		!strings.Contains(it.LastError, id2) {
		t.Fatalf("dead-letter reason: %q", it.LastError)
	}
	if n := b.inFlightStarts(); n != 0 {
		t.Fatalf("a dropped item leaked %d global-ceiling claim(s)", n)
	}
}

// Review Focus 2: a denial landing while the key's next item is ALREADY
// running must not touch that run; it reaches its own gate.
func TestRejectionLoop_RunningItemIsNotKilledByALaterDenial(t *testing.T) {
	b := rejectionQueueBroker(t, 2, nil)
	b.MaxDenials = 1
	b.StartDispatcher()
	defer b.StopDispatcher()
	id1, _ := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "x"})
	id2, _ := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "x"})
	waitPending(t, b, id1)
	waitPending(t, b, id2)
	deny(t, b, id1)
	waitForQueueState(t, b, id1, QueueCancelled)
	if st := queueItemState(t, b, id2).State; st != QueueAwaitingReview {
		t.Fatalf("running item was disturbed by a later denial: %s", st)
	}
	approve(t, b, id2)
	waitForQueueState(t, b, id2, QueueCompleted)
}

// A degraded ledger fails closed with the guard on: Enqueue refuses
// (503-mapped), dispatch parks. With max_denials 0 it is identity.
func TestRejectionLoop_DegradedLedgerRefusesAndParks(t *testing.T) {
	b := rejectionQueueBroker(t, 1, nil)
	b.MaxDenials = 0
	id, err := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "x"})
	if err != nil {
		t.Fatal(err)
	}
	b.Rejections = degradedLedger("line 3 is not a ledger entry; fix or remove it and restart brokerd")
	// Off is identity, even degraded.
	if _, err := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "y"}); err != nil {
		t.Fatalf("max_denials 0 must admit even with a degraded ledger: %v", err)
	}
	b.MaxDenials = 2
	_, err = b.Enqueue(Task{RepoRef: rtRepo, Instruction: "z"})
	var degraded *RejectionLedgerDegradedError
	if !errors.As(err, &degraded) {
		t.Fatalf("degraded ledger admitted an enqueue: %v", err)
	}
	rec := httptest.NewRecorder()
	b.HandleQueueAdd(rec, httptest.NewRequest("POST", "/queue",
		strings.NewReader(`{"repo_ref":"`+rtRepo+`","instruction":"z"}`)))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "rejection_ledger_degraded") ||
		!strings.Contains(rec.Body.String(), "line 3") {
		t.Fatalf("code=%d body=%s, want 503 naming the line", rec.Code, rec.Body)
	}
	if _, ok := b.takeDispatchable(); ok {
		t.Fatal("dispatch did not park on a degraded ledger")
	}
	if st := queueItemState(t, b, id).State; st != QueueQueued {
		t.Fatalf("parked item changed state: %s", st)
	}
}

// max_denials 0: the guard is off, but the ledger is still written.
func TestRejectionLoop_OffAtZeroStillRecords(t *testing.T) {
	b := rejectionQueueBroker(t, 1, nil)
	b.MaxDenials = 0
	b.StartDispatcher()
	defer b.StopDispatcher()
	for i := 0; i < 3; i++ {
		enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "x"})
	}
	repo, instr, _ := RejectionKeys(rtRepo, "", "x", "")
	if n, _ := b.Rejections.Denials(repo, instr, ""); n != 3 {
		t.Fatalf("ledger not written with the guard off: %d", n)
	}
}

// The CI-retry decision consults the guard BEFORE the enqueue-once mark: a
// tripped key refuses terminally with the detail recorded and the mark unset;
// a degraded ledger parks, exactly like the ceiling's unmeasured branch.
func TestRejectionLoop_RetryChildRefusedOrParked(t *testing.T) {
	t.Run("tripped key refuses", func(t *testing.T) {
		b := retryBroker(t, 3)
		b.Rejections = mustOpenRejections(t, b.AuditRoot)
		b.MaxDenials = 1
		parent := baseTask()
		it := seedTaskAwaitingCI(t, b, parent)
		repo, instr, _ := RejectionKeys(parent.RepoRef, parent.RootInstruction, parent.Instruction, parent.IssueURL)
		_ = b.Rejections.Record(RejectionEntry{Kind: RejectionKindDenied, AtMs: b.nowMs(), TaskID: rtID1,
			RepoKey: repo, InstructionSHA256: instr, DiffSHA256: "d", Path: "queue"})
		retryID, detail, park := b.maybeEnqueueCIRetry(failedObs(b, it), QueueCIFailed)
		if retryID != "" || park || !strings.Contains(detail, "no retry") || !strings.Contains(detail, "denied") {
			t.Fatalf("retryID=%q park=%v detail=%q", retryID, park, detail)
		}
		if cur := queueItemState(t, b, it.ID); cur.CIRetryEnqueued {
			t.Fatal("enqueue-once mark was consumed by a refused retry")
		}
	})
	t.Run("degraded ledger parks", func(t *testing.T) {
		b := retryBroker(t, 3)
		b.Rejections = degradedLedger("line 2 is not a ledger entry; fix or remove it and restart brokerd")
		b.MaxDenials = 2
		it := seedTaskAwaitingCI(t, b, baseTask())
		retryID, detail, park := b.maybeEnqueueCIRetry(failedObs(b, it), QueueCIFailed)
		if retryID != "" || !park || !strings.Contains(detail, "rejection ledger") {
			t.Fatalf("retryID=%q park=%v detail=%q", retryID, park, detail)
		}
	})
}
```

`retryBroker`, `seedTaskAwaitingCI`, `baseTask`, and `failedObs` are the existing fixtures in `ciretryloop_test.go`; `runSync` and `mustOpenRejections` come from Task 3's test file.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/broker/ -run 'TestRejectionLoop_(Third|IssueKey|AutoApprove|Dispatch|Running|Degraded|Off|RetryChild)' -v`
Expected: FAIL with "undefined: RejectionLoopError".

- [ ] **Step 3: Implement the guard, the errors, the handler mapping, and the retry gate**

Append to `internal/broker/rejections.go`:

```go
// RejectionLoopError is the identity guard's refusal: a key has reached
// MaxDenials since its last human approval.
type RejectionLoopError struct {
	RepoKey       string
	IssueURL      string
	Denials       int
	MaxDenials    int
	DeniedTaskIDs []string
}

func (e *RejectionLoopError) Error() string {
	return fmt.Sprintf("rejection loop: this work was denied %d time(s) at the diff gate for %s since its last approval (queue.max_denials %d); change the instruction, or run it synchronously with drydock submit to override",
		e.Denials, e.RepoKey, e.MaxDenials)
}

// Hint is the operator-facing remedy rendered into the 409 body.
func (e *RejectionLoopError) Hint() string {
	return fmt.Sprintf("this work was denied %d times at the diff gate since its last approval; change the instruction, or run it synchronously with drydock submit to override", e.Denials)
}

// RejectionLedgerDegradedError is the fail-closed refusal: the ledger could
// not be read, so the bound cannot be evaluated and nothing is admitted.
type RejectionLedgerDegradedError struct{ Reason string }

func (e *RejectionLedgerDegradedError) Error() string {
	return "rejection ledger unreadable (" + e.Reason + "); queue adds are refused and dispatch is parked until it is repaired"
}

// rejectionGuard is the identity guard. nil = admit. Consulted by Enqueue
// (POST /queue), by takeDispatchable (items that were queued before a denial
// landed, and items ResumeQueue re-appended without Enqueue), and by
// maybeEnqueueCIRetry (before the enqueue-once mark); never by the
// synchronous POST /tasks path. Off (MaxDenials <= 0) is identity even when
// the ledger is degraded.
func (b *Broker) rejectionGuard(t Task) error {
	if b.Rejections == nil || b.MaxDenials <= 0 {
		return nil
	}
	if reason := b.Rejections.LoadError(); reason != "" {
		return &RejectionLedgerDegradedError{Reason: reason}
	}
	repoKey, instrSHA, issueKey := RejectionKeys(t.RepoRef, t.RootInstruction, t.Instruction, t.IssueURL)
	n, ids := b.Rejections.Denials(repoKey, instrSHA, issueKey)
	if n >= b.MaxDenials {
		return &RejectionLoopError{RepoKey: repoKey, IssueURL: strings.TrimSpace(t.IssueURL),
			Denials: n, MaxDenials: b.MaxDenials, DeniedTaskIDs: ids}
	}
	return nil
}
```

In `internal/broker/queue.go` `Enqueue`, directly after the `EgressExtra` validation block and before `id := newID()`:

```go
	// The identity guard (rejections.go). Refused before an id is minted, so
	// nothing is persisted and a feeder retrying every few minutes cannot
	// grow the audit dir.
	if err := b.rejectionGuard(t); err != nil {
		return "", err
	}
```

Add to the `Broker` struct (broker.go, next to `Rejections`): `rejectionParkLogged atomic.Bool` (import `sync/atomic` if absent). In `takeDispatchable`, directly after the `if it.State != QueueQueued { continue }` check and BEFORE `if b.vendorExceeded(...)` (so no spend-cap or global-ceiling claim is taken for a refused item):

```go
		// The identity guard, re-asked at dispatch: an item enqueued before
		// its key's last denial landed must not run on the strength of having
		// beaten the bound to the queue, and ResumeQueue re-appends surviving
		// items without Enqueue. Placed before the spend cap and the global
		// claim so a refusal leaks nothing. A degraded ledger parks everything
		// (a fault to wait out, like the ceiling's `unmeasured` branch) and
		// logs once per process; a tripped bound drops the item with the
		// reason in last_error.
		if err := b.rejectionGuard(it.Task); err != nil {
			var loop *RejectionLoopError
			if errors.As(err, &loop) {
				b.dropQueuedLocked(it,
					fmt.Sprintf("dropped before dispatch: this work has been denied %d times at the diff gate since its last approval (queue.max_denials %d; denied tasks: %s)",
						loop.Denials, loop.MaxDenials, strings.Join(loop.DeniedTaskIDs, ", ")),
					"queue: dropped an item whose work reached the rejection-loop bound while it waited")
				b.queue = append(b.queue[:i], b.queue[i+1:]...)
				i--
				continue
			}
			if b.rejectionParkLogged.CompareAndSwap(false, true) {
				slog.Warn("queue: parking every queued item because the rejection ledger is unreadable; repair it and restart brokerd",
					"reason", safeErr(err))
			}
			continue
		}
```

Replace `dropSpendCappedRetryLocked`'s body with a general helper plus a thin wrapper (keep its existing doc comment):

```go
// dropQueuedLocked dead-letters a still-queued item with why in last_error.
// dead_letter, not cancelled: nobody cancelled it; it is the queue's existing
// "did not reach a clean finish" terminal and is visibly not a success. The
// durable write can fail (a full or read-only disk), but the DECISION does
// not depend on it: the next boot's ResumeQueue re-loads and re-drops.
func (b *Broker) dropQueuedLocked(it QueueItem, why, logMsg string) {
	if _, err := b.setQueueStateLocked(it.ID, QueueDeadLetter, func(q *QueueItem) {
		q.LastError = why
	}); err != nil {
		slog.Warn("queue: could not persist a pre-dispatch drop; it will not dispatch in this process and the next boot re-drops it",
			"task_id", it.ID, "err", err)
		return
	}
	slog.Info(logMsg, "task_id", it.ID, "retry_of", it.Task.RetryOf, "attempt", it.Task.Attempt)
}

func (b *Broker) dropSpendCappedRetryLocked(it QueueItem, why string) {
	b.dropQueuedLocked(it, why, "queue: dropped a spend-capped ci retry rather than parking it")
}
```

Add `"errors"` and `"strings"` to `queue.go`'s imports if absent.

In `internal/broker/queuestore.go`, replace the comment above `QueueQueued: {QueuePreparing, QueueDeadLetter, QueueCancelled},`:

```go
	// queued -> dead_letter has TWO writers, both pre-dispatch drops with the
	// reason in last_error: the dispatcher dropping a broker-initiated CI
	// retry whose vendor spend cap exhausted (dropSpendCappedRetryLocked),
	// and the rejection-loop identity guard dropping an item whose work
	// reached queue.max_denials while it waited (takeDispatchable). A
	// human-submitted item is otherwise never dead-lettered from queued; it
	// parks, because a person is waiting for it.
```

In `internal/broker/admin.go` `HandleQueueAdd`, replace the `if err != nil { http.Error(...) }` after `b.Enqueue(t)`:

```go
	id, err := b.Enqueue(t)
	if err != nil {
		var loop *RejectionLoopError
		var degraded *RejectionLedgerDegradedError
		switch {
		case errors.As(err, &loop):
			slog.Warn("queue: refused an enqueue at the rejection-loop bound",
				"repo", loop.RepoKey, "issue_url", loop.IssueURL, "denials", loop.Denials,
				"max_denials", loop.MaxDenials, "denied_task_ids", loop.DeniedTaskIDs)
			writeJSONStatus(w, http.StatusConflict, map[string]any{
				"error": "rejection_loop", "repo": loop.RepoKey, "issue_url": loop.IssueURL,
				"denials": loop.Denials, "max_denials": loop.MaxDenials,
				"denied_task_ids": loop.DeniedTaskIDs, "hint": loop.Hint(),
			})
		case errors.As(err, &degraded):
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{
				"error": "rejection_ledger_degraded", "reason": safeStr(degraded.Reason),
			})
		default:
			http.Error(w, safeErr(err), http.StatusBadRequest)
		}
		return
	}
```

Add next to `writeJSON` in `admin.go`:

```go
// writeJSONStatus is writeJSON with an explicit status code (the JSON 409/503
// refusal bodies; http.Error would write text/plain).
func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
```

(Add `"errors"` and `"log/slog"` to admin.go's imports if absent.)

In `internal/broker/ciretryloop.go` `maybeEnqueueCIRetry`, directly BEFORE the comment block that begins `// From here the child is an ORDINARY QUEUED TASK (D1).` (i.e. after `child` is built and before `markCIRetryEnqueued`), add:

```go
	// Gate 9a. THE REJECTION-LOOP IDENTITY GUARD, asked BEFORE the
	// enqueue-once mark so a refusal never consumes it. A tripped bound is a
	// VERDICT (a human denied this work max_denials times): refuse terminally,
	// with the reason on retry_detail. A degraded ledger is a FAULT, the same
	// class gate 8 parks on: park, bounded by the same park bound, so a
	// transient read failure does not end the chain.
	if gerr := b.rejectionGuard(child); gerr != nil {
		var degraded *RejectionLedgerDegradedError
		if errors.As(gerr, &degraded) {
			if expired, parked := b.ciRetryParkExpired(it); expired {
				slog.Warn("ci retry: the rejection ledger has been unreadable past the park bound; refusing rather than parking forever",
					"task_id", obs.TaskID, "parked_ms", parked)
				return "", fmt.Sprintf("no retry: the rejection ledger was still unreadable after %s of deferral",
					(time.Duration(b.ciRetryParkBoundMs()) * time.Millisecond).String()), false
			}
			return "", "retry deferred: the rejection ledger could not be read (" + safeStr(degraded.Reason) + "); nothing was enqueued, so the decision is re-asked next tick", true
		}
		slog.Info("ci retry: refusing a retry at the rejection-loop bound", "task_id", obs.TaskID, "err", safeErr(gerr))
		return "", "no retry: " + safeStr(gerr.Error()), false
	}
```

Add `"errors"` to `ciretryloop.go`'s imports if absent. Also update the numbered gate list in the file's header comment: insert `9a. THE REJECTION-LOOP IDENTITY GUARD (rejections.go), before the enqueue-once mark: tripped refuses, degraded parks.` between items 9 and the crash-window section.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/broker/ -run 'Rejection|TestQueue|TestHandleQueueAdd|TestCIRetry' -v`
Expected: PASS, including `TestQueueStateMachineIsForwardOnly`, the existing spend-cap drop tests, and the whole CI-retry suite.

- [ ] **Step 5: Commit**

```bash
git add internal/broker
git commit -m "feat(broker): rejection-loop identity guard at enqueue, dispatch, and the ci-retry decision (5C, task 4)"
```

---

### Task 5: The same-diff backstop

**Files:**
- Modify: `internal/broker/broker.go` (`pushAndOpenPR` at ~line 1580: facts computed once, lookup before `writeBrief`, auto-deny after `checkDiffCaps`, `repeat_of` on the `awaiting_approval` event)
- Modify: `internal/broker/queue.go` (`runQueued` terminal mapping, ~line 540)
- Create: `internal/broker/rejections_backstop_test.go`

**Interfaces:**
- Consumes: Task 3's `tr.repeatOf`, `tr.autoDenied`, `tr.fromQueue`, `writeBrief(tr, diff, facts)`, `trustbrief.HashDiff`.
- Produces: audit result row `{"type":"result","subtype":"denied","repeat_of":"<id>",...,"src":"broker"}`; stream event `result/denied` with `repeat_of`; `awaiting_approval` event field `repeat_of`; queue terminal `dead_letter` with `last_error` = `"auto-denied: identical to the diff denied in task <id>"`.

- [ ] **Step 1: Write the failing tests**

Create `internal/broker/rejections_backstop_test.go`:

```go
package broker

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"drydock/internal/trustbrief"
)

// A queue broker whose every stage returns the same diff, recording each
// stage so a test can assert nothing was pushed.
func sameDiffQueueBroker(t *testing.T, diff string) (*Broker, func() []*fakeStage) {
	t.Helper()
	b := queueBroker(t, 1, writesResult(`{"type":"result","subtype":"success"}`))
	var mu sync.Mutex
	var stages []*fakeStage
	b.prepareStage = func(context.Context, string, string) (taskStage, error) {
		st := &fakeStage{workDir: t.TempDir(), diff: diff}
		mu.Lock()
		stages = append(stages, st)
		mu.Unlock()
		return st, nil
	}
	b.Rejections = mustOpenRejections(t, b.AuditRoot)
	b.MaxDenials = 2
	return b, func() []*fakeStage { mu.Lock(); defer mu.Unlock(); return append([]*fakeStage(nil), stages...) }
}

// A queued task whose diff is byte-identical to a denied one never re-poses
// the gate, even with auto_approve, never pushes, and lands dead_letter.
func TestRejectionLoop_RepeatDiffNeverReposesGate(t *testing.T) {
	const diff = "diff --git a/auth.go b/auth.go\n+// backdoor\n"
	b, stages := sameDiffQueueBroker(t, diff)
	b.StartDispatcher()
	defer b.StopDispatcher()

	denied := enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "first wording"})

	id, err := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "second wording", AutoApprove: true})
	if err != nil {
		t.Fatal(err)
	}
	waitForQueueState(t, b, id, QueueDeadLetter)
	it := queueItemState(t, b, id)
	if it.LastError != "auto-denied: identical to the diff denied in task "+denied {
		t.Fatalf("last_error=%q", it.LastError)
	}
	if _, err := os.Stat(gateMarkerPath(b.AuditRoot, id)); err == nil {
		t.Fatal("a gate marker was written for an auto-denied task")
	}
	for _, st := range stages() {
		if st.pushed.Load() {
			t.Fatal("a repeat of a denied diff was pushed")
		}
	}
	audit, _ := os.ReadFile(filepath.Join(b.AuditRoot, id+".jsonl"))
	if !strings.Contains(string(audit), `"subtype":"denied"`) || !strings.Contains(string(audit), `"repeat_of":"`+denied+`"`) ||
		!strings.Contains(string(audit), `"type":"metrics"`) {
		t.Fatalf("audit lacks the broker-authored repeat denial: %s", audit)
	}
	brief, err := trustbrief.Read(b.AuditRoot, id)
	if err != nil || brief.Diff.RepeatOfDenied != denied {
		t.Fatalf("brief repeat_of_denied=%q err=%v", brief.Diff.RepeatOfDenied, err)
	}
	// No ledger entry for an auto-deny.
	repo, instr, _ := RejectionKeys(rtRepo, "", "second wording", "")
	if n, _ := b.Rejections.Denials(repo, instr, ""); n != 0 {
		t.Fatalf("auto-deny wrote a ledger entry: %d", n)
	}
}

// Review Focus 4: a truncated capture is never matched.
func TestRejectionLoop_TruncatedDiffIsNotMatched(t *testing.T) {
	// Build a diff that Analyze reports as truncated: a trailing line with
	// the exact prefix stage.gitDiffCapped appends (trustbrief's bpTruncated,
	// "... [diff truncated at ").
	diff := "diff --git a/x b/x\n+y\n... [diff truncated at 4 MiB]\n"
	if !trustbrief.Analyze(diff).Truncated {
		t.Fatal("test fixture: diff is not reported truncated; check bpTruncated in trustbrief/difffacts.go")
	}
	b, _ := sameDiffQueueBroker(t, diff)
	b.StartDispatcher()
	defer b.StopDispatcher()
	enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "a"})
	id, _ := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "b"})
	waitPending(t, b, id) // reached the gate: not auto-denied
	brief, _ := trustbrief.Read(b.AuditRoot, id)
	if brief.Diff.RepeatOfDenied != "" {
		t.Fatalf("truncated diff was matched: %q", brief.Diff.RepeatOfDenied)
	}
	deny(t, b, id)
}

// The synchronous path only WARNS: the gate is posed with repeat_of on the
// event, and a human approve frees the hash.
func TestRejectionLoop_SyncPathWarnsAndHumanApprovalFreesDiff(t *testing.T) {
	const diff = "diff --git a/x b/x\n+y\n"
	b, _ := sameDiffQueueBroker(t, diff)
	b.StartDispatcher()
	defer b.StopDispatcher()
	denied := enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "x"})

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		b.HandleTask(rec, httptest.NewRequest("POST", "/tasks",
			strings.NewReader(`{"repo_ref":"`+rtRepo+`","instruction":"x again","agent":"claude"}`)))
		close(done)
	}()
	id := waitForPending(t, b)
	approve(t, b, id)
	<-done
	events, term := parseEvents(rec.Body.String())
	if term["outcome"] != "pushed" {
		t.Fatalf("human-approved sync push: %v", term)
	}
	found := false
	for _, ev := range events {
		if ev["stage"] == "awaiting_approval" && ev["repeat_of"] == denied {
			found = true
		}
	}
	if !found {
		t.Fatalf("sync gate event lacks repeat_of=%s: %s", denied, rec.Body)
	}
	repo, _, _ := RejectionKeys(rtRepo, "", "", "")
	if _, ok := b.Rejections.DeniedDiff(repo, trustbrief.HashDiff(diff)); ok {
		t.Fatal("human approval did not free the denied diff hash")
	}
}

// max_denials 0 turns the auto-deny off on the queue path too: the repeat
// reaches the gate, the brief still says it is a repeat, and a human deny is
// labeled as such.
func TestRejectionLoop_BackstopOffAtZero(t *testing.T) {
	const diff = "diff --git a/x b/x\n+y\n"
	b, _ := sameDiffQueueBroker(t, diff)
	b.StartDispatcher()
	defer b.StopDispatcher()
	denied := enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "x"})
	b.MaxDenials = 0
	id, _ := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "y"})
	waitPending(t, b, id)
	brief, err := trustbrief.Read(b.AuditRoot, id)
	if err != nil || brief.Diff.RepeatOfDenied != denied {
		t.Fatalf("brief repeat_of_denied=%q err=%v", brief.Diff.RepeatOfDenied, err)
	}
	deny(t, b, id)
	waitForQueueState(t, b, id, QueueCancelled)
	if it := queueItemState(t, b, id); strings.Contains(it.LastError, "auto-denied") {
		t.Fatalf("a human deny was labeled auto-denied: %q", it.LastError)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/broker/ -run 'RepeatDiff|Truncated|SyncPathWarns|BackstopOff' -v`
Expected: FAIL (the repeat reaches the gate; `last_error` empty; no `repeat_of`).

- [ ] **Step 3: Implement the backstop**

In `internal/broker/broker.go` `pushAndOpenPR`, replace the opening lines through the `checkDiffCaps` block with:

```go
func (tr *taskRun) pushAndOpenPR(diff string) {
	b := tr.b
	files, insertions, deletions := diffStat(diff)
	tr.diffFiles = files
	tr.diffBytes = int64(len(diff))
	facts := trustbrief.Analyze(diff)
	// The same-diff backstop's LOOKUP happens before the brief is written, so
	// the brief on disk at gate time records the match on every path. The
	// hash is over the captured unified diff; a one-byte change evades it,
	// which is why the identity guard (rejectionGuard), not this, is the
	// bound. A TRUNCATED capture is never looked up: two different changes
	// with the same first N bytes hash equal, and a false auto-deny would
	// kill a legitimate queued task. A degraded ledger looks up nothing: the
	// human gate still stands.
	if b.Rejections != nil && b.Rejections.LoadError() == "" && !facts.Truncated {
		repoKey, _, _ := RejectionKeys(tr.repoRef, tr.rootInstruction, tr.instruction, tr.issueURL)
		if prior, hit := b.Rejections.DeniedDiff(repoKey, facts.SHA256); hit {
			tr.repeatOf = prior
		}
	}
	facts = b.writeBrief(tr, diff, facts)
	// Diff-policy caps are ENFORCEMENT, applied before any gate — including
	// the auto-approve branch below, which must never bypass them. A blocked
	// task fails closed: nothing is pushed and it never registers as pending.
	// The Brief and the .diff are already persisted, so `drydock inspect`
	// shows the operator exactly what was blocked. The synthetic audit result
	// row mirrors runVerify's verify_failed pattern (last-wins over the
	// agent's own success row, carrying metered cost).
	if blocked, reason := tr.checkDiffCaps(facts, diff); blocked {
		cost := tr.meteredCostUSD()
		fmt.Fprintf(tr.logf,
			`{"type":"result","subtype":"policy_blocked","is_error":false,"duration_ms":%d,%s,"num_turns":0,"src":"broker"}`+"\n",
			time.Since(tr.taskStart).Milliseconds(), tr.brokerResultSpendFields())
		tr.outcome = "policy_blocked"
		tr.sw.emit(map[string]any{"event": "result", "outcome": "policy_blocked",
			"task_id": tr.id, "reason": reason,
			"duration_ms": time.Since(tr.taskStart).Milliseconds(), "cost_usd": cost,
			"hint": "drydock inspect " + tr.id + " — a diff-policy cap blocked this task before review"})
		return
	}
	// The same-diff backstop's ENFORCEMENT, queue path only, guard on: a
	// human already rejected exactly this change, so it is not re-posed, and
	// auto_approve does not get to push it either. Mirrors the policy_blocked
	// row above. No ledger entry is written: the count is a count of human
	// verdicts, and the earlier denial already holds the hash.
	if tr.repeatOf != "" && tr.fromQueue && b.MaxDenials > 0 {
		cost := tr.meteredCostUSD()
		fmt.Fprintf(tr.logf,
			`{"type":"result","subtype":"denied","repeat_of":%q,"is_error":false,"duration_ms":%d,%s,"num_turns":0,"src":"broker"}`+"\n",
			tr.repeatOf, time.Since(tr.taskStart).Milliseconds(), tr.brokerResultSpendFields())
		tr.outcome = "denied"
		tr.autoDenied = true
		tr.sw.emit(map[string]any{"event": "result", "outcome": "denied",
			"task_id": tr.id, "repeat_of": tr.repeatOf, "diff_bytes": len(diff),
			"duration_ms": time.Since(tr.taskStart).Milliseconds(), "cost_usd": cost,
			"hint": "drydock inspect " + tr.id + ": auto-denied, identical to the diff denied in task " + tr.repeatOf})
		return
	}
```

(Everything from `tr.requiredAcks = requiredAcks(facts, b.DiffPolicy)` on stays as it is.) In the `gateEv` literal block, after `if len(tr.requiredAcks) > 0 { gateEv["second_look"] = tr.requiredAcks }` add:

```go
		if tr.repeatOf != "" {
			gateEv["repeat_of"] = tr.repeatOf
		}
```

In `internal/broker/queue.go` `runQueued`, replace:

```go
	to, lastErr := queueTerminal(tr.outcome, ctx.Err() != nil)
```

with:

```go
	to, lastErr := queueTerminal(tr.outcome, ctx.Err() != nil)
	if tr.autoDenied {
		// A broker decision nobody asked for is dead_letter with the reason
		// (the repo's own rule, see dropQueuedLocked), not cancelled.
		to, lastErr = QueueDeadLetter, "auto-denied: identical to the diff denied in task "+tr.repeatOf
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/broker/ -run 'Rejection|TestQueue|TestHandleTask_Lifecycle|TestRedteam' -v`
Expected: PASS, and `TestHandleTask_LifecycleUnchanged` still reports the same event sequence.

- [ ] **Step 5: Commit**

```bash
git add internal/broker internal/trustbrief
git commit -m "feat(broker): same-diff backstop, auto-deny on the queue path (5C, task 5)"
```

---

### Task 6: Operator surfaces: queue add, status, review, inspect, web UI

**Files:**
- Modify: `cmd/drydock/queue.go:109-135` (`postQueueAdd` error rendering)
- Modify: `cmd/drydock/status.go:17-25,47-56` (health line and `healthBody`)
- Modify: `internal/broker/admin.go:56-90` (`HandleHealth` field), `internal/broker/rejections.go` (`rejectionLedgerError`)
- Modify: `cmd/drydock/inspect.go:53-118` (`printBrief` REPEAT line; `review` reuses it)
- Modify: `internal/webui/assets/app.js:604-612` (diff row chip)
- Modify: `cmd/drydock/queue_test.go`, `cmd/drydock/inspect_test.go`, `internal/webui/ci_assets_test.go`; create `internal/broker/rejections_health_test.go`

**Interfaces:**
- Consumes: the 409/503 bodies from Task 4; `trustbrief.DiffFacts.RepeatOfDenied` from Task 3.
- Produces: `/healthz` field `rejection_ledger_error` (string, "" when healthy); `func (b *Broker) rejectionLedgerError() string`.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/drydock/queue_test.go` (point the client at the test server the same way `TestQueueAdd_BrokerErrorSurfaces` at line 61 does; the snippet assumes `BROKER_ADDR`):

```go
func TestQueueAdd_RendersRejectionLoop409(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"rejection_loop","repo":"github.com/o/r","issue_url":"","denials":2,"max_denials":2,"denied_task_ids":["aaa","bbb"],"hint":"change the instruction, or run it synchronously with drydock submit to override"}`))
	}))
	defer srv.Close()
	t.Setenv("BROKER_ADDR", strings.TrimPrefix(srv.URL, "http://"))
	_, err := postQueueAdd(taskRequest{RepoRef: "https://github.com/o/r.git", Instruction: "x"})
	if err == nil {
		t.Fatal("409 did not surface as an error")
	}
	for _, want := range []string{"refused", "drydock submit", "aaa", "bbb", "github.com/o/r"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestQueueAdd_RendersDegradedLedger503(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"rejection_ledger_degraded","reason":"line 3 is not a ledger entry; fix or remove it and restart brokerd"}`))
	}))
	defer srv.Close()
	t.Setenv("BROKER_ADDR", strings.TrimPrefix(srv.URL, "http://"))
	_, err := postQueueAdd(taskRequest{RepoRef: "https://github.com/o/r.git", Instruction: "x"})
	if err == nil || !strings.Contains(err.Error(), "rejection history") || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("503 rendering: %v", err)
	}
}
```

Append to `cmd/drydock/inspect_test.go`:

```go
func TestPrintBrief_RepeatOfDeniedLine(t *testing.T) {
	b := trustbrief.Brief{TaskID: "0123456789abcdef0123456789abcdef"}
	b.Diff.RepeatOfDenied = "fedcba9876543210fedcba9876543210"
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	printBrief(b)
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if !strings.Contains(string(out), "REPEAT   identical to the diff denied in task fedcba9876543210fedcba9876543210") {
		t.Fatalf("printBrief output lacks the REPEAT line:\n%s", out)
	}
}
```

Create `internal/broker/rejections_health_test.go`:

```go
package broker

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleHealth_ReportsDegradedRejectionLedger(t *testing.T) {
	b := &Broker{}
	rec := httptest.NewRecorder()
	b.HandleHealth(rec, httptest.NewRequest("GET", "/healthz", nil))
	if !strings.Contains(rec.Body.String(), `"rejection_ledger_error":""`) {
		t.Fatalf("healthy body: %s", rec.Body)
	}
	b.Rejections = degradedLedger("line 3 is not a ledger entry; fix or remove it and restart brokerd")
	rec = httptest.NewRecorder()
	b.HandleHealth(rec, httptest.NewRequest("GET", "/healthz", nil))
	if !strings.Contains(rec.Body.String(), `"rejection_ledger_error":"line 3 is not a ledger entry`) {
		t.Fatalf("degraded body: %s", rec.Body)
	}
}
```

In `internal/webui/ci_assets_test.go`, add an assertion in the same style as its neighbors that `app.js` contains `repeat_of_denied` and `REPEAT of denied`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/drydock/ -run 'TestQueueAdd_Renders|TestPrintBrief_Repeat' -v && go test ./internal/broker/ -run TestHandleHealth_Reports -v && go test ./internal/webui/`
Expected: FAIL on each new assertion.

- [ ] **Step 3: Implement the surfaces**

`cmd/drydock/queue.go` `postQueueAdd`: replace the `if resp.StatusCode != http.StatusOK { ... }` block with:

```go
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var refusal struct {
			Error         string   `json:"error"`
			Repo          string   `json:"repo"`
			IssueURL      string   `json:"issue_url"`
			Denials       int      `json:"denials"`
			DeniedTaskIDs []string `json:"denied_task_ids"`
			Hint          string   `json:"hint"`
			Reason        string   `json:"reason"`
		}
		if json.Unmarshal(msg, &refusal) == nil {
			switch refusal.Error {
			case "rejection_loop":
				where := refusal.Repo
				if refusal.IssueURL != "" {
					where += " (" + refusal.IssueURL + ")"
				}
				return "", fmt.Errorf("refused: %s\n  work: %s\n  denied tasks: %s",
					refusal.Hint, where, strings.Join(refusal.DeniedTaskIDs, ", "))
			case "rejection_ledger_degraded":
				return "", fmt.Errorf("brokerd cannot evaluate the rejection history (%s); queue adds are refused until it is repaired (see `drydock status`)", refusal.Reason)
			}
		}
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
```

`internal/broker/admin.go` `HandleHealth`: add `"rejection_ledger_error": b.rejectionLedgerError(),` to the response map, and add to `rejections.go`:

```go
// rejectionLedgerError is "" when the ledger is healthy or absent, else the
// path-free reason it is degraded (rendered on /healthz and by drydock status).
func (b *Broker) rejectionLedgerError() string {
	if b.Rejections == nil {
		return ""
	}
	return b.Rejections.LoadError()
}
```

`cmd/drydock/status.go`: add `RejectionLedgerError string \`json:"rejection_ledger_error"\`` to `healthBody`, and after the `in flight` line in `runStatus`:

```go
		if h.RejectionLedgerError != "" {
			fmt.Printf("WARNING     rejection ledger unreadable: %s. queue adds are refused and dispatch is parked; the file is %s\n",
				h.RejectionLedgerError, filepath.Join(auditDir(), "rejections", "ledger.jsonl"))
		}
```

`cmd/drydock/inspect.go` `printBrief`: directly after the `fmt.Printf("diff     sha %.12s ...` line:

```go
	if b.Diff.RepeatOfDenied != "" {
		fmt.Printf("REPEAT   identical to the diff denied in task %s\n", safeCell(b.Diff.RepeatOfDenied))
	}
```

(`drydock review` calls the same `printBrief`, so it gains the line for free.)

`internal/webui/assets/app.js`, directly after `if (d.truncated) diffKids.push(...)`:

```js
  if (d.repeat_of_denied) diffKids.push(el("span", { class: "brief-chip warn",
    text: "REPEAT of denied " + safeCell(String(d.repeat_of_denied)).slice(0, 12) }));
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./cmd/drydock/ ./internal/broker/ ./internal/webui/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/drydock internal/broker internal/webui
git commit -m "feat(cli,webui): surface rejection-loop refusals, repeats, and a degraded ledger (5C, task 6)"
```

---

### Task 7: Documentation, threat model, and roadmap

**Files:**
- Modify: `site/docs/configuration.md` (new section after "Host-side CI observation", before "Bring your own model")
- Modify: `site/docs/submitting-tasks.md` (after the paragraph ending "...show these outcomes distinctly from `pushed` and `push_failed`." near line 635)
- Modify: `site/docs/daemon.md` ("Know the limits before you walk away" list)
- Modify: `site/docs/troubleshooting.md` ("Common failures")
- Modify: `THREAT_MODEL.md` (end of N4)
- Modify: `docs/ROADMAP.md` (5C list and status line)
- Test: `go test ./cmd/docs-build/` and `make docs` renders.

- [ ] **Step 1: Write the docs**

`site/docs/configuration.md`, new section:

```markdown
## Rejection-loop detection (queue path only, on by default)

brokerd keeps a ledger of human diff-gate verdicts at
`<audit_root>/rejections/ledger.jsonl` (hashes, task ids, and the issue URL
you passed; never instruction or diff text). Two guards read it, both on the
**queue path only**:

- **The identity guard.** A `drydock queue add` (or `POST /queue`) whose
  canonical repository plus original instruction, or repository plus issue
  URL, has been **denied** `max_denials` times at the diff gate since its last
  human approval is refused with HTTP 409 before any VM boots. Nothing is
  persisted, so a feeder script that re-adds the same issue every few minutes
  cannot grow the audit dir. The issue-URL key is there because an issue's
  title and body belong to its author: editing the issue does not reset the
  count. An item that was already queued when its key reached the bound is
  dead-lettered at dispatch, with the reason in `drydock queue list`'s REASON
  column. A bounded CI retry child goes through the same check.
- **The same-diff backstop.** A queued task whose captured diff is
  byte-identical to a diff already denied for that repository is auto-denied
  before the gate (`outcome=denied`, `repeat_of=<earlier task>`, queue state
  `dead_letter`), even with `--auto-approve`. The hash is over the whole
  captured diff, so a one-byte change evades it, and a truncated capture is
  never matched; the identity guard is the bound, this is the backstop.

```yaml
queue:
  max_denials: 2   # 0 = off (the ledger is still written); max 10
```

| Field (under `queue:`) | Env override | Default | Meaning |
|---|---|---|---|
| `max_denials` | `DRYDOCK_QUEUE_MAX_DENIALS` | `2` | Human diff-gate denials of one repo + original instruction (or repo + issue URL), since its last human approval, after which a queue add is refused and a repeat diff is auto-denied. `0` turns both guards off, including the fail-closed refusal below; the ledger is still written. Max `10` |

What counts: only `drydock deny` (or the web UI's deny) at the **diff** gate,
on the live, queued, or resumed-after-restart path. A timeout auto-deny, a
kill, a shutdown, an egress-widening denial, `policy_blocked`, and
`verify_failed` do not count. Only a human `drydock approve` resets the count
and frees that diff's hash; `--auto-approve` is not a verdict and changes
nothing.

How to proceed past it: change the instruction (a reworded task is a new
key; an edited issue is not, see above), or run the task synchronously with
`drydock submit`, which never consults the guard and is the deliberate
override. A synchronous task whose diff matches a denied one still poses the
gate, with a `REPEAT` line in `drydock review`, `drydock inspect`, and the
web UI brief.

What it does not bound: items of the same key that are already running. N
same-key items enqueued before any denial all run (up to
`max_concurrent_tasks`) and each reaches its own gate; a denial never kills a
running task. The spend bound per key is therefore `max_denials` plus
`max_concurrent_tasks` runs.

If the ledger cannot be read at boot and `max_denials > 0`, the guards fail
closed: `queue add` returns 503 naming the bad line, dispatch parks every
queued item, and `drydock status` prints the reason and the path. Fix or
remove the named line (or the file) and restart brokerd; nothing clears it on
its own.
```

`site/docs/submitting-tasks.md`, after the outcomes paragraph:

```markdown
On the queue path, a diff that is byte-identical to one you already denied
for the same repository is **auto-denied** before the gate: `outcome=denied`
with `repeat_of=<the earlier task>` on the audit row, queue state
`dead_letter`, and `drydock queue list` says `auto-denied: identical to the
diff denied in task <id>`. A synchronous `drydock submit` is never
auto-denied; it shows a `REPEAT` line in the brief instead. And a
`drydock queue add` for work you have denied repeatedly is refused up front;
see
[Rejection-loop detection](configuration.html#rejection-loop-detection-queue-path-only-on-by-default).
```

`site/docs/daemon.md`, add to the "Know the limits" list:

```markdown
- **An unattended daemon will not loop on a change you keep rejecting.** A
  queue add for a repo + instruction (or repo + issue) you have denied
  `queue.max_denials` times (default 2) is refused before any spend, and a
  queued task that reproduces a diff you already denied is auto-denied before
  the gate. Items already running when you deny are not touched, so the bound
  per key is `max_denials` plus `max_concurrent_tasks` runs. See
  [Rejection-loop detection](configuration.html#rejection-loop-detection-queue-path-only-on-by-default).
```

`site/docs/troubleshooting.md`, add under "Common failures":

```markdown
- **`drydock queue add` says `refused: this work was denied N times`.** The
  rejection-loop guard: change the instruction, or run it with
  `drydock submit` to review it by hand. The refusal lists the denied task ids.
- **`drydock queue add` says `brokerd cannot evaluate the rejection history`.**
  `<audit_root>/rejections/ledger.jsonl` has a line brokerd could not read;
  the message and `drydock status` name it. Fix or remove that line (or the
  whole file, which only forgets past denials) and restart brokerd.
```

`THREAT_MODEL.md`, append to N4 (before N5):

```markdown
**Rejection-loop detection (`queue.max_denials`, default `2`).** The queue
path refuses a task whose repo + original instruction, or repo + issue URL, a
human has denied `max_denials` times at the diff gate since the last human
approval, and auto-denies a queued diff identical to one already denied. The
ledger it reads (`<audit_root>/rejections/ledger.jsonl`) holds gate causes,
hashes, task ids, and the operator-supplied issue URL only: no agent, issue,
or CI text can reach it, trip it, or clear it, because a denial is a human
`deny`, a reset is a human `approve`, and an issue author editing the issue
changes neither key. Stated limits: the diff hash is exact, so a one-byte
change evades the backstop (the identity guard is the bound); same-key tasks
already running when a denial lands finish on their own gates, so the per-key
bound is `max_denials + max_concurrent_tasks` runs; the synchronous
`drydock submit` path is the deliberate override and is never refused; and an
unreadable ledger fails closed on the queue path only. Verified by
`TestRejectionLoop_ThirdEnqueueRefusedBeforeSpend`,
`TestRejectionLoop_IssueKeyTripsAcrossEditedInstruction`, and
`TestRejectionLoop_RepeatDiffNeverReposesGate`.
```

`docs/ROADMAP.md` 5C: change the first bullet to:

```markdown
- **Rejection-loop detection.** *Landed.* A queue add whose repo + original
  instruction, or repo + issue URL, has been denied `queue.max_denials` times
  (default 2) since its last human approval is refused before any spend, and
  a queued task whose diff is identical to a denied one is auto-denied before
  the gate. Human verdicts only, in a durable ledger under `audit_root`; the
  synchronous path is the override. Spec:
  `docs/superpowers/specs/2026-10-08-rejection-loop-detection-design.md`.
```

and update the "Status" paragraph above the 5C list to say Increment C's first refinement has landed and the next three remain open.

- [ ] **Step 2: Check the docs build and the em-dash rule**

Run: `go test ./cmd/docs-build/ && make docs && git diff -- '*.md' 'config/*.yaml' | grep '^+' | grep -c '—'`
Expected: tests PASS, docs render, and the em-dash count is `0`.

- [ ] **Step 3: Commit**

```bash
git add site/docs THREAT_MODEL.md docs/ROADMAP.md
git commit -m "docs: rejection-loop detection (configuration, submitting, daemon, troubleshooting, threat model, roadmap)"
```

---

### Task 8: Whole-branch verification

- [ ] **Step 1: Full suite, lint, red team**

Run: `go build ./... && go test -race -count=1 ./... && make lint && make redteam`
Expected: all green. `make redteam`'s regex is untouched (the new tests are `TestRejectionLoop_*`, not A-claims).

- [ ] **Step 2: Request a whole-branch review per the execution skill before opening the PR.**
