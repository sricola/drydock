# Codex (ChatGPT subscription) Auth — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator run drydock Codex tasks on a ChatGPT subscription with no `OPENAI_API_KEY`, while the real OAuth credential (access + refresh token + account id) never enters the sandbox VM.

**Architecture:** Host-side only. The VM's Codex CLI runs in API-key mode pointed at the gateway with a minted `tok_` bearer (unchanged). The gateway injects `Authorization: Bearer <oauth>` + `chatgpt-account-id`, rewrites the host to the Codex backend (`chatgpt.com/backend-api/codex`), and preserves the CLI's `originator`/`User-Agent` + `store:false`/`stream:true` body. Reuses the Claude OAuth machinery wholesale — `Credential`, `OAuthCred`, `CredStore`, the request-count cap, `costCell`/`firstMeta`, `agentCredentialAvailable` — adding only an OpenAI vendor, an OpenAI refresh func, an account-id-aware store, and a config knob.

**Tech Stack:** Go; existing `internal/gateway` reverse proxy, `internal/config`, `cmd/brokerd`, `cmd/drydock`, `internal/broker`.

## Global Constraints

- **Spike gate:** Task 1 (validation spike) is a go/no-go gate. Do **not** start Task 2+ until it passes. The cheap `curl` replay already returned HTTP 200 (model `gpt-5.5`); Task 1 codifies it as a test, pins constants, and adds the **post-refresh 200** assertion. If a kill criterion hits, stop and report.
- **A1 invariant:** the real credential (OAuth access token, refresh token, AND `account_id`) must never enter the VM. The VM-facing env stays exactly `OPENAI_BASE_URL` + `OPENAI_API_KEY=<minted bearer>`. No image/entrypoint changes.
- **Scope:** OpenAI/Codex only. The Claude (`anthropic_auth`) path must stay byte-identical — do not touch it.
- **Auth model:** operator-global via `openai_auth: api_key | subscription` (default `api_key`), mirroring `anthropic_auth`.
- **Runaway control for subscription:** the existing request-count cap (`task_max_requests`) + `task_timeout`. `task_budget_usd` is ignored for subscription tasks (never silently imply it caps spend).
- **Reuse, don't fork:** use the existing `Credential`/`OAuthCred`/`CredStore`/request-cap/`costCell`/`firstMeta`/`agentCredentialAvailable`. Do not duplicate them.
- **`account_id` capture-once:** read `account_id` from the original `~/.codex/auth.json` (a plain `tokens.account_id` UUID) at bootstrap; persist it in the drydock store; inject it on every request independent of token rotation. It is **not** threaded through `CredSnapshot` (the shared Claude/Codex token-pair type stays as-is). This inherently mitigates the documented "refresh strips `chatgpt_account_id`" failure mode.
- **Never log secrets:** never print/log the access token, `id_token`, or refresh token, nor the **decoded JWT claims** (they carry `account_id`/plan/org). `account_id` is not a bearer secret but is not logged needlessly.
- **Credential storage:** drydock-owned copy at `~/.drydock/codex-oauth.json`, mode `0600`.
- **Honesty (docs):** no overclaiming. State the bigger blast radius and the ToS/rate-limit caveat (headless use of a ChatGPT subscription may brush OpenAI's terms and hit limits sooner; the operator assumes that risk; drydock makes no claim the mode is sanctioned).
- **Pinned constants** (`openaiOAuthClientID`, `openaiOAuthTokenURL`, the Codex backend base URL + base path) are **confirmed by Task 1** and live in `internal/gateway/codex_constants.go`. Later tasks reference them by name, never by guessed literal.

### Confirmed by the cheap feasibility replay (Task 1 re-confirms in test form)

- Backend: `https://chatgpt.com/backend-api/codex` (the `/responses` endpoint served there).
- Required headers: `Authorization: Bearer <access_token>`, `chatgpt-account-id: <uuid>`, `originator: codex_cli_rs`, a `codex_cli_rs/...` `User-Agent`. The originator/UA come from the VM's real Codex CLI and pass through; the gateway must not clobber them.
- Body: standard Responses API with `store:false`, `stream:true` (the real Codex CLI already sends these → no body surgery expected).
- Response: standard Responses SSE (`response.output_text.delta` → `response.completed`, with a `usage` object).
- Refresh: `https://auth.openai.com/oauth/token`, client_id `app_EMoamEEZ73f0CkXaXp7hrann`.
- `~/.codex/auth.json`: `auth_mode: "chatgpt"`, `OPENAI_API_KEY: null`, `tokens.{access_token (JWT), id_token (JWT), refresh_token, account_id (UUID)}`. No explicit expiry field → expiry comes from the access-token JWT `exp` claim.

## File Structure

| File | Responsibility |
|---|---|
| `internal/gateway/codex_constants.go` (new) | Pinned, Task-1-confirmed OpenAI/Codex constants (client_id, token URL, backend base URL + base path). |
| `internal/gateway/vendor.go` (modify) | `OpenAIOAuthVendor(accountID string)`; `Vendor.BasePath` field. |
| `internal/gateway/gateway.go` (modify) | Director path remap when `Vendor.BasePath != ""` (`codexPath`/`singleJoiningSlash`). |
| `internal/gateway/codex_oauth.go` (new) | `refreshOpenAI`; `CodexStore` (account-id-aware, 0600); `NewOAuthCredCodex`. |
| `internal/config/config.go` (modify) | `OpenAIAuth` + env override + validate + seed template. |
| `cmd/drydock/auth.go` (modify) | `drydock auth codex` — parse `~/.codex/auth.json`, derive expiry from JWT `exp`, store with account id. |
| `cmd/drydock/main.go` (modify) | (only if needed) `auth` usage text mentions `codex`. |
| `internal/broker/broker.go` (modify) | `Broker.OpenAIAuth`; extend the `drydock_meta` line to the openai vendor. |
| `cmd/brokerd/main.go` (modify) | Build the OpenAI backend per `openai_auth`; boot guard; subscription budget; set `Broker.OpenAIAuth`. |
| `cmd/drydock/start.go` (modify) | `agentCredentialAvailable` also accepts a Codex subscription. |
| `cmd/drydock/doctor.go` (modify) | `codex subscription → token valid` step. |
| `tests/integration/codex_spike_test.go` (new) | Task 1 opt-in validation spike. |
| `tests/integration/redteam_test.go` (modify) | A1 extended: Codex OAuth tokens + account_id never in VM env. |
| `THREAT_MODEL.md`, `SECURITY.md`, `README.md` (modify) | Subscription delta, blast-radius note, install path. |

---

### Task 1: Validation spike (GO/NO-GO GATE)

Research/validation, not red-green TDD. Codifies the already-passed `curl` replay as an opt-in Go test, pins the constants every later task depends on, and adds the post-refresh assertion. **Requires a real `codex login` (ChatGPT plan) on this host** (`~/.codex/auth.json`, `auth_mode: "chatgpt"`).

**Files:**
- Create: `tests/integration/codex_spike_test.go` (build tag `integration`)
- Create (output of this task): `internal/gateway/codex_constants.go`

**Interfaces:**
- Produces: `openaiOAuthClientID`, `openaiOAuthTokenURL`, `codexBackendBaseURL`, `codexBackendBasePath` constants; the confirmed inbound request path the VM's Codex emits; confirmation that `store:false`/`stream:true`/`originator` are sent natively.

- [ ] **Step 1: Read the host Codex credentials (values redacted in any output).** Read `~/.codex/auth.json`. Record the shape: `tokens.access_token` (JWT), `tokens.refresh_token`, `tokens.account_id` (UUID), `auth_mode`. Decode ONLY the access-token JWT `exp` claim to confirm an expiry is derivable. Never print token or claim values.

- [ ] **Step 2: Write the access probe** in `codex_spike_test.go` (skipped unless `DRYDOCK_CODEX_SPIKE=1`). It reads the access token + account_id, POSTs a minimal Responses request to `https://chatgpt.com/backend-api/codex/responses` with the confirmed header set + `store:false`/`stream:true`, and asserts a 200 with a streamed completion. Tokens are read into locals and never logged; no `httputil.DumpRequest` of auth headers.

```go
//go:build integration

package integration

// Run with: DRYDOCK_CODEX_SPIKE=1 go test -tags=integration -run TestCodexSpike ./tests/integration/ -v
// Requires `codex login` (ChatGPT plan) on this host.
func TestCodexSpike_SubscriptionAccessThroughBearer(t *testing.T) {
	if os.Getenv("DRYDOCK_CODEX_SPIKE") != "1" {
		t.Skip("set DRYDOCK_CODEX_SPIKE=1 (needs a real ChatGPT/codex login)")
	}
	access, _, account := readCodexCreds(t) // from ~/.codex/auth.json; refresh unused here
	resp := postCodex(t, codexBackendBaseURL+codexBackendBasePath+"/responses", access, account,
		`{"model":"gpt-5.5","instructions":"You are terse.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Reply with one word: hello"}]}],"store":false,"stream":true}`)
	if resp.StatusCode != 200 {
		t.Fatalf("KILL CRITERION: Codex subscription access denied to a gateway-shaped request: %d", resp.StatusCode)
	}
	body := readAll(t, resp.Body) // SSE; model output, not the credential
	if !strings.Contains(body, "response.completed") {
		t.Fatalf("KILL CRITERION: no completion event in stream")
	}
}
```

