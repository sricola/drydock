# drydock submit streaming UX — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `drydock submit` stream live progress, surface a visible approval gate, give actionable failure messages, and print a rich completion summary — by having brokerd emit NDJSON events over the existing POST connection.

**Architecture:** brokerd's `HandleTask` writes newline-delimited JSON *events* (`accepted` → stage transitions → one terminal `result`/`error`) and flushes each; the `submit` client consumes the stream and renders it. The legacy single-object response remains readable by the new client (a line with no `event` field), so new-client↔old-brokerd still works. Reference spec: `docs/superpowers/specs/2026-06-21-submit-ux-design.md`.

**Tech Stack:** Go 1.26 (module `drydock`), stdlib `net/http` (chunked + `http.Flusher`), `encoding/json`, `bufio`. Tests via `go test` with the existing `prepareStage`/`runAgent` seams.

---

## File structure

| File | Responsibility |
|---|---|
| `internal/broker/stream.go` (new) | `stream` emitter (`newStream`/`emit`); pure helpers `diffStat`, `reasonFromAudit`, `auditCost`, `progressLine` |
| `internal/broker/stream_test.go` (new) | Unit tests for the pure helpers |
| `internal/broker/broker.go` (modify) | `HandleTask` — emit `accepted`+stage events, convert every post-accept exit to a terminal event |
| `internal/broker/handle_task_test.go` (modify) | NDJSON-aware `submit()` helper + `parseEvents`; rewrite existing assertions to the event model; add stage-sequence, boot-failure, denied/cancelled tests |
| `cmd/drydock/submit_render.go` (new) | `renderer` (stateful, mode-aware) + `consume()` stream loop |
| `cmd/drydock/submit_render_test.go` (new) | Table tests for the renderer + `consume()` (piped mode, legacy passthrough, error→exit 1) |
| `cmd/drydock/submit.go` (modify) | Replace `ReadAll`+`printPretty` with `consume()`; add `--quiet`; `--json` raw passthrough; non-200 handling |

Conventions to follow (already in the codebase): `package broker` helpers are unexported; the CLI is `package main` under `cmd/drydock` (so `submit_render.go` can use the existing `tty` var from `init.go` and `printPretty`/`diffPath` from `submit.go`). Commit after each task.

---

## Task 1: Broker stream helpers

**Files:**
- Create: `internal/broker/stream.go`
- Test: `internal/broker/stream_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/broker/stream_test.go`:

```go
package broker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiffStat(t *testing.T) {
	diff := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1,2 @@\n-old\n+new\n+more\n"
	files, ins, del := diffStat(diff)
	if files != 1 || ins != 2 || del != 1 {
		t.Errorf("diffStat = (%d,%d,%d), want (1,2,1)", files, ins, del)
	}
}

func TestReasonFromAudit_LastInfraLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	content := `{"type":"drydock_meta","subscription":true}` + "\n" +
		"[6/6] Starting container [0s]\n" +
		"/usr/local/bin/entrypoint.sh: line 5: DRYDOCK_GW_IP: missing gateway ip\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := reasonFromAudit(path)
	want := "/usr/local/bin/entrypoint.sh: line 5: DRYDOCK_GW_IP: missing gateway ip"
	if !ok || got != want {
		t.Errorf("reasonFromAudit = %q,%v; want %q,true", got, ok, want)
	}
}

func TestReasonFromAudit_NoMeaningfulLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"result"}`+"\n[1/6] x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := reasonFromAudit(path); ok {
		t.Error("want ok=false when only JSON/progress lines are present")
	}
}

func TestAuditCost_LastResultLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	body := `{"type":"result","total_cost_usd":0.05}` + "\n" +
		`{"type":"result","total_cost_usd":0.11}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := auditCost(path); c != 0.11 {
		t.Errorf("auditCost = %v, want 0.11", c)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/broker/ -run 'TestDiffStat|TestReasonFromAudit|TestAuditCost' -v`
Expected: FAIL — `undefined: diffStat`, `undefined: reasonFromAudit`, `undefined: auditCost`.

- [ ] **Step 3: Implement the helpers**

Create `internal/broker/stream.go`:

```go
package broker

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// stream writes newline-delimited JSON events to the POST /tasks response and
// flushes after each, so the submit client renders progress live instead of
// blocking on a single response at the end.
type stream struct {
	enc *json.Encoder
	f   http.Flusher
}

// newStream commits the response to a 200 NDJSON stream. Call it only after all
// pre-accept validation passes: once the header is written the status can no
// longer change, so every later exit must emit a terminal event, not http.Error.
func newStream(w http.ResponseWriter) *stream {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	return &stream{enc: json.NewEncoder(w), f: f}
}

// emit writes one event line and flushes. Encode errors (client gone) are
// ignored on purpose — task cancellation is driven by the request context, not
// by the success of this write.
func (s *stream) emit(ev map[string]any) {
	_ = s.enc.Encode(ev)
	if s.f != nil {
		s.f.Flush()
	}
}

var progressLine = regexp.MustCompile(`^\[\d+/\d+\]`)

// reasonFromAudit returns the last human-meaningful line of the audit log — the
// line that explains a boot failure (e.g. an entrypoint error). It skips empty
// lines, container progress lines ("[6/6] …"), and JSON event lines. ok is
// false when nothing meaningful is found, so the caller falls back to safeErr.
func reasonFromAudit(path string) (line string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		ln := strings.TrimSpace(lines[i])
		if ln == "" || strings.HasPrefix(ln, "{") || progressLine.MatchString(ln) {
			continue
		}
		return ln, true
	}
	return "", false
}

// auditCost returns total_cost_usd of the last result line in the audit log —
// the same value `drydock tasks` reports (see cmd/drydock/tasks.go) — so the
// submit summary agrees with it. Returns 0 when no result line is present.
func auditCost(path string) float64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var cost float64
	for _, ln := range strings.Split(string(data), "\n") {
		if !strings.Contains(ln, `"result"`) {
			continue
		}
		var r struct {
			Type         string  `json:"type"`
			TotalCostUSD float64 `json:"total_cost_usd"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(ln)), &r) == nil && r.Type == "result" {
			cost = r.TotalCostUSD
		}
	}
	return cost
}

