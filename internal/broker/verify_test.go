package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"drydock/internal/trustbrief"
)

// These tests drive the verifying stage (runVerify + the push-time tree
// guard) through the same b.runAgent seam and fakes handle_task_test.go uses.
// The whole point of the stage is that the VERDICT comes from the container
// process exit code observed by the broker — never from anything the verify
// VM printed — so several tests below forge output and assert it changes
// nothing.

// verifyStage adds the optional staged-tree export capability to the shared
// fakeStage. It is deliberately a SEPARATE wrapper type: the brief tests rely
// on the shared fakeStage lacking optional capabilities (e.g. BaseCommit), so
// those methods must not be added to fakeStage itself.
type verifyStage struct {
	*fakeStage
	trees     []string // successive StagedTreeHash results; the last repeats
	treeCalls atomic.Int32
	treeErr   error
	exportErr error
	exported  atomic.Bool
	exportDst atomic.Value // string
}

func (v *verifyStage) StagedTreeHash() (string, error) {
	if v.treeErr != nil {
		return "", v.treeErr
	}
	n := int(v.treeCalls.Add(1)) - 1
	if n >= len(v.trees) {
		n = len(v.trees) - 1
	}
	return v.trees[n], nil
}

func (v *verifyStage) ExportStaged(dst string) error {
	if v.exportErr != nil {
		return v.exportErr
	}
	v.exported.Store(true)
	v.exportDst.Store(dst)
	return nil
}

// isVerifyArgs reports whether a runAgent argv is a verifier-VM run (named
// verify-<id>) as opposed to the main agent sandbox run (task-<id>).
func isVerifyArgs(args []string) bool {
	for i, a := range args {
		if a == "--name" && i+1 < len(args) && strings.HasPrefix(args[i+1], "verify-") {
			return true
		}
	}
	return false
}

// splitRun returns a runAgent seam that emits a successful agent result for
// the sandbox run and hands verifier-VM runs to verifyFn.
func splitRun(verifyFn func(ctx context.Context, args []string, stdout, stderr io.Writer) error,
) func(context.Context, []string, io.Writer, io.Writer) error {
	return func(ctx context.Context, args []string, stdout, stderr io.Writer) error {
		if isVerifyArgs(args) {
			return verifyFn(ctx, args, stdout, stderr)
		}
		fmt.Fprintln(stdout, `{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"total_cost_usd":0.01,"num_turns":1}`)
		return nil
	}
}

// realExitErr produces a genuine *exec.ExitError (exit code 1) the same way
// production sees one: from an actual process exit. runVerify classifies
// exit-vs-infra errors via errors.As on *exec.ExitError, which cannot be
// constructed literally.
func realExitErr(t *testing.T) error {
	t.Helper()
	err := exec.Command("false").Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("exec false did not yield an ExitError: %v", err)
	}
	return err
}

func readBrief(t *testing.T, b *Broker, id string) trustbrief.Brief {
	t.Helper()
	br, err := trustbrief.Read(b.AuditRoot, id)
	if err != nil {
		t.Fatalf("brief not written for %s: %v", id, err)
	}
	return br
}

func taskID(t *testing.T, events []map[string]any) string {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no events")
	}
	id, _ := events[0]["task_id"].(string)
	if id == "" {
		t.Fatalf("no task_id in first event: %v", events[0])
	}
	return id
}

func TestRunVerify_NotConfigured_PassesThrough(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	// No b.Verify entry at all.
	rec, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)

	if term["outcome"] != "pushed" {
		t.Fatalf("outcome=%v, want pushed; body=%s", term["outcome"], rec.Body)
	}
	got := stages(events)
	for _, s := range got {
		if s == "verifying" {
			t.Errorf("stage sequence %v must not contain 'verifying' when no verify config exists", got)
		}
	}
	id := taskID(t, events)
	br := readBrief(t, b, id)
	if br.Verification.Status != trustbrief.VerificationNotConfigured {
		t.Errorf("brief verification status=%q, want not_configured", br.Verification.Status)
	}
	if _, err := os.Stat(filepath.Join(b.AuditRoot, id+".verify.log")); !os.IsNotExist(err) {
		t.Error("no verify log should exist for an unconfigured repo")
	}
}

