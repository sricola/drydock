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
