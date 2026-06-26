# Provider registry refactor (Phase 3A) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Collapse the ~8 hardcoded `claude`/`codex` provider sites into one `internal/provider.Registry` the CLI/config layer reads, so a new provider becomes a registry row + a gateway `Vendor` — behavior-identical for the existing two agents.

**Architecture:** A new leaf package `internal/provider` (imports `gateway`, never `config`) holds a `Provider` descriptor + `Registry`. `config`/`agent`/`brokerd`/wizard/auth/doctor consume it. Import graph stays cycle-free: `provider → gateway → creds` (terminal); `config → provider`, `agent → provider`.

**Tech Stack:** Go 1.26.4, standard library + the existing gateway/creds packages.

## Global Constraints

- **Standard library only**, no new deps. Go 1.26.4. Ends green on `go build ./... && go vet ./... && go test ./...`, gofmt clean.
- **Behavior-identical** with ONE deliberate exception: the "set at least one credential" startup error generalizes from naming the two key env vars to a provider-agnostic message (Task 3). Everything else — `claude`/`codex` agents, config-file loading, wizard/auth/doctor output, all existing tests — is unchanged.
- **No import cycle:** `internal/provider` imports `gateway` (+ stdlib) and **never** `config`. The `OAuthBackend` hook takes `cfgDir string` as a parameter (brokerd passes `config.Dir()`), so provider never calls `config.Dir()` itself.
- **`grant.EnvVars` must stay byte-identical:** anthropic → `ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN` (note: `AUTH_TOKEN`, not `API_KEY`); openai → `OPENAI_BASE_URL` + `OPENAI_API_KEY`. This is the highest-risk site — pin it with a test.
- **Empty-agent default is the literal `claude`**, never `Registry[0]`.

---

### Task 1: The `internal/provider` package

A standalone new package — no consumers yet, so it compiles and tests in isolation. The `OAuthBackend` closures are lifted verbatim from `cmd/brokerd/main.go`'s current `switch` arms.

**Files:**
- Create: `internal/provider/provider.go`
- Test: `internal/provider/provider_test.go`

**Interfaces:**
- Produces: `provider.Provider` struct; `provider.Registry`; `provider.ByAgent(string)(Provider,bool)`; `provider.ByVendor(string)(Provider,bool)`; `provider.Agents()[]string`; `provider.Labels()[]string`.

- [ ] **Step 1: Write the registry tests**

Create `internal/provider/provider_test.go`:

```go
package provider

import "testing"

func TestRegistry_AgentsAndLabels(t *testing.T) {
	if got := Agents(); len(got) != 2 || got[0] != "claude" || got[1] != "codex" {
		t.Errorf("Agents() = %v, want [claude codex]", got)
	}
	labels := Labels()
	if len(labels) != 2 || labels[0] != "Claude Code (Anthropic)" || labels[1] != "OpenAI Codex" {
		t.Errorf("Labels() = %v", labels)
	}
}

func TestRegistry_Lookups(t *testing.T) {
	if p, ok := ByAgent("codex"); !ok || p.Vendor != "openai" {
		t.Errorf("ByAgent(codex) = %+v,%v", p, ok)
	}
	if _, ok := ByAgent("nope"); ok {
		t.Error("ByAgent(nope) should miss")
	}
	if p, ok := ByVendor("anthropic"); !ok || p.Agent != "claude" {
		t.Errorf("ByVendor(anthropic) = %+v,%v", p, ok)
	}
	if _, ok := ByVendor("nope"); ok {
		t.Error("ByVendor(nope) should miss")
	}
}

// Guards a future malformed row: every entry must carry the fields the
// CLI/config layer relies on.
func TestRegistry_EntriesComplete(t *testing.T) {
	for _, p := range Registry {
		if p.Agent == "" || p.Vendor == "" || p.Label == "" || p.APIKeyEnv == "" ||
			p.AuthCmd == "" || p.BaseURLEnv == "" || p.TokenEnv == "" || p.APIVendor == nil {
			t.Errorf("incomplete registry entry: %+v", p)
		}
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/provider/`
Expected: FAIL — package/symbols don't exist.

- [ ] **Step 3: Write `provider.go`**

