package stage

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// PushPreflight is a dry-run write-auth probe: it must authenticate and
// compute ref updates against the real remote without moving any refs. These
// tests reuse the package's existing bare-origin fixture helpers
// (makeOriginRepo, prepare, gitRun) rather than a new standalone builder.

func TestPushPreflight_SucceedsAgainstWritableRemote(t *testing.T) {
	s := prepare(t, makeOriginRepo(t))
	if err := s.PushPreflight("agent/test123"); err != nil {
		t.Fatalf("PushPreflight: %v", err)
	}
	// A dry-run must not create the ref on the remote.
	cmd := exec.Command("git", "--git-dir="+s.gitDir, "ls-remote", "origin", "refs/heads/agent/test123")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ls-remote: %v\n%s", err, out)
	}
	if len(out) != 0 {
		t.Fatalf("dry-run created the remote ref:\n%s", out)
	}
}

func TestPushPreflight_FailsAgainstMissingRemote(t *testing.T) {
	s := prepare(t, makeOriginRepo(t))
	// Point origin at a path that does not exist.
	if _, err := s.git("remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git")); err != nil {
		t.Fatal(err)
	}
	if err := s.PushPreflight("agent/test123"); err == nil {
		t.Fatal("PushPreflight succeeded against a nonexistent remote")
	}
}
