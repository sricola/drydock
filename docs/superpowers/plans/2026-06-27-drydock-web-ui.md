# drydock Web UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a loopback-only, token-gated web UI (`drydock ui`) that is a complete browser alternative to the CLI — submit, monitor, review-the-diff-and-approve/deny/kill, approve egress, and browse history — backed by a small brokerd change that detaches a task's lifetime from its submit connection.

**Architecture:** A new `internal/webui` HTTP server serves an embedded vanilla SPA and (a) proxies an allow-listed set of brokerd endpoints over the existing unix socket for live state + actions, and (b) reads the audit dir directly for diffs/logs/history. brokerd gains one behavioral change: task context derives from `context.Background()` instead of the request, so a disconnect no longer kills the task. Audit-file parsing is extracted to a shared `internal/audit` package.

**Tech Stack:** Go (stdlib `net/http`, Go 1.22 `ServeMux` method+wildcard patterns, `go:embed`), vanilla HTML/CSS/JS (no npm/bundler). Tests: Go `httptest` + temp unix sockets + temp audit dirs.

**Spec:** `docs/superpowers/specs/2026-06-27-drydock-web-ui-design.md`

## Global Constraints

- **brokerd's only change is the context-detach (Task 1).** No new brokerd routes, no TCP exposure; the socket-only default stands.
- **Loopback bind only** (`127.0.0.1`), default port `7878`; never `0.0.0.0`.
- **Token is an `Authorization: Bearer` header on every `/api/*` request — never a cookie.** Delivered to the browser via the URL **fragment** (`#t=...`), never the query string.
- **`/api/submit` must reject `auto_approve:true`** before forwarding.
- **Task-id path safety:** validate against `^[0-9a-f]{32}$` before building any filesystem path; reject symlinks when reading audit files.
- **Submit error path:** check `resp.StatusCode >= 400` and surface the plain body BEFORE reading any stream line (brokerd's validation errors are pre-stream, plain non-200).
- **Audit parsing reads the on-disk `{"type":"result","subtype":...}` form** and derives outcome + subscription-aware cost; it does NOT look for an `outcome` field (that only exists in the streamed protocol, never on disk).
- **No JS build step.** Assets are hand-written and `go:embed`'d.
- Run `gofmt`, `go vet ./...`, and `go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...` clean before each commit (staticcheck is the CI gate).
- Match house idiom: terse "why" comments, table-driven tests, `t.Helper()` on helpers.

---

### Task 1: brokerd — detach task lifetime from the submit request

**Files:**
- Modify: `internal/broker/broker.go:378-380` (the `taskCtx` derivation) and the stale comment above it.
- Modify: `internal/broker/stream.go:31-33` (stale comment in `emit`).
- Modify: `cmd/drydock/submit.go` (the SIGINT comment ~line 224-227; add a "still running" hint on interrupt).
- Test: `internal/broker/handle_task_test.go` (add a disconnect-survival test; fix any existing disconnect-cancellation assumption).

**Interfaces:**
- Produces: no API/signature change. Behavioral change only: a disconnected client no longer cancels a task; `/admin/kill` and brokerd shutdown (`CancelAll`) remain the cancellation paths.

Background: `CancelAll` (`broker.go:337-347`) iterates per-task stored cancels (`b.cancellers`), so shutdown cancellation does NOT depend on the request context. `emit` (`stream.go:31`) already ignores write errors. So detach is a one-line context change plus comment/UX cleanup.

- [ ] **Step 1: Write the failing test** in `internal/broker/handle_task_test.go`. Look at the existing tests in that file for how a `Broker` is constructed with fakes (`runAgent`, `prepareStage`, etc.) and how `HandleTask` is driven. Add a test that starts a task, cancels the *client request context* mid-run, and asserts the task still completes (a terminal `result` line lands in the audit `.jsonl`). Sketch:

```go
func TestHandleTask_SurvivesClientDisconnect(t *testing.T) {
	// Build a Broker whose fake runAgent blocks until a channel is closed, so we
	// can disconnect the client while the "agent" is still running. Mirror the
	// existing handle_task_test.go setup (auto-approve push, fake stage/agent).
	started := make(chan struct{})
	release := make(chan struct{})
	b := newTestBroker(t) // use whatever the file's existing helper/inline setup is
	b.runAgent = func(ctx context.Context, _ []string, stdout, _ io.Writer) error {
		close(started)
		<-release // hold the "agent" open across the disconnect
		// emit a minimal result so the task terminates like a real run
		fmt.Fprintln(stdout, `{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"total_cost_usd":0,"num_turns":1}`)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/tasks",
		strings.NewReader(`{"repo_ref":"https://example.test/r.git","instruction":"x","auto_approve":true}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { b.HandleTask(rec, req); close(done) }()

	<-started
	cancel()      // client "disconnects" while the agent is mid-run
	close(release) // let the agent finish
	<-done

	// The task must have completed and written a result line despite the disconnect.
	// Find the audit file for the single task and assert a result line exists.
	assertAuditHasResult(t, b.AuditRoot) // small helper: scan *.jsonl for a {"type":"result"} line
}
```

Adapt the `Broker` construction to the exact helper the existing tests use (read the top of `handle_task_test.go` first). If an existing test asserts that a client disconnect cancels the task, update it to the new contract (disconnect no longer cancels).

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/broker/ -run TestHandleTask_SurvivesClientDisconnect -v`
Expected: FAIL — the task is cancelled when the request context is cancelled (no result line).

- [ ] **Step 3: Make the context change** in `internal/broker/broker.go`. Replace:

```go
	// One context per task. Cancelling it propagates to the container run
	// (via exec.CommandContext below) AND to gatePush's select. /admin/kill
	// invokes the stored cancel; client disconnect also propagates here.
	taskCtx, cancel := context.WithCancel(r.Context())
```

with:

```go
	// One context per task, deliberately rooted at Background (NOT r.Context()):
	// a submit client disconnecting — CLI ^C, or the web UI closing the
	// connection right after the `accepted` line — must NOT cancel the task.
	// Cancellation is driven only by /admin/kill (the stored cancel) and
	// brokerd shutdown (CancelAll iterates the stored cancels). Event writes to
	// the response become best-effort (emit already ignores write errors).
	taskCtx, cancel := context.WithCancel(context.Background())
```

Update the `emit` comment in `internal/broker/stream.go:31-33` to say cancellation is driven by `/admin/kill`/shutdown, not the request context.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/broker/ -run TestHandleTask_SurvivesClientDisconnect -v`
Expected: PASS.

- [ ] **Step 5: Update the CLI submit UX** in `cmd/drydock/submit.go`. The existing SIGINT handling (`ossignal.NotifyContext` ~line 224) cancels the in-flight POST — which now just aborts the local stream, leaving the task running. Update the now-wrong comment ("brokerd's HandleTask reads the request context and treats cancellation as a task kill") to reflect that ^C detaches locally and the task keeps running. Where the CLI handles the interrupt/stream-abort, print an actionable line if the task id is known:

```go
// printed when the submit stream is interrupted (^C) — the task is NOT killed.
fmt.Fprintf(os.Stderr, "drydock: detached — task %s is still running; `drydock kill %s` to stop, `drydock logs %s -f` to reattach\n", id, id, id)
```

(Use the task id captured from the `accepted` event; if no id was seen yet, print a generic "task may still be running — check `drydock tasks`".) Keep this minimal — do not restructure submit.

- [ ] **Step 6: Run the broker + submit suites + vet/staticcheck**

Run: `go test ./internal/broker/ ./cmd/drydock/ -count=1 && go vet ./... && go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./internal/broker/... ./cmd/drydock/...`
Expected: PASS, no findings.

- [ ] **Step 7: Commit**

```bash
git add internal/broker/broker.go internal/broker/stream.go internal/broker/handle_task_test.go cmd/drydock/submit.go
git commit -m "broker: detach task lifetime from the submit request (kill via /admin/kill only)"
```

---

### Task 2: `internal/audit` — extract on-disk audit parsing

**Files:**
- Create: `internal/audit/audit.go`
- Create: `internal/audit/audit_test.go`
- Modify: `cmd/drydock/tasks.go` (delete the moved code; call the new package).

**Interfaces:**
- Produces (consumed by Task 5 and `cmd/drydock/tasks.go`):

```go
package audit

