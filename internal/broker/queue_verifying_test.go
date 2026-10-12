package broker

import (
	"context"
	"io"
	"testing"
	"time"
)

// The durable queue lifecycle documented in submitting-tasks.md is
// queued -> preparing -> running -> verifying -> awaiting_review -> ..., and
// the transition table has carried `verifying` since the verifier shipped.
// A queued task that enters the verifier stage must therefore be observable
// as `verifying` in `drydock queue list`, exactly as it is observable as
// `awaiting_review` at the gate, and not sit as `running` until the gate.
func TestQueuedTask_EntersVerifyingState(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	b := queueBroker(t, 1, func(ctx context.Context, args []string, stdout, _ io.Writer) error {
		if isVerifyArgs(args) {
			close(entered)
			<-release
			return nil
		}
		io.WriteString(stdout, `{"type":"result","subtype":"success","is_error":false,"num_turns":1}`+"\n")
		return nil
	})
	b.prepareStage = func(context.Context, string, string) (taskStage, error) {
		return &verifyStage{
			fakeStage: &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"},
			trees:     []string{"tree-sha-1"},
		}, nil
	}
	b.Verify = map[string]VerifyRepo{
		"github.com/o/r": {Commands: [][]string{{"go", "test"}}},
	}
	id, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git", Instruction: "x", AutoApprove: true})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	b.StartDispatcher()
	defer b.StopDispatcher()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("verify never started")
	}
	// The verifier is parked: the durable record must already say so.
	if got := queueItemState(t, b, id).State; got != QueueVerifying {
		t.Errorf("queue state while verifying = %q, want %q", got, QueueVerifying)
	}
	close(release)
	waitForQueueState(t, b, id, QueueCompleted)
}
