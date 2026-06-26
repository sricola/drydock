package stage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeOriginRepo creates a bare repo with one commit and returns its path.
// `-b main` is set on both init calls so the test isn't fragile against the
// host's init.defaultBranch — on Ubuntu (CI) without that config, the bare's
// HEAD would stay at refs/heads/master while we push to main, and `git clone`
// then produces an empty work tree (warning, exit 0) and the test fails.
func makeOriginRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	gitRun(t, work, "init", "-q", "-b", "main")
	gitRun(t, work, "config", "user.email", "t@example.com")
	gitRun(t, work, "config", "user.name", "t")
	os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644)
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-q", "-m", "init")

	bare := t.TempDir()
	gitRun(t, bare, "init", "-q", "-b", "main", "--bare")
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

func prepare(t *testing.T, origin string) *Stage {
	t.Helper()
	s, err := Prepare(filepath.Join(t.TempDir(), "stage"), origin)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// host git needs an identity to commit in these tests
	gitRun(t, s.gitDir, "config", "user.email", "broker@example.com")
	gitRun(t, s.gitDir, "config", "user.name", "broker")
	return s
}

func TestPrepare_SeparatesGitFromWorkTree(t *testing.T) {
	s := prepare(t, makeOriginRepo(t))
	if _, err := os.Stat(filepath.Join(s.WorkDir, "README.md")); err != nil {
		t.Errorf("README.md not present in work tree: %v", err)
	}
	// The mounted work tree must NOT contain .git (no creds, no hooks reachable).
	if _, err := os.Stat(filepath.Join(s.WorkDir, ".git")); !os.IsNotExist(err) {
		t.Errorf("work tree contains .git (would expose creds/hooks to the VM): err=%v", err)
	}
	// The host-only git dir must exist outside the mounted path.
	if _, err := os.Stat(s.gitDir); err != nil {
		t.Errorf("host-only git dir missing: %v", err)
	}
}

func TestWriteTaskFiles(t *testing.T) {
	s := prepare(t, makeOriginRepo(t))
	if err := s.WriteTaskFiles("do the thing", "api.anthropic.com 443\n"); err != nil {
		t.Fatalf("WriteTaskFiles: %v", err)
	}
	p, _ := os.ReadFile(filepath.Join(s.WorkDir, ".task", "prompt.txt"))
	if string(p) != "do the thing" {
		t.Errorf("prompt.txt = %q", p)
	}
	a, _ := os.ReadFile(filepath.Join(s.WorkDir, ".task", "allowlist.txt"))
	if string(a) != "api.anthropic.com 443\n" {
		t.Errorf("allowlist.txt = %q", a)
	}
}

func TestCaptureDiff_SeesChange(t *testing.T) {
	s := prepare(t, makeOriginRepo(t))
	os.WriteFile(filepath.Join(s.WorkDir, "README.md"), []byte("hello\nworld\n"), 0o644)
	diff, err := s.CaptureDiff()
	if err != nil {
		t.Fatalf("CaptureDiff: %v", err)
	}
	if !strings.Contains(diff, "+world") {
		t.Errorf("diff missing +world:\n%s", diff)
	}
}

// The .task control dir must never leak into the diff (it holds the agent prompt
// and the compiled egress allowlist, which would otherwise be pushed into the PR).
func TestCaptureDiff_ExcludesTaskDir(t *testing.T) {
	s := prepare(t, makeOriginRepo(t))
	if err := s.WriteTaskFiles("secret prompt", "api.anthropic.com 443\n"); err != nil {
		t.Fatalf("WriteTaskFiles: %v", err)
	}
	os.WriteFile(filepath.Join(s.WorkDir, "README.md"), []byte("hello\nworld\n"), 0o644)
	diff, err := s.CaptureDiff()
	if err != nil {
		t.Fatalf("CaptureDiff: %v", err)
	}
	if !strings.Contains(diff, "+world") {
		t.Errorf("diff should contain the real change:\n%s", diff)
	}
	if strings.Contains(diff, ".task") || strings.Contains(diff, "secret prompt") {
		t.Errorf("diff leaked .task control files:\n%s", diff)
	}
}

func TestCleanup_RefusesUnsafePaths(t *testing.T) {
	cases := []string{"", "/", ".", "relative/path"}
	for _, root := range cases {
		s := &Stage{Root: root}
		if err := s.Cleanup(); err == nil {
			t.Errorf("Cleanup(%q) must refuse but did not", root)
		}
	}
}

