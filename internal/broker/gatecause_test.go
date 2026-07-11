package broker

import (
	"context"
	"testing"
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
