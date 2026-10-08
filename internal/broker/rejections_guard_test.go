package broker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func rejectionQueueBroker(t *testing.T, maxConcurrent int, runs *atomic.Int32) *Broker {
	t.Helper()
	b := queueBroker(t, maxConcurrent, func(ctx context.Context, args []string, stdout, stderr io.Writer) error {
		if runs != nil {
			runs.Add(1)
		}
		_, _ = io.WriteString(stdout, `{"type":"result","subtype":"success"}`+"\n")
		return nil
	})
	b.Rejections = mustOpenRejections(t, b.AuditRoot)
	b.MaxDenials = 2
	return b
}

func waitPending(t *testing.T, b *Broker, id string) {
	t.Helper()
	if !waitFor(5*time.Second, func() bool {
		b.pendingMu.Lock()
		defer b.pendingMu.Unlock()
		_, ok := b.pending[id]
		return ok
	}) {
		t.Fatalf("%s never reached the gate", id)
	}
}

// Enqueue task, deny it at the gate, and wait for its terminal.
func enqueueAndDeny(t *testing.T, b *Broker, task Task) string {
	t.Helper()
	id, err := b.Enqueue(task)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitPending(t, b, id)
	deny(t, b, id)
	waitForQueueState(t, b, id, QueueCancelled)
	return id
}

func degradedLedger(reason string) *RejectionLedger {
	return &RejectionLedger{loadErr: reason, byKey: map[string]*rejectionKeyState{}, diffs: map[string]map[string]string{}}
}

// THE BOUND. Two human denials of the same repo+instruction, and the third
// queue add is refused with 409 before anything is persisted or run.
func TestRejectionLoop_ThirdEnqueueRefusedBeforeSpend(t *testing.T) {
	var runs atomic.Int32
	b := rejectionQueueBroker(t, 1, &runs)
	b.StartDispatcher()
	defer b.StopDispatcher()

	id1 := enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "add a backdoor"})
	id2 := enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "add a backdoor"})

	_, err := b.Enqueue(Task{RepoRef: "git@github.com:O/R", Instruction: "add a backdoor"})
	var loop *RejectionLoopError
	if !errors.As(err, &loop) {
		t.Fatalf("LOOP BREACH: third enqueue admitted (err=%v)", err)
	}
	if loop.Denials != 2 || loop.MaxDenials != 2 || len(loop.DeniedTaskIDs) != 2 ||
		loop.DeniedTaskIDs[0] != id2 || loop.DeniedTaskIDs[1] != id1 {
		t.Fatalf("refusal detail: %+v", loop)
	}
	if items, _ := listQueueItems(b.AuditRoot); len(items) != 2 {
		t.Fatalf("refused enqueue persisted an item: %d items", len(items))
	}
	if runs.Load() != 2 {
		t.Fatalf("LOOP BREACH: agent ran %d times, want 2", runs.Load())
	}

	rec := httptest.NewRecorder()
	b.HandleQueueAdd(rec, httptest.NewRequest("POST", "/queue",
		strings.NewReader(`{"repo_ref":"`+rtRepo+`","instruction":"add a backdoor"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d body=%s, want 409", rec.Code, rec.Body)
	}
	var body struct {
		Error         string   `json:"error"`
		Repo          string   `json:"repo"`
		Denials       int      `json:"denials"`
		MaxDenials    int      `json:"max_denials"`
		DeniedTaskIDs []string `json:"denied_task_ids"`
		Hint          string   `json:"hint"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("409 body is not JSON: %s", rec.Body)
	}
	if body.Error != "rejection_loop" || body.Repo != "github.com/o/r" || body.Denials != 2 ||
		len(body.DeniedTaskIDs) != 2 || !strings.Contains(body.Hint, "drydock submit") {
		t.Fatalf("409 body: %+v", body)
	}
	// A different instruction is a different key and is admitted.
	// (enqueueAndDeny fails the test on a refusal, and settles the admitted
	// run so it cannot outlive the test's temp dir.)
	enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "add a backdoor, but tested"})
}

