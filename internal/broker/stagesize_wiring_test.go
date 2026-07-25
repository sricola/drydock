package broker

import (
	"context"
	"errors"
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

// Quota attach failure fails the task closed: no clone happens onto an
// unbounded plain dir when the operator configured a hard bound (F-04).
func TestHandleTask_QuotaAttachFailureFailsClosed(t *testing.T) {
	staged := false
	st := &fakeStage{workDir: t.TempDir()}
	b := testBroker(t, "anthropic", st, &fakeGrant{},
		func(context.Context, []string, io.Writer, io.Writer) error { return nil })
	b.StageQuotaBytes = 1 << 30
	b.attachQuota = func(string, int64) error { return errors.New("hdiutil boom") }
	b.prepareStage = func(context.Context, string, string) (taskStage, error) { staged = true; return st, nil }

	_, _, terminal := submit(b, `{"repo_ref":"git@github.com:x/y","instruction":"go"}`)

	if terminal["event"] != "error" {
		t.Fatalf("terminal = %+v, want an error event", terminal)
	}
	if reason, _ := terminal["reason"].(string); !strings.Contains(reason, "quota") {
		t.Errorf("reason = %q, want it to mention the stage quota", reason)
	}
	if staged {
		t.Error("stage was prepared despite the quota failure; want fail closed")
	}
}

// With a quota configured, HandleTask attaches it at the task's stage dir
// with the configured size before preparing the stage.
func TestHandleTask_QuotaAttachedBeforePrepare(t *testing.T) {
	var gotRoot string
	var gotSize int64
	attached := false
	st := &fakeStage{workDir: t.TempDir()}
	b := testBroker(t, "anthropic", st, &fakeGrant{},
		func(context.Context, []string, io.Writer, io.Writer) error { return nil })
	b.StageQuotaBytes = 2 << 30
	b.attachQuota = func(root string, size int64) error {
		attached, gotRoot, gotSize = true, root, size
		return nil
	}
	b.prepareStage = func(_ context.Context, root string, _ string) (taskStage, error) {
		if !attached {
			t.Error("prepareStage ran before the quota was attached")
		}
		if root != gotRoot {
			t.Errorf("prepare root %q != quota root %q", root, gotRoot)
		}
		return st, nil
	}

	_, _, terminal := submit(b, `{"repo_ref":"git@github.com:x/y","instruction":"go"}`)

	if terminal["event"] == "error" {
		t.Fatalf("unexpected error terminal: %+v", terminal)
	}
	if gotSize != 2<<30 {
		t.Errorf("quota size = %d, want %d", gotSize, int64(2<<30))
	}
}

// The stage-size guard's free-floor must be measured on the HOST filesystem
// (b.StageRoot), never on filepath.Dir(stageRoot) (the quota image's own
// mountpoint): a silent revert there would make the guard measure the
// image's own free space and never trip. Pins the wiring via a seam so a
// revert fails this test instead of only a code comment.
func TestRunSandbox_FreeFloorMeasuredOnHostRoot(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "d"}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, writesResult(`{"type":"result","subtype":"success"}`))

	var gotHost string
	b.watchStage = func(root, hostRoot string, iv time.Duration, onExceed func()) *stageSizeGuard {
		gotHost = hostRoot
		return watchStageSize(root, hostRoot, iv, onExceed)
	}

	_, _, terminal := submit(b, `{"repo_ref":"git@github.com:x/y","instruction":"go","auto_approve":true}`)

	if terminal["event"] == "error" {
		t.Fatalf("unexpected error terminal: %+v", terminal)
	}
	if gotHost != b.StageRoot {
		t.Errorf("hostRoot = %q, want b.StageRoot %q", gotHost, b.StageRoot)
	}
	if gotHost == filepath.Dir(st.WorkDir()) {
		t.Error("hostRoot equals filepath.Dir(stageRoot); want the host root, not the quota image mountpoint's parent")
	}
}