(`readCodexCreds`/`postCodex` are test-local helpers; `postCodex` sets `Authorization: Bearer`, `chatgpt-account-id`, `originator: codex_cli_rs`, `User-Agent: codex_cli_rs/0.141.0`, `OpenAI-Beta: responses=experimental`, `Content-Type: application/json`.)

- [ ] **Step 3: Run the access probe.** `DRYDOCK_CODEX_SPIKE=1 go test -tags=integration -run TestCodexSpike_SubscriptionAccess ./tests/integration/ -v`. Expected: PASS (200 + `response.completed`). **KILL CRITERION:** a 403 on `originator`/`User-Agent` we can't reproduce host-side, or a body rejection a proxy can't coerce → STOP and report.

- [ ] **Step 4: Write + run the refresh probe (the account_id guard).** Add a test that POSTs the refresh grant to `https://auth.openai.com/oauth/token` (`grant_type=refresh_token`, `client_id=openaiOAuthClientID`, `refresh_token`), then repeats the Step-2 request with the **new** access token AND the **original** captured `account_id`, asserting 200. Run it. Expected: PASS. **KILL CRITERION:** a request that 200s pre-refresh but 401/403s post-refresh with the captured account_id, and no host-side mitigation → STOP. (Validated-by-typed-error is acceptable only for the token POST if rate-limited, never for the post-refresh request.)

