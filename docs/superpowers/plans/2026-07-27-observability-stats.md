# Observability (4.7): Metrics Enrichment + `drydock stats` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Per-task metrics recording (terminal `metrics` audit row + `ts` on stream events) and a `drydock stats` CLI that aggregates outcomes, durations, gate waits, spend, and egress-widen frequency across runs.

**Architecture:** The broker writes one broker-authored `type:"metrics"` row as the guaranteed-last line of each task's audit `.jsonl` (deferred hook registered when the audit log opens; resume path included). `internal/audit` gains a tail parser for it; a new `internal/stats` package collects samples from the audit dir and aggregates; `cmd/drydock/stats.go` renders. The gateway surfaces its existing per-lease request count on the grant via an optional-capability interface.

**Tech Stack:** Go 1.26 stdlib only. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-07-27-observability-design.md` (read it first).

## Global Constraints

- Branch: work on `observability-stats` (already exists, contains the spec).
- No em dashes anywhere (docs, comments, commit messages); use commas/colons/parens.
- Commit messages end with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- All audit reads use `audit.OpenRead` (O_NOFOLLOW); audit files are 0600, dir 0700.
- The metrics row must be broker-authored (`src:"broker"`), written after the agent's stream ends, and be the last row of the file; readers take the last matching row (last-wins).
- Existing readers must be unaffected: no changes to the semantics of `type:"result"`, `drydock_meta`, or `drydock_task` rows.
- `num_turns` in the result row changes from hardcoded 0 to the lease request count; `audit.Outcome` renders `ok (N turn)` when NumTurns > 0, which is desired.
- Run `gofmt` on every touched file; `go vet ./...` must stay clean.

---

### Task 1: Gateway surfaces the per-lease request count on the grant

**Files:**
- Modify: `internal/gateway/gateway.go` (add `requests()` next to `spent()` at ~line 110)
- Modify: `internal/gateway/provider.go` (add `Requests()` to `grant`)
- Test: `internal/gateway/provider_requests_test.go` (create)

**Interfaces:**
- Consumes: existing `Lease.Requests` (incremented by `admit()` on every admitted request), `Gateway.Mint`, `Provider.Mint`.
- Produces: `func (g *grant) Requests() int` on the gateway grant. Task 2 discovers it via `interface{ Requests() int }` type assertion; 0 after revoke or for non-gateway grants.

- [ ] **Step 1: Write the failing test**

Create `internal/gateway/provider_requests_test.go`:

```go
package gateway

import (
	"testing"
	"time"
)

