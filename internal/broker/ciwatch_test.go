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
// that has no checks configured.
func TestCIWatch_NoChecksIsRecordedAndIsNotPassed(t *testing.T) {
	b, obs, _ := watchBroker(t, func([]string, string, string, int) (remote.CheckSummary, error) {
		return remote.CheckSummary{Rollup: remote.RollupNoChecks}, nil
	})
	m := seedMarker(t, b, ciMarker{})
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

// TestOwnerRepoFromRef pins the repo-ref parse across the spellings a task's
// repo_ref can take.
func TestOwnerRepoFromRef(t *testing.T) {
	cases := []struct {
		ref         string
		owner, repo string
		ok          bool
	}{
		{"https://github.com/o/r.git", "o", "r", true},
		{"https://github.com/my-org/my.repo", "my-org", "my.repo", true},
		{"git@github.com:o/r.git", "o", "r", true},
		{"ssh://git@github.com/o/r.git", "o", "r", true},
		{"https://GitHub.com/O/R.git", "O", "R", true},
		{"https://github.com/o/r/", "o", "r", true},
		{"https://gitlab.com/o/r.git", "", "", false},
		{"https://github.mycorp.com/o/r.git", "", "", false},
		{"https://github.com/o", "", "", false},
		{"https://github.com/o/sub/r", "", "", false},
		{"", "", "", false},
		{"garbage", "", "", false},
	}
	for _, tc := range cases {
		o, r, ok := ownerRepoFromRef(tc.ref)
		if ok != tc.ok || o != tc.owner || r != tc.repo {
			t.Errorf("ownerRepoFromRef(%q) = %q,%q,%v; want %q,%q,%v", tc.ref, o, r, ok, tc.owner, tc.repo, tc.ok)
		}
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
