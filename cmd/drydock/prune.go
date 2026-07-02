package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// taskArtifacts groups every audit file that belongs to one task id.
type taskArtifacts struct {
	id     string
	newest time.Time // newest mtime across the task's files — the task's "age"
	bytes  int64     // total size of the task's files
	files  []string  // absolute paths, deleted all-or-nothing
}

// knownSuffixes are the per-task audit artifacts brokerd writes. prune only
// ever touches files matching <hex-id><suffix> in the audit dir — never
// recurses, never other files.
var knownSuffixes = []string{".jsonl", ".diff", ".widen.json"}

func isHexID(s string) bool {
	// Real task ids are 32 hex chars (128-bit, see broker.newID). Require a
	// conservative floor so a short stray file like "a.diff" can never be
	// mistaken for a task artifact.
	if len(s) < 16 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// taskIDFromFile returns the task id for a known audit artifact, or ok=false
// for anything that isn't one (so prune can't match a stray file).
func taskIDFromFile(name string) (id string, ok bool) {
	for _, s := range knownSuffixes {
		if strings.HasSuffix(name, s) {
			id := strings.TrimSuffix(name, s)
			if isHexID(id) {
				return id, true
			}
		}
	}
	return "", false
}

// scanAuditTasks groups the audit dir's per-task artifacts. Skips directories,
// non-regular files, and anything not matching a task-artifact name.
func scanAuditTasks(dir string) ([]taskArtifacts, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	byID := map[string]*taskArtifacts{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id, ok := taskIDFromFile(e.Name())
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		ta := byID[id]
		if ta == nil {
			ta = &taskArtifacts{id: id}
			byID[id] = ta
		}
		ta.files = append(ta.files, filepath.Join(dir, e.Name()))
		ta.bytes += info.Size()
		if info.ModTime().After(ta.newest) {
			ta.newest = info.ModTime()
		}
	}
	out := make([]taskArtifacts, 0, len(byID))
	for _, ta := range byID {
		out = append(out, *ta)
	}
	return out, nil
}

// selectForPrune returns the tasks to delete: those whose newest artifact is
// older than olderThan, excluding the keepLast most-recent tasks (a floor that
// applies regardless of age). Sorts tasks newest-first as a side effect.
func selectForPrune(tasks []taskArtifacts, olderThan time.Duration, keepLast int, now time.Time) []taskArtifacts {
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].newest.After(tasks[j].newest) })
	cutoff := now.Add(-olderThan)
	var prune []taskArtifacts
	for i, ta := range tasks {
		if i < keepLast {
			continue
		}
		if ta.newest.Before(cutoff) {
			prune = append(prune, ta)
		}
	}
	return prune
}

// parseRetention accepts Go durations (720h, 30m) plus Nd / Nw day/week
// shorthand, which time.ParseDuration doesn't.
func parseRetention(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if n := len(s); n >= 2 {
		switch s[n-1] {
		case 'd', 'w':
			num, err := strconv.Atoi(s[:n-1])
			if err != nil || num < 0 {
				return 0, fmt.Errorf("invalid duration %q", s)
			}
			unit := 24 * time.Hour
			if s[n-1] == 'w' {
				unit = 7 * 24 * time.Hour
			}
			return time.Duration(num) * unit, nil
		}
	}
	return time.ParseDuration(s)
}

func runPrune(args []string) {
	fs := flag.NewFlagSet("drydock prune", flag.ExitOnError)
	olderRaw := fs.String("older-than", "", "prune tasks whose newest artifact is older than this (e.g. 30d, 2w, 720h). Required.")
	keepLast := fs.Int("keep-last", 0, "always keep the N most-recent tasks regardless of age")
	yes := fs.Bool("yes", false, "actually delete (default: dry-run — just print what would be removed)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: drydock prune --older-than DUR [--keep-last N] [--yes]

Delete old per-task audit artifacts (<id>.jsonl/.diff/.widen.json) from the
audit dir. Dry-run by default — pass --yes to actually delete. Running and
awaiting-approval tasks have just-created files, so any sane --older-than
won't touch them.

Flags:`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *olderRaw == "" {
		die("--older-than is required (e.g. --older-than 30d). Refusing to prune everything.")
	}
	older, err := parseRetention(*olderRaw)
	if err != nil {
		die("--older-than: %v", err)
	}
	if older <= 0 {
		die("--older-than must be positive")
	}
	if *keepLast < 0 {
		die("--keep-last must be >= 0")
	}

	dir := auditDir()
	tasks, err := scanAuditTasks(dir)
	if err != nil {
		die("scan audit dir %s: %v", dir, err)
	}
	prune := selectForPrune(tasks, older, *keepLast, time.Now())
	if len(prune) == 0 {
		fmt.Printf("nothing to prune in %s (older than %s, keeping last %d)\n", dir, *olderRaw, *keepLast)
		return
	}

	fmt.Printf("%-14s  %5s  %9s\n", "ID", "AGE", "SIZE")
	var total int64
	for _, ta := range prune {
		fmt.Printf("%-14s  %5s  %9s\n", ta.id, relAge(ta.newest), humanBytes(ta.bytes))
		total += ta.bytes
	}

	if !*yes {
		fmt.Printf("\nwould free %s across %d task(s). Re-run with --yes to delete.\n", humanBytes(total), len(prune))
		return
	}

	removed, failed, freed := deleteTasks(prune)
	fmt.Printf("\nfreed %s across %d task(s)", humanBytes(freed), removed)
	if failed > 0 {
		fmt.Printf(" — %d task(s) had errors (see above)", failed)
	}
	fmt.Println()
}

// deleteTasks removes each task's files. freed accumulates the size of files
// actually removed (so a partial failure still reports what it freed, rather
// than crediting or discarding a whole task's bytes). A task with any failed
// removal is counted in failed, not removed.
func deleteTasks(tasks []taskArtifacts) (removed, failed int, freed int64) {
	for _, ta := range tasks {
		taskFailed := false
		for _, f := range ta.files {
			info, statErr := os.Stat(f)
			if err := os.Remove(f); err != nil {
				taskFailed = true
				fmt.Fprintf(os.Stderr, "  remove %s: %v\n", f, err)
				continue
			}
			if statErr == nil {
				freed += info.Size()
			}
		}
		if taskFailed {
			failed++
		} else {
			removed++
		}
	}
	return removed, failed, freed
}
