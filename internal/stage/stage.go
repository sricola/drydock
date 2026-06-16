// Package stage handles host-side repo I/O. The .git dir is kept host-only and
// is NEVER mounted into the VM: only the work tree crosses into the sandbox, and
// only the captured diff crosses back. This prevents untrusted in-VM code from
// (a) reading clone-URL credentials in .git/config, or (b) planting git
// hooks/config that the host-side push would execute on the trusted host.
package stage

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Stage is a prepared task workspace.
type Stage struct {
	Root    string // host scratch root (removed by Cleanup)
	WorkDir string // the ONLY path mounted into the VM
	gitDir  string // host-only git dir; never exposed to the VM
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %v: %w\n%s", args, err, out)
	}
	return string(out), nil
}

// Prepare clones repoRef on the host, then separates the work tree (mounted into
// the VM) from the .git dir (host-only). After Prepare, WorkDir contains no .git.
func Prepare(root, repoRef string) (*Stage, error) {
	clone := filepath.Join(root, "clone")
	if _, err := runGit("", "clone", "--depth", "1", repoRef, clone); err != nil {
		return nil, err
	}
	workDir := filepath.Join(root, "work")
	gitDir := filepath.Join(root, "git")
	if err := os.Rename(filepath.Join(clone, ".git"), gitDir); err != nil {
		return nil, fmt.Errorf("separate git dir: %w", err)
	}
	if err := os.Rename(clone, workDir); err != nil {
		return nil, fmt.Errorf("move work tree: %w", err)
	}
	return &Stage{Root: root, WorkDir: workDir, gitDir: gitDir}, nil
}

// git runs a host-side git command bound to the separated git dir and work tree,
// with hooks and fsmonitor neutralized so nothing the VM may have written into
// the work tree (a planted .git/hooks or core.fsmonitor) executes on the host.
func (s *Stage) git(args ...string) (string, error) {
	full := append([]string{
		"--git-dir=" + s.gitDir,
		"--work-tree=" + s.WorkDir,
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
	}, args...)
	return runGit(s.WorkDir, full...)
}

// WriteTaskFiles writes the prompt and compiled allowlist into the work tree's
// .task dir, read by the in-VM entrypoint. Excluded from the captured diff.
func (s *Stage) WriteTaskFiles(prompt, allowlist string) error {
	dir := filepath.Join(s.WorkDir, ".task")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte(prompt), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "allowlist.txt"), []byte(allowlist), 0o644)
}

// stageAll stages every change except the control dir and a top-level .git. (A
// nested work-tree .git is a gitlink boundary git already refuses to recurse
// into, so its contents and hooks never enter the diff/commit either way.)
func (s *Stage) stageAll() error {
	_, err := s.git("add", "-A", "--", ".", ":(exclude).task", ":(exclude).git")
	return err
}

// CaptureDiff returns the unified diff of the agent's changes (no commit).
func (s *Stage) CaptureDiff() (string, error) {
	if err := s.stageAll(); err != nil {
		return "", err
	}
	return s.git("diff", "--cached")
}

// RequestOpener opens a PR/MR for a freshly pushed branch. Satisfied by
// the adapters in internal/remote (GitHub / GitLab / push-only). Kept as
// an interface here so stage doesn't need to import remote — broker wires
// the two together. Adapters MUST honor the env vars (they carry GIT_DIR
// and the hook-neutralization that keeps any vendor CLI on the host-only
// git dir even if the work tree contains a planted .git).
type RequestOpener interface {
	OpenRequest(workDir, branch string, env []string) error
}

// Push commits the staged changes onto a new branch, pushes it via the
// host-only git dir, then asks the adapter to open a PR/MR. Called only
// after the approval gate.
func (s *Stage) Push(opener RequestOpener, branch, message string) error {
	if _, err := s.git("checkout", "-b", branch); err != nil {
		return err
	}
	if err := s.stageAll(); err != nil {
		return err
	}
	if _, err := s.git("commit", "-m", message); err != nil {
		return err
	}
	if _, err := s.git("push", "origin", branch); err != nil {
		return err
	}
	// The adapter shells out to gh/glab and must do so against the
	// host-only git dir with hooks neutralized — never the work-tree .git.
	env := append(os.Environ(),
		"GIT_DIR="+s.gitDir,
		"GIT_WORK_TREE="+s.WorkDir,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null",
		"GIT_CONFIG_KEY_1=core.fsmonitor", "GIT_CONFIG_VALUE_1=false",
	)
	return opener.OpenRequest(s.WorkDir, branch, env)
}

// Cleanup removes the entire host scratch dir (work tree + git dir).
func (s *Stage) Cleanup() error {
	return os.RemoveAll(s.Root)
}