// diffStat counts files changed and lines added/removed in a unified diff.
// Approximate — binary files and pure renames have no +/- content lines — which
// is fine for a one-line summary.
func diffStat(diff string) (files, insertions, deletions int) {
	for _, ln := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(ln, "diff --git "):
			files++
		case strings.HasPrefix(ln, "+++ "), strings.HasPrefix(ln, "--- "):
			// file headers, not content
		case strings.HasPrefix(ln, "+"):
			insertions++
		case strings.HasPrefix(ln, "-"):
			deletions++
		}
	}
	return
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/broker/ -run 'TestDiffStat|TestReasonFromAudit|TestAuditCost' -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/broker/stream.go internal/broker/stream_test.go
git commit -m "broker: add NDJSON stream emitter + diffstat/reason/cost helpers"
```

---

## Task 2: Update the broker test harness to parse NDJSON

The existing `submit()` helper unmarshals the whole body as one object; under streaming the body is multiple lines. Refactor the harness first (it still works against today's single-object response — that's one line), so the next task can write event-model assertions.

**Files:**
- Modify: `internal/broker/handle_task_test.go:94-101` (the `submit` helper)

- [ ] **Step 1: Replace the `submit` helper and add `parseEvents`**

Replace lines 94–101 (the current `submit` func) with:

```go
// parseEvents splits an NDJSON (or legacy single-object) body into decoded
// events. terminal is the last event — the result/error for a streamed task,
// or the single legacy object from an old code path.
func parseEvents(body string) (events []map[string]any, terminal map[string]any) {
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) == nil {
			events = append(events, ev)
		}
	}
	if len(events) > 0 {
		terminal = events[len(events)-1]
	}
	return events, terminal
}

func submit(b *Broker, body string) (*httptest.ResponseRecorder, []map[string]any, map[string]any) {
	req := httptest.NewRequest("POST", "/tasks", strings.NewReader(body))
	rec := httptest.NewRecorder()
	b.HandleTask(rec, req)
	events, terminal := parseEvents(rec.Body.String())
	return rec, events, terminal
}

