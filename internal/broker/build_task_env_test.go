package broker

import (
	"strings"
	"testing"
)

// TestBuildTaskEnv verifies the env slice produced by buildTaskEnv. It is pure
// (no Broker, no container) so it can run without any seams or fakes.
func TestBuildTaskEnv_ContainsExpectedVars(t *testing.T) {
	const (
		bearer = "tok_ephemeral_scoped_9c1d"
		gwIP   = "10.0.0.1"
		proxy  = "3128"
	)
	// grantEnv carries only the ephemeral bearer — the real upstream key stays on
	// the host. buildTaskEnv is never given the real key, so this test does NOT
	// assert its absence (that would be tautological); the genuine A1 property —
	// grant.EnvVars() emits only the bearer, never the real key — is proven in
	// internal/gateway/provider_test.go (TestRedteam_A1_*). Here we only check
	// buildTaskEnv forwards the grant env and proxy vars, and drops nothing.
	grantEnv := []string{"ANTHROPIC_AUTH_TOKEN=" + bearer}

	env := buildTaskEnv(grantEnv, "" /*proxyAuth*/, gwIP, 3128,
		"claude", "" /*taskModel*/, "" /*ocModel*/, "claude-sonnet-4-6" /*opDefault*/, "anthropic")

	joined := strings.Join(env, "\n")

	// The ephemeral bearer must be forwarded to the container.
	if !strings.Contains(joined, bearer) {
		t.Errorf("expected bearer %q in env:\n%s", bearer, joined)
	}

	// Proxy vars must be set.
	if !strings.Contains(joined, "HTTPS_PROXY=http://"+gwIP+":"+proxy) {
		t.Errorf("HTTPS_PROXY missing or wrong in env:\n%s", joined)
	}
	if !strings.Contains(joined, "HTTP_PROXY=http://"+gwIP+":"+proxy) {
		t.Errorf("HTTP_PROXY missing or wrong in env:\n%s", joined)
	}
	if !strings.Contains(joined, "NO_PROXY=127.0.0.1,localhost,"+gwIP) {
		t.Errorf("NO_PROXY missing or wrong in env:\n%s", joined)
	}
	if !strings.Contains(joined, "DRYDOCK_GW_IP="+gwIP) {
		t.Errorf("DRYDOCK_GW_IP missing in env:\n%s", joined)
	}

	// Operator default model forwarded for non-compat agent.
	if !strings.Contains(joined, "DRYDOCK_MODEL=claude-sonnet-4-6") {
		t.Errorf("operator default model missing for anthropic lane:\n%s", joined)
	}

	// Agent name forwarded.
	if !strings.Contains(joined, "DRYDOCK_AGENT=claude") {
		t.Errorf("DRYDOCK_AGENT missing in env:\n%s", joined)
	}
}

// TestBuildTaskEnv_ProxyAuthIncluded verifies the per-task proxy credential
// (user:secret@) is spliced into the proxy URLs when provided.
func TestBuildTaskEnv_ProxyAuthIncluded(t *testing.T) {
	proxyAuth := "task-abc:mysecret@"
	env := buildTaskEnv([]string{"ANTHROPIC_AUTH_TOKEN=tok"}, proxyAuth, "192.168.64.1", 3128,
		"claude", "", "", "", "anthropic")
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "http://task-abc:mysecret@192.168.64.1:3128") {
		t.Errorf("proxy credential not present in proxy URLs:\n%s", joined)
	}
}

// TestBuildTaskEnv_OpenAICompatModelNotLeaked mirrors the operator-default
// isolation check: when the vendor is openai-compat, the operator default model
// must NOT appear in the env (effectiveDefaultModel blanks it).
func TestBuildTaskEnv_OpenAICompatModelNotLeaked(t *testing.T) {
	const opDefault = "claude-sonnet-4-6"
	env := buildTaskEnv([]string{"TOKEN=x"}, "", "10.0.0.1", 3128,
		"opencode", "", "gemini-2.5-pro", opDefault, "openai-compat")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, opDefault) {
		t.Errorf("operator default model %q must not appear in openai-compat env:\n%s", opDefault, joined)
	}
	// The openai_compat.model should be forwarded instead.
	if !strings.Contains(joined, "DRYDOCK_MODEL=gemini-2.5-pro") {
		t.Errorf("openai_compat.model not forwarded:\n%s", joined)
	}
}

// The native google (gemini) vendor sets NoOperatorDefault, so the operator's
// claude/codex-oriented default_model must not reach the Gemini CLI — the
// entrypoint supplies the gemini default instead. Guards the call-path
// consequence of the registry row's NoOperatorDefault field.
func TestBuildTaskEnv_GoogleModelNotLeaked(t *testing.T) {
	const opDefault = "claude-sonnet-4-6"
	env := buildTaskEnv([]string{"TOKEN=x"}, "", "10.0.0.1", 3128,
		"gemini", "", "", opDefault, "google")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, opDefault) {
		t.Errorf("operator default model %q must not leak into the google lane:\n%s", opDefault, joined)
	}
}