Create `internal/provider/provider.go`. The two `OAuthBackend` closures are copied verbatim from `cmd/brokerd/main.go` lines ~230-256 (the `subscription` arms), with `config.Dir()` replaced by the `cfgDir` parameter:

```go
// Package provider is the single registry of coding-agent CLIs and the upstream
// API each talks to. The CLI/config layer enumerates providers from here so a
// new provider is one row, not edits across the codebase. Imports gateway only
// (never config — the OAuth hook takes cfgDir as a parameter).
package provider

import (
	"path/filepath"

	"drydock/internal/gateway"
)

// Provider is the static description of one agent + its upstream vendor.
type Provider struct {
	Agent      string // sandbox CLI: "claude", "codex"
	Vendor     string // gateway vendor name: "anthropic", "openai"
	Label      string // wizard display
	APIKeyEnv  string // host env holding the real key
	AuthCmd    string // remediation hint, e.g. "drydock auth claude"
	BaseURLEnv string // env injected into the VM: base URL var
	TokenEnv   string // env injected into the VM: token var
	APIVendor  func() gateway.Vendor                                  // API-key-mode vendor (static)
	OAuthBackend func(cfgDir string) (gateway.Backend, error)         // subscription mode; nil if unsupported
}

var Registry = []Provider{
	{
		Agent: "claude", Vendor: "anthropic", Label: "Claude Code (Anthropic)",
		APIKeyEnv: "ANTHROPIC_API_KEY", AuthCmd: "drydock auth claude",
		BaseURLEnv: "ANTHROPIC_BASE_URL", TokenEnv: "ANTHROPIC_AUTH_TOKEN",
		APIVendor: gateway.AnthropicVendor,
		OAuthBackend: func(cfgDir string) (gateway.Backend, error) {
			store := gateway.FileCredStore(filepath.Join(cfgDir, "claude-oauth.json"))
			snap, err := store.Load()
			if err != nil {
				return gateway.Backend{}, err
			}
			return gateway.Backend{Vendor: gateway.AnthropicOAuthVendor(), Cred: gateway.NewOAuthCred(snap, store)}, nil
		},
	},
	{
		Agent: "codex", Vendor: "openai", Label: "OpenAI Codex",
		APIKeyEnv: "OPENAI_API_KEY", AuthCmd: "drydock auth codex",
		BaseURLEnv: "OPENAI_BASE_URL", TokenEnv: "OPENAI_API_KEY",
		APIVendor: gateway.OpenAIVendor,
		OAuthBackend: func(cfgDir string) (gateway.Backend, error) {
			store := gateway.NewCodexStore(filepath.Join(cfgDir, "codex-oauth.json"))
			snap, err := store.Load()
			if err != nil {
				return gateway.Backend{}, err
			}
			return gateway.Backend{Vendor: gateway.OpenAIOAuthVendor(store.AccountID()), Cred: gateway.NewOAuthCredCodex(snap, store)}, nil
		},
	},
}

func ByAgent(agent string) (Provider, bool) {
	for _, p := range Registry {
		if p.Agent == agent {
			return p, true
		}
	}
	return Provider{}, false
}

func ByVendor(vendor string) (Provider, bool) {
	for _, p := range Registry {
		if p.Vendor == vendor {
			return p, true
		}
	}
	return Provider{}, false
}

func Agents() []string {
	out := make([]string, len(Registry))
	for i, p := range Registry {
		out[i] = p.Agent
	}
	return out
}

func Labels() []string {
	out := make([]string, len(Registry))
	for i, p := range Registry {
		out[i] = p.Label
	}
	return out
}
```

Before writing, open `cmd/brokerd/main.go` lines ~230-256 and confirm the closures match the real store/vendor/cred constructor names (`FileCredStore`, `NewCodexStore`, `AnthropicOAuthVendor`, `OpenAIOAuthVendor`, `NewOAuthCred`, `NewOAuthCredCodex`, `store.AccountID()`). If any name differs, use the real one — the closures must be byte-equivalent to today's wiring.

- [ ] **Step 4: Run the tests; build/vet/fmt**

