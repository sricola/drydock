# Aggregate Budget Cap Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a gateway-enforced ceiling on cross-task USD spend, per provider, over a configurable window, so a runaway loop of tasks can't drain an API key.

**Architecture:** A new in-memory `spendLedger` in `internal/gateway` tracks time-stamped per-vendor spend. The gateway's `admit()` rejects a request once a vendor's windowed spend reaches the cap; `meter()` feeds each metered delta into the ledger. brokerd seeds the rolling window at boot from the audit trail and pre-checks at task submission. Subscription vendors are excluded (bounded per-task by the existing request cap).

**Tech Stack:** Go, standard library only. Existing `internal/gateway`, `internal/config`, `internal/audit`, `internal/provider`, `cmd/brokerd`.

## Global Constraints

- Go standard library only; no new dependencies (repo has exactly 2 direct deps).
- TDD: every change is test-first. Run tests with `go test -race -count=1 ./...`.
- No em dashes in any prose/docs/commit messages; use commas, colons, or parentheses. Keep en dashes in ranges.
- Backward compatible: `aggregate_budget_usd` defaults to `0` (cap disabled); existing behavior unchanged when unset.
- Per-vendor scope; the USD cap applies only to `api_key`-mode vendors (`config.AuthMode(vendor) != "subscription"`).
- `gofmt` and `staticcheck` must be clean (CI runs `go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...`).

---

## File Structure

- `internal/gateway/spendledger.go` (new): the `spendLedger` unit. One responsibility: track and sum time-stamped per-vendor spend.
- `internal/gateway/spendledger_test.go` (new): its tests.
- `internal/gateway/gateway.go` (modify): add ledger + cap fields to `Gateway`, `SetAggregateCap`, `SeedAggregate`, `AggregateExceeded`; the `admit()` check; the `meter()` hook.
- `internal/gateway/gateway_test.go` (modify): admit/aggregate tests.
- `internal/config/config.go` (modify): `AggregateBudgetUSD` + `AggregateWindow` fields, defaults, env overrides, validation, SeedTemplate.
- `config/config.yaml` (modify): on-disk SeedTemplate mirror.
- `internal/config/config_test.go` (modify): validation test.
- `internal/audit/audit.go` (modify): add `TaskAgent(path)` accessor.
- `internal/audit/audit_test.go` (modify): its test.
- `cmd/brokerd/main.go` (modify): `seedAggregateFromAudit`, wire `SetAggregateCap` + seed + `b.AggregateExceeded` at boot.
- `cmd/brokerd/main_test.go` (modify): seed test.
- `internal/broker/broker.go` (modify): `AggregateExceeded` hook field on `Broker`; submit-time pre-check in `HandleTask`.
- `internal/broker/*_test.go` (modify): pre-check test.
- `site/docs/configuration.md`, `site/docs/daemon.md`, `CHANGELOG.md`, `docs/ROADMAP.md` (modify): docs.

---

### Task 1: `spendLedger` unit

**Files:**
- Create: `internal/gateway/spendledger.go`
- Test: `internal/gateway/spendledger_test.go`

**Interfaces:**
- Produces: `newSpendLedger(window time.Duration) *spendLedger`; `(*spendLedger).add(vendor string, usd float64, ts time.Time)`; `(*spendLedger).windowed(vendor string, now time.Time) float64`. `window == 0` means total mode (no time decay). It has its own mutex and is safe for concurrent use.

- [ ] **Step 1: Write the failing test**

```go
package gateway

import (
	"testing"
	"time"
)

func TestSpendLedger_RollingWindow(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	l := newSpendLedger(24 * time.Hour)
	l.add("anthropic", 1.0, now.Add(-48*time.Hour)) // outside window
	l.add("anthropic", 2.0, now.Add(-1*time.Hour))  // inside
	l.add("anthropic", 3.0, now.Add(-2*time.Hour))  // inside
	if got := l.windowed("anthropic", now); got != 5.0 {
		t.Errorf("windowed = %v, want 5.0 (old entry aged out)", got)
	}
}

func TestSpendLedger_TotalMode(t *testing.T) {
	now := time.Now()
	l := newSpendLedger(0) // total mode: no decay
	l.add("openai", 1.0, now.Add(-1000*time.Hour))
	l.add("openai", 2.5, now)
	if got := l.windowed("openai", now); got != 3.5 {
		t.Errorf("total-mode windowed = %v, want 3.5 (no decay)", got)
	}
}

func TestSpendLedger_PerVendorIsolation(t *testing.T) {
	now := time.Now()
	l := newSpendLedger(time.Hour)
	l.add("anthropic", 10.0, now)
	l.add("openai", 1.0, now)
	if got := l.windowed("openai", now); got != 1.0 {
		t.Errorf("openai windowed = %v, want 1.0 (anthropic must not bleed in)", got)
	}
	if got := l.windowed("google", now); got != 0 {
		t.Errorf("unknown vendor = %v, want 0", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gateway/ -run TestSpendLedger`
