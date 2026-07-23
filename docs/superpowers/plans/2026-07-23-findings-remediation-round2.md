# Findings Remediation Round 2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the open items from the findings.md verification pass (2026-07-13): V-01 fail-closed oversize diff, F-08 trailing YAML docs, F-05 per-lease in-flight cap, F-03 exact-match routes, F-02 request-cap default, F-07 broker-authored terminal results on every exit path, V-02 release-gate receipt, and the dependent doc reconciliation.

**Architecture:** Each finding is an independent, self-contained fix in an existing subsystem (stage, config/egress loaders, gateway, broker, Makefile/workflow, docs). No new packages. Task 3 changes the `Gateway.Mint` signature; Task 4 depends on that signature, so run 3 before 4.

**Tech Stack:** Go 1.26, gopkg.in/yaml.v3, GitHub Actions, GNU make.

## Global Constraints

- No em dashes anywhere in docs, comments, YAML, or commit messages: use commas, colons, or parens. En-dash ranges are fine.
- Commit messages follow the repo's conventional style with finding IDs, e.g. `fix(stage): fail closed on an oversize review diff (V-01, High)`. End every commit with the `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` trailer. Never add a "Generated with Claude Code" banner anywhere.
- After each task: `go test -race -count=1 ./<touched packages>` must pass; before the final task, `go test -race -count=1 ./...` and `go vet ./...` must pass.
- Do NOT delete or modify the untracked `findings.md` at the repo root. It is the reviewer's report.
- Any doc claim change must update BOTH the root doc and its `site/docs/` mirror where one exists (`.html` is gitignored and regenerated on deploy; only touch markdown).

---

### Task 1: V-01, fail closed on an oversize review diff

**Files:**
- Modify: `internal/stage/stage.go:121-185`
- Modify: `internal/broker/broker.go:487-491`
- Test: `internal/stage/diffcap_test.go`

**Interfaces:**
- Produces: `stage.ErrDiffTooLarge` (exported sentinel, `errors.Is`-matchable through `CaptureDiff`'s error). `CaptureDiff() (string, error)` signature unchanged; it now returns a non-nil error instead of a truncated string when the staged diff exceeds `maxDiffBytes`.

- [ ] **Step 1: Update the truncation test to expect an error**

In `internal/stage/diffcap_test.go`, rewrite the oversize case (currently asserting a marker at lines ~22-30) to:

```go
	out, err := s.gitDiffCapped(512) // cap far below the diff size
	if !errors.Is(err, ErrDiffTooLarge) {
		t.Fatalf("gitDiffCapped over cap: got (%q, %v), want ErrDiffTooLarge", out, err)
	}
	if out != "" {
		t.Errorf("oversize diff must return no partial content, got %d bytes", len(out))
	}
```

Add `"errors"` to the test imports. Leave the small-diff case (asserting no truncation) untouched.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestDiffCap -count=1 ./internal/stage/`
Expected: FAIL (the current code returns a truncated string with marker, nil error).

- [ ] **Step 3: Implement fail-closed in stage.go**

In `internal/stage/stage.go`, add `"errors"` to imports, add the sentinel above `maxDiffBytes`, and update the doc comments plus the tail of `gitDiffCapped`:

```go
// ErrDiffTooLarge means the staged diff exceeds maxDiffBytes. The broker fails
// the task instead of truncating: the approval gate must never authorize bytes
// it could not show the reviewer, and a hostile task could hide a malicious
// change behind 32 MiB of alphabetically earlier padding (V-01).
var ErrDiffTooLarge = errors.New("stage: staged diff exceeds the review cap")

// maxDiffBytes bounds the review diff held in broker memory and written to the
// audit .diff file. A hostile task that stages a giant or binary diff would
// otherwise allocate the whole thing (and a second copy on persist), risking
// broker OOM. Exceeding the cap is a task failure (ErrDiffTooLarge), never a
// truncated review: approve must only ever authorize fully reviewable bytes.
const maxDiffBytes = 32 << 20 // 32 MiB
```

In `gitDiffCapped`, replace the `truncated` marker append at the end:

```go
	if werr := cmd.Wait(); werr != nil {
		return "", fmt.Errorf("git diff --cached: %w\n%s", werr, stderr.String())
	}
	if truncated {
		return "", fmt.Errorf("%w (%d MiB)", ErrDiffTooLarge, max>>20)
	}
	return buf.String(), nil
```

Also update the `CaptureDiff` doc comment: "bounded to maxDiffBytes; a larger diff fails closed with ErrDiffTooLarge so a partial diff can never reach review."

- [ ] **Step 4: Run stage tests**

Run: `go test -race -count=1 ./internal/stage/`
Expected: PASS

- [ ] **Step 5: Give the broker call site a specific operator-facing reason**

In `internal/broker/broker.go` (~line 487, inside `HandleTask`):

```go
	diff, err := st.CaptureDiff()
	if err != nil {
		reason := "diff capture failed"
		if errors.Is(err, stage.ErrDiffTooLarge) {
			reason = "task failed closed: staged diff exceeds the 32 MiB review cap, so it cannot be fully reviewed (V-01)"
		}
		sw.emit(errorEvent(taskID, reason, ""))
		return
	}
```

Verify `"errors"` and the stage package are already imported in broker.go (they are; `realStage` wraps `*stage.Stage`).

- [ ] **Step 6: Run broker tests and commit**

Run: `go test -race -count=1 ./internal/stage/ ./internal/broker/ ./internal/trustbrief/`
Expected: PASS (trustbrief keeps its marker-parsing for historical audit files; do not touch it).

```bash
git add internal/stage/stage.go internal/stage/diffcap_test.go internal/broker/broker.go
git commit -m "fix(stage): fail closed on an oversize review diff (V-01, High)"
```

---

### Task 2: F-08, reject trailing YAML documents in both loaders

**Files:**
- Modify: `internal/config/config.go:244-263` (inside `Load`)
- Modify: `internal/egress/egress.go:111-128` (inside `Load`)
- Test: `internal/config/config_test.go`, `internal/egress/egress_test.go` (or the existing strictness test files; find with `grep -rln "KnownFields\|unknown" internal/config internal/egress --include='*_test.go'`)

**Interfaces:** No signature changes. Both `Load` functions gain a rejection: a file with a second YAML document after `---` returns an error containing "trailing YAML document".

- [ ] **Step 1: Write the failing tests**

Main config test:

```go
func TestLoad_RejectsTrailingYAMLDocument(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	// The second document carries a security-relevant field the operator
	// believes is active; silently ignoring it would fail open (F-08).
	body := "task_budget_usd: 2.0\n---\naggregate_budget_usd: 100\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "trailing YAML document") {
		t.Fatalf("Load with trailing document: got %v, want trailing-document rejection", err)
	}
}
```

Egress test (same shape; use a first document with a valid `default:` block copied from an existing test fixture in that file, then `---` and `requires_approval: false` as the second document; assert `Load` errors with "trailing YAML document").

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TrailingYAML -count=1 ./internal/config/ ./internal/egress/`
Expected: FAIL (both currently accept the file silently).