type Result struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	DurationMs   int64   `json:"duration_ms"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	NumTurns     int     `json:"num_turns"`
}
type Meta struct {
	Type         string `json:"type"`
	Subscription bool   `json:"subscription"`
	Sensitive    bool   `json:"sensitive"`
}
func ReadMeta(path string) Meta                       // first-line drydock_meta; zero value if absent
func LastResult(path string, size int64) (Result, bool) // tail-scan for the terminal {"type":"result"} line
func Outcome(r Result, ok bool, m Meta) string        // "running?" if !ok; else interrupted/error/ok (n turn)/subtype, +" · sensitive"
func Cost(m Meta, r Result, ok bool) string           // "-" if !ok; "subscription"; else "$%.4f"
func HasDuration(r Result, ok bool) bool              // ok && Subtype != "interrupted"
```

- [ ] **Step 1: Write the failing test** `internal/audit/audit_test.go`:

```go
package audit

import (
	"os"
	"path/filepath"
	"testing"
)

func writeJSONL(t *testing.T, lines ...string) (path string, size int64) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "id.jsonl")
	data := ""
	for _, l := range lines {
		data += l + "\n"
	}
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	return p, fi.Size()
}

func TestOutcomeAndCost(t *testing.T) {
	meta := `{"type":"drydock_meta","subscription":false,"sensitive":false}`
	subMeta := `{"type":"drydock_meta","subscription":true,"sensitive":false}`
	sens := `{"type":"drydock_meta","subscription":false,"sensitive":true}`

	cases := []struct {
		name        string
		lines       []string
		wantOutcome string
		wantCost    string
		wantDur     bool
	}{
		{"success with turns", []string{meta, `{"type":"result","subtype":"success","is_error":false,"duration_ms":12,"total_cost_usd":0.0731,"num_turns":2}`}, "ok (2 turn)", "$0.0731", true},
		{"success no turns", []string{meta, `{"type":"result","subtype":"success","is_error":false,"duration_ms":12,"total_cost_usd":0,"num_turns":0}`}, "ok", "$0.0000", true},
		{"error", []string{meta, `{"type":"result","subtype":"error","is_error":true,"duration_ms":5,"total_cost_usd":0.01,"num_turns":1}`}, "error", "$0.0100", true},
		{"interrupted", []string{meta, `{"type":"result","subtype":"interrupted","is_error":false,"duration_ms":0,"total_cost_usd":0,"num_turns":0}`}, "interrupted", "$0.0000", false},
		{"subscription", []string{subMeta, `{"type":"result","subtype":"success","is_error":false,"duration_ms":3,"total_cost_usd":0,"num_turns":1}`}, "ok (1 turn)", "subscription", true},
		{"sensitive suffix", []string{sens, `{"type":"result","subtype":"success","is_error":false,"duration_ms":3,"total_cost_usd":0,"num_turns":1}`}, "ok (1 turn) · sensitive", "$0.0000", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, sz := writeJSONL(t, tc.lines...)
			last, ok := LastResult(p, sz)
			if !ok {
				t.Fatal("expected a result line")
			}
			m := ReadMeta(p)
			if got := Outcome(last, ok, m); got != tc.wantOutcome {
				t.Errorf("Outcome = %q, want %q", got, tc.wantOutcome)
			}
			if got := Cost(m, last, ok); got != tc.wantCost {
				t.Errorf("Cost = %q, want %q", got, tc.wantCost)
			}
			if got := HasDuration(last, ok); got != tc.wantDur {
				t.Errorf("HasDuration = %v, want %v", got, tc.wantDur)
			}
		})
	}
}

func TestNoResultLine(t *testing.T) {
	p, sz := writeJSONL(t, `{"type":"drydock_meta","subscription":false,"sensitive":false}`)
	last, ok := LastResult(p, sz)
	if ok {
		t.Fatal("expected ok=false with no result line")
	}
	if Outcome(last, ok, ReadMeta(p)) != "running?" {
		t.Errorf("Outcome should be running? when no result")
	}
	if Cost(ReadMeta(p), last, ok) != "-" {
		t.Errorf("Cost should be - when no result")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/audit/ -v`
Expected: FAIL — package/functions undefined.

- [ ] **Step 3: Implement `internal/audit/audit.go`** by moving the logic from `cmd/drydock/tasks.go` (the `auditResult`→`Result`, `auditMeta`→`Meta`, `readMeta`→`ReadMeta`, `lastResult`→`LastResult` bodies are verbatim moves; add `Outcome`, `Cost`, `HasDuration` derived from `summarize`/`costCell`):

```go
// Package audit parses drydock's on-disk per-task audit log (<id>.jsonl). The
// last {"type":"result"} line summarises the run; the first {"type":"drydock_meta"}
// line records auth mode + sensitivity. This is the single source of truth for
// outcome/cost so `drydock tasks` and the web UI agree.
package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type Result struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	DurationMs   int64   `json:"duration_ms"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	NumTurns     int     `json:"num_turns"`
}

type Meta struct {
	Type         string `json:"type"`
	Subscription bool   `json:"subscription"`
	Sensitive    bool   `json:"sensitive"`
}

// ReadMeta returns the drydock_meta first line. Legacy/absent → zero value.
func ReadMeta(path string) Meta {
	f, err := os.Open(path)
	if err != nil {
		return Meta{}
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return Meta{}
	}
	var m Meta
	if json.Unmarshal(bytes.TrimSpace(line), &m) != nil || m.Type != "drydock_meta" {
		return Meta{}
	}
	return m
}

// LastResult finds the final {"type":"result",...} line by reading only the
// file tail. ok=false when none is present (still running / killed early). It
// tolerates an unterminated trailing line (brokerd may be mid-write).
func LastResult(path string, size int64) (Result, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Result{}, false
	}
	defer f.Close()
	const tail = 16 * 1024
	if size > tail {
		if _, err := f.Seek(size-tail, io.SeekStart); err != nil {
			return Result{}, false
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return Result{}, false
	}
	lines := bytes.Split(data, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		var x Result
		if json.Unmarshal(lines[i], &x) == nil && x.Type == "result" {
			return x, true
		}
	}
	return Result{}, false
}

// HasDuration reports whether a real duration is known. An interrupted task
// (brokerd died under it) has a synthetic 0ms we must not display as "0s".
func HasDuration(r Result, ok bool) bool { return ok && r.Subtype != "interrupted" }

// Outcome derives the human outcome string. Mirrors the old summarize() switch.
func Outcome(r Result, ok bool, m Meta) string {
	if !ok {
		return "running?"
	}
	var s string
	switch {
	case r.Subtype == "interrupted":
		s = "interrupted"
	case r.IsError:
		s = "error"
	case r.Subtype == "success":
		if r.NumTurns > 0 {
			s = fmt.Sprintf("ok (%d turn)", r.NumTurns)
		} else {
			s = "ok"
		}
	default:
		s = r.Subtype
	}
	if m.Sensitive {
		s += " · sensitive"
	}
	return s
}