Expected: FAIL, `undefined: newSpendLedger`.

- [ ] **Step 3: Write minimal implementation**

```go
package gateway

import (
	"sync"
	"time"
)

// spendLedger tracks time-stamped USD spend per vendor for the aggregate budget
// cap. window == 0 is total mode (no time decay). It has its own mutex; callers
// may hold the Gateway mutex when calling in, but this lock is never acquired
// before the Gateway's, so there is no lock-order inversion.
type spendLedger struct {
	mu       sync.Mutex
	window   time.Duration
	byVendor map[string][]ledgerEntry
}

type ledgerEntry struct {
	ts  time.Time
	usd float64
}

func newSpendLedger(window time.Duration) *spendLedger {
	return &spendLedger{window: window, byVendor: map[string][]ledgerEntry{}}
}

func (l *spendLedger) add(vendor string, usd float64, ts time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.byVendor[vendor] = append(l.byVendor[vendor], ledgerEntry{ts: ts, usd: usd})
}

// windowed returns the vendor's spend within the window (or all of it in total
// mode) and prunes aged-out entries in place.
func (l *spendLedger) windowed(vendor string, now time.Time) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := l.byVendor[vendor]
	if l.window == 0 {
		var sum float64
		for _, e := range entries {
			sum += e.usd
		}
		return sum
	}
	cutoff := now.Add(-l.window)
	kept := entries[:0]
	var sum float64
	for _, e := range entries {
		if e.ts.After(cutoff) {
			kept = append(kept, e)
			sum += e.usd
		}
	}
	l.byVendor[vendor] = kept
	return sum
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/gateway/ -run TestSpendLedger`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/spendledger.go internal/gateway/spendledger_test.go
git commit -m "feat(gateway): spendLedger for the aggregate budget cap"
```

---

### Task 2: Config fields, validation, env overrides, SeedTemplate

**Files:**
- Modify: `internal/config/config.go`
- Modify: `config/config.yaml`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `Config.AggregateBudgetUSD float64` (yaml `aggregate_budget_usd`, default `0`), `Config.AggregateWindow time.Duration` (yaml `aggregate_window`, default `24h`). Env overrides `DRYDOCK_AGGREGATE_BUDGET_USD`, `DRYDOCK_AGGREGATE_WINDOW`.

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go`:

```go
func TestValidate_RejectsNegativeAggregate(t *testing.T) {
	for _, yaml := range []string{
		"network: x\ngateway_ip: 1.2.3.4\naggregate_budget_usd: -1\n",
		"network: x\ngateway_ip: 1.2.3.4\naggregate_window: -5m\n",
	} {
		path := filepath.Join(t.TempDir(), "c.yaml")
		os.WriteFile(path, []byte(yaml), 0o644)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "aggregate") {
			t.Errorf("yaml=%q want aggregate rejection, got %v", yaml, err)
		}
	}
}

func TestAggregateDefaults(t *testing.T) {
	d := Defaults()
	if d.AggregateBudgetUSD != 0 {
		t.Errorf("aggregate_budget_usd default = %v, want 0 (disabled)", d.AggregateBudgetUSD)
	}
	if d.AggregateWindow != 24*time.Hour {
		t.Errorf("aggregate_window default = %v, want 24h", d.AggregateWindow)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run 'TestValidate_RejectsNegativeAggregate|TestAggregateDefaults'`
Expected: FAIL, `d.AggregateBudgetUSD undefined`.

- [ ] **Step 3a: Add the struct fields**

In `internal/config/config.go`, in the `Config` struct right after `TaskMaxRequests`:

```go
	// AggregateBudgetUSD caps cross-task USD spend per api_key-mode provider over
	// AggregateWindow. 0 (default) disables the aggregate cap. Subscription
	// vendors are out of scope (bounded per-task by TaskMaxRequests).
	AggregateBudgetUSD float64 `yaml:"aggregate_budget_usd"`
	// AggregateWindow is the rolling window for AggregateBudgetUSD. 0 means a
	// total since brokerd boot (session cap, no time decay, resets on restart).
	AggregateWindow time.Duration `yaml:"aggregate_window"`
```

- [ ] **Step 3b: Add the default**

In `Defaults()`, after `TaskMaxRequests: 0,`:

```go
		AggregateBudgetUSD: 0,
		AggregateWindow:    24 * time.Hour,
```

- [ ] **Step 3c: Add validation**

In `validate()`, before the final `return nil`:

```go
	if c.AggregateBudgetUSD < 0 {
		return fmt.Errorf("config: aggregate_budget_usd must be >= 0, got %v", c.AggregateBudgetUSD)
	}
	if c.AggregateWindow < 0 {
		return fmt.Errorf("config: aggregate_window must be >= 0, got %v", c.AggregateWindow)
	}
```

- [ ] **Step 3d: Add env overrides**

In the env-override block (near `DRYDOCK_TASK_BUDGET_USD`), add:

```go
	if v := os.Getenv("DRYDOCK_AGGREGATE_BUDGET_USD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			c.AggregateBudgetUSD = f
		}
	}
	if v := os.Getenv("DRYDOCK_AGGREGATE_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			c.AggregateWindow = d
		}
	}
```

- [ ] **Step 3e: Document in SeedTemplate + the on-disk mirror**

In `SeedTemplate` (config.go), under the per-task limits section after the `task_max_requests` line, add:

```
aggregate_budget_usd:   0              # cross-task USD ceiling per api_key provider over aggregate_window; 0 = disabled. subscription is out of scope (bounded per task by task_max_requests)
aggregate_window:       24h            # rolling window for aggregate_budget_usd; 0 = total since brokerd boot (resets on restart)
```

Make the identical addition at the same spot in `config/config.yaml`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/`
Expected: PASS (including the existing `TestSeedTemplate_MatchesOnDiskTemplate`, which proves the mirror stayed in sync).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go config/config.yaml internal/config/config_test.go
git commit -m "feat(config): aggregate_budget_usd + aggregate_window fields"
```

---

### Task 3: Gateway aggregate cap (fields, admit check, meter hook, accessors)

**Files:**
- Modify: `internal/gateway/gateway.go`
- Test: `internal/gateway/gateway_test.go`

**Interfaces:**
- Consumes: `newSpendLedger` from Task 1.
- Produces: `(*Gateway).SetAggregateCap(budgetUSD float64, window time.Duration, vendors []string)` (boot-only); `(*Gateway).SeedAggregate(vendor string, usd float64, ts time.Time)`; `(*Gateway).AggregateExceeded(vendor string) bool`. `admit` returns `402` when a vendor's windowed spend reaches the cap.

- [ ] **Step 1: Write the failing test**

Append to `internal/gateway/gateway_test.go`:

```go
func TestGateway_AggregateCap(t *testing.T) {
	g, err := New(testBackend("anthropic"), testBackend("openai"))
	if err != nil {
		t.Fatal(err)
	}
	g.SetAggregateCap(5.0, time.Hour, []string{"anthropic", "openai"})
	now := time.Now()

	// anthropic at the cap: a fresh lease's request must be refused with 402.
	g.SeedAggregate("anthropic", 5.0, now)
	tok, _ := g.Mint("anthropic", 100.0, 0, time.Hour) // generous per-task budget
	if _, code := g.admit(tok); code != http.StatusPaymentRequired {
		t.Errorf("anthropic over aggregate cap: admit code = %d, want 402", code)
	}

	// openai is under its own cap: must still admit (per-vendor isolation).
	tok2, _ := g.Mint("openai", 100.0, 0, time.Hour)
	if _, code := g.admit(tok2); code != 0 {
		t.Errorf("openai under aggregate cap: admit code = %d, want 0 (admitted)", code)
	}

	if !g.AggregateExceeded("anthropic") {
		t.Error("AggregateExceeded(anthropic) = false, want true")
	}
	if g.AggregateExceeded("openai") {
		t.Error("AggregateExceeded(openai) = true, want false")
	}
}

func TestGateway_AggregateCap_DisabledByDefault(t *testing.T) {
	g, _ := New(testBackend("anthropic"))
	// No SetAggregateCap call: cap disabled.
	tok, _ := g.Mint("anthropic", 100.0, 0, time.Hour)
	if _, code := g.admit(tok); code != 0 {
		t.Errorf("cap disabled: admit code = %d, want 0", code)
	}
	if g.AggregateExceeded("anthropic") {
		t.Error("AggregateExceeded with cap disabled = true, want false")
	}
}
```

