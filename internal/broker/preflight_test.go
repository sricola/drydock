package broker

import (
	"errors"
	"strings"
	"testing"
)

// preflightStage wraps fakeStage with the optional PushPreflight capability.
type preflightStage struct {
	fakeStage
	preflightErr error
	gotBranch    string
}

func (p *preflightStage) PushPreflight(branch string) error {
	p.gotBranch = branch
	return p.preflightErr
}

func TestHandleTask_PushPreflightAuthFailure_FailsBeforeAnyWork(t *testing.T) {
	// Shaped like runGit's wrap ("git %v: %w\n%s"): argv first, then a
	// newline, then git's actual combined output. The emitted reason must
	// come from the line AFTER the newline (git's message), never the argv.
	st := &preflightStage{
		fakeStage: fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"},
		preflightErr: errors.New(
			"git [--git-dir=/Users/op/.drydock/stage/t1/git --work-tree=/Users/op/.drydock/stage/t1/work " +
				"push --dry-run origin HEAD:refs/heads/agent/t1]: exit status 128\n" +
				"fatal: Authentication failed for 'https://github.com/o/r.git'"),
	}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	_, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"do x","agent":"claude","auto_approve":true}`)

	if term["event"] != "error" {
		t.Fatalf("terminal=%v, want error event", term)
	}
	reason, _ := term["reason"].(string)
	if !strings.Contains(reason, "push preflight failed (auth)") {
		t.Errorf("reason=%q, want push preflight failed (auth)", reason)
	}
	if !strings.Contains(reason, "fatal: Authentication failed for 'https://github.com/o/r.git'") {
		t.Errorf("reason=%q, want git's fatal line, not the argv echo", reason)
	}
	if strings.Contains(reason, "--git-dir") {
		t.Errorf("reason=%q leaks the argv/stage path", reason)
	}
	if hint, _ := term["hint"].(string); hint == "" {
		t.Error("auth-class preflight failure carries no hint")
	}
	id, _ := events[0]["task_id"].(string)
	if st.gotBranch != "agent/"+id {
		t.Errorf("probe branch=%q, want agent/%s", st.gotBranch, id)
	}
	// Fails BEFORE any task work: no prompt written, nothing pushed.
	if st.gotPrompt != "" {
		t.Errorf("WriteTaskFiles ran despite failed preflight (prompt=%q)", st.gotPrompt)
	}
	if st.pushed.Load() {
		t.Error("push happened despite failed preflight")
	}
}

func TestHandleTask_PushPreflightPasses_TaskRuns(t *testing.T) {
	st := &preflightStage{fakeStage: fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	_, _, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	if term["outcome"] != "pushed" {
		t.Fatalf("terminal=%v, want pushed (probe passed)", term)
	}
}
