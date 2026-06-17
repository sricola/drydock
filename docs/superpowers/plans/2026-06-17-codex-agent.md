# Codex Agent Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a drydock task run OpenAI Codex instead of Claude Code, with identical security properties (real key host-only, deny-by-default egress, per-task USD budget + revoke).

**Architecture:** One gateway gains a vendor registry. An *agent* (`claude`/`codex`) is the CLI in the VM; a *vendor* (`anthropic`/`openai`) is the API it calls. Each minted lease carries its vendor; `director`/`meter` dispatch through it. One image hosts both CLIs and `entrypoint.sh` branches on `DRYDOCK_AGENT`.

**Tech Stack:** Go 1.26, Apple `container`, httputil.ReverseProxy, squid, nftables, node:22 image.

## Global Constraints

- Build green + all tests pass after **every** task (each task updates its callers so the tree compiles).
- `brokerd` must require **at least one** of `ANTHROPIC_API_KEY` / `OPENAI_API_KEY`; a vendor with no key is unavailable and tasks requesting it are rejected with a clear 4xx.
- The real key NEVER enters the VM (gateway-brokered tokens only). Do not regress this.
- No maturity overclaims in any doc/copy: no implied third-party audit, no "race-clean", no "production"/"shipping". Calibrate to "working alpha".
- Image renamed `claude-sandbox` → `drydock-sandbox`. Do NOT edit historical files under `docs/superpowers/plans/2026-06-14-*`, `2026-06-15-*`, or `specs/2026-06-14-*` — they are frozen records.
- Codex non-interactive invocation runs with internal sandbox/approvals disabled — the VM is the isolation boundary.

---

### Task 1: Gateway vendor abstraction (refactor Anthropic into a vendor)

Pure refactor — no behavior change. Introduces the `Vendor` type, makes leases vendor-tagged, and routes `director`/`meter` through the lease's vendor. Updates `brokerd` + all gateway tests to the new single-(anthropic)-vendor API so the tree stays green.

**Files:**
- Create: `internal/gateway/vendor.go`
- Modify: `internal/gateway/gateway.go` (New signature, `Lease.Vendor`, `Mint`, `director`, `meter`)
- Modify: `internal/gateway/provider.go` (`Vendor` field, `grant.vendor`, `EnvVars` switch, `Mint`)
- Modify: `internal/gateway/usage.go` (rename `parseUsage` → `parseAnthropicUsage`)
- Modify: `internal/gateway/pricing.go` (rename `DefaultPrices` → `AnthropicPrices`)
- Modify: `cmd/brokerd/main.go:89-92,131,144-149` (new `gateway.New` + `Provider.Vendor`)
- Test: `internal/gateway/gateway_test.go`, `usage_test.go`, `pricing_test.go`, `provider` (new `provider_test.go`)

**Interfaces:**
- Produces:
  - `type Vendor struct { Name, BaseURL string; Inject func(*http.Request, realKey string); ParseUsage func(body []byte, contentType string) (model string, in, out int, ok bool); Prices map[string]Price }`
  - `func AnthropicVendor() Vendor`
  - `type Backend struct { Vendor Vendor; RealKey string }`
  - `func New(backends ...Backend) (*Gateway, error)`
  - `func (g *Gateway) Mint(vendor string, budgetUSD float64, ttl time.Duration) (string, error)`
  - `Lease.Vendor string`
  - `Provider.Vendor string`; `grant.vendor string`

- [ ] **Step 1: Write `vendor.go` with the type + AnthropicVendor**

```go
// Package-level: an upstream API description.
package gateway

import "net/http"

// Vendor describes one upstream API: where it lives, how to authenticate to it
// with the real key, and how to read token usage out of its responses.
type Vendor struct {
	Name       string
	BaseURL    string
	Inject     func(r *http.Request, realKey string)
	ParseUsage func(body []byte, contentType string) (model string, in, out int, ok bool)
	Prices     map[string]Price
}

// Backend pairs a Vendor with the real upstream key (host-only).
type Backend struct {
	Vendor  Vendor
	RealKey string
}

// AnthropicVendor is the api.anthropic.com upstream: X-Api-Key auth +
// anthropic-version header, Claude usage shapes, Claude prices.
func AnthropicVendor() Vendor {
	return Vendor{
		Name:    "anthropic",
		BaseURL: "https://api.anthropic.com",
		Inject: func(r *http.Request, realKey string) {
			r.Header.Del("Authorization")
			r.Header.Set("X-Api-Key", realKey)
			if r.Header.Get("anthropic-version") == "" {
				r.Header.Set("anthropic-version", "2023-06-01")
			}
		},
		ParseUsage: parseAnthropicUsage,
		Prices:     AnthropicPrices(),
	}
}
```

- [ ] **Step 2: Rewrite `gateway.go` core to be vendor-keyed**

Replace the `Gateway` struct, `New`, `Mint`, `Lease`, `director`, `meter`:

```go
type Lease struct {
	Token     string
	Vendor    string
	BudgetUSD float64
	SpentUSD  float64
	Expiry    time.Time
}

type vendorRT struct {
	v        Vendor
	realKey  string
	upstream *url.URL
}

type Gateway struct {
	mu      sync.Mutex
	leases  map[string]*Lease
	vendors map[string]vendorRT
	proxy   *httputil.ReverseProxy
}

func New(backends ...Backend) (*Gateway, error) {
	g := &Gateway{leases: map[string]*Lease{}, vendors: map[string]vendorRT{}}
	for _, b := range backends {
		u, err := url.Parse(b.Vendor.BaseURL)
		if err != nil {
			return nil, err
		}
		g.vendors[b.Vendor.Name] = vendorRT{v: b.Vendor, realKey: b.RealKey, upstream: u}
	}
	g.proxy = &httputil.ReverseProxy{Director: g.director, ModifyResponse: g.meter}
	return g, nil
}

func (g *Gateway) Mint(vendor string, budgetUSD float64, ttl time.Duration) (string, error) {
	if _, ok := g.vendors[vendor]; !ok {
		return "", fmt.Errorf("gateway: no backend for vendor %q", vendor)
	}
	b := make([]byte, 18)
	rand.Read(b)
	tok := "tok_" + hex.EncodeToString(b)
	g.mu.Lock()
	g.leases[tok] = &Lease{Token: tok, Vendor: vendor, BudgetUSD: budgetUSD, Expiry: time.Now().Add(ttl)}
	g.mu.Unlock()
	return tok, nil
}

func (g *Gateway) director(req *http.Request) {
	lease, _ := req.Context().Value(ctxKey{}).(*Lease)
	if lease == nil {
		return
	}
	vt, ok := g.vendors[lease.Vendor]
	if !ok {
		return
	}
	req.URL.Scheme = vt.upstream.Scheme
	req.URL.Host = vt.upstream.Host
	req.Host = vt.upstream.Host
	vt.v.Inject(req, vt.realKey)
}

func (g *Gateway) meter(resp *http.Response) error {
	lease, _ := resp.Request.Context().Value(ctxKey{}).(*Lease)
	if lease == nil {
		return nil
	}
	vt, ok := g.vendors[lease.Vendor]
	if !ok {
		return nil
	}
	ct := resp.Header.Get("Content-Type")
	resp.Body = &usageReader{rc: resp.Body, onDone: func(body []byte) {
		if model, in, out, ok := vt.v.ParseUsage(body, ct); ok {
			g.mu.Lock()
			lease.SpentUSD += cost(vt.v.Prices, model, in, out)
			g.mu.Unlock()
		}
	}}
	return nil
}
```

Add `"fmt"` to imports. `Revoke`, `spent`, `check`, `ServeHTTP`, `usageReader`, `contextWith`, `ctxKey` are unchanged.

- [ ] **Step 3: Rename `parseUsage`→`parseAnthropicUsage` (usage.go) and `DefaultPrices`→`AnthropicPrices` (pricing.go)**

In `usage.go` change `func parseUsage(` to `func parseAnthropicUsage(`. In `pricing.go` change `func DefaultPrices()` to `func AnthropicPrices()`.

- [ ] **Step 4: Make `provider.go` vendor-aware**

```go
type Provider struct {
	GW      *Gateway
	Vendor  string
	BaseURL string
	Budget  float64
	TTL     time.Duration
}

type grant struct {
	gw      *Gateway
	token   string
	baseURL string
	vendor  string
}

func (p *Provider) Mint(budgetUSD float64) (creds.Grant, error) {
	b := budgetUSD
	if b == 0 {
		b = p.Budget
	}
	ttl := p.TTL
	if ttl == 0 {
		ttl = time.Hour
	}
	tok, err := p.GW.Mint(p.Vendor, b, ttl)
	if err != nil {
		return nil, err
	}
	return &grant{gw: p.GW, token: tok, baseURL: p.BaseURL, vendor: p.Vendor}, nil
}

func (g *grant) EnvVars() []string {
	switch g.vendor {
	case "openai":
		return []string{
			"OPENAI_BASE_URL=" + g.baseURL,
			"OPENAI_API_KEY=" + g.token,
		}
	default: // anthropic
		return []string{
			"ANTHROPIC_BASE_URL=" + g.baseURL,
			"ANTHROPIC_AUTH_TOKEN=" + g.token,
		}
	}
}
```
`Revoke` unchanged.

- [ ] **Step 5: Update `cmd/brokerd/main.go` to the new API (anthropic only for now)**

Replace line 131:
```go
	gw, err := gateway.New(gateway.Backend{Vendor: gateway.AnthropicVendor(), RealKey: apiKey})
```
Replace the provider literal (144-149):
```go
	var provider creds.Provider = &gateway.Provider{
		GW:      gw,
		Vendor:  "anthropic",
		BaseURL: "http://" + gwAddr,
		Budget:  cfg.TaskBudgetUSD,
		TTL:     cfg.TaskTimeout + 5*time.Minute,
	}
```

