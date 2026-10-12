package broker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"drydock/internal/remote"
)

// A task resumed at the diff gate after a restart runs in its own goroutine,
// outside any HTTP handler and outside the dispatcher. brokerd's shutdown waits
// on srv.Shutdown (handlers) and WaitQueueDrain (dispatched queue items); the
// resumed gate must be covered by the same drain, or the process can exit while
// the cancelled resume is still writing its terminal result and cleaning its
// stage.
func TestWaitQueueDrain_CoversResumedGate(t *testing.T) {
	dir := t.TempDir()
	stageRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stageRoot, liveID, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, liveID+".diff"), []byte("diff"), 0o600)
	writeGateMarker(dir, liveID, gateMarker{RepoRef: "https://github.com/o/r", Agent: "claude", Platform: "github"})

	fs := &fakeStage{workDir: filepath.Join(stageRoot, liveID, "work")}
	b := &Broker{AuditRoot: dir,
		reopenStage: func(root string) (taskStage, error) { return fs, nil },
		newAdapter:  func(string, string) remote.Adapter { return &fakeAdapter{name: "github"} }}
	b.ResumeAwaiting(stageRoot)

	// Wait until the resumed task is sitting at the gate.
	if !waitFor(5*time.Second, func() bool {
		b.pendingMu.Lock()
		defer b.pendingMu.Unlock()
		return b.pending[liveID] != nil
	}) {
		t.Fatal("resumed task never reached the gate")
	}

	// Parked at the gate: the drain must not report completion.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if b.WaitQueueDrain(ctx) {
		t.Fatal("WaitQueueDrain returned while a resumed gate was still parked")
	}

	// Shutdown cancels it; the drain must then complete once it has unwound.
	b.CancelAll()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if !b.WaitQueueDrain(ctx2) {
		t.Fatal("WaitQueueDrain never completed after the resumed gate was cancelled")
	}
}