Note: use the test's existing backend/mint helpers. If `testBackend` does not exist, mirror the construction used by the other tests in `gateway_test.go` (they build a `Backend` with a fake `Cred` and vendor). Keep the helper name consistent with what is already there.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gateway/ -run TestGateway_AggregateCap`
Expected: FAIL, `g.SetAggregateCap undefined`.

- [ ] **Step 3a: Add fields to the Gateway struct**

In `internal/gateway/gateway.go`, extend `Gateway`:

```go
type Gateway struct {
	mu      sync.Mutex
	leases  map[string]*Lease
	vendors map[string]vendorRT
	proxy   *httputil.ReverseProxy

	// Aggregate budget cap (0 aggBudget = disabled). Set once at boot via
	// SetAggregateCap, before any request is served.
	ledger     *spendLedger
	aggBudget  float64
	aggVendors map[string]bool
}
```

In `New`, initialize the ledger so it is never nil:

```go
	g := &Gateway{leases: map[string]*Lease{}, vendors: map[string]vendorRT{},
		ledger: newSpendLedger(0), aggVendors: map[string]bool{}}
```

- [ ] **Step 3b: Add the cap methods**

```go
// SetAggregateCap enables the per-vendor aggregate USD cap. Call once at boot
// before serving. vendors is the set the cap applies to (api_key-mode only).
func (g *Gateway) SetAggregateCap(budgetUSD float64, window time.Duration, vendors []string) {
	g.aggBudget = budgetUSD
	g.aggVendors = map[string]bool{}
	for _, v := range vendors {
		g.aggVendors[v] = true
	}
	g.ledger = newSpendLedger(window)
}

// SeedAggregate adds historical spend to the ledger (boot seed from the audit
// trail). No-op when the cap is disabled.
func (g *Gateway) SeedAggregate(vendor string, usd float64, ts time.Time) {
	if g.aggBudget <= 0 {
		return
	}
	g.ledger.add(vendor, usd, ts)
}

// AggregateExceeded reports whether vendor is at or over its aggregate cap. Used
// by the broker for a submit-time pre-check. False when the cap is disabled or
// vendor is out of scope (e.g. subscription).
func (g *Gateway) AggregateExceeded(vendor string) bool {
	return g.aggBudget > 0 && g.aggVendors[vendor] &&
		g.ledger.windowed(vendor, time.Now()) >= g.aggBudget
}
```

- [ ] **Step 3c: Add the check to `admit`**

In `admit`, after the `MaxRequests` check and before `l.Requests++`:

```go
	if g.aggBudget > 0 && g.aggVendors[l.Vendor] &&
		g.ledger.windowed(l.Vendor, time.Now()) >= g.aggBudget {
		return nil, http.StatusPaymentRequired
	}
```

(This runs while holding `g.mu`; `ledger.windowed` takes only the ledger mutex, so the lock order is always Gateway-then-ledger, never reversed.)

- [ ] **Step 3d: Feed metered spend into the ledger**

In `meter`, change the `onDone` closure to capture the delta and record it:

```go
	resp.Body = &usageReader{rc: resp.Body, onDone: func(body []byte) {
		if model, in, out, ok := vt.v.ParseUsage(body, ct); ok {
			delta := cost(vt.v.Prices, model, in, out)
			g.mu.Lock()
			rc.lease.SpentUSD += delta
			g.mu.Unlock()
			if g.aggBudget > 0 {
				g.ledger.add(rc.lease.Vendor, delta, time.Now())
			}
		}
	}}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/gateway/`
