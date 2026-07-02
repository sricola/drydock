# Native Gemini Vendor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a native `gemini → google` vendor so drydock brokers Google's Gemini API directly (Google `x-goog-api-key` auth, native `usageMetadata` metering, the official Gemini CLI), added via one `provider.Registry` row plus shared vendor plumbing.

**Architecture:** A `GoogleVendor` in the gateway (base URL + key-header inject + usage parser + price table), one gateway-admission change to accept the per-task bearer from `x-goog-api-key`, one registry row (which auto-wires backend build, VM env, squid exclusion, and api-keys.env), and image/entrypoint integration to install and run `@google/gemini-cli` headless behind the gateway.

**Tech Stack:** Go 1.26 stdlib (`net/http`, `net/url`, `encoding/json`), `@google/gemini-cli@0.49.0` (Node), bash + jq (image scripts), Apple `container` (macOS integration only).

## Global Constraints

- Governing spec: `docs/superpowers/specs/2026-07-02-gemini-native-vendor-design.md`. Findings: `docs/superpowers/specs/2026-07-02-gemini-spike-findings.md`. Every task's requirements include the spec.
- **API-key only.** No OAuth. The backend is built only when `GEMINI_API_KEY` resolves (opt-in).
- **Threat model preserved (A1–A7).** The real `GEMINI_API_KEY` never enters the VM; the VM gets a per-task bearer. Egress stays deny-by-default. No invariant weakened.
- `go build ./...`, `go vet ./...`, `go test ./... -race` green; `gofmt` clean; **no new Go deps**.
- Pin `@google/gemini-cli@0.49.0` (spike-validated). Default model `gemini-2.5-pro`.
- Prices are USD per 1M tokens, approximate (≤200k-context tier), `"default"` keyed to the family high end (Pro).
- Commit trailer on every commit:
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`
  `Claude-Session: https://claude.ai/code/session_01X4BM2LKqjhHgqvaK2JnwaR`

**Execution note (read before Task 6):** Tasks 1–5 are fully CI-testable in this environment. Task 6 is a macOS/Apple-container + real-`GEMINI_API_KEY` end-to-end test — it can be *written and compiled* here but only *run* by the maintainer on macOS (same reality as the existing A1/A2 VM red-team). It is the final verification of the egress risk in the spec.

---

## Task 1: GoogleVendor + gateway admission from `x-goog-api-key`

**Files:**
- Modify: `internal/gateway/vendor.go` (add `GoogleVendor`, `stripQueryParam`; add `net/url` import)
- Modify: `internal/gateway/gateway.go` (`ServeHTTP` token extraction)
- Test: `internal/gateway/vendor_test.go`, `internal/gateway/gateway_test.go`

**Interfaces:**
- Produces: `func GoogleVendor() Vendor` — `Name:"google"`, `BaseURL:"https://generativelanguage.googleapis.com"`, `Inject` sets `X-Goog-Api-Key: <realKey>`, `ParseUsage: parseGoogleUsage` (defined in Task 2), `Prices: GooglePrices()` (Task 2). Consumed by the registry row (Task 3).
- Note: `GoogleVendor` references `parseGoogleUsage` and `GooglePrices` which Task 2 creates. To keep Task 1 compiling on its own, this task adds **temporary stubs** `parseGoogleUsage`/`GooglePrices` (returning `("",0,0,false)` and an empty map) and Task 2 replaces their bodies. The stubs are noted so the reviewer expects them.

- [ ] **Step 1: Write the failing vendor test** — `internal/gateway/vendor_test.go` (append):

```go
func TestGoogleVendor_InjectSwapsKey(t *testing.T) {
	v := GoogleVendor()
	if v.Name != "google" || v.BaseURL != "https://generativelanguage.googleapis.com" {
		t.Fatalf("unexpected vendor identity: %+v", v)
	}
	r, _ := http.NewRequest("POST",
		"https://gw/v1beta/models/gemini-2.5-pro:generateContent?key=tok_bearer&alt=sse", nil)
	r.Header.Set("X-Goog-Api-Key", "tok_bearer") // inbound = per-task bearer
	r.Header.Set("Authorization", "Bearer tok_bearer")

	v.Inject(r, "REAL-KEY")

	if got := r.Header.Get("X-Goog-Api-Key"); got != "REAL-KEY" {
		t.Errorf("x-goog-api-key = %q, want REAL-KEY", got)
	}
	if r.Header.Get("Authorization") != "" {
		t.Errorf("inbound Authorization must be removed, got %q", r.Header.Get("Authorization"))
	}
	if r.URL.Query().Has("key") {
		t.Errorf("key= query param must be stripped, got %q", r.URL.RawQuery)
	}
	if !r.URL.Query().Has("alt") {
		t.Errorf("other query params must be preserved (alt=sse)")
	}
}
```

