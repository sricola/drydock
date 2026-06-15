# Credential Gateway + Userspace Egress (v2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Get the real API key out of the untrusted VM and enforce egress host-side: an in-broker credential gateway (real key host-only, per-task bearer token, budget + revoke) plus a userspace squid forward proxy, with the VM pinned by in-VM nft to exactly those two host ports — no pf, no per-task network churn.

**Architecture:** `brokerd` runs a `gateway` goroutine (L7 reverse proxy to `api.anthropic.com` that swaps a bearer token for the real `X-Api-Key` and meters usage against a USD budget) and a userspace `squid` (CONNECT allowlist by hostname for registries), both bound to the vmnet gateway IP of one stable network. The VM gets `ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN` + `HTTPS_PROXY` and an nft rule allowing egress only to `:8088`/`:3128` (DNS dropped). The `creds` seam is refactored to a `Grant`/`EnvVars` interface so the gateway provider swaps in for the static-key provider.

**Tech Stack:** Go 1.26 (stdlib `net/http/httputil`, `crypto/rand`, `httptest`), `apple/container` 1.0.0, squid, `nftables`. Builds on the merged MVP.

**Spec:** `docs/superpowers/specs/2026-06-15-tcb-egress-credential-gateway-design.md`

## Execution amendments (from the Task 1 spike, 2026-06-15)

1. **Subnet/IP:** the default network already holds `192.168.64.0/24`, so the egress network uses
   `--subnet 192.168.66.0/24`; gateway IP = **`192.168.66.1`**. Use these everywhere
   (`MACAGENT_GW_IP=192.168.66.1`, `MACAGENT_SUBNET=192.168.66.0/24`).
2. **Network anchor (affects Task 9):** the host gateway IP exists only while a container is on the
   network, so `brokerd` must start a persistent idle **anchor** container on `macagent-egress` at
   startup and bind the gateway/squid to `192.168.66.1` **exclusively** (never `0.0.0.0`). The
   gateway goroutine must retry the bind until the interface is up. The Task 9 `main.go` below is
   superseded by this: add `startAnchor(network, image)` + a `listenWhenReady(addr)` retry loop and
   use `http.Serve(listener, gw)` instead of `http.ListenAndServe`.

---

## File Structure

```
internal/gateway/
  pricing.go        Price table + cost(model,in,out)
  pricing_test.go
  usage.go          parseUsage(body,contentType) (model,in,out,ok)  — JSON + SSE
  usage_test.go
  gateway.go        Lease, Gateway, Mint/Revoke/check, ServeHTTP, director, usage metering
  gateway_test.go
  provider.go       gateway.Provider implementing creds.Provider (Grant w/ BASE_URL+AUTH_TOKEN)
  provider_test.go
internal/creds/
  creds.go          REFACTOR: Grant + Provider interfaces; StaticProvider
  creds_test.go     REFACTOR
internal/netfw/
  netfw.go          CompileSquidAllowlist(egress.Config), gatewayIP(subnet); host ops (squid/network)
  netfw_test.go
internal/runner/
  runner.go         MODIFY: Spec.Env []string (replaces APIKey); keep CAP_NET_ADMIN
  runner_test.go    MODIFY
internal/broker/
  broker.go         MODIFY: build grant env + proxy env + GW_IP; attach network
cmd/brokerd/
  main.go           MODIFY: start gateway+squid, pick StaticProvider or gateway.Provider
image/
  init-firewall.sh  REWRITE: pin to GW_IP:8088 and :3128, drop DNS
  entrypoint.sh     MODIFY: call init-firewall with $MACAGENT_GW_IP
```

---

### Task 1: Phase-0 networking spike (host integration)

**Files:** none (verification only). **Requires** `apple/container` 1.0.0 (installed on the dev host).

Validate the one unknown before building `netfw`/wiring: can a host process bind the vmnet gateway IP and be reached from inside a VM on a stable network, and what is that IP.

- [ ] **Step 1: Create a stable network and inspect its gateway IP**

Run:
```bash
container network create --subnet 192.168.64.0/24 macagent-egress 2>&1 || true
container network inspect macagent-egress 2>&1 | head -40
```
Expected: a network exists with subnet `192.168.64.0/24`. Note the gateway/host address (expected `192.168.64.1`). If the inspect output names a different gateway address, record it — later tasks use `MACAGENT_GW_IP`.

- [ ] **Step 2: Start a host listener on the gateway IP and reach it from a VM**

Run (host listener in one shell):
```bash
# host: a trivial HTTP server bound to the vmnet gateway IP
python3 -c "import http.server,socketserver; socketserver.TCPServer(('192.168.64.1',8088),http.server.SimpleHTTPRequestHandler).serve_forever()" &
HOST_PID=$!
# VM: curl the host gateway IP from inside a container on that network
container run --rm --network macagent-egress --entrypoint /bin/sh claude-sandbox:latest \
  -c 'curl -sS -m 5 http://192.168.64.1:8088/ -o /dev/null -w "reach:%{http_code}\n" || echo "reach:FAIL"'
kill $HOST_PID 2>/dev/null
```
Expected: `reach:200` (or any HTTP code) → the VM can reach a host process on the vmnet gateway IP. If `reach:FAIL`, the bind/reachability assumption is wrong.

- [ ] **Step 3: Record the outcome and decide**

