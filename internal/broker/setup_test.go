package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"drydock/internal/trustbrief"
)

// These tests drive the setting_up stage (runSetup) through the same
// b.runAgent seam handle_task_test.go uses. The stage's security invariant is
// fail-closed-before-spend: a setup failure must terminate the task BEFORE
// the agent VM boots and BEFORE any grant bearer is injected into any VM —
// several tests below assert exactly that through the seam's recorded argvs.

// setupContainerName extracts the --name value from a runAgent argv so tests
// can tell setup-<id> / task-<id> / verify-<id> runs apart.
func setupContainerName(args []string) string {
	for i, a := range args {
		if a == "--name" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// runLog records every argv the b.runAgent seam saw, by container name, so a
// test can assert which VMs (setup-/task-) actually launched and what env
// each carried.
type runLog struct {
	mu    sync.Mutex
	argvs [][]string
}

func (l *runLog) record(args []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.argvs = append(l.argvs, append([]string(nil), args...))
}

func (l *runLog) all() [][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([][]string, len(l.argvs))
	copy(out, l.argvs)
	return out
}

// sawPrefix reports whether any recorded run's container name has the prefix.
func (l *runLog) sawPrefix(prefix string) bool {
	for _, args := range l.all() {
		if strings.HasPrefix(setupContainerName(args), prefix) {
			return true
		}
	}
	return false
}

// setupSplitRun returns a runAgent seam that records every run in log, hands
// setup-VM runs (setup-<id>) to setupFn, and emits a successful agent result
// for anything else (the task-<id> sandbox run).
func setupSplitRun(log *runLog, setupFn func(ctx context.Context, args []string, stdout, stderr io.Writer) error,
) func(context.Context, []string, io.Writer, io.Writer) error {
	return func(ctx context.Context, args []string, stdout, stderr io.Writer) error {
		log.record(args)
		if strings.HasPrefix(setupContainerName(args), "setup-") {
			return setupFn(ctx, args, stdout, stderr)
		}
		fmt.Fprintln(stdout, `{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"total_cost_usd":0.01,"num_turns":1}`)
		return nil
	}
}

func TestRunSetup_NotConfigured_PassesThrough(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	// No b.Setup entry at all.
	rec, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)

	if term["outcome"] != "pushed" {
		t.Fatalf("outcome=%v, want pushed; body=%s", term["outcome"], rec.Body)
	}
	for _, s := range stages(events) {
		if s == "setting_up" {
			t.Errorf("stage sequence %v must not contain 'setting_up' when no profile exists", stages(events))
		}
	}
	id := taskID(t, events)
	br := readBrief(t, b, id)
	if br.Setup.Status != trustbrief.SetupNotConfigured {
		t.Errorf("brief setup status=%q, want not_configured", br.Setup.Status)
	}
	if _, err := os.Stat(filepath.Join(b.AuditRoot, id+".setup.log")); !os.IsNotExist(err) {
		t.Error("no setup log should exist for an unconfigured repo")
	}
}

func TestRunSetup_AllPass_RecordedInBrief(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{}
	const noise = "setup VM noise: added 412 packages in 9s"
	log := &runLog{}
	setupFn := func(_ context.Context, _ []string, stdout, _ io.Writer) error {
		time.Sleep(5 * time.Millisecond) // make stage_ms.setup observable
		fmt.Fprintln(stdout, noise)
		return nil
	}
	b := testBroker(t, "anthropic", st, grant, setupSplitRun(log, setupFn))
	b.Setup = map[string]SetupProfile{
		"github.com/o/r": {
			Setup:     [][]string{{"npm", "ci"}},
			Readiness: [][]string{{"curl", "-fsS", "localhost:3000"}},
		},
	}
	rec, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)

	if term["outcome"] != "pushed" {
		t.Fatalf("outcome=%v, want pushed; body=%s", term["outcome"], rec.Body)
	}
	got := stages(events)
	want := []string{"preparing", "setting_up", "running", "pushing"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("stage sequence = %v, want %v", got, want)
	}

	id := taskID(t, events)
	br := readBrief(t, b, id)
	s := br.Setup
	if s.Status != trustbrief.SetupPassed {
		t.Fatalf("setup status=%q, want passed (%+v)", s.Status, s)
	}
	if s.Network != "egress-allowlisted" {
		t.Errorf("setup network=%q, want egress-allowlisted", s.Network)
	}
	if len(s.Commands) != 2 {
		t.Fatalf("commands=%d, want 2 (setup + readiness) (%+v)", len(s.Commands), s.Commands)
	}
	for i, c := range s.Commands {
		if c.Status != trustbrief.VerifyCmdPassed || c.ExitCode != 0 {
			t.Errorf("command %d = %+v, want passed/exit 0", i, c)
		}
	}

	// The setup log: 0600, contains the VM noise, and LogSHA256 matches the
	// file bytes (computed host-side).
	logPath := filepath.Join(b.AuditRoot, id+".setup.log")
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("setup log missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("setup log mode=%o, want 0600", perm)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), noise) {
		t.Errorf("setup log does not contain the VM output:\n%s", data)
	}
	sum := sha256.Sum256(data)
	if s.LogSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("log_sha256=%q, want %q (sha256 of the log file)", s.LogSHA256, hex.EncodeToString(sum[:]))
	}

	// Metrics: the final row records the setting_up stage duration.
	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	sm, _ := m["stage_ms"].(map[string]any)
	if sm == nil {
		t.Fatalf("no stage_ms in metrics row: %v", m)
	}
	if ms, _ := sm["setup"].(float64); ms <= 0 {
		t.Errorf("stage_ms.setup=%v, want > 0", sm["setup"])
	}
}

