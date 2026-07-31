package broker

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"drydock/internal/remote"
)

// These tests drive the host-side CI watcher (B1 task 2). The invariants they
// exist to protect:
//
//   - D4: the watch NEVER acquires a concurrency slot and never keeps a stage.
//   - D3: only broker-observed check conclusions move state; no log text is
//     read anywhere on the path.
//   - Absence of evidence never reads as passing: an error, a give-up, a
//     timeout, and a PR with no checks are each their own recorded state and
//     none of them is CIPassed.
//   - The watch deadline is ABSOLUTE and anchored to persisted data, so a
//     crash loop cannot extend a watch.
//
// Every test is deterministic: the clock is the b.now seam and the ticker runs
// at a sub-millisecond interval, so nothing waits on wall-clock CI timing.

// ---- harness ----

// ciObserver collects terminal observations delivered through the OnCIObserved
// hook (the seam Task 3 will hang the queue/audit surfacing off).
type ciObserver struct {
	mu   sync.Mutex
	seen []CIObservation
}

func (o *ciObserver) record(obs CIObservation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, obs)
}

func (o *ciObserver) all() []CIObservation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]CIObservation(nil), o.seen...)
}

func (o *ciObserver) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.seen)
}

// testClock is the deterministic clock the watcher reads through b.now. It is
// atomic because a test moves it forward WHILE the watch goroutine is running.
type testClock struct{ ms atomic.Int64 }

func (c *testClock) now() int64              { return c.ms.Load() }
func (c *testClock) set(v int64)             { c.ms.Store(v) }
func (c *testClock) advance(d time.Duration) { c.ms.Add(int64(d / time.Millisecond)) }

// watchBroker builds a minimal Broker wired for the watcher only: an audit
// dir, a fast tick, a deterministic clock, and a scripted checks seam. Nothing
// here touches the dispatcher, a stage, or a container.
func watchBroker(t *testing.T, checks func(env []string, owner, repo string, number int) (remote.CheckSummary, error)) (*Broker, *ciObserver, *testClock) {
	t.Helper()
	obs := &ciObserver{}
	clk := &testClock{}
	clk.set(1_700_000_000_000)
	b := &Broker{
		AuditRoot:      t.TempDir(),
		MaxConcurrent:  2,
		CIWatch:        true,
		CIPollInterval: 200 * time.Microsecond,
		CIWatchTimeout: time.Hour,
		OnCIObserved:   obs.record,
		checksFn:       checks,
		// Never read the host environment from a unit test.
		ciEnvFn: func() []string { return []string{"PATH=/usr/bin"} },
	}
	b.now = clk.now
	return b, obs, clk
}

// seedMarker writes a pending marker as finishPush would.
func seedMarker(t *testing.T, b *Broker, m ciMarker) ciMarker {
	t.Helper()
	if m.TaskID == "" {
		m.TaskID = newID()
	}
	if m.RepoRef == "" {
		m.RepoRef = "https://github.com/o/r.git"
	}
	if m.Branch == "" {
		m.Branch = "agent/" + m.TaskID
	}
	if m.PRNumber == 0 {
		m.PRNumber = 42
	}
	// The validated PR coordinate finishPush captures. It defaults to the same
	// owner/repo as the default RepoRef, but the two are independent fields —
	// the fork test sets them apart deliberately.
	if m.PROwner == "" {
		m.PROwner = "o"
	}
	if m.PRRepo == "" {
		m.PRRepo = "r"
	}
	if m.State == "" {
		m.State = CIPending
	}
	if m.CreatedAtMs == 0 {
		m.CreatedAtMs = b.nowMs()
		m.UpdatedAtMs = m.CreatedAtMs
	}
	if err := writeCIMarker(b.AuditRoot, m); err != nil {
		t.Fatalf("writeCIMarker: %v", err)
	}
	return m
}

// stopWatch stops the watcher AND waits for its goroutine to exit, so no poll
// can still be writing into the test's TempDir while t.Cleanup removes it.
// Production's StopCIWatch is deliberately non-blocking; this is the test-only
// deterministic form.
func stopWatch(t *testing.T, b *Broker) {
	t.Helper()
	b.StopCIWatch()
	select {
	case <-b.ciDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch goroutine did not exit after StopCIWatch")
	}
}

func markerGone(t *testing.T, b *Broker, id string) bool {
	t.Helper()
	_, err := readCIMarker(b.AuditRoot, id)
	return err != nil
}

func passing() remote.CheckSummary {
	return remote.CheckSummary{Rollup: remote.RollupPassed, Total: 1, Passed: 1,
		Checks: []remote.Check{{Name: "build", State: remote.CheckPassed}}}
}

func failing() remote.CheckSummary {
	return remote.CheckSummary{Rollup: remote.RollupFailed, Total: 2, Passed: 1, Failed: 1,
		Checks: []remote.Check{{Name: "build", State: remote.CheckPassed}, {Name: "test", State: remote.CheckFailed}}}
}