// Cost formats the cost column. Subscription runs show the literal word; a
// task with no result line shows "-".
func Cost(m Meta, r Result, ok bool) string {
	if !ok {
		return "-"
	}
	if m.Subscription {
		return "subscription"
	}
	return fmt.Sprintf("$%.4f", r.TotalCostUSD)
}
```

- [ ] **Step 4: Rewire `cmd/drydock/tasks.go`** to use the package. Delete `auditResult`, `auditMeta`, `readMeta`, `lastResult`, `costCell` from `tasks.go`. Add `import "drydock/internal/audit"`. Rewrite `summarize` to delegate (duration formatting via the existing `shortDur` stays in the CLI):

```go
func summarize(id, path string, info os.FileInfo) taskRow {
	r := taskRow{id: id, mtime: info.ModTime(), age: relAge(info.ModTime()), dur: "-", cost: "-", outcome: "running?"}
	last, ok := audit.LastResult(path, info.Size())
	meta := audit.ReadMeta(path)
	r.outcome = audit.Outcome(last, ok, meta)
	r.cost = audit.Cost(meta, last, ok)
	if audit.HasDuration(last, ok) {
		r.dur = shortDur(last.DurationMs)
	}
	return r
}
```

Move any `tasks_test.go` cases that tested the moved functions into `internal/audit/audit_test.go` (covered above); keep `tasks_test.go` cases that test `summarize`/`runTasks` formatting.

- [ ] **Step 5: Run the audit + CLI suites + vet/staticcheck**

Run: `go test ./internal/audit/ ./cmd/drydock/ -count=1 && go vet ./... && go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./internal/audit/... ./cmd/drydock/...`
Expected: PASS, no findings. `drydock tasks` output is unchanged.

- [ ] **Step 6: Commit**

```bash
git add internal/audit/ cmd/drydock/tasks.go cmd/drydock/tasks_test.go
git commit -m "audit: extract on-disk jsonl parsing into internal/audit (shared by CLI + web UI)"
```

---

### Task 3: `internal/webui` — server skeleton, middleware, embedded assets

**Files:**
- Create: `internal/webui/server.go` (the `Server`, `Handler()`, middleware, id-safety).
- Create: `internal/webui/embed.go` (`go:embed assets/*`).
- Create: `internal/webui/assets/index.html` (placeholder; replaced in Task 8).
- Create: `internal/webui/server_test.go`.

**Interfaces:**
- Produces (consumed by Tasks 4-7):

```go
package webui

// Server serves the loopback UI: embedded SPA + /api/* (proxy to brokerd over
// the socket + direct audit-dir reads).
type Server struct {
	AuditRoot string                       // <audit_root> for diff/logs/widen/history
	Token     string                       // "" means --no-token (gate disabled)
	BrokerDial func() (net.Conn, error)     // dials the brokerd unix socket
}

func (s *Server) Handler() http.Handler        // wires GET / + /api/*
// internal helpers (this task): s.authed(next) middleware, validID(id) bool
```

- Consumes: nothing yet (proxy/audit/submit handlers land in Tasks 4-6 but their route stubs return 501 here so the mux is complete and testable).

- [ ] **Step 1: Write the failing test** `internal/webui/server_test.go`:

```go
package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func testServer() *Server { return &Server{AuditRoot: "/tmp", Token: "secret"} }

func do(t *testing.T, s *Server, method, target, host, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if host != "" {
		req.Host = host
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAuth(t *testing.T) {
	s := testServer()
	// No token header → 403.
	if rec := do(t, s, "GET", "/api/tasks", "127.0.0.1:7878", ""); rec.Code != http.StatusForbidden {
		t.Errorf("no-token = %d, want 403", rec.Code)
	}
	// Wrong token → 403.
	if rec := do(t, s, "GET", "/api/tasks", "127.0.0.1:7878", "Bearer nope"); rec.Code != http.StatusForbidden {
		t.Errorf("bad-token = %d, want 403", rec.Code)
	}
	// Right token reaches the (stub) handler → not 403 (501 stub is fine here).
	if rec := do(t, s, "GET", "/api/tasks", "127.0.0.1:7878", "Bearer secret"); rec.Code == http.StatusForbidden {
		t.Errorf("good-token still 403")
	}
}

func TestHostCheck(t *testing.T) {
	s := testServer()
	if rec := do(t, s, "GET", "/api/tasks", "evil.example.com", "Bearer secret"); rec.Code != http.StatusForbidden {
		t.Errorf("rebinding host = %d, want 403", rec.Code)
	}
}

func TestNoTokenModeSkipsGate(t *testing.T) {
	s := &Server{AuditRoot: "/tmp", Token: ""} // --no-token
	if rec := do(t, s, "GET", "/api/tasks", "127.0.0.1:7878", ""); rec.Code == http.StatusForbidden {
		t.Errorf("--no-token must not 403")
	}
}

func TestValidID(t *testing.T) {
	good := "0123456789abcdef0123456789abcdef"
	for _, bad := range []string{"", "../etc", "ABC", good + "x", good[:31], "g" + good[1:]} {
		if validID(bad) {
			t.Errorf("validID(%q) = true, want false", bad)
		}
	}
	if !validID(good) {
		t.Errorf("validID(%q) = false, want true", good)
	}
}

func TestServesIndex(t *testing.T) {
	s := testServer()
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "127.0.0.1:7878"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/webui/ -v`
Expected: FAIL — package/symbols undefined.

- [ ] **Step 3: Create the placeholder asset** `internal/webui/assets/index.html`:

```html
<!doctype html><html><head><meta charset="utf-8"><title>drydock</title></head>
<body><p>drydock UI</p></body></html>
```

- [ ] **Step 4: Create `internal/webui/embed.go`:**

```go
package webui

import "embed"

//go:embed assets
var assetsFS embed.FS
```

- [ ] **Step 5: Implement `internal/webui/server.go`:**

```go
package webui

import (
	"io/fs"
	"net"
	"net/http"
	"regexp"
	"strings"
)

type Server struct {
	AuditRoot  string
	Token      string
	BrokerDial func() (net.Conn, error)
}

var idRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func validID(id string) bool { return idRe.MatchString(id) }

// Handler wires the SPA and the /api surface. Proxy/audit/submit handlers are
// added in later tasks; here they are stubs so the mux + middleware are testable.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	sub, _ := fs.Sub(assetsFS, "assets")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.authed(h)) }
	stub := func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "not implemented", http.StatusNotImplemented) }
	api("GET /api/tasks", stub)
	api("GET /api/pending", stub)
	api("POST /api/approve/{id}", stub)
	api("POST /api/deny/{id}", stub)
	api("POST /api/kill/{id}", stub)
	api("POST /api/submit", stub)
	api("GET /api/diff/{id}", stub)
	api("GET /api/logs/{id}", stub)
	api("GET /api/widen/{id}", stub)
	api("GET /api/history", stub)
	return mux
}

// authed enforces the loopback Host allowlist, an optional cross-origin Origin
// check, and the bearer token (unless --no-token). It is the only auth boundary
// for /api/*; the token is never read from a cookie (cookies are CSRF-forgeable).
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !loopbackOrigin(o) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		if s.Token != "" {
			if r.Header.Get("Authorization") != "Bearer "+s.Token {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

func hostname(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

func loopbackHost(hostport string) bool {
	h := hostname(hostport)
	return h == "127.0.0.1" || h == "localhost" || h == "[::1]" || h == "::1"
}

func loopbackOrigin(origin string) bool {
	// Origin is scheme://host[:port]; check the host component is loopback.
	i := strings.Index(origin, "://")
	if i < 0 {
		return false
	}
	return loopbackHost(origin[i+3:])
}
```

- [ ] **Step 6: Run the test to verify it passes**

Run: `go test ./internal/webui/ -v && go vet ./internal/webui/ && go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./internal/webui/...`
Expected: PASS, no findings.

- [ ] **Step 7: Commit**

```bash
git add internal/webui/
git commit -m "webui: server skeleton — embedded assets, loopback+token+host middleware, id-safety"
```

---

### Task 4: `internal/webui` — proxy routes to brokerd

**Files:**
- Modify: `internal/webui/server.go` (replace the proxy stubs; add a `proxy` helper + brokerd client).
- Test: `internal/webui/proxy_test.go` (new).

**Interfaces:**
- Consumes: `Server.BrokerDial func() (net.Conn, error)` (Task 3).
- Produces: real `GET /api/tasks`, `GET /api/pending`, `POST /api/approve|deny|kill/{id}` that forward to brokerd `/admin/...` over the socket and copy status+body back. Path `{id}` validated with `validID`.

- [ ] **Step 1: Write the failing test** `internal/webui/proxy_test.go`. Stand up a fake brokerd on a temp unix socket:

```go
package webui

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeBroker serves on a temp unix socket and records the last request.
func fakeBroker(t *testing.T, h http.Handler) func() (net.Conn, error) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "b.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(); os.Remove(sock) })
	return func() (net.Conn, error) { return net.Dial("unix", sock) }
}

func TestProxyTasks(t *testing.T) {
	dial := fakeBroker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/tasks" {
			t.Errorf("brokerd path = %s, want /admin/tasks", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"id":"x"}]`)
	}))
	s := &Server{Token: "secret", BrokerDial: dial}
	rec := do(t, s, "GET", "/api/tasks", "127.0.0.1:7878", "Bearer secret")
	if rec.Code != http.StatusOK || rec.Body.String() != `[{"id":"x"}]` {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestProxyApprovePostsToAdmin(t *testing.T) {
	var gotPath, gotMethod string
	dial := fakeBroker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	s := &Server{Token: "secret", BrokerDial: dial}
	id := "0123456789abcdef0123456789abcdef"
	rec := do(t, s, "POST", "/api/approve/"+id, "127.0.0.1:7878", "Bearer secret")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if gotMethod != "POST" || gotPath != "/admin/approve/"+id {
		t.Fatalf("brokerd got %s %s", gotMethod, gotPath)
	}
}

func TestProxyRejectsBadID(t *testing.T) {
	s := &Server{Token: "secret", BrokerDial: fakeBroker(t, http.NotFoundHandler())}
	rec := do(t, s, "POST", "/api/approve/NOTHEX", "127.0.0.1:7878", "Bearer secret")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id = %d, want 400", rec.Code)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/webui/ -run TestProxy -v`
Expected: FAIL — routes return 501 stub.

- [ ] **Step 3: Implement the proxy** in `internal/webui/server.go`. Add a brokerd client built from `BrokerDial`, and replace the proxy stubs:

```go
// brokerClient returns an http.Client that dials brokerd's unix socket. Used
// for the short admin pokes (5s timeout). base is the dummy host the dialer
// ignores. Submit (Task 6) uses a separate no-timeout client.
func (s *Server) brokerClient() (*http.Client, string) {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) { return s.BrokerDial() },
		},
	}, "http://brokerd"
}

// proxy forwards the request to brokerd at adminPath and copies status+body back.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, method, adminPath string) {
	c, base := s.brokerClient()
	req, err := http.NewRequestWithContext(r.Context(), method, base+adminPath, nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	resp, err := c.Do(req)
	if err != nil {
		http.Error(w, "brokerd not running — run `drydock start`", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// signalHandler builds an approve/deny/kill handler for the given verb.
func (s *Server) signalHandler(verb string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validID(id) {
			http.Error(w, "bad task id", http.StatusBadRequest)
			return
		}
		s.proxy(w, r, "POST", "/admin/"+verb+"/"+id)
	}
}
```

Wire them in `Handler()` (replace the stubs):

```go
	api("GET /api/tasks", func(w http.ResponseWriter, r *http.Request) { s.proxy(w, r, "GET", "/admin/tasks") })
	api("GET /api/pending", func(w http.ResponseWriter, r *http.Request) { s.proxy(w, r, "GET", "/admin/pending") })
	api("POST /api/approve/{id}", s.signalHandler("approve"))
	api("POST /api/deny/{id}", s.signalHandler("deny"))
	api("POST /api/kill/{id}", s.signalHandler("kill"))
```

Add imports: `context`, `io`, `time`.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/webui/ -count=1 && go vet ./internal/webui/ && go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./internal/webui/...`
Expected: PASS, no findings.

- [ ] **Step 5: Commit**

```bash
git add internal/webui/server.go internal/webui/proxy_test.go
git commit -m "webui: proxy /api/{tasks,pending,approve,deny,kill} to brokerd over the socket"
```

---

### Task 5: `internal/webui` — audit-read routes (diff, logs, widen, history)

**Files:**
- Create: `internal/webui/audit_routes.go`
- Modify: `internal/webui/server.go` (replace the four audit stubs).
- Test: `internal/webui/audit_routes_test.go`

**Interfaces:**
- Consumes: `internal/audit` (Task 2), `Server.AuditRoot`, `validID` (Task 3).
- Produces:
  - `GET /api/diff/{id}` → `<AuditRoot>/<id>.diff` as `text/plain`; 404 absent.
  - `GET /api/logs/{id}` → `<AuditRoot>/<id>.jsonl` as `text/plain`; 404 absent.
  - `GET /api/widen/{id}` → `<AuditRoot>/<id>.widen.json` as `application/json`; 404 absent.
  - `GET /api/history` → JSON `[]HistoryItem` (newest-first), where:

```go
type HistoryItem struct {
	ID          string `json:"id"`
	Outcome     string `json:"outcome"`
	Cost        string `json:"cost"`
	DurationMs  int64  `json:"duration_ms"`
	HasDuration bool   `json:"has_duration"`
	MtimeUnix   int64  `json:"mtime_unix"`
}
```

- [ ] **Step 1: Write the failing test** `internal/webui/audit_routes_test.go`:

```go
package webui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func auditServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	os.WriteFile(filepath.Join(dir, id+".diff"), []byte("diff --git a b\n+line\n"), 0o600)
	os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(
		`{"type":"drydock_meta","subscription":false,"sensitive":false}`+"\n"+
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":1200,"total_cost_usd":0.05,"num_turns":3}`+"\n"), 0o600)
	os.WriteFile(filepath.Join(dir, id+".widen.json"), []byte(`[{"host":"x.test","ports":[443]}]`), 0o600)
	return &Server{AuditRoot: dir, Token: "secret"}
}

func TestDiffAndLogsAndWiden(t *testing.T) {
	s := auditServer(t)
	id := "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct{ path, want string }{
		{"/api/diff/" + id, "diff --git a b\n+line\n"},
		{"/api/widen/" + id, `[{"host":"x.test","ports":[443]}]`},
	} {
		rec := do(t, s, "GET", tc.path, "127.0.0.1:7878", "Bearer secret")
		if rec.Code != http.StatusOK || rec.Body.String() != tc.want {
			t.Errorf("%s = %d %q", tc.path, rec.Code, rec.Body.String())
		}
	}
	// Missing → 404.
	rec := do(t, s, "GET", "/api/diff/ffffffffffffffffffffffffffffffff", "127.0.0.1:7878", "Bearer secret")
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing diff = %d, want 404", rec.Code)
	}
	// Bad id → 400.
	if rec := do(t, s, "GET", "/api/diff/NOPE", "127.0.0.1:7878", "Bearer secret"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad id = %d, want 400", rec.Code)
	}
}

func TestHistory(t *testing.T) {
	s := auditServer(t)
	rec := do(t, s, "GET", "/api/history", "127.0.0.1:7878", "Bearer secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("history = %d", rec.Code)
	}
	var items []HistoryItem
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Outcome != "ok (3 turn)" || items[0].Cost != "$0.0500" || !items[0].HasDuration || items[0].DurationMs != 1200 {
		t.Fatalf("history item wrong: %+v", items)
	}
}

func TestSymlinkRejected(t *testing.T) {
	s := auditServer(t)
	id := "ffffffffffffffffffffffffffffffff"
	os.Symlink("/etc/hosts", filepath.Join(s.AuditRoot, id+".diff"))
	rec := do(t, s, "GET", "/api/diff/"+id, "127.0.0.1:7878", "Bearer secret")
	if rec.Code == http.StatusOK {
		t.Fatalf("symlinked diff was served (status %d) — must not follow symlinks", rec.Code)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/webui/ -run 'TestDiff|TestHistory|TestSymlink' -v`
Expected: FAIL — stubs / undefined `HistoryItem`.

- [ ] **Step 3: Implement `internal/webui/audit_routes.go`:**

```go
package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"drydock/internal/audit"
)

type HistoryItem struct {
	ID          string `json:"id"`
	Outcome     string `json:"outcome"`
	Cost        string `json:"cost"`
	DurationMs  int64  `json:"duration_ms"`
	HasDuration bool   `json:"has_duration"`
	MtimeUnix   int64  `json:"mtime_unix"`
}

// openAuditFile opens <AuditRoot>/<id><suffix>, refusing symlinks (O_NOFOLLOW)
// and anything whose id isn't the exact task-id grammar. Returns 400/404-able
// errors via the bool: (file, 404?) — caller maps a nil file to the right code.
func (s *Server) openAuditFile(id, suffix string) (*os.File, bool) {
	if !validID(id) {
		return nil, false // caller already validated; defensive
	}
	p := filepath.Join(s.AuditRoot, id+suffix)
	f, err := os.OpenFile(p, os.O_RDONLY|syscallNoFollow, 0)
	if err != nil {
		return nil, true // treat as not-found (missing or symlink)
	}
	return f, true
}

func (s *Server) serveAuditFile(w http.ResponseWriter, r *http.Request, suffix, contentType string) {
	id := r.PathValue("id")
	if !validID(id) {
		http.Error(w, "bad task id", http.StatusBadRequest)
		return
	}
	f, _ := s.openAuditFile(id, suffix)
	if f == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", contentType)
	io.Copy(w, f)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(s.AuditRoot)
	if err != nil {
		_ = json.NewEncoder(w).Encode([]HistoryItem{}) // empty audit dir → empty list
		return
	}
	items := []HistoryItem{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		if !validID(id) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(s.AuditRoot, name)
		last, ok := audit.LastResult(path, info.Size())
		meta := audit.ReadMeta(path)
		items = append(items, HistoryItem{
			ID:          id,
			Outcome:     audit.Outcome(last, ok, meta),
			Cost:        audit.Cost(meta, last, ok),
			DurationMs:  last.DurationMs,
			HasDuration: audit.HasDuration(last, ok),
			MtimeUnix:   info.ModTime().Unix(),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].MtimeUnix > items[j].MtimeUnix })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}
```

Add a tiny platform shim `internal/webui/nofollow_unix.go` (drydock is macOS/Linux only):

```go
package webui

import "syscall"

// syscallNoFollow makes os.OpenFile refuse to traverse a final-component
// symlink, so a planted <id>.diff -> /etc/passwd can't be read out of the
// audit dir. drydock runs on macOS/Linux only.
const syscallNoFollow = syscall.O_NOFOLLOW
```

Wire the routes in `Handler()` (replace the four audit stubs):

```go
	api("GET /api/diff/{id}", func(w http.ResponseWriter, r *http.Request) { s.serveAuditFile(w, r, ".diff", "text/plain; charset=utf-8") })
	api("GET /api/logs/{id}", func(w http.ResponseWriter, r *http.Request) { s.serveAuditFile(w, r, ".jsonl", "text/plain; charset=utf-8") })
	api("GET /api/widen/{id}", func(w http.ResponseWriter, r *http.Request) { s.serveAuditFile(w, r, ".widen.json", "application/json") })
	api("GET /api/history", s.handleHistory)
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/webui/ -count=1 && go vet ./internal/webui/ && go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./internal/webui/...`
Expected: PASS, no findings. (Note: the symlink test proves `O_NOFOLLOW` works.)

- [ ] **Step 5: Commit**

```bash
git add internal/webui/audit_routes.go internal/webui/nofollow_unix.go internal/webui/server.go internal/webui/audit_routes_test.go
git commit -m "webui: audit-read routes — diff/logs/widen (no-follow) + history via internal/audit"
```

---

### Task 6: `internal/webui` — submit flow

**Files:**
- Create: `internal/webui/submit.go`
- Modify: `internal/webui/server.go` (replace the submit stub).
- Test: `internal/webui/submit_test.go`

**Interfaces:**
- Consumes: `Server.BrokerDial`.
- Produces: `POST /api/submit` — reject `auto_approve:true` (400); forward the body to brokerd `POST /tasks` on a **no-timeout, background-context** request; if brokerd returns `>=400`, copy status+body to the SPA (no stream read); on 200, read the first NDJSON line, expect `event:accepted`, return `{"id":"<task_id>"}`, then close the brokerd connection (the task survives via Task 1's detach).

- [ ] **Step 1: Write the failing test** `internal/webui/submit_test.go`:

```go
package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postSubmit(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/submit", strings.NewReader(body))
	req.Host = "127.0.0.1:7878"
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestSubmitRejectsAutoApprove(t *testing.T) {
	s := &Server{Token: "secret", BrokerDial: fakeBroker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("brokerd must not be called when auto_approve is set")
	}))}
	rec := postSubmit(t, s, `{"repo_ref":"https://x.test/r.git","instruction":"y","auto_approve":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("auto_approve submit = %d, want 400", rec.Code)
	}
}

func TestSubmitHappyPathReturnsID(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	s := &Server{Token: "secret", BrokerDial: fakeBroker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		io.WriteString(w, `{"event":"accepted","task_id":"`+id+`","repo":"https://x.test/r.git"}`+"\n")
		w.(http.Flusher).Flush()
		// then block as a real task would; the UI server should have closed already
		<-r.Context().Done()
	}))}
	rec := postSubmit(t, s, `{"repo_ref":"https://x.test/r.git","instruction":"y"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("submit = %d, want 200", rec.Code)
	}
	var got struct{ ID string `json:"id"` }
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.ID != id {
		t.Fatalf("returned id = %q, want %q", got.ID, id)
	}
}

func TestSubmitSurfacesPreAcceptError(t *testing.T) {
	s := &Server{Token: "secret", BrokerDial: fakeBroker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "repo_ref must be an https/git/ssh URL", http.StatusBadRequest)
	}))}
	rec := postSubmit(t, s, `{"repo_ref":"/local/path","instruction":"y"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "https/git/ssh") {
		t.Fatalf("error body not surfaced: %q", rec.Body.String())
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/webui/ -run TestSubmit -v`
Expected: FAIL — stub returns 501.

- [ ] **Step 3: Implement `internal/webui/submit.go`:**

```go
package webui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
)

// handleSubmit starts a task. The UI server — not the browser — owns the brokerd
// connection, but because brokerd roots task context at Background (see the
// broker detach change), the server can read the `accepted` line for the id and
// close immediately; the task runs independently and is observed via polling.
func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Refuse auto_approve: the entire point of the UI is a human at the gate.
	var probe struct {
		AutoApprove bool `json:"auto_approve"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.AutoApprove {
		http.Error(w, "auto_approve is not allowed from the web UI; approve at the gate", http.StatusBadRequest)
		return
	}

	// No-timeout, Background-context request: tasks run for minutes and must not
	// die when this handler returns.
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) { return s.BrokerDial() },
	}}
	req, err := http.NewRequestWithContext(context.Background(), "POST", "http://brokerd/tasks", bytes.NewReader(body))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "brokerd not running — run `drydock start`", http.StatusBadGateway)
		return
	}
	// Pre-accept failures (bad repo 400, slot full 503, bad egress 400) are plain
	// non-200 bodies with NO stream — surface verbatim and stop. No goroutine.
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}
	// 200: the first NDJSON line is the accepted event carrying the task id.
	line, err := bufio.NewReader(resp.Body).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		resp.Body.Close()
		http.Error(w, "brokerd accepted no task", http.StatusBadGateway)
		return
	}
	var ev struct {
		Event  string `json:"event"`
		TaskID string `json:"task_id"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &ev) != nil || ev.Event != "accepted" || ev.TaskID == "" {
		resp.Body.Close()
		w.WriteHeader(http.StatusBadGateway)
		w.Write(line) // surface whatever brokerd said (e.g. an early error event)
		return
	}
	// Got the id. Close the brokerd connection — the task is detached from it.
	resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"id": ev.TaskID})
}
```

Wire it in `Handler()` (replace the submit stub):

```go
	api("POST /api/submit", s.handleSubmit)
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/webui/ -count=1 && go vet ./internal/webui/ && go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./internal/webui/...`
Expected: PASS, no findings.

- [ ] **Step 5: Commit**

```bash
git add internal/webui/submit.go internal/webui/server.go internal/webui/submit_test.go
git commit -m "webui: submit flow — reject auto_approve, surface pre-accept errors, return task id and close"
```

---

### Task 7: `drydock ui` command + main wiring

**Files:**
- Create: `cmd/drydock/ui.go`
- Create: `cmd/drydock/ui_test.go`
- Modify: `cmd/drydock/main.go` (add the `ui` case + usage line).

**Interfaces:**
- Consumes: `internal/webui.Server` (Tasks 3-6); `socketPath()` and `auditDir()` (existing CLI helpers in `client.go`/`util.go`).
- Produces: `func runUI(args []string)`; pure helpers `mintToken() (string, error)` and `uiURL(port int, token string) string` (tested directly).

- [ ] **Step 1: Write the failing test** `cmd/drydock/ui_test.go`:

```go
package main

import (
	"regexp"
	"strings"
	"testing"
)

func TestMintToken(t *testing.T) {
	tok, err := mintToken()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(tok) {
		t.Fatalf("token %q is not 32 bytes hex", tok)
	}
	tok2, _ := mintToken()
	if tok == tok2 {
		t.Fatal("tokens must be random")
	}
}

func TestUIURL(t *testing.T) {
	// Token goes in the fragment, never the query.
	if got := uiURL(7878, "abc"); got != "http://127.0.0.1:7878/#t=abc" {
		t.Fatalf("uiURL = %q", got)
	}
	// --no-token: no fragment.
	if got := uiURL(7878, ""); got != "http://127.0.0.1:7878/" || strings.Contains(got, "#") {
		t.Fatalf("no-token uiURL = %q", got)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cmd/drydock/ -run 'TestMintToken|TestUIURL' -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement `cmd/drydock/ui.go`:**

```go
package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"

	"drydock/internal/webui"
)

func mintToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// uiURL builds the launch URL. The token rides in the fragment (#t=) so it is
// never sent in Referer headers or written to server logs.
func uiURL(port int, token string) string {
	base := fmt.Sprintf("http://127.0.0.1:%d/", port)
	if token == "" {
		return base
	}
	return base + "#t=" + token
}

func runUI(args []string) {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	port := fs.Int("port", 7878, "loopback port to bind")
	open := fs.Bool("open", false, "open the URL in the default browser")
	noToken := fs.Bool("no-token", false, "disable the access token (trusted single-user machines only)")
	_ = fs.Parse(args)

	token := ""
	if !*noToken {
		t, err := mintToken()
		if err != nil {
			die("mint token: %v", err)
		}
		token = t
	}

	srv := &webui.Server{
		AuditRoot:  auditDir(),
		Token:      token,
		BrokerDial: func() (net.Conn, error) { return net.Dial("unix", socketPath()) },
	}

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		die("cannot bind %s: %v (is another `drydock ui` running, or the port taken?)", addr, err)
	}

	url := uiURL(*port, token)
	if *noToken {
		fmt.Fprintln(os.Stderr, "WARNING: --no-token — any local process or web page can submit tasks, approve pushes, and kill tasks via this server.")
	}
	fmt.Printf("UI ready: %s\n", url)
	if *open {
		_ = exec.Command("open", url).Start() // macOS; best-effort
	}
	if err := http.Serve(ln, srv.Handler()); err != nil {
		die("ui server: %v", err)
	}
}
```

- [ ] **Step 4: Wire `main.go`.** Add to the `switch cmd` in `cmd/drydock/main.go` (near the other cases):

```go
	case "ui":
		runUI(os.Args[2:])
```

Add a usage line in `usage()` alongside the other commands, e.g.:

```go
	fmt.Println("  ui [--port N] [--open] [--no-token]   local web UI (loopback, token-gated)")
```

- [ ] **Step 5: Run tests + vet/staticcheck + a manual smoke**

Run: `go build ./... && go test ./cmd/drydock/ -count=1 && go vet ./... && go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...`
Expected: PASS, no findings.
Manual (optional, needs nothing running): `./drydock ui --port 7899 &` then `curl -s -H "Authorization: Bearer $(…)" http://127.0.0.1:7899/api/history` returns `[]` or a 502 if brokerd is down; `curl -s http://127.0.0.1:7899/` returns the placeholder HTML. Kill it after.

- [ ] **Step 6: Commit**

```bash
git add cmd/drydock/ui.go cmd/drydock/ui_test.go cmd/drydock/main.go
git commit -m "drydock ui: loopback server command (token in fragment, --open, --no-token)"
```

---

### Task 8: The SPA (Board, Review, Submit, History)

**Files:**
- Replace: `internal/webui/assets/index.html` (the real shell)
- Create: `internal/webui/assets/app.js`
- Create: `internal/webui/assets/style.css`

**Interfaces:**
- Consumes (all from earlier tasks): `GET /api/tasks`, `GET /api/pending`, `GET /api/diff/{id}`, `GET /api/logs/{id}`, `GET /api/widen/{id}`, `GET /api/history`, `POST /api/approve|deny|kill/{id}`, `POST /api/submit`. Token via `Authorization: Bearer` from the `#t=` fragment. TaskState JSON: `{id, repo, instruction, stage, started_at, egress_extra}`; stages `awaiting_egress|running|awaiting_approval|pushing`.
- Produces: no Go interface; verified by the server tests + manual smoke. Keep JS logic-light per the spec.

This is one task because the SPA is a single cohesive deliverable whose parts share the fetch wrapper, poll loop, and render shell. It has no Go test harness; correctness of the non-obvious helpers (token capture, auth header, optimistic update, diff coloring) is established by code review + a manual smoke against a running brokerd.

- [ ] **Step 1: Write `internal/webui/assets/index.html`:**

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="referrer" content="no-referrer">
  <title>drydock</title>
  <link rel="stylesheet" href="style.css">
</head>
<body>
  <header>
    <span class="brand">drydock</span>
    <nav>
      <button data-view="board" class="active">Board</button>
      <button data-view="submit">Submit</button>
      <button data-view="history">History</button>
    </nav>
    <span id="conn" class="conn"></span>
  </header>
  <main id="app"></main>
  <script src="app.js"></script>
</body>
</html>
```

Note the `<meta name="referrer" content="no-referrer">` (defense-in-depth for the fragment token).

- [ ] **Step 2: Write `internal/webui/assets/app.js`.** Full file:

```js
"use strict";

// --- token: captured from the URL fragment (#t=...), then stripped so it
// never lingers in the visible URL / history entry. Held only in memory.
let TOKEN = "";
(function captureToken() {
  const m = location.hash.match(/(?:^|[#&])t=([0-9a-f]+)/);
  if (m) {
    TOKEN = m[1];
    history.replaceState(null, "", location.pathname); // drop the fragment
  }
})();

// api wraps fetch with the bearer header and JSON/text handling. Every call
// sends the token; brokerd-down surfaces as a thrown Error the views render.
async function api(method, path, body) {
  const headers = {};
  if (TOKEN) headers["Authorization"] = "Bearer " + TOKEN;
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  return res;
}
async function apiJSON(path) {
  const res = await api("GET", path);
  if (!res.ok) throw new Error(`${res.status}`);
  return res.json();
}

// --- tiny DOM helpers
function el(tag, attrs = {}, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") e.className = v;
    else if (k === "onclick") e.onclick = v;
    else if (k === "text") e.textContent = v;
    else e.setAttribute(k, v);
  }
  for (const kid of kids) e.append(kid);
  return e;
}
function fmtAgeFromUnix(sec) { return fmtAge(Date.now() / 1000 - sec); }
function fmtAge(s) {
  s = Math.max(0, Math.floor(s));
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h";
  return Math.floor(s / 86400) + "d";
}
function elapsed(startedAt) { return fmtAge((Date.now() - new Date(startedAt).getTime()) / 1000); }
function fmtDurMs(ms) { return ms >= 1000 ? Math.round(ms / 1000) + "s" : ms + "ms"; }

// --- view router
const views = {};
let currentView = "board";
function show(view) {
  currentView = view;
  for (const b of document.querySelectorAll("nav button")) b.classList.toggle("active", b.dataset.view === view);
  views[view]();
}
document.querySelectorAll("nav button").forEach(b => (b.onclick = () => show(b.dataset.view)));

const app = () => document.getElementById("app");
function setConn(text, ok) {
  const c = document.getElementById("conn");
  c.textContent = text;
  c.className = "conn " + (ok ? "ok" : "bad");
}

// =================== BOARD ===================
let pollTimer = null;
let pinnedUntil = {}; // id -> timestamp; pin a card's position briefly after hover

async function renderBoard() {
  let tasks;
  try {
    tasks = await apiJSON("/api/tasks");
    setConn("brokerd connected", true);
  } catch (e) {
    setConn("brokerd not running — run `drydock start`", false);
    app().replaceChildren(el("p", { class: "empty", text: "brokerd is not running. Start it with `drydock start`, then this board will populate." }));
    scheduleBoardPoll(tasks);
    return;
  }
  // gate tasks float to top, but a recently-hovered card holds its slot ~2s.
  const now = Date.now();
  const gateRank = s => (s === "awaiting_approval" || s === "awaiting_egress" ? 0 : 1);
  tasks.sort((a, b) => {
    const pa = pinnedUntil[a.id] > now ? 1 : 0, pb = pinnedUntil[b.id] > now ? 1 : 0;
    if (pa !== pb) return 0; // pinned: leave relative order
    return gateRank(a.stage) - gateRank(b.stage);
  });

  const container = el("div", { class: "board" });
  if (tasks.length === 0) {
    container.append(el("p", { class: "empty" }, "No tasks running. ", el("a", { href: "#", onclick: () => show("submit") }, "Submit one"), " or see ", el("a", { href: "#", onclick: () => show("history") }, "History"), "."));
  }
  for (const t of tasks) container.append(taskCard(t));

  // "Just finished" strip: show the most recent completed tasks so a task that
  // leaves the live set (its most interesting moment) doesn't silently vanish.
  const liveIDs = new Set(tasks.map(t => t.id));
  try {
    const hist = await apiJSON("/api/history");
    const recent = hist.filter(h => !liveIDs.has(h.id)).slice(0, 5);
    if (recent.length) {
      const strip = el("div", { class: "recent" }, el("div", { class: "recent-title", text: "Just finished" }));
      for (const it of recent) {
        strip.append(el("div", { class: "recent-row hrow", onclick: () => openReview(it.id) },
          el("code", { text: it.id.slice(0, 12) }),
          el("span", { class: "age", text: fmtAgeFromUnix(it.mtime_unix) }),
          el("span", { text: it.cost }),
          el("span", { class: "outcome", text: it.outcome })));
      }
      container.append(strip);
    }
  } catch (_) { /* history is best-effort on the board */ }

  app().replaceChildren(container);
  scheduleBoardPoll(tasks);
}