Run: `gofmt -w internal/provider/ && go test ./internal/provider/ && go build ./... && go vet ./internal/provider/`
Expected: tests PASS, build clean.

- [ ] **Step 5: Commit**

```bash
git add internal/provider/
git commit -m "provider: registry of agents+vendors (foundation, no consumers yet)"
```

---

### Task 2: `agent` + `config` consume the registry

Wire the read-only consumers: agent→vendor mapping and config validation/auth accessor. Behavior-identical — existing `agent` and `config` tests pass unchanged.

**Files:**
- Modify: `internal/agent/agent.go`
- Modify: `internal/config/config.go` (validate; add `AuthMode`)
- Test: existing `internal/agent/agent_test.go` and `internal/config/config_test.go` pass unchanged; add a small `AuthMode` test.

**Interfaces:**
- Consumes: `provider` (Task 1).
- Produces: `config.AuthMode(vendor string) string`.

- [ ] **Step 1: Rewrite `agent.Vendor` to delegate**

Replace `internal/agent/agent.go`'s `Vendor` with:

```go
// Package agent maps a coding-agent CLI name to the API vendor it talks to,
// reading the single source of truth in internal/provider.
package agent

import "drydock/internal/provider"

// Vendor returns the gateway vendor backing an agent CLI. Empty agent means the
// claude default. Unknown agents return ok=false so callers fail closed.
func Vendor(name string) (string, bool) {
	if name == "" {
		name = "claude" // empty default is claude specifically, not Registry[0]
	}
	if p, ok := provider.ByAgent(name); ok {
		return p.Vendor, true
	}
	return "", false
}
```

- [ ] **Step 2: Run agent tests (must pass UNCHANGED)**

Run: `go test ./internal/agent/`
Expected: PASS — `TestVendor` (""→anthropic, claude→anthropic, codex→openai, bogus→"",false) is the behavior-identical proof.

- [ ] **Step 3: Registry-drive config validation + add `AuthMode`**

In `internal/config/config.go`: add `"drydock/internal/provider"` to imports. Replace the `default_agent` check (~line 293) with a registry-driven one:

```go
	if _, ok := provider.ByAgent(c.DefaultAgent); !ok {
		return fmt.Errorf("config: default_agent must be one of %v, got %q", provider.Agents(), c.DefaultAgent)
	}
```

Keep the `anthropic_auth`/`openai_auth` field validations unchanged. Add the accessor (near the other `Config` methods):

```go
// AuthMode returns the configured auth mode ("api_key" | "subscription") for a
// gateway vendor, reading the typed per-vendor field. Unknown vendor -> "".
func (c *Config) AuthMode(vendor string) string {
	switch vendor {
	case "anthropic":
		return c.AnthropicAuth
	case "openai":
		return c.OpenAIAuth
	default:
		return ""
	}
}
```

- [ ] **Step 4: Add an `AuthMode` test**

Append to `internal/config/config_test.go`:

```go
func TestAuthMode(t *testing.T) {
	c := &Config{AnthropicAuth: "subscription", OpenAIAuth: "api_key"}
	if c.AuthMode("anthropic") != "subscription" {
		t.Errorf("anthropic = %q", c.AuthMode("anthropic"))
	}
	if c.AuthMode("openai") != "api_key" {
		t.Errorf("openai = %q", c.AuthMode("openai"))
	}
	if c.AuthMode("nope") != "" {
		t.Errorf("unknown vendor should be empty, got %q", c.AuthMode("nope"))
	}
}
```

- [ ] **Step 5: Run config tests (existing pass unchanged + new)**

Run: `go test ./internal/config/`
Expected: PASS — including the existing `default_agent` validation tests (behavior-identical: claude/codex accepted, unknown rejected) and `TestAuthMode`.

- [ ] **Step 6: Build, vet, fmt, commit**

Run: `gofmt -w internal/agent/ internal/config/ && go build ./... && go vet ./...`

```bash
git add internal/agent/ internal/config/
git commit -m "agent/config: read provider registry (vendor map + default_agent + AuthMode)"
```

---

### Task 3: `gateway` env-name fields + `brokerd` generic backend loop

