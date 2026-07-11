# Precise Gateway Metering Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bound the per-task USD budget under concurrency by reserving a per-request cost ceiling at admission and reconciling it at completion, closing the concurrent-bypass hole. Off by default.

**Architecture:** The `Lease` gains a per-request reservation `R` (`MaxRequestCostUSD`) and a `Reserved` running sum. `admit` rejects when `SpentUSD + Reserved + R > BudgetUSD` and reserves `R`; `meter`'s `onDone` releases `R` and commits the actual metered cost. Config-gated (`max_request_cost_usd`, default 0 = disabled = current post-hoc behavior).

**Tech Stack:** Go, standard library only. Packages `internal/gateway`, `internal/config`, `cmd/brokerd`.

## Global Constraints

- Go standard library only; no new dependencies.
- TDD: test-first. `go test -race -count=1 ./...`.
- No em dashes (—) in code, comments, docs, or commit messages; use commas, colons, or parentheses.
- gofmt + staticcheck clean.
- Backward compatible: `max_request_cost_usd` defaults to `0` = reservation disabled = exact current behavior.
- `Reserved` can only make the budget stricter, never looser (fail-closed); both `Reserved` and `SpentUSD` are guarded by `g.mu`.

---

## File Structure

- `internal/config/config.go` + `config/config.yaml` (modify): `MaxRequestCostUSD` field.
- `internal/config/config_test.go` (modify): its validation test.
- `internal/gateway/gateway.go` (modify): `Lease.MaxRequestCostUSD` + `Lease.Reserved`; `Mint` param; `admit` reservation gate; `meter` onDone reconciliation.
- `internal/gateway/gateway_test.go` (modify): admit + meter reservation tests.
- `internal/gateway/provider.go` (modify): `Provider.MaxRequestCost` + pass through `Mint`.
- `cmd/brokerd/main.go` (modify): wire `MaxRequestCost` from config.
- `site/docs/configuration.md`, `CHANGELOG.md`, `docs/ROADMAP.md` (modify): docs.

---

### Task 1: Config field `max_request_cost_usd`