func TestCleanup_RemovesAbsoluteTempDir(t *testing.T) {
	root := t.TempDir() + "/scratch"
	if err := os.MkdirAll(root+"/sub", 0o700); err != nil {
		t.Fatal(err)
	}
	s := &Stage{Root: root}
	if err := s.Cleanup(); err != nil {
		t.Errorf("Cleanup: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("Cleanup didn't remove root: err=%v", err)
	}
}

func TestPush_CommitsAndPushes(t *testing.T) {
	bare := makeOriginRepo(t)
	s := prepare(t, bare)
	if err := os.WriteFile(filepath.Join(s.WorkDir, "README.md"), []byte("hello\nfeature\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.Push("agent/abc123", "agent: add feature"); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// The branch exists on the bare origin with the new commit.
	out, err := exec.Command("git", "--git-dir="+bare, "show", "agent/abc123", "--name-only").CombinedOutput()
	if err != nil {
		t.Fatalf("verifying push: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "README.md") {
		t.Errorf("pushed commit missing README.md change:\n%s", out)
	}
}

func TestPushEnv_CarriesGitDirAndHookNeutralization(t *testing.T) {
	s := prepare(t, makeOriginRepo(t))
	env := s.PushEnv()
	// PushEnv must carry the host-only git dir and the hook-neutralization that
	// the threat model claims is enforced. Drop these and an adapter that spawns
	// git would run against the work-tree .git instead.
	wantEnv := []string{
		"GIT_DIR=" + s.gitDir,
		"GIT_WORK_TREE=" + s.WorkDir,
		"GIT_CONFIG_KEY_0=core.hooksPath",
		"GIT_CONFIG_VALUE_0=/dev/null",
		"GIT_CONFIG_KEY_1=core.fsmonitor",
		"GIT_CONFIG_VALUE_1=false",
	}
	for _, want := range wantEnv {
		found := false
		for _, e := range env {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("PushEnv missing %q (adapter would lose the host-only-git-dir defense)", want)
		}
	}
}

// Simulates a malicious agent that plants a git hook in the work tree. The
// host-side commit must NOT execute it (host git uses the separated git dir with
// hooks disabled). This is the core trust-boundary regression test.
func TestHostCommit_IgnoresPlantedHook(t *testing.T) {
	s := prepare(t, makeOriginRepo(t))

	// Agent writes /work/.git/hooks/pre-commit that drops a marker on the host.
	marker := filepath.Join(t.TempDir(), "PWNED")
	hookDir := filepath.Join(s.WorkDir, ".git", "hooks")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(filepath.Join(hookDir, "pre-commit"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	os.WriteFile(filepath.Join(s.WorkDir, "README.md"), []byte("hello\nchanged\n"), 0o644)
	if err := s.stageAll(); err != nil {
		t.Fatalf("stageAll: %v", err)
	}
	if _, err := s.git("commit", "-m", "agent change"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("planted pre-commit hook EXECUTED on host (marker exists): err=%v", err)
	}
	// And the planted .git must not have leaked into the commit.
	diff, _ := s.git("show", "--name-only", "HEAD")
	if strings.Contains(diff, ".git/hooks") {
		t.Errorf("planted .git leaked into commit:\n%s", diff)
	}
}

func TestReapOrphans_RemovesChildDirsKeepsFiles(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"task-a", "task-b", "task-c"} {
		if err := os.MkdirAll(filepath.Join(root, d, "work"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stray := filepath.Join(root, "keep.txt")
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := ReapOrphans(root)
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if n != 3 {
		t.Errorf("reaped %d, want 3", n)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("stray file must survive: %v", err)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("dir %q should have been reaped", e.Name())
		}
	}
}

func TestReapOrphans_RefusesUnsafeRoot(t *testing.T) {
	for _, bad := range []string{"", "/", ".", "relative/path"} {
		if _, err := ReapOrphans(bad); err == nil {
			t.Errorf("ReapOrphans(%q) = nil error, want refusal", bad)
		}
	}
}

func TestReapOrphans_MissingRootIsNoop(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if n, err := ReapOrphans(missing); err != nil || n != 0 {
		t.Errorf("missing root = (%d,%v), want (0,nil)", n, err)
	}
}
