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
	for _, r := range rows {
		fmt.Printf("%-14s  %5s  %8s  %8s  %s\n", r.id, r.age, r.dur, r.cost, r.outcome)
	}
}

func summarize(id, path string, info os.FileInfo) taskRow {
	r := taskRow{id: id, mtime: info.ModTime(), age: relAge(info.ModTime()), dur: "-", cost: "-", outcome: "running?"}
	// Open the audit file once (O_NOFOLLOW) and read both the tail result and the
	// head meta from the same fd, rather than opening it twice.
	f, err := audit.OpenRead(path)
	if err != nil {
		return r // unreadable or a symlink — leave as "running?"
	}
	defer f.Close()
	last, ok := audit.LastResultFile(f)
	meta := audit.ReadMetaFile(f)
	r.outcome = audit.Outcome(last, ok, meta)
	r.cost = audit.Cost(meta, last, ok)
	if audit.HasDuration(last, ok) {
		r.dur = shortDur(last.DurationMs)
	}
	return r
}
