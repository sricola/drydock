# Resume Awaiting-Approval Across Restart Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A brokerd restart no longer destroys an awaiting-approval task or shows it a false "ok": a durable gate marker + preserved stage dir lets `drydock approve <id>` after a restart push the surviving branch, with an honest `interrupted` fallback.

**Architecture:** A gate marker file records an awaiting-approval push; boot reconciliation reopens the surviving stage and re-registers the task as pending, resuming the gate+push tail headlessly. The per-task context carries a cancel cause so shutdown (resume later) is distinguished from kill (drop). The stage reap skips marked dirs.

**Tech Stack:** Go, standard library only. Packages `internal/stage`, `internal/broker`, `internal/audit`, `cmd/brokerd`.

## Global Constraints

- Go standard library only; no new dependencies.
- TDD: test-first. `go test -race -count=1 ./...`.
- No em dashes (—) in code, comments, docs, or commit messages; use commas, colons, or parentheses.
- gofmt + staticcheck clean.
- Scope: the PUSH (diff-approval) gate only. Resume needs no model re-mint (push uses git remote auth via `PushEnv`).
- Never leave a false `ok` for a task that did not push: every restart resolution writes an honest terminal audit line.

---

## File Structure

- `internal/stage/stage.go` (modify): `Reopen`, `ReapOrphans` keep-set.
- `internal/stage/stage_test.go` (modify): their tests.
- `internal/broker/gatemarker.go` (new): `gateMarker` struct + write/read/list/remove helpers.
- `internal/broker/gatemarker_test.go` (new): round-trip tests.
- `internal/broker/broker.go` (modify): `cancellers` type, `context.WithCancelCause`, `errTaskKilled`/`errShutdown`, `finishPush` extraction, `CancelAll` cause.
- `internal/broker/admin.go` (modify): `HandleKill` cancels with `errTaskKilled`.
- `internal/broker/gates.go` (modify): `awaitGate` returns `(bool, gateCause)`; `gatePush` marker lifecycle.
- `internal/broker/reconcile.go` (modify): `ResumeAwaiting`, `resumePush`.
- `internal/broker/stream.go` (modify): `newDiscardStream`.
- `cmd/brokerd/main.go` (modify): `pruneOrphanTasks` keep-set; `b.ResumeAwaiting` before serve; `CancelAll` cause path already in broker.
- `internal/audit/audit.go` (modify): `Outcome` `denied`/`pushed` cases.
- `site/docs/daemon.md`, `CHANGELOG.md`, `docs/ROADMAP.md` (modify): docs.

---

### Task 1: `stage.Reopen` + `ReapOrphans` keep-set

**Files:**
- Modify: `internal/stage/stage.go`
- Test: `internal/stage/stage_test.go`

**Interfaces:**
- Produces: `Reopen(root string) (*Stage, error)`; `ReapOrphans(root string, keep map[string]bool) (int, error)` (existing callers pass `nil`).

- [ ] **Step 1: Write the failing test**

Append to `internal/stage/stage_test.go`:

```go
func TestReopen_RecoversAPreparedStage(t *testing.T) {
	origin, s := setupPushable(t) // bare origin + a Stage with an uncommitted change
	root := s.Root

	re, err := Reopen(root)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	// The reopened stage can commit the surviving work-tree change and push it.
	if err := re.Commit("agent/resumed", "agent: resumed"); err != nil {
		t.Fatalf("Commit on reopened stage: %v", err)
	}
	if err := re.PushBranch("agent/resumed", "agent/resumed"); err != nil {
		t.Fatalf("PushBranch on reopened stage: %v", err)
	}
	out, _ := exec.Command("git", "--git-dir="+origin, "branch", "--list", "agent/resumed").CombinedOutput()
	if !strings.Contains(string(out), "agent/resumed") {
		t.Errorf("reopened stage did not push; origin branches: %s", out)
	}
}

func TestReopen_ErrorsWhenGitDirMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := Reopen(dir); err == nil {
		t.Error("Reopen of a dir with no git dir should error")
	}
}

func TestReapOrphans_SkipsKeepSet(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"keepme", "reapme"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	n, err := ReapOrphans(root, map[string]bool{"keepme": true})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("reaped %d, want 1 (reapme only)", n)
	}
	if _, err := os.Stat(filepath.Join(root, "keepme")); err != nil {
		t.Error("keepme should have survived the reap")
	}
	if _, err := os.Stat(filepath.Join(root, "reapme")); !os.IsNotExist(err) {
		t.Error("reapme should have been removed")
	}
}
```

