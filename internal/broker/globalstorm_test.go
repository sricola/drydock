package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"drydock/internal/creds"
	"drydock/internal/remote"
)

// THE MOTIVATING SCENARIO, DRIVEN END TO END (plan Task 5, test 1).
//
// B2 made drydock spend money unattended: an observed CI failure enqueues a
// retry, up to ci.max_attempts times, PER CHAIN. N repositories whose PRs fail
// CI at the same time therefore start N chains at once, and the plan's opening
// question — "what stops a retry storm?" — had no global answer before this
// work. These tests are that answer, exercised through the REAL components
// rather than through the ceiling's own unit seams:
//
//	Enqueue -> the real dispatcher (takeDispatchable/admitGlobalStart)
//	        -> the real lifecycle (runQueued -> runLifecycle -> the agent run)
//	        -> the real push + PR capture (armCIWatch)
//	        -> the real CI watcher (StartCIWatch, a scripted FAILING rollup)
//	        -> the real retry gate (maybeEnqueueCIRetry, gate 8)
//	        -> back to the dispatcher for the child
//	        -> the real terminal recording (recordGlobalUsage -> the ledger)
//
// with a stand-in human at the diff gate, because D5 forces AutoApprove false
// on every retry child and a chain of depth > 1 is unreachable without an
// approval. That stand-in is the WORST CASE for the ceiling: an operator who
// approves everything instantly is exactly the install where nothing else
// bounds the loop.
//
// Every number asserted below is either a hard bound (the task-start limb) or
// the ACTUAL overshoot of a soft one (the USD limb), never "roughly".

// stormProvider mints a fresh grant per task whose lease reports a fixed spend,
// so the USD limb has something real to measure. freshMintProvider's grants
// always meter $0, which would make the USD limb untestable end to end.
type stormProvider struct{ usd float64 }

func (p stormProvider) Mint(float64) (creds.Grant, error) { return &fakeGrant{spent: p.usd}, nil }

// stormBroker wires the whole unattended loop described above. runs counts REAL
// TASK STARTS — one increment per agent-VM run — which is the quantity the
// task-start limb claims to bound and the only one an operator pays for.
func stormBroker(t *testing.T, maxConcurrent, maxAttempts int, perTaskUSD float64,
	runs *atomic.Int64) (*Broker, *testClock) {
	t.Helper()
	b := queueBroker(t, maxConcurrent, func(_ context.Context, _ []string, stdout, _ io.Writer) error {
		runs.Add(1)
		// The agent's OWN result line, and it is deliberately a lie: a huge
		// self-reported cost that no ledger, limb or headroom figure may ever
		// pick up (G4). The broker's own src:"broker" row is written after it.
		fmt.Fprintln(stdout, `{"type":"result","subtype":"success","total_cost_usd":9999.99}`)
		return nil
	})
	b.Providers = map[string]creds.Provider{"anthropic": stormProvider{usd: perTaskUSD}}
	b.TaskBudget = 1.0
	b.CIWatch = true
	b.CIMaxAttempts = maxAttempts
	b.CIPollInterval = 200 * time.Microsecond
	b.CIWatchTimeout = time.Hour
	b.ciEnvFn = func() []string { return []string{"PATH=/usr/bin"} }
	// A FRESH adapter per call: the dispatcher runs chains concurrently and
	// capturingAdapter records into its own fields.
	b.newAdapter = func(string, string) remote.Adapter {
		return &capturingAdapter{fakeAdapter: fakeAdapter{name: "github"}, pr: prIdentity(42, "o", "r")}
	}
	// Every PR this storm opens fails its checks, forever. That is the storm.
	b.checksFn = func([]string, string, string, int) (remote.CheckSummary, error) {
		return failing(), nil
	}
	clk := &testClock{}
	clk.set(capNow)
	b.now = clk.now
	return b, clk
}