- [ ] **Step 6: Update gateway tests to new API**

In `gateway_test.go`, change `New(...)` calls to `New(Backend{Vendor: AnthropicVendor(), RealKey: "real-key"})` (or a test vendor pointed at the test upstream — keep the existing upstream override by constructing a `Vendor{Name:"anthropic", BaseURL: ts.URL, Inject: AnthropicVendor().Inject, ParseUsage: parseAnthropicUsage, Prices: AnthropicPrices()}`). Change `g.Mint(budget, ttl)` to `tok, _ := g.Mint("anthropic", budget, ttl)`. In `pricing_test.go` change `DefaultPrices()` → `AnthropicPrices()`. `usage_test.go` calls `parseAnthropicUsage` now.

- [ ] **Step 7: Run + commit**

Run: `go build ./... && go test ./internal/gateway/... ./cmd/brokerd/...`
Expected: PASS.
```bash
git add internal/gateway cmd/brokerd/main.go
git commit -m "gateway: introduce vendor registry; refactor Anthropic into a Vendor"
```

---

### Task 2: OpenAI vendor (auth inject + usage parser + prices)

Pure additions; build stays green.

**Files:**
- Modify: `internal/gateway/vendor.go` (add `OpenAIVendor`)
- Modify: `internal/gateway/usage.go` (add `parseOpenAIUsage` + helpers)
- Modify: `internal/gateway/pricing.go` (add `OpenAIPrices`)
- Test: `internal/gateway/openai_test.go` (new)

**Interfaces:**
- Produces: `func OpenAIVendor() Vendor`, `func OpenAIPrices() map[string]Price`, `func parseOpenAIUsage(body []byte, contentType string) (model string, in, out int, ok bool)`

- [ ] **Step 1: Write the failing usage-parser test (`openai_test.go`)**

```go
package gateway

import "testing"

func TestParseOpenAIUsage_NonStreaming(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","usage":{"input_tokens":120,"output_tokens":40}}`)
	m, in, out, ok := parseOpenAIUsage(body, "application/json")
	if !ok || m != "gpt-5-codex" || in != 120 || out != 40 {
		t.Fatalf("got (%q,%d,%d,%v)", m, in, out, ok)
	}
}

func TestParseOpenAIUsage_ChatCompletionsNaming(t *testing.T) {
	body := []byte(`{"model":"gpt-5","usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	m, in, out, ok := parseOpenAIUsage(body, "application/json")
	if !ok || m != "gpt-5" || in != 7 || out != 3 {
		t.Fatalf("got (%q,%d,%d,%v)", m, in, out, ok)
	}
}

func TestParseOpenAIUsage_StreamingResponsesEvent(t *testing.T) {
	body := []byte("event: response.completed\n" +
		`data: {"type":"response.completed","response":{"model":"gpt-5-codex","usage":{"input_tokens":500,"output_tokens":222}}}` + "\n\n" +
		"data: [DONE]\n")
	m, in, out, ok := parseOpenAIUsage(body, "text/event-stream; charset=utf-8")
	if !ok || m != "gpt-5-codex" || in != 500 || out != 222 {
		t.Fatalf("got (%q,%d,%d,%v)", m, in, out, ok)
	}
}

func TestOpenAIVendor_InjectsBearer(t *testing.T) {
	r, _ := http.NewRequest("POST", "http://gw/v1/responses", nil)
	r.Header.Set("Authorization", "Bearer tok_fake")
	OpenAIVendor().Inject(r, "sk-real")
	if got := r.Header.Get("Authorization"); got != "Bearer sk-real" {
		t.Fatalf("Authorization = %q", got)
	}
	if r.Header.Get("X-Api-Key") != "" {
		t.Fatalf("X-Api-Key should be unset for openai")
	}
}
```
Add `"net/http"` to the test imports.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/gateway/ -run OpenAI -v`
Expected: FAIL (undefined: parseOpenAIUsage / OpenAIVendor).

- [ ] **Step 3: Implement `OpenAIVendor` in vendor.go**

```go
// OpenAIVendor is the api.openai.com upstream: bearer auth, OpenAI usage
// shapes (Responses + Chat Completions), OpenAI prices.
func OpenAIVendor() Vendor {
	return Vendor{
		Name:    "openai",
		BaseURL: "https://api.openai.com",
		Inject: func(r *http.Request, realKey string) {
			r.Header.Del("X-Api-Key")
			r.Header.Set("Authorization", "Bearer "+realKey)
		},
		ParseUsage: parseOpenAIUsage,
		Prices:     OpenAIPrices(),
	}
}
```

- [ ] **Step 4: Implement `parseOpenAIUsage` in usage.go**

```go
type openaiUsage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// parseOpenAIUsage extracts (model, input, output) from an OpenAI response.
// Handles non-streaming JSON and SSE streams, and both Responses naming
// (input_tokens/output_tokens) and Chat Completions naming
// (prompt_tokens/completion_tokens). For streams it keeps the last usage seen.
func parseOpenAIUsage(body []byte, contentType string) (model string, in, out int, ok bool) {
	if strings.Contains(contentType, "text/event-stream") {
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}
			if m, i, o, k := openaiUsageFromJSON([]byte(data)); k {
				model, in, out, ok = m, i, o, k
			}
		}
		return
	}
	return openaiUsageFromJSON(body)
}

