# openai-compat Lane Validation Hardening — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Harden and prove out the `openai-compat` (bring-your-own OpenAI-compatible model / `opencode`) lane: surface budget- and routing-misconfigurations as loud boot warnings, add the missing containment + coverage tests, and document the streaming metering limitation.

**Architecture:** drydock proxies `opencode` traffic through a credential-swapping gateway (real upstream key host-side, `tok_` lease in the VM). Misconfigurations in `~/.drydock/config.yaml`'s `openai_compat:` block currently pass `config.validate()` and fail (or silently overspend) at task time. This plan adds boot-time `slog.Warn` surfacing (NOT validation rejection — per owner decision, warn don't reject) plus tests.

**Tech Stack:** Go, `slog`, `go test`, the Apple `container` integration suite, the `cmd/docs-build` site renderer.

## Global Constraints

- **Warn, don't reject.** Owner decision: misconfigurations in `openai_compat` are surfaced via `slog.Warn` at brokerd boot, NOT rejected by `config.validate()`. Do not add new hard-fail returns to `validate()` for these cases.
- **No request-path mutation.** Do not change what the gateway forwards to the upstream (no injecting `stream_options`, no rewriting request bodies). The streaming metering gap is documented, not fixed in code.
- Boot warnings use `slog.Warn` (the existing brokerd mechanism; see `cmd/brokerd/main.go:275-281` for the precedent message style: a human-readable message + structured attrs).
- Warning-detection logic must live in a **pure, unit-testable helper** (not inline in `main()`, which has no test).
- All new Go tests must pass `go test` and `staticcheck` (CI gate: `go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...`).
- Match existing code idiom: terse comments explaining *why*, table-driven tests, `t.Helper()` on helpers.

---

### Task 1: Boot warnings for openai-compat misconfigurations

**Files:**
- Modify: `cmd/brokerd/backends.go` — add a pure helper `openAICompatWarnings(oc config.OpenAICompat) []string`.
- Modify: `cmd/brokerd/main.go` — call the helper after config load, emit one `slog.Warn` per returned string.
- Test: `cmd/brokerd/backends_test.go` — table-driven test of the helper.

**Interfaces:**
- Consumes: `config.Config.OpenAICompat` (struct with `BaseURL, BasePath, APIKeyEnv, Model string` and `Prices map[string]struct{Input, Output float64}`).
- Produces: `func openAICompatWarnings(oc config.OpenAICompat) []string` — returns a slice of human-readable warning strings (empty when the lane is disabled or clean).

The helper returns a warning string for each of these conditions (only when `oc.BaseURL != ""`):
1. **Negative price:** any model whose `Input < 0` or `Output < 0`. Message names the model, e.g. `openai_compat.prices["gpt-x"] has a negative value; a negative price makes the USD budget never trip — spend will be uncapped except by task_max_requests`.
2. **Partial prices, no default:** `len(oc.Prices) > 0` but no `"default"` key. Message: `openai_compat.prices is set but has no "default" entry; any model not listed is metered at $0 (uncapped USD spend) — add a "default" row or rely on task_max_requests`.
3. **base_url carries a path:** the parsed `oc.BaseURL` has a path that is non-empty and not `"/"`. Message names the path, e.g. `openai_compat.base_url has a path ("/api/v1"); that path is ignored by the gateway — move it to openai_compat.base_path`.
4. **base_path missing leading slash:** `oc.BasePath != "" && !strings.HasPrefix(oc.BasePath, "/")`. Message: `openai_compat.base_path ("v1") does not start with "/"; the upstream path will be mis-joined — prefix it with "/"`.

Use `net/url` to parse `BaseURL` for condition 3; if parse fails, skip condition 3 (validate() already rejects unparseable URLs at load — the helper is best-effort and must never panic).

- [ ] **Step 1: Write the failing test** in `cmd/brokerd/backends_test.go`:

```go
func TestOpenAICompatWarnings(t *testing.T) {
	mk := func(base, basePath string, prices map[string]struct {
		Input  float64
		Output float64
	}) config.Config {
		var c config.Config
		c.OpenAICompat.BaseURL = base
		c.OpenAICompat.BasePath = basePath
		c.OpenAICompat.APIKeyEnv = "OC_KEY"
		c.OpenAICompat.Model = "m"
		c.OpenAICompat.Prices = prices
		return c
	}
	price := func(in, out float64) struct {
		Input  float64
		Output float64
	} {
		return struct {
			Input  float64
			Output float64
		}{in, out}
	}

	t.Run("disabled lane yields no warnings", func(t *testing.T) {
		var c config.Config // BaseURL == ""
		if w := openAICompatWarnings(c.OpenAICompat); len(w) != 0 {
			t.Errorf("want none, got %v", w)
		}
	})
	t.Run("clean config yields no warnings", func(t *testing.T) {
		c := mk("https://up.test", "/api/v1", map[string]struct {
			Input  float64
			Output float64
		}{"default": price(1, 2)})
		if w := openAICompatWarnings(c.OpenAICompat); len(w) != 0 {
			t.Errorf("want none, got %v", w)
		}
	})
	t.Run("negative price warns", func(t *testing.T) {
		c := mk("https://up.test", "", map[string]struct {
			Input  float64
			Output float64
		}{"gpt-x": price(-1, 2), "default": price(1, 2)})
		w := strings.Join(openAICompatWarnings(c.OpenAICompat), "\n")
		if !strings.Contains(w, "gpt-x") || !strings.Contains(w, "negative") {
			t.Errorf("expected negative-price warning naming gpt-x; got %q", w)
		}
	})
	t.Run("partial prices without default warns", func(t *testing.T) {
		c := mk("https://up.test", "", map[string]struct {
			Input  float64
			Output float64
		}{"gpt-x": price(1, 2)})
		w := strings.Join(openAICompatWarnings(c.OpenAICompat), "\n")
		if !strings.Contains(w, "default") {
			t.Errorf("expected no-default warning; got %q", w)
		}
	})
	t.Run("base_url with path warns", func(t *testing.T) {
		c := mk("https://openrouter.ai/api/v1", "", nil)
		w := strings.Join(openAICompatWarnings(c.OpenAICompat), "\n")
		if !strings.Contains(w, "/api/v1") || !strings.Contains(w, "base_path") {
			t.Errorf("expected base_url-path warning; got %q", w)
		}
	})
	t.Run("base_path without leading slash warns", func(t *testing.T) {
		c := mk("https://up.test", "v1", nil)
		w := strings.Join(openAICompatWarnings(c.OpenAICompat), "\n")
		if !strings.Contains(w, "base_path") || !strings.Contains(w, "/") {
			t.Errorf("expected base_path slash warning; got %q", w)
		}
	})
}
```

Note: `config.OpenAICompat`'s `Prices` value type is an anonymous struct `struct{Input, Output float64}` — the test mirrors it. If referencing the field type is awkward, the implementer may add a named accessor, but must not change the YAML wire shape.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/brokerd/ -run TestOpenAICompatWarnings -v`
Expected: FAIL — `openAICompatWarnings` undefined.

- [ ] **Step 3: Implement `openAICompatWarnings`** in `cmd/brokerd/backends.go`. Pure function, no logging inside. Parse `BaseURL` with `net/url`; guard every branch so it never panics. Return `nil` when `oc.BaseURL == ""`.

- [ ] **Step 4: Wire it into boot** in `cmd/brokerd/main.go` — after the config is loaded and logging is initialized (near the existing `default_agent` warning at ~line 275), iterate `openAICompatWarnings(cfg.OpenAICompat)` and `slog.Warn(msg)` each. Keep each warning a single `slog.Warn` call with the message as the first arg.

- [ ] **Step 5: Run tests + staticcheck**

Run: `go test ./cmd/brokerd/ -count=1 && go vet ./cmd/brokerd/ && go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./cmd/brokerd/...`
Expected: PASS, no findings.

- [ ] **Step 6: Commit** `feat(brokerd): warn at boot on openai-compat budget/routing misconfig`

---

### Task 2: Gateway coverage tests for the compat lane (routing + metering + usage)