func TestRunVerify_AllPass_RecordedInBrief(t *testing.T) {
	st := &verifyStage{
		fakeStage: &fakeStage{workDir: filepath.Join(t.TempDir(), "work"), diff: "diff --git a/x b/x\n+y\n"},
		trees:     []string{"tree-sha-1"},
	}
	grant := &fakeGrant{}
	const noise = "verify VM noise: compiling 42 objects"
	verifyFn := func(_ context.Context, _ []string, stdout, _ io.Writer) error {
		time.Sleep(5 * time.Millisecond) // make stage_ms.verifying observable
		fmt.Fprintln(stdout, noise)
		return nil
	}
	b := testBroker(t, "anthropic", st, grant, splitRun(verifyFn))
	b.Verify = map[string]VerifyRepo{
		"github.com/o/r": {Commands: [][]string{{"go", "test", "./..."}, {"go", "vet", "./..."}}},
	}
	rec, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)

	if term["outcome"] != "pushed" {
		t.Fatalf("outcome=%v, want pushed; body=%s", term["outcome"], rec.Body)
	}
	got := stages(events)
	want := []string{"preparing", "running", "verifying", "pushing"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("stage sequence = %v, want %v", got, want)
	}
	if !st.exported.Load() {
		t.Error("ExportStaged was never called — verification must run on the sealed export, not the live work tree")
	}
	if dst, _ := st.exportDst.Load().(string); dst != filepath.Join(filepath.Dir(st.workDir), "verify") {
		t.Errorf("export dst=%q, want <stage-root>/verify beside the work dir", dst)
	}

	id := taskID(t, events)
	br := readBrief(t, b, id)
	v := br.Verification
	if v.Status != trustbrief.VerificationPassed {
		t.Fatalf("verification status=%q, want passed (%+v)", v.Status, v)
	}
	if v.Network != "denied" || v.Credentials != "none" {
		t.Errorf("posture=%q/%q, want denied/none", v.Network, v.Credentials)
	}
	if v.TreeSHA != "tree-sha-1" {
		t.Errorf("tree_sha=%q, want tree-sha-1", v.TreeSHA)
	}
	if len(v.Commands) != 2 {
		t.Fatalf("commands=%d, want 2 (%+v)", len(v.Commands), v.Commands)
	}
	for i, c := range v.Commands {
		if c.Status != trustbrief.VerifyCmdPassed || c.ExitCode != 0 {
			t.Errorf("command %d = %+v, want passed/exit 0", i, c)
		}
	}

	// The verify log: 0600, contains the VM noise, and LogSHA256 matches the
	// file bytes (computed host-side).
	logPath := filepath.Join(b.AuditRoot, id+".verify.log")
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("verify log missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("verify log mode=%o, want 0600", perm)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), noise) {
		t.Errorf("verify log does not contain the VM output:\n%s", data)
	}
	sum := sha256.Sum256(data)
	if v.LogSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("log_sha256=%q, want %q (sha256 of the log file)", v.LogSHA256, hex.EncodeToString(sum[:]))
	}

	// Metrics: the final row records the verifying stage duration.
	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	sm, _ := m["stage_ms"].(map[string]any)
	if sm == nil {
		t.Fatalf("no stage_ms in metrics row: %v", m)
	}
	if ms, _ := sm["verifying"].(float64); ms <= 0 {
		t.Errorf("stage_ms.verifying=%v, want > 0", sm["verifying"])
	}
}