func openaiUsageFromJSON(b []byte) (model string, in, out int, ok bool) {
	var m struct {
		Model    string       `json:"model"`
		Usage    *openaiUsage `json:"usage"`
		Response *struct {
			Model string       `json:"model"`
			Usage *openaiUsage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(b, &m) != nil {
		return "", 0, 0, false
	}
	u, model := m.Usage, m.Model
	if u == nil && m.Response != nil {
		u, model = m.Response.Usage, m.Response.Model
	}
	if u == nil {
		return "", 0, 0, false
	}
	in, out = u.InputTokens, u.OutputTokens
	if in == 0 {
		in = u.PromptTokens
	}
	if out == 0 {
		out = u.CompletionTokens
	}
	if in == 0 && out == 0 {
		return "", 0, 0, false
	}
	return model, in, out, true
}
```
`usage.go` already imports `encoding/json` and `strings`.

- [ ] **Step 5: Implement `OpenAIPrices` in pricing.go**

```go
// OpenAIPrices seeds the per-task budget gate for Codex tasks. USD per 1M
// tokens, approximate (OpenAI publishes live rates). "default" is the family
// high end so a new model can't overrun the budget before this table catches
// up. Tune for your workload — the gate is a safety cap, not billing truth.
func OpenAIPrices() map[string]Price {
	return map[string]Price{
		"gpt-5":        {InputPer1M: 1.25, OutputPer1M: 10},
		"gpt-5-codex":  {InputPer1M: 1.25, OutputPer1M: 10},
		"gpt-5-mini":   {InputPer1M: 0.25, OutputPer1M: 2},
		"o4-mini":      {InputPer1M: 1.1, OutputPer1M: 4.4},
		"default":      {InputPer1M: 1.25, OutputPer1M: 10},
	}
}
```

- [ ] **Step 6: Run + commit**

Run: `go test ./internal/gateway/ -v`
Expected: PASS.
```bash
git add internal/gateway
git commit -m "gateway: add OpenAI vendor (bearer auth, usage parser, prices)"
```

---

### Task 3: brokerd loads both keys + builds a provider per vendor

**Files:**
- Modify: `cmd/brokerd/main.go:89-92` (dual-key load), `131` (backends), `144-149` (providers map)

**Interfaces:**
- Consumes: `gateway.New(...Backend)`, `gateway.AnthropicVendor()`, `gateway.OpenAIVendor()`, `gateway.Provider{Vendor:...}`
- Produces: a local `providers map[string]creds.Provider` (vendor → provider) passed to the broker in Task 4.

- [ ] **Step 1: Replace the single-key load (89-92)**

```go
	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	openaiKey := os.Getenv("OPENAI_API_KEY")
	if anthropicKey == "" && openaiKey == "" {
		die("set at least one of ANTHROPIC_API_KEY or OPENAI_API_KEY on the broker host")
	}
```

- [ ] **Step 2: Build backends + gateway (replace line 131 block)**

```go
	var backends []gateway.Backend
	if anthropicKey != "" {
		backends = append(backends, gateway.Backend{Vendor: gateway.AnthropicVendor(), RealKey: anthropicKey})
	}
	if openaiKey != "" {
		backends = append(backends, gateway.Backend{Vendor: gateway.OpenAIVendor(), RealKey: openaiKey})
	}
	gw, err := gateway.New(backends...)
	if err != nil {
		cleanup()
		die("gateway init failed", "err", err)
	}
```

- [ ] **Step 3: Build the providers map (replace the single `provider` literal 144-149)**

```go
	providers := map[string]creds.Provider{}
	for _, b := range backends {
		providers[b.Vendor.Name] = &gateway.Provider{
			GW:      gw,
			Vendor:  b.Vendor.Name,
			BaseURL: "http://" + gwAddr,
			Budget:  cfg.TaskBudgetUSD,
			TTL:     cfg.TaskTimeout + 5*time.Minute,
		}
	}
	slog.Info("agents available", "anthropic", anthropicKey != "", "openai", openaiKey != "")
```
(The `b := &broker.Broker{...}` literal is updated in Task 4 to consume `providers`. Until then, temporarily set `Creds: providers["anthropic"]` so this task compiles — Task 4 replaces it.)

- [ ] **Step 4: Run + commit**

Run: `go build ./... && go test ./cmd/brokerd/...`
Expected: PASS.
```bash
git add cmd/brokerd/main.go
git commit -m "brokerd: load both vendor keys; build a provider per available vendor"
```

---

### Task 4: agent→vendor mapping + broker task plumbing

**Files:**
- Create: `internal/agent/agent.go`
- Test: `internal/agent/agent_test.go`
- Modify: `internal/broker/broker.go` (struct: drop `Creds`, add `Providers map[string]creds.Provider` + `DefaultAgent string`; `Task.Agent`; resolve+reject; inject `DRYDOCK_AGENT`)
- Modify: `cmd/brokerd/main.go` (pass `Providers: providers`, `DefaultAgent: cfg.DefaultAgent`)
- Modify: `internal/broker/broker_test.go` (use `Providers` map)

**Interfaces:**
- Produces: `func agent.Vendor(name string) (vendor string, ok bool)` — `""`/`"claude"`→`"anthropic"`, `"codex"`→`"openai"`. `Broker.Providers`, `Broker.DefaultAgent`, `Task.Agent`.

- [ ] **Step 1: Write `agent_test.go`**

```go
package agent

import "testing"

func TestVendor(t *testing.T) {
	cases := map[string]struct {
		vendor string
		ok     bool
	}{
		"":       {"anthropic", true},
		"claude": {"anthropic", true},
		"codex":  {"openai", true},
		"bogus":  {"", false},
	}
	for in, want := range cases {
		v, ok := Vendor(in)
		if v != want.vendor || ok != want.ok {
			t.Errorf("Vendor(%q) = (%q,%v), want (%q,%v)", in, v, ok, want.vendor, want.ok)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agent/ -v`
Expected: FAIL (no package / undefined Vendor).

- [ ] **Step 3: Implement `agent.go`**

```go
// Package agent maps a coding-agent CLI name to the API vendor it talks to.
package agent

// Vendor returns the gateway vendor backing an agent CLI. Empty agent means
// the operator default elsewhere resolved to claude. Unknown agents return
// ok=false so callers fail closed.
func Vendor(name string) (string, bool) {
	switch name {
	case "", "claude":
		return "anthropic", true
	case "codex":
		return "openai", true
	default:
		return "", false
	}
}
```

- [ ] **Step 4: Update the Broker struct + Task**

In `broker.go`, replace `Creds      creds.Provider` with:
```go
	Providers    map[string]creds.Provider // vendor -> provider
	DefaultAgent string                     // "" -> "claude"
```
Add to `Task` (after `Model`):
```go
	// Agent selects the sandbox CLI: "claude" (default) or "codex". Empty
	// falls back to Broker.DefaultAgent, then "claude". Unknown agents are
	// rejected before any VM starts (fail-closed).
	Agent string `json:"agent"`
```
Add import `"drydock/internal/agent"`.

- [ ] **Step 5: Resolve agent + reject, then mint from the right provider (replace lines 291-296)**

```go
	agentName := t.Agent
	if agentName == "" {
		agentName = b.DefaultAgent
	}
	if agentName == "" {
		agentName = "claude"
	}
	vendor, known := agent.Vendor(agentName)
	if !known {
		http.Error(w, "unknown agent: "+agentName+" (want claude|codex)", http.StatusBadRequest)
		return
	}
	prov := b.Providers[vendor]
	if prov == nil {
		http.Error(w, "agent unavailable — no API key configured for "+agentName, http.StatusBadRequest)
		return
	}
	grant, err := prov.Mint(b.TaskBudget)
	if err != nil {
		http.Error(w, "credential mint failed", http.StatusInternalServerError)
		return
	}
	defer grant.Revoke()
```

- [ ] **Step 6: Inject `DRYDOCK_AGENT` into the task env (after the `modelEnv` append, line ~326)**

```go
	env = append(env, modelEnv(t.Model, b.DefaultModel)...)
	env = append(env, "DRYDOCK_AGENT="+agentName)
```

- [ ] **Step 7: Update brokerd Broker literal**

In `cmd/brokerd/main.go`, replace the temporary `Creds: providers["anthropic"],` with:
```go
		Providers:    providers,
		DefaultAgent: cfg.DefaultAgent,
```

- [ ] **Step 8: Update `broker_test.go`**

Anywhere a `Broker{... Creds: fake ...}` literal appears, change to `Providers: map[string]creds.Provider{"anthropic": fake}`. (Codex paths are covered by the agent unit test + integration test; broker unit tests stay claude-only.)

- [ ] **Step 9: Run + commit**

Run: `go build ./... && go test ./internal/agent/ ./internal/broker/ ./cmd/brokerd/...`
Expected: PASS.
```bash
git add internal/agent internal/broker cmd/brokerd/main.go
git commit -m "broker: per-task agent selection (claude|codex), reject unknown/unavailable"
```

---

### Task 5: config `default_agent`

**Files:**
- Modify: `internal/config/config.go` (field, default, env override, validate, SeedTemplate)
- Modify: `config/config.yaml` (on-disk seed mirror — drift test guards it)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `Config.DefaultAgent string` (yaml `default_agent`), env `DRYDOCK_DEFAULT_AGENT`, default `"claude"`, validated to `{claude,codex}`.

- [ ] **Step 1: Write the failing tests (append to config_test.go)**

```go
func TestDefaultAgent_DefaultsToClaude(t *testing.T) {
	if got := Defaults().DefaultAgent; got != "claude" {
		t.Errorf("DefaultAgent default = %q, want claude", got)
	}
}

func TestValidate_RejectsBadAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte("network: x\ngateway_ip: 1.2.3.4\ndefault_agent: gpt\n"), 0o644)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "default_agent") {
		t.Errorf("want default_agent validation error, got %v", err)
	}
}
```

- [ ] **Step 2: Run → FAIL**

Run: `go test ./internal/config/ -run "DefaultAgent|BadAgent" -v`
Expected: FAIL.

- [ ] **Step 3: Add the field + default**

In `Config` struct after `DefaultModel`:
```go
	// DefaultAgent selects the sandbox CLI when a task doesn't pass --agent.
	// "claude" or "codex".
	DefaultAgent string `yaml:"default_agent"`
```
In `Defaults()` add: `DefaultAgent: "claude",`

- [ ] **Step 4: Env override + validation**

In `applyEnvOverrides`, after the `DRYDOCK_DEFAULT_MODEL` block:
```go
	if v := os.Getenv("DRYDOCK_DEFAULT_AGENT"); v != "" {
		c.DefaultAgent = v
	}
```
In `validate()`, before `return nil`:
```go
	if c.DefaultAgent != "claude" && c.DefaultAgent != "codex" {
		return fmt.Errorf("config: default_agent must be claude or codex, got %q", c.DefaultAgent)
	}
```

- [ ] **Step 5: Update SeedTemplate + config/config.yaml**

In the `SeedTemplate` const, under the per-task section after the `default_model:` line:
```
default_agent:          claude         # sandbox CLI: claude | codex. Per-task --agent overrides.
```
Make the **identical** edit to `config/config.yaml` (the drift test `TestSeedTemplate_MatchesOnDiskTemplate` requires byte-identical content).

- [ ] **Step 6: Run + commit**

Run: `go test ./internal/config/...`
Expected: PASS (incl. the drift test).
```bash
git add internal/config/config.go config/config.yaml internal/config/config_test.go
git commit -m "config: add default_agent (claude|codex), validated"
```

---

### Task 6: CLI `--agent` flag

**Files:**
- Modify: `cmd/drydock/submit.go` (`taskRequest.Agent`, flag, payload)

- [ ] **Step 1: Add the JSON field**

In `taskRequest` after `Model`:
```go
	Agent       string        `json:"agent,omitempty"`
```

- [ ] **Step 2: Add the flag**

In `runSubmit`'s flag block after `model`:
```go
		agent       = fs.String("agent", "", "sandbox agent: claude | codex (default: broker's default_agent)")
```

- [ ] **Step 3: Thread it into the request**

In the `req := taskRequest{...}` literal add:
```go
		Agent:       *agent,
```

- [ ] **Step 4: Run + commit**

Run: `go build ./... && go vet ./cmd/drydock/...`
Expected: PASS.
```bash
git add cmd/drydock/submit.go
git commit -m "cli: drydock submit --agent claude|codex"
```

---

### Task 7: Egress — gateway host set + allowlist additions

**Files:**
- Modify: `internal/netfw/netfw.go` (`modelAPIHost` const → set; skip both)
- Modify: `config/egress.yaml`, `cmd/drydock/init.go:164` (`defaultEgressYAML`), `internal/egress/testdata/egress.yaml` (add `api.openai.com`)
- Test: `internal/netfw/netfw_test.go` (if present — assert openai host excluded)

**Interfaces:**
- Produces: `netfw` excludes both `api.anthropic.com` and `api.openai.com` from the squid allowlist (both reached via the gateway).

- [ ] **Step 1: Generalize the const (netfw.go:13-26)**

```go
// gatewayHosts are reached via the credential gateway, not squid.
var gatewayHosts = map[string]bool{
	"api.anthropic.com": true,
	"api.openai.com":    true,
}

func CompileSquidAllowlist(cfg egress.Config) string {
	var b strings.Builder
	for _, d := range cfg.Default.Domains {
		if gatewayHosts[d.Host] {
			continue
		}
		fmt.Fprintf(&b, "%s\n", d.Host)
	}
	return b.String()
}
```

- [ ] **Step 2: Add `api.openai.com` to the three allowlist copies**

In `config/egress.yaml`, `cmd/drydock/init.go` (`defaultEgressYAML`), and `internal/egress/testdata/egress.yaml`, add directly under the `api.anthropic.com` line:
```
    - { host: api.openai.com,         ports: [443] }
```

- [ ] **Step 3: Run + commit**

Run: `go test ./internal/netfw/... ./internal/egress/...`
Expected: PASS.
```bash
git add internal/netfw/netfw.go config/egress.yaml cmd/drydock/init.go internal/egress/testdata/egress.yaml
git commit -m "egress: route api.openai.com via the gateway; add to allowlist"
```

---

### Task 8: Image — install Codex, dispatch in entrypoint, doctor check

**Files:**
- Modify: `image/Dockerfile` (add codex install ARG + RUN)
- Modify: `image/entrypoint.sh` (dispatch on `DRYDOCK_AGENT`)
- Modify: `cmd/drydock/doctor.go` (add a `codex --version` check)

**Interfaces:**
- Consumes: `DRYDOCK_AGENT` (set by broker, Task 4), `DRYDOCK_MODEL` (existing).

- [ ] **Step 1: Install Codex in the Dockerfile (after the claude-code RUN, line 24)**

```dockerfile
# Pin the Codex CLI alongside claude-code. Bump deliberately; every drydock
# release anchors to a known codex version. Override with
# --build-arg CODEX_VERSION=x.y.z.
ARG CODEX_VERSION=0.40.0
RUN npm install -g @openai/codex@${CODEX_VERSION}
```
(Verify the published version at build time; pick the current `@openai/codex` release.)

- [ ] **Step 2: Dispatch in entrypoint.sh (replace the exec block, lines 6-16)**

```bash
cd /work
PROMPT="$(cat /work/.task/prompt.txt)"
AGENT="${DRYDOCK_AGENT:-claude}"

case "$AGENT" in
  codex)
    # The VM is the isolation boundary, so disable codex's own sandbox and
    # approval prompts. DRYDOCK_MODEL (when set) selects the model.
    MODEL_ARGS=()
    if [ -n "${DRYDOCK_MODEL:-}" ]; then
        MODEL_ARGS=(--model "${DRYDOCK_MODEL}")
    fi
    exec gosu agent codex exec \
        --dangerously-bypass-approvals-and-sandbox \
        "${MODEL_ARGS[@]}" \
        "${PROMPT}"
    ;;
  claude)
    MODEL_ARGS=()
    if [ -n "${DRYDOCK_MODEL:-}" ]; then
        MODEL_ARGS=(--model "${DRYDOCK_MODEL}")
    fi
    exec gosu agent claude --bare -p "${PROMPT}" \
        "${MODEL_ARGS[@]}" \
        --dangerously-skip-permissions \
        --output-format stream-json --verbose --include-partial-messages
    ;;
  *)
    echo "drydock: unknown DRYDOCK_AGENT=$AGENT" >&2
    exit 64
    ;;
