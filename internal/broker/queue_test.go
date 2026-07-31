package broker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"drydock/internal/creds"
	"drydock/internal/egress"
	"drydock/internal/remote"
)

// These tests drive the durable queue + dispatcher (orchestration increment
// A): Enqueue -> dispatcher -> runQueued -> the SAME runLifecycle the
// synchronous POST /tasks path runs, headlessly, with the terminal outcome
// persisted onto the queue item.

// freshMintProvider mints a FRESH fakeGrant per task — the dispatcher runs
// tasks concurrently, and a single shared grant (mintingProvider's shape)
// would make the tests themselves race on Revoke.
type freshMintProvider struct{}

func (freshMintProvider) Mint(float64) (creds.Grant, error) { return &fakeGrant{}, nil }

// queueBroker builds a Broker for queue tests. Unlike testBroker it returns a
// FRESH fakeStage per prepareStage call (and a fresh grant per mint), because
// the dispatcher runs several tasks concurrently and shared fakes would be a
// data race in the tests themselves.
func queueBroker(t *testing.T, maxConcurrent int,
	run func(context.Context, []string, io.Writer, io.Writer) error) *Broker {
	t.Helper()
	return &Broker{
		Cfg:           egress.Config{},
		Providers:     map[string]creds.Provider{"anthropic": freshMintProvider{}},
		DefaultAgent:  "claude",
		ImageRef:      "test-image:latest",
		StageRoot:     t.TempDir(),
		AuditRoot:     t.TempDir(),
		Timeout:       5 * time.Second,
		Network:       "testnet",
		GatewayIP:     "10.0.0.1",
		ProxyPort:     3128,
		TaskBudget:    1.0,
		MaxConcurrent: maxConcurrent,
		queueTick:     5 * time.Millisecond,
		prepareStage: func(context.Context, string, string) (taskStage, error) {
			return &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}, nil
		},
		runAgent: run,
		newAdapter: func(repoRef, platform string) remote.Adapter {
			return &fakeAdapter{name: "github"}
		},
		// Keep failure-path teardown hermetic: never shell out to the real
		// `container` CLI from a unit test.
		deleteContainer: func(string) error { return nil },
	}
}

// queueItemState reads the durable state of one queue item.
func queueItemState(t *testing.T, b *Broker, id string) QueueItem {
	t.Helper()
	it, err := readQueueItem(b.AuditRoot, id)
	if err != nil {
		t.Fatalf("readQueueItem(%s): %v", id, err)
	}
	return it
}

// waitForQueueState polls until the item reaches want, failing the test on
// timeout with the state it was stuck in.
func waitForQueueState(t *testing.T, b *Broker, id string, want QueueState) {
	t.Helper()
	if !waitFor(5*time.Second, func() bool {
		return queueItemState(t, b, id).State == want
	}) {
		t.Fatalf("queue item %s stuck in %q, want %q", id, queueItemState(t, b, id).State, want)
	}
}

// TestQueue_DrainsRespectingConcurrency enqueues more tasks than MaxConcurrent
// and proves the dispatcher (a) reaches full concurrency, (b) NEVER exceeds
// it, and (c) drains everything to completed.
func TestQueue_DrainsRespectingConcurrency(t *testing.T) {
	const maxConcurrent = 2
	const k = 5
	var cur, maxSeen int64
	release := make(chan struct{})
	run := func(_ context.Context, _ []string, stdout, _ io.Writer) error {
		c := atomic.AddInt64(&cur, 1)
		defer atomic.AddInt64(&cur, -1)
		for {
			m := atomic.LoadInt64(&maxSeen)
			if c <= m || atomic.CompareAndSwapInt64(&maxSeen, m, c) {
				break
			}
		}
		<-release
		fmt.Fprintln(stdout, `{"type":"result","subtype":"success"}`)
		return nil
	}
	b := queueBroker(t, maxConcurrent, run)

	ids := make([]string, 0, k)
	for i := 0; i < k; i++ {
		id, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git",
			Instruction: fmt.Sprintf("task %d", i), AutoApprove: true})
		if err != nil {
			t.Fatalf("Enqueue #%d: %v", i, err)
		}
		ids = append(ids, id)
	}
	b.StartDispatcher()
	defer b.StopDispatcher()

	// The dispatcher must fill every slot...
	if !waitFor(5*time.Second, func() bool { return atomic.LoadInt64(&cur) == maxConcurrent }) {
		t.Fatalf("dispatcher never reached %d concurrent runs (cur=%d)", maxConcurrent, atomic.LoadInt64(&cur))
	}
	// ...and while the barrier holds, several ticks must not over-dispatch.
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&cur); got > maxConcurrent {
		t.Fatalf("concurrency exceeded while barrier held: cur=%d > %d", got, maxConcurrent)
	}
	close(release)

	for _, id := range ids {
		waitForQueueState(t, b, id, QueueCompleted)
	}
	if got := atomic.LoadInt64(&maxSeen); got != maxConcurrent {
		t.Errorf("max concurrent lifecycle runs = %d, want exactly %d", got, maxConcurrent)
	}
	// Terminal files are KEPT for `drydock queue list` history.
	items, err := listQueueItems(b.AuditRoot)
	if err != nil || len(items) != k {
		t.Errorf("listQueueItems after drain: n=%d err=%v, want %d items retained", len(items), err, k)
	}
}