- [ ] **Step 5: Confirm the inbound path the VM's Codex emits.** Determine what path Codex CLI sends when `OPENAI_BASE_URL` points at a bare host (the gateway): `/responses` vs `/v1/responses`. Record it — Task 2's path remap depends on it. (Capture from a real `codex` run's trace, or from the `OPENAI_BASE_URL` join behavior.)

- [ ] **Step 6: Record the constants.** Create `internal/gateway/codex_constants.go`:

```go
package gateway

const (
	openaiOAuthClientID  = "app_EMoamEEZ73f0CkXaXp7hrann" // confirmed Task 1
	codexBackendBaseURL  = "https://chatgpt.com"          // host only; path via BasePath
	codexBackendBasePath = "/backend-api/codex"           // confirmed Task 1
)

// openaiOAuthTokenURL is a var (not const) so tests can point refreshOpenAI at
// an httptest server, mirroring anthropicOAuthTokenURL.
var openaiOAuthTokenURL = "https://auth.openai.com/oauth/token" // confirmed Task 1
```

- [ ] **Step 7: Commit.**

```bash
git add tests/integration/codex_spike_test.go internal/gateway/codex_constants.go
git commit -m "spike: validate Codex ChatGPT-subscription access + refresh through the gateway"
```

---

### Task 2: `OpenAIOAuthVendor` + director path remap

**Depends on Task 1 constants.**

**Files:**
- Modify: `internal/gateway/vendor.go`
- Modify: `internal/gateway/gateway.go`
- Test: `internal/gateway/vendor_test.go`, `internal/gateway/gateway_test.go`

**Interfaces:**
- Produces: `func OpenAIOAuthVendor(accountID string) Vendor` — `Inject` sets `Authorization: Bearer <secret>` + `chatgpt-account-id: <accountID>`, removes `X-Api-Key`, and does **not** touch `originator`/`User-Agent`; `BaseURL = codexBackendBaseURL`, `BasePath = codexBackendBasePath`. New field `Vendor.BasePath string`. Helper `singleJoiningSlash`.
- Consumes: `OpenAIVendor()`, `codexBackendBaseURL`, `codexBackendBasePath` (Task 1).

- [ ] **Step 1: Write the failing tests.** In `vendor_test.go`:

```go
func TestOpenAIOAuthVendor_Inject(t *testing.T) {
	r, _ := http.NewRequest("POST", "https://chatgpt.com/backend-api/codex/responses", nil)
	r.Header.Set("X-Api-Key", "leftover")
	r.Header.Set("originator", "codex_cli_rs")
	r.Header.Set("User-Agent", "codex_cli_rs/0.141.0")
	OpenAIOAuthVendor("acc-123").Inject(r, "oauth-access-xyz")
	if r.Header.Get("X-Api-Key") != "" {
		t.Error("X-Api-Key not removed")
	}
	if r.Header.Get("Authorization") != "Bearer oauth-access-xyz" {
		t.Errorf("Authorization=%q", r.Header.Get("Authorization"))
	}
	if r.Header.Get("chatgpt-account-id") != "acc-123" {
		t.Errorf("account id=%q", r.Header.Get("chatgpt-account-id"))
	}
	if r.Header.Get("originator") != "codex_cli_rs" || r.Header.Get("User-Agent") != "codex_cli_rs/0.141.0" {
		t.Error("originator/User-Agent must be preserved (403 risk)")
	}
}
```

In `gateway_test.go`:

```go
func TestSingleJoiningSlash(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"/backend-api/codex", "/responses", "/backend-api/codex/responses"},
		{"/backend-api/codex", "responses", "/backend-api/codex/responses"},
		{"/backend-api/codex/", "/responses", "/backend-api/codex/responses"},
		{"", "/v1/messages", "/v1/messages"}, // non-codex vendors unaffected
	}
	for _, c := range cases {
		if got := singleJoiningSlash(c.a, c.b); got != c.want {
			t.Errorf("singleJoiningSlash(%q,%q)=%q want %q", c.a, c.b, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run — fails** (`OpenAIOAuthVendor`/`Vendor.BasePath`/`singleJoiningSlash` undefined). `go test ./internal/gateway/ -run 'OpenAIOAuthVendor|SingleJoiningSlash'`.

- [ ] **Step 3: Implement the vendor + field** in `vendor.go`. Add `BasePath string` to the `Vendor` struct (document: "non-empty only for the Codex subscription backend; the director joins it onto the inbound path"). Add:

```go
// OpenAIOAuthVendor is the ChatGPT-subscription Codex backend: Bearer OAuth +
// chatgpt-account-id, served at chatgpt.com/backend-api/codex. accountID is
// captured once at bootstrap and is constant across token refreshes. The VM's
// real Codex CLI supplies originator/User-Agent and store:false/stream:true;
// this vendor must not disturb them.
func OpenAIOAuthVendor(accountID string) Vendor {
	v := OpenAIVendor()
	v.BaseURL = codexBackendBaseURL
	v.BasePath = codexBackendBasePath
	v.Inject = func(r *http.Request, secret string) {
		r.Header.Del("X-Api-Key")
		r.Header.Set("Authorization", "Bearer "+secret)
		r.Header.Set("chatgpt-account-id", accountID)
	}
	return v
}
```

- [ ] **Step 4: Implement the director remap** in `gateway.go`. In `director`, after setting Host, remap the path only when `BasePath` is set (so all existing vendors are untouched):

```go
req.URL.Scheme = vt.upstream.Scheme
req.URL.Host = vt.upstream.Host
req.Host = vt.upstream.Host
if vt.v.BasePath != "" {
	// The VM's Codex posts to {gateway}/responses (api-key mode); the Codex
	// subscription backend serves it under /backend-api/codex. Tolerate an
	// optional /v1 prefix so either OPENAI_BASE_URL form maps correctly.
	req.URL.Path = singleJoiningSlash(vt.v.BasePath, strings.TrimPrefix(req.URL.Path, "/v1"))
}
vt.v.Inject(req, rc.secret)
```

Add the helper:

```go
func singleJoiningSlash(a, b string) string {
	aslash, bslash := strings.HasSuffix(a, "/"), strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		if a == "" {
			return b
		}
		return a + "/" + b
	}
	return a + b
}
```

(If Task 1 Step 5 found the inbound path is already `/responses` with no `/v1`, the `TrimPrefix` is a harmless no-op. If it found `/v1/responses`, the strip makes it correct.)

- [ ] **Step 5: Run — passes.** `go test ./internal/gateway/`. `go build ./...`.

- [ ] **Step 6: Commit.** `git add internal/gateway && git commit -m "gateway: OpenAIOAuthVendor (Bearer + chatgpt-account-id) + BasePath path remap"`

---

### Task 3: `refreshOpenAI` + account-id-aware `CodexStore`

**Depends on Task 1 constants.**

**Files:**
- Create: `internal/gateway/codex_oauth.go`
- Test: `internal/gateway/codex_oauth_test.go`

**Interfaces:**
- Produces: `func refreshOpenAI(refreshToken string) (CredSnapshot, error)`; `type CodexStore struct{...}` implementing `CredStore` plus `AccountID() string` and `Put(snap CredSnapshot, accountID string) error`; `func NewCodexStore(path string) *CodexStore`; `func NewOAuthCredCodex(snap CredSnapshot, store CredStore) *OAuthCred`.
- Consumes: `CredSnapshot`/`CredStore`/`OAuthCred` (existing), `openaiOAuthClientID`/`openaiOAuthTokenURL` (Task 1).

- [ ] **Step 1: Write the failing tests** in `codex_oauth_test.go`:

```go
func TestCodexStore_RoundTripPreservesAccountID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex-oauth.json")
	s := NewCodexStore(path)
	if err := s.Put(CredSnapshot{Access: "a1", Refresh: "r1", Expiry: time.Now().Add(time.Hour)}, "acct-uuid"); err != nil {
		t.Fatal(err)
	}
	// A fresh store Loads the snapshot AND learns the account id.
	s2 := NewCodexStore(path)
	snap, err := s2.Load()
	if err != nil || snap.Access != "a1" {
		t.Fatalf("Load=%+v,%v", snap, err)
	}
	if s2.AccountID() != "acct-uuid" {
		t.Errorf("AccountID=%q want acct-uuid", s2.AccountID())
	}
	// A refresh-driven Save (no account id passed) must NOT drop it.
	if err := s2.Save(CredSnapshot{Access: "a2", Refresh: "r2", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	s3 := NewCodexStore(path)
	_, _ = s3.Load()
	if s3.AccountID() != "acct-uuid" {
		t.Errorf("account id lost on refresh Save: %q", s3.AccountID())
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode=%v want 0600", fi.Mode().Perm())
	}
}

func TestRefreshOpenAI_ParsesAndDoesNotLeak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["grant_type"] != "refresh_token" || body["client_id"] != openaiOAuthClientID || body["refresh_token"] != "r1" {
			t.Errorf("bad refresh body: %+v", body)
		}
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"r2","expires_in":3600}`))
	}))
	defer srv.Close()
	old := openaiOAuthTokenURL
	openaiOAuthTokenURL = srv.URL
	defer func() { openaiOAuthTokenURL = old }()

	snap, err := refreshOpenAI("r1")
	if err != nil || snap.Access != "new-access" || snap.Refresh != "r2" {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
	if time.Until(snap.Expiry) < 50*time.Minute {
		t.Errorf("expiry too soon: %v", snap.Expiry)
	}
}

func TestRefreshOpenAI_NonOKErrorHasNoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"rate_limited"}`))
	}))
	defer srv.Close()
	old := openaiOAuthTokenURL
	openaiOAuthTokenURL = srv.URL
	defer func() { openaiOAuthTokenURL = old }()

	_, err := refreshOpenAI("super-secret-refresh")
	if err == nil {
		t.Fatal("want error on 429")
	}
	if strings.Contains(err.Error(), "super-secret-refresh") {
		t.Error("refresh token leaked into error")
	}
}
```

- [ ] **Step 2: Run — fails** (types undefined). `go test ./internal/gateway/ -run 'CodexStore|RefreshOpenAI'`.

- [ ] **Step 3: Implement** `internal/gateway/codex_oauth.go`:

```go
package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// codexFile is the on-disk shape of ~/.drydock/codex-oauth.json: the OAuth
// token pair + the captured ChatGPT account id (constant across refreshes).
type codexFile struct {
	Access    string    `json:"access_token"`
	Refresh   string    `json:"refresh_token"`
	Expiry    time.Time `json:"expiry"`
	AccountID string    `json:"account_id"`
}

