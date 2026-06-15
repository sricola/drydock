// Package stage handles host-side repo I/O: clone the repo in, write the task
// inputs, capture the resulting diff, and push from the host. The VM never
// gets git remote access — only data (the diff) crosses the trust boundary.
package stage

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %v: %w\n%s", args, err, out)
	}
	return string(out), nil
}

func Clone(repoRef, dest string) error {
	_, err := git("", "clone", "--depth", "1", repoRef, dest)
	return err
}

func WriteTaskFiles(dest, prompt, allowlist string) error {
	dir := filepath.Join(dest, ".task")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte(prompt), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "allowlist.txt"), []byte(allowlist), 0o644)
}

// CaptureDiff stages all changes and returns the unified diff (without committing).
// The .task/ control dir (prompt + compiled allowlist, written by WriteTaskFiles
// into the repo root so the in-VM entrypoint can read them) is excluded so it
// never leaks into the diff, the approval gate, or the pushed PR.
func CaptureDiff(dir string) (string, error) {
	if _, err := git(dir, "add", "-A", "--", ".", ":(exclude).task"); err != nil {
		return "", err
	}
	return git(dir, "diff", "--cached")
}

// Push creates a branch, commits the staged work, pushes with host creds, and
// opens a PR. Called only after the broker's approval gate.
func Push(dir, branch, message string) error {
	if _, err := git(dir, "checkout", "-b", branch); err != nil {
		return err
	}
	if _, err := git(dir, "commit", "-m", message); err != nil {
		return err
	}
	if _, err := git(dir, "push", "origin", branch); err != nil {
		return err
	}
	cmd := exec.Command("gh", "pr", "create", "--head", branch, "--fill")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr create: %w\n%s", err, out)
	}
	return nil
}