// TestQueue_ParksOnAggregateExceeded proves a spend-exceeded vendor parks the
// item: it stays queued (not failed, not dispatched), Attempts stays 0, and
// when the cap clears the same item dispatches and completes.
func TestQueue_ParksOnAggregateExceeded(t *testing.T) {
	var exceeded atomic.Bool
	exceeded.Store(true)
	var agentRuns int64
	run := func(_ context.Context, _ []string, stdout, _ io.Writer) error {
		atomic.AddInt64(&agentRuns, 1)
		fmt.Fprintln(stdout, `{"type":"result","subtype":"success"}`)
		return nil
	}
	b := queueBroker(t, 2, run)
	b.AggregateExceeded = func(vendor string) bool {
		if vendor != "anthropic" {
			t.Errorf("AggregateExceeded consulted for vendor %q, want anthropic", vendor)
		}
		return exceeded.Load()
	}

	id, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git", Instruction: "x", AutoApprove: true})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	b.StartDispatcher()
	defer b.StopDispatcher()

	// Give the dispatcher several ticks: the item must stay parked.
	time.Sleep(60 * time.Millisecond)
	it := queueItemState(t, b, id)
	if it.State != QueueQueued {
		t.Fatalf("parked item state = %q, want queued", it.State)
	}
	if it.Attempts != 0 {
		t.Fatalf("parked item Attempts = %d, want 0 (parking is not an attempt)", it.Attempts)
	}
	if atomic.LoadInt64(&agentRuns) != 0 {
		t.Fatal("agent ran while the vendor's aggregate budget was exhausted")
	}

	exceeded.Store(false)
	waitForQueueState(t, b, id, QueueCompleted)
	if got := atomic.LoadInt64(&agentRuns); got != 1 {
		t.Errorf("agent runs after unpark = %d, want 1", got)
	}
	if it = queueItemState(t, b, id); it.Attempts != 1 {
		t.Errorf("completed item Attempts = %d, want 1", it.Attempts)
	}
}

// TestQueue_CompletedAndDeadLetterStates: a pushing task lands completed; a
// task whose agent run fails lands dead_letter with LastError set.
func TestQueue_CompletedAndDeadLetterStates(t *testing.T) {
	t.Run("pushed lands completed", func(t *testing.T) {
		b := queueBroker(t, 2, writesResult(`{"type":"result","subtype":"success"}`))
		id, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git", Instruction: "x", AutoApprove: true})
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		b.StartDispatcher()
		defer b.StopDispatcher()
		waitForQueueState(t, b, id, QueueCompleted)
		it := queueItemState(t, b, id)
		if it.LastError != "" {
			t.Errorf("completed LastError = %q, want empty", it.LastError)
		}
		if it.StartedAtMs == 0 {
			t.Errorf("completed item StartedAtMs not stamped")
		}
	})
	t.Run("agent error lands dead_letter", func(t *testing.T) {
		run := func(context.Context, []string, io.Writer, io.Writer) error {
			return fmt.Errorf("container exited 1")
		}
		b := queueBroker(t, 2, run)
		id, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git", Instruction: "x", AutoApprove: true})
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		b.StartDispatcher()
		defer b.StopDispatcher()
		waitForQueueState(t, b, id, QueueDeadLetter)
		if it := queueItemState(t, b, id); it.LastError == "" {
			t.Error("dead_letter item has empty LastError")
		}
	})
}

// TestQueue_MetricsRowRecordsQueuedMs: a queued task's terminal metrics row
// carries stage_ms.queued = StartedAtMs - EnqueuedAtMs (time spent waiting on
// the durable queue before dispatch). The fake clock advances on every nowMs
// call, so the queued duration is deterministically > 0.
func TestQueue_MetricsRowRecordsQueuedMs(t *testing.T) {
	b := queueBroker(t, 1, writesResult(`{"type":"result","subtype":"success"}`))
	var clock atomic.Int64
	clock.Store(1_000_000)
	b.now = func() int64 { return clock.Add(250) }

	id, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git", Instruction: "x", AutoApprove: true})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	b.StartDispatcher()
	defer b.StopDispatcher()
	waitForQueueState(t, b, id, QueueCompleted)

	it := queueItemState(t, b, id)
	wantQueued := it.StartedAtMs - it.EnqueuedAtMs
	if wantQueued <= 0 {
		t.Fatalf("fixture broken: StartedAtMs-EnqueuedAtMs = %d, want > 0", wantQueued)
	}
	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	sm, _ := m["stage_ms"].(map[string]any)
	if sm == nil {
		t.Fatalf("no stage_ms in metrics row: %v", m)
	}
	queued, ok := sm["queued"].(float64)
	if !ok {
		t.Fatalf("stage_ms.queued missing from a queued task's metrics row: %v", sm)
	}
	if int64(queued) != wantQueued {
		t.Errorf("stage_ms.queued = %v, want %d (StartedAtMs - EnqueuedAtMs)", queued, wantQueued)
	}
}