// The broker's metrics row needs the number of requests a task actually made.
// The lease already counts admits; this verifies the grant surfaces it.
func TestGrantRequests_SurfacesLeaseCount(t *testing.T) {
	g, err := New(testBackend("anthropic"))
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{GW: g, Vendor: "anthropic", BaseURL: "http://gw",
		BaseURLEnv: "ANTHROPIC_BASE_URL", TokenEnv: "ANTHROPIC_AUTH_TOKEN",
		Budget: 1, TTL: time.Minute}
	cg, err := p.Mint(0)
	if err != nil {
		t.Fatal(err)
	}
	rc, ok := cg.(interface{ Requests() int })
	if !ok {
		t.Fatal("gateway grant does not implement Requests() int")
	}
	if got := rc.Requests(); got != 0 {
		t.Fatalf("fresh grant Requests()=%d, want 0", got)
	}

	// Find the minted token and simulate two admitted requests the way the
	// gateway itself does (admit() increments Lease.Requests).
	g.mu.Lock()
	for _, l := range g.leases {
		l.Requests = 2
	}
	g.mu.Unlock()
	if got := rc.Requests(); got != 2 {
		t.Fatalf("Requests()=%d, want 2", got)
	}

	// After revoke the lease is gone; report 0, never -1.
	_ = cg.Revoke()
	if got := rc.Requests(); got != 0 {
		t.Fatalf("Requests() after revoke=%d, want 0", got)
	}
}
```

Note: `testBackend(vendor)` is whatever helper the existing gateway tests use to build a `Backend` with a stub `Cred`. Find it with `grep -n "func testBackend\|Backend{" internal/gateway/*_test.go` and reuse the existing pattern; if the helper has a different name or signature, adapt the two setup lines (only the setup, not the assertions).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gateway/ -run TestGrantRequests -v`
Expected: FAIL with "gateway grant does not implement Requests() int"

- [ ] **Step 3: Implement**

In `internal/gateway/gateway.go`, directly below `spent()` (~line 117):

```go
// requests mirrors spent for the admitted-request count: -1 when the lease
// is gone (revoked/expired), so callers can distinguish absence from zero.
func (g *Gateway) requests(token string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if l := g.leases[token]; l != nil {
		return l.Requests
	}
	return -1
}
```

In `internal/gateway/provider.go`, below `Spent()`:

```go
// Requests reports how many requests the gateway admitted on this grant's
// lease. Not part of creds.Grant: the broker discovers it via an optional
// interface{ Requests() int } assertion, so non-gateway grants and test
// fakes need no change.
func (g *grant) Requests() int {
	n := g.gw.requests(g.token)
	if n < 0 {
		return 0
	}
	return n
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/gateway/ -v -run TestGrantRequests` then `go test ./internal/gateway/`
Expected: PASS, and the full package stays green.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/gateway.go internal/gateway/provider.go internal/gateway/provider_requests_test.go
git commit -m "feat(gateway): surface the per-lease admitted-request count on the grant

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: Broker writes the terminal metrics row (core paths)

**Files:**
- Create: `internal/broker/metrics.go`
- Modify: `internal/broker/broker.go` (taskRun fields ~line 294; defer + subscription capture in HandleTask ~line 508-518; prepStart at the "preparing" emit ~line 419; runEnd + num_turns in runSandbox/appendBrokerResult ~line 634-746)
- Test: `internal/broker/metrics_test.go` (create)

**Interfaces:**
- Consumes: `interface{ Requests() int }` from Task 1 (via type assertion on `tr.grant`); `trustbrief.RedactRepoRef(string) string`; `audit.TotalCost(path) float64`.
- Produces: the on-disk metrics row (JSON object below), and taskRun fields `prepStart, runEnd time.Time`, `egressGateWait, approvalGateWait, pushDur time.Duration`, `subscription bool`, `widenOutcome string`, `diffFiles int`, `diffBytes int64` that Task 3 fills for the gate/push paths. Task 4's parser must match these JSON keys exactly.

- [ ] **Step 1: Write the failing test**

Create `internal/broker/metrics_test.go` (reuses the fakes/helpers in `handle_task_test.go`, same package):

```go
package broker

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// lastMetricsLine returns the parsed last {"type":"metrics"} line and asserts
// it is the FINAL line of the audit file (guaranteed-last is the trust rule).
func lastMetricsLine(t *testing.T, auditData string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimRight(auditData, "\n"), "\n")
	var m map[string]any
	if json.Unmarshal([]byte(lines[len(lines)-1]), &m) != nil || m["type"] != "metrics" {
		t.Fatalf("last audit line is not a metrics row:\n%s", auditData)
	}
	return m
}

func TestHandleTask_Success_WritesMetricsRowLast(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{spent: 0.02}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"do x","agent":"claude","auto_approve":true}`)
	id, _ := events[0]["task_id"].(string)

	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	if m["src"] != "broker" || m["task_id"] != id {
		t.Fatalf("metrics row identity wrong: %v", m)
	}
	if m["agent"] != "claude" || m["vendor"] != "anthropic" || m["auth"] != "api_key" {
		t.Errorf("dimensions wrong: agent=%v vendor=%v auth=%v", m["agent"], m["vendor"], m["auth"])
	}
	if repo, _ := m["repo"].(string); repo == "" || strings.Contains(repo, "do x") {
		t.Errorf("repo=%q, want redacted non-empty repo ref", repo)
	}
	if _, ok := m["stage_ms"].(map[string]any); !ok {
		t.Errorf("stage_ms missing: %v", m)
	}
	if m["widen_outcome"] != "none" {
		t.Errorf("widen_outcome=%v, want none (no extras requested)", m["widen_outcome"])
	}
}

func TestHandleTask_AgentError_StillWritesMetricsRow(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir()}
	grant := &fakeGrant{spent: 0.01}
	b := testBroker(t, "anthropic", st, grant,
		func(ctx context.Context, args []string, stdout, stderr io.Writer) error {
			return errors.New("container exploded")
		})
	_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`)
	id, _ := events[0]["task_id"].(string)
	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	if m["src"] != "broker" {
		t.Fatalf("metrics row missing on the error path: %v", m)
	}
}

func TestAppendBrokerResult_NumTurnsFromGrantRequests(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &countingGrant{fakeGrant: fakeGrant{spent: 0.02}, requests: 7}
	b := testBroker(t, "anthropic", st, &grant.fakeGrant, writesResult(`{"type":"result","subtype":"success"}`))
	// testBroker takes *fakeGrant; swap the provider to mint the counting grant instead.
	b.Providers["anthropic"] = &staticProvider{g: grant}
	_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	id, _ := events[0]["task_id"].(string)
	audit := readAudit(t, b.AuditRoot, id)
	if !strings.Contains(audit, `"num_turns":7`) {
		t.Errorf("result row does not carry the lease request count:\n%s", audit)
	}
}
```

Add the two tiny fakes at the top of the file (imports: add `"context"`, `"io"`, `"drydock/internal/creds"`):

```go
type countingGrant struct {
	fakeGrant
	requests int
}

func (g *countingGrant) Requests() int { return g.requests }

type staticProvider struct{ g creds.Grant }

func (p *staticProvider) Mint(float64) (creds.Grant, error) { return p.g, nil }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/broker/ -run 'MetricsRow|NumTurnsFromGrant' -v`
Expected: FAIL ("last audit line is not a metrics row" and missing `"num_turns":7`)

- [ ] **Step 3: Implement the metrics row**

Create `internal/broker/metrics.go`:

```go
package broker

import (
	"encoding/json"
	"fmt"

	"drydock/internal/audit"
	"drydock/internal/trustbrief"
)

// stageMs is the per-stage wall-clock breakdown carried by the metrics row.
type stageMs struct {
	Preparing int64 `json:"preparing"`
	Running   int64 `json:"running"`
	Pushing   int64 `json:"pushing"`
}

// taskMetrics is the terminal broker-authored metrics row: one per task,
// written as the LAST line of the audit stream on every exit path once the
// audit log exists. Readers take the last such row (last-wins), mirroring
// the result-row trust rule: the broker writes it after the agent's output
// ends, so an in-VM agent printing a forged row is superseded.
type taskMetrics struct {
	Type               string  `json:"type"` // "metrics"
	Src                string  `json:"src"`  // "broker"
	TaskID             string  `json:"task_id"`
	Agent              string  `json:"agent"`
	Vendor             string  `json:"vendor"`
	Auth               string  `json:"auth"` // "api_key" | "subscription"
	Repo               string  `json:"repo"` // redacted, same form as the brief
	Model              string  `json:"model,omitempty"`
	StageMs            stageMs `json:"stage_ms"`
	EgressGateWaitMs   int64   `json:"egress_gate_wait_ms"`
	ApprovalGateWaitMs int64   `json:"approval_gate_wait_ms"`
	Requests           int     `json:"requests"`
	DiffFiles          int     `json:"diff_files"`
	DiffBytes          int64   `json:"diff_bytes"`
	CostUSD            float64 `json:"cost_usd"`
	WidenRequested     int     `json:"widen_requested"`
	WidenOutcome       string  `json:"widen_outcome"` // "none" | "approved"
}

// appendMetrics writes the terminal metrics row. Registered as a defer right
// after the audit log opens, so it runs on every exit path and lands after
// the terminal result row (defers registered later run first, so it still
// precedes the log Sync/Close pair). Cost is re-read from the audit's own
// last result row so live and resume paths agree with the displayed cost.
func (tr *taskRun) appendMetrics() {
	if tr.logf == nil {
		return
	}
	m := taskMetrics{
		Type: "metrics", Src: "broker", TaskID: tr.id,
		Agent: tr.agentName, Vendor: tr.taskVendor,
		Auth: "api_key",
		Repo: trustbrief.RedactRepoRef(tr.repoRef),
		Model: tr.model,
		EgressGateWaitMs:   tr.egressGateWait.Milliseconds(),
		ApprovalGateWaitMs: tr.approvalGateWait.Milliseconds(),
		DiffFiles:          tr.diffFiles,
		DiffBytes:          tr.diffBytes,
		CostUSD:            audit.TotalCost(tr.auditPath),
		WidenRequested:     len(tr.egressExtra),
		WidenOutcome:       tr.widenOutcome,
	}
	if tr.subscription {
		m.Auth = "subscription"
	}
	if m.WidenOutcome == "" {
		m.WidenOutcome = "none"
	}
	if !tr.prepStart.IsZero() && !tr.taskStart.IsZero() {
		m.StageMs.Preparing = tr.taskStart.Sub(tr.prepStart).Milliseconds()
	}
	if !tr.taskStart.IsZero() && !tr.runEnd.IsZero() {
		m.StageMs.Running = tr.runEnd.Sub(tr.taskStart).Milliseconds()
	}
	m.StageMs.Pushing = tr.pushDur.Milliseconds()
	if rc, ok := tr.grant.(interface{ Requests() int }); ok {
		m.Requests = rc.Requests()
	}
	if b, err := json.Marshal(m); err == nil {
		_, _ = fmt.Fprintf(tr.logf, "%s\n", b)
	}
}
```

In `internal/broker/broker.go`:

1. Add to the `taskRun` struct (after `taskStart time.Time`, ~line 294):

```go
	// Metrics capture (observability 4.7): filled as the lifecycle advances,
	// written once by the deferred appendMetrics.
	prepStart        time.Time     // set at the "preparing" stage emit
	runEnd           time.Time     // set when the agent run returns
	egressGateWait   time.Duration // wall-clock at the egress-widen gate
	approvalGateWait time.Duration // wall-clock at the diff-approval gate
	pushDur          time.Duration // finishPush wall-clock
	subscription     bool          // this task's lane is unmetered
	widenOutcome     string        // "" (none) | "approved"; denied dies pre-audit
	diffFiles        int
	diffBytes        int64
```

2. In `HandleTask`, right before the `sw.emit(... "preparing" ...)` line (~419): `tr.prepStart = time.Now()`.

3. In `HandleTask`, after `tr.auditPath = filepath.Join(...)` (~line 508), register the defer (later-registered defers run first, so this runs before the Sync/Close pair):

```go
	// Terminal metrics row (observability): registered after the Sync/Close
	// defers so it runs before them, on every exit path from here on.
	defer tr.appendMetrics()
```

4. Two lines below, the existing `subscription := ...` computation (~line 516): also store it, `tr.subscription = subscription`.

5. In `runSandbox` (~line 689), capture the run end on both branches by restructuring the call:

```go
	err := run(runCtx, args, outCap.wrap(io.MultiWriter(tr.logf, os.Stdout)), outCap.wrap(tr.logf))
	tr.runEnd = time.Now()
	if err != nil {
```

6. In `appendBrokerResult` (~line 634), replace the hardcoded `num_turns":0` with the lease count:

```go
func (tr *taskRun) appendBrokerResult(isError bool) {
	subtype := "success"
	if isError {
		subtype = "error"
	}
	turns := 0
	if rc, ok := tr.grant.(interface{ Requests() int }); ok {
		turns = rc.Requests()
	}
	_, _ = fmt.Fprintf(tr.logf,
		`{"type":"result","subtype":"%s","is_error":%t,"duration_ms":%d,"total_cost_usd":%.6f,"num_turns":%d,"src":"broker"}`+"\n",
		subtype, isError, time.Since(tr.taskStart).Milliseconds(), tr.grant.Spent(), turns)
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/broker/ -run 'MetricsRow|NumTurnsFromGrant' -v` then `go test ./internal/broker/`
Expected: new tests PASS and the whole package stays green. If an existing test asserts the exact result-row string with `num_turns":0`, it still passes (fakeGrant has no Requests method, so turns stays 0).

- [ ] **Step 5: Commit**

```bash
git add internal/broker/metrics.go internal/broker/broker.go internal/broker/metrics_test.go
git commit -m "feat(broker): terminal metrics audit row + num_turns from the lease request count

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: Gate waits, widen outcome, push metrics, resume path

**Files:**
- Modify: `internal/broker/broker.go` (`runEgressGate` ~line 571; `pushAndOpenPR` ~line 837; `finishPush` ~line 872)
- Modify: `internal/broker/reconcile.go` (`resumePush` ~line 116)
- Test: `internal/broker/metrics_test.go` (extend)

**Interfaces:**
- Consumes: taskRun fields from Task 2 (`egressGateWait`, `approvalGateWait`, `pushDur`, `widenOutcome`, `diffFiles`, `diffBytes`) and `tr.appendMetrics()`.
- Produces: those fields populated on the gate/push/resume paths; a metrics row on resumed tasks.

- [ ] **Step 1: Write the failing tests**

Append to `internal/broker/metrics_test.go`. Model the approval-gate driving on the existing non-auto-approve test (find it: `grep -n "HandleApprove\|approve" internal/broker/handle_task_test.go internal/broker/gatecause_test.go`); the pattern is: run `submit` in a goroutine, poll `b.HandlePending`/the pending map until the task is gated, then `POST /admin/approve/{id}` via `b.HandleApprove`, then wait for submit to return.

```go
func TestHandleTask_ApprovalGate_RecordsWaitAndDiffFacts(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{spent: 0.02}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))

	done := make(chan string, 1)
	go func() {
		_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`)
		id, _ := events[0]["task_id"].(string)
		done <- id
	}()
	id := approveWhenPending(t, b) // helper below
	got := <-done
	if got != id {
		t.Fatalf("approved %s but submit returned %s", id, got)
	}

	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	if w, _ := m["approval_gate_wait_ms"].(float64); w <= 0 {
		t.Errorf("approval_gate_wait_ms=%v, want > 0", m["approval_gate_wait_ms"])
	}
	if f, _ := m["diff_files"].(float64); f != 1 {
		t.Errorf("diff_files=%v, want 1", m["diff_files"])
	}
	if bts, _ := m["diff_bytes"].(float64); bts <= 0 {
		t.Errorf("diff_bytes=%v, want > 0", m["diff_bytes"])
	}
	if p, ok := m["stage_ms"].(map[string]any); !ok || p["pushing"] == nil {
		t.Errorf("stage_ms.pushing missing: %v", m)
	}
}
```

`approveWhenPending(t, b)` polls until one task is pending and approves it; write it by copying the wait-then-approve lines of the existing gate test verbatim into a helper (do not invent a new mechanism; reuse whatever that test does, typically polling an exported handler with `httptest`).

Widen-approved test (egress gate engaged and approved):

```go
func TestHandleTask_WidenApproved_RecordsOutcomeAndWait(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{spent: 0}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	b.Cfg = egress.Config{Widening: egress.Widening{RequiresApproval: true}}

	done := make(chan string, 1)
	go func() {
		_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true,"egress_extra":[{"host":"proxy.example.com","ports":[443]}]}`)
		id, _ := events[0]["task_id"].(string)
		done <- id
	}()
	approveWhenPending(t, b)
	id := <-done

	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	if m["widen_outcome"] != "approved" {
		t.Errorf("widen_outcome=%v, want approved", m["widen_outcome"])
	}
	if n, _ := m["widen_requested"].(float64); n != 1 {
		t.Errorf("widen_requested=%v, want 1", m["widen_requested"])
	}
	if w, _ := m["egress_gate_wait_ms"].(float64); w <= 0 {
		t.Errorf("egress_gate_wait_ms=%v, want > 0", m["egress_gate_wait_ms"])
	}
}
```

Check the real field names for `egress.Config`/widening before running (`grep -n "RequiresApproval\|WideningRequiresApproval" internal/egress/*.go internal/broker/*.go`) and use the same construction the existing widening tests use (`internal/broker/widening_test.go`); adapt the `b.Cfg` line only.

Resume test: extend the existing resume test file pattern (`internal/broker/reconcile_test.go` has a resume-approve test; find it with `grep -n "resume" internal/broker/reconcile_test.go`). Add an assertion at its end that the audit file's last line is a metrics row (reuse `lastMetricsLine`).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/broker/ -run 'ApprovalGate_RecordsWait|WidenApproved_Records' -v`
Expected: FAIL (wait 0, outcome "none").

- [ ] **Step 3: Implement**

In `runEgressGate` (broker.go ~line 584), around the gate call:

```go
	gateStart := time.Now()
	ok := b.gateEgressWiden(tr.ctx, tr.id, extras)
	tr.egressGateWait = time.Since(gateStart)
	b.setEgressExtra(tr.id, nil)
	if !ok {
		...unchanged...
	}
	tr.widenOutcome = "approved"
```

Also cover the no-gate widening path: at the top of `runEgressGate`, when `len(extras) > 0 && !b.Cfg.WideningRequiresApproval()`, set `tr.widenOutcome = "approved"` before returning true (extras are applied without a gate).

In `pushAndOpenPR` (~line 852):

```go
	tr.diffFiles = files
	tr.diffBytes = int64(len(diff))
	gateStart := time.Now()
	approved, cause := b.gatePushMarked(tr.ctx, tr, diff)
	if !tr.autoApprove {
		tr.approvalGateWait = time.Since(gateStart)
	}
```

In `finishPush` (~line 874), first lines:

```go
	pushStart := time.Now()
	defer func() { tr.pushDur = time.Since(pushStart) }()
```

In `resumePush` (reconcile.go ~line 138), after the `taskRun` literal is built, add the fields and the deferred row (the resumed task has no grant and no prep/run phases; those report zero):

```go
	tr.subscription = audit.ReadMeta(tr.auditPath).Subscription
	defer tr.appendMetrics()
```

and record the approval wait around its `gatePushMarked` call the same way as the live path (`gateStart := time.Now()` before, `tr.approvalGateWait = time.Since(gateStart)` after; the resume path never auto-approves). Also set `tr.diffFiles`/`tr.diffBytes` from its `diffStat(diff)`/`len(diff)` values. Note `resumePush` returns early on `gateShutdown` before the row would be useful; the defer still writes a row there, which is correct (the next boot's resume appends a fresher one; last-wins).

Note on `taskVendor` in resume: the gate marker records `Agent`; resolve the vendor with `provider.VendorForAgent(m.Agent)` (already imported in broker.go; add the same call in reconcile.go) and set `tr.taskVendor`, `tr.agentName` (agentName is already set from `m.Agent`).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/broker/ -race`
Expected: PASS, including the extended resume test.

- [ ] **Step 5: Commit**

```bash
git add internal/broker/broker.go internal/broker/reconcile.go internal/broker/metrics_test.go
git commit -m "feat(broker): record gate waits, widen outcome, and push metrics; metrics row on resume

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: `ts` on stream events

**Files:**
- Modify: `internal/broker/stream.go` (`emit`, ~line 38)
- Test: `internal/broker/stream_test.go` (extend)

**Interfaces:**
- Consumes: nothing new.
- Produces: every emitted stream event carries `ts` (RFC 3339 UTC). CLI submit rendering and tests that assert specific keys are unaffected (they read keys, not key sets).

- [ ] **Step 1: Write the failing test**

Append to `internal/broker/stream_test.go` (match its existing style; it already exercises `emit` through `newStream`):

```go
func TestEmit_StampsTS(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newStream(rec)
	s.emit(map[string]any{"event": "stage", "stage": "preparing"})
	var ev map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(rec.Body.String())), &ev); err != nil {
		t.Fatal(err)
	}
	tsStr, _ := ev["ts"].(string)
	if _, err := time.Parse(time.RFC3339, tsStr); err != nil {
		t.Fatalf("ts=%q not RFC3339: %v", tsStr, err)
	}
}
```

(Adjust imports to whatever the file already has: `encoding/json`, `net/http/httptest`, `strings`, `time`.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/broker/ -run TestEmit_StampsTS -v`
Expected: FAIL (ts empty).

- [ ] **Step 3: Implement**

In `stream.emit`:

```go
func (s *stream) emit(ev map[string]any) {
	if _, ok := ev["ts"]; !ok {
		ev["ts"] = time.Now().UTC().Format(time.RFC3339)
	}
	_ = s.enc.Encode(ev)
	if s.f != nil {
		s.f.Flush()
	}
}
```

(Add `"time"` to stream.go's imports.)

- [ ] **Step 4: Run tests**

Run: `go test ./internal/broker/ && go test ./cmd/drydock/`
Expected: PASS (submit rendering ignores unknown keys; if any test asserts an exact event map, add `ts` there rather than weakening emit).

- [ ] **Step 5: Commit**

```bash
git add internal/broker/stream.go internal/broker/stream_test.go
git commit -m "feat(broker): stamp stream events with an RFC3339 ts

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: `audit` package parses the metrics row

**Files:**
- Modify: `internal/audit/audit.go`
- Test: `internal/audit/metrics_test.go` (create)

**Interfaces:**
- Consumes: the JSON keys written by Task 2 (must match exactly: `type`, `src`, `task_id`, `agent`, `vendor`, `auth`, `repo`, `model`, `stage_ms{preparing,running,pushing}`, `egress_gate_wait_ms`, `approval_gate_wait_ms`, `requests`, `diff_files`, `diff_bytes`, `cost_usd`, `widen_requested`, `widen_outcome`).
- Produces: `audit.Metrics` struct, `audit.StageMs` struct, and `func LastMetricsFile(f *os.File) (Metrics, bool)`; Task 6 consumes them.

- [ ] **Step 1: Write the failing test**

Create `internal/audit/metrics_test.go`:

```go
package audit

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenRead(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestLastMetricsFile_ParsesBrokerRow(t *testing.T) {
	f := writeTemp(t, `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"result","subtype":"success","is_error":false,"duration_ms":1200,"total_cost_usd":0.05,"num_turns":3,"src":"broker"}
{"type":"metrics","src":"broker","task_id":"abc","agent":"claude","vendor":"anthropic","auth":"api_key","repo":"github.com/o/r","stage_ms":{"preparing":100,"running":1200,"pushing":50},"egress_gate_wait_ms":0,"approval_gate_wait_ms":900,"requests":3,"diff_files":2,"diff_bytes":512,"cost_usd":0.05,"widen_requested":0,"widen_outcome":"none"}
`)
	m, ok := LastMetricsFile(f)
	if !ok {
		t.Fatal("no metrics row found")
	}
	if m.Agent != "claude" || m.Vendor != "anthropic" || m.Auth != "api_key" {
		t.Errorf("dims: %+v", m)
	}
	if m.StageMs.Running != 1200 || m.ApprovalGateWaitMs != 900 || m.Requests != 3 {
		t.Errorf("timings: %+v", m)
	}
}

func TestLastMetricsFile_ForgedRowSuperseded(t *testing.T) {
	// An in-VM agent prints a forged metrics row (even with src:broker);
	// the broker's real row comes after, and last-wins must pick it.
	f := writeTemp(t, `{"type":"metrics","src":"broker","task_id":"abc","cost_usd":0.000001,"agent":"forged"}
{"type":"metrics","src":"broker","task_id":"abc","agent":"claude","cost_usd":0.05}
`)
	m, ok := LastMetricsFile(f)
	if !ok || m.Agent != "claude" {
		t.Fatalf("last-wins violated: %+v ok=%v", m, ok)
	}
}

func TestLastMetricsFile_AbsentOnOldFiles(t *testing.T) {
	f := writeTemp(t, `{"type":"drydock_meta","subscription":true,"sensitive":false}
{"type":"result","subtype":"success","is_error":false,"duration_ms":10,"total_cost_usd":0,"num_turns":0,"src":"broker"}
`)
	if _, ok := LastMetricsFile(f); ok {
		t.Fatal("metrics reported on a pre-metrics file")
	}
}

func TestLastMetricsFile_IgnoresNonBrokerSrc(t *testing.T) {
	f := writeTemp(t, `{"type":"metrics","src":"agent","task_id":"abc","agent":"evil"}
`)
	if _, ok := LastMetricsFile(f); ok {
		t.Fatal("non-broker metrics row must be ignored")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/audit/ -run LastMetricsFile -v`
Expected: compile FAIL (`LastMetricsFile` undefined).

- [ ] **Step 3: Implement**

In `internal/audit/audit.go`, after the `Result`/`Meta` declarations:

```go
// StageMs is the per-stage wall-clock breakdown of a metrics row.
type StageMs struct {
	Preparing int64 `json:"preparing"`
	Running   int64 `json:"running"`
	Pushing   int64 `json:"pushing"`
}

// Metrics is the broker-authored terminal {"type":"metrics"} row (one per
// task, last line of the file). Only Src=="broker" rows count, and the last
// one wins: the broker writes it after the agent's output ends, so a forged
// in-VM row is always superseded.
type Metrics struct {
	Type               string  `json:"type"`
	Src                string  `json:"src"`
	TaskID             string  `json:"task_id"`
	Agent              string  `json:"agent"`
	Vendor             string  `json:"vendor"`
	Auth               string  `json:"auth"`
	Repo               string  `json:"repo"`
	Model              string  `json:"model"`
	StageMs            StageMs `json:"stage_ms"`
	EgressGateWaitMs   int64   `json:"egress_gate_wait_ms"`
	ApprovalGateWaitMs int64   `json:"approval_gate_wait_ms"`
	Requests           int     `json:"requests"`
	DiffFiles          int     `json:"diff_files"`
	DiffBytes          int64   `json:"diff_bytes"`
	CostUSD            float64 `json:"cost_usd"`
	WidenRequested     int     `json:"widen_requested"`
	WidenOutcome       string  `json:"widen_outcome"`
}

// LastMetricsFile finds the final broker-authored {"type":"metrics"} line by
// reading only the file tail (same 16KB window as LastResultFile). ok=false
// when absent (pre-metrics trace, interrupted task, or still running).
func LastMetricsFile(f *os.File) (Metrics, bool) {
	info, err := f.Stat()
	if err != nil {
		return Metrics{}, false
	}
	const tail = 16 * 1024
	off := int64(0)
	if info.Size() > tail {
		off = info.Size() - tail
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return Metrics{}, false
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return Metrics{}, false
	}
	lines := bytes.Split(data, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		var m Metrics
		if json.Unmarshal(lines[i], &m) == nil && m.Type == "metrics" && m.Src == "broker" {
			return m, true
		}
	}
	return Metrics{}, false
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/audit/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/audit/audit.go internal/audit/metrics_test.go
git commit -m "feat(audit): parse the broker-authored terminal metrics row (last-wins, src:broker)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 6: `internal/stats` aggregator

**Files:**
- Create: `internal/stats/stats.go`
- Test: `internal/stats/stats_test.go` (create)

**Interfaces:**
- Consumes: `audit.OpenRead`, `audit.LastResultFile`, `audit.ReadMetaFile`, `audit.LastMetricsFile`, `audit.TaskAgent`, `audit.HasDuration`.
- Produces (Task 7 consumes exactly these):

```go
type Sample struct {
	ID       string
	MTime    time.Time
	Outcome  string // "ok"|"error"|"push_failed"|"interrupted"|"running"
	DurationMs int64
	HasDuration bool
	CostUSD  float64
	Metered  bool
	Agent, Vendor, Auth, Repo string
	HasMetrics bool
	M        audit.Metrics
}

type Summary struct {
	Tasks           int              `json:"tasks"`
	Outcomes        map[string]int   `json:"outcomes"`
	DurP50Ms        int64            `json:"dur_p50_ms"`
	DurP95Ms        int64            `json:"dur_p95_ms"`
	EgressWaitP50Ms int64            `json:"egress_wait_p50_ms"`
	EgressWaitP95Ms int64            `json:"egress_wait_p95_ms"`
	ApprovalWaitP50Ms int64          `json:"approval_wait_p50_ms"`
	ApprovalWaitP95Ms int64          `json:"approval_wait_p95_ms"`
	SpendUSD        float64          `json:"spend_usd"`
	SpendPerDayUSD  float64          `json:"spend_per_day_usd"`
	UnmeteredTasks  int              `json:"unmetered_tasks"`
	Requests        int              `json:"requests"`
	WidenRequested  int              `json:"widen_requested"`
	WidenApproved   int              `json:"widen_approved"`
	PreMetricsTasks int              `json:"pre_metrics_tasks"`
}

type Group struct {
	Key string  `json:"key"`
	Summary     // embedded
}

type Report struct {
	Since        time.Time `json:"since"`
	Overall      Summary   `json:"overall"`
	Groups       []Group   `json:"groups,omitempty"`
	GroupBy      string    `json:"group_by,omitempty"`
	OrphanWidens int       `json:"orphan_widens"` // widen requested, task never ran
	SkippedFiles int       `json:"skipped_files"`
}

func Collect(dir string, since time.Time) ([]Sample, int, int)
    // returns samples, orphanWidens, skippedFiles
func Summarize(samples []Sample) Summary
func GroupBy(samples []Sample, dim string) ([]Group, error) // agent|vendor|repo|day|week
```

- [ ] **Step 1: Write the failing tests**

Create `internal/stats/stats_test.go`. Fixture builder + the core behaviors:

```go
package stats

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeAudit drops an audit fixture file with a given mtime.
func writeAudit(t *testing.T, dir, id, content string, mtime time.Time) {
	t.Helper()
	p := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

const newFormat = `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":60000,"total_cost_usd":0.10,"num_turns":4,"src":"broker"}
{"type":"metrics","src":"broker","task_id":"ID","agent":"claude","vendor":"anthropic","auth":"api_key","repo":"github.com/o/r","stage_ms":{"preparing":5000,"running":60000,"pushing":800},"egress_gate_wait_ms":0,"approval_gate_wait_ms":30000,"requests":4,"diff_files":2,"diff_bytes":512,"cost_usd":0.10,"widen_requested":0,"widen_outcome":"none"}
`

const oldFormat = `{"type":"drydock_meta","subscription":true,"sensitive":false}
{"type":"drydock_task","agent":"codex"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":30000,"total_cost_usd":0,"num_turns":0,"src":"broker"}
`

func TestCollect_MixedFormatsAndSinceFilter(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeAudit(t, dir, "new1", newFormat, now.Add(-1*time.Hour))
	writeAudit(t, dir, "old1", oldFormat, now.Add(-2*time.Hour))
	writeAudit(t, dir, "ancient", oldFormat, now.Add(-90*24*time.Hour))
	// A malformed file must be skipped, not fatal.
	writeAudit(t, dir, "garbled", "not json at all\n", now.Add(-1*time.Hour))
	// Orphan widen: task denied at the egress gate, no .jsonl ever existed.
	if err := os.WriteFile(filepath.Join(dir, "gone.widen.json"), []byte(`[{"host":"x.example.com","ports":[443]}]`), 0o600); err != nil {
		t.Fatal(err)
	}

	samples, orphans, _ := Collect(dir, now.Add(-30*24*time.Hour))
	if len(samples) != 3 { // new1, old1, garbled(as running/unknown) — ancient filtered
		t.Fatalf("samples=%d, want 3 (ancient filtered by since)", len(samples))
	}
	if orphans != 1 {
		t.Errorf("orphanWidens=%d, want 1", orphans)
	}
	var newS, oldS *Sample
	for i := range samples {
		switch samples[i].ID {
		case "new1":
			newS = &samples[i]
		case "old1":
			oldS = &samples[i]
		}
	}
	if newS == nil || !newS.HasMetrics || newS.Vendor != "anthropic" || newS.Auth != "api_key" {
		t.Fatalf("new-format sample wrong: %+v", newS)
	}
	if oldS == nil || oldS.HasMetrics || oldS.Agent != "codex" || oldS.Metered {
		t.Fatalf("old-format sample wrong: %+v (agent from drydock_task, unmetered from meta)", oldS)
	}
	if oldS.Vendor != "openai" {
		t.Errorf("old-format vendor=%q, want openai derived from agent codex", oldS.Vendor)
	}
}

func TestSummarize_SpendGatesAndFallbacks(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeAudit(t, dir, "a", newFormat, now.Add(-1*time.Hour))
	writeAudit(t, dir, "b", oldFormat, now.Add(-25*time.Hour))
	samples, _, _ := Collect(dir, time.Time{})
	s := Summarize(samples)
	if s.Tasks != 2 || s.Outcomes["ok"] != 2 {
		t.Fatalf("summary counts wrong: %+v", s)
	}
	if s.SpendUSD != 0.10 || s.UnmeteredTasks != 1 {
		t.Errorf("spend must cover metered tasks only: %+v", s)
	}
	if s.ApprovalWaitP50Ms != 30000 {
		t.Errorf("approval wait p50=%d, want 30000 (zero-wait tasks excluded)", s.ApprovalWaitP50Ms)
	}
	if s.PreMetricsTasks != 1 {
		t.Errorf("pre_metrics_tasks=%d, want 1", s.PreMetricsTasks)
	}
	if s.DurP95Ms != 60000 {
		t.Errorf("dur p95=%d, want 60000", s.DurP95Ms)
	}
}

func TestGroupBy_Dimensions(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeAudit(t, dir, "a", newFormat, now.Add(-1*time.Hour))
	writeAudit(t, dir, "b", oldFormat, now.Add(-1*time.Hour))
	samples, _, _ := Collect(dir, time.Time{})

	for _, dim := range []string{"agent", "vendor", "day", "week"} {
		gs, err := GroupBy(samples, dim)
		if err != nil || len(gs) == 0 {
			t.Fatalf("GroupBy(%s): %v groups=%d", dim, err, len(gs))
		}
	}
	gs, _ := GroupBy(samples, "agent")
	if len(gs) != 2 {
		t.Fatalf("agent groups=%d, want 2 (claude, codex)", len(gs))
	}
	if _, err := GroupBy(samples, "flavor"); err == nil {
		t.Fatal("unknown dimension must error")
	}
}

func TestPercentile_Exact(t *testing.T) {
	vals := []int64{10, 20, 30, 40}
	if p := percentile(vals, 50); p != 20 {
		t.Errorf("p50=%d, want 20", p)
	}
	if p := percentile(vals, 95); p != 40 {
		t.Errorf("p95=%d, want 40", p)
	}
	if p := percentile(nil, 50); p != 0 {
		t.Errorf("p50(empty)=%d, want 0", p)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/stats/ -v`
Expected: compile FAIL (package does not exist).

- [ ] **Step 3: Implement**

Create `internal/stats/stats.go`. The types are specified in **Interfaces** above (copy them verbatim). Implementation notes, all mandatory:

```go
// Package stats aggregates drydock's per-task audit artifacts into run
// metrics: outcome rates, duration and gate-wait percentiles, spend, and
// egress-widen frequency. Read-only over the audit dir, brokerd not needed.
package stats
```

- `Collect(dir, since)`:
  - `os.ReadDir(dir)`; for every `*.jsonl` whose `Info().ModTime()` is not before `since` (zero `since` means no filter): open via `audit.OpenRead`; on open/stat error increment `skipped` and continue.
  - Build the Sample: `last, ok := audit.LastResultFile(f)`, `meta := audit.ReadMetaFile(f)`, `m, hasM := audit.LastMetricsFile(f)`.
  - Outcome mapping (keep keys stable for JSON): no result row → `"running"`; `subtype=="interrupted"` → `"interrupted"`; `subtype=="push_failed"` → `"push_failed"`; `IsError` → `"error"`; else `"ok"`.
  - `DurationMs`/`HasDuration` from `last.DurationMs` and `audit.HasDuration(last, ok)`.
  - `Metered = !meta.Subscription`; `Auth` = `"subscription"`/`"api_key"` from the same bit; `CostUSD = last.TotalCostUSD` only when `Metered`.
  - Dimensions: prefer the metrics row (`m.Agent`, `m.Vendor`, `m.Auth`, `m.Repo`); when absent fall back to `audit.TaskAgent(path)` for Agent and `provider.VendorForAgent(agent)` (import `drydock/internal/provider`; ignore its error, empty vendor groups under `"(unknown)"`). Repo fallback is `""` → grouped as `"(unknown)"`.
  - A file that yields no result row and no meta still becomes a Sample (Outcome `"running"`); truly unreadable files count as skipped.
  - Orphan widens: count `*.widen.json` entries (mtime ≥ since) whose `<id>.jsonl` does not exist in the dir listing.
- `Summarize(samples)`:
  - `Outcomes` map of the outcome keys above; `Tasks = len(samples)`.
  - Duration percentiles over samples with `HasDuration`.
  - Egress/approval wait percentiles over samples with `HasMetrics` and the respective wait `> 0` (a zero wait means the gate never engaged: auto-approve or no widen).
  - `SpendUSD` = sum of `CostUSD` over `Metered` samples; `UnmeteredTasks` = count of `!Metered`.
  - `SpendPerDayUSD` = `SpendUSD / days` where `days = max(1, ceil(newestMTime.Sub(oldestMTime).Hours()/24))` over the sample set; 0 tasks → 0.
  - `Requests` = sum of `M.Requests` over `HasMetrics`; `WidenRequested`/`WidenApproved` = sums over `HasMetrics` (`WidenApproved` counts samples with `M.WidenOutcome == "approved"`); `PreMetricsTasks` = count of `!HasMetrics`.
- `GroupBy(samples, dim)`: key funcs — `agent`→Agent, `vendor`→Vendor, `repo`→Repo, `day`→`MTime.Format("2006-01-02")`, `week`→`fmt.Sprintf("%d-W%02d", y, w)` from `MTime.ISOWeek()`; empty key → `"(unknown)"`. Any other dim returns an error naming the valid set. Sort groups by key ascending; run `Summarize` per group.
- `percentile(vals []int64, p float64) int64`: sort a copy ascending; `idx := int(math.Ceil(p/100*float64(len(vals)))) - 1`, clamp to `[0, len-1]`; empty input → 0.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/stats/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/stats/
git commit -m "feat(stats): audit-dir aggregator (outcomes, percentiles, spend, widen counts, grouping)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 7: `drydock stats` CLI

**Files:**
- Create: `cmd/drydock/stats.go`
- Modify: `cmd/drydock/main.go` (dispatch case near `case "tasks":` ~line 125; usage() block: add the stats line right under the tasks line)
- Test: `cmd/drydock/stats_test.go` (create)

**Interfaces:**
- Consumes: `stats.Collect`, `stats.Summarize`, `stats.GroupBy`, `stats.Report`; `parseRetention` from prune.go (same package); `auditDir()` helper (same package, used by runTasks).
- Produces: `runStats(args []string)` wired to `case "stats":`, and a testable `writeStats(w io.Writer, dir string, since time.Duration, by string, asJSON bool) error`.

- [ ] **Step 1: Write the failing test**

Create `cmd/drydock/stats_test.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const statsNewFixture = `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","agent":"claude"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":60000,"total_cost_usd":0.10,"num_turns":4,"src":"broker"}
{"type":"metrics","src":"broker","task_id":"a","agent":"claude","vendor":"anthropic","auth":"api_key","repo":"github.com/o/r","stage_ms":{"preparing":5000,"running":60000,"pushing":800},"egress_gate_wait_ms":0,"approval_gate_wait_ms":30000,"requests":4,"diff_files":2,"diff_bytes":512,"cost_usd":0.10,"widen_requested":0,"widen_outcome":"none"}
`

const statsOldFixture = `{"type":"drydock_meta","subscription":true,"sensitive":false}
{"type":"drydock_task","agent":"codex"}
{"type":"result","subtype":"error","is_error":true,"duration_ms":5000,"total_cost_usd":0,"num_turns":0,"src":"broker"}
`

func statsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for id, content := range map[string]string{"a": statsNewFixture, "b": statsOldFixture} {
		p := filepath.Join(dir, id+".jsonl")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestWriteStats_Text(t *testing.T) {
	var buf bytes.Buffer
	if err := writeStats(&buf, statsDir(t), 30*24*time.Hour, "agent", false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"tasks: 2", "ok: 1", "error: 1", "$0.10", "claude", "codex", "1 task predates metrics"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "$0.00") {
		t.Errorf("unmetered subscription task rendered as $0:\n%s", out)
	}
}

func TestWriteStats_JSON(t *testing.T) {
	var buf bytes.Buffer
	if err := writeStats(&buf, statsDir(t), 30*24*time.Hour, "", true); err != nil {
		t.Fatal(err)
	}
	var rep map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, buf.String())
	}
	overall, _ := rep["overall"].(map[string]any)
	if overall == nil || overall["tasks"] != float64(2) {
		t.Fatalf("overall.tasks wrong: %v", rep)
	}
}

func TestWriteStats_BadDimension(t *testing.T) {
	var buf bytes.Buffer
	if err := writeStats(&buf, statsDir(t), 0, "flavor", false); err == nil {
		t.Fatal("unknown --by dimension must error")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/drydock/ -run TestWriteStats -v`
Expected: compile FAIL (`writeStats` undefined).

- [ ] **Step 3: Implement**

Create `cmd/drydock/stats.go`:

```go
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"drydock/internal/stats"
)

// runStats aggregates the audit dir into run metrics. Like `tasks`, it reads
// AUDIT_ROOT directly: brokerd does not need to be running.
func runStats(args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	since := fs.String("since", "30d", "window (e.g. 7d, 2w, 720h); 0 = everything")
	by := fs.String("by", "", "group by: agent | vendor | repo | day | week")
	asJSON := fs.Bool("json", false, "emit the report as JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: drydock stats [--since 30d] [--by agent|vendor|repo|day|week] [--json]

Aggregates recent runs from the audit dir: outcome rates, duration and
gate-wait percentiles, spend, and egress-widen frequency.`)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	d, err := parseRetention(*since)
	if err != nil {
		die("--since: %v", err)
	}
	if err := writeStats(os.Stdout, auditDir(), d, *by, *asJSON); err != nil {
		die("stats: %v", err)
	}
}

// writeStats is the testable core: collect, aggregate, render to w.
func writeStats(w io.Writer, dir string, since time.Duration, by string, asJSON bool) error {
	cutoff := time.Time{}
	if since > 0 {
		cutoff = time.Now().Add(-since)
	}
	samples, orphans, skipped := stats.Collect(dir, cutoff)
	rep := stats.Report{
		Since: cutoff, Overall: stats.Summarize(samples),
		OrphanWidens: orphans, SkippedFiles: skipped,
	}
	if by != "" {
		groups, err := stats.GroupBy(samples, by)
		if err != nil {
			return err
		}
		rep.Groups, rep.GroupBy = groups, by
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	renderStats(w, rep)
	return nil
}
```

`renderStats(w io.Writer, rep stats.Report)` in the same file, plain text. Required content and formats (the tests assert the quoted fragments):

- Header line: `tasks: N` plus the window (`since <date>` or `all time`).
- One line per outcome present, rate included: e.g. `  ok: 1 (50%)`, `  error: 1 (50%)`.
- Durations: `dur p50/p95: <shortDur>/<shortDur>` using the existing `shortDur(ms)` helper from tasks.go; print `-` when no sample has a duration.
- Gate waits: `approval wait p50/p95: .../...` and `egress wait p50/p95: .../...`; `-` when no engaged-gate samples.
- Spend: `spend: $%.2f total, $%.2f/day` over metered tasks; when `UnmeteredTasks > 0` append ` (+N unmetered subscription task(s))`. Never render unmetered as a dollar amount.
- Widen: `egress widens: N requested, N approved` plus `, N never ran (denied/cancelled at gate)` when `OrphanWidens > 0`.
- Footnotes when nonzero: `N task(s) predate metrics (timing columns partial)` and `N unreadable file(s) skipped`. The exact singular/plural spelling the test asserts is `1 task predates metrics`.
- With groups: a table headed by the dimension, one row per group: key, tasks, ok-rate, dur p50, approval-wait p50, spend. Use `fmt.Fprintf` fixed-width columns like runTasks does.

In `cmd/drydock/main.go`: add the dispatch case (keep alphabetical-ish placement next to `tasks`):

```go
	case "stats":
		runStats(os.Args[2:])
```

and the usage line right under the `drydock tasks` line:

```
  drydock stats [--since 30d] [--by DIM] [--json]   aggregate run metrics (outcomes, durations, gate waits, spend)
```

(Match the exact usage() column alignment of the neighboring lines.)

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/drydock/ -run TestWriteStats -v` then `go test ./cmd/drydock/`
Expected: PASS (main_test.go usage assertions may need the new line added if they enumerate commands; fix the test data, not the feature).

- [ ] **Step 5: Commit**

```bash
git add cmd/drydock/stats.go cmd/drydock/stats_test.go cmd/drydock/main.go
git commit -m "feat(cli): drydock stats: aggregate run metrics from the audit dir

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 8: Docs, changelog, roadmap, full verification

**Files:**
- Modify: `README.md` (command list: add `drydock stats` under the tasks/logs lines; find with `grep -n "drydock tasks" README.md`)
- Modify: `site/docs/submitting-tasks.md` (document the stats command where tasks/logs are documented, and the audit additions where the audit stream is described; find with `grep -n "drydock tasks\|jsonl\|audit" site/docs/*.md`)
- Modify: `docs/ROADMAP.md` (item 4.7 ~line 271)
- Modify: `CHANGELOG.md` (new Unreleased section at the top)
- Test: full suite

- [ ] **Step 1: README + site docs**

README: after the `drydock logs` line in the command list add:

```
- `drydock stats [--since 30d] [--by agent|vendor|repo|day|week] [--json]`:
  aggregate run metrics across tasks (outcome rates, duration and gate-wait
  percentiles, spend, egress-widen frequency), read straight from the audit
  dir.
```

(Adapt to the README's actual list formatting; keep one entry, same voice as neighbors.)

site/docs: in the page that documents `drydock tasks`/`drydock logs`, add a `drydock stats` subsection with the usage line, the flags, and two sentences: what it aggregates, and that pre-upgrade tasks show partial timings. Where the audit `.jsonl` stream is described, add: each stream event now carries `ts` (RFC 3339) on the submit stream, and the file ends with a broker-authored `{"type":"metrics","src":"broker"}` row (stage durations, gate waits, request count, spend, widen outcome); readers must take the last such row.

- [ ] **Step 2: ROADMAP 4.7**

Replace the `**4.7 Observability.**` bullet body with:

```
- **4.7 Observability.** *Landed.* The broker records a terminal
  broker-authored `metrics` row in each task's audit stream (stage
  durations, egress/approval gate waits, admitted request count, diff
  size, spend, widen outcome; `src:"broker"`, last-wins like the result
  row) and stamps stream events with `ts`. `drydock stats [--since]
  [--by agent|vendor|repo|day|week] [--json]` aggregates the audit dir
  into outcome rates, duration and gate-wait percentiles, spend (metered
  lanes only; subscription tasks reported as unmetered), and egress-widen
  frequency, including widen requests whose task never ran. Pre-upgrade
  audit files still aggregate (outcomes/durations/cost), with timings
  reported as absent.
```

Also delete the now-stale backlog entry `1. **4.7 Observability**: wants real multi-run usage first, ...` (~line 372) and replace with a short `(backlog empty; next items are event-driven or parked, below)` note if nothing else remains ranked.

- [ ] **Step 3: CHANGELOG**

Add at the top (below the header paragraph, above `## v0.6.4`):

```markdown
## Unreleased

### Added

- **Run metrics + `drydock stats` (roadmap 4.7).** Each task's audit stream
  now ends with a broker-authored `{"type":"metrics"}` row (stage durations,
  egress/approval gate waits, admitted request count, diff size, spend,
  egress-widen outcome), stream events carry an RFC 3339 `ts`, and the
  result row's `num_turns` now reports the gateway-admitted request count.
  `drydock stats [--since 30d] [--by agent|vendor|repo|day|week] [--json]`
  aggregates the audit dir into outcome rates, duration and gate-wait
  percentiles, spend, and egress-widen frequency, with graceful fallback for
  pre-upgrade audit files.
```

- [ ] **Step 4: Full verification**

Run, expecting every one to pass:

```bash
go test -race -count=1 ./...
go vet ./...
make lint
make redteam
```

Plus the docs currency guards specifically: `go test ./cmd/docs-build/`.

- [ ] **Step 5: Commit**

```bash
git add README.md site/docs/ docs/ROADMAP.md CHANGELOG.md
git commit -m "docs: drydock stats + audit metrics row (README, site docs, roadmap 4.7 landed, changelog)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 9: PR

- [ ] **Step 1: Push and open the PR** (no banner in the body):

```bash
git push -u origin observability-stats
gh pr create --title "feat: observability (4.7): audit metrics row + drydock stats" --body "..."
```

PR body: the spec/plan paths, a summary of the write side (metrics row, ts, num_turns) and read side (internal/stats, drydock stats CLI), the trust rule (broker-authored last-wins row), compatibility notes (old files degrade, no reader changes), and the verification commands run with their results.

- [ ] **Step 2: Request review** via the requesting-code-review skill. Review agents must be read-only or worktree-isolated (a review agent once deleted untracked files in the main tree).

---

## Self-Review Notes

- Spec coverage: recording (Tasks 2-4), gateway counter (Task 1), parser (Task 5), aggregator with all four dimensions + orphan widens + unmetered handling (Task 6), CLI with --since/--by/--json (Task 7), docs/roadmap/changelog (Task 8). The spec's "subscription spend never rendered as $0" rule is asserted by `TestWriteStats_Text`.
- The `egress.Config`/widening literal in Task 3's widen test and the `testBackend` helper in Task 1 are the two places where the plan defers to existing test constructions; both carry an explicit grep + "reuse the existing pattern" instruction rather than invented API.
- Type consistency: JSON keys in Task 2's `taskMetrics` match Task 5's `audit.Metrics` field tags one-to-one; Task 6 consumes `audit.Metrics` directly so no third mapping exists; Task 7's fixtures reuse the exact row shape from Task 2.
- `resumePush` gateShutdown early-return: the deferred metrics row writes with partial data there by design (next boot's resume supersedes it, last-wins).