// CodexStore is a CredStore that also retains the ChatGPT account id. Save
// (called by OAuthCred on refresh rotation) preserves the account id captured
// at bootstrap, guarding the documented "refresh strips account id" failure.
type CodexStore struct {
	path      string
	accountID string
}

func NewCodexStore(path string) *CodexStore { return &CodexStore{path: path} }

func (s *CodexStore) Load() (CredSnapshot, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return CredSnapshot{}, err
	}
	var f codexFile
	if err := json.Unmarshal(data, &f); err != nil {
		return CredSnapshot{}, err
	}
	s.accountID = f.AccountID
	return CredSnapshot{Access: f.Access, Refresh: f.Refresh, Expiry: f.Expiry}, nil
}

func (s *CodexStore) Save(snap CredSnapshot) error {
	data, err := json.Marshal(codexFile{Access: snap.Access, Refresh: snap.Refresh, Expiry: snap.Expiry, AccountID: s.accountID})
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Put writes the initial credential file including the account id. Used by
// `drydock auth codex`; later refreshes persist via Save, preserving it.
func (s *CodexStore) Put(snap CredSnapshot, accountID string) error {
	s.accountID = accountID
	return s.Save(snap)
}

func (s *CodexStore) AccountID() string { return s.accountID }

// NewOAuthCredCodex is OAuthCred wired to the OpenAI refresh grant.
func NewOAuthCredCodex(snap CredSnapshot, store CredStore) *OAuthCred {
	return &OAuthCred{snap: snap, store: store, refresh: refreshOpenAI}
}

// refreshOpenAI exchanges a refresh token for a new CredSnapshot via the OpenAI
// OAuth token endpoint. Shape confirmed in Task 1. Never interpolates the token
// into errors.
func refreshOpenAI(refreshToken string) (CredSnapshot, error) {
	body, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     openaiOAuthClientID,
	})
	if err != nil {
		return CredSnapshot{}, err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(openaiOAuthTokenURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return CredSnapshot{}, fmt.Errorf("oauth: codex token request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return CredSnapshot{}, fmt.Errorf("oauth: codex token endpoint returned %d", resp.StatusCode)
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return CredSnapshot{}, fmt.Errorf("oauth: decode codex token response: %w", err)
	}
	if result.AccessToken == "" {
		return CredSnapshot{}, fmt.Errorf("oauth: codex refresh response had no access_token")
	}
	return CredSnapshot{
		Access:  result.AccessToken,
		Refresh: result.RefreshToken,
		Expiry:  time.Now().Add(time.Duration(result.ExpiresIn) * time.Second),
	}, nil
}
```

(If Task 1 found the token endpoint wants form-encoding rather than JSON, adjust `Post` accordingly — pin to whatever Task 1 confirmed.)

- [ ] **Step 4: Run — passes.** `go test ./internal/gateway/ -run 'CodexStore|RefreshOpenAI'`.

- [ ] **Step 5: Commit.** `git add internal/gateway && git commit -m "gateway: refreshOpenAI + account-id-aware CodexStore + NewOAuthCredCodex"`

---

### Task 4: Config — `openai_auth`

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go` (add)

**Interfaces:**
- Produces: `Config.OpenAIAuth string` (yaml `openai_auth`, default `"api_key"`); env override `DRYDOCK_OPENAI_AUTH`; `validate()` rejects values not in {`api_key`,`subscription`}.

- [ ] **Step 1: Write the failing test** in `config_test.go`:

```go
func TestConfig_OpenAIAuth(t *testing.T) {
	t.Setenv("DRYDOCK_OPENAI_AUTH", "subscription")
	c, err := Load("/nonexistent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.OpenAIAuth != "subscription" {
		t.Errorf("OpenAIAuth=%q", c.OpenAIAuth)
	}
}

func TestConfig_OpenAIAuthDefaultsToApiKey(t *testing.T) {
	c, _ := Load("/nonexistent.yaml")
	if c.OpenAIAuth != "api_key" {
		t.Errorf("default OpenAIAuth=%q, want api_key", c.OpenAIAuth)
	}
}

func TestConfig_OpenAIAuthRejectsGarbage(t *testing.T) {
	t.Setenv("DRYDOCK_OPENAI_AUTH", "bogus")
	if _, err := Load("/nonexistent.yaml"); err == nil {
		t.Error("want validate error for openai_auth=bogus")
	}
}
```

- [ ] **Step 2: Run — fails** (field undefined). `go test ./internal/config/ -run TestConfig_OpenAIAuth`.

- [ ] **Step 3: Implement**, mirroring `AnthropicAuth` exactly:
  - Struct field after `AnthropicAuth`: `OpenAIAuth string \`yaml:"openai_auth"\`` with a comment.
  - Defaults constructor: `OpenAIAuth: "api_key",`.
  - Env block (next to `DRYDOCK_ANTHROPIC_AUTH`): `if v := os.Getenv("DRYDOCK_OPENAI_AUTH"); v != "" { c.OpenAIAuth = v }`.
  - `validate()` (after the `anthropic_auth` check): `if c.OpenAIAuth != "api_key" && c.OpenAIAuth != "subscription" { return fmt.Errorf("config: openai_auth must be api_key or subscription, got %q", c.OpenAIAuth) }`.
  - `SeedTemplate` (after the `anthropic_auth:` line): `openai_auth:            api_key        # authentication mode: api_key | subscription`.

- [ ] **Step 4: Run — passes.** `go test ./internal/config/`.

- [ ] **Step 5: Commit.** `git add internal/config && git commit -m "config: openai_auth (api_key | subscription)"`

---

### Task 5: `drydock auth codex` (bootstrap creds)

**Files:**
- Modify: `cmd/drydock/auth.go`
- Modify: `cmd/drydock/main.go` (usage/subHelp mention `codex`, if present)
- Test: `cmd/drydock/auth_test.go` (add)

**Interfaces:**
- Produces: pure helpers `func parseCodexCreds(raw []byte) (gateway.CredSnapshot, string, error)` (returns snapshot + account id) and `func jwtExpiry(token string) (time.Time, error)` (decodes ONLY the `exp` claim); `func runAuthCodex(args []string)`; `auth` dispatch routes `codex`.
- Consumes: `gateway.CredSnapshot`/`gateway.NewCodexStore` (Task 3), `config.Dir()`.

- [ ] **Step 1: Write the failing tests** in `auth_test.go` (build a JWT with a known `exp`):

```go
func makeJWTExp(exp int64) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"chatgpt_account_id":"SHOULD_NOT_BE_LOGGED"}`, exp)))
	return "h." + payload + ".s"
}