Expected: PASS (all gateway tests, including the existing ones).

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/gateway.go internal/gateway/gateway_test.go
git commit -m "feat(gateway): enforce the aggregate budget cap in admit"
```

---

### Task 4: Audit `TaskAgent` accessor + boot seed from the audit trail

**Files:**
- Modify: `internal/audit/audit.go`
- Test: `internal/audit/audit_test.go`
- Modify: `cmd/brokerd/main.go`
- Test: `cmd/brokerd/main_test.go`

**Interfaces:**
- Consumes: `SeedAggregate`, `SetAggregateCap` from Task 3; `audit.ReadMeta`, `audit.LastResult` (existing); `provider.VendorForAgent` (existing).
- Produces: `audit.TaskAgent(path string) string` (the `drydock_task` line's agent, or ""); `seedAggregateFromAudit(gw *gateway.Gateway, auditRoot string, window time.Duration, defaultAgent string)`.

- [ ] **Step 1: Write the failing test (audit accessor)**

Append to `internal/audit/audit_test.go`:

```go
func TestTaskAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	os.WriteFile(path, []byte(
		`{"type":"drydock_meta","subscription":false,"sensitive":false}`+"\n"+
			`{"type":"drydock_task","repo_ref":"r","instruction":"i","agent":"codex"}`+"\n"+
			`{"type":"result","subtype":"success","total_cost_usd":0.5}`+"\n"), 0o600)
	if got := TaskAgent(path); got != "codex" {
		t.Errorf("TaskAgent = %q, want codex", got)
	}
	// No drydock_task line -> "".
	p2 := filepath.Join(dir, "old.jsonl")
	os.WriteFile(p2, []byte(`{"type":"result","subtype":"success"}`+"\n"), 0o600)
	if got := TaskAgent(p2); got != "" {
		t.Errorf("TaskAgent(no task line) = %q, want empty", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/audit/ -run TestTaskAgent`
Expected: FAIL, `undefined: TaskAgent`.

- [ ] **Step 3: Implement `TaskAgent`**

Append to `internal/audit/audit.go`:

```go
// taskLine is the {"type":"drydock_task",...} invocation record.
type taskLine struct {
	Type  string `json:"type"`
	Agent string `json:"agent"`
}

// TaskAgent returns the agent recorded in path's drydock_task line, or "" if
// absent (a pre-v0.6.0 trace) or unreadable. Opened O_NOFOLLOW like the other
// audit reads.
func TaskAgent(path string) string {
	f, err := openRead(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		if !bytes.Contains(sc.Bytes(), []byte(`"drydock_task"`)) {
			continue
		}
		var tl taskLine
		if json.Unmarshal(sc.Bytes(), &tl) == nil && tl.Type == "drydock_task" {
			return tl.Agent
		}
	}
	return ""
}
```

- [ ] **Step 4: Run the audit test to verify it passes**

Run: `go test ./internal/audit/ -run TestTaskAgent`
Expected: PASS.

- [ ] **Step 5: Write the failing test (boot seed)**

Append to `cmd/brokerd/main_test.go`:

```go
func TestSeedAggregateFromAudit(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name, meta, task string, cost float64) {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(meta+"\n"+task+"\n"+
			fmt.Sprintf(`{"type":"result","subtype":"success","total_cost_usd":%.4f}`, cost)+"\n"), 0o600)
		os.Chtimes(p, now, now) // in window
	}
	write("a.jsonl",
		`{"type":"drydock_meta","subscription":false,"sensitive":false}`,
		`{"type":"drydock_task","agent":"claude"}`, 2.0)
	write("b.jsonl",
		`{"type":"drydock_meta","subscription":true,"sensitive":false}`, // subscription: excluded
		`{"type":"drydock_task","agent":"claude"}`, 9.0)

	gw, _ := gateway.New(testBackendFor(t, "anthropic"))
	gw.SetAggregateCap(100.0, 24*time.Hour, []string{"anthropic"})
	seedAggregateFromAudit(gw, dir, 24*time.Hour, "claude")

	if gw.AggregateExceeded("anthropic") { // 2.0 seeded, cap 100 -> not exceeded
		t.Fatal("unexpectedly exceeded")
	}
	gw.SeedAggregate("anthropic", 98.0, now) // total now 100 -> at cap
	if !gw.AggregateExceeded("anthropic") {
		t.Error("want exceeded after seeding to 100 (only the non-subscription 2.0 should have counted from audit)")
	}
}
```

Note: `testBackendFor` mirrors whatever backend helper `cmd/brokerd/main_test.go` (or the gateway tests) already use; anthropic maps to vendor "anthropic".

- [ ] **Step 6: Run test to verify it fails**

Run: `go test ./cmd/brokerd/ -run TestSeedAggregateFromAudit`
Expected: FAIL, `undefined: seedAggregateFromAudit`.

- [ ] **Step 7: Implement `seedAggregateFromAudit` and wire it at boot**

Add to `cmd/brokerd/main.go` (near `pruneOrphanTasks`):

```go
// seedAggregateFromAudit primes the gateway's rolling aggregate ledger from the
// audit trail so the cap survives a brokerd restart. Only non-subscription
// tasks with a determinable vendor and a positive cost, whose trace mtime is
// within the window, are counted.
func seedAggregateFromAudit(gw *gateway.Gateway, auditRoot string, window time.Duration, defaultAgent string) {
	cutoff := time.Now().Add(-window)
	entries, err := os.ReadDir(auditRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			continue
		}
		path := filepath.Join(auditRoot, e.Name())
		if audit.ReadMeta(path).Subscription {
			continue // subscription is out of scope for the USD cap
		}
		res, ok := audit.LastResult(path, info.Size())
		if !ok || res.TotalCostUSD <= 0 {
			continue
		}
		agent := audit.TaskAgent(path)
		if agent == "" {
			agent = defaultAgent
		}
		vendor, ok := provider.VendorForAgent(agent)
		if !ok {
			continue
		}
		gw.SeedAggregate(vendor, res.TotalCostUSD, info.ModTime())
	}
}
```

Then, in `main()` after the provider-building loop and `gw` is available (right after the `slog.Info("agents available", ...)` line), add:

```go
	if cfg.AggregateBudgetUSD > 0 {
		var apiKeyVendors []string
		for _, b := range backends {
			if cfg.AuthMode(b.Vendor.Name) != "subscription" {
				apiKeyVendors = append(apiKeyVendors, b.Vendor.Name)
			}
		}
		gw.SetAggregateCap(cfg.AggregateBudgetUSD, cfg.AggregateWindow, apiKeyVendors)
		if cfg.AggregateWindow > 0 {
			seedAggregateFromAudit(gw, cfg.AuditRoot, cfg.AggregateWindow, cfg.DefaultAgent)
		}
		slog.Info("aggregate budget cap enabled",
			"usd", cfg.AggregateBudgetUSD, "window", cfg.AggregateWindow, "vendors", apiKeyVendors)
	}