function scheduleBoardPoll(tasks) {
  if (pollTimer) clearTimeout(pollTimer);
  if (currentView !== "board") return;
  const atGate = Array.isArray(tasks) && tasks.some(t => t.stage === "awaiting_approval" || t.stage === "awaiting_egress");
  pollTimer = setTimeout(renderBoard, atGate ? 500 : 1500); // faster while a gate is open
}

function stageBadge(stage) {
  const label = { awaiting_egress: "egress?", running: "running", awaiting_approval: "review?", pushing: "pushing" }[stage] || stage;
  return el("span", { class: "badge stage-" + stage, text: label });
}

function taskCard(t) {
  const card = el("div", { class: "card" });
  card.onmouseenter = () => { pinnedUntil[t.id] = Date.now() + 2000; };
  const head = el("div", { class: "card-head" },
    el("code", { class: "tid", title: "click to copy", onclick: () => navigator.clipboard && navigator.clipboard.writeText(t.id), text: t.id.slice(0, 12) }),
    stageBadge(t.stage),
    el("span", { class: "age", text: elapsed(t.started_at) }));
  card.append(head);
  card.append(el("div", { class: "repo", text: shortRepo(t.repo) }));
  card.append(el("div", { class: "instr", text: t.instruction || "" }));

  if (t.stage === "awaiting_egress") card.append(egressGate(t));
  else if (t.stage === "awaiting_approval") card.append(pushGate(t));
  if (t.stage === "running" || t.stage === "pushing" || t.stage === "awaiting_egress" || t.stage === "awaiting_approval") {
    card.append(el("div", { class: "actions" }, dangerButton("Kill", () => act("kill", t.id))));
  }
  return card;
}