// ---- the loop ----

// TestCIWatch_PollsObservesRemovesAndStops is the happy path: the watcher picks
// the marker up, keeps polling while CI is pending, records the terminal
// conclusion exactly once, deletes the marker, and stops cleanly.
func TestCIWatch_PollsObservesRemovesAndStops(t *testing.T) {
	var polls int64
	b, obs, _ := watchBroker(t, func(_ []string, owner, repo string, n int) (remote.CheckSummary, error) {
		if owner != "o" || repo != "r" || n != 42 {
			t.Errorf("checks called with %s/%s#%d, want o/r#42", owner, repo, n)
		}
		if atomic.AddInt64(&polls, 1) < 3 {
			return remote.CheckSummary{Rollup: remote.RollupPending, Total: 1, Pending: 1}, nil
		}
		return failing(), nil
	})
	m := seedMarker(t, b, ciMarker{})

	b.StartCIWatch()
	defer stopWatch(t, b)

	if !waitFor(5*time.Second, func() bool { return obs.count() == 1 }) {
		t.Fatalf("no terminal observation after %d polls", atomic.LoadInt64(&polls))
	}
	got := obs.all()[0]
	if got.State != CIFailed {
		t.Fatalf("observed state = %q, want %q", got.State, CIFailed)
	}
	if got.TaskID != m.TaskID || got.PRNumber != 42 || got.Branch != m.Branch || got.RepoRef != m.RepoRef {
		t.Errorf("observation identity = %+v, want the marker's", got)
	}
	if got.Summary.Failed != 1 || got.Summary.Rollup != remote.RollupFailed {
		t.Errorf("observation carries no summary: %+v", got.Summary)
	}
	if got.ObservedAtMs != b.nowMs() {
		t.Errorf("ObservedAtMs = %d, want the broker clock seam's value", got.ObservedAtMs)
	}
	if !waitFor(5*time.Second, func() bool { return markerGone(t, b, m.TaskID) }) {
		t.Fatal("marker survived a terminal observation")
	}
	// The watch is over: no further polls, and exactly one observation.
	after := atomic.LoadInt64(&polls)
	if !waitFor(200*time.Millisecond, func() bool { return false }) && atomic.LoadInt64(&polls) != after {
		t.Errorf("polling continued after the marker was removed (%d -> %d)", after, atomic.LoadInt64(&polls))
	}
	if n := obs.count(); n != 1 {
		t.Errorf("observations = %d, want exactly 1", n)
	}
}

// TestCIWatch_StopIsClean proves StopCIWatch ends the goroutine: polling stops
// and stays stopped.
func TestCIWatch_StopIsClean(t *testing.T) {
	var polls int64
	b, _, _ := watchBroker(t, func([]string, string, string, int) (remote.CheckSummary, error) {
		atomic.AddInt64(&polls, 1)
		return remote.CheckSummary{Rollup: remote.RollupPending, Total: 1, Pending: 1}, nil
	})
	seedMarker(t, b, ciMarker{})
	b.StartCIWatch()
	if !waitFor(5*time.Second, func() bool { return atomic.LoadInt64(&polls) >= 3 }) {
		t.Fatal("watcher never polled")
	}
	stopWatch(t, b) // returns only once the goroutine has actually exited
	frozen := atomic.LoadInt64(&polls)
	waitFor(100*time.Millisecond, func() bool { return false })
	if got := atomic.LoadInt64(&polls); got != frozen {
		t.Fatalf("polls kept climbing after StopCIWatch: %d -> %d", frozen, got)
	}
}

// ---- D4: no concurrency slot ----

// TestCIWatch_HoldsNoConcurrencySlot is the D4 test. With MaxConcurrent=2 and a
// CI poll parked mid-flight (exactly the minutes-to-hours wait a real GitHub
// poll can be), BOTH task slots must still be claimable. If the watch ever took
// a slot this fails.
func TestCIWatch_HoldsNoConcurrencySlot(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	b, _, _ := watchBroker(t, func([]string, string, string, int) (remote.CheckSummary, error) {
		once.Do(func() { close(entered) })
		<-release
		return remote.CheckSummary{Rollup: remote.RollupPending, Total: 1, Pending: 1}, nil
	})
	seedMarker(t, b, ciMarker{})

	b.StartCIWatch()
	// Defers run LIFO: unpark the poll FIRST, then wait for the goroutine to
	// exit, so nothing is still writing when t.TempDir is cleaned up.
	defer stopWatch(t, b)
	defer close(release)

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never reached the checks call")
	}
	// The poll is parked. Every task slot must still be free.
	for i := 0; i < b.MaxConcurrent; i++ {
		if !b.acquireSlot() {
			t.Fatalf("slot %d/%d unavailable while a CI poll is in flight: the watch is holding a concurrency slot (D4)",
				i+1, b.MaxConcurrent)
		}
	}
	for i := 0; i < b.MaxConcurrent; i++ {
		b.releaseSlot()
	}
}