```

Ensure `cmd/brokerd/main.go` imports `drydock/internal/audit` (add if absent).

- [ ] **Step 8: Run tests to verify they pass**

Run: `go test -race ./cmd/brokerd/ ./internal/audit/`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/audit/audit.go internal/audit/audit_test.go cmd/brokerd/main.go cmd/brokerd/main_test.go
git commit -m "feat(brokerd): seed the aggregate cap ledger from the audit trail at boot"
```

---

### Task 5: Broker submit-time pre-check

**Files:**
- Modify: `internal/broker/broker.go`
- Test: `internal/broker/broker_test.go` (or the existing broker test file that drives `HandleTask`)
- Modify: `cmd/brokerd/main.go` (wire the hook)

**Interfaces:**
- Consumes: `AggregateExceeded` from Task 3.
- Produces: `Broker.AggregateExceeded func(vendor string) bool` (nil = no pre-check). When non-nil and it returns true for the task's vendor, `HandleTask` rejects the submission before starting the task.

- [ ] **Step 1: Write the failing test**

Append to the broker test file that already constructs a `Broker` and calls `HandleTask` (follow that file's existing setup/fakes):

```go
func TestHandleTask_AggregatePreCheck(t *testing.T) {
	b := newTestBroker(t) // existing helper that wires fakes (prepareStage/runAgent/etc.)
	b.AggregateExceeded = func(vendor string) bool { return true } // vendor over cap

	rr := postTask(t, b, taskJSON(`{"repo_ref":"https://github.com/o/r","instruction":"x"}`))
	if rr.Code != http.StatusPaymentRequired {
		t.Fatalf("over aggregate cap: status = %d, want 402", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "running") {
		t.Error("task should not have started when the aggregate cap is exhausted")
	}
}
```

Note: `newTestBroker`, `postTask`, and `taskJSON` stand in for the existing helpers in the broker test file. Use the real ones; the point is a `Broker` with `AggregateExceeded` returning true must 402 before staging.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/broker/ -run TestHandleTask_AggregatePreCheck`
Expected: FAIL, `b.AggregateExceeded undefined`.

- [ ] **Step 3a: Add the hook field**

In `internal/broker/broker.go`, add to the `Broker` struct (near the other optional hooks):

```go
	// AggregateExceeded, when set, is consulted at task submission: if it
	// returns true for the task's vendor, the submission is rejected (402)
	// before any VM starts. nil disables the pre-check. Wired to the gateway's
	// AggregateExceeded by brokerd.
	AggregateExceeded func(vendor string) bool
```

- [ ] **Step 3b: Add the pre-check in `HandleTask`**

In `HandleTask`, after the agent name and `taskVendor` are resolved (where `provider.VendorForAgent(agentName)` is called) and before the sandbox/lease work begins, add:

```go
	if b.AggregateExceeded != nil && taskVendor != "" && b.AggregateExceeded(taskVendor) {
		sw.emit(errorEvent(taskID, "aggregate budget exhausted for "+taskVendor+
			" (raise aggregate_budget_usd or wait for the window to clear)", ""))
		http.Error(w, "aggregate budget exhausted", http.StatusPaymentRequired)
		return
	}
```

Match the surrounding code for how `sw`, `taskID`, `w`, and `errorEvent` are named/used in `HandleTask`; place this after `taskVendor` is known but before staging/minting.

- [ ] **Step 3c: Wire the hook in brokerd**

In `cmd/brokerd/main.go`, where the `broker.Broker{...}` is constructed (or right after), set the hook when the cap is enabled:

```go
	if cfg.AggregateBudgetUSD > 0 {
		b.AggregateExceeded = gw.AggregateExceeded
	}
```

(Use the actual broker variable name at that construction site.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/broker/ ./cmd/brokerd/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/broker/broker.go internal/broker/broker_test.go cmd/brokerd/main.go
git commit -m "feat(broker): submit-time pre-check against the aggregate budget cap"
```

---

### Task 6: Docs + roadmap

**Files:**
- Modify: `site/docs/configuration.md`
- Modify: `site/docs/daemon.md`
- Modify: `CHANGELOG.md`
- Modify: `docs/ROADMAP.md`

- [ ] **Step 1: Document the config fields**

In `site/docs/configuration.md`, in the per-task-limits table/section, add rows for `aggregate_budget_usd` (cross-task USD ceiling per api_key provider over the window; 0 disables; subscription out of scope) and `aggregate_window` (rolling window; 0 = total since boot). No em dashes.

- [ ] **Step 2: Update the daemon caveat**

In `site/docs/daemon.md`, replace the "no aggregate spend cap yet" caveat with the enabled feature: set `aggregate_budget_usd` (and optionally `aggregate_window`) to bound cross-task spend per provider; note subscription is bounded per-task by `task_max_requests`.

- [ ] **Step 3: CHANGELOG**

Add an `## Unreleased` section (above the latest version) with an Added entry for the aggregate budget cap, describing the config fields, per-vendor scope, rolling/total modes, audit-seeded persistence, and the subscription exclusion. No em dashes.

- [ ] **Step 4: Mark ROADMAP 4.3 landed**

In `docs/ROADMAP.md`, change 4.3 from "Partial" to "Landed", and remove it from the ranked backlog (renumber). Update the "Done when" note if needed.

- [ ] **Step 5: Regenerate docs + verify**

Run: `make docs && go test ./... && grep -rn '—' site/docs/configuration.md site/docs/daemon.md CHANGELOG.md docs/ROADMAP.md`
Expected: `make docs OK`, tests PASS, grep returns nothing (no em dashes).

- [ ] **Step 6: Commit**

```bash
git add site/docs/configuration.md site/docs/daemon.md CHANGELOG.md docs/ROADMAP.md
git commit -m "docs: aggregate budget cap (ROADMAP 4.3 landed)"
```

---

## Notes for the implementer

- The gateway lock order is always Gateway-mutex then ledger-mutex (in `admit`/`meter`); `AggregateExceeded` and the boot seed take only the ledger mutex. Never call a ledger method that would then try to take the Gateway mutex.
- `config.AuthMode` only special-cases anthropic/openai; every other vendor (google, the openai_compat lane) returns "" and is therefore treated as api_key (correct: those are api-key only), so it is included in `apiKeyVendors`.
- `total_cost_usd` is present for api_key tasks (claude writes its own result line; the broker synthesizes one for other agents from `grant.Spent()`), so the audit seed has real costs to sum.
- Run the full `go test -race ./...` plus `gofmt -l` and `staticcheck ./...` before opening the PR; staticcheck is a CI gate.
