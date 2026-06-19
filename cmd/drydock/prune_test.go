package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ids(ts []taskArtifacts) string {
	s := make([]string, len(ts))
	for i, t := range ts {
		s[i] = t.id
	}
	return strings.Join(s, ",")
}

func TestParseRetention(t *testing.T) {
	cases := map[string]time.Duration{
		"30d":  30 * 24 * time.Hour,
		"2w":   14 * 24 * time.Hour,
		"720h": 720 * time.Hour,
		"30m":  30 * time.Minute,
	}
	for in, want := range cases {
		got, err := parseRetention(in)
		if err != nil || got != want {
			t.Errorf("parseRetention(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "5x", "-3d"} {
		if _, err := parseRetention(bad); err == nil {
			t.Errorf("parseRetention(%q) should error", bad)
		}
	}
}

func TestTaskIDFromFile(t *testing.T) {
	ok := map[string]string{
		"2d66a317c7c77f92.jsonl":      "2d66a317c7c77f92",
		"2d66a317c7c77f92.diff":       "2d66a317c7c77f92",
		"2d66a317c7c77f92.widen.json": "2d66a317c7c77f92",
	}
	for name, wantID := range ok {
		if id, got := taskIDFromFile(name); !got || id != wantID {
			t.Errorf("taskIDFromFile(%q) = %q,%v; want %q,true", name, id, got, wantID)
		}
	}
	for _, name := range []string{"config.yaml", "NOTHEX.jsonl", "abc.txt", ".jsonl", "ABCDEF.diff"} {
		if _, ok := taskIDFromFile(name); ok {
			t.Errorf("taskIDFromFile(%q) should be rejected", name)
		}
	}
}

func TestSelectForPrune(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	mk := func(id string, ageDays int) taskArtifacts {
		return taskArtifacts{id: id, newest: now.Add(-time.Duration(ageDays) * 24 * time.Hour)}
	}
	base := []taskArtifacts{mk("a", 1), mk("b", 10), mk("c", 40), mk("d", 60)}
	clone := func() []taskArtifacts { return append([]taskArtifacts{}, base...) }

	if got := selectForPrune(clone(), 30*24*time.Hour, 0, now); ids(got) != "c,d" {
		t.Errorf("older-than 30d = %q, want c,d", ids(got))
	}
	// keep-last 3 protects a,b,c (the 3 newest); only d remains eligible and it's old.
	if got := selectForPrune(clone(), 30*24*time.Hour, 3, now); ids(got) != "d" {
		t.Errorf("older-than 30d keep-last 3 = %q, want d", ids(got))
	}
	// keep-last larger than the set protects everything.
	if got := selectForPrune(clone(), 30*24*time.Hour, 10, now); len(got) != 0 {
		t.Errorf("keep-last 10 = %q, want none", ids(got))
	}
	// nothing old enough.
	if got := selectForPrune(clone(), 100*24*time.Hour, 0, now); len(got) != 0 {
		t.Errorf("older-than 100d = %q, want none", ids(got))
	}
}

// scanAuditTasks must group a task's files, compute its size/age, and never
// match a non-task file (a stray config, a non-hex name, a directory).
func TestScanAuditTasks_GroupsAndIgnoresNonTaskFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		os.Chtimes(p, mt, mt)
	}
	const oldID = "aaaa1111aaaa1111aaaa1111aaaa1111" // 32-char hex, like a real id
	const newID = "bbbb2222bbbb2222bbbb2222bbbb2222"
	write(oldID+".jsonl", "trace", 40*24*time.Hour)
	write(oldID+".diff", "diffbody", 40*24*time.Hour)
	write(newID+".jsonl", "recent", time.Hour)
	write("config.yaml", "secret", 100*24*time.Hour)                              // wrong suffix — must be ignored
	write("a.jsonl", "shorthex", 100*24*time.Hour)                                // too-short id — must be ignored
	write("NOTHEXNOTHEXNOTH.jsonl", "x", 100*24*time.Hour)                        // non-hex id — must be ignored
	os.Mkdir(filepath.Join(dir, "cccc3333cccc3333cccc3333cccc3333.jsonl"), 0o755) // directory — must be skipped

	tasks, err := scanAuditTasks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("found %d tasks, want 2 (ids: %s)", len(tasks), ids(tasks))
	}
	for _, ta := range tasks {
		if ta.id == oldID {
			if len(ta.files) != 2 {
				t.Errorf("old task grouped %d files, want 2", len(ta.files))
			}
			if ta.bytes != int64(len("trace")+len("diffbody")) {
				t.Errorf("old task bytes = %d, want %d", ta.bytes, len("trace")+len("diffbody"))
			}
		}
	}

	prune := selectForPrune(tasks, 30*24*time.Hour, 0, time.Now())
	if len(prune) != 1 || prune[0].id != oldID {
		t.Fatalf("prune = %s, want [%s]", ids(prune), oldID)
	}
	deleteTasks(prune)
	// The recent task and the non-task files must remain untouched.
	if _, err := os.Stat(filepath.Join(dir, newID+".jsonl")); err != nil {
		t.Error("recent task was removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err != nil {
		t.Error("config.yaml must never be touched by prune")
	}
	if _, err := os.Stat(filepath.Join(dir, "a.jsonl")); err != nil {
		t.Error("too-short-id file must never be touched by prune")
	}
}

func TestDeleteTasks_RemovesFilesAndCountsFreed(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ta := taskArtifacts{
		id:    "deadbeefdeadbeefdeadbeefdeadbeef",
		files: []string{mk("x.jsonl", "12345"), mk("x.diff", "678")},
	}
	removed, failed, freed := deleteTasks([]taskArtifacts{ta})
	if removed != 1 || failed != 0 || freed != 8 {
		t.Fatalf("removed=%d failed=%d freed=%d, want 1,0,8", removed, failed, freed)
	}
	for _, f := range ta.files {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s not removed", f)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "0B", 512: "512B", 1024: "1.0KB", 1536: "1.5KB", 1048576: "1.0MB"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