// ---- rollup -> recorded state ----

// TestCIWatch_RollupToState pins the mapping, and in particular that a PR with
// no checks records CINoChecks and NEVER CIPassed.
func TestCIWatch_RollupToState(t *testing.T) {
	cases := []struct {
		rollup remote.CheckRollup
		want   CIState
	}{
		{remote.RollupPassed, CIPassed},
		{remote.RollupFailed, CIFailed},
		{remote.RollupNoChecks, CINoChecks},
		{remote.RollupPending, CIPending},
		{remote.CheckRollup("martian"), CIPending},
		{remote.CheckRollup(""), CIPending},
	}
	for _, tc := range cases {
		if got := ciStateFor(tc.rollup); got != tc.want {
			t.Errorf("ciStateFor(%q) = %q, want %q", tc.rollup, got, tc.want)
		}
		if tc.rollup != remote.RollupPassed && ciStateFor(tc.rollup) == CIPassed {
			t.Errorf("rollup %q read as passed", tc.rollup)
		}
	}
}

// TestCIWatch_NoChecksIsRecordedAndIsNotPassed drives the whole loop for a PR
// that has no checks configured — from ABOVE the dispatch floor, which is the
// only place `no_checks` is a legitimate conclusion (see the floor tests
// below).
func TestCIWatch_NoChecksIsRecordedAndIsNotPassed(t *testing.T) {
	b, obs, clk := watchBroker(t, func([]string, string, string, int) (remote.CheckSummary, error) {
		return remote.CheckSummary{Rollup: remote.RollupNoChecks}, nil
	})
	m := seedMarker(t, b, ciMarker{})
	clk.advance(ciNoChecksMinGrace + time.Minute) // past the dispatch floor
	b.StartCIWatch()
	defer stopWatch(t, b)

	if !waitFor(5*time.Second, func() bool { return obs.count() == 1 }) {
		t.Fatal("no observation")
	}
	got := obs.all()[0]
	if got.State != CINoChecks {
		t.Fatalf("state = %q, want %q", got.State, CINoChecks)
	}
	if got.State == CIPassed {
		t.Fatal("a PR with no checks was recorded as passed")
	}
	if !waitFor(5*time.Second, func() bool { return markerGone(t, b, m.TaskID) }) {
		t.Fatal("marker survived a terminal observation")
	}
}

// ---- the no_checks dispatch floor (C1) ----
//
// `no_checks` is the ONE terminal state derived from seeing nothing, so it is
// the one state that has to be a function of TIME. GitHub Actions dispatch is
// asynchronous: a poll that lands in the gap between the push and the checks
// appearing sees an empty rollup that means "not yet", not "never". These tests
// therefore assert on the CLOCK, not just on "no_checks != passed".

// TestCIWatch_NoChecksBelowTheFloorIsPendingThenConcludes is the core one: the
// SAME empty rollup, polled repeatedly, is non-terminal before the floor and
// terminal after it. Nothing changes except the clock.
func TestCIWatch_NoChecksBelowTheFloorIsPendingThenConcludes(t *testing.T) {
	var polls int64
	b, obs, clk := watchBroker(t, func([]string, string, string, int) (remote.CheckSummary, error) {
		atomic.AddInt64(&polls, 1)
		return remote.CheckSummary{Rollup: remote.RollupNoChecks}, nil
	})
	m := seedMarker(t, b, ciMarker{})
	b.StartCIWatch()
	defer stopWatch(t, b)

	// Several independent polls inside the dispatch window: no conclusion, and
	// the marker stays live and explicitly `pending`.
	if !waitFor(5*time.Second, func() bool { return atomic.LoadInt64(&polls) >= 4 }) {
		t.Fatal("the watch did not keep polling below the floor")
	}
	if n := obs.count(); n != 0 {
		t.Fatalf("observations below the dispatch floor = %d, want 0 — an empty rollup seconds after a push means CI has not dispatched yet", n)
	}
	live, err := readCIMarker(b.AuditRoot, m.TaskID)
	if err != nil {
		t.Fatalf("the marker was removed below the floor: %v", err)
	}
	if live.State != CIPending {
		t.Fatalf("marker state below the floor = %q, want %q", live.State, CIPending)
	}

	// Cross the floor. The rollup the fake returns has not changed at all.
	clk.advance(ciNoChecksMinGrace + time.Second)
	if !waitFor(5*time.Second, func() bool { return obs.count() == 1 }) {
		t.Fatal("no observation after the dispatch floor elapsed")
	}
	if got := obs.all()[0]; got.State != CINoChecks {
		t.Fatalf("state after the floor = %q, want %q", got.State, CINoChecks)
	}
	if !waitFor(5*time.Second, func() bool { return markerGone(t, b, m.TaskID) }) {
		t.Fatal("marker survived a terminal observation")
	}
}

