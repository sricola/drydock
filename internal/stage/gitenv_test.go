package stage

import (
	"strings"
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

func TestGitHardenedEnv_AlwaysDisablesPrompts(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "") // ensure unset semantics via os.LookupEnv below
	// t.Setenv sets it to empty string, which still counts as "set" for
	// os.LookupEnv; unset it explicitly for the not-set case.
	t.Setenv("GIT_SSH_COMMAND", "")
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

func TestGitHardenedEnv_RespectsOperatorSSHCommand(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "ssh -i /custom/key")
	env := gitHardenedEnv(nil)
	if has(env, "GIT_SSH_COMMAND=ssh -oBatchMode=yes") {
		t.Error("operator GIT_SSH_COMMAND clobbered by BatchMode default")
	}
}

func TestGitHardenedEnv_BatchModeWhenUnset(t *testing.T) {
	// Simulate truly-unset: gitHardenedEnv keys the decision on
	// os.LookupEnv, so clear it for this test.
	t.Setenv("GIT_SSH_COMMAND", "x")
	// no direct way to unset via t.Setenv; use the helper's documented
	// treatment: an empty value counts as unset.
	t.Setenv("GIT_SSH_COMMAND", "")
	env := gitHardenedEnv(nil)
	if !has(env, "GIT_SSH_COMMAND=ssh -oBatchMode=yes") {
		t.Error("BatchMode default missing when operator has no GIT_SSH_COMMAND")
	}
}

func TestPushEnv_CarriesHardenedGitEnv(t *testing.T) {
	s := &Stage{WorkDir: t.TempDir(), gitDir: t.TempDir()}
	env := s.PushEnv()
	if !has(env, "GIT_TERMINAL_PROMPT=0") || !has(env, "GCM_INTERACTIVE=never") {
		t.Errorf("PushEnv missing hardened git env: %v", env)
	}
	if !hasKey(env, "GIT_DIR") {
		t.Errorf("PushEnv lost its existing GIT_DIR: %v", env)
	}
}