// TestQueue_SynchronousTaskOmitsQueued is the back-compat half: a synchronous
// POST /tasks task never sat on the queue, so its metrics row must omit
// stage_ms.queued entirely (omitempty), keeping the row byte-shape unchanged.
func TestQueue_SynchronousTaskOmitsQueued(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, writesResult(`{"type":"result","subtype":"success"}`))
	_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	id, _ := events[0]["task_id"].(string)
	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	sm, _ := m["stage_ms"].(map[string]any)
	if sm == nil {
		t.Fatalf("no stage_ms in metrics row: %v", m)
	}
	if v, present := sm["queued"]; present {
		t.Errorf("stage_ms.queued = %v on a synchronous task, want the key omitted entirely", v)
	}
}

// TestQueue_CancelQueuedBeforeDispatch: a queued (never dispatched) item
// cancels cleanly and its agent never runs.
func TestQueue_CancelQueuedBeforeDispatch(t *testing.T) {
	var agentRuns int64
	run := func(_ context.Context, _ []string, stdout, _ io.Writer) error {
		atomic.AddInt64(&agentRuns, 1)
		fmt.Fprintln(stdout, `{"type":"result","subtype":"success"}`)
		return nil
	}
	b := queueBroker(t, 2, run)
	// Dispatcher deliberately NOT started yet: the cancel races nothing.
	id, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git", Instruction: "x", AutoApprove: true})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if !b.cancelQueued(id) {
		t.Fatal("cancelQueued(queued item) = false, want true")
	}
	if it := queueItemState(t, b, id); it.State != QueueCancelled {
		t.Fatalf("cancelled item state = %q, want cancelled", it.State)
	}
	// A second cancel (already terminal, no longer in the queue) reports false.
	if b.cancelQueued(id) {
		t.Error("cancelQueued(already-cancelled item) = true, want false")
	}

	// Even with the dispatcher running, the cancelled item must never dispatch.
	b.StartDispatcher()
	defer b.StopDispatcher()
	time.Sleep(40 * time.Millisecond)
	if got := atomic.LoadInt64(&agentRuns); got != 0 {
		t.Errorf("agent ran %d times for a cancelled queue item, want 0", got)
	}
	if b.cancelQueued("00000000000000000000000000000000") {
		t.Error("cancelQueued(unknown id) = true, want false")
	}
}

// TestQueue_EnqueueValidates: Enqueue applies the same accept validation as
// POST /tasks — repo_ref shape and egress domain validity — before anything
// is persisted.
func TestQueue_EnqueueValidates(t *testing.T) {
	b := queueBroker(t, 2, writesResult(`{"type":"result","subtype":"success"}`))
	if _, err := b.Enqueue(Task{RepoRef: "/local/path", Instruction: "x"}); err == nil {
		t.Error("Enqueue accepted a local-path repo_ref")
	}
	if _, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git",
		EgressExtra: []egress.Domain{{Host: "*", Ports: []int{443}}}}); err == nil {
		t.Error("Enqueue accepted a wildcard egress domain")
	}
	if items, _ := listQueueItems(b.AuditRoot); len(items) != 0 {
		t.Errorf("invalid Enqueue persisted %d items, want 0", len(items))
	}
}

// TestHandleTask_LifecycleUnchanged pins the synchronous path's event
// sequence across the runLifecycle refactor: an auto-approved push produces
// the exact accepted -> preparing -> running -> pushing -> result/pushed
// stream it always has.
func TestHandleTask_LifecycleUnchanged(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{spent: 0.02}
	b := testBroker(t, "anthropic", st, grant,
		writesResult(`{"type":"result","subtype":"success","is_error":false,"duration_ms":12,"total_cost_usd":0.02,"num_turns":2}`))
	rec, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"do x","agent":"claude","auto_approve":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// The full ordered event/stage signature, not just the terminal.
	var sig []string
	for _, ev := range events {
		e, _ := ev["event"].(string)
		switch e {
		case "stage":
			s, _ := ev["stage"].(string)
			sig = append(sig, "stage:"+s)
		case "result":
			o, _ := ev["outcome"].(string)
			sig = append(sig, "result:"+o)
		default:
			sig = append(sig, e)
		}
	}
	want := "accepted,stage:preparing,stage:running,stage:pushing,result:pushed"
	if got := strings.Join(sig, ","); got != want {
		t.Errorf("event sequence changed by the refactor:\n got %s\nwant %s", got, want)
	}
	if term["outcome"] != "pushed" || !st.pushed.Load() {
		t.Errorf("terminal=%v pushed=%v, want result/pushed with a real push", term, st.pushed.Load())
	}
	// And no queue file appears for a synchronous task.
	if items, _ := listQueueItems(b.AuditRoot); len(items) != 0 {
		t.Errorf("synchronous task persisted %d queue items, want 0", len(items))
	}
}
