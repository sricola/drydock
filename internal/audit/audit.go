// Package audit parses drydock's on-disk per-task audit log (<id>.jsonl). The
// last {"type":"result"} line summarises the run; the first {"type":"drydock_meta"}
// line records auth mode + sensitivity. This is the single source of truth for
// outcome/cost so `drydock tasks` and the web UI agree.
package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

type Result struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	DurationMs   int64   `json:"duration_ms"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	NumTurns     int     `json:"num_turns"`
}

type Meta struct {
	Type         string `json:"type"`
	Subscription bool   `json:"subscription"`
	Sensitive    bool   `json:"sensitive"`
}

// ReadMeta returns the drydock_meta first line. Legacy/absent → zero value.
func ReadMeta(path string) Meta {
	f, err := os.Open(path)
	if err != nil {
		return Meta{}
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return Meta{}
	}
	var m Meta
	if json.Unmarshal(bytes.TrimSpace(line), &m) != nil || m.Type != "drydock_meta" {
		return Meta{}
	}
	return m
}

// LastResult finds the final {"type":"result",...} line by reading only the
// file tail. ok=false when none is present (still running / killed early). It
// tolerates an unterminated trailing line (brokerd may be mid-write).
func LastResult(path string, size int64) (Result, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Result{}, false
	}
	defer f.Close()
	const tail = 16 * 1024
	if size > tail {
		if _, err := f.Seek(size-tail, io.SeekStart); err != nil {
			return Result{}, false
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return Result{}, false
	}
	lines := bytes.Split(data, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		var x Result
		if json.Unmarshal(lines[i], &x) == nil && x.Type == "result" {
			return x, true
		}
	}
	return Result{}, false
}

// ReadMetaFile reads the drydock_meta line from an already-opened file.
// The caller is responsible for opening the file with appropriate flags
// (e.g. O_NOFOLLOW) and for closing it. The file offset is reset to the
// beginning before reading so callers may interleave ReadMetaFile with
// LastResultFile in any order.
func ReadMetaFile(f *os.File) Meta {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Meta{}
	}
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return Meta{}
	}
	var m Meta
	if json.Unmarshal(bytes.TrimSpace(line), &m) != nil || m.Type != "drydock_meta" {
		return Meta{}
	}
	return m
}

// LastResultFile finds the final {"type":"result",...} line in an
// already-opened file. Like LastResult but accepts a pre-opened *os.File so
// the caller can control how the file was opened (e.g. with O_NOFOLLOW to
// refuse symlinks). ok=false when no result line is present.
func LastResultFile(f *os.File) (Result, bool) {
	info, err := f.Stat()
	if err != nil {
		return Result{}, false
	}
	size := info.Size()
	const tail = 16 * 1024
	if size > tail {
		if _, err := f.Seek(size-tail, io.SeekStart); err != nil {
			return Result{}, false
		}
	} else {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return Result{}, false
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return Result{}, false
	}
	lines := bytes.Split(data, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		var x Result
		if json.Unmarshal(lines[i], &x) == nil && x.Type == "result" {
			return x, true
		}
	}
	return Result{}, false
}

// HasDuration reports whether a real duration is known. An interrupted task
// (brokerd died under it) has a synthetic 0ms we must not display as "0s".
func HasDuration(r Result, ok bool) bool { return ok && r.Subtype != "interrupted" }

// Outcome derives the human outcome string. Mirrors the old summarize() switch.
func Outcome(r Result, ok bool, m Meta) string {
	if !ok {
		return "running?"
	}
	var s string
	switch {
	case r.Subtype == "interrupted":
		s = "interrupted"
	case r.IsError:
		s = "error"
	case r.Subtype == "success":
		if r.NumTurns > 0 {
			s = fmt.Sprintf("ok (%d turn)", r.NumTurns)
		} else {
			s = "ok"
		}
	default:
		s = r.Subtype
	}
	if m.Sensitive {
		s += " · sensitive"
	}
	return s
}

// Cost formats the cost column. Subscription runs show the literal word; a
// task with no result line shows "-".
func Cost(m Meta, r Result, ok bool) string {
	if !ok {
		return "-"
	}
	if m.Subscription {
		return "subscription"
	}
	return fmt.Sprintf("$%.4f", r.TotalCostUSD)
}

// TotalCost returns total_cost_usd from the last result line in path.
// Returns 0 when no result line is present or the file cannot be read.
func TotalCost(path string) float64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	r, ok := LastResult(path, fi.Size())
	if !ok {
		return 0
	}
	return r.TotalCostUSD
}

// HasResultLine reports whether path's tail contains a parsed
// {"type":"result",...} line. Returns (false, nil) when no result is
// present; returns (false, err) when the file cannot be read.
func HasResultLine(path string) (bool, error) {
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

var progressLine = regexp.MustCompile(`^\[\d+/\d+\]`)

// looksLikeError reports whether a line reads as a failure message, so
// Reason can prefer it over an incidental trailing line.
func looksLikeError(ln string) bool {
	l := strings.ToLower(ln)
	return strings.Contains(l, "error") || strings.Contains(l, "fatal") ||
		strings.Contains(l, "panic") || strings.Contains(l, "failed")
}

// Reason returns the last human-meaningful line of an audit log — the line
// that explains a boot failure (e.g. an entrypoint error). It skips empty
// lines, container progress lines ("[6/6] …"), and JSON event lines.
// ok is false when nothing meaningful is found, so the caller falls back to
// a generic error message.
func Reason(path string) (line string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	lastMeaningful := ""
	for i := len(lines) - 1; i >= 0; i-- {
		ln := strings.TrimSpace(lines[i])
		if ln == "" || strings.HasPrefix(ln, "{") || strings.HasPrefix(ln, "[") || progressLine.MatchString(ln) {
			continue
		}
		// Prefer the most recent line that actually reads as an error: some
		// agents (e.g. codex) print incidental trailing output after the real
		// failure — `ERROR: exceeded retry limit …` followed by a bare token
		// count — and the bare count is useless as an operator-facing reason.
		if looksLikeError(ln) {
			return ln, true
		}
		if lastMeaningful == "" {
			lastMeaningful = ln // fallback when no line reads as an error
		}
	}
	if lastMeaningful != "" {
		return lastMeaningful, true
	}
	return "", false
}