esac
```
(Verify codex's exact non-interactive flags against the pinned version; `codex exec` is the headless subcommand. Adjust `--model`/bypass flag names to that version.)

- [ ] **Step 3: Add a codex check to doctor.go (after the `sandbox boot` block, ~line 62)**

```go
	// 2b. Codex CLI must also be installed (the image hosts both agents).
	out, err = exec.Command("container", "run", "--rm", "--entrypoint", "/bin/sh",
		cfg.SandboxImage, "-c", "codex --version 2>&1").CombinedOutput()
	if err != nil {
		step("codex present", false, "codex --version failed: "+strings.TrimSpace(string(out)))
		failed = true
	} else {
		step("codex present", true, strings.TrimSpace(lastLine(string(out))))
	}
```
Add helper near `claudeVersionLine`:
```go
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
```

- [ ] **Step 4: Build the image + smoke (manual, macOS host)**

Run: `container build -t drydock-sandbox:latest image/` then `drydock doctor`
Expected: `claude --version` and `codex --version` both pass. (If the build host isn't macOS/Apple-silicon, note that this step is deferred to a capable host — do not claim it passed if unrun.)

- [ ] **Step 5: Commit**

```bash
git add image/Dockerfile image/entrypoint.sh cmd/drydock/doctor.go
git commit -m "image: install Codex CLI; entrypoint dispatches on DRYDOCK_AGENT; doctor checks codex"
```

---

### Task 9: Rename `claude-sandbox` → `drydock-sandbox`

The image now hosts both agents; the name should be vendor-neutral. **Only edit live files** (skip the frozen `docs/superpowers/plans/2026-06-14*`, `2026-06-15*`, `specs/2026-06-14*`).

**Files:**
- Modify: `internal/config/config.go:67,258`, `config/config.yaml:10`, `internal/config/config_test.go:25,162`, `internal/runner/runner_test.go:12,39`, `cmd/drydock/init.go:331,345`, `Makefile:25,71`, `README.md:254,300`, `cmd/brokerd/main.go:342` (comment)

- [ ] **Step 1: Replace the string across live files**

Set every live `claude-sandbox` occurrence to `drydock-sandbox`. The drift test requires `config/config.yaml` and `SeedTemplate` (`config.go:258`) to stay byte-identical, so edit both. In `init.go:345`, update the stale-image guard condition `name == "claude-sandbox"` → `name == "drydock-sandbox"` (and the `ensureNamedImage("claude-sandbox", ...)` call at 331).

Command to find remaining live refs (should be empty after edits):
```bash
grep -rn "claude-sandbox" --include="*.go" --include="*.yaml" --include="Makefile" --include="README.md" . | grep -v "docs/superpowers"
```

- [ ] **Step 2: Run + commit**

Run: `go build ./... && go test ./internal/config/... ./internal/runner/...`
Expected: PASS (drift test green).
```bash
git add -u
git commit -m "rename sandbox image claude-sandbox -> drydock-sandbox (hosts both agents)"
```

---

### Task 10: Docs — README + site + threat model (two agents, honest)

**Files:**
- Modify: `README.md`, `site/index.html`, `THREAT_MODEL.md`

**Constraint:** No maturity overclaims. Describe Codex support as new; state the OpenAI key is host-only exactly like the Anthropic key; do not imply an audit.

- [ ] **Step 1: README**

- Update the intro / agent mentions to say drydock runs **Claude Code or OpenAI Codex** in the sandbox.
- In the config table, the `sandbox_image` default is now `drydock-sandbox:latest`; add a `default_agent` / `DRYDOCK_DEFAULT_AGENT` row (`claude`).
- Add `OPENAI_API_KEY` next to `ANTHROPIC_API_KEY` in setup, noting at least one is required and both are host-only.
- Add a `drydock submit --agent codex` example.

- [ ] **Step 2: site/index.html**

- Update copy that says "Claude Code" as the only agent to include Codex (e.g. the architecture / "what runs in the VM" prose). Keep the honest hero status line accurate (still working alpha, no third-party audit).

- [ ] **Step 3: THREAT_MODEL.md**

- Note the gateway now fronts two upstreams; the real key for **whichever** vendor (Anthropic or OpenAI) stays host-only and the VM only ever sees a budget-capped bearer token. No new trust assumptions.

- [ ] **Step 4: Commit**

```bash
git add README.md site/index.html THREAT_MODEL.md
git commit -m "docs: document Codex agent + OPENAI_API_KEY (host-only), no overclaims"
```

---

### Task 11: Integration test — Codex variant (macOS-only, not in CI)

**Files:**
- Modify: `tests/integration/brokerd_test.go`

- [ ] **Step 1: Add a Codex submit variant**

Mirror the existing brokerd task test but POST `{"agent":"codex", ...}` with `OPENAI_API_KEY=sk-...` set on the brokerd env. Assert the task reaches the gated-diff stage. Guard it behind the same macOS/integration build tag the file already uses; skip if `OPENAI_API_KEY` is unset.

- [ ] **Step 2: Run (on a capable host) + commit**

Run: `go test -tags integration ./tests/integration/ -run Codex`
Expected: PASS on macOS/Apple-silicon with a real key; SKIP otherwise. Do not claim a pass if unrun on this host.
```bash
git add tests/integration/brokerd_test.go
git commit -m "test: codex brokerd integration variant (macOS-only)"
```

---

## Self-Review

**Spec coverage:**
- Auth model (gateway-brokered OpenAI) → Tasks 1-3. ✓
- Per-task `--agent` + `default_agent` → Tasks 4-6. ✓
- One image both CLIs, `DRYDOCK_AGENT` dispatch, rename → Tasks 8-9. ✓
- Full budget parity (OpenAI prices + streaming/non-streaming usage parser) → Task 2. ✓
- Egress for `api.openai.com` → Task 7. ✓
- Docs honest, no overclaims → Task 10. ✓
- Verification of OpenAI usage shape against real capture → flagged in Task 2 Step 1 (covers both Responses + Chat Completions naming) and Task 8 Step 2/4 (verify codex flags/traffic on a capable host). ✓

**Type consistency:** `Vendor`, `Backend`, `New(...Backend)`, `Mint(vendor,budget,ttl)`, `Lease.Vendor`, `Provider.Vendor`, `grant.vendor`, `agent.Vendor(name)`, `Broker.Providers`/`DefaultAgent`, `Task.Agent`, `Config.DefaultAgent` — names match across tasks.

**Known verification points (not placeholders — explicit unknowns to confirm at execution):**
- Exact `@openai/codex` version + non-interactive flag names (Task 8) — verify against the published CLI.
- OpenAI usage event/field names (Task 2) — parser handles both known shapes; confirm against a real Codex capture during the integration test.
- OpenAI price numbers (Task 2) are approximate by design (safety cap, like the Anthropic table).