func TestParseCodexCreds(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	raw := []byte(fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"access_token":%q,"refresh_token":"r1","account_id":"acc-uuid"}}`, makeJWTExp(exp)))
	snap, account, err := parseCodexCreds(raw)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Refresh != "r1" || account != "acc-uuid" {
		t.Fatalf("snap=%+v account=%q", snap, account)
	}
	if d := snap.Expiry.Unix() - exp; d < -2 || d > 2 {
		t.Errorf("expiry from JWT exp wrong: %v vs %v", snap.Expiry.Unix(), exp)
	}
}

func TestParseCodexCreds_NotLoggedIn(t *testing.T) {
	if _, _, err := parseCodexCreds([]byte(`{"tokens":{}}`)); err == nil {
		t.Error("want error when no access token")
	}
}

func TestJWTExpiry_Malformed(t *testing.T) {
	if _, err := jwtExpiry("not-a-jwt"); err == nil {
		t.Error("want error for non-JWT")
	}
}
```

- [ ] **Step 2: Run — fails** (`parseCodexCreds`/`jwtExpiry` undefined). `go test ./cmd/drydock/ -run 'CodexCreds|JWTExpiry'`.

- [ ] **Step 3: Implement** in `auth.go`. Add the import block entries (`encoding/base64`, `strings`). Add:

```go
// codexAuthFile is the relevant shape of ~/.codex/auth.json (auth_mode
// "chatgpt"). Only these fields are read; the id_token is ignored.
type codexAuthFile struct {
	Tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
}

// jwtExpiry decodes ONLY the exp claim from a JWT. It never returns or logs any
// other claim (the Codex access token's payload carries account_id/plan/org).
func jwtExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("auth: access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("auth: decode JWT payload: %w", err)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("auth: parse JWT claims: %w", err)
	}
	if claims.Exp == 0 {
		return time.Time{}, fmt.Errorf("auth: JWT has no exp claim")
	}
	return time.Unix(claims.Exp, 0), nil
}

// parseCodexCreds maps ~/.codex/auth.json to a CredSnapshot + the ChatGPT
// account id. Expiry comes from the access-token JWT exp claim.
func parseCodexCreds(raw []byte) (gateway.CredSnapshot, string, error) {
	var f codexAuthFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return gateway.CredSnapshot{}, "", fmt.Errorf("auth: parse codex auth.json: %w", err)
	}
	if f.Tokens.AccessToken == "" {
		return gateway.CredSnapshot{}, "", fmt.Errorf("auth: no Codex credentials found — run `codex login` first")
	}
	exp, err := jwtExpiry(f.Tokens.AccessToken)
	if err != nil {
		return gateway.CredSnapshot{}, "", err
	}
	return gateway.CredSnapshot{Access: f.Tokens.AccessToken, Refresh: f.Tokens.RefreshToken, Expiry: exp}, f.Tokens.AccountID, nil
}
```

Add `runAuthCodex` and route it from `runAuth`:

```go
// in runAuth's switch:
case "codex":
	runAuthCodex(args[1:])
```

```go
// runAuthCodex implements `drydock auth codex [--status]`.
func runAuthCodex(args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Println("drydock auth codex — copy your ChatGPT/Codex login into drydock's store")
			os.Exit(0)
		}
	}
	statusOnly := len(args) > 0 && (args[0] == "--status" || args[0] == "-status")
	credPath := filepath.Join(config.Dir(), "codex-oauth.json")
	store := gateway.NewCodexStore(credPath)

	if statusOnly {
		snap, err := store.Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, "auth: no stored credentials —", err)
			os.Exit(1)
		}
		printCodexValidity(snap)
		return
	}

	home, _ := os.UserHomeDir()
	raw, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "auth: could not read ~/.codex/auth.json — run `codex login` first")
		os.Exit(1)
	}
	snap, account, err := parseCodexCreds(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "auth:", err)
		os.Exit(1)
	}
	if err := store.Put(snap, account); err != nil {
		fmt.Fprintln(os.Stderr, "auth: failed to save credentials:", err)
		os.Exit(1)
	}
	printCodexValidity(snap)
}

// printCodexValidity prints a token-free status line. The token and account id
// are never printed.
func printCodexValidity(snap gateway.CredSnapshot) {
	remaining := time.Until(snap.Expiry)
	if remaining <= 0 {
		fmt.Println("authenticated as Codex (ChatGPT) subscription · token EXPIRED (will refresh on next use)")
		return
	}
	fmt.Printf("authenticated as Codex (ChatGPT) subscription · token valid for %dm\n", int(math.Round(remaining.Minutes())))
}
```

Update the `runAuth` no-arg usage line to `drydock auth claude|codex [--status]`. If `cmd/drydock/main.go` has a `subHelp["auth"]` string, broaden it to mention codex.

- [ ] **Step 4: Run — passes.** `go test ./cmd/drydock/ -run 'CodexCreds|JWTExpiry'`. `go build ./...`. `drydock auth codex --help` works.

- [ ] **Step 5: Commit.** `git add cmd/drydock && git commit -m "cli: drydock auth codex — bootstrap ChatGPT subscription creds"`

---

### Task 6: broker — per-task `drydock_meta` for the openai vendor

**Files:**
- Modify: `internal/broker/broker.go`
- Test: covered by existing `cmd/drydock` tasks tests (`firstMeta`/`costCell` already vendor-agnostic) + build.

**Interfaces:**
- Produces: `Broker.OpenAIAuth string` field; the `drydock_meta` line is `subscription:true` when the task's vendor is on subscription auth (anthropic OR openai).
- Consumes: `agent.Vendor(agentName)` (existing).

- [ ] **Step 1: Add the field.** Next to `AnthropicAuth string` (broker.go:77), add `OpenAIAuth string // "api_key" | "subscription"; recorded per task for `+"`drydock tasks`"+`.

- [ ] **Step 2: Extend the meta write** (broker.go:402-404):

```go
taskVendor, _ := agent.Vendor(agentName)
subscription := (taskVendor == "anthropic" && b.AnthropicAuth == "subscription") ||
	(taskVendor == "openai" && b.OpenAIAuth == "subscription")