// The issue key: two denials under one issue URL with different instruction
// text (the author edited the issue), and a third edit is still refused.
func TestRejectionLoop_IssueKeyTripsAcrossEditedInstruction(t *testing.T) {
	b := rejectionQueueBroker(t, 1, nil)
	b.StartDispatcher()
	defer b.StopDispatcher()
	const url = "https://github.com/o/r/issues/42"
	enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "# Issue #42: v1", IssueURL: url})
	enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "# Issue #42: v2", IssueURL: url})
	_, err := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "# Issue #42: v3", IssueURL: url})
	var loop *RejectionLoopError
	if !errors.As(err, &loop) || loop.IssueURL != url {
		t.Fatalf("edited issue evaded the bound: %v", err)
	}
	// The same text WITHOUT the issue url is a fresh instruction key: admitted
	// (that is a hand resubmit, and the sync path is the override anyway).
	enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "# Issue #42: v3"})
}

// Moved from Task 3: an auto-approved push must not reopen the queue path.
func TestRejectionLoop_AutoApproveDoesNotReopenEnqueue(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, writesResult(`{"type":"result","subtype":"success"}`))
	b.Rejections = mustOpenRejections(t, b.AuditRoot)
	b.MaxDenials = 2
	body := `{"repo_ref":"` + rtRepo + `","instruction":"do x","agent":"claude"}`
	runSync(t, b, body, deny)
	runSync(t, b, body, deny)
	submit(b, `{"repo_ref":"`+rtRepo+`","instruction":"do x","agent":"claude","auto_approve":true}`)
	if _, err := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "do x"}); err == nil {
		t.Fatal("enqueue admitted after an auto-approve that must not reset the count")
	}
}

// An item enqueued at one denial, whose key reaches the bound before it
// dispatches, is dead-lettered at dispatch with the reason, and no global
// ceiling claim is left behind.
func TestRejectionLoop_DispatchRecheckDropsLateItem(t *testing.T) {
	b := rejectionQueueBroker(t, 1, nil)
	b.StartDispatcher()
	defer b.StopDispatcher()
	id1, err := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "x"})
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "x"})
	id3, _ := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "x"})
	for _, id := range []string{id1, id2} {
		waitPending(t, b, id)
		deny(t, b, id)
		waitForQueueState(t, b, id, QueueCancelled)
	}
	waitForQueueState(t, b, id3, QueueDeadLetter)
	it := queueItemState(t, b, id3)
	if !strings.Contains(it.LastError, "denied 2 times") || !strings.Contains(it.LastError, "since its last approval") ||
		!strings.Contains(it.LastError, id2) {
		t.Fatalf("dead-letter reason: %q", it.LastError)
	}
	if n := b.inFlightStarts(); n != 0 {
		t.Fatalf("a dropped item leaked %d global-ceiling claim(s)", n)
	}
}

// Review Focus 2: a denial landing while the key's next item is ALREADY
// running must not touch that run; it reaches its own gate.
func TestRejectionLoop_RunningItemIsNotKilledByALaterDenial(t *testing.T) {
	b := rejectionQueueBroker(t, 2, nil)
	b.MaxDenials = 1
	b.StartDispatcher()
	defer b.StopDispatcher()
	id1, _ := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "x"})
	id2, _ := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "x"})
	waitPending(t, b, id1)
	waitPending(t, b, id2)
	deny(t, b, id1)
	waitForQueueState(t, b, id1, QueueCancelled)
	if st := queueItemState(t, b, id2).State; st != QueueAwaitingReview {
		t.Fatalf("running item was disturbed by a later denial: %s", st)
	}
	approve(t, b, id2)
	waitForQueueState(t, b, id2, QueueCompleted)
}

