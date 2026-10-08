package broker

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"drydock/internal/trustbrief"
)

const rtRepo = "https://github.com/o/r.git"

func mustOpenRejections(t *testing.T, auditRoot string) *RejectionLedger {
	t.Helper()
	l, err := OpenRejectionLedger(auditRoot)
	if err != nil {
		t.Fatalf("open rejection ledger: %v", err)
	}
	return l
}

// runSync submits body on the synchronous path, resolves its gate with
// decide (approve/deny) once pending, and returns the task id.
func runSync(t *testing.T, b *Broker, body string, decide func(*testing.T, *Broker, string)) string {
	t.Helper()
	done := make(chan struct{})
	go func() {
		b.HandleTask(httptest.NewRecorder(), httptest.NewRequest("POST", "/tasks", strings.NewReader(body)))
		close(done)
	}()
	id := waitForPending(t, b)
	decide(t, b, id)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("HandleTask did not return")
	}
	return id
}

func TestRejectionLoop_OnlyHumanDiffDenialsCount(t *testing.T) {
	const diff = "diff --git a/x b/x\n+y\n"
	st := &fakeStage{workDir: t.TempDir(), diff: diff}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, writesResult(`{"type":"result","subtype":"success"}`))
	b.Rejections = mustOpenRejections(t, b.AuditRoot)
	b.MaxDenials = 2
	body := `{"repo_ref":"` + rtRepo + `","instruction":"do x","agent":"claude"}`

	id := runSync(t, b, body, deny)
	repo, instr, _ := RejectionKeys(rtRepo, "", "do x", "")
	if n, ids := b.Rejections.Denials(repo, instr, ""); n != 1 || len(ids) != 1 || ids[0] != id {
		t.Fatalf("after one human deny: denials=%d ids=%v", n, ids)
	}
	if got, ok := b.Rejections.DeniedDiff(repo, trustbrief.HashDiff(diff)); !ok || got != id {
		t.Fatalf("denied diff not indexed: %q %v", got, ok)
	}
	// Non-human causes record nothing.
	tr := &taskRun{b: b, id: "0123456789abcdef0123456789abcdef", repoRef: rtRepo, instruction: "do x"}
	for _, c := range []gateCause{gateTimeout, gateKilled, gateShutdown} {
		b.recordGateVerdict(tr, c, "zzz", "live")
	}
	if n, _ := b.Rejections.Denials(repo, instr, ""); n != 1 {
		t.Fatalf("a non-human cause changed the count: %d", n)
	}
	// A human approve resets the key and frees the hash.
	runSync(t, b, body, approve)
	if n, _ := b.Rejections.Denials(repo, instr, ""); n != 0 {
		t.Fatalf("human approval did not reset the key: %d", n)
	}
	if _, ok := b.Rejections.DeniedDiff(repo, trustbrief.HashDiff(diff)); ok {
		t.Fatal("human approval did not free the denied diff hash")
	}
}

// Review Focus 3: auto-approve is not a human verdict. It neither resets a
// count nor frees a denied hash, on the sync path.
func TestRejectionLoop_HumanApprovalResetsAutoApproveDoesNot(t *testing.T) {
	const diff = "diff --git a/x b/x\n+y\n"
	st := &fakeStage{workDir: t.TempDir(), diff: diff}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, writesResult(`{"type":"result","subtype":"success"}`))
	b.Rejections = mustOpenRejections(t, b.AuditRoot)
	b.MaxDenials = 2
	body := `{"repo_ref":"` + rtRepo + `","instruction":"do x","agent":"claude"}`
	runSync(t, b, body, deny)
	runSync(t, b, body, deny)
	_, _, term := submit(b, `{"repo_ref":"`+rtRepo+`","instruction":"do x","agent":"claude","auto_approve":true}`)
	if term["outcome"] != "pushed" {
		t.Fatalf("auto-approved sync push: %v", term)
	}
	repo, instr, _ := RejectionKeys(rtRepo, "", "do x", "")
	if n, _ := b.Rejections.Denials(repo, instr, ""); n != 2 {
		t.Fatalf("auto-approve reset the human count: %d", n)
	}
	if _, ok := b.Rejections.DeniedDiff(repo, trustbrief.HashDiff(diff)); !ok {
		t.Fatal("auto-approve freed a human-denied diff hash")
	}
}