// stages returns the ordered list of stage names from an event slice.
func stages(events []map[string]any) []string {
	var out []string
	for _, ev := range events {
		if ev["event"] == "stage" {
			if s, ok := ev["stage"].(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}
```

- [ ] **Step 2: Confirm it still compiles (callers updated in Task 3)**

Run: `go vet ./internal/broker/ 2>&1 | head`
Expected: compile errors in `handle_task_test.go` about `submit` now returning 3 values — **expected**; Task 3 updates every caller. (If you prefer a green checkpoint, do Step 1 of Task 3 in the same commit.)

- [ ] **Step 3: Commit**

```bash
git add internal/broker/handle_task_test.go
git commit -m "broker(test): NDJSON-aware submit() helper + parseEvents/stages"
```

---

## Task 3: Stream events from `HandleTask`

Test-first: rewrite the existing tests to the event model (they fail against today's single-write handler), then implement the emits.

**Files:**
- Modify: `internal/broker/handle_task_test.go` (all `submit`/`out` call sites + new tests)
- Modify: `internal/broker/broker.go` (`HandleTask`, ~L291–500)

- [ ] **Step 1: Rewrite existing test assertions to the event model**

In `internal/broker/handle_task_test.go`, update each test that calls `submit`/reads `out`:

`TestHandleTask_ClaudeAutoApprove_Pushes` — replace the call + assertions body (everything from the `rec, out := submit(...)` line through the audit checks) with:

```go
	b := testBroker(t, "anthropic", st, grant, writesResult(resultLine))
	rec, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"do x","agent":"claude","auto_approve":true}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if st.gotPrompt != "do x" {
		t.Errorf("WriteTaskFiles prompt=%q, want %q", st.gotPrompt, "do x")
	}
	if term["event"] != "result" || term["outcome"] != "pushed" {
		t.Fatalf("terminal=%v, want result/pushed (body=%s)", term, rec.Body)
	}
	id, _ := events[0]["task_id"].(string)
	if events[0]["event"] != "accepted" || id == "" {
		t.Fatalf("first event=%v, want accepted with task_id", events[0])
	}
	if term["branch"] != "agent/"+id {
		t.Errorf("branch=%v, want agent/%s", term["branch"], id)
	}
	if term["platform"] != "github" {
		t.Errorf("platform=%v, want github", term["platform"])
	}
	if !st.pushed || st.pushBranch != "agent/"+id || st.pushAdapter != "github" {
		t.Errorf("stage.Push wrong: pushed=%v branch=%q adapter=%q", st.pushed, st.pushBranch, st.pushAdapter)
	}
	if !grant.revoked {
		t.Error("grant.Revoke not called (defer)")
	}
	if !st.cleaned {
		t.Error("stage.Cleanup not called (defer)")
	}
	audit := readAudit(t, b.AuditRoot, id)
	if strings.Count(audit, `"type":"result"`) != 1 {
		t.Errorf("expected exactly one result line for claude, got:\n%s", audit)
	}
```

`TestHandleTask_CodexAutoApprove_SynthesizesResultWithCost` — replace its `rec, out := submit(...)` + the `rec.Code`/`out["pushed"]` check with:

```go
	rec, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"do x","agent":"codex","auto_approve":true}`)
	if rec.Code != http.StatusOK || term["outcome"] != "pushed" {
		t.Fatalf("code=%d outcome=%v body=%s", rec.Code, term["outcome"], rec.Body)
	}
	id, _ := events[0]["task_id"].(string)
```

(Keep the existing `audit := readAudit(...)` cost assertions that follow.)

`TestHandleTask_EmptyDiff_NoPush` — replace body after constructing `b` with:

```go
	rec, _, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if term["event"] != "result" || term["outcome"] != "no_diff" {
		t.Errorf("terminal=%v, want result/no_diff", term)
	}
	if st.pushed {
		t.Error("stage.Push must not be called when the diff is empty")
	}
```

`TestHandleTask_AgentRunFails_500AndErrorResult` — rename to `TestHandleTask_AgentRunFails_ErrorEvent` and replace its body after constructing `b` with:

```go
	rec, _, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`)
	// Streaming has already sent 200 by the time the run fails; the failure is
	// the terminal error event, not an HTTP status.
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200 (streamed); body=%s", rec.Code, rec.Body)
	}
	if term["event"] != "error" {
		t.Fatalf("terminal=%v, want error event", term)
	}
	if st.pushed {
		t.Error("stage.Push must not be called when the agent run fails")
	}
	audit := readOnlyAudit(t, b.AuditRoot)
	if !strings.Contains(audit, `"subtype":"error"`) || !strings.Contains(audit, `"is_error":true`) {
		t.Errorf("no synthesized error result:\n%s", audit)
	}
```

`TestHandleTask_GatedApprove_Pushes` and `TestHandleTask_GatedDeny_NoPush` — each ends by unmarshalling `rec.Body` into `out`. Replace those tail blocks:

For approve:
```go
	_, term := parseEvents(rec.Body.String())
	if term["event"] != "result" || term["outcome"] != "pushed" {
		t.Errorf("terminal=%v, want result/pushed; body=%s", term, rec.Body)
	}
	if !st.pushed {
		t.Error("stage.Push not called after approve")
	}
```

For deny:
```go
	_, term := parseEvents(rec.Body.String())
	if term["event"] != "result" || term["outcome"] != "denied" {
		t.Errorf("terminal=%v, want result/denied; body=%s", term, rec.Body)
	}
	if st.pushed {
		t.Error("stage.Push must not be called after deny")
	}
```

- [ ] **Step 2: Add new tests (stage sequence, approval-gate event, boot-failure hint)**

Append to `internal/broker/handle_task_test.go`:

```go
func TestHandleTask_StreamsStageSequence(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{spent: 0.02}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)

	got := stages(events)
	want := []string{"preparing", "running", "pushing"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("stage sequence = %v, want %v", got, want)
	}
	if events[0]["event"] != "accepted" {
		t.Errorf("first event = %v, want accepted", events[0])
	}
}

func TestHandleTask_ApprovalGateEvent(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/tasks",
		strings.NewReader(`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`))
	done := make(chan struct{})
	go func() { b.HandleTask(rec, req); close(done) }()

	id := waitForPending(t, b)
	approve(t, b, id)
	<-done

	events, _ := parseEvents(rec.Body.String())
	var gate map[string]any
	for _, ev := range events {
		if ev["stage"] == "awaiting_approval" {
			gate = ev
		}
	}
	if gate == nil {
		t.Fatalf("no awaiting_approval event; body=%s", rec.Body)
	}
	if gate["approve"] != "drydock approve "+id {
		t.Errorf("approve hint = %v, want 'drydock approve %s'", gate["approve"], id)
	}
}