function shortRepo(r) { const i = r.lastIndexOf(":"); return i >= 0 ? r.slice(i + 1) : r; }

// egress gate: show the requested hosts (from the persisted widen file) PLUS the
// instruction/repo so the operator can judge WHY the host was requested.
function egressGate(t) {
  const box = el("div", { class: "gate egress" }, el("div", { class: "gate-title", text: "Egress widening requested" }));
  apiJSON("/api/widen/" + t.id).then(domains => {
    box.append(el("ul", {}, ...domains.map(d => el("li", { text: d.host + ":" + (d.ports || []).join(",") }))));
  }).catch(() => box.append(el("p", { class: "muted", text: "(host list unavailable)" })));
  box.append(el("div", { class: "actions" },
    el("button", { class: "ok", onclick: () => act("approve", t.id) }, "Approve egress"),
    dangerButton("Deny", () => act("deny", t.id))));
  return box;
}

// push gate: cost + budget, open the diff to review, approve only after viewing.
function pushGate(t) {
  const box = el("div", { class: "gate push" }, el("div", { class: "gate-title", text: "Push awaiting review" }));
  const cost = el("span", { class: "cost", text: "spent: …" });
  box.append(cost);
  spentSoFar(t.id).then(s => (cost.textContent = s));
  const approveBtn = el("button", { class: "ok", disabled: "", onclick: () => act("approve", t.id) }, "Approve push");
  box.append(el("div", { class: "actions" },
    el("button", { onclick: () => { openReview(t.id); approveBtn.removeAttribute("disabled"); } }, "Review diff"),
    approveBtn,
    dangerButton("Deny", () => act("deny", t.id))));
  return box;
}