**Files:**
- Test: `internal/gateway/gateway_test.go` — director routing through a compat vendor; end-to-end metering through a compat vendor.
- Test: `internal/gateway/usage_test.go` — `parseOpenAIUsage` on missing/null/zero usage.

**Interfaces (consumed, all existing — do not modify production code unless a test reveals a real defect; if it does, STOP and report it as a finding rather than silently changing behavior):**
- `gateway.OpenAICompatVendor(name, baseURL, basePath string, prices map[string]gateway.Price) gateway.Vendor`
- `gateway.New(...Backend) (*Gateway, error)`, `gateway.StaticKey(string)`, `gateway.Price{InputPer1M, OutputPer1M float64}`
- `(*Gateway).director` is unexported; route a request by constructing the gateway and exercising it the way the existing `TestDirector_CodexRemapPreservesQuery` (gateway_test.go:282) does — follow that test's pattern exactly for accessing routing.
- The metering path: follow `TestGateway_ValidTokenProxiesAndMeters` (gateway_test.go:52) and `TestGateway_OverBudget402` (gateway_test.go:110) for how to mint a lease, drive a request through `ServeHTTP`, and read `SpentUSD`. Over-budget returns **402** (StatusPaymentRequired), not 429.
- `parseOpenAIUsage(body []byte, contentType string) (model string, in, out int, ok bool)` (usage.go:73).

- [ ] **Step 1: Write director routing test** `TestDirector_OpenAICompatRoutes` in `gateway_test.go`, mirroring `TestDirector_CodexRemapPreservesQuery`. Build a compat vendor with `baseURL="https://up.test"`, `basePath="/api/v1"`; assert a request whose inbound path is `/v1/chat/completions` is rewritten to scheme `https`, host `up.test`, path `/api/v1/chat/completions` (basePath joined, leading `/v1` stripped). Add a second case with `basePath=""` asserting the path is forwarded byte-identical (`/v1/chat/completions`) and host/scheme are still rewritten.

- [ ] **Step 2: Write metering test** `TestGateway_OpenAICompatMetersAndCaps` in `gateway_test.go`, mirroring `TestGateway_ValidTokenProxiesAndMeters` + `TestGateway_OverBudget402` but using `OpenAICompatVendor` with a priced model. Stand up an `httptest` upstream returning a chat/completions body with a known `usage` (`prompt_tokens`/`completion_tokens`). Assert: real key swapped in upstream-side (`Authorization: Bearer <realKey>`), `SpentUSD` matches the priced cost, and once the budget is exhausted the next request returns **402**.

- [ ] **Step 3: Write usage-parsing tests** in `usage_test.go`: table-driven `TestParseOpenAIUsage_MissingOrNull` covering: body with no `usage` field → `ok==false`; body with `"usage":null` → `ok==false`; body with `usage` present but zero tokens → `ok==true, in==0, out==0` (zero is a valid metered value). Use `application/json` content type.

- [ ] **Step 4: Run tests + staticcheck**

Run: `go test ./internal/gateway/ -count=1 -run 'OpenAICompat|ParseOpenAIUsage_MissingOrNull' -v && go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./internal/gateway/...`
Expected: PASS, no findings. If any assertion reveals a real production defect (e.g. routing does something other than documented), STOP and report — do not paper over it.

- [ ] **Step 5: Commit** `test(gateway): cover openai-compat routing, metering, and empty-usage parsing`

---

### Task 3: Broker test — DefaultModel must not leak into the opencode lane

**Files:**
- Test: `internal/broker/broker_test.go`.

**Interfaces:** the blanking logic is `internal/broker/broker.go:506-509` (when `taskVendor == "openai-compat"`, the operator `DefaultModel` is zeroed before building the model env). Find how the existing `TestTaskModelFor` (broker_test.go:642) and `TestModelEnv` (broker_test.go:659) construct inputs and assert env, and follow the closest one. Prefer testing the smallest unit that exercises the branch; if the branch is only reachable through a larger method, assert via that method's observable env output (the `DRYDOCK_*MODEL*` env var), not by duplicating logic.

- [ ] **Step 1: Write the test** asserting that with a non-empty operator `DefaultModel` (a claude-oriented model) and `taskVendor == "openai-compat"`, the produced model env does NOT carry the operator default — only `--model`/`openai_compat.model` resolves. Contrast with a non-compat vendor where `DefaultModel` IS used.

