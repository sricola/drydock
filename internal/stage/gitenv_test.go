package stage

import (
	"strings"
	"sync"
	"testing"
)

func has(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func hasKey(env []string, key string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return true
		}
	}
	return false
}

// withCoreSSHCommand overrides the coreSSHCommand() seam for the duration of
// a test: it stubs coreSSHCommandFunc and resets coreSSHCommandOnce/cached so
// the stub actually runs (the real lookup is memoized process-wide), then
// restores both on cleanup.
func withCoreSSHCommand(t *testing.T, value string) {
	t.Helper()
	prevFunc := coreSSHCommandFunc
	prevOnce := coreSSHCommandOnce
	prevCached := coreSSHCommandCached
	coreSSHCommandFunc = func() string { return value }
	coreSSHCommandOnce = &sync.Once{}
	t.Cleanup(func() {
		coreSSHCommandFunc = prevFunc
		coreSSHCommandOnce = prevOnce
		coreSSHCommandCached = prevCached
	})
}

func TestGitHardenedEnv_AlwaysDisablesPrompts(t *testing.T) {
	withCoreSSHCommand(t, "")
	env := gitHardenedEnv([]string{"PATH=/usr/bin"})
	if !has(env, "GIT_TERMINAL_PROMPT=0") {
		t.Error("GIT_TERMINAL_PROMPT=0 missing")
	}
	if !has(env, "GCM_INTERACTIVE=never") {
		t.Error("GCM_INTERACTIVE=never missing")
	}
	if !has(env, "PATH=/usr/bin") {
		t.Error("base env not preserved")
	}
}

// TestGitHardenedEnv_RespectsOperatorSSHCommand covers runGit/gitDiffCapped's
// shape: base is (effectively) os.Environ(), so a GIT_SSH_COMMAND entry in
// base is how the operator's transport reaches the decision.
func TestGitHardenedEnv_RespectsOperatorSSHCommand(t *testing.T) {
	withCoreSSHCommand(t, "")
	env := gitHardenedEnv([]string{"GIT_SSH_COMMAND=ssh -i /custom/key"})
	if has(env, "GIT_SSH_COMMAND=ssh -oBatchMode=yes") {
		t.Error("operator GIT_SSH_COMMAND clobbered by BatchMode default")
	}
}

func TestGitHardenedEnv_BatchModeWhenUnset(t *testing.T) {
	withCoreSSHCommand(t, "")
	env := gitHardenedEnv(nil)
	if !has(env, "GIT_SSH_COMMAND=ssh -oBatchMode=yes") {
		t.Error("BatchMode default missing when operator has no GIT_SSH_COMMAND and no core.sshCommand")
	}
}

// TestGitHardenedEnv_RespectsCoreSSHCommand covers an operator whose
// transport lives in git config (core.sshCommand) with no env var set: the
// BatchMode default must not clobber it either.
func TestGitHardenedEnv_RespectsCoreSSHCommand(t *testing.T) {
	withCoreSSHCommand(t, "ssh -i /configured/key")
	env := gitHardenedEnv(nil)
	if has(env, "GIT_SSH_COMMAND=ssh -oBatchMode=yes") {
		t.Error("core.sshCommand transport clobbered by BatchMode default")
	}
}

func TestPushEnv_CarriesHardenedGitEnv(t *testing.T) {
	withCoreSSHCommand(t, "")
	s := &Stage{WorkDir: t.TempDir(), gitDir: t.TempDir()}
	env := s.PushEnv()
	if !has(env, "GIT_TERMINAL_PROMPT=0") || !has(env, "GCM_INTERACTIVE=never") {
		t.Errorf("PushEnv missing hardened git env: %v", env)
	}
	if !hasKey(env, "GIT_DIR") {
		t.Errorf("PushEnv lost its existing GIT_DIR: %v", env)
	}
}

// TestPushEnv_RespectsOperatorSSHCommand is the PushEnv-with-custom-transport
// case (finding #2): adapterAllowedEnv now forwards GIT_SSH_COMMAND into the
// curated env PushEnv builds, so an operator's custom transport must reach
// gh/glab's internal git instead of being silently overridden by the
// BatchMode default.
func TestPushEnv_RespectsOperatorSSHCommand(t *testing.T) {
	withCoreSSHCommand(t, "")
	t.Setenv("GIT_SSH_COMMAND", "ssh -i /custom/key")
	s := &Stage{WorkDir: t.TempDir(), gitDir: t.TempDir()}
	env := s.PushEnv()
	if !has(env, "GIT_SSH_COMMAND=ssh -i /custom/key") {
		t.Errorf("PushEnv lost the operator's custom GIT_SSH_COMMAND: %v", env)
	}
	if has(env, "GIT_SSH_COMMAND=ssh -oBatchMode=yes") {
		t.Errorf("PushEnv let the BatchMode default clobber the custom transport: %v", env)
	}
}