The core. `gateway.Provider`/`grant` carry the injected env-var names (no more vendor switch); brokerd replaces its two hand-written `switch cfg.*Auth` blocks + the credential guard with one registry loop. These couple (the gateway fields must be set by brokerd), so they land together.

**Files:**
- Modify: `internal/gateway/provider.go` (`Provider`, `grant`, `Mint`, `EnvVars`)
- Test: `internal/gateway/*_test.go` (pin `EnvVars`)
- Modify: `cmd/brokerd/main.go` (credential guard ~142-150; backend switch ~230-256; provider-map ~270-286; "agents available" log ~287; boot guard ~290-296)

**Interfaces:**
- Consumes: `provider` (Task 1), `config.AuthMode` (Task 2).
- Produces: `gateway.Provider{…, BaseURLEnv, TokenEnv string}`; `grant.EnvVars()` driven by those names.

- [ ] **Step 1: Pin `grant.EnvVars` with a test FIRST**

Add to a `gateway` test file (e.g. `internal/gateway/provider_test.go`, create if absent). This must pass both before (current switch) and after (generalized) — it's the behavior-identical anchor:

```go
func TestGrantEnvVars(t *testing.T) {
	cases := []struct {
		vendor, baseURLEnv, tokenEnv string
		wantBase, wantTok            string
	}{
		{"anthropic", "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL=http://gw", "ANTHROPIC_AUTH_TOKEN=tok_x"},
		{"openai", "OPENAI_BASE_URL", "OPENAI_API_KEY", "OPENAI_BASE_URL=http://gw", "OPENAI_API_KEY=tok_x"},
	}
	for _, tc := range cases {
		g := &grant{token: "tok_x", baseURL: "http://gw", baseURLEnv: tc.baseURLEnv, tokenEnv: tc.tokenEnv}
		env := g.EnvVars()
		if len(env) != 2 || env[0] != tc.wantBase || env[1] != tc.wantTok {
			t.Errorf("%s: EnvVars() = %v", tc.vendor, env)
		}
	}
}
```

(If the test references the pre-refactor `grant` shape, write it against the POST-refactor shape — it will fail to compile until Step 2, which is the RED.)

- [ ] **Step 2: Generalize `gateway.Provider`/`grant`/`EnvVars`**

