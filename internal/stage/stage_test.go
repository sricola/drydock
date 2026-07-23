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
	if err := s.WriteTaskFiles("do the thing"); err != nil {
		t.Fatalf("WriteTaskFiles: %v", err)
	}
	p, _ := os.ReadFile(filepath.Join(s.WorkDir, ".task", "prompt.txt"))
	if string(p) != "do the thing" {
		t.Errorf("prompt.txt = %q", p)
	}
	// Egress is enforced host-side by squid; no allowlist file is written into
	// the VM-visible work tree (nothing in-VM reads it, and a bogus "allowlist"
	// there would falsely imply in-VM enforcement).
	if _, err := os.Stat(filepath.Join(s.WorkDir, ".task", "allowlist.txt")); !os.IsNotExist(err) {
		t.Errorf("allowlist.txt should not be written into the work tree: err=%v", err)
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

// The .task control dir must never leak into the diff (it holds the agent
// prompt, which would otherwise be pushed into the PR).
func TestCaptureDiff_ExcludesTaskDir(t *testing.T) {
	s := prepare(t, makeOriginRepo(t))
	if err := s.WriteTaskFiles("secret prompt"); err != nil {
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

// setupPushable creates a bare origin repo and a Stage with a pending change,
// ready for Commit or Push. Returns the bare origin git dir and the Stage.
func setupPushable(t *testing.T) (string, *Stage) {
	t.Helper()
	bare := makeOriginRepo(t)
	s := prepare(t, bare)
	if err := os.WriteFile(filepath.Join(s.WorkDir, "README.md"), []byte("hello\nfeature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return bare, s
}

func TestPush_CommitsAndPushes(t *testing.T) {
	bare, s := setupPushable(t)

	// The production push path (broker pushWithRecovery) is Commit then
	// PushBranch; on the happy path the remote name equals the local branch.
	if err := s.Commit("agent/abc123", "agent: add feature"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := s.PushBranch("agent/abc123", "agent/abc123"); err != nil {
		t.Fatalf("PushBranch: %v", err)
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

func TestPushBranch_PushesToDifferentRemoteName(t *testing.T) {
	origin, s := setupPushable(t) // builds a bare origin + a Stage with a change

	if err := s.Commit("agent/x", "agent: change"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Push the committed local branch to a DIFFERENTLY named remote branch.
	if err := s.PushBranch("agent/x", "agent/x-2"); err != nil {
		t.Fatalf("PushBranch: %v", err)
	}
	out, err := exec.Command("git", "--git-dir="+origin, "branch", "--list", "agent/x-2").Output()
	if err != nil {
		t.Fatalf("list remote branches: %v", err)
	}
	if !strings.Contains(string(out), "agent/x-2") {
		t.Errorf("remote missing agent/x-2 branch; got %q", out)
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
	n, err := ReapOrphans(root, nil)
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
		if _, err := ReapOrphans(bad, nil); err == nil {
			t.Errorf("ReapOrphans(%q) = nil error, want refusal", bad)
		}
	}
}

func TestReapOrphans_MissingRootIsNoop(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if n, err := ReapOrphans(missing, nil); err != nil || n != 0 {
		t.Errorf("missing root = (%d,%v), want (0,nil)", n, err)
	}
}

func TestReopen_RecoversAPreparedStage(t *testing.T) {
	origin, s := setupPushable(t) // bare origin + a Stage with an uncommitted change
	root := s.Root

	re, err := Reopen(root)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	// The reopened stage can commit the surviving work-tree change and push it.
	if err := re.Commit("agent/resumed", "agent: resumed"); err != nil {
		t.Fatalf("Commit on reopened stage: %v", err)
	}
	if err := re.PushBranch("agent/resumed", "agent/resumed"); err != nil {
		t.Fatalf("PushBranch on reopened stage: %v", err)
	}
	out, _ := exec.Command("git", "--git-dir="+origin, "branch", "--list", "agent/resumed").CombinedOutput()
	if !strings.Contains(string(out), "agent/resumed") {
		t.Errorf("reopened stage did not push; origin branches: %s", out)
	}
}

func TestReopen_ErrorsWhenGitDirMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := Reopen(dir); err == nil {
		t.Error("Reopen of a dir with no git dir should error")
	}
}

func TestBaseCommit_ReturnsCloneHead(t *testing.T) {
	// Build a source repo with one commit.
	src := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"},
		{"commit", "--allow-empty", "-q", "-m", "base"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	want, err := exec.Command("git", "-C", src, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}

	st, err := Prepare(t.TempDir(), src)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	got, err := st.BaseCommit()
	if err != nil {
		t.Fatalf("BaseCommit: %v", err)
	}
	if got != strings.TrimSpace(string(want)) {
		t.Errorf("BaseCommit = %q, want %q", got, strings.TrimSpace(string(want)))
	}
}

func TestReapOrphans_SkipsKeepSet(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"keepme", "reapme"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	n, err := ReapOrphans(root, map[string]bool{"keepme": true})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("reaped %d, want 1 (reapme only)", n)
	}
	if _, err := os.Stat(filepath.Join(root, "keepme")); err != nil {
		t.Error("keepme should have survived the reap")
	}
	if _, err := os.Stat(filepath.Join(root, "reapme")); !os.IsNotExist(err) {
		t.Error("reapme should have been removed")
	}
}