- [ ] **Step 2: Run it — expect FAIL** (`GoogleVendor` undefined)

Run: `go test ./internal/gateway/ -run TestGoogleVendor_InjectSwapsKey`
Expected: compile error `undefined: GoogleVendor`.

- [ ] **Step 3: Implement `GoogleVendor` + `stripQueryParam` + stubs** — in `internal/gateway/vendor.go`. Add `"net/url"` to the import block, then append:

```go
// GoogleVendor is the generativelanguage.googleapis.com upstream: Google's
// x-goog-api-key auth, Gemini usageMetadata shapes, Gemini prices. The VM's
// Gemini CLI (API-key mode) sends the per-task bearer in x-goog-api-key; the
// gateway admits it (see ServeHTTP) and this Inject swaps in the real key.
func GoogleVendor() Vendor {
	return Vendor{
		Name:    "google",
		BaseURL: "https://generativelanguage.googleapis.com",
		Inject: func(r *http.Request, realKey string) {
			r.Header.Del("Authorization")
			r.Header.Del("X-Goog-Api-Key")
			// Defensive: the CLI uses the header, but strip any ?key= so a
			// per-task bearer can never leak upstream in the query string.
			stripQueryParam(r.URL, "key")
			r.Header.Set("X-Goog-Api-Key", realKey)
		},
		ParseUsage: parseGoogleUsage,
		Prices:     GooglePrices(),
	}
}

// stripQueryParam removes one query key from u in place, preserving the rest.
func stripQueryParam(u *url.URL, key string) {
	if u == nil || u.RawQuery == "" {
		return
	}
	q := u.Query()
	if q.Has(key) {
		q.Del(key)
		u.RawQuery = q.Encode()
	}
}

// --- Task 2 replaces these two stub bodies ---
func parseGoogleUsage(body []byte, contentType string) (model string, in, out int, ok bool) {
	return "", 0, 0, false
}
func GooglePrices() map[string]Price { return map[string]Price{} }
```

- [ ] **Step 4: Run vendor test — expect PASS**

Run: `go test ./internal/gateway/ -run TestGoogleVendor_InjectSwapsKey`
Expected: PASS.

- [ ] **Step 5: Write the failing admission test** — `internal/gateway/gateway_test.go` (append). This asserts the gateway admits a token presented in `x-goog-api-key`, and (regression) still admits `Authorization: Bearer`.

```go
func TestServeHTTP_AdmitsGoogleKeyHeader(t *testing.T) {
	// Fake upstream records the injected key.
	var gotKey string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Goog-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2},"modelVersion":"gemini-2.5-pro"}`))
	}))
	defer up.Close()

	v := GoogleVendor()
	v.BaseURL = up.URL // redirect upstream to the fake
	g, err := New(Backend{Vendor: v, Cred: StaticKey("REAL-KEY")})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := g.Mint("google", 1.0, 0, time.Minute)

	// Present the bearer ONLY in x-goog-api-key (how the Gemini CLI sends it).
	r := httptest.NewRequest("POST", "/v1beta/models/gemini-2.5-pro:generateContent", nil)
	r.Header.Set("X-Goog-Api-Key", tok)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (token in x-goog-api-key must admit)", w.Code)
	}
	if gotKey != "REAL-KEY" {
		t.Errorf("upstream x-goog-api-key = %q, want REAL-KEY (real key injected, bearer swapped)", gotKey)
	}
}
```

- [ ] **Step 6: Run it — expect FAIL** (401, because `ServeHTTP` only reads `Authorization`)

Run: `go test ./internal/gateway/ -run TestServeHTTP_AdmitsGoogleKeyHeader`
Expected: FAIL with `status = 401`.

- [ ] **Step 7: Implement the admission change** — in `internal/gateway/gateway.go` `ServeHTTP`, replace the token-extraction block (currently lines ~135–138):

```go
	tok := ""
	if a := r.Header.Get("Authorization"); len(a) > 7 && a[:7] == "Bearer " {
		tok = a[7:]
	} else if k := r.Header.Get("X-Goog-Api-Key"); k != "" {
		// The Gemini CLI (API-key mode) presents the per-task bearer here, not
		// as an Authorization: Bearer header. Admission is otherwise identical.
		tok = k
	}