func TestHandleTask_BootFailure_DistilledReasonAndHint(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "ignored"}
	grant := &fakeGrant{}
	// Simulate a boot failure: the runner writes an entrypoint error to stdout
	// (as `container run` would) and exits non-zero without a result line.
	run := func(_ context.Context, _ []string, stdout, _ io.Writer) error {
		fmt.Fprintln(stdout, "[6/6] Starting container [0s]")
		fmt.Fprintln(stdout, "/usr/local/bin/entrypoint.sh: line 5: DRYDOCK_GW_IP: missing gateway ip")
		return fmt.Errorf("exit status 1")
	}
	b := testBroker(t, "anthropic", st, grant, run)
	_, _, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`)

	if term["event"] != "error" {
		t.Fatalf("terminal=%v, want error", term)
	}
	if r, _ := term["reason"].(string); !strings.Contains(r, "missing gateway ip") {
		t.Errorf("reason=%q, want the distilled entrypoint line", term["reason"])
	}
	if h, _ := term["hint"].(string); !strings.Contains(h, "drydock doctor") {
		t.Errorf("hint=%q, want a 'drydock doctor' nudge", term["hint"])
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/broker/ -run TestHandleTask -v`
Expected: FAIL — terminals are the legacy object (`event`/`outcome` absent), no stage events, run-failure still returns 500. These are the assertions Task 3 Step 4 makes real.

- [ ] **Step 4: Implement the emits in `HandleTask`**

Apply these edits to `internal/broker/broker.go` (anchors are the current code; line numbers are approximate).

**4a — start the stream + `accepted`.** After the egress-validation block (the `if len(t.EgressExtra) > 0 { if err := egress.ValidateDomains(...) }` ending ~L329) and before the widening-gate comment, insert:

```go
	sw := newStream(w)
	sw.emit(map[string]any{"event": "accepted", "task_id": taskID, "repo": t.RepoRef})
```

**4b — egress gate.** In the `if len(t.EgressExtra) > 0 && b.Cfg.PerTaskWidening.RequiresApproval {` block, after `b.setStage(taskID, StageAwaitingEgress)` add:

```go
		sw.emit(map[string]any{
			"event": "stage", "stage": "awaiting_egress",
			"extras":  summariseExtras(t.EgressExtra),
			"approve": "drydock approve " + taskID,
			"deny":    "drydock deny " + taskID,
		})
```

Then replace the two failure exits inside that block:

```go
		if !ok {
			if taskCtx.Err() != nil {
				sw.emit(map[string]any{"event": "result", "outcome": "cancelled", "task_id": taskID})
				return
			}
			sw.emit(map[string]any{"event": "error", "task_id": taskID,
				"reason": "egress widening denied"})
			return
		}
```

**4c — `preparing` + clone/stage/agent/mint/audit failures.** Replace the block from `st, err := prepare(stageDir, t.RepoRef)` through the audit-file `os.OpenFile` error handling with the same logic but terminal events. Specifically:

Before the `prepare(...)` call, add:
```go
	sw.emit(map[string]any{"event": "stage", "stage": "preparing"})
```

Then convert these five `http.Error` exits to error events (same conditions, new bodies):

```go
	st, err := prepare(stageDir, t.RepoRef)
	if err != nil {
		sw.emit(errorEvent(taskID, "clone failed", "check the repo URL and that brokerd can reach it"))
		return
	}
	defer st.Cleanup()

	allowlist := egress.CompileAllowlist(b.Cfg, t.EgressExtra)
	if err := st.WriteTaskFiles(t.Instruction, allowlist); err != nil {
		sw.emit(errorEvent(taskID, "stage failed", ""))
		return
	}

	agentName, prov, status, msg := b.resolveAgent(t.Agent)
	if status != 0 {
		sw.emit(errorEvent(taskID, msg, ""))
		return
	}
	grant, err := prov.Mint(b.TaskBudget)
	if err != nil {
		sw.emit(errorEvent(taskID, "credential mint failed", ""))
		return
	}
	defer grant.Revoke()

	if err := os.MkdirAll(b.AuditRoot, 0o700); err != nil {
		sw.emit(errorEvent(taskID, "audit setup failed", ""))
		return
	}
	logf, err := os.OpenFile(
		filepath.Join(b.AuditRoot, taskID+".jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		sw.emit(errorEvent(taskID, "audit setup failed", ""))
		return
	}
	defer logf.Close()
```

Add this small constructor near the bottom of `broker.go` (next to `writeJSON`):

```go
// errorEvent builds a terminal error event. hint may be empty.
func errorEvent(taskID, reason, hint string) map[string]any {
	ev := map[string]any{"event": "error", "task_id": taskID, "reason": reason}
	if hint != "" {
		ev["hint"] = hint
	}
	return ev
}
```

**4d — `running`.** Immediately before `taskStart := time.Now()` (just above the `run(...)` call), add:

```go
	b.setStage(taskID, StageRunning)
	runningEv := map[string]any{"event": "stage", "stage": "running", "agent": agentName}
	if m := t.Model; m != "" {
		runningEv["model"] = m
	} else if b.DefaultModel != "" {
		runningEv["model"] = b.DefaultModel
	}
	sw.emit(runningEv)
```

**4e — run failure.** Replace the run-error block (currently the `if err := run(...); err != nil {` body that force-deletes the VM, writes the synthetic result, and `http.Error`s) so the two terminal exits become events. Keep the force-delete and synthetic-result writes unchanged; change only the responses:

```go
	if err := run(runCtx, args, io.MultiWriter(logf, os.Stdout), logf); err != nil {
		if derr := exec.Command("container", "delete", "--force", "task-"+taskID).Run(); derr != nil {
			slog.Warn("force-delete of task VM failed; reaped at next brokerd boot",
				"task_id", taskID, "err", derr)
		}
		if taskCtx.Err() != nil {
			sw.emit(map[string]any{"event": "result", "outcome": "cancelled", "task_id": taskID})
			return
		}
		_, _ = fmt.Fprintf(logf,
			`{"type":"result","subtype":"error","is_error":true,"duration_ms":%d,"total_cost_usd":0,"num_turns":0}`+"\n",
			time.Since(taskStart).Milliseconds())
		auditPath := filepath.Join(b.AuditRoot, taskID+".jsonl")
		reason := "task failed: " + safeErr(err)
		ev := map[string]any{"event": "error", "task_id": taskID,
			"audit": auditPath, "duration_ms": time.Since(taskStart).Milliseconds()}
		// Boot failure: the container never produced a real result. Surface the
		// last infra line and point at the doctor (see spec F4).
		if line, ok := reasonFromAudit(auditPath); ok {
			reason = line
			ev["hint"] = "run `drydock doctor` to check the sandbox image"
		}
		ev["reason"] = reason
		sw.emit(ev)
		return
	}
```

**4f — no-diff, gate, push, pushed.** Replace the remaining `writeJSON`/`http.Error` exits from `CaptureDiff` onward:

```go
	diff, err := st.CaptureDiff()
	if err != nil {
		sw.emit(errorEvent(taskID, "diff capture failed", ""))
		return
	}
	auditPath := filepath.Join(b.AuditRoot, taskID+".jsonl")
	if diff == "" {
		sw.emit(map[string]any{"event": "result", "outcome": "no_diff",
			"task_id": taskID, "duration_ms": time.Since(taskStart).Milliseconds(),
			"cost_usd": auditCost(auditPath)})
		return
	}

	files, insertions, deletions := diffStat(diff)
	b.setStage(taskID, StagePending)
	sw.emit(map[string]any{"event": "stage", "stage": "awaiting_approval",
		"task_id": taskID, "diff_bytes": len(diff), "files": files,
		"approve": "drydock approve " + taskID,
		"deny":    "drydock deny " + taskID,
		"review":  "drydock review " + taskID})
	if !b.gatePush(taskCtx, taskID, diff, t.AutoApprove) {
		outcome := "denied"
		if taskCtx.Err() != nil {
			outcome = "cancelled"
		}
		sw.emit(map[string]any{"event": "result", "outcome": outcome,
			"task_id": taskID, "diff_bytes": len(diff)})
		return
	}

	b.setStage(taskID, StagePushing)
	branch := "agent/" + taskID
	adapter := remote.AdapterFor(t.RepoRef, t.Platform)
	sw.emit(map[string]any{"event": "stage", "stage": "pushing", "task_id": taskID, "branch": branch})
	if err := st.Push(adapter, branch, "agent: "+firstLine(t.Instruction)); err != nil {
		sw.emit(errorEvent(taskID, "push failed: "+safeErr(err), "check the remote and push credentials"))
		return
	}
	sw.emit(map[string]any{"event": "result", "outcome": "pushed",
		"task_id": taskID, "branch": branch, "platform": adapter.Name(),
		"files": files, "insertions": insertions, "deletions": deletions,
		"duration_ms": time.Since(taskStart).Milliseconds(), "cost_usd": auditCost(auditPath)})
```

> Note: this removes the last use of `writeJSON` in `HandleTask`. Leave `writeJSON` defined — it's still used by other admin handlers (`grep -n writeJSON internal/broker/*.go` to confirm before deleting anything; do **not** delete it).

- [ ] **Step 5: Run the whole broker suite to verify it passes**

Run: `go test ./internal/broker/ -v`
Expected: PASS — all rewritten tests plus the three new ones.

- [ ] **Step 6: Commit**

```bash
git add internal/broker/broker.go internal/broker/handle_task_test.go
git commit -m "broker: stream NDJSON progress + terminal events from HandleTask"
```

---

## Task 4: Client renderer + stream consumer

**Files:**
- Create: `cmd/drydock/submit_render.go`
- Test: `cmd/drydock/submit_render_test.go`

- [ ] **Step 1: Write the failing tests**

Create `cmd/drydock/submit_render_test.go`:

