package broker

import (
	"os"
	"testing"
)

// A brokerd crash mid-write leaves a temp file beside the queue item, and
// under launchd the restart comes within seconds. Boot reconcile must still
// be able to write that item (here: dead-letter the interrupted `running`
// item): a previous process's temp, under either the per-process name or the
// fixed name an older build used, is never in this process's way.
func TestResumeQueue_StaleTempFromCrashedProcessDoesNotBlockReconcile(t *testing.T) {
	var runs int64
	b := queueBroker(t, 2, countingAgent(&runs))
	id := queueTestID('f')
	writeQueueFixture(t, b, id, QueueRunning)
	path := queueMarkerPath(b.AuditRoot, id)
	for _, p := range []string{path + ".4242.tmp", path + ".tmp"} {
		if err := os.WriteFile(p, []byte("half"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.ResumeQueue(b.StageRoot); err != nil {
		t.Fatalf("ResumeQueue: %v", err)
	}
	if got := queueItemState(t, b, id).State; got != QueueDeadLetter {
		t.Fatalf("state after boot reconcile = %q, want dead_letter", got)
	}
}