- [ ] **Step 3: Implement the second-decode check**

`internal/config/config.go`, directly after the existing `dec.Decode(cfg)` error check:

```go
			// A second YAML document (---) would be silently ignored, and it can
			// carry security config the operator believes is active. Fail closed:
			// exactly one document is allowed (F-08).
			if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("parse %s: trailing YAML document; only one document is allowed", path)
			}
```

`internal/egress/egress.go`, directly after its `dec.Decode(&cfg)` error check:

```go
	// One document only: a trailing document could carry requires_approval or
	// extra domains the operator believes are active. Fail closed (F-08).
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("parse egress config %s: trailing YAML document; only one document is allowed", path)
	}
```

Add `"errors"` and `"io"` to imports where missing.

- [ ] **Step 4: Run tests and commit**

Run: `go test -race -count=1 ./internal/config/ ./internal/egress/`
Expected: PASS

```bash
git add internal/config/config.go internal/egress/egress.go internal/config/config_test.go internal/egress/*_test.go
git commit -m "fix(config): reject trailing YAML documents in both loaders (F-08 residual)"
```

---

### Task 3: F-05, per-lease in-flight request cap (default 1)

**Files:**
- Modify: `internal/gateway/gateway.go` (Lease struct ~line 23, `Mint` ~line 84, `admit` ~line 155, `ServeHTTP` ~line 215)
- Modify: `internal/gateway/provider.go:32-49`
- Modify: `internal/config/config.go` (struct ~line 87, `Defaults()` ~line 150, `validate()` ~line 397)
- Modify: `cmd/brokerd/main.go:358-368` (provider construction)
- Modify: `config/config.yaml:22-24`
- Test: `internal/gateway/gateway_test.go` (new tests) plus every existing `.Mint(` call site (`grep -rn "\.Mint(" internal/gateway cmd/drydock/redteam.go`)

**Interfaces:**
- Produces: `Gateway.Mint(vendor string, budgetUSD float64, maxRequests int, maxRequestCostUSD float64, maxInFlight int, ttl time.Duration) (string, error)` (new `maxInFlight` param, 0 = unlimited); `gateway.Provider` gains field `MaxInFlight int`; `config.Config` gains `TaskMaxInFlight int` (yaml `task_max_inflight`, default 1); unexported `(*Gateway).release(token string)`.
- Consumed by: Task 4's test (uses the 6-arg Mint), Task 8 docs.