```go
package main

import (
	"strings"
	"testing"
)

func TestConsume_PipedHappyPath(t *testing.T) {
	stream := strings.Join([]string{
		`{"event":"accepted","task_id":"7f3a0000","repo":"r"}`,
		`{"event":"stage","stage":"preparing"}`,
		`{"event":"stage","stage":"running","agent":"claude"}`,
		`{"event":"stage","stage":"pushing","branch":"agent/7f3a0000"}`,
		`{"event":"result","outcome":"pushed","branch":"agent/7f3a0000","platform":"github","files":4,"insertions":120,"deletions":8,"duration_ms":138000,"cost_usd":0.11}`,
	}, "\n") + "\n"

	var out strings.Builder
	exit := consume(strings.NewReader(stream), &out, modePiped)
	got := out.String()

	if exit != 0 {
		t.Errorf("exit=%d, want 0", exit)
	}
	for _, want := range []string{"accepted", "preparing", "running", "pushing", "pushed", "agent/7f3a0000", "github", "4 files", "+120", "-8", "$0.11"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestConsume_ApprovalBlock(t *testing.T) {
	stream := `{"event":"accepted","task_id":"7f3a0000"}` + "\n" +
		`{"event":"stage","stage":"awaiting_approval","diff_bytes":1234,"files":4,"approve":"drydock approve 7f3a0000","deny":"drydock deny 7f3a0000","review":"drydock review 7f3a0000"}` + "\n" +
		`{"event":"result","outcome":"pushed","branch":"b","platform":"github"}` + "\n"

	var out strings.Builder
	consume(strings.NewReader(stream), &out, modePiped)
	got := out.String()
	for _, want := range []string{"awaiting approval", "drydock approve 7f3a0000", "drydock review 7f3a0000"} {
		if !strings.Contains(got, want) {
			t.Errorf("approval block missing %q:\n%s", want, got)
		}
	}
}

func TestConsume_ErrorExitsOne(t *testing.T) {
	stream := `{"event":"accepted","task_id":"7f3a0000"}` + "\n" +
		`{"event":"error","reason":"entrypoint.sh: missing gateway ip","hint":"run drydock doctor","audit":"/x.jsonl"}` + "\n"

	var out strings.Builder
	exit := consume(strings.NewReader(stream), &out, modePiped)
	if exit != 1 {
		t.Errorf("exit=%d, want 1", exit)
	}
	got := out.String()
	for _, want := range []string{"missing gateway ip", "drydock doctor"} {
		if !strings.Contains(got, want) {
			t.Errorf("error output missing %q:\n%s", want, got)
		}
	}
}

func TestConsume_LegacyObjectFallsBackToPrintPretty(t *testing.T) {
	// An old brokerd: a single JSON object with no "event" field.
	stream := `{"task_id":"7f3a0000","branch":"agent/7f3a0000","platform":"github","pushed":true}` + "\n"
	var out strings.Builder
	exit := consume(strings.NewReader(stream), &out, modePiped)
	if exit != 0 {
		t.Errorf("exit=%d, want 0", exit)
	}
	if !strings.Contains(out.String(), "pushed agent/7f3a0000") {
		t.Errorf("legacy fallback output unexpected:\n%s", out.String())
	}
}

func TestConsume_QuietPrintsOnlyResult(t *testing.T) {
	stream := strings.Join([]string{
		`{"event":"accepted","task_id":"7f3a0000"}`,
		`{"event":"stage","stage":"running","agent":"claude"}`,
		`{"event":"result","outcome":"pushed","branch":"b","platform":"github"}`,
	}, "\n") + "\n"
	var out strings.Builder
	consume(strings.NewReader(stream), &out, modeQuiet)
	got := out.String()
	if strings.Contains(got, "running") || strings.Contains(got, "accepted") {
		t.Errorf("quiet mode leaked progress:\n%s", got)
	}
	if !strings.Contains(got, "pushed") {
		t.Errorf("quiet mode dropped the result:\n%s", got)
	}
}
```

This test references `printPretty` (already in `submit.go`) for the legacy fallback. The expected string `pushed agent/7f3a0000` matches `printPretty`'s `pushed %s (%s)` format.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/drydock/ -run TestConsume -v`
Expected: FAIL — `undefined: consume`, `undefined: modePiped`, `modeQuiet`.

- [ ] **Step 3: Implement the renderer and consumer**

Create `cmd/drydock/submit_render.go`:

