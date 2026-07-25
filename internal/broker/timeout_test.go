package broker

import (
	"context"
	"io"
	"testing"
	"time"
)

// The task_timeout security default (b.Timeout) must actually terminate a
// stuck run: the run context is cancelled at expiry, the task ends in an
// error terminal, and nothing is pushed. Before this test the only coverage
// was a value pin (TestDefaults_MatchV01EnvFallbacks), which never drove the
// enforcement path in runSandbox.
func TestHandleTask_TimeoutTerminatesAndDoesNotPush(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "d"}
	block := func(ctx context.Context, _ []string, _, _ io.Writer) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
			return nil // the timeout failed to fire; let the assertion catch it
		}
	}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, block)
	b.Timeout = 40 * time.Millisecond // tiny vs the 2s fallback above

	_, events, terminal := submit(b, `{"repo_ref":"git@github.com:x/y","instruction":"go"}`)

	if terminal["event"] != "error" {
		t.Fatalf("terminal = %+v, want an error event from the task_timeout expiry", terminal)
	}
	found := false
	for _, s := range stages(events) {
		if s == "running" {
			found = true
		}
	}
	if !found {
		t.Error("no \"running\" stage event; want proof the task started before it was terminated")
	}
	if st.pushed.Load() {
		t.Error("task pushed after a task_timeout termination; want no push")
	}
}