Note: `setupPushable` was added in the 4.2 work and returns `(originGitDir string, s *stage.Stage)` where `s.Root` is the stage root and the work tree has an uncommitted change. If its return shape differs, adapt; the point is a prepared stage whose `Root` you can `Reopen`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/stage/ -run 'TestReopen|TestReapOrphans_SkipsKeepSet'`
Expected: FAIL, `undefined: Reopen` and a signature error on `ReapOrphans`.

- [ ] **Step 3a: Implement `Reopen`**

`Prepare` builds `workDir := filepath.Join(root, "work")` and `gitDir := filepath.Join(root, "git")` (verify the exact subdir names in `Prepare` and reuse them EXACTLY). Add:

```go
// Reopen reconstructs a Stage from an existing root left by a prior Prepare
// (e.g. an awaiting-approval task that survived a brokerd restart), without
// cloning. It errors if the work tree or the host-only git dir is missing.
func Reopen(root string) (*Stage, error) {
	workDir := filepath.Join(root, "work")
	gitDir := filepath.Join(root, "git")
	for _, d := range []string{workDir, gitDir} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("stage: cannot reopen %q: missing %q", root, d)
		}
	}
	return &Stage{Root: root, WorkDir: workDir, gitDir: gitDir}, nil
}
```

Adjust the `"work"`/`"git"` literals to match `Prepare`'s actual subdir names.

- [ ] **Step 3b: Add the keep-set to `ReapOrphans`**

Change the signature and skip loop:

```go
func ReapOrphans(root string, keep map[string]bool) (int, error) {
	// ... existing unsafe-root guard + ReadDir ...
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if keep[e.Name()] {
			continue // awaiting-approval stage: preserved for resume
		}
		// ... existing RemoveAll + count ...
	}
	// ...
}
```

Update the existing caller in `cmd/brokerd/main.go` `pruneOrphanTasks` to pass `nil` for now (Task 7 replaces it): `stage.ReapOrphans(stageRoot, nil)`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/stage/ ./cmd/brokerd/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/stage/stage.go internal/stage/stage_test.go cmd/brokerd/main.go
git commit -m "feat(stage): Reopen + ReapOrphans keep-set for resume"
```

---

### Task 2: Gate marker helpers

**Files:**
- Create: `internal/broker/gatemarker.go`
- Test: `internal/broker/gatemarker_test.go`

**Interfaces:**
- Produces: `type gateMarker struct { RepoRef, Instruction, Platform, Agent string; Draft bool; TaskStartMs int64 }`; `writeGateMarker(auditRoot, id string, m gateMarker) error`; `readGateMarker(auditRoot, id string) (gateMarker, error)`; `ListGateMarkers(auditRoot string) map[string]gateMarker`; `removeGateMarker(auditRoot, id string) error`.

- [ ] **Step 1: Write the failing test**

Create `internal/broker/gatemarker_test.go`:

```go
package broker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGateMarker_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := gateMarker{RepoRef: "https://github.com/o/r", Instruction: "do x",
		Platform: "github", Agent: "claude", Draft: true, TaskStartMs: 42}
	if err := writeGateMarker(dir, "abc", m); err != nil {
		t.Fatal(err)
	}
	got, err := readGateMarker(dir, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if got != m {
		t.Errorf("round-trip = %+v, want %+v", got, m)
	}

	// Only the marker file, plus an unrelated file, in the dir.
	os.WriteFile(filepath.Join(dir, "abc.diff"), []byte("d"), 0o600)
	all := ListGateMarkers(dir)
	if len(all) != 1 {
		t.Fatalf("ListGateMarkers = %d, want 1", len(all))
	}
	if _, ok := all["abc"]; !ok {
		t.Errorf("marker id abc missing from %v", all)
	}

	if err := removeGateMarker(dir, "abc"); err != nil {
		t.Fatal(err)
	}
	if len(ListGateMarkers(dir)) != 0 {
		t.Error("marker not removed")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/broker/ -run TestGateMarker_RoundTrip`
Expected: FAIL, `undefined: gateMarker`.

- [ ] **Step 3: Implement**

Create `internal/broker/gatemarker.go`:

