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

// WriteTaskFiles writes the agent prompt into the work tree's .task dir, read by
// the in-VM entrypoint. Excluded from the captured diff. Egress is enforced
// host-side by squid (per-domain host+port ACLs); no allowlist file is written
// into the VM — nothing in-VM reads one, and shipping a bogus "allowlist" there
// would falsely imply in-VM enforcement.
func (s *Stage) WriteTaskFiles(prompt string) error {
	dir := filepath.Join(s.WorkDir, ".task")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte(prompt), 0o644)
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

// adapterAllowedEnv is the curated allowlist of host env vars forwarded to
// gh/glab. The rest of os.Environ() (AWS_*, SLACK_*, cloud creds, every
// secret you've ever exported) stays out — drastically narrows the blast
// radius of a future gh plugin or supply-chain compromise.
var adapterAllowedEnv = []string{
	"PATH",    // find gh, glab, git
	"HOME",    // gh/glab read ~/.config/gh, ~/.config/glab
	"USER",    // some CLIs require it
	"LOGNAME", // ditto
	"LANG",    // utf-8 output
	"LC_ALL",
	"LC_CTYPE",
	"TMPDIR", // gh writes per-request temp files
	// Vendor tokens — forwarded only if explicitly present.
	"GH_TOKEN",
	"GITHUB_TOKEN",
	"GH_HOST",
	"GH_ENTERPRISE_TOKEN",
	"GLAB_TOKEN",
	"GITLAB_TOKEN",
	"GITLAB_HOST",
	// SSH auth path for git push over ssh (glab/gh push via git).
	"SSH_AUTH_SOCK",
}

// Commit creates the branch and records all agent changes as one commit on the
// host-only git dir. Run once per task.
func (s *Stage) Commit(branch, message string) error {
	if _, err := s.git("checkout", "-b", branch); err != nil {
		return err
	}
	if err := s.stageAll(); err != nil {
		return err
	}
	if _, err := s.git("commit", "-m", message); err != nil {
		return err
	}
	return nil
}

// PushBranch pushes the committed local branch to remoteBranch on origin. The
// explicit refspec lets recovery push the same commit to a fresh remote name
// after a branch-name collision, without re-committing.
func (s *Stage) PushBranch(localBranch, remoteBranch string) error {
	_, err := s.git("push", "origin", localBranch+":"+remoteBranch)
	return err
}

// Push commits then pushes to the same-named remote branch. Kept for callers
// that do not need recovery.
func (s *Stage) Push(branch, message string) error {
	if err := s.Commit(branch, message); err != nil {
		return err
	}
	return s.PushBranch(branch, branch)
}

// PushEnv is the curated host env a PR/MR adapter must run with: the allowlisted
// vars plus GIT_DIR and hook-neutralization that keep any vendor CLI on the
// host-only git dir even if the work tree contains a planted .git. The broker
// passes this to remote.Adapter.OpenRequest after Push succeeds.
func (s *Stage) PushEnv() []string {
	env := curatedEnv()
	env = append(env,
		"GIT_DIR="+s.gitDir,
		"GIT_WORK_TREE="+s.WorkDir,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null",
		"GIT_CONFIG_KEY_1=core.fsmonitor", "GIT_CONFIG_VALUE_1=false",
	)
	return env
}

func curatedEnv() []string {
	out := make([]string, 0, len(adapterAllowedEnv))
	for _, key := range adapterAllowedEnv {
		if v, ok := os.LookupEnv(key); ok {
			out = append(out, key+"="+v)
		}
	}
	return out
}

// Cleanup removes the entire host scratch dir (work tree + git dir).
// Defense in depth: never RemoveAll an empty or root-shaped path so a
// misconfigured StageRoot ("" or "/") can't catastrophically widen the
// blast radius. The path must be absolute and not the FS root.
func (s *Stage) Cleanup() error {
	clean := filepath.Clean(s.Root)
	if clean == "" || clean == "/" || clean == "." || !filepath.IsAbs(clean) {
		return fmt.Errorf("stage: refusing to clean unsafe path %q", s.Root)
	}
	return os.RemoveAll(s.Root)
}

// ReapOrphans removes every child directory under root. Used at brokerd boot to
// clear stage dirs orphaned by a crash (the per-task Cleanup defer never ran).
// SAFE ONLY AT BOOT, when no task is live. Applies the same guard as Cleanup so
// a misconfigured (empty/relative/root-shaped) StageRoot can't widen the blast
// radius. A missing root is a no-op. Non-directory entries are left untouched.
// Returns the count of dirs reaped and the first error (per-entry errors are
// non-fatal and don't abort the sweep).
func ReapOrphans(root string) (int, error) {
	clean := filepath.Clean(root)
	if clean == "" || clean == "/" || clean == "." || !filepath.IsAbs(clean) {
		return 0, fmt.Errorf("stage: refusing to reap unsafe root %q", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	var firstErr error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if rerr := os.RemoveAll(filepath.Join(root, e.Name())); rerr != nil {
			if firstErr == nil {
				firstErr = rerr
			}
			continue
		}
		n++
	}
	return n, firstErr
}