// spentSoFar parses the latest total_cost_usd from the live jsonl (best-effort).
async function spentSoFar(id) {
  try {
    const res = await api("GET", "/api/logs/" + id);
    const text = await res.text();
    let cost = null;
    for (const line of text.split("\n")) {
      const i = line.indexOf('"total_cost_usd"');
      if (i >= 0) { try { const o = JSON.parse(line); if (typeof o.total_cost_usd === "number") cost = o.total_cost_usd; } catch (_) {} }
    }
    return cost === null ? "spent: (not reported)" : "spent: $" + cost.toFixed(4);
  } catch (_) { return "spent: —"; }
}

// act performs approve/deny/kill with optimistic feedback + immediate re-poll.
// deny/kill are destructive, so they confirm first.
async function act(verb, id) {
  if ((verb === "deny" || verb === "kill") && !confirm(`${verb} task ${id.slice(0, 12)}? This cannot be undone.`)) return;
  try {
    const res = await api("POST", "/api/" + verb + "/" + id);
    if (res.status === 409 || res.status === 404) { /* already resolved — just refresh */ }
    else if (!res.ok) alert(`${verb} failed: HTTP ${res.status}`);
  } catch (e) { alert(`${verb} failed: ${e.message}`); }
  renderBoard(); // immediate re-poll (don't wait the interval)
}