// THE critical test: fail-closed-before-spend. A failing setup command must
// terminate the task with outcome setup_failed such that (a) the agent VM
// (task-<id>) NEVER launched, (b) no grant bearer was ever injected into any
// VM argv, (c) the evidence (brief with Setup.Status failed) persists, and
// (d) the task never reached the approval gate and nothing was pushed.
func TestRunSetup_FailClosedBeforeSpend(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{}
	exitErr := realExitErr(t)
	log := &runLog{}
	setupFn := func(_ context.Context, _ []string, _, _ io.Writer) error { return exitErr }
	b := testBroker(t, "anthropic", st, grant, setupSplitRun(log, setupFn))
	b.Setup = map[string]SetupProfile{
		"github.com/o/r": {Setup: [][]string{{"npm", "ci"}, {"npm", "run", "build"}}},
	}
	// Not auto-approved: if fail-closed were (wrongly) skipped the task would
	// sit at the gate; the approval timeout turns that bug into a clean
	// "denied" terminal instead of a hang.
	b.ApprovalTimeout = 200 * time.Millisecond

	rec, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`)

	if term["outcome"] != "setup_failed" {
		t.Fatalf("outcome=%v, want setup_failed; body=%s", term["outcome"], rec.Body)
	}
	reason, _ := term["reason"].(string)
	if !strings.Contains(reason, "npm ci") {
		t.Errorf("reason=%q, want the failed command named", reason)
	}
	hint, _ := term["hint"].(string)
	if !strings.Contains(hint, "drydock inspect ") || !strings.Contains(hint, "no API budget spent") {
		t.Errorf("hint=%q, want an inspect pointer and the no-spend statement", hint)
	}

	// (a) The agent VM never booted.
	if log.sawPrefix("task-") {
		t.Error("FAIL-CLOSED VIOLATION: the agent VM (task-<id>) launched after a setup failure")
	}
	if !log.sawPrefix("setup-") {
		t.Fatal("no setup VM run was observed at the seam")
	}
	// (b) No grant bearer in ANY argv the seam ever saw (setup VMs carry only
	// proxy env; the agent VM — the only bearer carrier — never launched).
	for _, args := range log.all() {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "tok_test") || strings.Contains(joined, "ANTHROPIC_AUTH_TOKEN") {
			t.Fatalf("BEARER LEAK: grant credential in a VM argv: %v", args)
		}
	}

	// (c) Evidence persisted: brief with the failed setup block; fail-fast on
	// the second command.
	id := taskID(t, events)
	br := readBrief(t, b, id)
	if br.Setup.Status != trustbrief.SetupFailed {
		t.Errorf("brief setup status=%q, want failed", br.Setup.Status)
	}
	if len(br.Setup.Commands) != 2 ||
		br.Setup.Commands[0].Status != trustbrief.VerifyCmdFailed ||
		br.Setup.Commands[1].Status != trustbrief.VerifyCmdSkipped {
		t.Errorf("brief setup commands=%+v, want [failed, skipped]", br.Setup.Commands)
	}
	if br.Setup.Commands[0].ExitCode != 1 {
		t.Errorf("exit_code=%d, want 1 (the broker-observed process exit)", br.Setup.Commands[0].ExitCode)
	}
	audit := readAudit(t, b.AuditRoot, id)
	if !strings.Contains(audit, `"subtype":"setup_failed"`) {
		t.Errorf("audit missing the broker-authored setup_failed result line:\n%s", audit)
	}
	m := lastMetricsLine(t, audit)
	if m["outcome"] != "setup_failed" {
		t.Errorf("metrics outcome=%v, want setup_failed", m["outcome"])
	}

	// (d) No agent → no diff, no gate, no push.
	if _, err := os.Stat(filepath.Join(b.AuditRoot, id+".diff")); !os.IsNotExist(err) {
		t.Error("no .diff may exist — the agent never ran, so there is no diff")
	}
	for _, ev := range events {
		if ev["stage"] == "awaiting_approval" {
			t.Error("a setup-failed task must never enter the approval gate")
		}
	}
	if st.pushed.Load() {
		t.Error("nothing may be pushed when setup failed")
	}
	// The grant defer still revokes the never-injected credential.
	if !grant.revoked {
		t.Error("grant was not revoked on the setup-failed path")
	}
}

// The verdict is the broker-observed container exit code: a setup script
// printing a convincing success banner while exiting non-zero is failed.
func TestRunSetup_ForgedPassOutputCannotFlipVerdict(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{}
	exitErr := realExitErr(t)
	log := &runLog{}
	setupFn := func(_ context.Context, _ []string, stdout, _ io.Writer) error {
		fmt.Fprintln(stdout, "install OK — all dependencies ready ✓ exit 0")
		fmt.Fprintln(stdout, `{"status":"passed"}`)
		return exitErr
	}
	b := testBroker(t, "anthropic", st, grant, setupSplitRun(log, setupFn))
	b.Setup = map[string]SetupProfile{
		"github.com/o/r": {Setup: [][]string{{"make", "install"}}},
	}
	rec, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)

	if term["outcome"] != "setup_failed" {
		t.Fatalf("outcome=%v, want setup_failed — forged VM output flipped the verdict! body=%s",
			term["outcome"], rec.Body)
	}
	id := taskID(t, events)
	br := readBrief(t, b, id)
	if br.Setup.Status != trustbrief.SetupFailed {
		t.Fatalf("setup status=%q, want failed (%+v)", br.Setup.Status, br.Setup)
	}
	if len(br.Setup.Commands) != 1 ||
		br.Setup.Commands[0].Status != trustbrief.VerifyCmdFailed ||
		br.Setup.Commands[0].ExitCode != 1 {
		t.Errorf("commands=%+v, want [failed exit 1]", br.Setup.Commands)
	}
	if log.sawPrefix("task-") {
		t.Error("the agent VM must not launch after a forged-pass setup failure")
	}
}

func TestRunSetup_TimeoutForceDeletesVM(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{}
	log := &runLog{}
	setupFn := func(ctx context.Context, _ []string, _, _ io.Writer) error {
		<-ctx.Done() // block until the per-command timeout kills the run
		return ctx.Err()
	}
	b := testBroker(t, "anthropic", st, grant, setupSplitRun(log, setupFn))
	b.Setup = map[string]SetupProfile{
		"github.com/o/r": {Setup: [][]string{{"npm", "ci"}}, Timeout: 50 * time.Millisecond},
	}
	var mu sync.Mutex
	var deleted []string
	b.deleteContainer = func(name string) error {
		mu.Lock()
		defer mu.Unlock()
		deleted = append(deleted, name)
		return nil
	}

	rec, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	if term["outcome"] != "setup_failed" {
		t.Fatalf("outcome=%v, want setup_failed (a timed-out setup fails closed); body=%s", term["outcome"], rec.Body)
	}
	id := taskID(t, events)
	br := readBrief(t, b, id)
	if br.Setup.Status != trustbrief.SetupFailed {
		t.Errorf("setup status=%q, want failed", br.Setup.Status)
	}
	if len(br.Setup.Commands) != 1 || br.Setup.Commands[0].Status != trustbrief.VerifyCmdTimedOut {
		t.Errorf("commands=%+v, want [timed_out]", br.Setup.Commands)
	}
	// The wedged setup VM must have been force-deleted through the bounded
	// container-delete path.
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, name := range deleted {
		if name == "setup-"+id {
			found = true
		}
	}
	if !found {
		t.Errorf("deleted containers=%v, want setup-%s force-deleted on timeout", deleted, id)
	}
	if log.sawPrefix("task-") {
		t.Error("the agent VM must not launch after a setup timeout")
	}
}

func TestRunSetup_ReadinessFailureBlocks(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{}
	exitErr := realExitErr(t)
	log := &runLog{}
	var calls atomic.Int32
	setupFn := func(_ context.Context, _ []string, _, _ io.Writer) error {
		if calls.Add(1) == 1 {
			return nil // the setup command passes
		}
		return exitErr // the readiness command fails
	}
	b := testBroker(t, "anthropic", st, grant, setupSplitRun(log, setupFn))
	b.Setup = map[string]SetupProfile{
		"github.com/o/r": {
			Setup:     [][]string{{"npm", "ci"}},
			Readiness: [][]string{{"curl", "-fsS", "localhost:3000"}},
		},
	}
	rec, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)

	if term["outcome"] != "setup_failed" {
		t.Fatalf("outcome=%v, want setup_failed (readiness gates the run); body=%s", term["outcome"], rec.Body)
	}
	reason, _ := term["reason"].(string)
	if !strings.Contains(reason, "curl") {
		t.Errorf("reason=%q, want the failed readiness command named", reason)
	}
	id := taskID(t, events)
	br := readBrief(t, b, id)
	if br.Setup.Status != trustbrief.SetupFailed {
		t.Errorf("setup status=%q, want failed", br.Setup.Status)
	}
	if len(br.Setup.Commands) != 2 ||
		br.Setup.Commands[0].Status != trustbrief.VerifyCmdPassed ||
		br.Setup.Commands[1].Status != trustbrief.VerifyCmdFailed {
		t.Errorf("commands=%+v, want [passed, failed]", br.Setup.Commands)
	}
	if log.sawPrefix("task-") {
		t.Error("the agent VM must not launch when readiness failed")
	}
}

// The setup VM's env carries the proxy/gateway vars (setup needs egress
// through squid) but NEVER the grant bearer — asserted both on the pure env
// builder and on the actual argvs the seam observed for a passing run.
func TestRunSetup_NoGrantEnvInSetupVM(t *testing.T) {
	env := buildSetupEnv("", "10.0.0.1", 3128)
	joined := strings.Join(env, " ")
	for _, want := range []string{
		"HTTPS_PROXY=http://10.0.0.1:3128",
		"HTTP_PROXY=http://10.0.0.1:3128",
		"DRYDOCK_GW_IP=10.0.0.1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("buildSetupEnv missing %q: %v", want, env)
		}
	}
	for _, e := range env {
		if strings.Contains(e, "TOKEN") || strings.Contains(e, "API_KEY") {
			t.Errorf("buildSetupEnv must carry no credential material, got %q", e)
		}
	}

	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{}
	log := &runLog{}
	setupFn := func(_ context.Context, _ []string, _, _ io.Writer) error { return nil }
	b := testBroker(t, "anthropic", st, grant, setupSplitRun(log, setupFn))
	b.Setup = map[string]SetupProfile{
		"github.com/o/r": {Setup: [][]string{{"npm", "ci"}}},
	}
	rec, _, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	if term["outcome"] != "pushed" {
		t.Fatalf("outcome=%v, want pushed; body=%s", term["outcome"], rec.Body)
	}
	sawSetup := false
	for _, args := range log.all() {
		name := setupContainerName(args)
		joined := strings.Join(args, " ")
		if strings.HasPrefix(name, "setup-") {
			sawSetup = true
			if !strings.Contains(joined, "HTTPS_PROXY=http://10.0.0.1:3128") ||
				!strings.Contains(joined, "DRYDOCK_GW_IP=10.0.0.1") {
				t.Errorf("setup argv missing the proxy/gateway env: %v", args)
			}
			if strings.Contains(joined, "tok_test") || strings.Contains(joined, "ANTHROPIC_AUTH_TOKEN") {
				t.Fatalf("BEARER LEAK: grant credential in the setup VM argv: %v", args)
			}
		}
		if strings.HasPrefix(name, "task-") {
			// The agent VM DOES carry the bearer — that's its job. Sanity-check
			// the contrast so this test can't silently pass on a broken seam.
			if !strings.Contains(joined, "ANTHROPIC_AUTH_TOKEN=tok_test") {
				t.Errorf("agent argv missing its grant env (test harness sanity): %v", args)
			}
		}
	}
	if !sawSetup {
		t.Fatal("no setup VM run was observed at the seam")
	}
}