```go
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

type renderMode int

const (
	modeTTY renderMode = iota
	modePiped
	modeQuiet
)

// renderer turns a stream of broker events into terminal output. It holds the
// little state TTY rendering needs (task id, start time); piped/quiet modes are
// stateless line printers and are what the tests exercise.
type renderer struct {
	w     io.Writer
	mode  renderMode
	id    string
	start time.Time
}

func newRenderer(w io.Writer, mode renderMode) *renderer {
	return &renderer{w: w, mode: mode}
}

// progress prints a transient status line (suppressed in quiet mode).
func (r *renderer) progress(s string) {
	if r.mode == modeQuiet {
		return
	}
	if r.mode == modeTTY {
		fmt.Fprintf(r.w, "\r\033[2K  %s", s)
		return
	}
	fmt.Fprintf(r.w, "  %s\n", s)
}

// persist prints a line that stays on screen (approval block, summary, error).
// On a TTY it first clears the transient progress line.
func (r *renderer) persist(s string) {
	if r.mode == modeTTY {
		fmt.Fprintf(r.w, "\r\033[2K%s\n", s)
		return
	}
	fmt.Fprintf(r.w, "%s\n", s)
}

// handle renders one event and reports the exit code + whether the stream is
// terminal.
func (r *renderer) handle(ev map[string]any) (exit int, done bool) {
	switch ev["event"] {
	case "accepted":
		r.id, _ = ev["task_id"].(string)
		r.start = time.Now()
		r.progress("task " + short(r.id) + " accepted")
	case "stage":
		r.stage(ev)
	case "result":
		r.summary(ev)
		return 0, true
	case "error":
		r.errorOut(ev)
		return 1, true
	}
	return 0, false
}

func (r *renderer) stage(ev map[string]any) {
	switch ev["stage"] {
	case "awaiting_egress":
		r.persist("⏸ awaiting egress approval · " + str(ev["extras"]))
		r.persist("   approve: " + str(ev["approve"]) + "     deny: " + str(ev["deny"]))
	case "preparing":
		r.progress("preparing · cloning repo")
	case "running":
		agent := str(ev["agent"])
		r.progress("running · " + agent + " working")
	case "awaiting_approval":
		files := num(ev["files"])
		r.persist(fmt.Sprintf("⏸ awaiting approval · %s diff (%d files)", humanBytes(num(ev["diff_bytes"])), files))
		if rv := str(ev["review"]); rv != "" {
			r.persist("   review:  " + rv)
		}
		r.persist("   approve: " + str(ev["approve"]) + "     deny: " + str(ev["deny"]) + "     (^C aborts)")
	case "pushing":
		r.progress("pushing → " + str(ev["branch"]))
	}
}

func (r *renderer) summary(ev map[string]any) {
	id := short(r.id)
	switch ev["outcome"] {
	case "pushed":
		stat := fmt.Sprintf("%d files +%d/−%d", num(ev["files"]), num(ev["insertions"]), num(ev["deletions"]))
		r.persist(fmt.Sprintf("✓ pushed %s (%s) · %s · %s%s",
			str(ev["branch"]), str(ev["platform"]), stat, durStr(ev["duration_ms"]), costStr(ev["cost_usd"])))
	case "no_diff":
		r.persist(fmt.Sprintf("✓ task %s finished · no changes%s", id, costStr(ev["cost_usd"])))
	case "denied":
		r.persist(fmt.Sprintf("task %s: diff denied — not pushed", id))
	case "cancelled":
		r.persist(fmt.Sprintf("task %s: cancelled", id))
	default:
		r.persist(fmt.Sprintf("task %s: %v", id, ev["outcome"]))
	}
}

func (r *renderer) errorOut(ev map[string]any) {
	r.persist("✗ task " + short(r.id) + " failed: " + str(ev["reason"]))
	if h := str(ev["hint"]); h != "" {
		r.persist("   → " + h)
	}
	if a := str(ev["audit"]); a != "" {
		r.persist("   audit: " + a)
	}
}

// consume reads the NDJSON event stream (or a legacy single object) from r,
// renders it to w, and returns the process exit code.
func consume(r io.Reader, w io.Writer, mode renderMode) int {
	rnd := newRenderer(w, mode)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			fmt.Fprintln(w, line) // not JSON — pass through defensively
			continue
		}
		if _, ok := ev["event"]; !ok {
			printPretty(ev) // legacy single-object response from an old brokerd
			return 0
		}
		if exit, done := rnd.handle(ev); done {
			return exit
		}
	}
	// Stream ended with no terminal event — brokerd likely died mid-task.
	rnd.persist("✗ connection closed before the task finished")
	return 1
}

// --- small formatting helpers ---

func short(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) int {
	if f, ok := v.(float64); ok { // JSON numbers decode to float64
		return int(f)
	}
	return 0
}

func durStr(v any) string {
	ms := num(v)
	if ms <= 0 {
		return "0s"
	}
	return (time.Duration(ms) * time.Millisecond).Truncate(time.Second).String()
}

func costStr(v any) string {
	f, ok := v.(float64)
	if !ok || f <= 0 {
		return ""
	}
	return fmt.Sprintf(" · $%.2f", f)
}

func humanBytes(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KB", float64(n)/1024)
}
```

> `printPretty` already prints `task %s: pushed %s (%s)` — the legacy test asserts the `pushed agent/7f3a0000` substring, so no change to `printPretty` is needed.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./cmd/drydock/ -run TestConsume -v`
Expected: PASS (5 tests).

- [ ] **Step 5: Commit**

```bash
git add cmd/drydock/submit_render.go cmd/drydock/submit_render_test.go
git commit -m "submit: NDJSON renderer + consume() with TTY/piped/quiet modes"
```

---

## Task 5: Wire the consumer into `submit.go`

**Files:**
- Modify: `cmd/drydock/submit.go` (flag set ~L44-58; `runSubmit` call ~L112; `postSubmit` ~L182-258)

- [ ] **Step 1: Add the `--quiet` flag**

In `runSubmit`, add to the `var ( … )` flag block (after the `jsonOut` line):

```go
		quiet = fs.Bool("quiet", false, "only print the final result line (no live progress)")