func TestRunVerify_FailFastAndAdvisory(t *testing.T) {
	st := &verifyStage{
		fakeStage: &fakeStage{workDir: filepath.Join(t.TempDir(), "work"), diff: "diff --git a/x b/x\n+y\n"},
		trees:     []string{"tree-a"},
	}
	grant := &fakeGrant{}
	exitErr := realExitErr(t)
	var verifyCalls atomic.Int32
	verifyFn := func(_ context.Context, _ []string, _, _ io.Writer) error {
		verifyCalls.Add(1)
		return exitErr
	}
	b := testBroker(t, "anthropic", st, grant, splitRun(verifyFn))
	b.Verify = map[string]VerifyRepo{
		"github.com/o/r": {Commands: [][]string{{"go", "test"}, {"go", "vet"}}}, // Required: false — advisory
	}

	// Gated flow: a failed-but-advisory verification must still reach the gate.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/tasks",
		strings.NewReader(`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`))
	done := make(chan struct{})
	go func() {
		b.HandleTask(rec, req)
		close(done)
	}()
	id := waitForPending(t, b)
	approve(t, b, id)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleTask did not return after approve")
	}

	events, term := parseEvents(rec.Body.String())
	if term["outcome"] != "pushed" {
		t.Fatalf("outcome=%v, want pushed (advisory verification must not block); body=%s", term["outcome"], rec.Body)
	}
	if n := verifyCalls.Load(); n != 1 {
		t.Errorf("verify runAgent calls=%d, want 1 (fail fast after the first failure)", n)
	}

	br := readBrief(t, b, id)
	v := br.Verification
	if v.Status != trustbrief.VerificationFailed {
		t.Fatalf("verification status=%q, want failed (%+v)", v.Status, v)
	}
	if len(v.Commands) != 2 ||
		v.Commands[0].Status != trustbrief.VerifyCmdFailed ||
		v.Commands[1].Status != trustbrief.VerifyCmdSkipped {
		t.Errorf("commands=%+v, want [failed, skipped]", v.Commands)
	}
	if v.Commands[0].ExitCode != 1 {
		t.Errorf("exit_code=%d, want 1 (the broker-observed process exit)", v.Commands[0].ExitCode)
	}

	// The gate announcement must surface the verify status to the reviewer.
	var gate map[string]any
	for _, ev := range events {
		if ev["stage"] == "awaiting_approval" {
			gate = ev
		}
	}
	if gate == nil {
		t.Fatalf("no awaiting_approval event; body=%s", rec.Body)
	}
	if gate["verify"] != "failed" {
		t.Errorf("awaiting_approval verify=%v, want \"failed\"", gate["verify"])
	}
}