In `internal/gateway/provider.go`:
- Add to `Provider`: `BaseURLEnv string` and `TokenEnv string` (after `Vendor`).
- Add to `grant`: `baseURLEnv string` and `tokenEnv string` (drop `vendor` if it's now unused elsewhere — check first; keep it if other code reads it).
- In `Mint`, thread the names: `return &grant{gw: p.GW, token: tok, baseURL: p.BaseURL, baseURLEnv: p.BaseURLEnv, tokenEnv: p.TokenEnv}, nil`.
- Replace `EnvVars` with:

```go
func (g *grant) EnvVars() []string {
	return []string{g.baseURLEnv + "=" + g.baseURL, g.tokenEnv + "=" + g.token}
}
```

- [ ] **Step 3: Run the gateway test**

Run: `go test ./internal/gateway/ -run TestGrantEnvVars`
Expected: PASS (byte-identical to the old switch output).

- [ ] **Step 4: Replace brokerd's backend construction with the registry loop**

In `cmd/brokerd/main.go`, add `"drydock/internal/provider"` to imports.

Replace the credential guard (~142-150) AND the two `switch cfg.*Auth` blocks (~230-256) with a single loop. First delete the `anthropicKey`/`openaiKey`/`haveAnthropic`/`haveOpenAI` block (142-150) and the two switch blocks. Then, where the backends are built:

```go
	var backends []gateway.Backend
	fileKeys := config.LoadAPIKeys(config.APIKeysPath())
	for _, p := range provider.Registry {
		switch cfg.AuthMode(p.Vendor) {
		case "subscription":
			b, err := p.OAuthBackend(config.Dir())
			if err != nil {
				die(p.Vendor+"_auth=subscription but no usable credentials — run `"+p.AuthCmd+"`", "err", err)
			}
			backends = append(backends, b)
		default: // api_key
			if key := resolveAPIKey(p.APIKeyEnv, fileKeys); key != "" {
				backends = append(backends, gateway.Backend{Vendor: p.APIVendor(), Cred: gateway.StaticKey(key)})
			}
		}
	}
	if len(backends) == 0 {
		die("set at least one provider's API key (e.g. ANTHROPIC_API_KEY/OPENAI_API_KEY) or enable a subscription mode")
	}
```

(`fileKeys` was previously declared at 142 — keep a single declaration; `resolveAPIKey` is unchanged.)

- [ ] **Step 5: Registry-drive the provider-map, "agents available" log, and boot guard**

In the provider-map loop (~270-286), replace the subscription detection and add the env names:

```go
	providers := map[string]creds.Provider{}
	for _, b := range backends {
		budget := cfg.TaskBudgetUSD
		if cfg.AuthMode(b.Vendor.Name) == "subscription" {
			budget = math.MaxFloat64
		}
		p, _ := provider.ByVendor(b.Vendor.Name)
		providers[b.Vendor.Name] = &gateway.Provider{
			GW: gw, Vendor: b.Vendor.Name, BaseURL: "http://" + gwAddr,
			BaseURLEnv: p.BaseURLEnv, TokenEnv: p.TokenEnv,
			Budget: budget, TTL: cfg.TaskTimeout + 5*time.Minute, MaxRequests: cfg.TaskMaxRequests,
		}
	}
```

Replace the hardcoded "agents available" `slog.Info` (~287) with a registry-driven one, e.g. log the sorted keys of `providers`:

```go
	avail := make([]string, 0, len(providers))
	for v := range providers {
		avail = append(avail, v)
	}
	sort.Strings(avail)
	slog.Info("agents available", "vendors", avail)
```

(Add `"sort"` to imports if not present.) The boot guard (~290-296) is already `agent.Vendor(cfg.DefaultAgent)` + `providers[v]` — keep it; if its message names a vendor env, derive it via `provider.ByVendor(v).APIKeyEnv`.

- [ ] **Step 6: Build, vet, fmt, test brokerd + gateway**

Run: `gofmt -w internal/gateway/ cmd/brokerd/main.go && go build ./... && go vet ./... && go test ./internal/gateway/ ./cmd/brokerd/ ./internal/broker/`
Expected: build/vet clean (a leftover use of the deleted `anthropicKey`/`switch` would fail the build — that's the consistency guard); tests PASS. Resolve any now-unused gateway imports in main.go (goimports).

- [ ] **Step 7: Commit**

```bash
gofmt -w internal/ cmd/
git add internal/gateway/ cmd/brokerd/main.go
git commit -m "brokerd/gateway: registry-driven backend loop + injected env names"
```

---

### Task 4: `wizard` + `auth` + `doctor` registry-driven

The CLI surface. Behavior-identical for the two-entry registry.

**Files:**
- Modify: `cmd/drydock/wizard.go` (menu + selection loop)
- Modify: `cmd/drydock/auth.go` (dispatch + usage)
- Modify: `cmd/drydock/doctor.go` (availability lines, if they name vendors)
- Test: existing `cmd/drydock` tests pass unchanged; add a wizard-menu test if one doesn't cover it.

**Interfaces:**
- Consumes: `provider` (Task 1).

- [ ] **Step 1: Registry-drive the wizard menu + selection**

In `cmd/drydock/wizard.go`, replace the hardcoded menu (~168-175) so the choices come from `provider.Labels()` plus a trailing "all" option, and the selection maps to the wanted agents. Keep `wantClaude`/`wantCodex` behavior-identical for two entries — but express it generically:

```go
	labels := provider.Labels()
	choices := append(append([]string{}, labels...), "all")
	sel := promptChoice(d.in, d.out, "Which coding agent?", choices, 1)
	wanted := map[string]bool{}
	if sel == len(choices) { // "all"
		for _, p := range provider.Registry {
			wanted[p.Agent] = true
		}
	} else {
		wanted[provider.Registry[sel-1].Agent] = true
	}
	// DefaultAgent: the single selection, or claude when multiple ("all").
	if len(wanted) == 1 {
		for a := range wanted {
			c.DefaultAgent = a
		}
	} else {
		c.DefaultAgent = "claude"
	}
```

Then replace the per-provider `authStep` calls with a loop over `provider.Registry` filtered by `wanted`, calling `authStep` with each entry's `Label`/`APIKeyEnv` and the bootstrap func resolved locally (the bootstrap funcs live in `wizardDeps`; map them by agent):

```go
	bootstrap := map[string]func() error{"claude": d.bootstrapClaude, "codex": d.bootstrapCodex}
	for _, p := range provider.Registry {
		if !wanted[p.Agent] {
			continue
		}
		mode := authStep(d, p.Label, p.APIKeyEnv, bootstrap[p.Agent])
		switch p.Vendor {
		case "anthropic":
			c.AnthropicAuth = mode
		case "openai":
			c.OpenAIAuth = mode
		}
	}
```

(For two entries + "both"→"all" relabel, the produced config is identical to today. Verify any wizard test's expected menu string is updated from "both" to "all" if it asserts the literal — this label change is the one visible wizard tweak; if a test pins "both", update it and note it.)

- [ ] **Step 2: Registry-drive `auth` dispatch + usage**

In `cmd/drydock/auth.go`, build the usage string from `provider.Agents()` (`drydock auth <agents> [--status]`) and validate `args[0]` against the registry. Keep the per-agent implementations (`runAuthClaude`/`runAuthCodex`) dispatched by agent name (a small map or switch is fine — the dispatch enumerates the registry for validation, the impls stay):

```go
	if _, ok := provider.ByAgent(args[0]); !ok {
		fmt.Fprintf(os.Stderr, "drydock auth: unknown subcommand %q (want one of %v)\n", args[0], provider.Agents())
		os.Exit(2)
	}
	switch args[0] {
	case "claude":
		runAuthClaude(args[1:])
	case "codex":
		runAuthCodex(args[1:])
	}
```

- [ ] **Step 3: doctor availability (if it names vendors)**

If `cmd/drydock/doctor.go` has per-vendor availability lines naming anthropic/openai, iterate `provider.Registry` (`p.Vendor`, `p.APIKeyEnv`) instead. If doctor doesn't currently enumerate providers, leave it unchanged (don't invent a new check — out of scope).

- [ ] **Step 4: Build, vet, fmt, full suite**

Run: `gofmt -w cmd/drydock/ && go build ./... && go vet ./... && go test ./...`
Expected: all packages PASS. Update any wizard test asserting the literal "both" menu label to "all".

- [ ] **Step 5: Commit**

```bash
gofmt -w cmd/
git add cmd/drydock/
git commit -m "drydock: registry-driven wizard menu, auth dispatch, doctor"
```

---

## Self-Review

**Spec coverage:**
- New `internal/provider` package (registry + helpers) → Task 1. ✓
- `agent.Vendor` delegation + empty-default pinned to `claude` → Task 2 Step 1. ✓
- `config.validate` via `provider.Agents()` + `AuthMode` accessor → Task 2 Steps 3-4. ✓
- brokerd generic backend loop + the credential-guard generalization (F3) → Task 3 Steps 4-5. ✓
- `grant.EnvVars` env-names-passed-in (the pinned-risk site) → Task 3 Steps 1-3. ✓
- provider-map `ByVendor` for `BaseURLEnv`/`TokenEnv` (F2) → Task 3 Step 5. ✓
- wizard menu + auth + doctor → Task 4. ✓
- Behavior-identical proof: existing agent/config/brokerd/drydock tests pass unchanged (called out per task). ✓

**Placeholder scan:** none — each step has complete code or an exact instruction against named files/lines. The verbatim-lift steps (Task 1 closures, Task 3 brokerd) instruct the implementer to confirm the real constructor names before transcribing.

**Type consistency:** `provider.Provider`/`Registry`/`ByAgent`/`ByVendor`/`Agents`/`Labels` defined in Task 1 are consumed with those exact signatures in Tasks 2-4; `config.AuthMode(vendor)` defined in Task 2 and called in Task 3's loop; `gateway.Provider.BaseURLEnv`/`TokenEnv` set in Task 3 Step 5 match the `grant` fields added in Step 2; `OAuthBackend func(cfgDir string)` defined in Task 1 and called with `config.Dir()` in Task 3 Step 4.