```

Change the `postSubmit` call near the end of `runSubmit` from:

```go
	if err := postSubmit(req, *jsonOut); err != nil {
```

to:

```go
	if err := postSubmit(req, *jsonOut, *quiet); err != nil {
```

- [ ] **Step 2: Replace the response-handling block in `postSubmit`**

Change the signature:

```go
func postSubmit(req taskRequest, jsonOut, quiet bool) error {
```

Replace everything from `respBody, rerr := io.ReadAll(resp.Body)` (~L230) through the end of the function (the `printPretty(out); return nil`) with:

```go
	if jsonOut {
		// Raw passthrough — works for the NDJSON stream or a legacy object,
		// and streams live so scripts see events as they arrive.
		_, _ = io.Copy(os.Stdout, resp.Body)
		if resp.StatusCode >= 400 {
			os.Exit(1)
		}
		return nil
	}

	// Pre-accept failures keep an HTTP error status + plain body (the stream
	// never started). Surface them directly.
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "drydock submit: %s\n", strings.TrimSpace(string(body)))
		os.Exit(1)
	}

	mode := modePiped
	if tty { // package-level, defined in init.go
		mode = modeTTY
	}
	if quiet {
		mode = modeQuiet
	}
	if exit := consume(resp.Body, os.Stdout, mode); exit != 0 {
		os.Exit(exit)
	}
	return nil
}
```

Then delete the now-unused `printPretty`? **No** — `consume` calls `printPretty` for the legacy path. Keep it.

After this edit, check imports: `bytes`, `encoding/json`, `strconv` may still be used elsewhere in the file (they are — `taskRequest` marshaling, `parseEgressExtras`). Run `goimports`/`gofmt` in Step 4; if `go build` flags an unused import, remove only the genuinely unused one.

- [ ] **Step 3: Manual smoke check against the real daemon (optional but recommended)**

If brokerd is running with a freshly built binary:
```bash
go build -o /tmp/drydock ./cmd/drydock
/tmp/drydock submit --repo git@github.com:sricola/drydock \
  --instruction "add a one-line comment to README.md" --auto-approve
```
Expected: live lines `accepted → preparing → running → pushing → ✓ pushed …`, not a silent block.

- [ ] **Step 4: Build, vet, format**

Run:
```bash
gofmt -l cmd/drydock/ internal/broker/
go vet ./cmd/drydock/ ./internal/broker/
go build ./...
```
Expected: `gofmt -l` prints nothing; vet and build clean.

- [ ] **Step 5: Commit**

```bash
git add cmd/drydock/submit.go
git commit -m "submit: stream live progress; add --quiet; --json raw passthrough"
```

---

## Task 6: Docs + full verification

**Files:**
- Modify: `CHANGELOG.md` (top entry)
- Modify: `README.md` (if it shows `drydock submit` output — update the example)

- [ ] **Step 1: Update CHANGELOG**

Add an entry under the latest/unreleased section describing: live streaming progress for `drydock submit`, a visible approval gate, actionable boot-failure errors (with a `drydock doctor` hint), a rich completion summary (diffstat/duration/cost), and the new `--quiet` flag. Note the one accepted limitation: an old `submit` client against a new brokerd prints raw NDJSON.

- [ ] **Step 2: Update README example (if present)**

Run: `grep -n "drydock submit" README.md`
If an output sample is shown, replace it with the streamed form:
```
task 7f3a… accepted
  running · claude working
  ⏸ awaiting approval · 1.2 KB diff (4 files)
     approve: drydock approve 7f3a…     review: drydock review 7f3a…
✓ pushed agent/7f3a… (github) · 4 files +120/−8 · 2m18s · $0.11
```

- [ ] **Step 3: Full suite + build**

Run:
```bash
go test ./...
gofmt -l .
go vet ./...
go build ./...
```
Expected: all tests pass; `gofmt -l` empty; vet/build clean.

- [ ] **Step 4: Commit**

```bash
git add CHANGELOG.md README.md
git commit -m "docs: document streaming submit UX + --quiet"
```

---

## Self-review (completed during planning)

**Spec coverage:**
- Live progress → Task 3 (stage emits) + Task 4 (renderer). ✓
- Actionable failure output (F4) → Task 1 (`reasonFromAudit`) + Task 3 step 4e (boot-failure hint) + test in Task 3. ✓
- Approval-gate clarity → Task 3 (`awaiting_approval` event w/ approve/deny/review) + Task 4 (`stage` approval block). ✓
- Rich summary → Task 3 (`result` carries diffstat/duration/cost) + Task 4 (`summary`). ✓
- Wire protocol / 200-then-terminal-event invariant → Task 3 step 4 converts **every** post-accept exit (egress, clone, stage, resolve, mint, audit, run, no-diff, gate, push, pushed) to a terminal event. ✓
- Cost from audit result line (F3) → `auditCost` (Task 1), used in `result` events (Task 3). ✓
- `preparing` stage (F2) → Task 3 step 4c + sequence test. ✓
- Backward compat (new client ↔ old brokerd) → `consume` legacy branch + test (Task 4). ✓
- `--json` / `--quiet` / TTY-piped modes → Task 4 (`renderMode`) + Task 5. ✓
- Streaming constraints (F6) → no `ResponseWriter` wrapper / no `WriteTimeout`; unchanged, noted in spec. Tasks don't touch `hardenedServer`. ✓

**Placeholder scan:** none — every code step is complete.

**Type consistency:** `stream`/`emit`, `errorEvent(taskID,reason,hint)`, `consume(r,w,mode)`, `renderMode{modeTTY,modePiped,modeQuiet}`, `renderer.handle(ev)(exit,done)`, helpers `short/str/num/durStr/costStr/humanBytes` are defined once and used consistently. `submit()` test helper's new 3-value signature is applied at every call site in Task 3 step 1.

---

## Execution handoff

See the bottom of this plan for execution options once you start.