// autoApprover is the stand-in human at the diff gate: it approves every task
// that reaches the gate, acknowledging whatever second-look categories the
// broker required. It is what lets a chain reach depth > 1 (D5 forces
// AutoApprove false on a retry child), and it is the worst case for the
// ceiling — see the file header.
func autoApprover(t *testing.T, b *Broker) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			b.pendingMu.Lock()
			acks := make(map[string][]string, len(b.pending))
			for id := range b.pending {
				acks[id] = append([]string(nil), b.requiredAcks[id]...)
			}
			b.pendingMu.Unlock()
			for id, ack := range acks {
				body, _ := json.Marshal(map[string][]string{"acknowledge": ack})
				req := httptest.NewRequest("POST", "/admin/approve/"+id, bytes.NewReader(body))
				req.SetPathValue("id", id)
				b.HandleApprove(httptest.NewRecorder(), req)
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

// stormSettled waits until the storm has stopped moving: no task is live in the
// broker and the agent-run counter has been still for a full quiet period. It
// deliberately does NOT wait for an empty queue — under a tripped ceiling
// human-submitted items stay parked in `queued` by design (G5's admission-time
// refusal), so "the queue drained" would never become true.
func stormSettled(t *testing.T, b *Broker, runs *atomic.Int64, quiet time.Duration) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	last := runs.Load()
	stableSince := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		n := runs.Load()
		if n != last {
			last, stableSince = n, time.Now()
			continue
		}
		if !anyTaskLive(b) && time.Since(stableSince) >= quiet {
			return
		}
		if anyTaskLive(b) {
			stableSince = time.Now()
		}
	}
	t.Fatalf("the storm never settled: %d agent runs, tasks still live = %v", runs.Load(), anyTaskLive(b))
}

func anyTaskLive(b *Broker) bool {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	return len(b.tasks) > 0
}

// stormRun drives one storm to a standstill and returns the agent-run count and
// the ledger's own view. chains is how many independent CI-failing PRs start at
// the same instant.
func stormRun(t *testing.T, b *Broker, clk *testClock, chains int, runs *atomic.Int64) GlobalUsage {
	t.Helper()
	stopApprove := autoApprover(t, b)
	defer stopApprove()
	b.StartDispatcher()
	defer b.StopDispatcher()
	b.StartCIWatch()
	defer stopWatch(t, b)

	for i := 0; i < chains; i++ {
		if _, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git",
			Instruction: fmt.Sprintf("chain %d", i), AutoApprove: true}); err != nil {
			t.Fatalf("Enqueue chain %d: %v", i, err)
		}
	}
	stormSettled(t, b, runs, 250*time.Millisecond)
	if b.GlobalLedger == nil {
		return GlobalUsage{}
	}
	return b.GlobalLedger.Usage(clk.now())
}

// ---------------------------------------------------------------------------
// (1) The retry storm, bounded.
// ---------------------------------------------------------------------------