function dangerButton(label, fn) { return el("button", { class: "danger", onclick: fn }, label); }
views.board = renderBoard;

// =================== REVIEW (diff overlay) ===================
async function openReview(id) {
  const overlay = el("div", { class: "overlay" });
  const panel = el("div", { class: "panel" });
  panel.append(el("div", { class: "panel-head" },
    el("strong", { text: "Review " + id.slice(0, 12) }),
    el("button", { class: "close", onclick: () => overlay.remove() }, "✕")));
  const diffBox = el("div", { class: "diff", text: "loading diff…" });
  panel.append(diffBox);
  panel.append(el("div", { class: "actions" },
    el("button", { class: "ok", onclick: () => { act("approve", id); overlay.remove(); } }, "Approve push"),
    dangerButton("Deny", () => { act("deny", id); overlay.remove(); })));
  overlay.append(panel);
  document.body.append(overlay);
  try {
    const res = await api("GET", "/api/diff/" + id);
    if (res.status === 404) { diffBox.textContent = "no diff yet"; return; }
    renderDiff(diffBox, await res.text());
  } catch (_) { diffBox.textContent = "could not load diff"; }
}

// renderDiff colors a unified diff line-by-line (+ add, - del, @@ hunk, file
// headers) and shows a +X/-Y summary. No syntax highlighting (per spec).
function renderDiff(box, text) {
  box.replaceChildren();
  let add = 0, del = 0;
  const pre = el("pre", { class: "diff-pre" });
  for (const line of text.split("\n")) {
    let cls = "ctx";
    if (line.startsWith("+++") || line.startsWith("---") || line.startsWith("diff ") || line.startsWith("index ")) cls = "fhead";
    else if (line.startsWith("@@")) cls = "hunk";
    else if (line.startsWith("+")) { cls = "add"; add++; }
    else if (line.startsWith("-")) { cls = "del"; del++; }
    pre.append(el("span", { class: "dl " + cls, text: line + "\n" }));
  }
  box.append(el("div", { class: "diffstat", text: `+${add} −${del}` }), pre);
}

// =================== SUBMIT ===================
const AGENTS = ["claude", "codex", "opencode"];
function renderSubmit() {
  if (pollTimer) clearTimeout(pollTimer);
  const form = el("form", { class: "submit-form" });
  const repo = el("input", { type: "text", placeholder: "https://github.com/owner/repo.git", required: "" });
  const instr = el("textarea", { placeholder: "What should the agent do?", rows: "4", required: "" });
  const agent = el("select", {});
  for (const a of AGENTS) agent.append(el("option", { value: a, text: a }));
  const model = el("input", { type: "text", placeholder: "model (optional)" });
  const msg = el("div", { class: "msg" });
  form.append(
    label("Repo URL (https/git/ssh — no local paths)", repo),
    label("Instruction", instr),
    label("Agent", agent),
    label("Model", model),
    el("button", { type: "submit", class: "ok" }, "Submit task"),
    msg);
  form.onsubmit = async (e) => {
    e.preventDefault();
    msg.textContent = "submitting…";
    const body = { repo_ref: repo.value.trim(), instruction: instr.value, agent: agent.value };
    if (model.value.trim()) body.model = model.value.trim();
    try {
      const res = await api("POST", "/api/submit", body);
      const txt = await res.text();
      if (!res.ok) { msg.textContent = "error: " + txt; return; }
      const { id } = JSON.parse(txt);
      newTaskID = id;
      show("board");
    } catch (e) { msg.textContent = "error: " + e.message; }
  };
  app().replaceChildren(el("h2", { text: "Submit a task" }), form);
}
let newTaskID = null;
function label(text, input) { return el("label", {}, el("span", { text }), input); }
views.submit = renderSubmit;

