package broker

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// This file is the round-5 fix set for the bounded-retry PARK — the process-
// local shadow (Broker.ciRetryParked) and the bound measured over it. Every
// test here fails against the code as it was before its fix.
//
//	F1  A TRANSIENT READ FAULT WIPED A SHADOW-ONLY PARK and destroyed the chain.
//	F2  THE SHADOW LEAKED for parents that vanished while parked.
//	F4  REPEATED BACKWARD WALL-CLOCK JUMPS kept a park alive indefinitely.

// parkShadow reads the process-local park shadow for id under the lock that
// owns it, so the race detector has nothing to say about these assertions.
func parkShadow(b *Broker, id string) int64 {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	return b.ciRetryParked[id]
}

// setParkShadow plants a shadow entry directly. It is used for ONE thing: to
// construct the state a read fault at the CLEARING call leaves behind (a park
// shadow that outlived the decision it belonged to), which is otherwise only
// reachable by faulting a single read inside applyCIObservation.
func setParkShadow(b *Broker, id string, at int64) {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	if b.ciRetryParked == nil {
		b.ciRetryParked = map[string]int64{}
	}
	b.ciRetryParked[id] = at
}

// faultQueueItemRead makes readQueueItem fail for id — the file is replaced by a
// DIRECTORY, which is what a read fault looks like from the caller's side (an
// error that is not "gone", and one that clears). It returns the restore.
func faultQueueItemRead(t *testing.T, b *Broker, id string) func() {
	t.Helper()
	path := filepath.Join(b.AuditRoot, id+".queue.json")
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("faultQueueItemRead: read %s: %v", path, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, saved, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// parkShadowOnly drives a parent into a SHADOW-ONLY park: its terminal is
// durable, its deferral flag is not (queue-item writes were failing when the
// park was taken, which is the fault the park exists for), and nothing has been
// enqueued. Returns the parent's id.
func parkShadowOnly(t *testing.T, b *Broker, parentID string) {
	t.Helper()
	if _, err := b.setQueueState(parentID, QueueCIFailed, func(q *QueueItem) {
		q.CIState = string(CIFailed)
	}); err != nil {
		t.Fatalf("setup: terminal transition: %v", err)
	}
	// A directory on the atomic-write temp path is a full or read-only disk.
	qtmp := filepath.Join(b.AuditRoot, parentID+".queue.json.tmp")
	if err := os.Mkdir(qtmp, 0o700); err != nil {
		t.Fatal(err)
	}
	b.setCIRetryDeferred(parentID, true)
	if err := os.Remove(qtmp); err != nil {
		t.Fatal(err)
	}
	cur, err := readQueueItem(b.AuditRoot, parentID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.CIRetryDeferred {
		t.Fatal("setup: the deferral flag reached disk, so the shadow is not what is under test")
	}
	if parkShadow(b, parentID) == 0 {
		t.Fatal("setup: no shadow was recorded, so there is nothing for the fault to wipe")
	}
}

// ---- F1 ----

// TestCIRetry_ATransientReadFaultDoesNotWipeAShadowOnlyPark.
//
// setCIRetryDeferred used to `delete` the shadow UNCONDITIONALLY at the top of
// the function, ABOVE the readQueueItem that would have told it the item was
// unreachable. One transient read of `<id>.queue.json` was then enough to
// destroy a retry chain permanently:
//
//	recordCIQueueTerminal maps ANY read error to "synchronous task, nothing to
//	move" -> (qs="", replay=false); maybeEnqueueCIRetry refuses at gate 2 with
//	park=false; and the not-parked branch calls setCIRetryDeferred(id, false),
//	which wiped the shadow. The durable flag said nothing had been deferred —
//	its own write is what failed, which is why the park was shadow-only — so
//	every later pass short-circuited as a replay and no child ever existed.
//
// Note the ASYMMETRY the fix removes: a DURABLE park always survived this fault,
// because the clearing write cannot reach the file either. Only the shadow was
// destroyed by it.
func TestCIRetry_ATransientReadFaultDoesNotWipeAShadowOnlyPark(t *testing.T) {
	b := retryBroker(t, 3)
	b.CIWatchTimeout = time.Hour
	it := seedTaskAwaitingCI(t, b, baseTask())
	obs, parentID := failedObs(b, it), it.ID
	b.GlobalMaxTasks = 100
	b.GlobalLedger = capLedgerAt(t, b.AuditRoot, time.Hour, 0, 0, b.nowMs())
	parkShadowOnly(t, b, parentID)
	before := parkShadow(b, parentID)

	// ONE pass under a read fault. This is the whole bug: nothing about it is
	// concurrent, and nothing about it needs a crash.
	restore := faultQueueItemRead(t, b, parentID)
	b.applyCIObservation(obs)
	restore()

	if after := parkShadow(b, parentID); after == 0 {
		t.Fatalf("a transient read fault wiped the park shadow (before=%d after=%d); "+
			"the durable flag says nothing was deferred, so every later pass short-circuits as a replay "+
			"and the retry chain is destroyed", before, after)
	}

	// The fault has cleared. The decision must still be re-asked.
	b.applyCIObservation(obs)
	child, ok := childOf(t, b, parentID)
	if !ok {
		t.Fatal("no child after the fault cleared: the chain was destroyed by one transient read")
	}
	// ...and still exactly once, over as many replays as anyone cares to drive.
	for i := 0; i < 5; i++ {
		b.applyCIObservation(obs)
	}
	if again, _ := childOf(t, b, parentID); again.ID != child.ID {
		t.Fatalf("the parent's child changed identity across replays: %s -> %s", child.ID, again.ID)
	}
}

// TestCIRetry_AShadowThatOutlivedItsDecisionStillMintsOneChild is the invariant
// the fix above must not reopen, and it is re-tested rather than assumed because
// enqueue-once has broken twice on this path.
//
// The state under test is the one a read fault at the CLEARING call leaves: a
// child EXISTS, and the shadow that belonged to the park before it was never
// removed. The shadow widens "the decision is unmade", so recordCIQueueTerminal
// falls through and re-asks — and every gate between a re-ask and an Enqueue
// reads the DURABLE record, so all of them refuse.
func TestCIRetry_AShadowThatOutlivedItsDecisionStillMintsOneChild(t *testing.T) {
	b := retryBroker(t, 5)
	b.CIWatchTimeout = time.Hour
	it := seedTaskAwaitingCI(t, b, baseTask())
	obs, parentID := failedObs(b, it), it.ID
	b.GlobalMaxTasks = 100
	b.GlobalLedger = capLedgerAt(t, b.AuditRoot, time.Hour, 0, 0, b.nowMs())

	b.applyCIObservation(obs)
	child, ok := childOf(t, b, parentID)
	if !ok {
		t.Fatal("setup: the first decision minted no child")
	}
	// The clearing read faulted, so the shadow outlived the decision it belonged
	// to. (Planted rather than faulted: the clear happens inside the same call
	// that mints the child, so there is no seam between them to fault.)
	setParkShadow(b, parentID, b.ceilingNowMs())

	for i := 0; i < 5; i++ {
		b.applyCIObservation(obs)
	}
	again, ok := childOf(t, b, parentID)
	if !ok {
		t.Fatal("the child vanished")
	}
	if again.ID != child.ID {
		t.Fatalf("a shadow that outlived its decision minted a SECOND child: %s -> %s", child.ID, again.ID)
	}
}

// TestCIRetry_AnItemThatIsGoneDoesNotResurrectADecision. The read-fault fix
// retains the shadow for an item that cannot be READ; an item that is GONE must
// still not produce a child, because gate 4 has no persisted invocation to
// re-run. Recorded so "retain the shadow" is never read as "retry anyway".
func TestCIRetry_AnItemThatIsGoneDoesNotResurrectADecision(t *testing.T) {
	b := retryBroker(t, 3)
	b.CIWatchTimeout = time.Hour
	it := seedTaskAwaitingCI(t, b, baseTask())
	obs, parentID := failedObs(b, it), it.ID
	b.GlobalMaxTasks = 100
	b.GlobalLedger = capLedgerAt(t, b.AuditRoot, time.Hour, 0, 0, b.nowMs())
	parkShadowOnly(t, b, parentID)

	if err := os.Remove(filepath.Join(b.AuditRoot, parentID+".queue.json")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		b.applyCIObservation(obs)
	}
	if child, ok := childOf(t, b, parentID); ok {
		t.Errorf("a parent with no queue item minted a child: %s", child.ID)
	}
}

// ---- F2 ----

// TestCIRetry_TheParkShadowIsReapedWhenTheParentIsGone.
//
// Broker.ciRetryParked's own comment claimed "entries are removed as soon as the
// decision is made", which is true only when a decision is REACHED. A parent
// whose marker is cancelled or pruned mid-park never reaches one, so its entry
// was retained for the daemon's life — ~50 bytes each, bounded only by
// parents-ever-parked.
//
// ciWatchPass reaps every shadow with no live marker, which is exactly right: a
// park is created inside pollCIMarker, so a shadow with no marker can never be
// read again.
func TestCIRetry_TheParkShadowIsReapedWhenTheParentIsGone(t *testing.T) {
	b := retryBroker(t, 3)
	b.CIWatchTimeout = time.Hour
	b.GlobalMaxTasks = 100
	b.GlobalLedger = nil // unmeasurable -> park

	var ids []string
	for i := 0; i < 25; i++ {
		it := seedTaskAwaitingCI(t, b, baseTask())
		seedSettledMarker(t, b, ciMarker{TaskID: it.ID, State: CIFailed, Summary: failing(),
			RepoRef: it.Task.RepoRef, PRNumber: 7, PROwner: "o", PRRepo: "r"})
		if recorded := b.applyCIObservation(failedObs(b, it)); recorded {
			t.Fatalf("setup: task %d was not parked", i)
		}
		ids = append(ids, it.ID)
	}

	// A pass with every marker still live reaps NOTHING: these parks can still be
	// re-asked, and dropping their shadows would destroy the chains.
	b.ciWatchPass(map[string]int{})
	for _, id := range ids {
		if parkShadow(b, id) == 0 {
			t.Fatalf("the reap dropped a LIVE park's shadow (%s); the chain it belongs to is destroyed", id)
		}
	}

	// Now the markers go — a `drydock queue cancel`, a prune, an operator's rm.
	for _, id := range ids {
		if err := os.Remove(filepath.Join(b.AuditRoot, id+".ci.json")); err != nil {
			t.Fatal(err)
		}
	}
	b.ciWatchPass(map[string]int{})
	for _, id := range ids {
		if at := parkShadow(b, id); at != 0 {
			t.Fatalf("the shadow for a vanished parent (%s) was retained (at=%d); the map grows for the daemon's life", id, at)
		}
	}
	b.queueMu.Lock()
	n, nm := len(b.ciRetryParked), len(b.ciRetryParkedMono)
	b.queueMu.Unlock()
	if n != 0 || nm != 0 {
		t.Errorf("after the reap the park maps hold %d/%d entries, want 0/0", n, nm)
	}
}

// ---- F4 ----

// TestCIRetry_RepeatedBackwardClockJumpsCannotHoldAParkOpenForever.
//
// ciRetryParkExpired measured `ceilingNowMs() - since`, and ceilingNowMs
// deliberately leaves BACKWARD jumps uncorrected (a backward clock counts more,
// which is the direction a ceiling is allowed to take). A host that steps back an
// hour on every tick therefore kept parkedMs pinned below the bound forever:
// measured, 100/100 ticks still parked over 100 real minutes. Nothing spends —
// the cost is the `<id>.ci.json` marker the bound exists to release, leaked for
// the daemon's life, which is the exact failure the bound was added to prevent.
//
// The monotonic anchor is the second measure, and expiry is the OR of the two.
func TestCIRetry_RepeatedBackwardClockJumpsCannotHoldAParkOpenForever(t *testing.T) {
	wall, mono := &testClock{}, &testClock{}
	wall.set(1_700_000_000_000)
	mono.set(0)
	b := retryBroker(t, 3)
	b.now, b.mono = wall.now, mono.now
	b.CIWatchTimeout = time.Hour // the park bound IS the watch timeout
	it := seedTaskAwaitingCI(t, b, baseTask())
	obs := failedObs(b, it)
	b.GlobalMaxTasks = 100
	b.GlobalLedger = nil // unmeasurable -> park
	_ = b.CeilingNowMs() // anchor the correction, as the ceiling's wiring does at boot

	if recorded := b.applyCIObservation(obs); recorded {
		t.Fatal("setup: the first decision was not parked")
	}

	// One real minute per tick, and the host clock steps an hour BACKWARD each
	// time. On the corrected wall clock alone the park never ages a millisecond.
	ended := 0
	for i := 0; i < 100; i++ {
		mono.advance(time.Minute)
		wall.advance(time.Minute - time.Hour)
		if _, _, park := b.maybeEnqueueCIRetry(obs, QueueCIFailed); !park {
			ended = i + 1
			break
		}
	}
	if ended == 0 {
		t.Fatal("100 ticks and 100 minutes of REAL time later the park is still open: " +
			"a host clock that keeps stepping backward holds the marker for the daemon's life")
	}
	// It ends on the bound, not early: 60 one-minute ticks is the 1h bound.
	if ended < 60 {
		t.Errorf("the park ended after %d minutes of real time, well inside its 1h bound", ended)
	}
}

// TestCIRetry_TheMonotonicAnchorCannotEndAParkEarly is the other direction of the
// same change: adding a second elapsed measure must not make a park expire before
// the bound on an ordinary, well-behaved host.
func TestCIRetry_TheMonotonicAnchorCannotEndAParkEarly(t *testing.T) {
	wall, mono := &testClock{}, &testClock{}
	wall.set(1_700_000_000_000)
	mono.set(0)
	b := retryBroker(t, 3)
	b.now, b.mono = wall.now, mono.now
	b.CIWatchTimeout = time.Hour
	it := seedTaskAwaitingCI(t, b, baseTask())
	obs := failedObs(b, it)
	b.GlobalMaxTasks = 100
	b.GlobalLedger = nil
	_ = b.CeilingNowMs()

	if recorded := b.applyCIObservation(obs); recorded {
		t.Fatal("setup: the first decision was not parked")
	}
	for i := 0; i < 59; i++ {
		mono.advance(time.Minute)
		wall.advance(time.Minute)
		if _, detail, park := b.maybeEnqueueCIRetry(obs, QueueCIFailed); !park {
			t.Fatalf("the park ended after %d minutes of a 1h bound (detail=%q)", i+1, detail)
		}
	}
	mono.advance(2 * time.Minute)
	wall.advance(2 * time.Minute)
	if _, _, park := b.maybeEnqueueCIRetry(obs, QueueCIFailed); park {
		t.Error("the park outlived its bound on a clock that never moved oddly at all")
	}
}