**Files:**
- Modify: `internal/config/config.go`, `config/config.yaml`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `Config.MaxRequestCostUSD float64` (yaml `max_request_cost_usd`, default `0`). Env `DRYDOCK_MAX_REQUEST_COST_USD`.

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go`:

```go
func TestMaxRequestCost_DefaultAndValidation(t *testing.T) {
	if d := Defaults(); d.MaxRequestCostUSD != 0 {
		t.Errorf("max_request_cost_usd default = %v, want 0 (disabled)", d.MaxRequestCostUSD)
	}
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte("network: x\ngateway_ip: 1.2.3.4\nmax_request_cost_usd: -1\n"), 0o644)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "max_request_cost_usd") {
		t.Errorf("negative max_request_cost_usd should be rejected, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestMaxRequestCost_DefaultAndValidation`
Expected: FAIL, `d.MaxRequestCostUSD undefined`.

- [ ] **Step 3: Implement** (mirror the `AggregateBudgetUSD` pattern exactly)

Struct field (after `TaskMaxRequests`, near the other per-task limits):

```go
	// MaxRequestCostUSD is the worst-case USD a single request may cost,
	// reserved against the lease budget while the request is in flight so
	// concurrent requests cannot all admit at spend=0. 0 (default) disables the
	// reservation (post-hoc metering only).
	MaxRequestCostUSD float64 `yaml:"max_request_cost_usd"`
```

`Defaults()`: `MaxRequestCostUSD: 0,` (after `TaskMaxRequests: 0,`).

`validate()` (with the other `>= 0` checks):

```go
	if c.MaxRequestCostUSD < 0 {
		return fmt.Errorf("config: max_request_cost_usd must be >= 0, got %v", c.MaxRequestCostUSD)
	}
```

Env override (near `DRYDOCK_TASK_BUDGET_USD`):

```go
	if v := os.Getenv("DRYDOCK_MAX_REQUEST_COST_USD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			c.MaxRequestCostUSD = f
		}
	}
```

SeedTemplate (after the `task_max_requests` line) AND the identical line in `config/config.yaml`:

```
max_request_cost_usd:   0              # worst-case USD reserved per in-flight request so concurrent requests can't admit past the budget; 0 = disabled (post-hoc metering only)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/`
Expected: PASS (including `TestSeedTemplate_MatchesOnDiskTemplate`).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go config/config.yaml internal/config/config_test.go
git commit -m "feat(config): max_request_cost_usd for in-flight reservation"
```

---

### Task 2: Lease fields + `Mint` param + Provider

**Files:**
- Modify: `internal/gateway/gateway.go`, `internal/gateway/provider.go`
- Test: `internal/gateway/gateway_test.go` (update Mint call sites)

**Interfaces:**
- Produces: `Lease.MaxRequestCostUSD float64`, `Lease.Reserved float64`; `Gateway.Mint(vendor string, budgetUSD float64, maxRequests int, maxRequestCostUSD float64, ttl time.Duration) (string, error)`; `Provider.MaxRequestCost float64`.

- [ ] **Step 1: Update the Lease + Mint (compile-driven)**

Add to `Lease` (gateway.go:24-36):

```go
	MaxRequestCostUSD float64 // per-request reservation R (0 = disabled)
	Reserved          float64 // sum of R for admitted-but-unmetered requests; guarded by g.mu
```

Change `Mint` (gateway.go:81) to take `maxRequestCostUSD float64` (insert before `ttl`) and write it into the lease:

```go
func (g *Gateway) Mint(vendor string, budgetUSD float64, maxRequests int, maxRequestCostUSD float64, ttl time.Duration) (string, error) {
	// ... existing token gen ...
	g.leases[tok] = &Lease{Token: tok, Vendor: vendor, BudgetUSD: budgetUSD,
		MaxRequests: maxRequests, MaxRequestCostUSD: maxRequestCostUSD,
		Expiry: time.Now().Add(ttl)}
	// ...
}
```

- [ ] **Step 2: Update Provider + all Mint callers to compile**

In `provider.go`: add `MaxRequestCost float64` to `Provider`, and pass it in `Provider.Mint` where it calls `p.GW.Mint(p.Vendor, b, p.MaxRequests, ttl)` -> `p.GW.Mint(p.Vendor, b, p.MaxRequests, p.MaxRequestCost, ttl)`.

Update EVERY other `Mint(` call site (mostly `internal/gateway/gateway_test.go`, e.g. `g.Mint("anthropic", 100.0, 0, time.Hour)`) to pass the new arg: `g.Mint("anthropic", 100.0, 0, 0, time.Hour)` (0 = reservation off, preserving existing test behavior). Grep for `.Mint(` across `internal/` and `cmd/` and update all.

- [ ] **Step 3: Run to verify it compiles + existing tests pass**

Run: `go build ./... && go test ./internal/gateway/`
Expected: PASS (existing gateway tests green with the added-but-zero reservation arg; this task adds no new behavior yet).

- [ ] **Step 4: Commit**

```bash
git add internal/gateway/gateway.go internal/gateway/provider.go internal/gateway/gateway_test.go
git commit -m "feat(gateway): Lease reservation fields + Mint maxRequestCost param"
```

---

### Task 3: `admit` reservation gate

**Files:**
- Modify: `internal/gateway/gateway.go`
- Test: `internal/gateway/gateway_test.go`

**Interfaces:**
- Consumes: `Lease.MaxRequestCostUSD`, `Lease.Reserved` (Task 2).

- [ ] **Step 1: Write the failing test**

Append to `internal/gateway/gateway_test.go` (use the existing backend/test helpers; mint with a small budget + a reservation):

```go
func TestAdmit_InFlightReservationBounds(t *testing.T) {
	g, _ := New(Backend{Vendor: AnthropicVendor(), Cred: StaticKey("k")})
	// Budget 1.0, per-request reservation 0.6: the first admit reserves 0.6,
	// a second concurrent admit (before any meter) would need 0.6+0.6=1.2 > 1.0
	// and must be rejected.
	tok, _ := g.Mint("anthropic", 1.0, 0, 0.6, time.Hour)
	if _, code := g.admit(tok); code != 0 {
		t.Fatalf("first admit code = %d, want 0", code)
	}
	if _, code := g.admit(tok); code != http.StatusPaymentRequired {
		t.Errorf("second concurrent admit code = %d, want 402 (reservation bound)", code)
	}
}

func TestAdmit_NoReservationWhenDisabled(t *testing.T) {
	g, _ := New(Backend{Vendor: AnthropicVendor(), Cred: StaticKey("k")})
	tok, _ := g.Mint("anthropic", 1.0, 0, 0, time.Hour) // R=0 disables reservation
	for i := 0; i < 5; i++ {
		if _, code := g.admit(tok); code != 0 {
			t.Fatalf("admit %d code = %d, want 0 (R=0, no reservation)", i, code)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gateway/ -run TestAdmit_InFlightReservation`
Expected: FAIL (the second admit currently passes, since nothing reserves).

- [ ] **Step 3: Implement**

In `admit`, after the `if l.SpentUSD >= l.BudgetUSD { return nil, 402 }` check, add the reject gate; and after ALL checks pass (right before/with `l.Requests++`), add the reserve:

```go
	if l.SpentUSD >= l.BudgetUSD {
		return nil, http.StatusPaymentRequired
	}
	if l.MaxRequestCostUSD > 0 &&
		l.SpentUSD+l.Reserved+l.MaxRequestCostUSD > l.BudgetUSD {
		return nil, http.StatusPaymentRequired
	}
	// ... existing MaxRequests (429) and aggregate-cap (402) checks unchanged ...
	l.Requests++
	if l.MaxRequestCostUSD > 0 {
		l.Reserved += l.MaxRequestCostUSD
	}
	return l, 0
```

The reserve happens only after every check passes (a request rejected by the request cap or aggregate cap does not reserve). All under the existing `g.mu`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/gateway/`
Expected: PASS (new + existing).

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/gateway.go internal/gateway/gateway_test.go
git commit -m "feat(gateway): admit reserves the per-request cost ceiling"
```

---

### Task 4: `meter` onDone reconciliation

**Files:**
- Modify: `internal/gateway/gateway.go`
- Test: `internal/gateway/gateway_test.go`

**Interfaces:**
- Consumes: the reservation from Task 3.

- [ ] **Step 1: Write the failing test**

The reservation must be released at stream end so a completed request frees its hold, and a request whose stream yielded NO usage still releases. Drive `meter` end-to-end via the gateway's HTTP path (mirror the existing metering tests in `gateway_test.go` that push an SSE body through the proxy), or, if the existing tests exercise `meter`'s onDone via a fake upstream, follow that pattern. Assert: after a request completes, `lease.Reserved` returns to 0 and `SpentUSD` reflects the metered cost; after a request with an empty/usageless body, `Reserved` returns to 0 and `SpentUSD` is unchanged.

```go
func TestMeter_ReleasesReservation(t *testing.T) {
	// Build a gateway whose upstream returns a minimal Anthropic usage body,
	// mint a lease with a reservation, drive one request through the proxy,
	// then assert Reserved == 0 afterward and SpentUSD == the metered cost.
	// (Follow the existing metering test's upstream + request construction.)
	// ... setup mirrors the existing "meter" test in this file ...
	// after the request completes and the body is fully read/closed:
	lease := g.leaseFor(tok) // use whatever accessor the existing tests use, or read g.leases under a test helper
	if lease.Reserved != 0 {
		t.Errorf("Reserved = %v after completion, want 0 (released)", lease.Reserved)
	}
}
```

Note: use the same lease-inspection approach the existing gateway tests use (they already assert on `SpentUSD` after metering, so there is an established way to reach the lease). If there is no accessor, read `g.leases[tok].Reserved`/`.SpentUSD` directly in the test (same package).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gateway/ -run TestMeter_ReleasesReservation`
Expected: FAIL (Reserved is never decremented, so it stays at the reserved value).

- [ ] **Step 3: Implement**

Rewrite the `onDone` closure in `meter` so the reservation release runs whether or not usage parsed, and the delta is committed the same way:

```go
	resp.Body = &usageReader{rc: resp.Body, onDone: func(body []byte) {
		delta := 0.0
		if model, in, out, ok := vt.v.ParseUsage(body, ct); ok {
			delta = cost(vt.v.Prices, model, in, out)
		}
		g.mu.Lock()
		if rc.lease.MaxRequestCostUSD > 0 {
			rc.lease.Reserved -= rc.lease.MaxRequestCostUSD
			if rc.lease.Reserved < 0 {
				rc.lease.Reserved = 0 // floor: defense in depth
			}
		}
		rc.lease.SpentUSD += delta
		g.mu.Unlock()
		if g.aggBudget > 0 && delta > 0 {
			g.ledger.add(rc.lease.Vendor, delta, time.Now())
		}
	}}
```

(The reservation release is outside the `if ok` block so a usageless stream still releases; the aggregate `ledger.add` stays gated on a real `delta`.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/gateway/`
Expected: PASS (reservation released; existing metering + aggregate-cap tests unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/gateway.go internal/gateway/gateway_test.go
git commit -m "feat(gateway): meter releases the reservation and commits actual cost"
```

---

### Task 5: brokerd wiring + docs

**Files:**
- Modify: `cmd/brokerd/main.go`
- Modify: `site/docs/configuration.md`, `CHANGELOG.md`, `docs/ROADMAP.md`
- Test: `cmd/brokerd/main_test.go` or `backends_test.go` if a provider-build assertion fits; otherwise the build + existing brokerd tests suffice.

- [ ] **Step 1: Wire the config onto the provider**

In `cmd/brokerd/main.go`, where each `&gateway.Provider{...}` is built (around line 341-357), add:

```go
		MaxRequestCost: cfg.MaxRequestCostUSD,
```

- [ ] **Step 2: Verify build + tests**

Run: `go build ./... && go test -race ./cmd/brokerd/ ./internal/gateway/ ./internal/config/`
Expected: PASS.

- [ ] **Step 3: Docs**

`site/docs/configuration.md`: add a `max_request_cost_usd` row (env `DRYDOCK_MAX_REQUEST_COST_USD`, default `0`, meaning: worst-case USD reserved per in-flight request so concurrent requests cannot admit past the budget; `0` disables). Match the table format.

`CHANGELOG.md`: add an `## Unreleased` Added entry for the in-flight reservation / per-request cost ceiling (config field, closes the concurrent-bypass hole from #139, off by default).

`docs/ROADMAP.md`: mark 4.15 Landed with a short description; remove from the ranked backlog and renumber; no dangling "4.15 pending" reference.

- [ ] **Step 4: Regenerate + verify**

Run: `make docs && go test ./... && grep -rn '—' site/docs/configuration.md CHANGELOG.md docs/ROADMAP.md`
Expected: docs OK, tests PASS, grep empty.

- [ ] **Step 5: Commit**

```bash
git add cmd/brokerd/main.go site/docs/configuration.md CHANGELOG.md docs/ROADMAP.md
git commit -m "feat(brokerd): wire max_request_cost_usd; docs (ROADMAP 4.15 landed)"
```

---

## Notes for the implementer

- `Mint` gains a parameter, so EVERY `.Mint(` call site must be updated (mostly `internal/gateway/gateway_test.go`); pass `0` for `maxRequestCostUSD` to preserve existing behavior. Grep `\.Mint(` across `internal/` and `cmd/`.
- The reservation release in `meter`'s onDone MUST run even when `ParseUsage` returns `ok == false` (move it outside the `if ok` block), or a usageless stream leaks its reservation.
- `Reserved` and `SpentUSD` are both guarded by `g.mu`; the reservation gate in `admit` and the release in `meter` both hold `g.mu`. The aggregate `ledger.add` stays outside `g.mu` (after Unlock), as it is today.
- A reservation leaks (until lease expiry) only if a request admits but the upstream fails before a response body exists (no `meter`). This is fail-closed (stricter budget, never looser) and documented as a known bound, not a bug.
- Run `go test -race ./...`, `gofmt -l .`, `staticcheck ./...` before the PR.