// TestCIWatch_NoChecksFloorIsAbsolute: the floor is measured from the marker's
// PERSISTED CreatedAtMs, so a restart — or a crash loop that restarts the watch
// over and over — cannot dodge it. Each pass here is a fresh Broker over the
// same audit dir, exactly as a re-exec would be.
func TestCIWatch_NoChecksFloorIsAbsolute(t *testing.T) {
	b, obs, clk := watchBroker(t, func([]string, string, string, int) (remote.CheckSummary, error) {
		return remote.CheckSummary{Rollup: remote.RollupNoChecks}, nil
	})
	created := b.nowMs()
	m := seedMarker(t, b, ciMarker{CreatedAtMs: created, UpdatedAtMs: created})

	for i := 0; i < 5; i++ {
		// A "restart": a pass with brand-new in-memory watcher state (a fresh
		// error-run map, no memory of any earlier poll) over the same durable
		// marker. The clock advances a little each life, never past the floor.
		clk.advance(30 * time.Second)
		b.ciWatchPass(map[string]int{})
		if n := obs.count(); n != 0 {
			t.Fatalf("restart %d concluded no_checks below the floor (%d observations)", i, n)
		}
	}
	if _, err := readCIMarker(b.AuditRoot, m.TaskID); err != nil {
		t.Fatalf("the marker did not survive the restarts: %v", err)
	}
	// Only real elapsed time from CreatedAtMs releases it.
	clk.set(b.ciNoChecksFloor(m))
	b.ciWatchPass(map[string]int{})
	if obs.count() != 1 || obs.all()[0].State != CINoChecks {
		t.Fatalf("observations at the floor = %+v, want exactly one no_checks", obs.all())
	}
}

// TestCINoChecksFloor pins the floor arithmetic: max(minPolls * poll_interval,
// minGrace), anchored at the marker's CreatedAtMs.
func TestCINoChecksFloor(t *testing.T) {
	created := int64(1_700_000_000_000)
	m := ciMarker{TaskID: newID(), CreatedAtMs: created}
	cases := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		// Unset -> the 60s default; 2 polls is under the grace, so grace wins.
		{"default interval", 0, ciNoChecksMinGrace},
		// A fast poll interval must NOT be able to shrink the dispatch window.
		{"very fast poll", 5 * time.Millisecond, ciNoChecksMinGrace},
		// A slow operator interval wins: the guarantee is "more than one
		// independent look", whatever the operator's cadence.
		{"slow poll", 10 * time.Minute, time.Duration(ciNoChecksMinPolls) * 10 * time.Minute},
	}
	for _, c := range cases {
		b := &Broker{CIPollInterval: c.interval}
		if got, want := b.ciNoChecksFloor(m), created+c.want.Milliseconds(); got != want {
			t.Errorf("%s: ciNoChecksFloor = %d, want %d (created + %s)", c.name, got, want, c.want)
		}
	}
}

// TestCIWatch_NoChecksFloorNeverOutlivesTheDeadline: when the watch deadline
// falls inside the dispatch floor, the watch ends as an honest `timed_out` —
// never as `no_checks`, and never by hanging past its bound.
func TestCIWatch_NoChecksFloorNeverOutlivesTheDeadline(t *testing.T) {
	b, obs, clk := watchBroker(t, func([]string, string, string, int) (remote.CheckSummary, error) {
		return remote.CheckSummary{Rollup: remote.RollupNoChecks}, nil
	})
	b.CIWatchTimeout = time.Minute // well inside the 5-minute floor
	created := b.nowMs()
	m := seedMarker(t, b, ciMarker{CreatedAtMs: created, UpdatedAtMs: created})
	b.StartCIWatch()
	defer stopWatch(t, b)

	clk.advance(2 * time.Minute)
	if !waitFor(5*time.Second, func() bool { return obs.count() == 1 }) {
		t.Fatal("no observation after the deadline expired")
	}
	if got := obs.all()[0]; got.State != CITimedOut {
		t.Fatalf("state = %q, want %q — an unelapsed floor must not become a conclusion", got.State, CITimedOut)
	}
	if !waitFor(5*time.Second, func() bool { return markerGone(t, b, m.TaskID) }) {
		t.Fatal("marker survived a terminal observation")
	}
}

