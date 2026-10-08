package broker

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"drydock/internal/trustbrief"
)

// A queue broker whose every stage returns the same diff, recording each
// stage so a test can assert nothing was pushed.
func sameDiffQueueBroker(t *testing.T, diff string) (*Broker, func() []*fakeStage) {
	t.Helper()
	b := queueBroker(t, 1, writesResult(`{"type":"result","subtype":"success"}`))
	var mu sync.Mutex
	var stages []*fakeStage
	b.prepareStage = func(context.Context, string, string) (taskStage, error) {
		st := &fakeStage{workDir: t.TempDir(), diff: diff}
		mu.Lock()
		stages = append(stages, st)
		mu.Unlock()
		return st, nil
	}
	b.Rejections = mustOpenRejections(t, b.AuditRoot)
	b.MaxDenials = 2
	return b, func() []*fakeStage { mu.Lock(); defer mu.Unlock(); return append([]*fakeStage(nil), stages...) }
}

// A queued task whose diff is byte-identical to a denied one never re-poses
// the gate, even with auto_approve, never pushes, and lands dead_letter.
func TestRejectionLoop_RepeatDiffNeverReposesGate(t *testing.T) {
	const diff = "diff --git a/auth.go b/auth.go\n+// backdoor\n"
	b, stages := sameDiffQueueBroker(t, diff)
	b.StartDispatcher()
	defer b.StopDispatcher()

	denied := enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "first wording"})

	id, err := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "second wording", AutoApprove: true})
	if err != nil {
		t.Fatal(err)
	}
	waitForQueueState(t, b, id, QueueDeadLetter)
	it := queueItemState(t, b, id)
	if it.LastError != "auto-denied: identical to the diff denied in task "+denied {
		t.Fatalf("last_error=%q", it.LastError)
	}
	if _, err := os.Stat(gateMarkerPath(b.AuditRoot, id)); err == nil {
		t.Fatal("a gate marker was written for an auto-denied task")
	}
	for _, st := range stages() {
		if st.pushed.Load() {
			t.Fatal("a repeat of a denied diff was pushed")
		}
	}
	audit, _ := os.ReadFile(filepath.Join(b.AuditRoot, id+".jsonl"))
	if !strings.Contains(string(audit), `"subtype":"denied"`) || !strings.Contains(string(audit), `"repeat_of":"`+denied+`"`) ||
		!strings.Contains(string(audit), `"type":"metrics"`) {
		t.Fatalf("audit lacks the broker-authored repeat denial: %s", audit)
	}
	brief, err := trustbrief.Read(b.AuditRoot, id)
	if err != nil || brief.Diff.RepeatOfDenied != denied {
		t.Fatalf("brief repeat_of_denied=%q err=%v", brief.Diff.RepeatOfDenied, err)
	}
	// No ledger entry for an auto-deny.
	repo, instr, _ := RejectionKeys(rtRepo, "", "second wording", "")
	if n, _ := b.Rejections.Denials(repo, instr, ""); n != 0 {
		t.Fatalf("auto-deny wrote a ledger entry: %d", n)
	}
}

// Review Focus 4: a truncated capture is never matched.
func TestRejectionLoop_TruncatedDiffIsNotMatched(t *testing.T) {
	// Build a diff that Analyze reports as truncated: a trailing line with
	// the exact prefix stage.gitDiffCapped appends (trustbrief's bpTruncated,
	// "... [diff truncated at ").
	diff := "diff --git a/x b/x\n+y\n... [diff truncated at 4 MiB]\n"
	if !trustbrief.Analyze(diff).Truncated {
		t.Fatal("test fixture: diff is not reported truncated; check bpTruncated in trustbrief/difffacts.go")
	}
	b, _ := sameDiffQueueBroker(t, diff)
	b.StartDispatcher()
	defer b.StopDispatcher()
	enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "a"})
	id, _ := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "b"})
	waitPending(t, b, id) // reached the gate: not auto-denied
	brief, _ := trustbrief.Read(b.AuditRoot, id)
	if brief.Diff.RepeatOfDenied != "" {
		t.Fatalf("truncated diff was matched: %q", brief.Diff.RepeatOfDenied)
	}
	deny(t, b, id)
	waitForQueueState(t, b, id, QueueCancelled)
}

// The synchronous path only WARNS: the gate is posed with repeat_of on the
// event, and a human approve frees the hash.
func TestRejectionLoop_SyncPathWarnsAndHumanApprovalFreesDiff(t *testing.T) {
	const diff = "diff --git a/x b/x\n+y\n"
	b, _ := sameDiffQueueBroker(t, diff)
	b.StartDispatcher()
	defer b.StopDispatcher()
	denied := enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "x"})

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		b.HandleTask(rec, httptest.NewRequest("POST", "/tasks",
			strings.NewReader(`{"repo_ref":"`+rtRepo+`","instruction":"x again","agent":"claude"}`)))
		close(done)
	}()
	id := waitForPending(t, b)
	approve(t, b, id)
	<-done
	events, term := parseEvents(rec.Body.String())
	if term["outcome"] != "pushed" {
		t.Fatalf("human-approved sync push: %v", term)
	}
	found := false
	for _, ev := range events {
		if ev["stage"] == "awaiting_approval" && ev["repeat_of"] == denied {
			found = true
		}
	}
	if !found {
		t.Fatalf("sync gate event lacks repeat_of=%s: %s", denied, rec.Body)
	}
	repo, _, _ := RejectionKeys(rtRepo, "", "", "")
	if _, ok := b.Rejections.DeniedDiff(repo, trustbrief.HashDiff(diff)); ok {
		t.Fatal("human approval did not free the denied diff hash")
	}
}

// max_denials 0 turns the auto-deny off on the queue path too: the repeat
// reaches the gate, the brief still says it is a repeat, and a human deny is
// labeled as such.
func TestRejectionLoop_BackstopOffAtZero(t *testing.T) {
	const diff = "diff --git a/x b/x\n+y\n"
	b, _ := sameDiffQueueBroker(t, diff)
	b.StartDispatcher()
	defer b.StopDispatcher()
	denied := enqueueAndDeny(t, b, Task{RepoRef: rtRepo, Instruction: "x"})
	b.MaxDenials = 0
	id, _ := b.Enqueue(Task{RepoRef: rtRepo, Instruction: "y"})
	waitPending(t, b, id)
	brief, err := trustbrief.Read(b.AuditRoot, id)
	if err != nil || brief.Diff.RepeatOfDenied != denied {
		t.Fatalf("brief repeat_of_denied=%q err=%v", brief.Diff.RepeatOfDenied, err)
	}
	deny(t, b, id)
	waitForQueueState(t, b, id, QueueCancelled)
	if it := queueItemState(t, b, id); strings.Contains(it.LastError, "auto-denied") {
		t.Fatalf("a human deny was labeled auto-denied: %q", it.LastError)
	}
}