// =================== HISTORY ===================
async function renderHistory() {
  if (pollTimer) clearTimeout(pollTimer);
  let items;
  try { items = await apiJSON("/api/history"); }
  catch (e) { app().replaceChildren(el("p", { class: "empty", text: "could not load history" })); return; }
  const table = el("table", { class: "history" });
  table.append(el("tr", {}, ...["ID", "AGE", "DUR", "COST", "OUTCOME"].map(h => el("th", { text: h }))));
  if (items.length === 0) table.append(el("tr", {}, el("td", { colspan: "5", text: "(no tasks yet)" })));
  for (const it of items) {
    const row = el("tr", { class: "hrow" },
      el("td", {}, el("code", { onclick: () => navigator.clipboard && navigator.clipboard.writeText(it.id), title: "copy", text: it.id.slice(0, 12) })),
      el("td", { text: fmtAgeFromUnix(it.mtime_unix) }),
      el("td", { text: it.has_duration ? fmtDurMs(it.duration_ms) : "-" }),
      el("td", { text: it.cost }),
      el("td", { text: it.outcome }));
    row.onclick = () => openReview(it.id); // read-only diff/logs for past tasks
    table.append(row);
  }
  app().replaceChildren(el("h2", { text: "History" }), table);
}
views.history = renderHistory;

// boot
show("board");
```

- [ ] **Step 3: Write `internal/webui/assets/style.css`.** A small, readable stylesheet (no framework):

```css
:root { --bg:#0f1115; --fg:#e6e6e6; --muted:#8a8f98; --card:#181b21; --line:#2a2e37; --ok:#2ea043; --danger:#d1242f; --warn:#d29922; }
* { box-sizing: border-box; }
body { margin:0; font:14px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace; background:var(--bg); color:var(--fg); }
header { display:flex; align-items:center; gap:16px; padding:10px 16px; border-bottom:1px solid var(--line); }
.brand { font-weight:700; }
nav button { background:none; border:none; color:var(--muted); cursor:pointer; font:inherit; padding:4px 8px; }
nav button.active { color:var(--fg); border-bottom:2px solid var(--ok); }
.conn { margin-left:auto; font-size:12px; } .conn.ok { color:var(--ok); } .conn.bad { color:var(--danger); }
main { padding:16px; max-width:980px; margin:0 auto; }
.board { display:flex; flex-direction:column; gap:12px; }
.card { background:var(--card); border:1px solid var(--line); border-radius:8px; padding:12px; }
.card-head { display:flex; align-items:center; gap:10px; }
.tid { cursor:pointer; color:var(--muted); } .age { margin-left:auto; color:var(--muted); }
.repo { color:var(--fg); } .instr { color:var(--muted); white-space:pre-wrap; }
.badge { font-size:11px; padding:1px 6px; border-radius:10px; border:1px solid var(--line); }
.stage-awaiting_approval,.stage-awaiting_egress { color:var(--warn); border-color:var(--warn); }
.gate { margin-top:10px; padding:10px; border:1px dashed var(--warn); border-radius:6px; }
.gate-title { font-weight:700; margin-bottom:6px; } .cost { color:var(--warn); }
.actions { display:flex; gap:8px; margin-top:8px; }
button { background:var(--card); color:var(--fg); border:1px solid var(--line); border-radius:6px; padding:5px 10px; cursor:pointer; font:inherit; }
button.ok { border-color:var(--ok); color:var(--ok); } button.danger { border-color:var(--danger); color:var(--danger); }
button[disabled] { opacity:.5; cursor:not-allowed; }
.empty { color:var(--muted); } .muted { color:var(--muted); } a { color:var(--ok); }
.overlay { position:fixed; inset:0; background:rgba(0,0,0,.6); display:flex; justify-content:center; align-items:flex-start; padding:5vh 16px; }
.panel { background:var(--card); border:1px solid var(--line); border-radius:8px; width:min(900px,100%); max-height:90vh; overflow:auto; padding:14px; }
.panel-head { display:flex; justify-content:space-between; align-items:center; margin-bottom:8px; }
.diffstat { color:var(--muted); margin-bottom:6px; }
.diff-pre { margin:0; white-space:pre; overflow-x:auto; }
.dl { display:block; } .dl.add { color:var(--ok); } .dl.del { color:var(--danger); } .dl.hunk { color:var(--warn); } .dl.fhead { color:#79c0ff; } .dl.ctx { color:var(--fg); }
.submit-form { display:flex; flex-direction:column; gap:12px; max-width:560px; }
.submit-form label { display:flex; flex-direction:column; gap:4px; }
input,textarea,select { background:var(--bg); color:var(--fg); border:1px solid var(--line); border-radius:6px; padding:7px; font:inherit; }
.msg { color:var(--warn); white-space:pre-wrap; }
table.history { width:100%; border-collapse:collapse; } .history th,.history td { text-align:left; padding:6px 8px; border-bottom:1px solid var(--line); }
.hrow { cursor:pointer; } .hrow:hover { background:var(--card); }
.recent { margin-top:18px; border-top:1px solid var(--line); padding-top:10px; }
.recent-title { color:var(--muted); font-size:12px; margin-bottom:6px; }
.recent-row { display:flex; gap:12px; align-items:center; padding:4px 0; color:var(--muted); }
.recent-row .outcome { margin-left:auto; color:var(--fg); }
```

- [ ] **Step 4: Build + manual smoke.** There is no JS test harness; verify by building and eyeballing against a running brokerd.

Run: `go build ./... && go vet ./internal/webui/ && go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./internal/webui/...`
Expected: builds; the embedded assets compile in.
Manual smoke (if `drydock start` + a task is available): `./drydock ui --open`, confirm the Board renders, a task at the push gate shows the diff overlay and spent figure, Approve/Deny/Kill work, Submit posts, History lists past tasks. Confirm the URL bar has no `#t=` after load (token stripped).

- [ ] **Step 5: Commit**

```bash
git add internal/webui/assets/
git commit -m "webui: the SPA — board, diff review, submit, history (vanilla, embedded)"
```

---

### Task 9: Discoverability — surface `drydock ui`

**Files:**
- Modify: `cmd/drydock/init.go` (the "ready. next:" block).
- Modify: `cmd/drydock/status.go` (a UI hint line).
- Modify: `cmd/drydock/client.go` (`listPending` — suggest the UI when a task is at a gate).

**Interfaces:** none new; text additions only.

- [ ] **Step 1: Add the init hint.** In `cmd/drydock/init.go`'s final block (after the existing numbered "next:" lines), append:

```go
	fmt.Println("  5. drydock ui                          (optional: browser dashboard for review/submit)")
```

- [ ] **Step 2: Add the status hint.** In `cmd/drydock/status.go`, after the existing status output, add a single line:

```go
	fmt.Println("tip: `drydock ui` opens a local web dashboard (review diffs, submit, history).")
```

- [ ] **Step 3: Add the pending hint.** In `cmd/drydock/client.go`'s `listPending`, after the table prints (only when there is ≥1 pending task), add:

```go
	fmt.Println("\ntip: `drydock ui` reviews these diffs in a browser.")
```

(Place it inside the function after the loop that prints rows, guarded by the existing `len(pending) != 0` path — i.e. not when "(no pending tasks)" was printed.)

- [ ] **Step 4: Build + run the CLI suite.** These are print-only changes; if `init_test.go`/`status`/`pending` tests assert exact output, update those assertions.

Run: `go build ./... && go test ./cmd/drydock/ -count=1 && go vet ./...`
Expected: PASS (update any output-matching test to include the new hint lines).

- [ ] **Step 5: Commit**

```bash
git add cmd/drydock/init.go cmd/drydock/status.go cmd/drydock/client.go cmd/drydock/init_test.go
git commit -m "cli: surface `drydock ui` in init/status/pending hints"
```

---

## Final verification (after all tasks)

- [ ] `go build ./...` clean.
- [ ] `go test ./... -count=1` green.
- [ ] `go vet ./...` and `go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...` clean (CI gate).
- [ ] `make redteam` still green (the brokerd detach must not weaken containment).
- [ ] Manual end-to-end: `drydock start`, submit a task (CLI or UI), open `drydock ui`, watch it run, review the diff with the spent figure, approve, confirm it pushes; verify closing the UI does NOT kill an in-flight task (the detach); verify `--no-token` prints the warning and `#t=` is stripped from the URL after load.