// TestCIWatch_PendingPersistsTheMarker: a non-terminal observation refreshes
// the marker (state, timestamp, and the now-materialized absolute deadline)
// without removing it.
func TestCIWatch_PendingPersistsTheMarker(t *testing.T) {
	var polls int64
	b, obs, clk := watchBroker(t, func([]string, string, string, int) (remote.CheckSummary, error) {
		atomic.AddInt64(&polls, 1)
		return remote.CheckSummary{Rollup: remote.RollupPending, Total: 1, Pending: 1}, nil
	})
	b.CIWatchTimeout = time.Hour
	created := b.nowMs()
	m := seedMarker(t, b, ciMarker{CreatedAtMs: created, UpdatedAtMs: created})
	// Advance the clock a little for the update stamp, but nowhere near the
	// deadline.
	clk.advance(5 * time.Second)

	b.StartCIWatch()
	defer stopWatch(t, b)
	if !waitFor(5*time.Second, func() bool { return atomic.LoadInt64(&polls) >= 2 }) {
		t.Fatal("watcher never polled")
	}
	cur, err := readCIMarker(b.AuditRoot, m.TaskID)
	if err != nil {
		t.Fatalf("marker was removed on a PENDING observation: %v", err)
	}
	if cur.State != CIPending {
		t.Errorf("state = %q, want pending", cur.State)
	}
	if cur.UpdatedAtMs != created+5_000 {
		t.Errorf("updated_at = %d, want the clock seam's value %d", cur.UpdatedAtMs, created+5_000)
	}
	if cur.CreatedAtMs != created {
		t.Errorf("created_at moved: %d, want %d", cur.CreatedAtMs, created)
	}
	if want := created + int64(time.Hour/time.Millisecond); cur.DeadlineMs != want {
		t.Errorf("deadline = %d, want created_at + watch_timeout = %d", cur.DeadlineMs, want)
	}
	if obs.count() != 0 {
		t.Errorf("a pending poll produced %d terminal observations, want 0", obs.count())
	}
}

// ---- boot resume + the absolute deadline ----

// TestCIWatch_BootResumeRewatchesSurvivingMarker: a marker written by a
// previous brokerd is picked up on the first pass with no separate resume call.
func TestCIWatch_BootResumeRewatchesSurvivingMarker(t *testing.T) {
	b, obs, _ := watchBroker(t, func([]string, string, string, int) (remote.CheckSummary, error) {
		return passing(), nil
	})
	// A marker created long before this "boot", as a crashed daemon would leave.
	created := b.nowMs() - 60_000
	m := seedMarker(t, b, ciMarker{CreatedAtMs: created, UpdatedAtMs: created,
		DeadlineMs: created + int64(time.Hour/time.Millisecond)})

	b.StartCIWatch()
	defer stopWatch(t, b)
	if !waitFor(5*time.Second, func() bool { return obs.count() == 1 }) {
		t.Fatal("a surviving marker was not re-watched at boot")
	}
	if got := obs.all()[0]; got.State != CIPassed || got.TaskID != m.TaskID {
		t.Fatalf("observation = %+v, want passed for the resumed task", got)
	}
}

// TestCIWatch_ResumeKeepsTheOriginalAbsoluteDeadline is the crash-loop test: a
// marker whose PERSISTED deadline has already passed times out immediately on
// the next boot. A restart must not restart the clock, or a task that crash
// loops could hold a watch open forever.
func TestCIWatch_ResumeKeepsTheOriginalAbsoluteDeadline(t *testing.T) {
	var polls int64
	b, obs, _ := watchBroker(t, func([]string, string, string, int) (remote.CheckSummary, error) {
		atomic.AddInt64(&polls, 1)
		return remote.CheckSummary{Rollup: remote.RollupPending, Total: 1, Pending: 1}, nil
	})
	// Generous configured timeout — the point is that the marker's own,
	// already-expired deadline wins over it.
	b.CIWatchTimeout = 24 * time.Hour
	created := b.nowMs() - 10_000
	m := seedMarker(t, b, ciMarker{
		CreatedAtMs: created,
		UpdatedAtMs: created,
		DeadlineMs:  created + 1_000, // expired 9s before this "boot"
	})

	b.StartCIWatch()
	defer stopWatch(t, b)
	if !waitFor(5*time.Second, func() bool { return obs.count() == 1 }) {
		t.Fatal("an expired watch was not concluded at boot")
	}
	got := obs.all()[0]
	if got.State != CITimedOut {
		t.Fatalf("state = %q, want %q — the restart must not extend the deadline", got.State, CITimedOut)
	}
	if got.State == CIPassed {
		t.Fatal("a timed-out watch read as passed")
	}
	if n := atomic.LoadInt64(&polls); n != 0 {
		t.Errorf("gh was polled %d times for an already-expired watch, want 0", n)
	}
	if !waitFor(5*time.Second, func() bool { return markerGone(t, b, m.TaskID) }) {
		t.Fatal("marker survived the timeout")
	}
}

