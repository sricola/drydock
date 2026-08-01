package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"drydock/internal/audit"
)

type taskRow struct {
	id      string
	mtime   time.Time
	age     string
	dur     string
	cost    string
	outcome string
	// costAgentReported marks a cost that came from the AGENT's own result
	// line, not from a broker-authored one (G4). Such a cell carries
	// audit.AgentReportedCostMark and the table prints the legend beneath it.
	costAgentReported bool
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

	// Newest-first. mtime was captured once per row from the ReadDir entry, so
	// the comparator is a field read — no per-comparison stat() syscalls.
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].mtime.After(rows[j].mtime)
	})

	fmt.Printf("%-14s  %5s  %8s  %8s  %s\n", "ID", "AGE", "DUR", "COST", "OUTCOME")
	anyAgentReported := false
	for _, r := range rows {
		fmt.Printf("%-14s  %5s  %8s  %8s  %s\n", r.id, r.age, r.dur, r.cost, r.outcome)
		anyAgentReported = anyAgentReported || r.costAgentReported
	}
	// Printed only when a marked cell is actually on screen, so a table of
	// broker-metered tasks (the normal case) is byte-identical to before.
	if anyAgentReported {
		fmt.Println("\n" + audit.AgentReportedCostLegend)
	}
}

func summarize(id, path string, info os.FileInfo) taskRow {
	r := taskRow{id: id, mtime: info.ModTime(), age: relAge(info.ModTime()), dur: "-", cost: "-", outcome: "running?"}
	// Open the audit file once (O_NOFOLLOW) and read the tail result+metrics
	// rows in a single pass (LastResultAndMetricsFile) plus the head meta,
	// all from the same fd, rather than opening it twice or tail-scanning it
	// twice.
	f, err := audit.OpenRead(path)
	if err != nil {
		return r // unreadable or a symlink — leave as "running?"
	}
	defer f.Close()
	rows := audit.LastRowsFile(f)
	last, ok := rows.Result, rows.HasResult
	meta := audit.ReadMetaFile(f)
	r.outcome = audit.OutcomeWithMetrics(last, ok, meta, rows.Metrics, rows.HasMetrics)
	// Spend is BROKER-OBSERVED (G4): audit.Cost reads the src=="broker" row and
	// marks anything the agent merely reported, so a compromised CLI cannot make
	// this column say what it likes.
	r.cost = audit.Cost(meta, rows)
	r.costAgentReported = audit.CostIsAgentReported(meta, rows)
	if audit.HasDuration(last, ok) {
		r.dur = shortDur(last.DurationMs)
	}
	return r
}