- [ ] **Step 2: Run it**

Run: `go test ./internal/broker/ -run 'TestTaskModelFor|TestModelEnv|DefaultModel' -v -count=1`
Expected: PASS.

- [ ] **Step 3: Commit** `test(broker): assert operator default_model never leaks into the opencode lane`

---

### Task 4: VM-backed A1 red-team for the openai-compat lane

**Files:**
- Test: `tests/integration/redteam_test.go` (build tag `//go:build integration`).

**Interfaces:** mirror `TestRedteam_A1_RealKeyNeverInVM` (redteam_test.go:57). Use `gateway.OpenAICompatVendor("openai-compat", "https://up.invalid", "", nil)` + `gateway.StaticKey(sentinel)`; build a `gateway.Provider` with env names sourced from the registry via the existing `gwEnvNames("openai-compat")` helper (redteam_test.go:19); `Mint`, inject `grant.EnvVars()` into a `container run`, inspect the VM env, assert the sentinel real key is ABSENT and a `tok_` bearer is PRESENT. Reuse `requireContainer`, `sandboxImage`, `containerRun`.

Note: `gwEnvNames("openai-compat")` must return non-empty env names (the registry row defines `BaseURLEnv=OPENAI_BASE_URL`, `TokenEnv=OPENAI_API_KEY`); if it returns empty, that's a real registry gap — STOP and report.

- [ ] **Step 1: Write `TestRedteam_A1_OpenAICompatRealKeyNeverInVM`** mirroring the anthropic A1 VM test exactly, swapping the vendor and sentinel (`const sentinel = "sk-oc-SENTINEL-DO-NOT-LEAK-7a2b"`).

- [ ] **Step 2: Verify it compiles under the integration tag**

Run: `go test -tags=integration -run TestRedteam_A1_OpenAICompat ./tests/... -v` (will SKIP if no `container` CLI, which is acceptable on CI; the point is it compiles and runs where the runtime exists).
Expected: PASS or SKIP (no `container`), never a compile error.

- [ ] **Step 3: Run it for real if `container` is available** (controller will run `make redteam-vm` during review). Expected: PASS — sentinel absent, `tok_` present.

- [ ] **Step 4: Commit** `test(integration): VM-backed A1 for the openai-compat lane`

---

### Task 5: Document the streaming metering limitation

**Files:**
- Modify: `site/docs/configuration.md` — under the `openai_compat` / budget discussion, add the limitation note.
- Modify: `THREAT_MODEL.md` — add one line where budget/metering is discussed.
- Run: `make docs` to regenerate the gitignored HTML (do not commit HTML; it's gitignored).

**Interface:** the docs build is `cmd/docs-build` (`make docs`); the sidebar order is pinned in `cmd/docs-build/main.go:19`. Do not add new pages — edit existing ones. The site test `cmd/docs-build/site_test.go` (`TestNoStaleVersion`, `TestLandingInternalLinksResolve`) must still pass.

- [ ] **Step 1: Add the note to `site/docs/configuration.md`.** Content (adapt prose to the page's voice): streaming chat/completions responses often omit token usage unless the client requests it, so a *streamed* task against a priced `openai_compat` endpoint may be metered at $0 against `task_budget_usd`. The robust backstop is `task_max_requests`, which applies regardless of usage reporting. Recommend setting `task_max_requests` for any `openai_compat` lane that streams. Cross-reference: a `prices` map should include a `"default"` row so unlisted models are still metered.

- [ ] **Step 2: Add one line to `THREAT_MODEL.md`** where budget enforcement is discussed: note that USD metering depends on upstream usage reporting; `task_max_requests` is the usage-independent cap. Keep it factual and short.

- [ ] **Step 3: Rebuild docs and run the site test**

Run: `make docs && go test ./cmd/docs-build/ -count=1`
Expected: HTML regenerates; tests PASS.

- [ ] **Step 4: Commit** `docs: note the openai-compat streaming metering limitation` (stage only the `.md` files — HTML is gitignored).