- [ ] **Step 1: Write the failing tests**

In `internal/gateway/gateway_test.go` (they will not compile until Mint changes; that is the failing state):

```go
// A lease with MaxInFlight=1 admits a second concurrent request only after the
// first completes. Spend is metered at response completion, so every
// concurrently admitted request can overshoot the budget by its own cost;
// this cap bounds that overshoot (F-02/F-05).
func TestAdmit_InFlightLimit(t *testing.T) {
	g, err := New(Backend{Vendor: AnthropicVendor(), Cred: StaticKey("k")})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := g.Mint("anthropic", 5, 0, 0, 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, code := g.admit(tok); code != 0 {
		t.Fatalf("first admit: code %d", code)
	}
	if _, code := g.admit(tok); code != http.StatusTooManyRequests {
		t.Fatalf("second concurrent admit: code %d, want 429", code)
	}
	g.release(tok)
	if _, code := g.admit(tok); code != 0 {
		t.Fatalf("admit after release: want admitted")
	}
}

// End to end: while one proxied request is still streaming, a second request
// gets a local 429 with Retry-After, and succeeds after the first completes.
func TestServeHTTP_InFlightLimit(t *testing.T) {
	unblock := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-unblock
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"claude-x","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()
	v := AnthropicVendor()
	v.BaseURL = up.URL
	g, err := New(Backend{Vendor: v, Cred: StaticKey("k")})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := g.Mint("anthropic", 100, 0, 0, 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, req)
		return w
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- do() }()
	// Wait until the first request is admitted and blocked in the upstream.
	deadline := time.Now().Add(2 * time.Second)
	for {
		g.mu.Lock()
		inflight := g.leases[tok].InFlight
		g.mu.Unlock()
		if inflight == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first request never became in-flight")
		}
		time.Sleep(time.Millisecond)
	}
	if w := do(); w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("concurrent request: code %d retry-after %q, want 429 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
	close(unblock)
	if w := <-first; w.Code != http.StatusOK {
		t.Fatalf("first request: code %d", w.Code)
	}
	if w := do(); w.Code != http.StatusOK {
		t.Fatalf("request after completion: code %d, want 200", w.Code)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -run InFlight -count=1 ./internal/gateway/`
Expected: compile FAIL (Mint has 5 args, `release` and `InFlight` undefined).

- [ ] **Step 3: Implement in gateway.go**

Lease struct additions:

```go
	MaxInFlight int // max concurrently admitted requests (0 = unlimited)
	InFlight    int // admitted, response not yet complete; guarded by g.mu
```

`Mint` gains `maxInFlight int` before `ttl` and stores `MaxInFlight: maxInFlight` in the lease literal.

In `admit`, after the `MaxRequests` check and before the aggregate check:

```go
	if l.MaxInFlight > 0 && l.InFlight >= l.MaxInFlight {
		// Spend is metered when a response body completes, so every
		// concurrently admitted request can overshoot the budget by its own
		// cost. Serialize instead of admitting the race (F-02/F-05).
		return nil, http.StatusTooManyRequests
	}
```

and next to `l.Requests++` add `l.InFlight++`.

New method:

```go
// release marks one admitted request complete, freeing its in-flight slot.
// Looked up by token: the lease may have been revoked mid-request.
func (g *Gateway) release(token string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if l := g.leases[token]; l != nil && l.InFlight > 0 {
		l.InFlight--
	}
}
```

In `ServeHTTP`, replace the admit block:

```go
	lease, status := g.admit(tok)
	if status != 0 {
		if status == http.StatusTooManyRequests {
			// A sequential agent CLI only hits this when a side call overlaps
			// its main stream; SDKs back off and retry on 429.
			w.Header().Set("Retry-After", "1")
		}
		http.Error(w, http.StatusText(status), status)
		return
	}
	// ReverseProxy.ServeHTTP streams the response body before returning, so
	// metering (which runs at body EOF) has already settled SpentUSD when the
	// slot frees: the next admitted request sees the updated spend.
	defer g.release(tok)
```

- [ ] **Step 4: Wire through provider, config, brokerd**

`internal/gateway/provider.go`: add `MaxInFlight int` field to `Provider` (next to `MaxRequests`); change the call to `p.GW.Mint(p.Vendor, b, p.MaxRequests, p.MaxRequestCost, p.MaxInFlight, ttl)`.

`internal/config/config.go`: after `TaskMaxRequests`:

```go
	// TaskMaxInFlight caps concurrently admitted gateway requests per task
	// lease. Spend is metered post-hoc, so each concurrently admitted request
	// can overshoot the budget by its own cost; this bounds the overshoot to
	// task_max_inflight requests. 1 (the default) restores the documented
	// "at most one request" bound. 0 = unlimited (pre-v0.6.3 behavior).
	TaskMaxInFlight int `yaml:"task_max_inflight"`
```

`Defaults()`: `TaskMaxInFlight: 1,`. `validate()`:

```go
	if c.TaskMaxInFlight < 0 {
		return fmt.Errorf("config: task_max_inflight must be >= 0, got %d", c.TaskMaxInFlight)
	}
```

`cmd/brokerd/main.go` provider construction: add `MaxInFlight: cfg.TaskMaxInFlight,` after `MaxRequestCost`.

`config/config.yaml` after the `task_max_requests` line:

```yaml
task_max_inflight:      1              # concurrent gateway requests per task lease; bounds budget overshoot to this many in-flight requests (0 = unlimited)
```

Run `grep -rn "task_max_requests" cmd internal/config --include='*.go' -l` and mirror the new key into any embedded config template/seed those files carry (the claims test pins config.go's seed to config.yaml).

- [ ] **Step 5: Update every existing Mint call site**

Run `grep -rn "\.Mint(" internal/gateway cmd/drydock` and add `0,` (unlimited) as the new fifth argument in tests and `cmd/drydock/redteam.go` so existing behavior-pinning tests stay valid. Do not change their assertions.

- [ ] **Step 6: Run tests and commit**

Run: `go test -race -count=1 ./internal/gateway/ ./internal/config/ ./cmd/... && go vet ./...`
Expected: PASS

```bash
git add internal/gateway/ internal/config/config.go cmd/brokerd/main.go cmd/drydock/redteam.go config/config.yaml
git commit -m "fix(gateway): per-lease in-flight request cap, default 1 (F-05/F-02, High)"
```

---

### Task 4: F-03, exact-match inference routes (block the Batches API)

**Files:**
- Modify: `internal/gateway/vendor.go:59-69` (routeMatch), `vendor.go:87-174` (vendor route lists)
- Test: `internal/gateway/route_tighten_test.go`

**Interfaces:** `routeMatch` semantics change: a pattern without a trailing slash matches ONLY the exact path (sub-resources no longer implied); a trailing-slash pattern remains a directory prefix. `Route` struct unchanged.

- [ ] **Step 1: Update the tests to the new contract**

In `TestRouteAllowed_SegmentBoundary`, change/add cases:

```go
		{"POST", "/v1/messages", true},                // exact endpoint
		{"POST", "/v1/messages/batches", false},       // batch API: up to 100k unmetered messages (F-03), blocked
		{"POST", "/v1/messages/count_tokens", true},   // used by Claude Code, explicitly allowed
		{"POST", "/v1/messages/count_tokens/x", false}, // no implied sub-resources
		{"POST", "/v1/messagesX", false},              // sibling prefix: blocked
		{"GET", "/v1/models", true},                   // exact list
		{"GET", "/v1/models/claude-x", true},          // retrieve, via the /v1/models/ directory route
		{"GET", "/v1/models_secret", false},           // sibling prefix: blocked
		{"GET", "/v1/messages", false},                // wrong method
```

In `TestRouteAllowed_DirectoryPrefix` add:

```go
		{"GET", "/v1beta/models/gemini-2", true}, // retrieve, via the /v1beta/models/ directory route
```

Add an end-to-end regression (reuse the existing `routedGateway` helper in this file, whose mock upstream fails the test on contact):

```go
// The Batches endpoint creates up to 100k Message requests whose spend the
// response-usage meter never sees; it must 403 locally, never reach upstream.
func TestGateway_MessagesBatchesBlockedLocally(t *testing.T) {
	g := routedGateway(t, AnthropicVendor())
	tok, err := g.Mint("anthropic", 5, 0, 0, 0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/messages/batches", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST /v1/messages/batches: code %d, want 403", w.Code)
	}
}
```

(Match the Mint arity and any setup details to how the other `routedGateway` tests in the file mint tokens.)

- [ ] **Step 2: Run to verify failure**

Run: `go test -run 'RouteAllowed|BatchesBlocked' -count=1 ./internal/gateway/`
Expected: FAIL (batches currently allowed; count_tokens case passes via the old implied-sub-resource rule, gemini retrieve passes; the batches cases fail).

- [ ] **Step 3: Implement exact-match semantics and route lists**

`routeMatch`:

```go
// routeMatch matches path against a route pattern. A pattern with a trailing
// slash is a directory prefix (matches any sub-path). One without matches ONLY
// the exact path: sub-resources are never implied, because a sub-resource can
// carry authority the parent's metering never sees, e.g. POST
// /v1/messages/batches creates up to 100k Message requests whose spend the
// response-usage meter cannot observe (F-03). Sub-resources a pinned CLI
// actually needs are allowlisted explicitly.
func routeMatch(path, prefix string) bool {
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(path, prefix)
	}
	return path == prefix
}
```

Vendor route lists:

```go
// AnthropicVendor:
			// Claude Code posts inference to /v1/messages, counts context with
			// /v1/messages/count_tokens, and may list or retrieve models.
			// /v1/messages/batches is deliberately NOT here (F-03).
			AllowedRoutes: []Route{
				{"POST", "/v1/messages"},
				{"POST", "/v1/messages/count_tokens"},
				{"GET", "/v1/models"},
				{"GET", "/v1/models/"},
			},

// OpenAIVendor:
			AllowedRoutes: []Route{
				{"POST", "/v1/chat/completions"},
				{"POST", "/v1/responses"},
				{"GET", "/v1/models"},
				{"GET", "/v1/models/"},
			},

// GoogleVendor:
			AllowedRoutes: []Route{
				{"POST", "/v1beta/models/"},
				{"GET", "/v1beta/models"},
				{"GET", "/v1beta/models/"},
				{"GET", "/v1/models"},
			},
```

`OpenAIOAuthVendor` keeps `{"POST", "/responses"}, {"POST", "/v1/responses"}` (already exact).

- [ ] **Step 4: Run the full gateway suite**

Run: `go test -race -count=1 ./internal/gateway/`
Expected: PASS. If another existing test pinned implied-sub-resource behavior (e.g. a `/v1/models/<id>` retrieve through the exact route), it now exercises the new directory routes; fix only assertions that contradict the new contract, and keep a `false` expectation for `/v1/messages/batches`.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/vendor.go internal/gateway/route_tighten_test.go
git commit -m "fix(gateway): exact-match inference routes; block the Batches API (F-03 residual)"
```

---

### Task 5: F-02, request-cap default in every mode

**Files:**
- Modify: `cmd/brokerd/main.go:81-91` (effectiveRequestCap), `main.go:349-357` (call site + log)
- Modify: `internal/broker/broker.go:684-691` (writeBrief request-cap reporting)
- Modify: `config/config.yaml:22`, matching comment in `internal/config/config.go:83-87`
- Test: the existing `effectiveRequestCap` test (find with `grep -rn "effectiveRequestCap" cmd/brokerd`), `internal/broker` brief tests

**Interfaces:** `effectiveRequestCap(configured int) int` (drops the `uncapped` param): 0/unset now fails closed to `broker.DefaultUncappedRequestCap` (1000) in every auth mode.

- [ ] **Step 1: Update the effectiveRequestCap test**

Rewrite the existing test cases to the new single-arg contract:

```go
func TestEffectiveRequestCap(t *testing.T) {
	cases := []struct{ configured, want int }{
		{0, broker.DefaultUncappedRequestCap},  // unset fails closed in every mode (F-02)
		{-1, broker.DefaultUncappedRequestCap}, // nonsense treated as unset
		{50, 50},                               // explicit operator bound wins
		{5000, 5000},                           // explicit raise wins too
	}
	for _, c := range cases {
		if got := effectiveRequestCap(c.configured); got != c.want {
			t.Errorf("effectiveRequestCap(%d) = %d, want %d", c.configured, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -run EffectiveRequestCap -count=1 ./cmd/brokerd/`
Expected: compile FAIL (two-arg signature).

- [ ] **Step 3: Implement**

```go
// effectiveRequestCap returns the per-task request cap to enforce. 0/unset
// fails closed to broker.DefaultUncappedRequestCap in EVERY auth mode: the
// request count is independent defense in depth against a runaway or hostile
// task even when a USD budget also bounds the lane (F-02). The operator opts
// into a different bound (or a higher one) by setting task_max_requests.
func effectiveRequestCap(configured int) int {
	if configured <= 0 {
		return broker.DefaultUncappedRequestCap
	}
	return configured
}
```

Call site:

```go
		maxReq := effectiveRequestCap(cfg.TaskMaxRequests)
		if maxReq != cfg.TaskMaxRequests {
			slog.Info("task_max_requests unset: applying the default per-task request cap",
				"vendor", b.Vendor.Name, "request_cap", maxReq,
				"hint", "set task_max_requests to change this bound")
		}
```

(Keep the surrounding uncapped/budget logic untouched; only the cap derivation changes. Delete the now-unused `uncapped` argument plumbing but NOT the `uncapped` budget logic itself.)

In `writeBrief` (`internal/broker/broker.go`), the unmetered branch currently does `if policy.MaxRequests == 0 { policy.MaxRequests = DefaultUncappedRequestCap }`. Move that default outside the branch so the Brief reports the enforced cap in every mode:

```go
	if policy.MaxRequests == 0 {
		policy.MaxRequests = DefaultUncappedRequestCap
	}
```

placed right after the `policy` literal is built (delete the copy inside the unmetered branch).

Update `config/config.yaml:22` comment:

```yaml
task_max_requests:      0              # per-task request cap. 0 falls closed to a built-in default (1000) in every mode; set explicitly to change the bound
```

and mirror the wording in the `TaskMaxRequests` comment in `internal/config/config.go` and any embedded config seed.

- [ ] **Step 4: Run tests and commit**

Run: `go test -race -count=1 ./cmd/brokerd/ ./internal/broker/ ./internal/config/ ./cmd/docs-build/`
Expected: PASS

```bash
git add cmd/brokerd/main.go internal/broker/broker.go config/config.yaml internal/config/config.go cmd/brokerd/*_test.go
git commit -m "fix(brokerd): default per-task request cap in every auth mode (F-02 defense in depth)"
```

---

### Task 6: F-07, broker-authored terminal result on every exit path

**Files:**
- Modify: `internal/broker/broker.go:600-670` (runSandbox exit paths)
- Test: `internal/broker/broker_test.go` (new test; check `internal/creds` for the `Grant` interface before writing the fake)

**Interfaces:**
- Produces: `(tr *taskRun) appendBrokerResult(isError bool)`, writes one `src:"broker"` result line using `tr.grant.Spent()`.

- [ ] **Step 1: Write the failing test**

First inspect the `creds.Grant` interface (`grep -n "type Grant" internal/creds/*.go`) and mirror its method set in the fake. Expected shape:

```go
type fakeSpendGrant struct{ spent float64 }

func (f fakeSpendGrant) EnvVars() []string { return nil }
func (f fakeSpendGrant) Revoke()           {}
func (f fakeSpendGrant) Spent() float64    { return f.spent }

// Every runSandbox exit path must end the audit stream with a broker-authored
// result carrying gateway-metered spend: an untagged or zero-cost record makes
// the restart seed (src:"broker" + positive cost only) drop real spend from
// the aggregate ledger (F-07).
func TestAppendBrokerResult_BrokerAuthoredSpend(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := &taskRun{logf: f, taskStart: time.Now().Add(-2 * time.Second), grant: fakeSpendGrant{spent: 1.25}}
	tr.appendBrokerResult(true)
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("broker result is not one valid JSON line: %v\n%s", err, b)
	}
	if rec["src"] != "broker" || rec["is_error"] != true || rec["total_cost_usd"] != 1.25 {
		t.Fatalf("broker result = %v; want src=broker is_error=true total_cost_usd=1.25", rec)
	}
}
```

(Adapt the `taskRun` field names to the struct if they differ; `logf` and `grant` exist per broker.go. If `logf` is a narrower interface than `*os.File`, pass the file through it.)

- [ ] **Step 2: Run to verify failure**

Run: `go test -run AppendBrokerResult -count=1 ./internal/broker/`
Expected: compile FAIL (`appendBrokerResult` undefined).

- [ ] **Step 3: Implement the helper and rewire every exit path**

Add near `runSandbox`:

```go
// appendBrokerResult writes the broker-authored terminal result for this task.
// It must be the LAST result line in the audit stream on EVERY exit path:
// last-wins parsing plus the src:"broker" seed filter make it the only record
// the cost display and the restart-seeded aggregate ledger trust. A path that
// skips it (or writes total_cost_usd:0) erases that task's gateway-metered
// spend from the rolling cap after a brokerd restart (F-07).
func (tr *taskRun) appendBrokerResult(isError bool) {
	subtype := "success"
	if isError {
		subtype = "error"
	}
	_, _ = fmt.Fprintf(tr.logf,
		`{"type":"result","subtype":"%s","is_error":%t,"duration_ms":%d,"total_cost_usd":%.6f,"num_turns":0,"src":"broker"}`+"\n",
		subtype, isError, time.Since(tr.taskStart).Milliseconds(), tr.grant.Spent())
}
```

In `runSandbox`:
1. Cancel branch (`tr.ctx.Err() != nil` inside the run-error handler): add `tr.appendBrokerResult(true)` before emitting the cancelled event.
2. Output-cap branch: replace the inline `fmt.Fprintf(tr.logf, ...total_cost_usd:0...)` with `tr.appendBrokerResult(true)`.
3. Stage-cap branch: same replacement.
4. Generic-failure branch: same replacement.
5. Success path: replace the existing trailing `fmt.Fprintf(tr.logf, ...subtype":"success"...)` with `tr.appendBrokerResult(false)` (keep its existing explanatory comment, folded into the helper's).

After this, `grep -n 'total_cost_usd":0' internal/broker/broker.go` must return nothing.

- [ ] **Step 4: Run tests and commit**

Run: `go test -race -count=1 ./internal/broker/ ./internal/audit/ ./cmd/brokerd/`
Expected: PASS (audit.LastResult already parses `src`).

```bash
git add internal/broker/broker.go internal/broker/broker_test.go
git commit -m "fix(broker): broker-authored terminal result on every runSandbox exit path (F-07 residual)"
```

---

### Task 7: V-02, verify a preflight receipt before publishing a release

**Files:**
- Modify: `Makefile:128-137` (tag-release)
- Modify: `.github/workflows/release.yml` (new step after checkout in the `release` job)

**Interfaces:** `tag-release` embeds a `preflight: <commit-sha> green <utc>` line in the annotated tag message; `release.yml` refuses to build a tag whose annotation lacks that line for the exact tagged commit.

- [ ] **Step 1: Update tag-release**

```make
# tag-release is the blessed release path: it enforces release-preflight (so a
# release can never ship without the VM containment tests behind its headline
# claims), then creates and pushes the vX.Y.Z tag. The tag annotation carries a
# preflight receipt line that release.yml verifies before building, so a bare
# `git tag && git push` cannot publish artifacts by accident. (The receipt is
# workflow enforcement, not cryptographic proof against a hostile maintainer.)
# Requires main, a clean tree, a stamped CHANGELOG, and VERSION, e.g.
# make tag-release VERSION=v0.6.3
tag-release: check-release-args release-preflight
	git tag -a "$(VERSION)" -m "drydock $(VERSION)" \
		-m "preflight: $$(git rev-parse HEAD) green $$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	git push origin "$(VERSION)"
	@echo "== tagged + pushed $(VERSION); release.yml verifies the receipt, then builds the signed artifacts =="
```

- [ ] **Step 2: Add the verify step to release.yml**

Insert directly after the checkout step of the `release` job (checkout already uses `fetch-depth: 0`, which fetches tags):

```yaml
      - name: Verify preflight receipt
        # tag-release embeds "preflight: <commit> green <utc>" in the tag
        # annotation only after release-preflight (unit + host red-team +
        # VM-backed A1/A2/A7) passes. Refuse to build a bare `git tag` so the
        # VM gate cannot be skipped by accident (V-02). This binds publication
        # to the blessed workflow; it is not cryptographic proof against a
        # deliberately hostile maintainer.
        run: |
          tag="${GITHUB_REF_NAME}"
          case "$tag" in v*) ;; *) echo "::error::not a tag ref: $tag"; exit 2 ;; esac
          commit="$(git rev-parse "refs/tags/${tag}^{commit}")"
          if ! git tag -l --format='%(contents)' "$tag" | grep -Eq "^preflight: ${commit} green "; then
            echo "::error::tag ${tag} has no preflight receipt for ${commit}; cut releases with 'make tag-release VERSION=${tag}'"
            exit 1
          fi
```

- [ ] **Step 3: Verify locally**

Simulate both outcomes without running the (long) preflight:

```bash
git tag -a vtest-receipt -m "drydock vtest" -m "preflight: $(git rev-parse HEAD) green 2026-07-23T00:00:00Z"
commit=$(git rev-parse "refs/tags/vtest-receipt^{commit}")
git tag -l --format='%(contents)' vtest-receipt | grep -Eq "^preflight: ${commit} green " && echo RECEIPT-OK
git tag -a vtest-bare -m "drydock vtest"
git tag -l --format='%(contents)' vtest-bare | grep -Eq "^preflight: ${commit} green " || echo BARE-REJECTED
git tag -d vtest-receipt vtest-bare
```

Expected output: `RECEIPT-OK` then `BARE-REJECTED`. Also confirm the Makefile still parses: `make -n check-release-args VERSION=v9.9.9` exits 0 or fails only on its own argument checks.

- [ ] **Step 4: Commit**

```bash
git add Makefile .github/workflows/release.yml
git commit -m "build: release.yml verifies the tag-release preflight receipt (V-02)"
```

---

### Task 8: Doc reconciliation and claim sentinels

**Files:**
- Modify: `README.md:28`, `site/docs/quickstart.md:24`, `THREAT_MODEL.md:112` and its concurrent-overshoot section (~line 229), `config/config.yaml:14`, `docs/ROADMAP.md:107`, `CHANGELOG.md`
- Modify: `cmd/docs-build/claims_test.go`
- Check: `grep -rn "tag-release\|budget-capped\|overshoot" README.md THREAT_MODEL.md SECURITY.md site/docs docs` for any statement the earlier tasks made stale

**Interfaces:** none; prose only. No em dashes in any edit.

- [ ] **Step 1: Extend the claims sentinel first (failing state)**

Add to the `forbidden` table in `cmd/docs-build/claims_test.go`:

```go
		{"README.md", "budget-capped token", "spend can overshoot by task_max_inflight requests (default 1); say budget-scoped and state the bound (F-02)"},
		{"site/docs/quickstart.md", "budget-capped token", "same as README (F-02)"},
		{"THREAT_MODEL.md", "budget-capped bearer", "same bound applies to the bearer description (F-02)"},
		{"docs/ROADMAP.md", "every external input is pinned", "apt and npm transitive graphs still float at image build (F-09)"},
```

Run: `go test -run SecurityClaimsNoDrift -count=1 ./cmd/docs-build/`
Expected: FAIL on all four files (the stale phrases are still present).

- [ ] **Step 2: Fix the claims**

- `README.md:28`: replace "only ever sees a short-lived, budget-capped token." with "only ever sees a short-lived, budget-scoped token (spend overshoot is bounded to one in-flight request by default)."
- `site/docs/quickstart.md:24`: same replacement.
- `THREAT_MODEL.md:112`: replace "budget-capped bearer token" with "budget-scoped bearer token (overshoot bounded by task_max_inflight, default 1)".
- `THREAT_MODEL.md` overshoot section (~229): rewrite to state the current mechanics: admission serializes each lease to `task_max_inflight` concurrent requests (default 1), so worst-case overshoot is `task_max_inflight` times the largest single request cost; `max_request_cost_usd` adds a reservation-backed bound; the aggregate ledger's residual overshoot is one in-flight request per concurrent task (bounded by `max_concurrent`). Remove any remaining claim that concurrency makes the bound unbounded.
- `config/config.yaml:14`: update the `task_budget_usd` comment to "soft USD cap: metered post-hoc; overshoot bounded to task_max_inflight in-flight requests (default 1); set max_request_cost_usd for a reservation-backed bound (api_key mode only; ignored in subscription mode)". Mirror in the embedded seed in `internal/config/config.go` if the comment text is duplicated there.
- `docs/ROADMAP.md:107`: replace "every external input is pinned and bumped deliberately:" with "top-level external inputs are pinned and bumped deliberately (the apt and npm transitive graphs still float at image build; locking them is tracked as F-09 follow-up):".
- `CHANGELOG.md`: add an Unreleased section (or extend it) with one line per task: V-01 fail-closed diff, F-08 trailing YAML rejection, F-05/F-02 in-flight cap, F-03 batches block, F-02 request-cap default, F-07 terminal results, V-02 release receipt.

- [ ] **Step 3: Sweep for stale statements the code changes created**

Run `grep -rn "truncated at\|full change is still committed" README.md SECURITY.md THREAT_MODEL.md site/docs docs` and fix any doc describing the old truncate-and-continue diff behavior to describe fail-closed. Run `grep -rn "task_max_requests" site/docs docs README.md THREAT_MODEL.md` and align any "0 = unlimited" phrasing with the new always-default cap. Run `grep -rEn "—" $(git diff --name-only HEAD~7.. -- '*.md' '*.yaml' 2>/dev/null)` (adjust the range to this branch's doc commits) to confirm no em dashes were introduced.

- [ ] **Step 4: Run the full suite and commit**

Run: `go test -race -count=1 ./... && go vet ./...`
Expected: PASS, including `TestSecurityClaimsNoDrift`.

```bash
git add README.md THREAT_MODEL.md CHANGELOG.md config/config.yaml docs/ROADMAP.md site/docs cmd/docs-build/claims_test.go internal/config/config.go
git commit -m "docs: reconcile budget, diff-cap, request-cap, and pinning claims (F-02/F-09/F-10)"
```

---

## Out of scope (explicitly)

- F-09 lockfile/snapshot work (npm dependency graph lockfiles, dated Debian snapshots): a separate image supply-chain project; the ROADMAP claim is amended instead (Task 8).
- Aggregate in-flight reservations across leases: residual documented in THREAT_MODEL (Task 8); bounded by `max_concurrent` after Task 3.
- Cryptographic release attestation: Task 7 blocks accidental bypass only; documented as a residual.
