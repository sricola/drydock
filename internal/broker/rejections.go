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
	// Check if file ends with a newline. If a previous append was torn,
	// the fragment may still be on disk without a trailing newline.
	// We need to find and truncate to the last complete line before appending.
	var needsRepair bool
	var lastNewlinePos int64
	if fi, err := os.Stat(l.path); err == nil && fi.Size() > 0 {
		// Use a read-only handle to check and locate the last newline
		rf, err := os.OpenFile(l.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err == nil {
			defer rf.Close()
			// Read last byte to check if file ends with newline
			lastByte := make([]byte, 1)
			if _, err := rf.ReadAt(lastByte, fi.Size()-1); err == nil && lastByte[0] != '\n' {
				needsRepair = true
				// Find the last newline by reading the file
				data := make([]byte, fi.Size())
				if _, err := rf.ReadAt(data, 0); err == nil {
					// Find the last newline
					for i := len(data) - 1; i >= 0; i-- {
						if data[i] == '\n' {
							lastNewlinePos = int64(i + 1)
							break
						}
					}
				}
			}
		}
	}
	// If repair needed, truncate to last complete line before appending
	if needsRepair {
		if err := os.Truncate(l.path, lastNewlinePos); err != nil {
			return err
		}
	}
	// Now append the new entry
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