func TestRunVerify_RequiredBlocksOnFailure(t *testing.T) {
	st := &verifyStage{
		fakeStage: &fakeStage{workDir: filepath.Join(t.TempDir(), "work"), diff: "diff --git a/x b/x\n+y\n"},
		trees:     []string{"tree-a"},
	}
	grant := &fakeGrant{}
	exitErr := realExitErr(t)
	verifyFn := func(_ context.Context, _ []string, _, _ io.Writer) error { return exitErr }
	b := testBroker(t, "anthropic", st, grant, splitRun(verifyFn))
	b.Verify = map[string]VerifyRepo{
		"github.com/o/r": {Commands: [][]string{{"go", "test"}}, Required: true},
	}
	// Not auto-approved: if the block were (wrongly) skipped the task would sit
	// at the gate; the approval timeout turns that bug into a clean "denied"
	// terminal instead of a hang.
	b.ApprovalTimeout = 200 * time.Millisecond

	rec, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`)

	if term["outcome"] != "verify_failed" {
		t.Fatalf("outcome=%v, want verify_failed; body=%s", term["outcome"], rec.Body)
	}
	if term["verify_status"] != "failed" {
		t.Errorf("verify_status=%v, want failed", term["verify_status"])
	}
	for _, ev := range events {
		if ev["stage"] == "awaiting_approval" {
			t.Error("required-and-failed verification must never enter the approval gate")
		}
	}
	if st.pushed.Load() {
		t.Error("nothing may be pushed when required verification failed")
	}
	id := taskID(t, events)
	audit := readAudit(t, b.AuditRoot, id)
	if !strings.Contains(audit, `"subtype":"verify_failed"`) {
		t.Errorf("audit missing the broker-authored verify_failed result line:\n%s", audit)
	}
	m := lastMetricsLine(t, audit)
	if m["outcome"] != "verify_failed" {
		t.Errorf("metrics outcome=%v, want verify_failed", m["outcome"])
	}
}

func TestRunVerify_RequiredBlocksOnInconclusive(t *testing.T) {
	// The shared fakeStage has NO StagedTreeHash/ExportStaged capability, so
	// verification cannot run → inconclusive. Required=true must fail closed;
	// Required=false proceeds with the gap recorded in the brief.
	t.Run("required=true blocks", func(t *testing.T) {
		st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
		grant := &fakeGrant{}
		b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
		b.Verify = map[string]VerifyRepo{
			"github.com/o/r": {Commands: [][]string{{"go", "test"}}, Required: true},
		}
		b.ApprovalTimeout = 200 * time.Millisecond
		rec, _, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`)
		if term["outcome"] != "verify_failed" {
			t.Fatalf("outcome=%v, want verify_failed (inconclusive must fail closed when required); body=%s",
				term["outcome"], rec.Body)
		}
		if term["verify_status"] != "inconclusive" {
			t.Errorf("verify_status=%v, want inconclusive", term["verify_status"])
		}
		if st.pushed.Load() {
			t.Error("nothing may be pushed when required verification is inconclusive")
		}
	})
	t.Run("required=false proceeds", func(t *testing.T) {
		st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
		grant := &fakeGrant{}
		b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
		b.Verify = map[string]VerifyRepo{
			"github.com/o/r": {Commands: [][]string{{"go", "test"}}},
		}
		rec, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
		if term["outcome"] != "pushed" {
			t.Fatalf("outcome=%v, want pushed (advisory inconclusive proceeds); body=%s", term["outcome"], rec.Body)
		}
		id := taskID(t, events)
		br := readBrief(t, b, id)
		if br.Verification.Status != trustbrief.VerificationInconclusive {
			t.Fatalf("verification status=%q, want inconclusive", br.Verification.Status)
		}
		found := false
		for _, m := range br.MissingEvidence {
			if strings.Contains(m, "verification inconclusive") {
				found = true
			}
		}
		if !found {
			t.Errorf("missing_evidence=%v, want an 'inconclusive — treat as unverified' line", br.MissingEvidence)
		}
	})
}

func TestRunVerify_TimeoutIsInconclusiveNeverPassed(t *testing.T) {
	st := &verifyStage{
		fakeStage: &fakeStage{workDir: filepath.Join(t.TempDir(), "work"), diff: "diff --git a/x b/x\n+y\n"},
		trees:     []string{"tree-a"},
	}
	grant := &fakeGrant{}
	verifyFn := func(ctx context.Context, _ []string, _, _ io.Writer) error {
		<-ctx.Done() // block until the per-command timeout kills the run
		return ctx.Err()
	}
	b := testBroker(t, "anthropic", st, grant, splitRun(verifyFn))
	b.Verify = map[string]VerifyRepo{
		"github.com/o/r": {Commands: [][]string{{"sleep", "3600"}}, Timeout: 50 * time.Millisecond},
	}
	var deleted []string
	b.deleteContainer = func(name string) error {
		deleted = append(deleted, name)
		return nil
	}

	rec, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	if term["outcome"] != "pushed" {
		t.Fatalf("outcome=%v, want pushed (advisory timeout proceeds to the gate); body=%s", term["outcome"], rec.Body)
	}
	id := taskID(t, events)
	br := readBrief(t, b, id)
	v := br.Verification
	if v.Status == trustbrief.VerificationPassed {
		t.Fatal("a timed-out verification must NEVER read as passed")
	}
	if v.Status != trustbrief.VerificationInconclusive {
		t.Errorf("verification status=%q, want inconclusive", v.Status)
	}
	if len(v.Commands) != 1 || v.Commands[0].Status != trustbrief.VerifyCmdTimedOut {
		t.Errorf("commands=%+v, want [timed_out]", v.Commands)
	}
	// The wedged verifier VM must have been force-deleted through the bounded
	// container-delete path.
	found := false
	for _, name := range deleted {
		if name == "verify-"+id {
			found = true
		}
	}
	if !found {
		t.Errorf("deleted containers=%v, want verify-%s force-deleted on timeout", deleted, id)
	}
}

