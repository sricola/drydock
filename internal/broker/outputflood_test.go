package broker

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// Drives the flood path through the real runSandbox wiring (not the outputCap
// type in isolation): an agent that writes past the cap must be cancelled, emit
// the "output exceeded" terminal reason, and never push. This is what makes the
// broker.go wiring revert-resistant, without it, deleting the cap wiring would
// leave the suite green.
func TestHandleTask_OutputFloodTerminatesAndDoesNotPush(t *testing.T) {
	orig := maxTaskOutputBytes
	maxTaskOutputBytes = 1024 // 1 KiB, so a small write trips it
	t.Cleanup(func() { maxTaskOutputBytes = orig })

	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	flood := func(ctx context.Context, _ []string, stdout, _ io.Writer) error {
		_, _ = stdout.Write(make([]byte, 4096)) // 4 KiB > 1 KiB cap: trips it
		// The cap fires runCancel; a real container run returns on ctx cancel.
		// The timeout fallback keeps a wiring regression from hanging the test.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
			return nil
		}
	}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, flood)

	_, _, terminal := submit(b, `{"repo_ref":"git@github.com:x/y","instruction":"go"}`)

	if terminal["event"] != "error" {
		t.Fatalf("terminal = %+v, want an error event from the output cap", terminal)
	}
	if reason, _ := terminal["reason"].(string); !strings.Contains(reason, "output exceeded") {
		t.Errorf("reason = %q, want it to mention the output cap", reason)
	}
	if st.pushed.Load() {
		t.Error("task pushed after an output-flood termination; want no push")
	}
}