// TestCIWatch_WatchTimeoutFires: with no persisted deadline the bound is
// CreatedAtMs + CIWatchTimeout — still absolute, since CreatedAtMs is persisted.
func TestCIWatch_WatchTimeoutFires(t *testing.T) {
	b, obs, clk := watchBroker(t, func([]string, string, string, int) (remote.CheckSummary, error) {
		return remote.CheckSummary{Rollup: remote.RollupPending, Total: 1, Pending: 1}, nil
	})
	b.CIWatchTimeout = time.Minute
	created := b.nowMs()
	m := seedMarker(t, b, ciMarker{CreatedAtMs: created, UpdatedAtMs: created})

	// First: not yet expired -> the marker survives and stays pending.
	b.StartCIWatch()
	if !waitFor(5*time.Second, func() bool {
		cur, err := readCIMarker(b.AuditRoot, m.TaskID)
		return err == nil && cur.DeadlineMs == created+60_000
	}) {
		t.Fatal("the derived absolute deadline was never persisted")
	}
	if obs.count() != 0 {
		t.Fatalf("premature observation: %+v", obs.all())
	}
	// Now move the clock past the deadline.
	clk.set(created + 61_000)
	if !waitFor(5*time.Second, func() bool { return obs.count() == 1 }) {
		t.Fatal("the watch timeout never fired")
	}
	stopWatch(t, b)
	got := obs.all()[0]
	if got.State != CITimedOut {
		t.Fatalf("state = %q, want %q", got.State, CITimedOut)
	}
	if !waitFor(5*time.Second, func() bool { return markerGone(t, b, m.TaskID) }) {
		t.Fatal("marker survived the timeout")
	}
}

// ---- transient errors ----

// TestCIWatch_ToleratesTransientErrorsThenGivesUpHonestly: a flaky gh must not
// end the watch, but it must not extend it forever either. After the bounded
// run of consecutive failures the watch concludes with an honest state that is
// NOT passed.
func TestCIWatch_ToleratesTransientErrorsThenGivesUpHonestly(t *testing.T) {
	var polls int64
	b, obs, _ := watchBroker(t, func([]string, string, string, int) (remote.CheckSummary, error) {
		atomic.AddInt64(&polls, 1)
		return remote.CheckSummary{}, errors.New("gh: exit status 1\nnetwork unreachable")
	})
	m := seedMarker(t, b, ciMarker{})

	b.StartCIWatch()
	defer stopWatch(t, b)
	if !waitFor(5*time.Second, func() bool { return obs.count() == 1 }) {
		t.Fatalf("the watch never concluded after %d failing polls", atomic.LoadInt64(&polls))
	}
	got := obs.all()[0]
	if got.State != CIUnknown {
		t.Fatalf("state = %q, want %q", got.State, CIUnknown)
	}
	if got.State == CIPassed || got.State == CINoChecks {
		t.Fatal("a run of errors was laundered into a non-error conclusion")
	}
	if got.Detail == "" {
		t.Error("a give-up must record why")
	}
	if n := atomic.LoadInt64(&polls); n != ciWatchMaxConsecutiveErrors {
		t.Errorf("polls before giving up = %d, want exactly %d", n, ciWatchMaxConsecutiveErrors)
	}
	if !waitFor(5*time.Second, func() bool { return markerGone(t, b, m.TaskID) }) {
		t.Fatal("marker survived the give-up")
	}
}

// TestCIWatch_ErrorRunResetsOnSuccess: the tolerance counts CONSECUTIVE errors.
// A flaky gh that eventually answers must not be cut off — here the error count
// exceeds the tolerance in total, but never consecutively, and the watch still
// reaches the real conclusion.
func TestCIWatch_ErrorRunResetsOnSuccess(t *testing.T) {
	var polls int64
	b, obs, _ := watchBroker(t, func([]string, string, string, int) (remote.CheckSummary, error) {
		n := atomic.AddInt64(&polls, 1)
		switch {
		case n <= 3*int64(ciWatchMaxConsecutiveErrors):
			// alternate: error, pending, error, pending, ...
			if n%2 == 1 {
				return remote.CheckSummary{}, errors.New("gh: transient")
			}
			return remote.CheckSummary{Rollup: remote.RollupPending, Total: 1, Pending: 1}, nil
		default:
			return passing(), nil
		}
	})
	seedMarker(t, b, ciMarker{})
	b.StartCIWatch()
	defer stopWatch(t, b)

	if !waitFor(5*time.Second, func() bool { return obs.count() == 1 }) {
		t.Fatalf("no conclusion after %d polls", atomic.LoadInt64(&polls))
	}
	if got := obs.all()[0]; got.State != CIPassed {
		t.Fatalf("state = %q, want %q — a non-consecutive error run must not end the watch", got.State, CIPassed)
	}
}

// ---- crash safety + unwatchable markers ----