// TestGlobalCeiling_RetryStormIsBoundedEndToEnd. Four repositories' PRs fail CI
// at once. Each failure is observed by the real watcher, each observation
// reaches the real retry gate, and every admitted child goes through the real
// dispatcher — which is where the ceiling lives.
//
// The CONTROL subtest is load-bearing: with both limbs off the same storm runs
// chains * (1 + ci.max_attempts) tasks. Without it, "the ceiling held" would be
// indistinguishable from "the harness never produced a storm".
func TestGlobalCeiling_RetryStormIsBoundedEndToEnd(t *testing.T) {
	const (
		chains         = 4
		maxAttempts    = 3
		maxConcurrent  = 2
		perTaskUSD     = 0.50
		uncappedStarts = chains * (1 + maxAttempts) // 16
	)

	// --- the control: no ceiling, so the storm runs to its per-chain bound ---
	var controlRuns atomic.Int64
	t.Run("with no ceiling the storm runs to chains x (1+max_attempts)", func(t *testing.T) {
		b, clk := stormBroker(t, maxConcurrent, maxAttempts, perTaskUSD, &controlRuns)
		// Both limbs off. No ledger at all, exactly as a stock install.
		u := stormRun(t, b, clk, chains, &controlRuns)
		if got := controlRuns.Load(); got != uncappedStarts {
			t.Fatalf("uncapped storm ran %d agents, want %d (chains x (1+max_attempts)); "+
				"the bounded cases below prove nothing unless the storm is real",
				got, uncappedStarts)
		}
		if u.Starts != 0 {
			t.Errorf("the ceiling recorded %d starts with both limbs off; off must mean off", u.Starts)
		}
		t.Logf("control: %d agent runs, $%.2f spent, and NOTHING global stopped it", controlRuns.Load(),
			float64(controlRuns.Load())*perTaskUSD)
	})

	// --- the task-start limb: a HARD bound at admission ---
	t.Run("the task-start limb stops the storm exactly at the limb", func(t *testing.T) {
		const limb = 5
		var runs atomic.Int64
		b, clk := stormBroker(t, maxConcurrent, maxAttempts, perTaskUSD, &runs)
		b.GlobalMaxTasks = limb
		b.GlobalLedger = capLedgerAt(t, b.AuditRoot, 24*time.Hour, 0, 0, capNow)

		u := stormRun(t, b, clk, chains, &runs)

		// THE PROPERTY. Not "about the limb" — never more than the limb, ever.
		if got := runs.Load(); got > limb {
			t.Fatalf("the storm ran %d agents against a global_max_tasks of %d: the ceiling was crossed", got, limb)
		}
		if got := runs.Load(); got != limb {
			t.Errorf("the storm ran %d agents, want exactly %d: the limb should be reached, not merely respected", got, limb)
		}
		if u.Starts > limb {
			t.Errorf("the ledger recorded %d starts against a limb of %d", u.Starts, limb)
		}
		if int64(u.Starts) != runs.Load() {
			t.Errorf("the ledger recorded %d starts for %d agent runs; every start must be recorded exactly once",
				u.Starts, runs.Load())
		}
		// And the storm really was cut short rather than having run out of work.
		if runs.Load() >= uncappedStarts {
			t.Errorf("the storm was not actually truncated (%d runs vs an uncapped %d)", runs.Load(), uncappedStarts)
		}
		// The truncation must be VISIBLE and DURABLE, and there are three places it
		// can legitimately land depending on where the limb happened to fill:
		//
		//   - a retry that was enqueued and then found the limb exhausted at
		//     DISPATCH is dead-lettered (unattended work is dropped, not parked);
		//   - a human-submitted item at the same point PARKS in `queued`;
		//   - a retry the limb refused at the DECISION was never enqueued at all,
		//     so there is no queue item to look at — the record is the parent's
		//     own broker-authored ci_observation row and its retry_detail.
		//
		// Which one happens is a race between the dispatcher draining the queue and
		// the watch concluding the last chain, so requiring the first two made this
		// assertion flaky (it failed roughly 40% of runs) while testing nothing
		// stronger. Any of the three is the ceiling explaining itself.
		var dropped, parked int
		for _, it := range queueItemsIn(t, b) {
			switch {
			case it.State == QueueDeadLetter && it.Task.RetryOf != "":
				dropped++
				if !bytes.Contains([]byte(it.LastError), []byte("global_max_tasks")) {
					t.Errorf("a dropped retry's reason does not name the limb: %q", it.LastError)
				}
			case it.State == QueueQueued:
				parked++
			}
		}
		refusedAtDecision := 0
		traces, err := filepath.Glob(filepath.Join(b.AuditRoot, "*.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range traces {
			raw, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			if bytes.Contains(raw, []byte(`"retry_detail"`)) && bytes.Contains(raw, []byte("global_max_tasks")) {
				refusedAtDecision++
			}
		}
		if dropped == 0 && parked == 0 && refusedAtDecision == 0 {
			t.Error("the ceiling truncated the storm but left no dropped retry, no parked item and no " +
				"recorded decision-time refusal to explain why")
		}
		t.Logf("bounded: %d agent runs (limb %d), %d retries dropped, %d items parked, %d refused at the decision",
			runs.Load(), limb, dropped, parked, refusedAtDecision)
	})

	// --- the USD limb: soft, with an overshoot that is asserted exactly ---
	t.Run("the USD limb stops the storm within its documented overshoot", func(t *testing.T) {
		const budget = 1.25 // 2.5 tasks' worth at $0.50 each
		var runs atomic.Int64
		b, clk := stormBroker(t, maxConcurrent, maxAttempts, perTaskUSD, &runs)
		b.GlobalBudgetUSD = budget
		b.GlobalLedger = capLedgerAt(t, b.AuditRoot, 24*time.Hour, 0, 0, capNow)

		u := stormRun(t, b, clk, chains, &runs)

		// The USD limb is post-hoc: a task's spend is not knowable until it
		// ends, so the ceiling can be crossed by the tasks already in flight
		// when the last admission passed. The ACTUAL bound, asserted:
		//
		//	spend <= budget + (in-flight tasks) x (per-task metered spend)
		//
		// and in-flight tasks is bounded by max_concurrent_tasks. The bound
		// globalcap.go documents (max_concurrent x task_budget_usd) is the same
		// expression with the LEASE budget in place of the realised spend; it is
		// asserted too, because that is the number the docs promise an operator.
		actualBound := budget + float64(maxConcurrent)*perTaskUSD
		documentedBound := budget + float64(maxConcurrent)*b.TaskBudget
		if u.USD > actualBound {
			t.Fatalf("spend $%.2f exceeded the ACTUAL overshoot bound $%.2f "+
				"(budget $%.2f + max_concurrent %d x realised per-task $%.2f)",
				u.USD, actualBound, budget, maxConcurrent, perTaskUSD)
		}
		if u.USD > documentedBound {
			t.Fatalf("spend $%.2f exceeded the DOCUMENTED overshoot bound $%.2f "+
				"(budget $%.2f + max_concurrent %d x task_budget_usd $%.2f)",
				u.USD, documentedBound, budget, maxConcurrent, b.TaskBudget)
		}
		// It must actually have bitten: the uncapped storm spends 16 x $0.50.
		if uncapped := float64(uncappedStarts) * perTaskUSD; u.USD >= uncapped {
			t.Fatalf("spend $%.2f is the whole uncapped storm ($%.2f); the USD limb never bit", u.USD, uncapped)
		}
		if int(runs.Load()) != u.Starts {
			t.Errorf("%d agent runs but %d recorded starts", runs.Load(), u.Starts)
		}
		// G4, on the live path: the agent printed $9999.99 on its own result
		// line for every one of those runs. None of it is in the limb.
		if u.USD > actualBound {
			t.Fatal("an agent-reported cost reached the ceiling")
		}
		t.Logf("bounded: $%.2f spent against a $%.2f budget (actual bound $%.2f, documented $%.2f), %d runs",
			u.USD, budget, actualBound, documentedBound, runs.Load())
	})
}

// ---------------------------------------------------------------------------
// (2) Subscription mode: the whole reason the second limb exists.
// ---------------------------------------------------------------------------

// TestGlobalCeiling_SubscriptionModeIsBoundedByTheTaskLimb.
//
// A subscription lane has NO USD signal at all — it is $0 by construction, not
// by measurement — so the USD limb is structurally incapable of bounding it.
// Before this work that meant a subscription install had no cross-task bound of
// any kind: aggregate_budget_usd is api_key-only, and the per-task limits do not
// compose into a cumulative one.
//
// The test proves both halves against the SAME storm:
//
//	(a) with only global_budget_usd set — however small — the storm runs to its
//	    full per-chain bound, and the ledger's trustworthy USD total stays $0
//	    because there is nothing to meter;
//	(b) with global_max_tasks set, the same storm stops at the limb.
func TestGlobalCeiling_SubscriptionModeIsBoundedByTheTaskLimb(t *testing.T) {
	const (
		chains         = 3
		maxAttempts    = 2
		maxConcurrent  = 2
		uncappedStarts = chains * (1 + maxAttempts) // 9
	)
	// A subscription lane still mints a grant; what it does NOT do is meter.
	// UnmeteredVendors is the one signal writeBrief, the trust brief and the
	// ledger all key on, so using it here is exactly what a live install does.
	subscription := func(b *Broker) {
		b.UnmeteredVendors = map[string]bool{"anthropic": true}
	}

	t.Run("the USD limb cannot bound it", func(t *testing.T) {
		var runs atomic.Int64
		// A grant that meters a LOT. On a subscription lane the figure is not
		// trustworthy by construction, so it must never reach the limb.
		b, clk := stormBroker(t, maxConcurrent, maxAttempts, 100.0, &runs)
		subscription(b)
		b.GlobalBudgetUSD = 0.01 // as small as a USD limb can be
		b.GlobalLedger = capLedgerAt(t, b.AuditRoot, 24*time.Hour, 0, 0, capNow)

		u := stormRun(t, b, clk, chains, &runs)

		if got := runs.Load(); got != uncappedStarts {
			t.Fatalf("the subscription storm ran %d agents, want %d: a USD limb that bounded an "+
				"unmetered lane would mean the ledger is trusting a figure it should not", got, uncappedStarts)
		}
		if u.USD != 0 {
			t.Errorf("the USD limb accumulated $%.2f on an unmetered lane; $0 by construction is not a measurement", u.USD)
		}
		if u.Starts != uncappedStarts {
			t.Errorf("the ledger recorded %d starts, want %d: the start limb's input must exist even "+
				"where the USD limb's does not", u.Starts, uncappedStarts)
		}
		if u.UntrustedUSD == 0 {
			t.Error("the unmetered spend was not carried as UntrustedUSD; an operator must still see it")
		}
	})

	t.Run("the task limb does bound it", func(t *testing.T) {
		const limb = 4
		var runs atomic.Int64
		b, clk := stormBroker(t, maxConcurrent, maxAttempts, 100.0, &runs)
		subscription(b)
		b.GlobalMaxTasks = limb
		b.GlobalLedger = capLedgerAt(t, b.AuditRoot, 24*time.Hour, 0, 0, capNow)

		u := stormRun(t, b, clk, chains, &runs)

		if got := runs.Load(); got > limb {
			t.Fatalf("a subscription storm ran %d agents against a global_max_tasks of %d", got, limb)
		}
		if got := runs.Load(); got != limb {
			t.Errorf("the subscription storm ran %d agents, want exactly %d", got, limb)
		}
		if u.USD != 0 {
			t.Errorf("USD = $%.2f on an unmetered lane, want $0", u.USD)
		}
		if u.Starts != limb {
			t.Errorf("recorded starts = %d, want %d", u.Starts, limb)
		}
	})
}

// ---------------------------------------------------------------------------
// (3) The real-world shape: the ceiling trips mid-storm, then the window rolls.
// ---------------------------------------------------------------------------

// TestGlobalCeiling_MidStormTripThenWindowRollResumesWork.
//
// A rolling ceiling is not a latch. When the window rolls past the starts that
// exhausted it, the work an operator submitted must resume — and the retries the
// ceiling already dead-lettered must STAY dead-lettered, because dropping them
// was a decision about unattended work, not a pause.
func TestGlobalCeiling_MidStormTripThenWindowRollResumesWork(t *testing.T) {
	const (
		limb   = 2
		window = time.Hour
	)
	var runs atomic.Int64
	// No CI watch here: this is about the DISPATCHER's park/drop split and the
	// window roll, and an armed watch would hold the surviving item in
	// awaiting_ci where its terminal is the watcher's to write, not the
	// dispatcher's. The retry-storm tests above are where the whole loop runs.
	b, clk := globalCapQueueBroker(t, 2, func(_ context.Context, _ []string, stdout, _ io.Writer) error {
		runs.Add(1)
		fmt.Fprintln(stdout, `{"type":"result","subtype":"success"}`)
		return nil
	})
	b.GlobalMaxTasks = limb
	// Two starts half an hour ago: the limb is exhausted now and rolls clear
	// when the clock passes them.
	b.GlobalLedger = capLedgerAt(t, b.AuditRoot, window, limb, 0, capNow-int64(30*time.Minute/time.Millisecond))

	b.StartDispatcher()
	defer b.StopDispatcher()

	human, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git", Instruction: "human", AutoApprove: true})
	if err != nil {
		t.Fatal(err)
	}
	retry, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git", Instruction: "retry",
		AutoApprove: true, RetryOf: newID(), Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}

	// The unattended retry is dropped; the human item parks.
	waitForQueueState(t, b, retry, QueueDeadLetter)
	time.Sleep(60 * time.Millisecond)
	if got := queueItemState(t, b, human).State; got != QueueQueued {
		t.Fatalf("human item = %q, want queued (parked behind the ceiling)", got)
	}
	if got := runs.Load(); got != 0 {
		t.Fatalf("%d agents ran while the ceiling was exhausted", got)
	}

	// Roll the window past the two starts that exhausted the limb.
	clk.advance(31 * time.Minute)
	waitForQueueState(t, b, human, QueueCompleted)
	if got := runs.Load(); got != 1 {
		t.Errorf("agent runs after the roll = %d, want exactly 1 (the parked human item)", got)
	}
	// The dropped retry is a TERMINAL decision. The roll must not resurrect it.
	if got := queueItemState(t, b, retry).State; got != QueueDeadLetter {
		t.Fatalf("the dropped retry moved to %q when the window rolled; a drop is not a pause", got)
	}
	// And the roll did not lose the new start: the ledger holds it.
	if u := b.GlobalLedger.Usage(clk.now()); u.Starts != 1 {
		t.Errorf("after the roll the ledger reports %d in-window starts, want 1 (the one that just ran)", u.Starts)
	}
}
