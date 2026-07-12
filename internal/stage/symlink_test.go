package stage

import (
	"os"
	"path/filepath"
	"testing"
)

// makeHostileOriginRepo builds a bare repo whose work tree commits linkPath (a
// path relative to the repo root) as a symlink to target. This reproduces the
// F-01 pre-VM symlink-traversal attack: the staged repository is untrusted and
// controls its own files, so a symlinked control path must not let the trusted
// broker's WriteTaskFiles write outside the stage before any VM boundary exists.
func makeHostileOriginRepo(t *testing.T, linkPath, target string) string {
	t.Helper()
	work := t.TempDir()
	gitRun(t, work, "init", "-q", "-b", "main")
	gitRun(t, work, "config", "user.email", "evil@example.com")
	gitRun(t, work, "config", "user.name", "evil")
	os.WriteFile(filepath.Join(work, "README.md"), []byte("hi\n"), 0o644)

	full := filepath.Join(work, linkPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-q", "-m", "hostile")

	bare := t.TempDir()
	gitRun(t, bare, "init", "-q", "-b", "main", "--bare")
	gitRun(t, work, "remote", "add", "origin", bare)
	gitRun(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	return bare
}

// Variant A: .task/prompt.txt committed as a symlink to an operator file. The
// broker must not follow it and truncate the host file.
func TestRedteam_A3_RefusesSymlinkedPromptFile(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "precious.txt")
	if err := os.WriteFile(victim, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := prepare(t, makeHostileOriginRepo(t, ".task/prompt.txt", victim))

	err := s.WriteTaskFiles("PWNED-BY-DRYDOCK")

	if got, _ := os.ReadFile(victim); string(got) != "ORIGINAL" {
		t.Errorf("host file overwritten through a symlinked prompt.txt: got %q", got)
	}
	if err == nil {
		t.Error("WriteTaskFiles followed a hostile symlink without error; want fail-closed")
	}
}

// Variant B: .task itself committed as a symlink to an operator directory. The
// broker must not create prompt.txt inside the redirected directory.
func TestRedteam_A3_RefusesSymlinkedTaskDir(t *testing.T) {
	victimDir := t.TempDir()
	victimFile := filepath.Join(victimDir, "prompt.txt")
	if err := os.WriteFile(victimFile, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := prepare(t, makeHostileOriginRepo(t, ".task", victimDir))

	err := s.WriteTaskFiles("PWNED-VIA-TASKDIR")

	if got, _ := os.ReadFile(victimFile); string(got) != "ORIGINAL" {
		t.Errorf("host file written through a symlinked .task: got %q", got)
	}
	if err == nil {
		t.Error("WriteTaskFiles wrote through a symlinked .task without error; want fail-closed")
	}
}