```

- [ ] **Step 8: Run both gateway tests + the full gateway package — expect PASS**

Run: `go test ./internal/gateway/ -race`
Expected: ok (new tests pass; existing claude/codex/opencode admission tests unaffected).

- [ ] **Step 9: Verify build + gofmt/vet**

Run: `go build ./... && go vet ./internal/gateway/ && gofmt -l internal/gateway/`
Expected: clean.

- [ ] **Step 10: Commit**

```bash
git add internal/gateway/vendor.go internal/gateway/gateway.go internal/gateway/vendor_test.go internal/gateway/gateway_test.go
git commit -m "feat(gateway): GoogleVendor + admit per-task bearer from x-goog-api-key"
```

---

## Task 2: Gemini usage parser + price table

**Files:**
- Modify: `internal/gateway/usage.go` (replace the `parseGoogleUsage` stub; add `googleUsageFromJSON`)
- Modify: `internal/gateway/pricing.go` (replace the `GooglePrices` stub)
- Test: `internal/gateway/usage_test.go`, `internal/gateway/pricing_test.go`

**Interfaces:**
- Consumes: the `parseGoogleUsage`/`GooglePrices` stubs Task 1 added; this task fills their bodies (same signatures).
- Produces: `parseGoogleUsage(body, contentType) (model string, in, out int, ok bool)` reading Gemini `usageMetadata`; `GooglePrices() map[string]Price`.

- [ ] **Step 1: Write failing usage tests** — `internal/gateway/usage_test.go` (append):

```go
func TestParseGoogleUsage_NonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],
		"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":40,"thoughtsTokenCount":10,"totalTokenCount":150},
		"modelVersion":"gemini-2.5-pro"}`)
	model, in, out, ok := parseGoogleUsage(body, "application/json")
	if !ok || model != "gemini-2.5-pro" || in != 100 || out != 50 { // 40 + 10 thoughts
		t.Fatalf("got model=%q in=%d out=%d ok=%v; want gemini-2.5-pro/100/50/true", model, in, out, ok)
	}
}

func TestParseGoogleUsage_SSEKeepsLast(t *testing.T) {
	body := []byte("data: {\"usageMetadata\":{\"promptTokenCount\":100,\"candidatesTokenCount\":10},\"modelVersion\":\"gemini-2.5-flash\"}\n\n" +
		"data: {\"usageMetadata\":{\"promptTokenCount\":100,\"candidatesTokenCount\":42},\"modelVersion\":\"gemini-2.5-flash\"}\n\n")
	model, in, out, ok := parseGoogleUsage(body, "text/event-stream")
	if !ok || model != "gemini-2.5-flash" || in != 100 || out != 42 {
		t.Fatalf("got model=%q in=%d out=%d ok=%v; want gemini-2.5-flash/100/42/true (last wins)", model, in, out, ok)
	}
}

func TestParseGoogleUsage_NoUsage(t *testing.T) {
	if _, _, _, ok := parseGoogleUsage([]byte(`{"candidates":[]}`), "application/json"); ok {
		t.Error("a body with no usageMetadata must return ok=false")
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (stub returns ok=false)

Run: `go test ./internal/gateway/ -run TestParseGoogleUsage`
Expected: FAIL (stub returns `"",0,0,false`).

- [ ] **Step 3: Implement `parseGoogleUsage`** — in `internal/gateway/usage.go`, replace the stub added in Task 1 with:

```go
// parseGoogleUsage extracts (model, input, output) from a Gemini response.
// Input = promptTokenCount; output = candidatesTokenCount + thoughtsTokenCount
// (Gemini 2.5 bills thinking tokens as output). Handles non-streaming JSON and
// SSE (alt=sse), keeping the last usageMetadata seen (the final chunk carries
// the cumulative totals). model comes from modelVersion.
func parseGoogleUsage(body []byte, contentType string) (model string, in, out int, ok bool) {
	if strings.Contains(contentType, "text/event-stream") {
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			if m, i, o, k := googleUsageFromJSON([]byte(data)); k {
				model, in, out, ok = m, i, o, k
			}
		}
		return
	}
	return googleUsageFromJSON(body)
}