// TestCIWatch_TerminalMarkerIsReReportedWithoutQuerying: a marker whose state
// is already terminal is a crash between "record the conclusion" and "remove
// the marker". The next pass re-reports it and removes it, and must NOT spend
// another gh call re-deciding an already-decided watch.
func TestCIWatch_TerminalMarkerIsReReportedWithoutQuerying(t *testing.T) {
	var polls int64
	b, obs, _ := watchBroker(t, func([]string, string, string, int) (remote.CheckSummary, error) {
		atomic.AddInt64(&polls, 1)
		return passing(), nil
	})
	m := seedMarker(t, b, ciMarker{State: CIFailed})

	b.StartCIWatch()
	defer stopWatch(t, b)
	if !waitFor(5*time.Second, func() bool { return obs.count() == 1 }) {
		t.Fatal("a crash-surviving terminal marker was never reported")
	}
	if got := obs.all()[0]; got.State != CIFailed {
		t.Fatalf("state = %q, want the persisted %q (not a fresh poll's answer)", got.State, CIFailed)
	}
	if n := atomic.LoadInt64(&polls); n != 0 {
		t.Errorf("gh was polled %d times for an already-concluded watch, want 0", n)
	}
	if !waitFor(5*time.Second, func() bool { return markerGone(t, b, m.TaskID) }) {
		t.Fatal("marker survived re-reporting")
	}
}

// TestCIWatch_NonGitHubRefGivesUpHonestly: Checks pins github.com, so a marker
// for any other host is unwatchable. It concludes as unknown immediately rather
// than burning the whole timeout, and gh is never invoked.
func TestCIWatch_NonGitHubRefGivesUpHonestly(t *testing.T) {
	for _, ref := range []string{
		"https://gitlab.com/o/r.git",
		"https://git.mycorp.internal/o/r.git",
		"not-a-ref",
	} {
		t.Run(ref, func(t *testing.T) {
			var polls int64
			b, obs, _ := watchBroker(t, func([]string, string, string, int) (remote.CheckSummary, error) {
				atomic.AddInt64(&polls, 1)
				return passing(), nil
			})
			m := seedMarker(t, b, ciMarker{RepoRef: ref})
			b.StartCIWatch()
			defer stopWatch(t, b)
			if !waitFor(5*time.Second, func() bool { return obs.count() == 1 }) {
				t.Fatal("an unwatchable marker was never concluded")
			}
			if got := obs.all()[0]; got.State != CIUnknown {
				t.Fatalf("state = %q, want %q", got.State, CIUnknown)
			}
			if n := atomic.LoadInt64(&polls); n != 0 {
				t.Errorf("gh was invoked %d times for a non-github ref, want 0", n)
			}
			if !waitFor(5*time.Second, func() bool { return markerGone(t, b, m.TaskID) }) {
				t.Fatal("marker survived")
			}
		})
	}
}

// TestCIRefHostWatchable pins the HOST gate across the spellings a task's
// repo_ref can take. It answers only "is this a github.com repository?" — the
// owner/repo the watch queries come from the marker's validated PR fields.
func TestCIRefHostWatchable(t *testing.T) {
	cases := map[string]bool{
		"https://github.com/o/r.git":              true,
		"https://github.com/my-org/my.repo":       true,
		"git@github.com:o/r.git":                  true,
		"ssh://git@github.com/o/r.git":            true,
		"https://GitHub.com/O/R.git":              true,
		"https://github.com/o/r/":                 true,
		"https://alice:tok@github.com/o/r.git":    true, // redaction is not required for the gate
		"https://gitlab.com/o/r.git":              false,
		"https://github.mycorp.com/o/r.git":       false,
		"https://github.com.attacker.net/o/r.git": false,
		"https://github.com/o":                    false,
		"https://github.com/o/sub/r":              false,
		"":                                        false,
		"garbage":                                 false,
	}
	for ref, want := range cases {
		if got := ciRefHostWatchable(ref); got != want {
			t.Errorf("ciRefHostWatchable(%q) = %v, want %v", ref, got, want)
		}
	}
}

// The watch queries the PR's OWN owner/repo, taken from the marker's validated
// fields — never the task's repo ref, and never a re-parse of the PR URL. For a
// fork-opened PR those differ, and the PR's repo is the one with the checks.
func TestCIWatch_QueriesTheValidatedPRRepoNotTheTaskRepo(t *testing.T) {
	type target struct{ owner, repo string }
	got := make(chan target, 4)
	b, obs, _ := watchBroker(t, func(_ []string, owner, repo string, _ int) (remote.CheckSummary, error) {
		select {
		case got <- target{owner, repo}:
		default:
		}
		return passing(), nil
	})
	// The task cloned the fork; the PR was opened against the upstream. The
	// PR URL is deliberately a THIRD value, so a watcher that re-parsed it
	// instead of reading the validated fields would be caught too.
	seedMarker(t, b, ciMarker{
		RepoRef: "https://github.com/myfork/proj.git",
		PROwner: "upstream", PRRepo: "proj",
		PRURL: "https://github.com/decoy/decoy/pull/42",
	})

	b.StartCIWatch()
	defer stopWatch(t, b)
	if !waitFor(5*time.Second, func() bool { return obs.count() == 1 }) {
		t.Fatal("the fork's PR was never watched")
	}
	select {
	case tg := <-got:
		if tg.owner != "upstream" || tg.repo != "proj" {
			t.Fatalf("checks queried %s/%s, want the PR's own upstream/proj", tg.owner, tg.repo)
		}
	default:
		t.Fatal("checks was never called")
	}
}

