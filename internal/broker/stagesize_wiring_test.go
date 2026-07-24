package broker

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Preflight: a task must be refused before staging when host free space is below
// the floor, rather than piling a fresh clone + run onto a nearly-full disk.
func TestHandleTask_LowDiskRefusesBeforeStaging(t *testing.T) {
	orig := minFreeStageBytes
	minFreeStageBytes = 1 << 62 // require an impossible amount of free space
	t.Cleanup(func() { minFreeStageBytes = orig })

	staged := false
	prepared := &fakeStage{workDir: t.TempDir()}
	b := testBroker(t, "anthropic", prepared, &fakeGrant{},
		func(context.Context, []string, io.Writer, io.Writer) error { return nil })
	b.prepareStage = func(context.Context, string, string) (taskStage, error) { staged = true; return prepared, nil }

	_, _, terminal := submit(b, `{"repo_ref":"git@github.com:x/y","instruction":"go"}`)

	if terminal["event"] != "error" {
		t.Fatalf("terminal = %+v, want an error event", terminal)
	}
	if reason, _ := terminal["reason"].(string); !strings.Contains(reason, "low on disk") {
		t.Errorf("reason = %q, want it to mention low disk", reason)
	}
	if staged {
		t.Error("stage was prepared despite the low-disk preflight; want refused before staging")
	}
}

// Size guard through the real runSandbox wiring: a task whose /work grows past
// the cap is cancelled with the /work reason and never pushes. Makes the
// broker.go wiring revert-resistant.
func TestHandleTask_StageFillTerminatesAndDoesNotPush(t *testing.T) {
	ob, oi := maxStageBytes, stageSizeInterval
	maxStageBytes = 1024
	stageSizeInterval = 10 * time.Millisecond
	t.Cleanup(func() { maxStageBytes = ob; stageSizeInterval = oi })

	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "fill"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &fakeStage{workDir: work, diff: "d"}
	block := func(ctx context.Context, _ []string, _, _ io.Writer) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
			return nil // the guard failed to cancel; let the assertion catch it
		}
	}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, block)

	_, _, terminal := submit(b, `{"repo_ref":"git@github.com:x/y","instruction":"go"}`)

	if terminal["event"] != "error" {
		t.Fatalf("terminal = %+v, want an error event from the size guard", terminal)
	}
	if reason, _ := terminal["reason"].(string); !strings.Contains(reason, "/work") {
		t.Errorf("reason = %q, want it to mention the /work disk cap", reason)
	}
	if st.pushed.Load() {
		t.Error("task pushed after a stage-fill termination; want no push")
	}
}
