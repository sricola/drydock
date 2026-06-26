package broker

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// interruptedResultLine is the synthetic terminal event appended to a task
// trace that a brokerd crash left without a result line. subtype "interrupted"
// (distinct from "error") tells `drydock tasks` the daemon died under the task
// rather than the task itself failing; duration_ms is 0 (death time unknown).
const interruptedResultLine = `{"type":"result","subtype":"interrupted","is_error":true,"duration_ms":0,"total_cost_usd":0,"num_turns":0}` + "\n"

// TerminateStuckAudits scans auditRoot for <id>.jsonl traces with no terminal
// result line — tasks that were running when a prior brokerd crashed — and
// appends a synthetic "interrupted" result so `drydock tasks` resolves them
// instead of showing "running?" forever. Idempotent: a trace that already has a
// result line is left untouched. SAFE ONLY AT BOOT, when no task is live.
// Returns the count terminated and the first error (per-file errors are
// non-fatal).
func TerminateStuckAudits(auditRoot string) (int, error) {
	entries, err := os.ReadDir(auditRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	var firstErr error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(auditRoot, e.Name())
		has, herr := hasResultLine(path)
		if herr != nil {
			if firstErr == nil {
				firstErr = herr
			}
			continue
		}
		if has {
			continue
		}
		if aerr := appendLine(path, interruptedResultLine); aerr != nil {
			if firstErr == nil {
				firstErr = aerr
			}
			continue
		}
		n++
	}
	return n, firstErr
}

// hasResultLine reports whether the file's tail contains a JSON line whose
// decoded "type" is "result". Mirrors cmd/drydock's lastResult: it PARSES each
// line rather than substring-matching, so a stream event whose text payload
// contains the literal `"type":"result"` is not mistaken for a real result. A
// completed task's result line is always last, hence always within the tail.
func hasResultLine(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	const tail = 16 * 1024
	if info.Size() > tail {
		if _, err := f.Seek(info.Size()-tail, io.SeekStart); err != nil {
			return false, err
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return false, err
	}
	for _, ln := range bytes.Split(data, []byte("\n")) {
		var x struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(ln, &x) == nil && x.Type == "result" {
			return true, nil
		}
	}
	return false, nil
}

// appendLine appends s to the file at path.
func appendLine(path, s string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(s)
	return err
}