func googleUsageFromJSON(b []byte) (model string, in, out int, ok bool) {
	var m struct {
		ModelVersion  string `json:"modelVersion"`
		UsageMetadata *struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
		} `json:"usageMetadata"`
	}
	if json.Unmarshal(b, &m) != nil || m.UsageMetadata == nil {
		return "", 0, 0, false
	}
	in = m.UsageMetadata.PromptTokenCount
	out = m.UsageMetadata.CandidatesTokenCount + m.UsageMetadata.ThoughtsTokenCount
	if in == 0 && out == 0 {
		return "", 0, 0, false
	}
	return m.ModelVersion, in, out, true
}
```

(`usage.go` already imports `encoding/json` and `strings` — no import change.)

- [ ] **Step 4: Write failing pricing test** — `internal/gateway/pricing_test.go` (append):

```go
func TestGooglePrices_MetersKnownAndDefault(t *testing.T) {
	p := GooglePrices()
	if _, ok := p["gemini-2.5-pro"]; !ok {
		t.Fatal("missing gemini-2.5-pro")
	}
	if _, ok := p["default"]; !ok {
		t.Fatal("missing default fallback")
	}
	// 1M in + 1M out on Pro = 1.25 + 10 = 11.25
	if got := cost(p, "gemini-2.5-pro", 1_000_000, 1_000_000); got != 11.25 {
		t.Errorf("pro cost = %v, want 11.25", got)
	}
	// Unknown model falls back to default (Pro high end), not $0.
	if got := cost(p, "gemini-9-ultra", 1_000_000, 0); got != 1.25 {
		t.Errorf("unknown-model input cost = %v, want 1.25 (default)", got)
	}
}
```

- [ ] **Step 5: Run — expect FAIL** (stub returns empty map → cost 0)

Run: `go test ./internal/gateway/ -run TestGooglePrices`
Expected: FAIL.

- [ ] **Step 6: Implement `GooglePrices`** — in `internal/gateway/pricing.go`, replace the stub with:

```go
// GooglePrices seeds the per-task budget gate for Gemini tasks. USD per 1M
// tokens, approximate (≤200k-context tier; the >200k tier carries a premium not
// modeled here). "default" is keyed to Pro (the family high end) so a new model
// can't overrun the budget before this table catches up. Keys match the
// modelVersion string parseGoogleUsage extracts.
func GooglePrices() map[string]Price {
	return map[string]Price{
		"gemini-2.5-pro":        {InputPer1M: 1.25, OutputPer1M: 10},
		"gemini-2.5-flash":      {InputPer1M: 0.30, OutputPer1M: 2.50},
		"gemini-2.5-flash-lite": {InputPer1M: 0.10, OutputPer1M: 0.40},
		"default":               {InputPer1M: 1.25, OutputPer1M: 10},
	}
}
```

- [ ] **Step 7: Run gateway package — expect PASS**

Run: `go test ./internal/gateway/ -race`
Expected: ok (usage + pricing pass; the Task 1 admission test now meters a real cost via the real parser).

- [ ] **Step 8: Verify + commit**

Run: `go build ./... && gofmt -l internal/gateway/`

```bash
git add internal/gateway/usage.go internal/gateway/pricing.go internal/gateway/usage_test.go internal/gateway/pricing_test.go
git commit -m "feat(gateway): Gemini usageMetadata parser + price table"
```

---

## Task 3: The `gemini → google` registry row

**Files:**
- Modify: `internal/provider/provider.go` (add the row)
- Test: `internal/provider/provider_test.go`, `cmd/brokerd/backends_test.go`

**Interfaces:**
- Consumes: `gateway.GoogleVendor` (Task 1).
- Produces: `provider.ByAgent("gemini")` / `ByVendor("google")` returning a complete row; auto-wires backend build (via the existing `default` api-key branch — no `buildBackends` change), VM env (`GOOGLE_GEMINI_BASE_URL`/`GEMINI_API_KEY`), `GatewayHosts()` inclusion, and `knownAPIKeys` inclusion.

- [ ] **Step 1: Write failing registry tests** — `internal/provider/provider_test.go` (append):

```go
func TestRegistry_GeminiRow(t *testing.T) {
	p, ok := ByAgent("gemini")
	if !ok {
		t.Fatal("gemini agent not registered")
	}
	if p.Vendor != "google" || p.APIKeyEnv != "GEMINI_API_KEY" ||
		p.BaseURLEnv != "GOOGLE_GEMINI_BASE_URL" || p.TokenEnv != "GEMINI_API_KEY" {
		t.Errorf("gemini row fields wrong: %+v", p)
	}
	if p.APIVendor == nil {
		t.Error("gemini must have a static APIVendor (native, not config-built)")
	}
	if p.ConfigBuilt || p.OAuthBackend != nil {
		t.Error("gemini is api-key-only native: ConfigBuilt=false, OAuthBackend=nil")
	}
	if !p.NoOperatorDefault {
		t.Error("gemini must set NoOperatorDefault (operator default_model is claude/codex-oriented)")
	}
	if v := p.APIVendor(); v.Name != "google" {
		t.Errorf("APIVendor().Name = %q, want google", v.Name)
	}
}