**EXECUTED 2026-06-15 — RESULT: PASS, with a caveat that adds an anchor requirement.**
- The default network already occupies `192.168.64.0/24`, so the egress network was created on
  `--subnet 192.168.66.0/24` → gateway IP **`192.168.66.1`**. Defaults for Tasks 6/8/9/10:
  `MACAGENT_GW_IP=192.168.66.1`, `MACAGENT_SUBNET=192.168.66.0/24`.
- **Caveat:** the host address `192.168.66.1` exists **only while a container is attached** to the
  network. Binding it with no VM present fails `Can't assign requested address`. With a VM attached,
  a host listener on `192.168.66.1:8088` is reachable from a second VM (`reach:200`).
- **Decision → network anchor (NOT 0.0.0.0).** `brokerd` keeps one idle "anchor" container on
  `macagent-egress` for its lifetime so the gateway interface stays up and the gateway/squid can bind
  `192.168.66.1` **exclusively**. Binding `0.0.0.0` is rejected: it would expose the credential
  gateway on every host interface (LAN/wifi). This anchor handling is folded into Task 9.

---

### Task 2: Pricing + usage parsing (pure functions)

**Files:**
- Create: `internal/gateway/pricing.go`, `internal/gateway/pricing_test.go`
- Create: `internal/gateway/usage.go`, `internal/gateway/usage_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/gateway/pricing_test.go`:
```go
package gateway

import (
	"math"
	"testing"
)

func TestCost_KnownModel(t *testing.T) {
	prices := map[string]Price{"claude-x": {InputPer1M: 3, OutputPer1M: 15}}
	got := cost(prices, "claude-x", 1_000_000, 2_000_000)
	want := 3.0 + 30.0
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("cost = %v, want %v", got, want)
	}
}

func TestCost_FallsBackToDefault(t *testing.T) {
	prices := map[string]Price{"default": {InputPer1M: 10, OutputPer1M: 10}}
	got := cost(prices, "unknown-model", 500_000, 0)
	if math.Abs(got-5.0) > 1e-9 {
		t.Errorf("cost = %v, want 5.0", got)
	}
}
```

Create `internal/gateway/usage_test.go`:
```go
package gateway

import "testing"

func TestParseUsage_JSON(t *testing.T) {
	body := []byte(`{"model":"claude-x","usage":{"input_tokens":12,"output_tokens":34}}`)
	model, in, out, ok := parseUsage(body, "application/json")
	if !ok || model != "claude-x" || in != 12 || out != 34 {
		t.Fatalf("got (%q,%d,%d,%v)", model, in, out, ok)
	}
}

func TestParseUsage_SSE(t *testing.T) {
	body := []byte(
		"event: message_start\n" +
			`data: {"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","usage":{"output_tokens":42}}` + "\n\n")
	model, in, out, ok := parseUsage(body, "text/event-stream; charset=utf-8")
	if !ok || model != "claude-x" || in != 10 || out != 42 {
		t.Fatalf("got (%q,%d,%d,%v)", model, in, out, ok)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/gateway/`
Expected: FAIL — `undefined: Price`, `undefined: cost`, `undefined: parseUsage`.

- [ ] **Step 3: Write the implementations**

Create `internal/gateway/pricing.go`:
```go
package gateway

// Price is the USD cost per 1M tokens.
type Price struct {
	InputPer1M  float64
	OutputPer1M float64
}

// DefaultPrices is a coarse table; "default" is the fallback for unknown models.
// Tune to your actual model mix — this only gates the per-task budget.
func DefaultPrices() map[string]Price {
	return map[string]Price{
		"default": {InputPer1M: 3, OutputPer1M: 15},
	}
}

func cost(prices map[string]Price, model string, in, out int) float64 {
	p, ok := prices[model]
	if !ok {
		p = prices["default"]
	}
	return float64(in)/1e6*p.InputPer1M + float64(out)/1e6*p.OutputPer1M
}
```