// Review Focus 1: the sync path keys on `instruction` alone.
func TestRejectionLoop_SyncPathIgnoresClientRootInstruction(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, writesResult(`{"type":"result","subtype":"success"}`))
	b.Rejections = mustOpenRejections(t, b.AuditRoot)
	b.MaxDenials = 2
	runSync(t, b, `{"repo_ref":"`+rtRepo+`","instruction":"do x","root_instruction":"someone else's chain","agent":"claude"}`, deny)
	repo, own, _ := RejectionKeys(rtRepo, "", "do x", "")
	_, forged, _ := RejectionKeys(rtRepo, "", "someone else's chain", "")
	if n, _ := b.Rejections.Denials(repo, own, ""); n != 1 {
		t.Fatalf("sync deny not keyed on instruction: %d", n)
	}
	if n, _ := b.Rejections.Denials(repo, forged, ""); n != 0 {
		t.Fatalf("client root_instruction reached the key: %d", n)
	}
}

// A retry child's live write keys on its chain, and a resumed gate marker
// reproduces the same keys, with a fallback for markers from older builds.
func TestRejectionLoop_RetryChainSharesParentKey(t *testing.T) {
	const url = "https://github.com/o/r/issues/7"
	// Live write from a queued run built from QueueItem.Task.
	b := queueBroker(t, 1, writesResult(`{"type":"result","subtype":"success"}`))
	b.Rejections = mustOpenRejections(t, b.AuditRoot)
	b.MaxDenials = 2
	child := Task{RepoRef: "git@github.com:O/R", Instruction: "root text\n\n<<ci evidence>>",
		RootInstruction: "root text", IssueURL: url, RetryOf: "0123456789abcdef0123456789abcdef", Attempt: 1}
	id, err := b.Enqueue(child)
	if err != nil {
		t.Fatal(err)
	}
	b.StartDispatcher()
	defer b.StopDispatcher()
	if !waitFor(5*time.Second, func() bool {
		b.pendingMu.Lock()
		defer b.pendingMu.Unlock()
		_, ok := b.pending[id]
		return ok
	}) {
		t.Fatal("child never reached the gate")
	}
	deny(t, b, id)
	waitForQueueState(t, b, id, QueueCancelled)
	repo, instr, issue := RejectionKeys(rtRepo, "", "root text", url)
	if n, _ := b.Rejections.Denials(repo, instr, issue); n != 1 {
		t.Fatalf("child's denial did not land on the chain key: %d", n)
	}
	// Resume-path keys: new marker fields, and the old-marker fallback.
	newM := gateMarker{RepoRef: "git@github.com:o/r", Instruction: "root text\n\n<<ci evidence>>", RootInstruction: "root text", IssueURL: url}
	oldM := gateMarker{RepoRef: rtRepo, Instruction: "root text"}
	_, fromNew, issueNew := RejectionKeys(newM.RepoRef, newM.RootInstruction, newM.Instruction, newM.IssueURL)
	_, fromOld, issueOld := RejectionKeys(oldM.RepoRef, oldM.RootInstruction, oldM.Instruction, oldM.IssueURL)
	if fromNew != instr || issueNew != issue || fromOld != instr || issueOld != "" {
		t.Fatal("resume-path keys do not match the live keys")
	}
}

// The resume path records too, from the marker's fields.
func TestRejectionLoop_ResumePathRecordsDenial(t *testing.T) {
	const diff = "diff --git a/x b/x\n+y\n"
	st := &fakeStage{workDir: t.TempDir(), diff: diff}
	b := testBroker(t, "anthropic", st, &fakeGrant{}, writesResult(`{"type":"result","subtype":"success"}`))
	b.Rejections = mustOpenRejections(t, b.AuditRoot)
	b.MaxDenials = 2
	b.reopenStage = func(string) (taskStage, error) { return st, nil }
	id := "0123456789abcdef0123456789abcdef"
	b.persistDiff(id, diff)
	if err := writeGateMarker(b.AuditRoot, id, gateMarker{RepoRef: rtRepo,
		Instruction: "root\n\nevidence", RootInstruction: "root", Agent: "claude",
		TaskStartMs: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	b.ResumeAwaiting(b.StageRoot)
	deny(t, b, waitForPending(t, b))
	repo, instr, _ := RejectionKeys(rtRepo, "", "root", "")
	if !waitFor(2*time.Second, func() bool { n, _ := b.Rejections.Denials(repo, instr, ""); return n == 1 }) {
		t.Fatal("resume-path deny was not recorded on the chain key")
	}
}