func TestGatewayHosts_IncludesGemini(t *testing.T) {
	if !GatewayHosts()["generativelanguage.googleapis.com"] {
		t.Error("GatewayHosts must exclude the Gemini API host from the squid allowlist")
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (gemini not registered)

Run: `go test ./internal/provider/ -run 'TestRegistry_GeminiRow|TestGatewayHosts_IncludesGemini'`
Expected: FAIL.

- [ ] **Step 3: Add the row** — in `internal/provider/provider.go`, insert into the `Registry` slice literal **after the `codex` row and before the `opencode` row**:

```go
	{
		Agent: "gemini", Vendor: "google", Label: "Gemini (Google)",
		APIKeyEnv: "GEMINI_API_KEY", AuthCmd: "", // api-key only; no auth subcommand
		BaseURLEnv:        "GOOGLE_GEMINI_BASE_URL",
		TokenEnv:          "GEMINI_API_KEY",
		APIVendor:         gateway.GoogleVendor,
		NoOperatorDefault: true, // operator default_model is claude/codex-oriented; the entrypoint supplies the gemini default
		// OAuthBackend / OAuthFile / LoadOAuthSnap nil; ConfigBuilt false; NeedsModel false.
	},
```

- [ ] **Step 4: Run provider tests — expect PASS**

Run: `go test ./internal/provider/ -race`
Expected: ok. (The existing `TestGatewayHosts` "one host per static-APIVendor provider" invariant now expects 3 hosts — if it hardcodes a count, update it to derive from the registry; the row adds `generativelanguage.googleapis.com`.)

- [ ] **Step 5: Write the failing backend-wiring test** — `cmd/brokerd/backends_test.go` (append). Confirms the google backend is built iff `GEMINI_API_KEY` resolves, with **no `buildBackends` change**:

```go
func TestBuildBackends_GeminiOptIn(t *testing.T) {
	base := &config.Config{} // no auth-mode fields set → api_key default for all
	// Absent key: no google backend.
	bs, err := buildBackends(base, map[string]string{"ANTHROPIC_API_KEY": "sk-a"})
	if err != nil {
		t.Fatal(err)
	}
	if hasVendor(bs, "google") {
		t.Error("google backend must be absent when GEMINI_API_KEY is unset")
	}
	// Present key: google backend appears.
	bs, err = buildBackends(base, map[string]string{"ANTHROPIC_API_KEY": "sk-a", "GEMINI_API_KEY": "sk-g"})
	if err != nil {
		t.Fatal(err)
	}
	if !hasVendor(bs, "google") {
		t.Error("google backend must be built when GEMINI_API_KEY resolves")
	}
}

// hasVendor: if this helper doesn't already exist in the test file, add it.
func hasVendor(bs []gateway.Backend, name string) bool {
	for _, b := range bs {
		if b.Vendor.Name == name {
			return true
		}
	}
	return false
}
```

(Read `cmd/brokerd/backends_test.go` first: it may already have a `hasVendor`-equivalent or a config constructor — reuse the existing helper/pattern rather than duplicating. The `config.Config{}` zero value must be a valid api-key config; if the existing tests build config differently, match them.)

- [ ] **Step 6: Run — expect PASS** (no production change needed; the `default` branch already handles it)

Run: `go test ./cmd/brokerd/ -run TestBuildBackends_GeminiOptIn -race`
Expected: ok. If it FAILS because the google backend isn't built, STOP — that means an assumption in the spec is wrong; report it (do not hack `buildBackends`). Expected outcome per the spec analysis: it passes with zero `buildBackends` edits.

- [ ] **Step 7: Full build + suite**

Run: `go build ./... && go test ./... -race 2>&1 | tail -3 && gofmt -l internal/provider/ cmd/brokerd/`
Expected: green, clean.

- [ ] **Step 8: Commit**

```bash
git add internal/provider/provider.go internal/provider/provider_test.go cmd/brokerd/backends_test.go
git commit -m "feat(provider): gemini → google registry row (native vendor, api-key only)"
```

---

## Task 4: Image install + settings script + entrypoint case

**Files:**
- Create: `image/write-gemini-config.sh`
- Modify: `image/Dockerfile`, `image/entrypoint.sh`
- Test: `tests/imagescripts/geminiconfig_test.go` (mirror `opencodeconfig_test.go`)

**Interfaces:**
- Consumes: the VM env the grant injects (`GOOGLE_GEMINI_BASE_URL`, `GEMINI_API_KEY`) and `DRYDOCK_AGENT=gemini`, `DRYDOCK_MODEL` (optional).
- Produces: an in-image `gemini)` dispatch that runs the CLI headless behind the gateway.

- [ ] **Step 1: Read the patterns** — `image/write-opencode-config.sh`, the `opencode)` case in `image/entrypoint.sh`, `tests/imagescripts/opencodeconfig_test.go`, and the CLI install lines in `image/Dockerfile`. Match their conventions (jq for JSON, `set -euo pipefail`, XDG under `/home/agent`, the `## help` Makefile-comment style is not relevant here).

- [ ] **Step 2: Write the failing imagescripts test** — `tests/imagescripts/geminiconfig_test.go`:

```go
package imagescripts

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Verifies write-gemini-config.sh emits a settings.json that pins API-key auth
// and disables phone-home, so the CLI runs headless inside deny-by-default egress.
func TestWriteGeminiConfig(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("jq required in CI")
		}
		t.Skip("jq not installed")
	}
	dir := t.TempDir()
	cmd := exec.Command("bash", "../../image/write-gemini-config.sh", filepath.Join(dir, ".gemini"))
	cmd.Env = append(os.Environ(), "GEMINI_API_KEY=tok_test")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".gemini", "settings.json"))
	if err != nil {
		t.Fatalf("settings.json not written: %v", err)
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("settings.json not valid JSON: %v\n%s", err, raw)
	}
	sec, _ := s["security"].(map[string]any)
	auth, _ := sec["auth"].(map[string]any)
	if auth["selectedType"] != "gemini-api-key" {
		t.Errorf("security.auth.selectedType = %v, want gemini-api-key", auth["selectedType"])
	}
}
```

- [ ] **Step 3: Run — expect FAIL** (script missing)

Run: `go test ./tests/imagescripts/ -run TestWriteGeminiConfig`
Expected: FAIL (script not found).

- [ ] **Step 4: Create `image/write-gemini-config.sh`**:

```bash
#!/usr/bin/env bash
# Write a Gemini CLI settings.json that (1) pins API-key auth — mandatory because
# @google/gemini-cli 0.49.0 sees GOOGLE_GEMINI_BASE_URL and otherwise auto-selects
# a "gateway" auth type its non-interactive validator rejects (exit 41) — and
# (2) disables every phone-home so the CLI stays within deny-by-default egress
# (it may reach only the drydock gateway + squid). Written under the agent's
# home, never /work, so it can't land in the captured diff.
#
# Usage: write-gemini-config.sh <gemini-dir>
set -euo pipefail
GEMINI_DIR="${1:?usage: write-gemini-config.sh <gemini-dir>}"
: "${GEMINI_API_KEY:?missing GEMINI_API_KEY}"

mkdir -p "$GEMINI_DIR"
jq -n '{
  security: { auth: { selectedType: "gemini-api-key" } },
  telemetry: { enabled: false },
  usageStatisticsEnabled: false,
  general: { checkForUpdates: false }
}' > "$GEMINI_DIR/settings.json"
```

Note for the implementer: the exact phone-home key names (`telemetry.enabled`, `usageStatisticsEnabled`, `general.checkForUpdates`) are the 0.49.0 settings-schema names; if `gemini --version`'s bundled schema differs, correct them to the real keys and note it. The `selectedType` key is confirmed by the spike findings and is the load-bearing one the test asserts.

- [ ] **Step 5: Run — expect PASS**

Run: `go test ./tests/imagescripts/ -run TestWriteGeminiConfig`
Expected: PASS.

- [ ] **Step 6: Add the Dockerfile install** — in `image/Dockerfile`, after the opencode install block, add (matching the pin-arg style of the other CLIs):

```dockerfile
# Pin the Gemini CLI for the native google vendor. Bump deliberately; every
# drydock release anchors to a known version (0.49.0 validated by the 3B spike).
ARG GEMINI_CLI_VERSION=0.49.0
RUN npm install -g @google/gemini-cli@${GEMINI_CLI_VERSION} && gemini --version
```

And add `write-gemini-config.sh` to the existing `COPY` + `chmod 0755` lines alongside the other `write-*-config.sh` scripts.

- [ ] **Step 7: Add the entrypoint case** — in `image/entrypoint.sh`, add a `gemini)` case (place before the `*)` default), modeled on `opencode)`:

```bash
  gemini)
    # The gateway injects GOOGLE_GEMINI_BASE_URL + GEMINI_API_KEY (per-task
    # bearer). The CLI (API-key mode) sends the bearer in x-goog-api-key; the
    # gateway admits it and swaps in the real key. A settings.json pinning
    # api-key auth is MANDATORY (env alone makes 0.49.0 pick a rejected auth
    # type); it also disables phone-home so the CLI stays within egress limits.
    : "${GOOGLE_GEMINI_BASE_URL:?missing GOOGLE_GEMINI_BASE_URL}"
    : "${GEMINI_API_KEY:?missing GEMINI_API_KEY}"
    MODEL="${DRYDOCK_MODEL:-gemini-2.5-pro}"
    export GEMINI_DIR=/home/agent/.gemini
    /usr/local/bin/write-gemini-config.sh "$GEMINI_DIR"
    chown -R agent:agent "$GEMINI_DIR"
    # VM is the isolation boundary: trust the workspace, skip the trust prompt.
    exec gosu agent env \
        "HOME=/home/agent" \
        "GEMINI_DIR=$GEMINI_DIR" \
        "GOOGLE_GEMINI_BASE_URL=$GOOGLE_GEMINI_BASE_URL" \
        "GEMINI_API_KEY=$GEMINI_API_KEY" \
        "GEMINI_CLI_TRUST_WORKSPACE=true" \
        gemini -p "${PROMPT}" -m "${MODEL}" --skip-trust
    ;;
```

- [ ] **Step 8: Verify scripts lint + full suite**

Run: `bash -n image/entrypoint.sh && bash -n image/write-gemini-config.sh` (syntax check), then `go build ./... && go test ./... -race 2>&1 | tail -3`.
Expected: no bash syntax errors; suite green. (The Dockerfile/entrypoint runtime behavior is verified in Task 6 on macOS.)

- [ ] **Step 9: Commit**

```bash
git add image/write-gemini-config.sh image/Dockerfile image/entrypoint.sh tests/imagescripts/geminiconfig_test.go
git commit -m "feat(image): install gemini-cli, write api-key settings, dispatch gemini agent"
```

---

## Task 5: Doctor check + docs + CHANGELOG

**Files:**
- Modify: `cmd/drydock/doctor.go` (gemini-presence check via the `runCmd` seam)
- Modify: `site/docs/models.md`, `site/docs/authentication.md`, `site/docs/configuration.md`, `CHANGELOG.md`
- Test: `cmd/drydock/doctor_test.go` (if a doctor test file exists; else add one using the `runCmd` seam)

**Interfaces:**
- Consumes: the `runCmd` package var seam in `cmd/drydock` (added in the earlier sweep) so the container exec is injectable in tests.

- [ ] **Step 1: Read `cmd/drydock/doctor.go`** — the `codex present` block (~lines 65–78) and how `runCmd` is used. Mirror it for gemini.

- [ ] **Step 2: Write a failing doctor test** — `cmd/drydock/doctor_test.go` (append or create). Use the `runCmd` seam to fake the container output:

```go
func TestDoctor_GeminiPresence(t *testing.T) {
	orig := runCmd
	t.Cleanup(func() { runCmd = orig })
	// Fake: gemini --version returns a version; assert geminiPresent parses it.
	runCmd = func(name string, args ...string) ([]byte, error) {
		if len(args) > 0 && contains(args, "gemini --version") {
			return []byte("0.49.0"), nil
		}
		return []byte(""), nil
	}
	if !geminiPresent("0.49.0", nil) {
		t.Error("geminiPresent must accept a version string")
	}
	if geminiPresent("", assertErr) {
		t.Error("geminiPresent must reject an error/empty output")
	}
}
```

Adjust to whatever small pure helper you extract (`geminiPresent(out string, err error) bool`, mirroring `codexPresent`). If `codexPresent` is the exact shape you need, model `geminiPresent` on it. `contains`/`assertErr` are trivial test helpers — inline them or reuse existing ones.

- [ ] **Step 3: Run — expect FAIL** (`geminiPresent` undefined)

Run: `go test ./cmd/drydock/ -run TestDoctor_GeminiPresence`
Expected: FAIL.

- [ ] **Step 4: Implement the doctor check** — add a `geminiPresent` helper mirroring `codexPresent`, and a check block in `runDoctor` after the codex block:

```go
	// 2c. Gemini CLI presence (native google vendor). Absence usually means the
	// image predates native Gemini — point at `drydock init` rather than a raw
	// shell error.
	out, err = runCmd("container", "run", "--rm", "--entrypoint", "/bin/sh",
		cfg.SandboxImage, "-c", "gemini --version 2>&1")
	if geminiPresent(string(out), err) {
		step("gemini present", true, strings.TrimSpace(lastLine(string(out))))
	} else {
		step("gemini present", false, "not found in "+cfg.SandboxImage)
		fmt.Println("    → that image likely predates native Gemini. Fix: run `drydock init` to rebuild")
		failed = true
	}
```

```go
// geminiPresent reports whether `gemini --version` returned a usable version.
func geminiPresent(out string, err error) bool {
	return err == nil && strings.TrimSpace(out) != ""
}
```

- [ ] **Step 5: Run — expect PASS**

Run: `go test ./cmd/drydock/ -run TestDoctor_GeminiPresence`
Expected: PASS.

- [ ] **Step 6: Update docs** — reflect the native gemini lane:
  - `site/docs/models.md`: add a "Gemini (native)" section — `--agent gemini`, `GEMINI_API_KEY`, default model `gemini-2.5-pro`, models `gemini-2.5-pro`/`flash`/`flash-lite`, note it's API-key only (no subscription). Contrast with the existing openai-compat route to Gemini.
  - `site/docs/authentication.md`: add gemini to the api-key matrix (`GEMINI_API_KEY` host env / `api-keys.env`); note no subscription mode.
  - `site/docs/configuration.md`: note `default_agent: gemini` is valid and `GEMINI_API_KEY` is a recognized key.
  - `CHANGELOG.md` `Unreleased`: "Native Gemini vendor (`--agent gemini`) — Google `x-goog-api-key` brokering, native token metering, API-key auth."

- [ ] **Step 7: Verify docs build + suite**

Run: `go run ./cmd/docs-build` (no error, sidebar resolves), `go build ./... && go test ./... -race 2>&1 | tail -3`, `gofmt -l cmd/drydock/`.
Expected: clean, green.

- [ ] **Step 8: Commit**

```bash
git add cmd/drydock/doctor.go cmd/drydock/doctor_test.go site/docs/models.md site/docs/authentication.md site/docs/configuration.md CHANGELOG.md
git commit -m "feat(gemini): doctor presence check + docs + changelog"
```

---

## Task 6: macOS-gated integration + red-team (egress verification)

**Files:**
- Create: `tests/integration/gemini_test.go` (build tag `integration`)

**Interfaces:**
- Consumes: the built sandbox image (with gemini-cli, from Task 4) + a real `GEMINI_API_KEY` + a running Apple `container` runtime.

**IMPORTANT — environment reality:** This test requires macOS/Apple silicon, the `container` runtime, a rebuilt sandbox image, and a real `GEMINI_API_KEY`. The implementer CAN write it and confirm it COMPILES under the `integration` tag (`go vet -tags integration ./tests/integration/`), but likely CANNOT run it here. Running it is the maintainer's step (`make test-integration` on macOS). Report this clearly; do not fake a pass. This is the spec's egress-risk verification: it is where an unexpected CLI phone-home would surface.

- [ ] **Step 1: Read** `tests/integration/redteam_test.go` for the A1/A2 harness (how it submits a task, asserts VM env has the bearer not the real key, and asserts egress deny-by-default). Model the gemini case on it.

- [ ] **Step 2: Write `tests/integration/gemini_test.go`** (build tag `integration`), with three checks mirroring the existing red-team style:
  - **A1**: submit a `--agent gemini` task with a sentinel real `GEMINI_API_KEY`; assert the container env carries `GEMINI_API_KEY=tok_...` (bearer) and NEVER the sentinel real key; assert the gateway forwards the real key upstream.
  - **A2**: assert the gemini sandbox enforces deny-by-default egress (a non-allowlisted host is blocked) — reuse the existing egress assertion helper.
  - **End-to-end**: submit a trivial gemini task and assert it completes through the gateway (this exercises the real CLI in the sandbox — the egress-risk check). Skip with a clear message if `GEMINI_API_KEY` is unset.

  Use the existing integration helpers/fixtures; do not invent a parallel harness. Keep assertions behavioral (env contents, block/allow, task outcome), not string-shape.

- [ ] **Step 3: Verify it compiles under the tag**

Run: `go vet -tags integration ./tests/integration/`
Expected: clean compile. (Do NOT expect to run it here.)

- [ ] **Step 4: Commit**

```bash
git add tests/integration/gemini_test.go
git commit -m "test(integration): gemini A1/A2 + end-to-end (macOS-gated, egress verification)"
```

- [ ] **Step 5: Report the maintainer-run requirement** — in the task report, state explicitly that Task 6 must be run by the maintainer via `make test-integration` on macOS with a rebuilt image + real `GEMINI_API_KEY`, and that a GREEN there is the true completion of the egress-risk verification. If the maintainer's run reveals a required non-gateway host, the follow-up is either the CLI's disabling flag or a narrow squid allowlist entry.

---

## Self-review notes

- **Spec coverage:** §1 admission → T1; §2 GoogleVendor → T1; §3 parser → T2; §4 prices → T2; §5 registry row (+ auto-wiring) → T3; §6 image/entrypoint → T4; §7 doctor → T5; docs/CHANGELOG → T5; test plan (unit) → T1–T3, T5; test plan (macOS) → T6; egress risk → T4 (settings disable phone-home) + T6 (verification). Model-resolution correctness (`NoOperatorDefault`) → T3 assertion.
- **Type consistency:** `GoogleVendor`/`parseGoogleUsage`/`GooglePrices` signatures identical across T1 (stubs) → T2 (bodies); `Price{InputPer1M,OutputPer1M}` and `cost()` match `pricing.go`; the registry row fields match `provider.Provider`. The T1 stubs are called out so the T1 reviewer expects them and the T2 reviewer expects them replaced.
- **No `buildBackends`/brokerd change** is an explicit, tested assumption (T3 Step 6) — if it proves false, the implementer stops and reports rather than hacking.
- **Placeholder honesty:** the phone-home settings key names (T4) and the exact integration helpers (T6) are flagged as "confirm against the pinned CLI / existing harness" — genuine version/harness-specific discovery, with the load-bearing `selectedType` key pinned and asserted.