Create `internal/gateway/usage.go`:
```go
package gateway

import (
	"encoding/json"
	"strings"
)

// parseUsage extracts (model, input tokens, output tokens) from an Anthropic
// response body, handling both a single JSON message and an SSE stream.
func parseUsage(body []byte, contentType string) (model string, in, out int, ok bool) {
	if strings.Contains(contentType, "text/event-stream") {
		return parseSSEUsage(body)
	}
	var m struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &m) == nil && (m.Usage.InputTokens > 0 || m.Usage.OutputTokens > 0) {
		return m.Model, m.Usage.InputTokens, m.Usage.OutputTokens, true
	}
	return "", 0, 0, false
}

func parseSSEUsage(body []byte) (model string, in, out int, ok bool) {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Model string `json:"model"`
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			model, in, out, ok = ev.Message.Model, ev.Message.Usage.InputTokens, ev.Message.Usage.OutputTokens, true
		case "message_delta":
			if ev.Usage.OutputTokens > 0 {
				out, ok = ev.Usage.OutputTokens, true
			}
		}
	}
	return
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/gateway/`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/pricing.go internal/gateway/pricing_test.go internal/gateway/usage.go internal/gateway/usage_test.go
git commit -m "feat(gateway): model pricing and usage parsing (JSON + SSE)"
```

---

### Task 3: Gateway core (token store + reverse proxy + metering)

**Files:**
- Create: `internal/gateway/gateway.go`, `internal/gateway/gateway_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/gateway/gateway_test.go`:
```go
package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// upstream stands in for api.anthropic.com; it asserts the gateway swapped creds.
func upstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("Authorization should be stripped, got %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Api-Key") != "REAL" {
			t.Errorf("X-Api-Key = %q, want REAL", r.Header.Get("X-Api-Key"))
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"claude-x","usage":{"input_tokens":1000000,"output_tokens":1000000}}`)
	}))
}

func newGW(t *testing.T, up string) *Gateway {
	t.Helper()
	g, err := New("REAL", up, map[string]Price{"claude-x": {InputPer1M: 3, OutputPer1M: 15}})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func do(g *Gateway, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "http://gw/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	return rec
}

func TestGateway_ValidTokenProxiesAndMeters(t *testing.T) {
	up := upstream(t)
	defer up.Close()
	g := newGW(t, up.URL)
	tok := g.Mint(100, time.Minute)

	rec := do(g, tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	// 1M in @3 + 1M out @15 = 18.0 spent
	if got := g.spent(tok); got < 17.9 || got > 18.1 {
		t.Errorf("spent = %v, want ~18", got)
	}
}

func TestGateway_UnknownToken401(t *testing.T) {
	g := newGW(t, "http://unused")
	if rec := do(g, "nope"); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestGateway_ExpiredToken401(t *testing.T) {
	g := newGW(t, "http://unused")
	tok := g.Mint(100, -time.Second) // already expired
	if rec := do(g, tok); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestGateway_OverBudget402(t *testing.T) {
	up := upstream(t)
	defer up.Close()
	g := newGW(t, up.URL)
	tok := g.Mint(1.0, time.Minute) // budget 1.0; one call spends ~18 → next call 402
	do(g, tok)
	if rec := do(g, tok); rec.Code != http.StatusPaymentRequired {
		t.Errorf("status = %d, want 402", rec.Code)
	}
}

func TestGateway_RevokeInvalidates(t *testing.T) {
	g := newGW(t, "http://unused")
	tok := g.Mint(100, time.Minute)
	g.Revoke(tok)
	if rec := do(g, tok); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d after revoke, want 401", rec.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gateway/ -run TestGateway`
Expected: FAIL — `undefined: New`, `undefined: Gateway`, etc.

- [ ] **Step 3: Write the implementation**

Create `internal/gateway/gateway.go`:
```go
// Package gateway is an in-broker reverse proxy in front of api.anthropic.com.
// The VM authenticates with a per-task bearer token; the gateway holds the real
// key (never exposed to the VM), swaps it in, and meters usage against a budget.
package gateway

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

type Lease struct {
	BudgetUSD float64
	SpentUSD  float64
	Expiry    time.Time
}

type Gateway struct {
	mu       sync.Mutex
	leases   map[string]*Lease
	realKey  string
	upstream *url.URL
	prices   map[string]Price
	proxy    *httputil.ReverseProxy
}

type ctxKey struct{}

func New(realKey, upstream string, prices map[string]Price) (*Gateway, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	g := &Gateway{
		leases:   map[string]*Lease{},
		realKey:  realKey,
		upstream: u,
		prices:   prices,
	}
	g.proxy = &httputil.ReverseProxy{Director: g.director, ModifyResponse: g.meter}
	return g, nil
}

func (g *Gateway) Mint(budgetUSD float64, ttl time.Duration) string {
	b := make([]byte, 18)
	rand.Read(b)
	tok := "tok_" + hex.EncodeToString(b)
	g.mu.Lock()
	g.leases[tok] = &Lease{BudgetUSD: budgetUSD, Expiry: time.Now().Add(ttl)}
	g.mu.Unlock()
	return tok
}

func (g *Gateway) Revoke(token string) {
	g.mu.Lock()
	delete(g.leases, token)
	g.mu.Unlock()
}

func (g *Gateway) spent(token string) float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if l := g.leases[token]; l != nil {
		return l.SpentUSD
	}
	return -1
}

// check returns (lease, 0) when usable, or (nil, statusCode) to reject.
func (g *Gateway) check(token string) (*Lease, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	l := g.leases[token]
	if l == nil || time.Now().After(l.Expiry) {
		return nil, http.StatusUnauthorized
	}
	if l.SpentUSD >= l.BudgetUSD {
		return nil, http.StatusPaymentRequired
	}
	return l, 0
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok := ""
	if a := r.Header.Get("Authorization"); len(a) > 7 && a[:7] == "Bearer " {
		tok = a[7:]
	}
	lease, status := g.check(tok)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	ctx := contextWith(r, lease)
	g.proxy.ServeHTTP(w, r.WithContext(ctx))
}

func (g *Gateway) director(req *http.Request) {
	req.URL.Scheme = g.upstream.Scheme
	req.URL.Host = g.upstream.Host
	req.Host = g.upstream.Host
	req.Header.Del("Authorization")
	req.Header.Set("X-Api-Key", g.realKey)
	if req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
}

// meter tees the response body and, on completion, adds its cost to the lease.
func (g *Gateway) meter(resp *http.Response) error {
	lease, _ := resp.Request.Context().Value(ctxKey{}).(*Lease)
	if lease == nil {
		return nil
	}
	ct := resp.Header.Get("Content-Type")
	resp.Body = &usageReader{rc: resp.Body, onDone: func(body []byte) {
		if model, in, out, ok := parseUsage(body, ct); ok {
			g.mu.Lock()
			lease.SpentUSD += cost(g.prices, model, in, out)
			g.mu.Unlock()
		}
	}}
	return nil
}

// usageReader buffers the streamed body and invokes onDone once at EOF/Close.
type usageReader struct {
	rc     io.ReadCloser
	buf    bytes.Buffer
	onDone func([]byte)
	done   bool
}

func (u *usageReader) Read(p []byte) (int, error) {
	n, err := u.rc.Read(p)
	if n > 0 {
		u.buf.Write(p[:n])
	}
	if err == io.EOF {
		u.finish()
	}
	return n, err
}

func (u *usageReader) Close() error {
	u.finish()
	return u.rc.Close()
}

func (u *usageReader) finish() {
	if u.done {
		return
	}
	u.done = true
	u.onDone(u.buf.Bytes())
}
```