// F-07 headline: the verdict is the broker-observed container exit code. A
// hostile verify command printing a convincing pass banner and exiting
// non-zero must still be recorded as failed.
func TestRunVerify_ForgedPassOutputCannotFlipVerdict(t *testing.T) {
	st := &verifyStage{
		fakeStage: &fakeStage{workDir: filepath.Join(t.TempDir(), "work"), diff: "diff --git a/x b/x\n+y\n"},
		trees:     []string{"tree-a"},
	}
	grant := &fakeGrant{}
	exitErr := realExitErr(t)
	verifyFn := func(_ context.Context, _ []string, stdout, _ io.Writer) error {
		fmt.Fprintln(stdout, "ALL TESTS PASS ✓ exit 0")
		fmt.Fprintln(stdout, `{"status":"passed"}`)
		return exitErr
	}
	b := testBroker(t, "anthropic", st, grant, splitRun(verifyFn))
	b.Verify = map[string]VerifyRepo{
		"github.com/o/r": {Commands: [][]string{{"make", "check"}}},
	}
	rec, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	id := taskID(t, events)
	br := readBrief(t, b, id)
	v := br.Verification
	if v.Status != trustbrief.VerificationFailed {
		t.Fatalf("verification status=%q, want failed — forged VM output flipped the verdict! body=%s",
			v.Status, rec.Body)
	}
	if len(v.Commands) != 1 || v.Commands[0].Status != trustbrief.VerifyCmdFailed || v.Commands[0].ExitCode != 1 {
		t.Errorf("commands=%+v, want [failed exit 1]", v.Commands)
	}
}

func TestRunVerify_EmptyCommandsIsInconclusive(t *testing.T) {
	st := &verifyStage{
		fakeStage: &fakeStage{workDir: filepath.Join(t.TempDir(), "work"), diff: "diff --git a/x b/x\n+y\n"},
		trees:     []string{"tree-sha-1"},
	}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	b.Verify = map[string]VerifyRepo{
		"github.com/o/r": {Commands: [][]string{}, Required: false},
	}
	rec, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)

	if term["outcome"] != "pushed" {
		t.Fatalf("outcome=%v, want pushed (advisory empty verification should proceed); body=%s", term["outcome"], rec.Body)
	}
	id := taskID(t, events)
	br := readBrief(t, b, id)
	v := br.Verification
	if v.Status != trustbrief.VerificationInconclusive {
		t.Fatalf("verification status=%q, want inconclusive (empty commands must not pass)", v.Status)
	}
	if len(v.Commands) != 0 {
		t.Errorf("commands=%v, want empty list (no commands were configured to run)", v.Commands)
	}
}

func TestFinishPush_TreeMismatchFailsClosed(t *testing.T) {
	st := &verifyStage{
		fakeStage: &fakeStage{workDir: filepath.Join(t.TempDir(), "work"), diff: "diff --git a/x b/x\n+y\n"},
		trees:     []string{"aaa", "bbb"}, // verified against aaa; changed to bbb by push time
	}
	grant := &fakeGrant{}
	verifyFn := func(_ context.Context, _ []string, _, _ io.Writer) error { return nil }
	b := testBroker(t, "anthropic", st, grant, splitRun(verifyFn))
	b.Verify = map[string]VerifyRepo{
		"github.com/o/r": {Commands: [][]string{{"go", "test"}}},
	}
	rec, _, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)

	if term["outcome"] != "push_failed" {
		t.Fatalf("outcome=%v, want push_failed (fail closed on tree drift); body=%s", term["outcome"], rec.Body)
	}
	reason, _ := term["reason"].(string)
	if !strings.Contains(reason, "mismatch") {
		t.Errorf("reason=%q, want a verified-tree mismatch reason", reason)
	}
	if st.pushed.Load() {
		t.Error("nothing may be pushed when the work tree changed after verification")
	}
}