```go
package broker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// gateMarker records a task blocked at the push-approval gate so a brokerd
// restart can resume it. Written when the gate is entered, removed when it
// resolves (approve/deny/kill/timeout), left in place on shutdown.
type gateMarker struct {
	RepoRef     string `json:"repo_ref"`
	Instruction string `json:"instruction"`
	Platform    string `json:"platform"`
	Agent       string `json:"agent"`
	Draft       bool   `json:"draft"`
	TaskStartMs int64  `json:"task_start_ms"`
}

func gateMarkerPath(auditRoot, id string) string {
	return filepath.Join(auditRoot, id+".gate.json")
}

func writeGateMarker(auditRoot, id string, m gateMarker) error {
	if err := os.MkdirAll(auditRoot, 0o700); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(gateMarkerPath(auditRoot, id), payload, 0o600)
}

func readGateMarker(auditRoot, id string) (gateMarker, error) {
	var m gateMarker
	data, err := os.ReadFile(gateMarkerPath(auditRoot, id))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(data, &m)
	return m, err
}

func removeGateMarker(auditRoot, id string) error {
	err := os.Remove(gateMarkerPath(auditRoot, id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ListGateMarkers returns every task id with a live gate marker in auditRoot.
// Unreadable or malformed markers are skipped (logged by the caller if needed).
func ListGateMarkers(auditRoot string) map[string]gateMarker {
	out := map[string]gateMarker{}
	entries, err := os.ReadDir(auditRoot)
	if err != nil {
		return out
	}
	const suffix = ".gate.json"
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), suffix)
		if m, err := readGateMarker(auditRoot, id); err == nil {
			out[id] = m
		}
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/broker/ -run TestGateMarker_RoundTrip`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/broker/gatemarker.go internal/broker/gatemarker_test.go
git commit -m "feat(broker): gate marker for awaiting-approval resume"
```

---

### Task 3: Context cancel cause + `awaitGate` cause

**Files:**
- Modify: `internal/broker/broker.go`, `internal/broker/admin.go`, `internal/broker/gates.go`
- Test: `internal/broker/gates_test.go` (or the file that tests gates)

**Interfaces:**
- Produces: package errors `errTaskKilled`, `errShutdown`; `Broker.cancellers` becomes `map[string]context.CancelCauseFunc`; `awaitGate(...)` returns `(bool, gateCause)`; `type gateCause int` with `gateApproved, gateDenied, gateKilled, gateTimeout, gateShutdown`.

- [ ] **Step 1: Write the failing test**

Create `internal/broker/gatecause_test.go`:

```go
package broker

import (
	"context"
	"testing"
)

func TestAwaitGate_CauseFromContext(t *testing.T) {
	b := &Broker{}
	// Shutdown cause -> gateShutdown.
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errShutdown)
	if ok, cause := b.awaitGate(ctx, "t1", "to", "cx", func() {}); ok || cause != gateShutdown {
		t.Errorf("shutdown: ok=%v cause=%v, want false/gateShutdown", ok, cause)
	}
	// Kill cause -> gateKilled.
	ctx2, cancel2 := context.WithCancelCause(context.Background())
	cancel2(errTaskKilled)
	if ok, cause := b.awaitGate(ctx2, "t2", "to", "cx", func() {}); ok || cause != gateKilled {
		t.Errorf("kill: ok=%v cause=%v, want false/gateKilled", ok, cause)
	}
}

