# OpenAI-compatible provider (bring-your-own-model) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator point drydock at any OpenAI-compatible endpoint (Gemini's `/v1beta/openai`, OpenRouter, local) via config, run `drydock submit --agent opencode`, and get a sandboxed run whose model traffic flows VM → gateway → that endpoint with the real key host-only.

**Architecture:** Reuse the existing credential-gateway lane. A config-driven `openai-compat` gateway `Vendor` (operator base URL + key); the sandbox runs `opencode` (chat/completions) pointed at the gateway. The 3A registry enumerates the provider; brokerd builds its backend from a new config section (it's config-parameterized, so not a static registry row).

**Tech Stack:** Go 1.26.4, the existing `gateway`/`provider`/`config` packages, `httputil.ReverseProxy` (unchanged), `opencode` static binary in the sandbox image.

## Global Constraints

- **Depends on Phase 3A (merged):** builds on `internal/provider.Registry`, `agent.Vendor` delegation, `config.AuthMode`, and `gateway.Provider.BaseURLEnv/TokenEnv`.
- **Standard library + existing packages only**, no new Go deps. Go 1.26.4. Each Go task ends green on `go build ./... && go vet ./... && go test ./...`, gofmt + staticcheck clean (CI runs `staticcheck`).
- **Trust boundary unchanged:** the real openai-compat key stays host-only; the VM sees only the `tok_` lease + the gateway base URL. The VM reaches only the gateway IP for model traffic (no new squid/egress entry — `netfw.go:13`).
- **No price guessing:** budget in USD only when `openai_compat.prices` supplies the model's rates; otherwise fall back to the request-count cap (`task_max_requests`), exactly as subscription mode does.
- **Key never in the config file:** the operator names a host env var (`api_key_env`); brokerd reads the key from it at boot.
- **Spike-gated tasks:** Task 1 (spike) and Task 6 (image/entrypoint) depend on `opencode`'s actual runtime behavior and the macOS `container` service — they run in the operator's environment. Tasks 2–5 + 7 (host-side Go) are fully buildable/testable now.

---

### Task 1: Validation spike — opencode through a gateway-style proxy *(environment-gated)*

A hands-on investigation, not integration code. It gates Task 6 and confirms the env contract. Run it where `opencode` + a real OpenAI-compatible endpoint/key are available (mirrors how the Codex-subscription spike was deferred).

**Deliverable:** a findings note appended to this plan answering, with evidence:
- **S1 — install:** how `opencode` installs reproducibly (pinned binary URL + sha256, or a pinned npm package). Record the exact version.
- **S2 — endpoint config:** does opencode read `OPENAI_BASE_URL` + `OPENAI_API_KEY` from env and speak `/chat/completions`, OR does it need a written config file (like codex's `write-codex-config.sh`)? Capture the exact mechanism + a config sample if needed.
- **S3 — model selection:** how the model id is passed (flag/env/config).
- **S4 — headless run:** the exact non-interactive invocation that takes a prompt + a repo working dir and runs to completion without a TUI/approval prompt (the opencode equivalent of `codex exec --dangerously-bypass-approvals-and-sandbox "$PROMPT"`).
- **S5 — proof:** run it against a local proxy that forwards `/v1/chat/completions` to a real OpenAI-compat endpoint, and confirm a trivial repo task completes and the proxy saw chat/completions traffic with the bearer token.

- [ ] **Step 1: Install opencode and capture the version + install method (S1).**
- [ ] **Step 2: Stand up a trivial local proxy** (any tool) that logs requests and forwards `/v1/chat/completions` to a real OpenAI-compat endpoint (e.g. Gemini's `/v1beta/openai/`) with your real key.
- [ ] **Step 3: Point opencode at the proxy** (`OPENAI_BASE_URL=http://localhost:PORT` + key, or a config file) and run a one-line non-interactive task on a throwaway repo (S2–S4).
- [ ] **Step 4: Confirm** the task completed and the proxy logged a `/chat/completions` POST with `Authorization: Bearer <key>` (S5).
- [ ] **Step 5: Write the findings (S1–S5) into a "## Task 1 spike findings" section at the bottom of this plan.** If opencode can't be driven this way, STOP and report — reassess the CLI (aider) before Task 6.

---

### Task 2: Gateway `OpenAICompatVendor`

A config-parameterized OpenAI vendor: same bearer inject + usage parser as OpenAI, with operator-set `BaseURL`/`BasePath`/`Prices`.

**Files:**
- Modify: `internal/gateway/vendor.go` (add `OpenAICompatVendor`)
- Test: `internal/gateway/gateway_test.go` (or a new `vendor_test.go`)

**Interfaces:**
- Consumes: existing `OpenAIVendor()`, `parseOpenAIUsage`, the `Vendor` struct, `Price`.
- Produces: `gateway.OpenAICompatVendor(name, baseURL, basePath string, prices map[string]Price) Vendor`.

- [ ] **Step 1: Write the failing test**

In `internal/gateway/gateway_test.go`:

```go
func TestOpenAICompatVendor(t *testing.T) {
	v := OpenAICompatVendor("openai-compat", "https://example.test", "/v1beta/openai", nil)
	if v.Name != "openai-compat" || v.BaseURL != "https://example.test" || v.BasePath != "/v1beta/openai" {
		t.Fatalf("vendor fields = %+v", v)
	}
	// Inject must set bearer + clear X-Api-Key (identical to OpenAI).
	r, _ := http.NewRequest("POST", "http://gw/v1/chat/completions", nil)
	r.Header.Set("X-Api-Key", "should-be-removed")
	v.Inject(r, "real-key")
	if r.Header.Get("Authorization") != "Bearer real-key" || r.Header.Get("X-Api-Key") != "" {
		t.Errorf("inject headers = %v", r.Header)
	}
	// Usage parser is the OpenAI one (non-nil).
	if v.ParseUsage == nil {
		t.Error("ParseUsage must be set (parseOpenAIUsage)")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/gateway/ -run TestOpenAICompatVendor`
Expected: FAIL — `undefined: OpenAICompatVendor`.

- [ ] **Step 3: Implement**

Add to `internal/gateway/vendor.go`:

```go
// OpenAICompatVendor is a config-driven OpenAI-compatible upstream (Gemini's
// /v1beta/openai endpoint, OpenRouter, a local server, …). It reuses OpenAI's
// bearer auth and usage parsing; the operator supplies the base URL, an
// optional base path joined onto the inbound path, and optional prices for USD
// metering. Constructed by brokerd from config (not a static registry row).
func OpenAICompatVendor(name, baseURL, basePath string, prices map[string]Price) Vendor {
	v := OpenAIVendor()
	v.Name = name
	v.BaseURL = baseURL
	v.BasePath = basePath
	v.Prices = prices
	return v
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/gateway/ -run TestOpenAICompatVendor`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/gateway/ && git add internal/gateway/
git commit -m "gateway: OpenAICompatVendor (config-driven OpenAI-compatible upstream)"
```

---

### Task 3: Config `openai_compat` section + validation

**Files:**
- Modify: `internal/config/config.go` (struct field + `validate` + seed template)
- Modify: `config/config.yaml` (on-disk template — must match `SeedTemplate`)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.Config.OpenAICompat` with `BaseURL, BasePath, APIKeyEnv, Model string` and `Prices map[string]struct{Input, Output float64}`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`:

```go
func TestValidate_OpenAICompat(t *testing.T) {
	base := Defaults()
	// Unconfigured (empty base_url) is valid — provider just inactive.
	if err := base.validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
	// base_url set but no api_key_env / model -> error.
	c := Defaults()
	c.OpenAICompat.BaseURL = "https://example.test"
	if err := c.validate(); err == nil {
		t.Error("base_url without api_key_env+model must error")
	}
	// non-https base_url (non-localhost) -> error.
	c = Defaults()
	c.OpenAICompat.BaseURL = "http://example.test"
	c.OpenAICompat.APIKeyEnv = "X_KEY"
	c.OpenAICompat.Model = "m"
	if err := c.validate(); err == nil {
		t.Error("non-https non-localhost base_url must error")
	}
	// fully configured https -> ok.
	c.OpenAICompat.BaseURL = "https://example.test"
	if err := c.validate(); err != nil {
		t.Errorf("configured openai_compat must validate: %v", err)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/config/ -run TestValidate_OpenAICompat`
Expected: FAIL (field/validation absent).

- [ ] **Step 3: Add the struct field**

In `internal/config/config.go`, after `TaskMaxRequests`:

```go
	// OpenAICompat configures a bring-your-own OpenAI-compatible upstream
	// (Gemini's /v1beta/openai, OpenRouter, local). Empty BaseURL = disabled.
	// The real key is read from the host env var named by APIKeyEnv — never
	// stored here. Prices (USD per 1M tokens) enable USD metering; omit to fall
	// back to the task_max_requests cap.
	OpenAICompat struct {
		BaseURL   string `yaml:"base_url"`
		BasePath  string `yaml:"base_path"`
		APIKeyEnv string `yaml:"api_key_env"`
		Model     string `yaml:"model"`
		Prices    map[string]struct {
			Input  float64 `yaml:"input"`
			Output float64 `yaml:"output"`
		} `yaml:"prices"`
	} `yaml:"openai_compat"`
```

- [ ] **Step 4: Add validation**

In `validate()` (after the `openai_auth` check):

```go
	if oc := c.OpenAICompat; oc.BaseURL != "" {
		if oc.APIKeyEnv == "" || oc.Model == "" {
			return fmt.Errorf("config: openai_compat.base_url set but api_key_env and model are required")
		}
		u, err := url.Parse(oc.BaseURL)
		if err != nil || u.Host == "" {
			return fmt.Errorf("config: openai_compat.base_url must be an absolute URL, got %q", oc.BaseURL)
		}
		isLocal := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"
		if u.Scheme != "https" && !(u.Scheme == "http" && isLocal) {
			return fmt.Errorf("config: openai_compat.base_url must be https (http allowed only for localhost), got %q", oc.BaseURL)
		}
	}
```

Add `"net/url"` to the config imports.

- [ ] **Step 5: Seed template parity**

Append the `openai_compat` block to BOTH `SeedTemplate` (in `config.go`) and the on-disk `config/config.yaml` (they must stay byte-identical — `TestSeedTemplate_MatchesOnDiskTemplate` enforces it). Add after the `task_max_requests` line:

```yaml

# --- Bring-your-own OpenAI-compatible model (optional; e.g. Gemini, OpenRouter, local) ---
openai_compat:
  base_url:    ""        # e.g. https://generativelanguage.googleapis.com  (empty = disabled)
  base_path:   ""        # e.g. /v1beta/openai
  api_key_env: ""        # name of the host env var holding the real key (never the key itself)
  model:       ""        # model id passed to the agent, e.g. gemini-2.5-pro
```

(Leave `prices` out of the seed — it's an optional advanced map.)

- [ ] **Step 6: Run tests (new + the seed-parity test)**

Run: `go test ./internal/config/`
Expected: PASS — `TestValidate_OpenAICompat` and `TestSeedTemplate_MatchesOnDiskTemplate`.

- [ ] **Step 7: Commit**

```bash
gofmt -w internal/config/ && git add internal/config/ config/config.yaml
git commit -m "config: openai_compat section (BYO OpenAI-compatible endpoint)"
```

---

### Task 4: Provider registry row + agent mapping

**Files:**
- Modify: `internal/provider/provider.go` (add `ConfigBuilt` field + the `opencode` row)
- Test: `internal/provider/provider_test.go`

**Interfaces:**
- Consumes: the 3A `Provider` struct.
- Produces: a `Provider{Agent:"opencode", Vendor:"openai-compat", ConfigBuilt:true, …}` entry; `provider.Provider.ConfigBuilt bool`.

- [ ] **Step 1: Write the failing test**

Add to `internal/provider/provider_test.go`:

```go
func TestRegistry_OpenAICompatRow(t *testing.T) {
	p, ok := ByAgent("opencode")
	if !ok || p.Vendor != "openai-compat" {
		t.Fatalf("ByAgent(opencode) = %+v,%v", p, ok)
	}
	if !p.ConfigBuilt {
		t.Error("opencode row must be ConfigBuilt (brokerd builds it from config)")
	}
	if p.APIVendor != nil || p.OAuthBackend != nil {
		t.Error("config-built provider must have nil APIVendor/OAuthBackend")
	}
	if p.BaseURLEnv != "OPENAI_BASE_URL" || p.TokenEnv != "OPENAI_API_KEY" {
		t.Errorf("opencode env names = %q/%q", p.BaseURLEnv, p.TokenEnv)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/provider/ -run TestRegistry_OpenAICompatRow`
Expected: FAIL (`ConfigBuilt` field / row absent).

- [ ] **Step 3: Add `ConfigBuilt` + the row**

In `internal/provider/provider.go`, add to the `Provider` struct:

```go
	// ConfigBuilt marks a provider whose backend brokerd builds from config
	// (operator-parameterized endpoint), rather than the static APIVendor /
	// OAuthBackend hooks. Such a provider has nil APIVendor and OAuthBackend.
	ConfigBuilt bool
```

Append to `Registry` (after codex):

```go
	{
		Agent: "opencode", Vendor: "openai-compat", Label: "OpenAI-compatible (bring your own)",
		APIKeyEnv: "", AuthCmd: "",
		BaseURLEnv: "OPENAI_BASE_URL", TokenEnv: "OPENAI_API_KEY",
		ConfigBuilt: true,
		// APIVendor / OAuthBackend intentionally nil — brokerd builds from config.
	},
```

- [ ] **Step 4: Update the entry-completeness test**

The 3A `TestRegistry_EntriesComplete` asserts `APIKeyEnv != "" && AuthCmd != "" && APIVendor != nil` for every row — which the `opencode` row deliberately violates. Relax it to exempt `ConfigBuilt` rows:

```go
	for _, p := range Registry {
		if p.Agent == "" || p.Vendor == "" || p.Label == "" || p.BaseURLEnv == "" || p.TokenEnv == "" {
			t.Errorf("incomplete registry entry: %+v", p)
		}
		if !p.ConfigBuilt && (p.APIKeyEnv == "" || p.AuthCmd == "" || p.APIVendor == nil) {
			t.Errorf("static provider missing APIKeyEnv/AuthCmd/APIVendor: %+v", p)
		}
	}
```

- [ ] **Step 5: Run provider + agent tests**

Run: `go test ./internal/provider/ ./internal/agent/`
Expected: PASS — including `TestVendor` (now also resolving `opencode`→`openai-compat` via the registry; if `TestVendor` is a closed map it won't test opencode, which is fine — `TestRegistry_OpenAICompatRow` covers it).

- [ ] **Step 6: Commit**

```bash
gofmt -w internal/provider/ && git add internal/provider/
git commit -m "provider: opencode → openai-compat registry row (ConfigBuilt)"
```

---

### Task 5: brokerd builds the openai-compat backend from config

**Files:**
- Modify: `cmd/brokerd/main.go` (the registry backend loop + provider-map)
- Test: covered by `cmd/brokerd` build/vet + the existing brokerd tests (behavior unchanged for the other providers); add a focused unit test if a testable seam exists.

**Interfaces:**
- Consumes: Task 2 `OpenAICompatVendor`, Task 3 `cfg.OpenAICompat`, Task 4 `ConfigBuilt`.

- [ ] **Step 1: Add the `ConfigBuilt` branch to the backend loop**

In `cmd/brokerd/main.go`'s `for _, p := range provider.Registry` loop, before the `switch cfg.AuthMode(p.Vendor)`:

```go
		if p.ConfigBuilt {
			oc := cfg.OpenAICompat
			if oc.BaseURL == "" {
				continue // provider not configured; skip
			}
			key := resolveAPIKey(oc.APIKeyEnv, fileKeys)
			if key == "" {
				die("openai_compat.base_url is set but its api_key_env ("+oc.APIKeyEnv+") is empty")
			}
			prices := map[string]gateway.Price{}
			for m, pr := range oc.Prices {
				prices[m] = gateway.Price{Input: pr.Input, Output: pr.Output}
			}
			backends = append(backends, gateway.Backend{
				Vendor: gateway.OpenAICompatVendor(p.Vendor, oc.BaseURL, oc.BasePath, prices),
				Cred:   gateway.StaticKey(key),
			})
			continue
		}
```

(Confirm the `gateway.Price` field names — match the real `Price` struct; if they differ, use the real names.)

- [ ] **Step 2: Budget — request-cap fallback when no prices**

In the provider-map loop, when building the `gateway.Provider` for the `openai-compat` vendor, set `budget = math.MaxFloat64` if `len(cfg.OpenAICompat.Prices) == 0` (USD metering is meaningless without rates — rely on `TaskMaxRequests`), mirroring the subscription branch. The `BaseURLEnv`/`TokenEnv` already come from `provider.ByVendor` (3A).

- [ ] **Step 3: Build, vet, test**

Run: `gofmt -w cmd/brokerd/ && go build ./... && go vet ./... && go test ./cmd/brokerd/ ./internal/broker/`
Expected: build/vet clean; tests PASS (other providers unchanged; the new branch is inert when `openai_compat.base_url` is empty, which the default config is).

- [ ] **Step 4: Commit**

```bash
gofmt -w cmd/ && git add cmd/brokerd/main.go
git commit -m "brokerd: build the openai-compat backend from config"
```

---

### Task 6: Sandbox image — opencode install + entrypoint *(spike-gated; build deferred)*

Written against Task 1's findings. The gateway injects the standard `OPENAI_BASE_URL`/`OPENAI_API_KEY`/`DRYDOCK_AGENT=opencode`/`DRYDOCK_MODEL`; this task makes the image consume them.

**Files:**
- Modify: `image/Dockerfile` (pin + install opencode)
- Modify: `image/entrypoint.sh` (an `opencode)` case)
- Possibly create: `image/write-opencode-config.sh` (only if Task 1/S2 shows opencode needs a config file like codex)

- [ ] **Step 1: Pin + install opencode** in `image/Dockerfile` using the install method + version from S1 (an `ARG OPENCODE_VERSION=...` + a checksum-verified binary fetch, or a pinned `npm install -g`), alongside the claude-code/codex installs.
- [ ] **Step 2: Add the `opencode)` case to `image/entrypoint.sh`** using S2–S4: export/bridge the gateway env (or write a config file via `write-opencode-config.sh` if S2 requires it), select the model from `DRYDOCK_MODEL` (or `cfg.OpenAICompat.Model`, injected as `DRYDOCK_MODEL`), and `exec gosu agent …` the exact non-interactive opencode invocation from S4. Mirror the codex case's structure.
- [ ] **Step 3: Build the image** (`make image-sandbox`) and run a real `drydock submit --agent opencode` against a configured endpoint — **environment-gated** (needs the macOS container service + a real endpoint/key). Confirm a diff is produced and pushed through the normal gate.
- [ ] **Step 4: Commit** the Dockerfile + entrypoint changes (+ any config script).

---

### Task 7: Red-team A1 for the openai-compat lane + CLI surface

**Files:**
- Modify: `cmd/drydock/redteam.go` (A1 for the new lane) — host logic now; live run env-gated.
- Modify: `cmd/drydock/wizard.go`, `cmd/drydock/doctor.go` (surface the provider).

- [ ] **Step 1: Red-team A1 for openai-compat.** Mirror `redteamA1`: build a gateway with an `OpenAICompatVendor` fronting a sentinel key, mint a grant, and assert the VM env carries only the `tok_` lease + the gateway base URL — never the sentinel. Source `BaseURLEnv`/`TokenEnv` from `provider.ByVendor("openai-compat")` (so it can't drift, per the 3A final-review lesson). The container-run portion is environment-gated; the host-side grant/env assertion runs in CI.
- [ ] **Step 2: Wizard.** Add the openai-compat entry to the registry-driven menu; when chosen, prompt for `base_url`, `base_path` (optional), `model`, and the `api_key_env` name, and write the `openai_compat:` block via `setYAMLKey` (extend it to nested keys, or write the block directly). No OAuth, so no `auth` subcommand.
- [ ] **Step 3: Doctor.** Add a non-fatal line: if `cfg.OpenAICompat.BaseURL != ""`, report configured + whether `os.Getenv(cfg.OpenAICompat.APIKeyEnv) != ""` (no network call).
- [ ] **Step 4: Build, vet, full suite, commit.**

Run: `gofmt -w cmd/ internal/ && go build ./... && go vet ./... && go test ./...`

```bash
git add cmd/drydock/ && git commit -m "drydock: openai-compat red-team A1 + wizard/doctor surface"
```

---

## Self-Review

**Spec coverage:**
- Spike (spec §"Phasing") → Task 1. ✓
- Gateway openai-compat vendor (§Components 2) → Task 2. ✓
- Config section + validation (§3) → Task 3. ✓
- Registry row, ConfigBuilt, agent map (§4) → Task 4. ✓
- brokerd config-built backend + request-cap budget (§5, §6) → Task 5. ✓
- Image + opencode entrypoint (§1) → Task 6 (spike-gated). ✓
- Red-team A1 + wizard/doctor (§7, §Testing) → Task 7. ✓
- No squid change (§Architecture) → honored (no egress task; noted in Global Constraints). ✓

**Placeholder scan:** the Go tasks (2–5, 7-host) carry complete code. Tasks 1 and 6 are deliberately findings-driven (opencode's runtime is unverifiable from here) — they specify exactly what to determine/produce, not fabricated code, and are explicitly environment-gated. The two "confirm the real struct/field names" notes (gateway `Price`, the codex-style entrypoint shape) point the implementer at the authoritative source rather than guessing.

**Type consistency:** `OpenAICompatVendor(name, baseURL, basePath string, prices map[string]Price)` (Task 2) is called with those args in Task 5; `cfg.OpenAICompat.{BaseURL,BasePath,APIKeyEnv,Model,Prices}` (Task 3) consumed in Task 5; `provider.Provider.ConfigBuilt` (Task 4) branched on in Task 5; `BaseURLEnv/TokenEnv` = `OPENAI_BASE_URL`/`OPENAI_API_KEY` consistent across Tasks 4, 6, 7.

## Execution note

Tasks 2–5 and Task 7's host-side are buildable + unit-testable now. Task 1 (spike) and Task 6 (image build + live run), plus the container-run portions of Task 7, require the operator's environment (the `opencode` binary, a real OpenAI-compatible endpoint + key, and the macOS `container` service) — deferred exactly as the Codex-subscription spike + integration were.