// A degraded ledger fails closed with the guard on: Enqueue refuses
// (503-mapped), dispatch parks. With max_denials 0 it is identity.
func TestRejectionLoop_DegradedLedgerRefusesAndParks(t *testing.T) {
	b := rejectionQueueBroker(t, 1, nil)
	b.MaxDenials = 0
	id, err := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "x"})
	if err != nil {
		t.Fatal(err)
	}
	b.Rejections = degradedLedger("line 3 is not a ledger entry; fix or remove it and restart brokerd")
	// Off is identity, even degraded.
	if _, err := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "y"}); err != nil {
		t.Fatalf("max_denials 0 must admit even with a degraded ledger: %v", err)
	}
	b.MaxDenials = 2
	_, err = b.Enqueue(Task{RepoRef: rtRepo, Instruction: "z"})
	var degraded *RejectionLedgerDegradedError
	if !errors.As(err, &degraded) {
		t.Fatalf("degraded ledger admitted an enqueue: %v", err)
	}
	rec := httptest.NewRecorder()
	b.HandleQueueAdd(rec, httptest.NewRequest("POST", "/queue",
		strings.NewReader(`{"repo_ref":"`+rtRepo+`","instruction":"z"}`)))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "rejection_ledger_degraded") ||
		!strings.Contains(rec.Body.String(), "line 3") {
		t.Fatalf("code=%d body=%s, want 503 naming the line", rec.Code, rec.Body)
	}
	if _, ok := b.takeDispatchable(); ok {
		t.Fatal("dispatch did not park on a degraded ledger")
	}
	if st := queueItemState(t, b, id).State; st != QueueQueued {
		t.Fatalf("parked item changed state: %s", st)
	}
}

// max_denials 0: the guard is off, but the ledger is still written.
func TestRejectionLoop_OffAtZeroStillRecords(t *testing.T) {
	b := rejectionQueueBroker(t, 1, nil)
	b.MaxDenials = 0
	b.StartDispatcher()
	defer b.StopDispatcher()
	for i := 0; i < 3; i++ {
		enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "x"})
	}
	repo, instr, _ := RejectionKeys(rtRepo, "", "x", "")
	if n, _ := b.Rejections.Denials(repo, instr, ""); n != 3 {
		t.Fatalf("ledger not written with the guard off: %d", n)
	}
}

// The CI-retry decision consults the guard BEFORE the enqueue-once mark: a
// tripped key refuses terminally with the detail recorded and the mark unset;
// a degraded ledger parks, exactly like the ceiling's unmeasured branch.
func TestRejectionLoop_RetryChildRefusedOrParked(t *testing.T) {
	t.Run("tripped key refuses", func(t *testing.T) {
		b := retryBroker(t, 3)
		b.Rejections = mustOpenRejections(t, b.AuditRoot)
		b.MaxDenials = 1
		parent := baseTask()
		it := seedTaskAwaitingCI(t, b, parent)
		repo, instr, _ := RejectionKeys(parent.RepoRef, parent.RootInstruction, parent.Instruction, parent.IssueURL)
		_ = b.Rejections.Record(RejectionEntry{Kind: RejectionKindDenied, AtMs: b.nowMs(), TaskID: rtID1,
			RepoKey: repo, InstructionSHA256: instr, DiffSHA256: "d", Path: "queue"})
		retryID, detail, park := b.maybeEnqueueCIRetry(failedObs(b, it), QueueCIFailed)
		if retryID != "" || park || !strings.Contains(detail, "no retry") || !strings.Contains(detail, "denied") {
			t.Fatalf("retryID=%q park=%v detail=%q", retryID, park, detail)
		}
		if cur := queueItemState(t, b, it.ID); cur.CIRetryEnqueued {
			t.Fatal("enqueue-once mark was consumed by a refused retry")
		}
	})
	t.Run("degraded ledger parks", func(t *testing.T) {
		b := retryBroker(t, 3)
		b.Rejections = degradedLedger("line 2 is not a ledger entry; fix or remove it and restart brokerd")
		b.MaxDenials = 2
		it := seedTaskAwaitingCI(t, b, baseTask())
		retryID, detail, park := b.maybeEnqueueCIRetry(failedObs(b, it), QueueCIFailed)
		if retryID != "" || !park || !strings.Contains(detail, "rejection ledger") {
			t.Fatalf("retryID=%q park=%v detail=%q", retryID, park, detail)
		}
	})
}