func TestAwaitGate_ApproveDeny(t *testing.T) {
	b := &Broker{}
	go func() {
		// wait until pending is registered, then approve
		for {
			b.pendingMu.Lock()
			ch := b.pending["t3"]
			b.pendingMu.Unlock()
			if ch != nil {
				ch <- true
				return
			}
		}
	}()
	if ok, cause := b.awaitGate(context.Background(), "t3", "to", "cx", func() {}); !ok || cause != gateApproved {
		t.Errorf("approve: ok=%v cause=%v, want true/gateApproved", ok, cause)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/broker/ -run TestAwaitGate`
Expected: FAIL, `undefined: errShutdown` / `awaitGate` returns one value.

- [ ] **Step 3a: Errors + cause type (gates.go)**

Add near the top of `gates.go`:

```go
import "errors"

var (
	errTaskKilled = errors.New("task killed")
	errShutdown   = errors.New("brokerd shutting down")
)

type gateCause int

const (
	gateApproved gateCause = iota
	gateDenied
	gateKilled
	gateTimeout
	gateShutdown
)
```

- [ ] **Step 3b: `awaitGate` returns the cause**

Rewrite `awaitGate`'s return and select:

```go
func (b *Broker) awaitGate(ctx context.Context, taskID, timeoutMsg, cancelMsg string, onReady func()) (bool, gateCause) {
	// ... existing ApprovalTimeout, ch registration, defer delete, onReady() ...
	select {
	case ok := <-ch:
		if ok {
			return true, gateApproved
		}
		return false, gateDenied
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			slog.Warn(timeoutMsg, "task_id", taskID, "timeout", b.ApprovalTimeout)
			return false, gateTimeout
		}
		switch context.Cause(ctx) {
		case errShutdown:
			slog.Info("task gate interrupted by shutdown", "task_id", taskID)
			return false, gateShutdown
		case errTaskKilled:
			slog.Info(cancelMsg, "task_id", taskID)
			return false, gateKilled
		default:
			slog.Info(cancelMsg, "task_id", taskID)
			return false, gateKilled
		}
	}
}
```

Update `gateEgressWiden` and `gatePush` call sites: `gateEgressWiden` returns `ok` only (`ok, _ := b.awaitGate(...); return ok`). `gatePush` is rewritten in Task 4; for now make it compile with `ok, _ := b.awaitGate(...); return ok`.

- [ ] **Step 3c: Cancel cause plumbing (broker.go + admin.go)**

In `broker.go`: change `cancellers map[string]context.CancelFunc` to `map[string]context.CancelCauseFunc`; `registerTask`'s cancel param type to `context.CancelCauseFunc`; in `HandleTask` use `taskCtx, cancel := context.WithCancelCause(context.Background())` and `defer cancel(nil)`. In `CancelAll`, call `c(errShutdown)` (the loop var is now a `CancelCauseFunc`). In `admin.go` `HandleKill`, call `cancel(errTaskKilled)`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/broker/ ./cmd/brokerd/`
Expected: PASS (existing gate/kill tests still pass; new cause test passes). Run `go build ./...`.

- [ ] **Step 5: Commit**

```bash
git add internal/broker/gates.go internal/broker/broker.go internal/broker/admin.go internal/broker/gatecause_test.go
git commit -m "feat(broker): gate cancel cause (shutdown vs kill) + awaitGate cause"
```

---

### Task 4: gatePush marker lifecycle

**Files:**
- Modify: `internal/broker/gates.go`
- Test: `internal/broker/gates_test.go` (append)

**Interfaces:**
- Consumes: gate marker helpers (Task 2), `gateCause` (Task 3). The gatePush signature stays `gatePush(ctx, taskID, diff, auto) bool` but internally writes/removes the marker and needs the marker fields, so it also reads them from a new `taskRun`-provided source. To keep it callable from the existing `pushAndOpenPR`, add a variant `gatePushMarked(ctx context.Context, tr *taskRun, diff string) (bool, gateCause)` that writes the marker from `tr` and returns the cause; `pushAndOpenPR` calls it.

- [ ] **Step 1: Write the failing test**

Append to a broker test file:

```go
func TestGatePush_MarkerLifecycle(t *testing.T) {
	dir := t.TempDir()
	b := &Broker{AuditRoot: dir}
	tr := &taskRun{b: b, id: "gp1", repoRef: "https://github.com/o/r",
		instruction: "x", platform: "github", agentName: "claude"}

	// Approve path: marker exists during the block, removed after.
	go func() {
		for {
			b.pendingMu.Lock()
			ch := b.pending["gp1"]
			b.pendingMu.Unlock()
			if ch != nil {
				if _, err := os.Stat(gateMarkerPath(dir, "gp1")); err != nil {
					t.Errorf("marker should exist while awaiting: %v", err)
				}
				ch <- true
				return
			}
		}
	}()
	ok, _ := b.gatePushMarked(context.Background(), tr, "diff x")
	if !ok {
		t.Fatal("expected approval")
	}
	if _, err := os.Stat(gateMarkerPath(dir, "gp1")); !os.IsNotExist(err) {
		t.Error("marker should be removed after approval")
	}
}

func TestGatePush_ShutdownLeavesMarker(t *testing.T) {
	dir := t.TempDir()
	b := &Broker{AuditRoot: dir}
	tr := &taskRun{b: b, id: "gp2", repoRef: "r", instruction: "x", platform: "github", agentName: "claude"}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errShutdown)
	ok, cause := b.gatePushMarked(ctx, tr, "diff")
	if ok || cause != gateShutdown {
		t.Fatalf("ok=%v cause=%v, want false/gateShutdown", ok, cause)
	}
	if _, err := os.Stat(gateMarkerPath(dir, "gp2")); err != nil {
		t.Error("marker must be LEFT on shutdown for boot resume")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/broker/ -run TestGatePush_`
Expected: FAIL, `undefined: gatePushMarked`.

- [ ] **Step 3: Implement**

In `gates.go`, add `gatePushMarked` and route `pushAndOpenPR` through it. `gatePush` (the auto/plain form) stays for tests that call it directly:

```go
// gatePushMarked is gatePush with a durable resume marker. It persists the diff
// and a gate marker, blocks for approval, then removes the marker UNLESS the
// gate was interrupted by shutdown (left for boot resume).
func (b *Broker) gatePushMarked(ctx context.Context, tr *taskRun, diff string) (bool, gateCause) {
	ok, cause := b.awaitGate(ctx, tr.id,
		"task auto-denied at approval gate (approval_timeout reached)",
		"task killed or broker shutting down before approval; aborting",
		func() {
			diffPath := filepath.Join(b.AuditRoot, tr.id+".diff")
			if werr := os.WriteFile(diffPath, []byte(diff), 0o600); werr != nil {
				slog.Warn("could not persist diff for review", "task_id", tr.id, "err", werr)
			}
			if werr := writeGateMarker(b.AuditRoot, tr.id, gateMarker{
				RepoRef: tr.repoRef, Instruction: tr.instruction, Platform: tr.platform,
				Agent: tr.agentName, Draft: tr.draft,
				TaskStartMs: tr.taskStart.UnixMilli(),
			}); werr != nil {
				slog.Warn("could not persist gate marker", "task_id", tr.id, "err", werr)
			}
			slog.Info("task awaiting approval",
				"task_id", tr.id, "diff_bytes", len(diff), "diff_path", diffPath,
				"hint", "drydock approve "+tr.id+" | drydock deny "+tr.id)
			b.notifyMac("drydock: task awaiting approval",
				fmt.Sprintf("task %s (%d byte diff): drydock approve %s", tr.id, len(diff), tr.id))
		})
	if cause != gateShutdown {
		_ = removeGateMarker(b.AuditRoot, tr.id)
	}
	return ok, cause
}
```

In `pushAndOpenPR`, replace `if !b.gatePush(tr.ctx, tr.id, diff, tr.autoApprove) {` with:

```go
	approved := tr.autoApprove
	cause := gateApproved
	if !tr.autoApprove {
		approved, cause = b.gatePushMarked(tr.ctx, tr, diff)
	}
	if !approved {
		outcome := "denied"
		if cause == gateKilled || cause == gateShutdown {
			outcome = "cancelled"
		}
		tr.sw.emit(map[string]any{"event": "result", "outcome": outcome,
			"task_id": tr.id, "diff_bytes": len(diff)})
		return
	}
```

(Keep the auto-approve slog line by moving it into the `tr.autoApprove` branch, or leave the existing `gatePush` auto handling if `pushAndOpenPR` still special-cases auto.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/broker/`
Expected: PASS (new lifecycle tests + existing push/gate tests).

- [ ] **Step 5: Commit**

```bash
git add internal/broker/gates.go internal/broker/*_test.go
git commit -m "feat(broker): gatePush writes/removes the resume marker"
```

---

### Task 5: Extract `finishPush` from `pushAndOpenPR`

**Files:**
- Modify: `internal/broker/broker.go`
- Test: existing broker push tests must still pass (no new test needed; this is a behavior-preserving extraction verified by the suite).

**Interfaces:**
- Produces: `(tr *taskRun) finishPush(diff string, files, insertions, deletions int)` containing everything `pushAndOpenPR` does AFTER the gate returns approved (the `pushWithRecovery` call, the failure emit + synthetic audit line, and the success PR-open + `pushed` event). `pushAndOpenPR` calls `tr.finishPush(...)` after a successful gate.

- [ ] **Step 1: Confirm the covering tests**

The extraction is behavior-preserving, so the existing tests are the check: `TestHandleTask_PushFailed_TerminalOutcome`, `TestHandleTask_PROpenFailure_StillPushed`, and any `outcome=="pushed"` test. Run them first to record green: `go test ./internal/broker/ -run 'TestHandleTask_Push|TestHandleTask_PROpen'`.

- [ ] **Step 2: Extract**

Move the post-gate body of `pushAndOpenPR` (from `b.setStage(tr.id, StagePushing)` through the final `tr.sw.emit(ev)`) into:

```go
// finishPush performs the branch push (with recovery) and PR-open after the
// approval gate has passed, emitting the terminal pushed/push_failed event and,
// on failure, the synthetic audit line. Shared by the live path and resume.
func (tr *taskRun) finishPush(diff string, files, insertions, deletions int) {
	b := tr.b
	b.setStage(tr.id, StagePushing)
	// ... the moved body (adapter, pushWithRecovery, failure + success paths) ...
}
```

`pushAndOpenPR` becomes: compute diffStat, gate (Task 4 code), then on approval `tr.finishPush(diff, files, insertions, deletions)`.

- [ ] **Step 3: Run tests to verify they pass**

Run: `go test -race ./internal/broker/`
Expected: PASS (identical behavior; the suite is the proof). `go build ./...`.

- [ ] **Step 4: Commit**

```bash
git add internal/broker/broker.go
git commit -m "refactor(broker): extract finishPush for resume reuse"
```

---

### Task 6: `resumePush` + `ResumeAwaiting`

**Files:**
- Modify: `internal/broker/reconcile.go`, `internal/broker/stream.go`
- Test: `internal/broker/reconcile_test.go` (append)

**Interfaces:**
- Consumes: `stage.Reopen` (Task 1), gate markers (Task 2), `gatePushMarked`/`gateCause` (Task 4), `finishPush` (Task 5), `interruptedResultLine`/`appendLine` (existing).
- Produces: `newDiscardStream() *stream`; `(b *Broker) ResumeAwaiting(stageRoot string)`; `(b *Broker) resumePush(id string, m gateMarker, st taskStage, diff string, logf io.Writer)`.

- [ ] **Step 1: Write the failing test**

Append to `internal/broker/reconcile_test.go`:

```go
func TestResumeAwaiting_StageGone_WritesInterrupted(t *testing.T) {
	dir := t.TempDir()
	// A task that finished the agent (ok) then hit the gate, whose stage did NOT survive.
	audit := filepath.Join(dir, "gone.jsonl")
	os.WriteFile(audit, []byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":1}`+"\n"), 0o600)
	writeGateMarker(dir, "gone", gateMarker{RepoRef: "r", Agent: "claude"})

	b := &Broker{AuditRoot: dir}
	b.ResumeAwaiting(t.TempDir()) // stageRoot has no "gone" dir

	data, _ := os.ReadFile(audit)
	if !strings.Contains(string(data), `"subtype":"interrupted"`) {
		t.Errorf("stage-gone task should get an interrupted line, got:\n%s", data)
	}
	if _, err := os.Stat(gateMarkerPath(dir, "gone")); !os.IsNotExist(err) {
		t.Error("marker should be removed after the interrupted fallback")
	}
}

func TestResumeAwaiting_StageSurvives_ApprovePushes(t *testing.T) {
	dir := t.TempDir()
	stageRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stageRoot, "live", "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(stageRoot, "live", "git"), 0o700)
	os.WriteFile(filepath.Join(dir, "live.diff"), []byte("diff"), 0o600)
	writeGateMarker(dir, "live", gateMarker{RepoRef: "https://github.com/o/r", Agent: "claude", Platform: "github"})

	fs := &fakeStage{workDir: filepath.Join(stageRoot, "live", "work")}
	b := &Broker{AuditRoot: dir,
		reopenStage: func(root string) (taskStage, error) { return fs, nil }, // test seam
		newAdapter:  func(string, string) remote.Adapter { return &fakeAdapter{name: "github"} }}
	b.ResumeAwaiting(stageRoot)

	// The task is now pending; approve it and assert the surviving branch pushed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b.pendingMu.Lock()
		ch := b.pending["live"]
		b.pendingMu.Unlock()
		if ch != nil {
			ch <- true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Give the resume goroutine time to push.
	for i := 0; i < 200 && !fs.pushed; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !fs.pushed {
		t.Error("approve after resume should have pushed the surviving branch")
	}
}
```

Note: this task adds a `reopenStage func(root string) (taskStage, error)` seam on `Broker` (defaulting to a real `stage.Reopen` adapter) so the test can inject a fake, mirroring the existing `prepareStage`/`newAdapter` seams. The `fakeStage` (with `Commit`/`PushBranch` from the 4.2 work) records `pushed`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/broker/ -run TestResumeAwaiting`
Expected: FAIL, `undefined: ResumeAwaiting` / `b.reopenStage`.

- [ ] **Step 3a: `newDiscardStream` (stream.go)**

```go
import "io"

// newDiscardStream is a stream with no client (used by resume after a restart:
// the original submit connection is gone). Events are discarded; the audit line
// is the durable record.
func newDiscardStream() *stream {
	return &stream{enc: json.NewEncoder(io.Discard), f: nil}
}
```

- [ ] **Step 3b: `reopenStage` seam + `ResumeAwaiting` + `resumePush` (broker.go + reconcile.go)**

Add to `Broker`: `reopenStage func(root string) (taskStage, error)`. Add a default adapter used when it is nil:

```go
func defaultReopenStage(root string) (taskStage, error) {
	s, err := stage.Reopen(root)
	if err != nil {
		return nil, err
	}
	return realStage{s: s}, nil
}
```

In `reconcile.go`:

```go
// ResumeAwaiting reconciles awaiting-approval push gates left by a prior
// brokerd life. For each gate marker: if the stage survived, re-register the
// task as pending and resume the gate+push headlessly; otherwise append an
// honest interrupted line and drop the marker (never leave a false ok).
func (b *Broker) ResumeAwaiting(stageRoot string) {
	reopen := b.reopenStage
	if reopen == nil {
		reopen = defaultReopenStage
	}
	for id, m := range ListGateMarkers(b.AuditRoot) {
		auditPath := filepath.Join(b.AuditRoot, id+".jsonl")
		st, err := reopen(filepath.Join(stageRoot, id))
		if err != nil {
			slog.Warn("resume: stage gone, marking interrupted", "task_id", id, "err", err)
			_ = appendLine(auditPath, interruptedResultLine)
			_ = removeGateMarker(b.AuditRoot, id)
			continue
		}
		diff, _ := os.ReadFile(filepath.Join(b.AuditRoot, id+".diff"))
		logf, err := os.OpenFile(auditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			slog.Warn("resume: cannot open audit for append", "task_id", id, "err", err)
			continue
		}
		slog.Info("resuming awaiting-approval task", "task_id", id)
		go b.resumePush(id, m, st, string(diff), logf)
	}
}

// resumePush re-registers a resumed task as pending and drives the gate+push
// tail headlessly (no live client). Approve pushes the surviving branch;
// deny/timeout write the honest terminal outcome. On shutdown the marker is
// left for the next boot.
func (b *Broker) resumePush(id string, m gateMarker, st taskStage, diff string, logf io.Writer) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	b.registerTask(id, m.RepoRef, m.Instruction, cancel)
	defer b.unregisterTask(id)

	tr := &taskRun{
		b: b, ctx: ctx, sw: newDiscardStream(), id: id,
		repoRef: m.RepoRef, instruction: m.Instruction, platform: m.Platform,
		draft: m.Draft, agentName: m.Agent, st: st, logf: logf,
		auditPath: filepath.Join(b.AuditRoot, id+".jsonl"),
		taskStart: time.UnixMilli(m.TaskStartMs),
	}
	if c, ok := st.(interface{ Cleanup() error }); ok {
		defer c.Cleanup()
	}

	ok, cause := b.gatePushMarked(ctx, tr, diff)
	if cause == gateShutdown {
		return // leave the marker; next boot resumes
	}
	files, insertions, deletions := diffStat(diff)
	if !ok {
		subtype := "denied"
		if cause == gateTimeout {
			subtype = "interrupted"
		}
		fmt.Fprintf(logf,
			`{"type":"result","subtype":%q,"is_error":false,"duration_ms":0,"total_cost_usd":%.6f,"num_turns":0}`+"\n",
			subtype, audit.TotalCost(tr.auditPath))
		return
	}
	tr.finishPush(diff, files, insertions, deletions)
}
```

Wire `defer c.Cleanup()` only if the reopened stage exposes Cleanup (realStage does). Ensure `reconcile.go` imports `context`, `io`, `time`, `fmt`, `os`, `path/filepath`, `drydock/internal/audit`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/broker/`
Expected: PASS. `go build ./...`.

- [ ] **Step 5: Commit**

```bash
git add internal/broker/reconcile.go internal/broker/stream.go internal/broker/broker.go internal/broker/reconcile_test.go
git commit -m "feat(broker): ResumeAwaiting + resumePush for restart recovery"
```

---

### Task 7: Boot wiring

**Files:**
- Modify: `cmd/brokerd/main.go`
- Test: `cmd/brokerd/main_test.go` (append a keep-set assertion)

**Interfaces:**
- Consumes: `broker.ListGateMarkers` (Task 2), `stage.ReapOrphans` keep-set (Task 1), `b.ResumeAwaiting` (Task 6).

- [ ] **Step 1: Write the failing test**

Append to `cmd/brokerd/main_test.go`:

```go
func TestPruneOrphanTasks_KeepsGatedStages(t *testing.T) {
	stageRoot := t.TempDir()
	auditRoot := t.TempDir()
	os.MkdirAll(filepath.Join(stageRoot, "gated"), 0o700)
	os.MkdirAll(filepath.Join(stageRoot, "orphan"), 0o700)
	// A live gate marker for "gated".
	os.WriteFile(filepath.Join(auditRoot, "gated.gate.json"), []byte(`{"repo_ref":"r"}`), 0o600)

	pruneOrphanTasks(stageRoot, auditRoot)

	if _, err := os.Stat(filepath.Join(stageRoot, "gated")); err != nil {
		t.Error("gated stage (with a live marker) must survive the reap")
	}
	if _, err := os.Stat(filepath.Join(stageRoot, "orphan")); !os.IsNotExist(err) {
		t.Error("orphan stage must be reaped")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/brokerd/ -run TestPruneOrphanTasks_KeepsGatedStages`
Expected: FAIL (orphan-and-gated both reaped, or a compile error on the keep-set).

- [ ] **Step 3: Implement**

In `pruneOrphanTasks`, build the keep set and pass it:

```go
	keep := map[string]bool{}
	for id := range broker.ListGateMarkers(auditRoot) {
		keep[id] = true
	}
	if n, err := stage.ReapOrphans(stageRoot, keep); err != nil {
		// ... existing warn/info ...
	}
```

After the broker `b` is built and BEFORE the serve loop (`srv.Serve`), add:

```go
	b.ResumeAwaiting(cfg.StageRoot)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./cmd/brokerd/`
Expected: PASS. `go build ./...`.

- [ ] **Step 5: Commit**

```bash
git add cmd/brokerd/main.go cmd/brokerd/main_test.go
git commit -m "feat(brokerd): keep gated stages at boot + ResumeAwaiting"
```

---

### Task 8: Audit `denied` / `pushed` rendering

**Files:**
- Modify: `internal/audit/audit.go`
- Test: `internal/audit/audit_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/audit/audit_test.go`:

```go
func TestOutcome_DeniedAndPushed(t *testing.T) {
	if got := Outcome(Result{Type: "result", Subtype: "denied"}, true, Meta{}); got != "denied" {
		t.Errorf("denied = %q, want denied", got)
	}
	if got := Outcome(Result{Type: "result", Subtype: "pushed"}, true, Meta{}); got != "pushed" {
		t.Errorf("pushed = %q, want pushed", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/audit/ -run TestOutcome_DeniedAndPushed`
Expected: they may already pass via the default case (which returns `r.Subtype`). If so, this test documents the contract; verify by running. If `denied`/`pushed` already render as themselves through the default branch, keep the test as a guard and skip Step 3.

- [ ] **Step 3: Implement (only if Step 2 failed)**

Add explicit cases after the `push_failed` case in `Outcome`:

```go
	case r.Subtype == "denied":
		s = "denied"
	case r.Subtype == "pushed":
		s = "pushed"
```

(If the default already yields these, this task is a no-op guard test; commit the test alone.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/audit/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/audit/audit.go internal/audit/audit_test.go
git commit -m "test(audit): lock denied/pushed outcome rendering"
```

---

### Task 9: Docs + roadmap

**Files:**
- Modify: `site/docs/daemon.md`, `CHANGELOG.md`, `docs/ROADMAP.md`

- [ ] **Step 1: Document the behavior**

In `site/docs/daemon.md`, add a "Restart and awaiting-approval tasks" note: a brokerd restart preserves a task blocked at the approval gate; after restart `drydock approve <id>` pushes the surviving branch (no agent re-run); if the stage did not survive, the task is marked `interrupted` (never a false `ok`) and the diff is preserved for `drydock retry <id>`.

- [ ] **Step 2: CHANGELOG**

Add to `## Unreleased` an Added entry for resume-awaiting-approval-across-restart (gate marker, skip-reap, boot resume, honest interrupted fallback, the false-ok fix).

- [ ] **Step 3: Mark ROADMAP 4.14 landed**

In `docs/ROADMAP.md`, mark 4.14 Landed with a short description, and remove it from the ranked backlog (renumber). No dangling "4.14 pending" reference.

- [ ] **Step 4: Regenerate + verify**

Run: `make docs && go test ./... && grep -rn '—' site/docs/daemon.md CHANGELOG.md docs/ROADMAP.md`
Expected: docs OK, tests PASS, grep empty.

- [ ] **Step 5: Commit**

```bash
git add site/docs/daemon.md CHANGELOG.md docs/ROADMAP.md
git commit -m "docs: resume awaiting-approval across restart (ROADMAP 4.14 landed)"
```

---

## Notes for the implementer

- Verify `Prepare`'s exact work/git subdir names and reuse them in `Reopen` (the plan assumes `work` and `git`; correct if different).
- The resume path needs NO model credential: `finishPush` uses `pushWithRecovery` (git remote auth via `PushEnv`) and `adapter.OpenRequest`. No lease is minted.
- `registerTask` now stores a `context.CancelCauseFunc`; make sure every caller (HandleTask, resumePush) and every canceller (HandleKill, CancelAll) uses the cause form. `defer cancel(nil)` for normal completion.
- Keep the marker lifecycle exact: written in the gate onReady, removed on approve/deny/kill/timeout, LEFT on shutdown. A resumed task that hits shutdown again leaves its marker for the next boot (idempotent).
- Run `go test -race ./...`, `gofmt -l .`, `staticcheck ./...` before the PR.
