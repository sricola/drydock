package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	age     string
	dur     string
	cost    string
	outcome string
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
		rows = append(rows, summarize(id, filepath.Join(dir, name), info))
	}
	if len(rows) == 0 {
		fmt.Println("(no tasks yet)")
		return
	}

	// Sort newest-first by sortable key embedded in age. Simpler: just sort
	// by mtime via a parallel slice. Re-read info() each time would be ugly,
	// so we reread mtime here for ordering only.
	sort.SliceStable(rows, func(i, j int) bool {
		ii, _ := os.Stat(filepath.Join(dir, rows[i].id+".jsonl"))
		jj, _ := os.Stat(filepath.Join(dir, rows[j].id+".jsonl"))
		return ii.ModTime().After(jj.ModTime())
	})

	fmt.Printf("%-14s  %5s  %8s  %8s  %s\n", "ID", "AGE", "DUR", "COST", "OUTCOME")
	for _, r := range rows {
		fmt.Printf("%-14s  %5s  %8s  %8s  %s\n", r.id, r.age, r.dur, r.cost, r.outcome)
	}
}

func summarize(id, path string, info os.FileInfo) taskRow {
	r := taskRow{id: id, age: relAge(info.ModTime()), dur: "-", cost: "-", outcome: "running?"}

	f, err := os.Open(path)
	if err != nil {
		return r
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// Audit jsonl can have very long lines (stream events). Bump the limit.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var last auditResult
	for sc.Scan() {
		var x auditResult
		if err := json.Unmarshal(sc.Bytes(), &x); err != nil {
			continue
		}
		if x.Type == "result" {
			last = x
		}
	}
	if last.Type == "" {
		return r
	}
	r.dur = shortDur(last.DurationMs)
	r.cost = fmt.Sprintf("$%.4f", last.TotalCostUSD)
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