Create `internal/gateway/context.go` (tiny helper so `ctxKey` stays unexported and shared):
```go
package gateway

import (
	"context"
	"net/http"
)

func contextWith(r *http.Request, l *Lease) context.Context {
	return context.WithValue(r.Context(), ctxKey{}, l)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/gateway/`
Expected: PASS (all gateway tests + Task 2 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/gateway.go internal/gateway/gateway_test.go internal/gateway/context.go
git commit -m "feat(gateway): token store + reverse proxy + usage metering"
```

---

### Task 4: Refactor `creds` to Grant/Provider

**Files:**
- Modify: `internal/creds/creds.go`
- Modify: `internal/creds/creds_test.go`

This generalizes the seam so the gateway provider is interchangeable with the static one.

- [ ] **Step 1: Replace the test**

Replace the entire contents of `internal/creds/creds_test.go`:
```go
package creds

import (
	"slices"
	"testing"
)

func TestStaticProvider_GrantEnvAndRevoke(t *testing.T) {
	var p Provider = StaticProvider{Key: "sk-static"}
	g, err := p.Mint(5.0)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !slices.Contains(g.EnvVars(), "ANTHROPIC_API_KEY=sk-static") {
		t.Errorf("EnvVars = %v", g.EnvVars())
	}
	if err := g.Revoke(); err != nil {
		t.Errorf("Revoke: %v", err)
	}
}

func TestStaticProvider_EmptyKeyErrors(t *testing.T) {
	if _, err := (StaticProvider{Key: ""}).Mint(1.0); err == nil {
		t.Errorf("want error for empty key")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/creds/`
Expected: FAIL — compile error (`Mint` signature changed, `Grant` undefined).

- [ ] **Step 3: Replace the implementation**

Replace the entire contents of `internal/creds/creds.go`:
```go
// Package creds issues a credential Grant per task. A Grant exposes the env vars
// to inject into the sandbox and a Revoke hook. This lets a gateway-backed,
// per-task-token provider replace the static-key provider without changing callers.
package creds

import "errors"

type Grant interface {
	EnvVars() []string
	Revoke() error
}

type Provider interface {
	// Mint issues a grant; budgetUSD is advisory for providers that meter spend.
	Mint(budgetUSD float64) (Grant, error)
}

type StaticProvider struct {
	Key string
}

type staticGrant struct{ key string }

func (p StaticProvider) Mint(float64) (Grant, error) {
	if p.Key == "" {
		return nil, errors.New("creds: empty static key")
	}
	return staticGrant{p.Key}, nil
}

func (g staticGrant) EnvVars() []string { return []string{"ANTHROPIC_API_KEY=" + g.key} }
func (g staticGrant) Revoke() error     { return nil }
```

- [ ] **Step 4: Run test to verify it passes (creds only — broker won't compile yet)**

Run: `go test ./internal/creds/`
Expected: PASS. (`go build ./...` will still fail until Task 6 updates the broker — that's expected; this task is scoped to `creds`.)

- [ ] **Step 5: Commit**

```bash
git add internal/creds/
git commit -m "refactor(creds): Grant/EnvVars seam for interchangeable providers"
```

---

### Task 5: Gateway provider (implements creds.Provider)

**Files:**
- Create: `internal/gateway/provider.go`, `internal/gateway/provider_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/gateway/provider_test.go`:
```go
package gateway

import (
	"slices"
	"strings"
	"testing"
	"time"

	"macagent/internal/creds"
)

func TestProvider_GrantCarriesBaseURLAndToken(t *testing.T) {
	g, _ := New("REAL", "http://unused", DefaultPrices())
	var p creds.Provider = &Provider{GW: g, BaseURL: "http://192.168.64.1:8088", TTL: time.Minute}

	grant, err := p.Mint(2.5)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	env := grant.EnvVars()
	if !slices.Contains(env, "ANTHROPIC_BASE_URL=http://192.168.64.1:8088") {
		t.Errorf("env missing base url: %v", env)
	}
	hasToken := false
	for _, e := range env {
		if strings.HasPrefix(e, "ANTHROPIC_AUTH_TOKEN=tok_") {
			hasToken = true
		}
	}
	if !hasToken {
		t.Errorf("env missing auth token: %v", env)
	}
	// Revoke must invalidate the underlying gateway lease.
	if err := grant.Revoke(); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gateway/ -run TestProvider`
Expected: FAIL — `undefined: Provider`.

- [ ] **Step 3: Write the implementation**

Create `internal/gateway/provider.go`:
```go
package gateway

import (
	"time"

	"macagent/internal/creds"
)

// Provider issues creds.Grants backed by gateway tokens. The real key never
// leaves the host; the VM only ever sees a bearer token + the base URL.
type Provider struct {
	GW      *Gateway
	BaseURL string        // e.g. http://192.168.64.1:8088
	Budget  float64       // default budget when Mint's arg is 0
	TTL     time.Duration // safety-net expiry (= task timeout + margin)
}

type grant struct {
	gw      *Gateway
	token   string
	baseURL string
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
	return &grant{gw: p.GW, token: p.GW.Mint(b, ttl), baseURL: p.BaseURL}, nil
}

func (g *grant) EnvVars() []string {
	return []string{
		"ANTHROPIC_BASE_URL=" + g.baseURL,
		"ANTHROPIC_AUTH_TOKEN=" + g.token,
	}
}

func (g *grant) Revoke() error {
	g.gw.Revoke(g.token)
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/gateway/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/provider.go internal/gateway/provider_test.go
git commit -m "feat(gateway): creds.Provider backed by gateway tokens"
```

---

### Task 6: Generalize runner env + fix the broker to compile

**Files:**
- Modify: `internal/runner/runner.go`, `internal/runner/runner_test.go`
- Modify: `internal/broker/broker.go`

- [ ] **Step 1: Update the runner test**

In `internal/runner/runner_test.go`, replace the `Spec{...}` construction and the `ANTHROPIC_API_KEY` expectation. Set the Spec to:
```go
	args := BuildRunArgs(Spec{
		TaskID:     "abc123",
		Network:    "macagent-egress",
		ImageRef:   "claude-sandbox:latest",
		Env:        []string{"ANTHROPIC_BASE_URL=http://gw:8088", "MACAGENT_GW_IP=192.168.64.1"},
		StageDir:   "/tmp/broker/stage/abc123",
		PromptFile: "/work/.task/prompt.txt",
		MemoryGB:   4,
		CPUs:       4,
	})
```
And in the `want` table replace the `ANTHROPIC_API_KEY` line with:
```go
		{"--env", "ANTHROPIC_BASE_URL=http://gw:8088"},
		{"--env", "MACAGENT_GW_IP=192.168.64.1"},
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/runner/`
Expected: FAIL — `unknown field 'Env'` / `APIKey` removed.

- [ ] **Step 3: Update the runner implementation**

Replace `internal/runner/runner.go` with:
```go
// Package runner builds the `container` CLI argv for a sandbox task.
package runner

import "fmt"

type Spec struct {
	TaskID     string
	Network    string
	ImageRef   string
	Env        []string // injected as --env pairs (grant env + proxy env + GW ip)
	StageDir   string
	PromptFile string
	MemoryGB   int
	CPUs       int
}

// BuildRunArgs returns the argv that follows the `container` binary name.
func BuildRunArgs(s Spec) []string {
	args := []string{
		"run", "--rm",
		"--name", "task-" + s.TaskID,
		"--user", "agent",
		// nft egress firewall installs as root in the entrypoint; needs CAP_NET_ADMIN.
		"--cap-add", "CAP_NET_ADMIN",
		"--memory", fmt.Sprintf("%dG", s.MemoryGB),
		"--cpus", fmt.Sprintf("%d", s.CPUs),
		"--network", s.Network,
	}
	for _, e := range s.Env {
		args = append(args, "--env", e)
	}
	args = append(args,
		"--env", "TASK_PROMPT_FILE="+s.PromptFile,
		"--mount", fmt.Sprintf("type=bind,source=%s,target=/work,readonly=false", s.StageDir),
		s.ImageRef,
		"/usr/local/bin/entrypoint.sh",
	)
	return args
}
```

- [ ] **Step 4: Update the broker to use the new seam**

In `internal/broker/broker.go`, the `Broker` struct gains the gateway wiring inputs. Replace the credential + run section. First add fields to the struct:
```go
type Broker struct {
	Cfg        egress.Config
	Creds      creds.Provider
	Approve    ApprovalFn
	ImageRef   string
	StageRoot  string
	AuditRoot  string
	Timeout    time.Duration
	Network    string  // stable egress network name (e.g. macagent-egress)
	GatewayIP  string  // vmnet gateway IP the VM reaches (e.g. 192.168.64.1)
	ProxyPort  int     // squid port (e.g. 3128)
	TaskBudget float64 // USD budget per task
}
```
Then replace the mint+run block (the part from `tok, err := b.Creds.Mint(...)` through the `runner.BuildRunArgs(...)` call and `dropEmptyNetwork`) with:
```go
	grant, err := b.Creds.Mint(b.TaskBudget)
	if err != nil {
		http.Error(w, "credential mint failed", http.StatusInternalServerError)
		return
	}
	defer grant.Revoke()

	if err := os.MkdirAll(b.AuditRoot, 0o755); err != nil {
		http.Error(w, "audit dir failed", http.StatusInternalServerError)
		return
	}
	logf, err := os.Create(filepath.Join(b.AuditRoot, taskID+".jsonl"))
	if err != nil {
		http.Error(w, "audit file failed", http.StatusInternalServerError)
		return
	}
	defer logf.Close()

	env := append([]string{}, grant.EnvVars()...)
	env = append(env,
		fmt.Sprintf("HTTPS_PROXY=http://%s:%d", b.GatewayIP, b.ProxyPort),
		fmt.Sprintf("HTTP_PROXY=http://%s:%d", b.GatewayIP, b.ProxyPort),
		"NO_PROXY=127.0.0.1,localhost",
		"MACAGENT_GW_IP="+b.GatewayIP,
	)

	args := runner.BuildRunArgs(runner.Spec{
		TaskID:     taskID,
		Network:    b.Network,
		ImageRef:   b.ImageRef,
		Env:        env,
		StageDir:   st.WorkDir,
		PromptFile: "/work/.task/prompt.txt",
		MemoryGB:   4,
		CPUs:       4,
	})
```
Delete the now-unused `dropEmptyNetwork` function and its tests in `internal/broker/broker_test.go` (the network is always set now). Keep the rest of `HandleTask` (the `container run` exec, force-delete on error, `CaptureDiff`, approval gate, `Push`) unchanged. Ensure `fmt` is imported.

- [ ] **Step 5: Delete the obsolete broker test**

Replace `internal/broker/broker_test.go` with a build-only placeholder (the unit-testable logic moved to gateway/creds):
```go
package broker

// HandleTask is exercised by the host-integration end-to-end test (Task 10);
// its pure helpers now live in the gateway and creds packages.
```

- [ ] **Step 6: Run tests + build**

Run: `go build ./... && go test ./internal/runner/ ./internal/broker/`
Expected: build OK; runner PASS; broker has no tests (OK).

- [ ] **Step 7: Commit**

```bash
git add internal/runner/ internal/broker/
git commit -m "refactor(runner,broker): inject grant env + proxy env, drop empty-network hack"
```

---

### Task 7: netfw — squid allowlist compiler + gateway IP

**Files:**
- Create: `internal/netfw/netfw.go`, `internal/netfw/netfw_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/netfw/netfw_test.go`:
```go
package netfw

import (
	"strings"
	"testing"

	"macagent/internal/egress"
)

func cfg() egress.Config {
	var c egress.Config
	c.Default.Domains = []egress.Domain{
		{Host: "api.anthropic.com", Ports: []int{443}},
		{Host: "registry.npmjs.org", Ports: []int{443}},
		{Host: "pypi.org", Ports: []int{443}},
	}
	return c
}

func TestCompileSquidAllowlist_ExcludesModelAPI(t *testing.T) {
	out := CompileSquidAllowlist(cfg())
	if strings.Contains(out, "anthropic.com") {
		t.Errorf("model API must go via the gateway, not squid:\n%s", out)
	}
	if !strings.Contains(out, "registry.npmjs.org") || !strings.Contains(out, "pypi.org") {
		t.Errorf("registries missing from squid allowlist:\n%s", out)
	}
}

func TestGatewayIP(t *testing.T) {
	got, err := GatewayIP("192.168.64.0/24")
	if err != nil || got != "192.168.64.1" {
		t.Fatalf("GatewayIP = %q err=%v, want 192.168.64.1", got, err)
	}
	if _, err := GatewayIP("not-a-cidr"); err == nil {
		t.Errorf("want error for bad CIDR")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/netfw/`
Expected: FAIL — `undefined: CompileSquidAllowlist`, `undefined: GatewayIP`.

- [ ] **Step 3: Write the implementation**

Create `internal/netfw/netfw.go`:
```go
// Package netfw compiles egress config into a userspace squid allowlist and
// derives the stable network's gateway IP. No pf, no per-task network churn.
package netfw

import (
	"fmt"
	"net"
	"strings"

	"macagent/internal/egress"
)

// modelAPIHost is reached via the credential gateway, not squid.
const modelAPIHost = "api.anthropic.com"

// CompileSquidAllowlist renders one dstdomain per allowed host, excluding the
// model API (which the gateway handles). Consumed as a squid "dstdomain" file.
func CompileSquidAllowlist(cfg egress.Config) string {
	var b strings.Builder
	for _, d := range cfg.Default.Domains {
		if d.Host == modelAPIHost {
			continue
		}
		fmt.Fprintf(&b, "%s\n", d.Host)
	}
	return b.String()
}

// GatewayIP returns the .1 host address of the given CIDR (the vmnet gateway).
func GatewayIP(cidr string) (string, error) {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("netfw: bad subnet %q: %w", cidr, err)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return "", fmt.Errorf("netfw: not an IPv4 subnet: %q", cidr)
	}
	ip4[3] = 1
	return ip4.String(), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/netfw/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/netfw/
git commit -m "feat(netfw): squid allowlist compiler + gateway IP derivation"
```

---

### Task 8: Image — nft pin + entrypoint

**Files:**
- Modify: `image/init-firewall.sh`, `image/entrypoint.sh`

**[host integration for the smoke test; the file edits themselves are unit-checkable with `bash -n`.]**

- [ ] **Step 1: Rewrite the firewall as a pin to the gateway IP**

Replace `image/init-firewall.sh`:
```bash
#!/usr/bin/env bash
# nft default-deny output; allow egress ONLY to the host gateway IP on the
# gateway+proxy ports. No DNS, nothing else. Run as ROOT pre-priv-drop.
set -euo pipefail
GW="${1:?usage: init-firewall.sh <gateway-ip> <port> [<port>...]}"
shift
nft flush ruleset
nft add table inet fw
nft add chain inet fw out '{ type filter hook output priority 0; policy drop; }'
nft add rule inet fw out ct state established,related accept
nft add rule inet fw out oifname "lo" accept
for port in "$@"; do
  nft add rule inet fw out ip daddr "$GW" tcp dport "$port" accept
done
```

- [ ] **Step 2: Update the entrypoint to call the pin**

Replace `image/entrypoint.sh`:
```bash
#!/usr/bin/env bash
# Root installs the egress pin (only the host gateway:8088 and :3128), then
# drops privileges to run Claude. The non-root agent cannot flush nft.
set -euo pipefail
/usr/local/bin/init-firewall.sh "${MACAGENT_GW_IP:?missing gateway ip}" 8088 3128
cd /work
exec gosu agent claude --bare -p "$(cat /work/.task/prompt.txt)" \
     --dangerously-skip-permissions \
     --output-format stream-json --verbose --include-partial-messages
```

- [ ] **Step 3: Syntax check, rebuild image, smoke-test the pin [host integration]**

Run:
```bash
bash -n image/init-firewall.sh && bash -n image/entrypoint.sh && echo "syntax OK"
cd image && container build -t claude-sandbox:latest . && cd ..
# Smoke: with the pin to a (here unreachable) gw, only that ip:port is allowed; all else blocked.
container run --rm --user root --entrypoint /bin/bash --cap-add CAP_NET_ADMIN \
  claude-sandbox:latest -lc '
    set +e
    /usr/local/bin/init-firewall.sh 192.168.64.1 8088 3128; echo "pin-exit:$?"
    curl -sS -m 5 https://example.com/ -o /dev/null -w "example:%{http_code}\n" || echo "example:BLOCKED"
    curl -sS -m 5 https://api.anthropic.com/ -o /dev/null -w "anthropic-direct:%{http_code}\n" || echo "anthropic-direct:BLOCKED"
  '
```
Expected: `pin-exit:0`, `example:BLOCKED`, `anthropic-direct:BLOCKED` — the VM can no longer reach the internet directly (it must go through the host gateway/proxy). DNS is also gone, so name resolution itself fails closed.

- [ ] **Step 4: Commit**

```bash
git add image/
git commit -m "feat(image): pin VM egress to host gateway+proxy ports, drop DNS"
```

---

### Task 9: Wire brokerd — start gateway + squid, pick provider

**Files:**
- Modify: `cmd/brokerd/main.go`

- [ ] **Step 1: Update main to start the gateway and select the provider**

Replace `cmd/brokerd/main.go`:
```go
package main

import (
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"macagent/internal/broker"
	"macagent/internal/creds"
	"macagent/internal/egress"
	"macagent/internal/gateway"
)

func main() {
	cfg, err := egress.Load(env("EGRESS_CONFIG", "config/egress.yaml"))
	if err != nil {
		log.Fatalf("load egress config: %v", err)
	}
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		log.Fatal("ANTHROPIC_API_KEY must be set on the broker host")
	}

	gwIP := env("MACAGENT_GW_IP", "192.168.64.1")
	network := env("MACAGENT_NETWORK", "macagent-egress")
	proxyPort := 3128
	gwPort := 8088
	budget := envFloat("MACAGENT_TASK_BUDGET_USD", 2.0)
	taskTimeout := 30 * time.Minute

	// Credential gateway: real key host-only; VM gets a bearer token.
	gw, err := gateway.New(apiKey, "https://api.anthropic.com", gateway.DefaultPrices())
	if err != nil {
		log.Fatalf("gateway: %v", err)
	}
	go func() {
		addr := net.JoinHostPort(gwIP, strconv.Itoa(gwPort))
		log.Printf("gateway listening on %s", addr)
		log.Fatal(http.ListenAndServe(addr, gw))
	}()

	var provider creds.Provider = &gateway.Provider{
		GW:      gw,
		BaseURL: "http://" + net.JoinHostPort(gwIP, strconv.Itoa(gwPort)),
		Budget:  budget,
		TTL:     taskTimeout + 5*time.Minute,
	}

	b := &broker.Broker{
		Cfg:        cfg,
		Creds:      provider,
		Approve:    func(kind string, _ any) bool { log.Printf("approval gate: %s -> auto-approve (MVP)", kind); return true },
		ImageRef:   env("SANDBOX_IMAGE", "claude-sandbox:latest"),
		StageRoot:  env("STAGE_ROOT", "/tmp/broker/stage"),
		AuditRoot:  env("AUDIT_ROOT", "/tmp/broker/audit"),
		Timeout:    taskTimeout,
		Network:    network,
		GatewayIP:  gwIP,
		ProxyPort:  proxyPort,
		TaskBudget: budget,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /tasks", b.HandleTask)

	addr := env("BROKER_ADDR", "127.0.0.1:8765")
	log.Printf("brokerd listening on %s (gateway %s:%d, squid expected on %s:%d)", addr, gwIP, gwPort, gwIP, proxyPort)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
```

> Note: squid is started out-of-band for this pass (Task 10 step). A later refactor can have `netfw` spawn/manage squid from `brokerd`; the MVP wiring assumes squid is already listening on `gwIP:3128` with the compiled allowlist.

- [ ] **Step 2: Build, vet, test the whole module**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: build/vet clean; all unit tests PASS (gateway, creds, netfw, runner); broker/cmd have no tests.

- [ ] **Step 3: Commit**

```bash
git add cmd/brokerd/
git commit -m "feat(brokerd): start credential gateway, select gateway provider"
```

---

### Task 10: Host integration — squid + end-to-end (needs secrets)

**Files:** none (verification). **Requires** the dev host, a real `ANTHROPIC_API_KEY`, `gh` auth, a throwaway repo, and Task 1 = PASS.

- [ ] **Step 1: Start a userspace squid with the compiled allowlist**

Run:
```bash
brew install squid 2>&1 | tail -2 || true
# Compile the allowlist from config (a tiny Go helper or by hand for the smoke):
printf 'registry.npmjs.org\n.npmjs.org\npypi.org\nfiles.pythonhosted.org\n' > /tmp/squid-allow.txt
cat > /tmp/squid.conf <<'CONF'
http_port 192.168.64.1:3128
acl allowed dstdomain "/tmp/squid-allow.txt"
acl SSL_ports port 443
acl CONNECT method CONNECT
http_access deny CONNECT !SSL_ports
http_access deny CONNECT !allowed
http_access allow CONNECT allowed SSL_ports
http_access allow allowed
http_access deny all
dns_nameservers 1.1.1.1 8.8.8.8
cache deny all
CONF
/opt/homebrew/opt/squid/sbin/squid -N -f /tmp/squid.conf &
```
Expected: squid is listening on `192.168.64.1:3128` (`curl -x http://192.168.64.1:3128 -sS https://registry.npmjs.org/ -o /dev/null -w '%{http_code}\n'` from the host returns a code; a non-allowlisted host returns 403).

- [ ] **Step 2: Run brokerd and submit a task**

Run:
```bash
export ANTHROPIC_API_KEY=sk-...   # workspace key with a spend cap
go run ./cmd/brokerd &
curl -sS -X POST http://127.0.0.1:8765/tasks -H 'content-type: application/json' \
  -d '{"repo_ref":"https://github.com/<you>/<throwaway>.git","instruction":"Add a HELLO line to README.md"}' | jq .
```
Expected: `{"task_id":"…","branch":"agent/…","pushed":true}`. The claude run inside the VM reached the model **through the gateway** (the VM had no real key and no direct internet).

- [ ] **Step 3: Prove the boundary holds**

Run (inside a VM on the network, as in Task 8 but using the real gateway):
```bash
container run --rm --user root --entrypoint /bin/bash --cap-add CAP_NET_ADMIN --network macagent-egress \
  --env MACAGENT_GW_IP=192.168.64.1 claude-sandbox:latest -lc '
    set +e
    /usr/local/bin/init-firewall.sh 192.168.64.1 8088 3128
    curl -sS -m 6 https://api.anthropic.com/ -o /dev/null -w "direct-anthropic:%{http_code}\n" || echo "direct-anthropic:BLOCKED"
    curl -sS -m 6 http://192.168.64.1:8088/v1/messages -o /dev/null -w "via-gateway:%{http_code}\n" || echo "via-gateway:FAIL"
  '
```
Expected: `direct-anthropic:BLOCKED` (VM cannot reach Anthropic directly) and `via-gateway:` returns an HTTP code (401/400 without a valid token is fine — it proves reachability). Document the results.

- [ ] **Step 4: Verify budget + revoke**

Confirm via the audit log / a scripted token that (a) a second request after budget exhaustion returns 402, and (b) after the task completes, the task's token is rejected (gateway 401). Record the observations.

---

## Self-Review

**Spec coverage:**
- §1 real key host-only + bearer token — gateway (Task 3) + provider (Task 5) + brokerd wiring (Task 9). ✓
- §3 revoke + budget, no rotation — `Revoke`, budget-gated `check` (Task 3); `grant.Revoke` on task end (Task 6). ✓
- §4 gateway reverse proxy w/ X-Api-Key swap + usage metering — Task 3. ✓
- §4 squid CONNECT hostname allowlist — config in Task 10; allowlist compiler Task 7. ✓
- §4 in-VM nft pin to two ports, DNS dropped — Task 8. ✓
- §5.2 creds Grant/EnvVars — Task 4. ✓
- §5.3 netfw squid allowlist + gateway IP — Task 7. ✓
- §6 networking spike + fallback — Task 1. ✓
- §7 tests (gateway/creds/netfw unit, host integration) — Tasks 2,3,5,7,8,10. ✓
- **Deferred within this subsystem (noted, not silently dropped):** `netfw` spawning/managing squid from `brokerd` (Task 9 note) — squid is started out-of-band in Task 10; folding it into the broker lifecycle is a follow-up.

**Placeholder scan:** no TBD/TODO; every code step has complete code; host-integration steps give exact commands + expected output. ✓

**Type consistency:** `gateway.New(realKey, upstream, prices)`, `Gateway.Mint(budget, ttl)`, `Gateway.Revoke`, `Gateway.ServeHTTP`, `Price{InputPer1M,OutputPer1M}`, `cost`, `parseUsage`, `gateway.Provider{GW,BaseURL,Budget,TTL}`, `creds.Grant{EnvVars,Revoke}`, `creds.Provider.Mint(budgetUSD)`, `runner.Spec.Env`, `broker.Broker{Network,GatewayIP,ProxyPort,TaskBudget}`, `netfw.CompileSquidAllowlist`, `netfw.GatewayIP` — used consistently across tasks. ✓

---

## Open risks (carried from spec §8, verify during execution)

1. **vmnet gateway IP bind/reachability** — gated by Task 1; fallback to a LAN/loopback bind there.
2. **`HTTPS_PROXY` honored by `npm`/`pip`** — raw-socket tools fail closed (nft); configure each tool's proxy explicitly in the image if a registry op is needed.
3. **SSE usage shape** — `parseUsage` assumes `message_start`/`message_delta` carry `usage`; confirm against a real claude-code stream in Task 10 and adjust the fixture if the field path differs.
4. **Budget request-gated** — a single long response can overshoot by one request (accepted, spec §5.1).
5. **squid lifecycle** — started out-of-band this pass; fold into `brokerd`/`netfw` later.