fmt.Fprintf(logf, `{"type":"drydock_meta","subscription":%t}`+"\n", subscription)
```

- [ ] **Step 3: Build.** `go build ./...`. (`firstMeta` in `cmd/drydock/tasks.go` already reads this line vendor-agnostically, so `drydock tasks` will label Codex-subscription runs as `subscription` with no change there.)

- [ ] **Step 4: Commit.** `git add internal/broker && git commit -m "broker: record subscription meta for codex/openai tasks too"`

---

### Task 7: brokerd wiring (build the OpenAI backend per `openai_auth`)

**Files:**
- Modify: `cmd/brokerd/main.go`

**Interfaces:**
- Consumes: `cfg.OpenAIAuth` (Task 4), `gateway.OpenAIOAuthVendor`/`NewCodexStore`/`NewOAuthCredCodex` (Tasks 2-3), `Broker.OpenAIAuth` (Task 6).

- [ ] **Step 1: Replace the OpenAI backend build** (main.go:184-186) with an `openai_auth` switch parallel to the anthropic one:

```go
switch cfg.OpenAIAuth {
case "subscription":
	store := gateway.NewCodexStore(filepath.Join(config.Dir(), "codex-oauth.json"))
	snap, err := store.Load()
	if err != nil {
		die("openai_auth=subscription but no usable credentials — run `drydock auth codex`", "err", err)
	}
	backends = append(backends, gateway.Backend{Vendor: gateway.OpenAIOAuthVendor(store.AccountID()), Cred: gateway.NewOAuthCredCodex(snap, store)})
default: // api_key
	if openaiKey != "" {
		backends = append(backends, gateway.Backend{Vendor: gateway.OpenAIVendor(), Cred: gateway.StaticKey(openaiKey)})
	}
}
```

- [ ] **Step 2: Fix the boot guard** (main.go:101-104) so a subscription on either side satisfies it:

```go
haveAnthropic := cfg.AnthropicAuth == "subscription" || anthropicKey != ""
haveOpenAI := cfg.OpenAIAuth == "subscription" || openaiKey != ""
if !haveAnthropic && !haveOpenAI {
	die("set at least one of ANTHROPIC_API_KEY or OPENAI_API_KEY, or an auth subscription mode, on the broker host")
}
```

- [ ] **Step 3: Subscription budget** (main.go:202-205) — a subscription openai vendor also gets the no-USD-cap treatment:

```go
budget := cfg.TaskBudgetUSD
subAnthropic := cfg.AnthropicAuth == "subscription" && b.Vendor.Name == "anthropic"
subOpenAI := cfg.OpenAIAuth == "subscription" && b.Vendor.Name == "openai"
if subAnthropic || subOpenAI {
	budget = math.MaxFloat64
}
```

- [ ] **Step 4: Set `Broker.OpenAIAuth`** where `AnthropicAuth: cfg.AnthropicAuth` is set (main.go:242): add `OpenAIAuth: cfg.OpenAIAuth,`. Update the `slog.Info("agents available", ...)` line to `"openai", openaiKey != "" || cfg.OpenAIAuth == "subscription"`.

- [ ] **Step 5: Build + boot in api_key mode.** `go build ./... && make redteam` — host-side suite stays green (no behavior change for the API-key path).

- [ ] **Step 6: Boot in subscription mode (manual).** With `codex-oauth.json` present (Task 5) and `DRYDOCK_OPENAI_AUTH=subscription`, `drydock start` boots and logs openai available. With the file absent, it dies with the `drydock auth codex` hint.

- [ ] **Step 7: Commit.** `git add cmd/brokerd && git commit -m "brokerd: build OpenAI backend per openai_auth; boot guard; subscription budget"`

---

### Task 8: `start` guard + `doctor` codex subscription check

**Files:**
- Modify: `cmd/drydock/start.go`, `cmd/drydock/doctor.go`
- Test: `cmd/drydock/start_test.go`

**Interfaces:**
- Produces: `agentCredentialAvailable(anthropicAuth, openaiAuth, anthropicKey, openaiKey string) bool`.
- Consumes: `cfg.OpenAIAuth`, `gateway.NewCodexStore`/`NewOAuthCredCodex` (Tasks 3).

- [ ] **Step 1: Write the failing test** in `start_test.go` (extend the existing table):

```go
func TestAgentCredentialAvailable_Codex(t *testing.T) {
	cases := []struct {
		anthropicAuth, openaiAuth, aKey, oKey string
		want                                  bool
	}{
		{"api_key", "subscription", "", "", true},  // codex subscription alone is enough
		{"api_key", "api_key", "", "", false},      // nothing configured
		{"subscription", "api_key", "", "", true},  // claude subscription alone
		{"api_key", "api_key", "", "sk-o", true},   // openai key
	}
	for _, c := range cases {
		if got := agentCredentialAvailable(c.anthropicAuth, c.openaiAuth, c.aKey, c.oKey); got != c.want {
			t.Errorf("agentCredentialAvailable(%q,%q,%q,%q)=%v want %v", c.anthropicAuth, c.openaiAuth, c.aKey, c.oKey, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run — fails** (signature mismatch). `go test ./cmd/drydock/ -run TestAgentCredentialAvailable`.

- [ ] **Step 3: Implement.** Change `agentCredentialAvailable` to take `openaiAuth`:

```go
func agentCredentialAvailable(anthropicAuth, openaiAuth, anthropicKey, openaiKey string) bool {
	anthropicReady := anthropicAuth == "subscription" || anthropicKey != ""
	openaiReady := openaiAuth == "subscription" || openaiKey != ""
	return anthropicReady || openaiReady
}
```

Update `runStart` to load `cfg.OpenAIAuth` and pass it:

```go
anthropicAuth, openaiAuth := "api_key", "api_key"
if cfg, err := config.Load(config.DefaultPath()); err == nil {
	anthropicAuth, openaiAuth = cfg.AnthropicAuth, cfg.OpenAIAuth
}
if !agentCredentialAvailable(anthropicAuth, openaiAuth, os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("OPENAI_API_KEY")) {
	// ...existing hints, plus:
	fmt.Fprintln(os.Stderr, "  or set openai_auth: subscription           # use your ChatGPT/Codex subscription (run `drydock auth codex` first)")
	os.Exit(1)
}
```

- [ ] **Step 4: Add the doctor step** in `doctor.go`, immediately after the existing `anthropic` subscription block (mirror it):

```go
if cfg.OpenAIAuth == "subscription" {
	credPath := filepath.Join(config.Dir(), "codex-oauth.json")
	store := gateway.NewCodexStore(credPath)
	snap, err := store.Load()
	if err != nil {
		step("codex subscription", false, "load creds: "+err.Error())
		failed = true
	} else {
		cred := gateway.NewOAuthCredCodex(snap, store)
		if _, err := cred.Current(); err != nil {
			step("codex subscription", false, err.Error())
			failed = true
		} else {
			step("codex subscription", true, "token valid")
		}
	}
}
```

- [ ] **Step 5: Run — passes.** `go test ./cmd/drydock/`. `go build ./...`.

- [ ] **Step 6: Commit.** `git add cmd/drydock && git commit -m "cli: start accepts codex subscription; doctor codex subscription check"`

---

### Task 9: Red-team — Codex OAuth tokens + account_id never in the VM env

**Files:**
- Modify: `tests/integration/redteam_test.go`

**Interfaces:**
- Consumes: the existing A1 OAuth test pattern, `gateway.OpenAIOAuthVendor`/`NewOAuthCredCodex` + a no-op store.

- [ ] **Step 1: Add an A1 variant** that builds a gateway with `OpenAIOAuthVendor("acct-SENTINEL")` + an `OAuthCred` (via `NewOAuthCredCodex`) carrying sentinel access/refresh tokens, mints a grant, runs the same `env`/`/proc/self/environ` dump in the VM, and asserts **none** of the three sentinels (access, refresh, account id) appears while a `tok_` bearer does:

```go
func TestRedteam_A1_CodexOAuthNeverInVM(t *testing.T) {
	requireContainer(t)
	const access, refresh, account = "sk-codex-SENTINEL-ACCESS", "sk-codex-SENTINEL-REFRESH", "acct-SENTINEL-ID"
	gw, err := gateway.New(gateway.Backend{
		Vendor: gateway.OpenAIOAuthVendor(account),
		Cred:   gateway.NewOAuthCredCodex(gateway.CredSnapshot{Access: access, Refresh: refresh, Expiry: time.Now().Add(time.Hour)}, gateway.NoopStore{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	// mint a grant for vendor "openai", build VM env from grant.EnvVars(), run the
	// env dump in the sandbox, assert none of access/refresh/account appears and a
	// tok_ bearer does. (Mirror TestRedteam_A1_OAuthTokensNeverInVM.)
}
```

(Reuse the existing `NoopStore` helper from the Claude A1 test; if it's unexported/test-local, reuse the same one.)

- [ ] **Step 2: Run** (macOS + sandbox): `make redteam-vm` — passes; all three sentinels absent.

- [ ] **Step 3: Commit.** `git add tests/integration && git commit -m "redteam: A1 — Codex OAuth tokens + account_id never enter the VM"`

---

### Task 10: Docs — threat model, security, README

**Files:**
- Modify: `THREAT_MODEL.md`, `SECURITY.md`, `README.md`

- [ ] **Step 1: THREAT_MODEL — extend the subscription note.** State that `openai_auth: subscription` behaves like `anthropic_auth: subscription`: **no USD cap**; runaway controls are the **request cap (`task_max_requests`)** and **`task_timeout`**. Add the Codex backend (`chatgpt.com/backend-api/codex`) as the upstream for that mode.

- [ ] **Step 2: SECURITY.md blast-radius note.** Add: Codex subscription mode keeps a **full-account ChatGPT OAuth token + refresh token + account id** host-side at `~/.drydock/codex-oauth.json` (`0600`). It never enters the VM (A1 holds), but it is broader than a scoped API key and is not per-task revocable. Note the **ToS/rate-limit** caveat: automating a personal ChatGPT subscription headlessly may brush against OpenAI's terms and hit limits sooner; drydock makes no claim the mode is sanctioned; the operator assumes that risk.

- [ ] **Step 3: README install path.** Add a "Use your ChatGPT/Codex subscription (no API key)" subsection: `codex login` → `drydock auth codex` → set `openai_auth: subscription` (or `DRYDOCK_OPENAI_AUTH=subscription`) → `drydock start`, with `--agent codex`. State plainly that the USD budget doesn't apply; set `task_max_requests`; note the same request-cap retry caveat the Claude section uses.

- [ ] **Step 4: Commit.** `git add THREAT_MODEL.md SECURITY.md README.md && git commit -m "docs: Codex (ChatGPT) subscription auth — threat model, blast radius, install path"`

---

## Self-Review

**Spec coverage:** validation spike as gate (T1) ✓; `OpenAIOAuthVendor` + path remap (T2) ✓; `refreshOpenAI` + account-id-aware store (T3) ✓; config `openai_auth` (T4) ✓; `drydock auth codex` + JWT-exp expiry (T5) ✓; per-task subscription label for openai (T6, reusing `firstMeta`/`costCell`) ✓; brokerd wiring + boot guard + subscription budget (T7) ✓; `start` guard + `doctor` check (T8) ✓; red-team OAuth+account_id sentinel (T9) ✓; docs/blast-radius/ToS (T10) ✓. Reuse of `Credential`/`OAuthCred`/`CredStore`/request-cap is explicit in Global Constraints. No spec section is unaddressed.

**Placeholder scan:** The only deferred values are the OAuth constants and the exact inbound path — both explicit **outputs of Task 1** (a spike whose nature is discovery), referenced by name thereafter, not guessed literals. The token-endpoint encoding (JSON vs form) and the `/v1` path strip are pinned to whatever Task 1 confirms. These are real task dependencies, not TODOs.

**Type consistency:** `CredSnapshot{Access,Refresh,Expiry}` reused unchanged (T3/T5/T9); `CodexStore` implements `CredStore` (`Load`/`Save`) + `AccountID()`/`Put()` (T3), consumed in T5/T7/T8; `OpenAIOAuthVendor(accountID string) Vendor` (T2) called in T7/T9; `Vendor.BasePath` defined T2, consumed by the director T2; `NewOAuthCredCodex(snap, store)` (T3) used in T7/T8/T9; `refreshOpenAI` wired only via `NewOAuthCredCodex`; `agentCredentialAvailable(anthropicAuth, openaiAuth, anthropicKey, openaiKey)` (T8) — every caller updated in T8; `Broker.OpenAIAuth` defined T6, set T7; `parseCodexCreds → (CredSnapshot, string, error)` (T5). Consistent.
