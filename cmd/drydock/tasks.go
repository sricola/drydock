package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"drydock/internal/config"
)

// auditResult is the single line at the end of each <id>.jsonl that
// summarises the run. We only decode the few fields we need.
type auditResult struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	DurationMs   int64   `json:"duration_ms"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	NumTurns     int     `json:"num_turns"`
}

type taskRow struct {
	id      string
	mtime   time.Time
	age     string
	dur     string
	cost    string
	outcome string
}

// costCell formats the cost column for the tasks table. When subscription is
// true it returns the literal "subscription" regardless of the USD amount.
// Otherwise it formats the dollar value as "$x.xxxx".
func costCell(subscription bool, usd float64) string {
	if subscription {
		return "subscription"
	}
	return fmt.Sprintf("$%.4f", usd)
}

// runTasks lists recent runs by scanning AUDIT_ROOT. brokerd doesn't keep
// a registry of past task ids — the audit dir IS the registry.
func runTasks() {
	dir := auditDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("(no tasks yet)")
			return
		}
		die("read audit dir %s: %v", dir, err)
	}

	// Determine auth mode from config so the cost column can display
	// "subscription" instead of a dollar amount for subscription-auth runs.
	subscription := false
	if cfg, err := config.Load(config.DefaultPath()); err == nil {
		subscription = cfg.AnthropicAuth == "subscription"
	}

	rows := make([]taskRow, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		info, err := e.Info()
		if err != nil {
			continue
		}
		rows = append(rows, summarize(id, filepath.Join(dir, name), info, subscription))
	}
	if len(rows) == 0 {
		fmt.Println("(no tasks yet)")
		return
	}

	// Newest-first. mtime was captured once per row from the ReadDir entry, so
	// the comparator is a field read — no per-comparison stat() syscalls.
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].mtime.After(rows[j].mtime)
	})

	fmt.Printf("%-14s  %5s  %8s  %8s  %s\n", "ID", "AGE", "DUR", "COST", "OUTCOME")
	for _, r := range rows {
		fmt.Printf("%-14s  %5s  %8s  %8s  %s\n", r.id, r.age, r.dur, r.cost, r.outcome)
	}
}

func summarize(id, path string, info os.FileInfo, subscription bool) taskRow {
	r := taskRow{id: id, mtime: info.ModTime(), age: relAge(info.ModTime()), dur: "-", cost: "-", outcome: "running?"}

	last, ok := lastResult(path, info.Size())
	if !ok {
		return r
	}
	r.dur = shortDur(last.DurationMs)
	r.cost = costCell(subscription, last.TotalCostUSD)
	switch {
	case last.IsError:
		r.outcome = "error"
	case last.Subtype == "success":
		if last.NumTurns > 0 {
			r.outcome = fmt.Sprintf("ok (%d turn)", last.NumTurns)
		} else {
			r.outcome = "ok"
		}
	default:
		r.outcome = last.Subtype
	}
	return r
}

// lastResult finds the final {"type":"result",...} line, which the broker
// appends at task completion. It reads only the file's tail, not the whole
// (potentially multi-MB) stream-event trace. Returns ok=false for a task that
// never wrote a result line (still running, or killed before completion).
func lastResult(path string, size int64) (auditResult, bool) {
	f, err := os.Open(path)
	if err != nil {
		return auditResult{}, false
	}
	defer f.Close()

	const tail = 16 * 1024 // the result line is ~120 bytes and always last
	if size > tail {
		if _, err := f.Seek(size-tail, io.SeekStart); err != nil {
			return auditResult{}, false
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return auditResult{}, false
	}
	lines := bytes.Split(data, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		var x auditResult
		if json.Unmarshal(lines[i], &x) == nil && x.Type == "result" {
			return x, true
		}
	}
	return auditResult{}, false
}