// A marker with no validated PR owner/repo (an old or tampered file) is
// unwatchable: the argv cannot be built from it, and guessing one from the URL
// is exactly what the explicit fields exist to prevent. It concludes as unknown
// without spending a gh call, and unknown is never a pass.
func TestCIWatch_MarkerWithoutValidatedOwnerRepoGivesUpHonestly(t *testing.T) {
	for _, bad := range [][2]string{
		{"", "r"},         // a marker written before pr_owner/pr_repo existed
		{"o", ""},         //
		{"..", "r"},       // traversal
		{"-evil", "r"},    // flag confusion
		{"o", "a..b"},     // interior dot-dot
		{"o/../x", "r"},   // an embedded path
		{"o r", "r"},      // whitespace
		{"o", "r --json"}, // argv smuggling
	} {
		t.Run(bad[0]+"|"+bad[1], func(t *testing.T) {
			var polls int64
			b, obs, _ := watchBroker(t, func([]string, string, string, int) (remote.CheckSummary, error) {
				atomic.AddInt64(&polls, 1)
				return passing(), nil
			})
			// Written directly, NOT through seedMarker: the point is the exact
			// bad value, and seedMarker fills empty fields with good defaults.
			now := b.nowMs()
			if err := writeCIMarker(b.AuditRoot, ciMarker{
				TaskID: newID(), RepoRef: "https://github.com/o/r.git", Branch: "agent/x",
				PRNumber: 42, PROwner: bad[0], PRRepo: bad[1],
				PRURL: "https://github.com/o/r/pull/42",
				State: CIPending, CreatedAtMs: now, UpdatedAtMs: now,
			}); err != nil {
				t.Fatal(err)
			}
			b.StartCIWatch()
			defer stopWatch(t, b)
			if !waitFor(5*time.Second, func() bool { return obs.count() == 1 }) {
				t.Fatal("an unwatchable marker was never concluded")
			}
			if got := obs.all()[0]; got.State != CIUnknown {
				t.Fatalf("state = %q, want %q", got.State, CIUnknown)
			}
			if n := atomic.LoadInt64(&polls); n != 0 {
				t.Errorf("gh was invoked %d times for a marker with no validated coordinate, want 0", n)
			}
		})
	}
}

// TestCIWatch_MultipleMarkersEachConcluded: the pass is per-marker, and one
// unwatchable marker cannot strand another.
func TestCIWatch_MultipleMarkersEachConcluded(t *testing.T) {
	b, obs, _ := watchBroker(t, func(_ []string, _, _ string, n int) (remote.CheckSummary, error) {
		if n == 1 {
			return failing(), nil
		}
		return passing(), nil
	})
	bad := seedMarker(t, b, ciMarker{RepoRef: "https://gitlab.com/o/r.git"})
	one := seedMarker(t, b, ciMarker{PRNumber: 1})
	two := seedMarker(t, b, ciMarker{PRNumber: 2})

	b.StartCIWatch()
	defer stopWatch(t, b)
	if !waitFor(5*time.Second, func() bool { return obs.count() == 3 }) {
		t.Fatalf("observations = %d, want 3", obs.count())
	}
	byID := map[string]CIState{}
	for _, o := range obs.all() {
		byID[o.TaskID] = o.State
	}
	if byID[bad.TaskID] != CIUnknown || byID[one.TaskID] != CIFailed || byID[two.TaskID] != CIPassed {
		t.Fatalf("per-marker states = %v", byID)
	}
	for _, m := range []ciMarker{bad, one, two} {
		if !markerGone(t, b, m.TaskID) {
			t.Errorf("marker %s survived", m.TaskID)
		}
	}
}

// TestCIWatch_NoHookIsSafe: OnCIObserved is optional (Task 3 wires it). With no
// hook the watch still concludes and cleans up.
func TestCIWatch_NoHookIsSafe(t *testing.T) {
	b, _, _ := watchBroker(t, func([]string, string, string, int) (remote.CheckSummary, error) {
		return failing(), nil
	})
	b.OnCIObserved = nil
	m := seedMarker(t, b, ciMarker{})
	b.StartCIWatch()
	defer stopWatch(t, b)
	if !waitFor(5*time.Second, func() bool { return markerGone(t, b, m.TaskID) }) {
		t.Fatal("marker survived with no observation hook wired")
	}
}
