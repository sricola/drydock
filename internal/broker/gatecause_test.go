package broker

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestAwaitGate_CauseFromContext(t *testing.T) {
	b := &Broker{}
	// Shutdown cause -> gateShutdown.
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errShutdown)
	if ok, cause := b.awaitGate(ctx, "t1", "to", "cx", func() {}); ok || cause != gateShutdown {
		t.Errorf("shutdown: ok=%v cause=%v, want false/gateShutdown", ok, cause)
	}
	// Kill cause -> gateKilled.
	ctx2, cancel2 := context.WithCancelCause(context.Background())
	cancel2(errTaskKilled)
	if ok, cause := b.awaitGate(ctx2, "t2", "to", "cx", func() {}); ok || cause != gateKilled {
		t.Errorf("kill: ok=%v cause=%v, want false/gateKilled", ok, cause)
	}
}

func TestAwaitGate_ApproveDeny(t *testing.T) {
	b := &Broker{}
	go func() {
		// wait until pending is registered, then approve
		for {
			b.pendingMu.Lock()
			ch := b.pending["t3"]
			b.pendingMu.Unlock()
			if ch != nil {
				ch <- true
				return
			}
		}
	}()
	if ok, cause := b.awaitGate(context.Background(), "t3", "to", "cx", func() {}); !ok || cause != gateApproved {
		t.Errorf("approve: ok=%v cause=%v, want true/gateApproved", ok, cause)
	}
}

func TestGatePush_MarkerLifecycle(t *testing.T) {
	dir := t.TempDir()
	b := &Broker{AuditRoot: dir}
	tr := &taskRun{b: b, id: "gp1", repoRef: "https://github.com/o/r",
		instruction: "x", platform: "github", agentName: "claude"}

	// Approve path: wait for the marker (written inside onReady, after channel
	// registration), verify it exists, then send approval. The marker must be
	// gone once gatePushMarked returns.
	go func() {
		for {
			b.pendingMu.Lock()
			ch := b.pending["gp1"]
			b.pendingMu.Unlock()
			if ch != nil {
				// onReady runs after channel registration; poll until the marker
				// appears to avoid a race between registration and the write.
				if !waitFor(500*time.Millisecond, func() bool {
					_, err := os.Stat(gateMarkerPath(dir, "gp1"))
					return err == nil
				}) {
					t.Errorf("marker should exist while awaiting gate")
				}
				ch <- true
				return
			}
		}
	}()
	ok, _ := b.gatePushMarked(context.Background(), tr, "diff x")
	if !ok {
		t.Fatal("expected approval")
	}
	if _, err := os.Stat(gateMarkerPath(dir, "gp1")); !os.IsNotExist(err) {
		t.Error("marker should be removed after approval")
	}
}

func TestGatePush_ShutdownLeavesMarker(t *testing.T) {
	dir := t.TempDir()
	b := &Broker{AuditRoot: dir}
	tr := &taskRun{b: b, id: "gp2", repoRef: "r", instruction: "x", platform: "github", agentName: "claude"}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errShutdown)
	ok, cause := b.gatePushMarked(ctx, tr, "diff")
	if ok || cause != gateShutdown {
		t.Fatalf("ok=%v cause=%v, want false/gateShutdown", ok, cause)
	}
	if _, err := os.Stat(gateMarkerPath(dir, "gp2")); err != nil {
		t.Error("marker must be LEFT on shutdown for boot resume")
	}
}
