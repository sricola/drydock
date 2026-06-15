package stage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeOriginRepo creates a bare repo with one commit and returns its path.
func makeOriginRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	gitRun(t, work, "init", "-q")
	gitRun(t, work, "config", "user.email", "t@example.com")
	gitRun(t, work, "config", "user.name", "t")
	os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644)
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-q", "-m", "init")

	bare := t.TempDir()
	gitRun(t, bare, "init", "-q", "--bare")
	gitRun(t, work, "remote", "add", "origin", bare)
	gitRun(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	return bare
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestClone_BringsFilesIn(t *testing.T) {
	origin := makeOriginRepo(t)
	dest := filepath.Join(t.TempDir(), "stage")
	if err := Clone(origin, dest); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Errorf("README.md not present after clone: %v", err)
	}
}

func TestWriteTaskFiles(t *testing.T) {
	dest := t.TempDir()
	if err := WriteTaskFiles(dest, "do the thing", "api.anthropic.com 443\n"); err != nil {
		t.Fatalf("WriteTaskFiles: %v", err)
	}
	p, _ := os.ReadFile(filepath.Join(dest, ".task", "prompt.txt"))
	if string(p) != "do the thing" {
		t.Errorf("prompt.txt = %q", p)
	}
	a, _ := os.ReadFile(filepath.Join(dest, ".task", "allowlist.txt"))
	if string(a) != "api.anthropic.com 443\n" {
		t.Errorf("allowlist.txt = %q", a)
	}
}

func TestCaptureDiff_SeesChange(t *testing.T) {
	origin := makeOriginRepo(t)
	dest := filepath.Join(t.TempDir(), "stage")
	if err := Clone(origin, dest); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	os.WriteFile(filepath.Join(dest, "README.md"), []byte("hello\nworld\n"), 0o644)
	diff, err := CaptureDiff(dest)
	if err != nil {
		t.Fatalf("CaptureDiff: %v", err)
	}
	if !strings.Contains(diff, "+world") {
		t.Errorf("diff missing +world:\n%s", diff)
	}
}
