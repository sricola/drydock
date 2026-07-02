# Gemini Native Vendor Spike — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prove (or disprove) that a pinned `@google/gemini-cli` can be brokered through the drydock gateway in API-key mode — via a tag-gated Go test that runs the real CLI against a fake gateway and records the traffic shapes.

**Architecture:** One build-tagged Go test in `tests/integration/` stands up an `httptest` fake gateway, points the Gemini CLI at it with a sentinel API key, runs one headless prompt, and asserts the go/no-go gate while logging every captured request/response shape. A Makefile target runs it. Findings + verdict are authored by hand from the test's logged output.

**Tech Stack:** Go 1.26 stdlib (`net/http/httptest`, `os/exec`), `@google/gemini-cli` (Node, host-installed), Make.

## Global Constraints

- Governing spec: `docs/superpowers/specs/2026-07-02-gemini-native-vendor-spike-design.md`. Every task's requirements include it.
- **No live endpoint, no real key.** The test uses an `httptest.Server` and a `SENTINEL-<random>` key only. It must never call `generativelanguage.googleapis.com` or use a real credential.
- **Tag-gated, out of CI.** Build tag `//go:build geminispike`; the file lives in `tests/integration/` with `package integration` (matching `brokerd_test.go`). CI runs only untagged tests, so this never runs there.
- **No production code.** This phase adds only the test file, a Makefile target, and (in Task 2) a findings doc. Zero changes to `internal/`, `cmd/`, `image/`, config, or the registry.
- `go build ./...` stays green; `go vet -tags geminispike ./tests/integration/` compiles clean; `gofmt` clean; no new Go deps.
- Commit trailer on every commit:
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`
  `Claude-Session: https://claude.ai/code/session_01X4BM2LKqjhHgqvaK2JnwaR`

---

## Task 1: The spike harness (fake gateway + CLI exec + go/no-go gate)

**Files:**
- Create: `tests/integration/gemini_spike_test.go`
- Modify: `Makefile` (add a `test-gemini-spike` target)

**Interfaces:**
- Consumes: nothing from the drydock module (pure stdlib + the external `gemini` binary).
- Produces: a `go test -tags geminispike ./tests/integration/ -run TestGeminiBrokering_Spike` entry point that skips cleanly when `gemini` is absent, and otherwise runs the CLI against the fake gateway and emits the gate verdict + captured shapes.

Note on TDD for a spike: the "assertion under test" is the external CLI's behavior, which we cannot pre-implement. So Task 1's deliverable is the *harness that compiles and skips correctly*; Task 2 is where the real CLI exercises it. Verify Task 1 by (a) `go vet -tags geminispike` compiling clean and (b) the test SKIPPING when `gemini` is not installed.

- [ ] **Step 1: Write the harness test file**

Create `tests/integration/gemini_spike_test.go`:

```go
//go:build geminispike

// Spike: does @google/gemini-cli route through a custom base URL in API-key
// mode with a strippable auth header, so the drydock gateway can broker it?
// Uses a fake httptest gateway and a SENTINEL key — no live endpoint, no real
// credential. See docs/superpowers/specs/2026-07-02-gemini-native-vendor-spike-design.md.
package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type capturedReq struct {
	Method string
	Path   string // URL.Path + "?" + RawQuery
	Header http.Header
	Body   string
}

func randHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

func geminiVersion(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("gemini", "--version").CombinedOutput()
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	return strings.TrimSpace(string(out))
}

func TestGeminiBrokering_Spike(t *testing.T) {
	if _, err := exec.LookPath("gemini"); err != nil {
		t.Skip("gemini CLI not installed; `npm i -g @google/gemini-cli` then rerun `make test-gemini-spike`")
	}

	// Minimal valid generateContent response with usageMetadata so the CLI
	// treats the turn as successful.
	resp := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": "ok"}}},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount": 3, "candidatesTokenCount": 1, "totalTokenCount": 4,
		},
		"modelVersion": "gemini-2.5-flash",
	}
	respBytes, _ := json.Marshal(resp)

	var (
		mu   sync.Mutex
		reqs []capturedReq
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, capturedReq{
			Method: r.Method,
			Path:   r.URL.Path + "?" + r.URL.RawQuery,
			Header: r.Header.Clone(),
			Body:   string(body),
		})
		mu.Unlock()
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: " + string(respBytes) + "\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(respBytes)
	}))
	defer srv.Close()

	sentinel := "SENTINEL-" + randHex(t)
	home := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Candidate headless invocation; Task 2 confirms/adjusts the flag.
	cmd := exec.CommandContext(ctx, "gemini", "-p", "say ok")
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"GOOGLE_GEMINI_BASE_URL="+srv.URL, // primary override
		"GEMINI_BASEURL="+srv.URL,         // belt-and-suspenders alternate name
		"GEMINI_API_KEY="+sentinel,
		"GEMINI_DIR="+filepath.Join(home, ".gemini"),
		"GOOGLE_GENAI_USE_GCA=",           // neutralize Cloud/Code-Assist auto-auth
		"GOOGLE_GENAI_USE_VERTEXAI=false", // force the Gemini API (not Vertex)
		"NO_COLOR=1",
		"TERM=dumb",
	)
	out, runErr := cmd.CombinedOutput()

	t.Logf("=== gemini version: %s", geminiVersion(t))
	t.Logf("=== invocation: gemini -p 'say ok' (exit err: %v)", runErr)
	t.Logf("=== stdout+stderr:\n%s", out)

	mu.Lock()
	defer mu.Unlock()
	t.Logf("=== captured %d request(s)", len(reqs))
	for i, rq := range reqs {
		t.Logf("[req %d] %s %s", i, rq.Method, rq.Path)
		t.Logf("[req %d] x-goog-api-key=%q authorization=%q key-in-query=%v",
			i, rq.Header.Get("x-goog-api-key"), rq.Header.Get("Authorization"),
			strings.Contains(rq.Path, "key="))
		t.Logf("[req %d] body: %s", i, rq.Body)
	}

	// --- GREEN gate (see spec §Go/No-Go) ---
	if len(reqs) == 0 {
		t.Fatalf("RED: gemini did not hit GOOGLE_GEMINI_BASE_URL — base-URL override not honored")
	}
	var brokered *capturedReq
	for i := range reqs {
		h := reqs[i].Header
		if h.Get("x-goog-api-key") == sentinel ||
			h.Get("Authorization") == "Bearer "+sentinel ||
			strings.Contains(reqs[i].Path, "key="+sentinel) {
			brokered = &reqs[i]
			break
		}
	}
	if brokered == nil {
		t.Fatalf("RED: sentinel key not found in any request header/query — gateway can't strip/replace it")
	}
	if runErr != nil {
		t.Fatalf("RED: gemini did not exit 0 in headless mode: %v", runErr)
	}
	t.Log("GREEN: base-URL honored, sentinel key isolated in a replaceable header, headless exit 0")
}
```

- [ ] **Step 2: Verify the harness compiles under the tag**

Run: `go vet -tags geminispike ./tests/integration/`
Expected: no output (compiles clean).

- [ ] **Step 3: Verify it SKIPS when gemini is absent** (in CI/dev without the CLI)

Run: `go test -tags geminispike ./tests/integration/ -run TestGeminiBrokering_Spike -v` (on a host without `gemini` on PATH)
Expected: `--- SKIP: TestGeminiBrokering_Spike` with the "gemini CLI not installed" message. (If `gemini` happens to be installed, the test will actually run — that's Task 2's job; for Task 1 verification on a clean host, SKIP is the pass condition.)

- [ ] **Step 4: Add the Makefile target**

Add to `Makefile` (match the existing target + help-comment style; place near `test-integration`):

```make
test-gemini-spike: ## Run the Gemini brokering feasibility spike (needs `npm i -g @google/gemini-cli`)
	go test -tags=geminispike -count=1 -timeout=2m ./tests/integration/ -run TestGeminiBrokering_Spike -v
```

If the Makefile uses a `.PHONY` block, add `test-gemini-spike` to it.

- [ ] **Step 5: Verify the target is wired**

Run: `make help | grep gemini` (or `grep -n test-gemini-spike Makefile`)
Expected: the target and its help text appear.

- [ ] **Step 6: Confirm no production impact**

Run: `go build ./... && go test ./... -count=1 2>&1 | tail -3` and `gofmt -l tests/integration/gemini_spike_test.go`
Expected: all packages green (the untagged build ignores the spike file); gofmt prints nothing.

- [ ] **Step 7: Commit**

```bash
git add tests/integration/gemini_spike_test.go Makefile
git commit -m "test(spike): Gemini brokering feasibility harness (fake gateway, tag-gated)"
```

---

## Task 2: Run the spike, capture findings, record the verdict

This task runs the real CLI. It is exploratory: the invocation flag and the
Cloud-auth-neutralizing env in Task 1 are best-guess starting points; adjust
them here based on what the CLI actually does, then record the truth.

**Files:**
- Modify (only if needed): `tests/integration/gemini_spike_test.go` (correct the invocation flags / env discovered to be necessary)
- Create: `docs/superpowers/specs/2026-07-02-gemini-spike-findings.md`

**Interfaces:**
- Consumes: the Task 1 harness.
- Produces: a committed findings doc ending in a `VERDICT: GREEN | RED` line — the go/no-go input to the native-vendor build spec.

- [ ] **Step 1: Install the CLI and record the version**

Run: `npm i -g @google/gemini-cli && gemini --version`
Record the exact version string; the build phase will pin it.
(If npm global install is not permitted in the environment, report BLOCKED with the constraint — the controller will advise on running the spike where the CLI can be installed.)

- [ ] **Step 2: Run the spike**

Run: `make test-gemini-spike`
Read the full `-v` output: the captured request path(s), which header carried the sentinel, the request body shape, the stdout shape, and the `usageMetadata` fields.

- [ ] **Step 3: If it fails on invocation/env (not on the gate), iterate**

If the failure is "the CLI waited for a TTY / didn't accept `-p`" or "it still chose Cloud auth", consult `gemini --help` and the CLI docs, adjust the invocation flag and/or the auth-neutralizing env in `gemini_spike_test.go`, and rerun. Distinguish a *harness* problem (fixable here) from a *real RED* (the CLI cannot be brokered — e.g. it ignores the base URL by design, or embeds the key un-interceptably). Record which it is.

- [ ] **Step 4: Author the findings doc**

Create `docs/superpowers/specs/2026-07-02-gemini-spike-findings.md` from the logged output:

```markdown
# Gemini Brokering Spike — Findings (2026-07-02)

**CLI version validated:** @google/gemini-cli <version>
**Working headless invocation:** `gemini <flags>`
**Auth-neutralizing env required:** <the env that forced API-key mode>

## Captured shapes
- Auth header carrying the key: `<x-goog-api-key | Authorization | key= query>`
- Request path: `<e.g. /v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse>`
- Request body (abridged): <shape>
- Response usageMetadata: `promptTokenCount` / `candidatesTokenCount` / `totalTokenCount` (confirm exact names)
- CLI stdout shape (headless): <what it prints>

## Assessment against the five questions
1. Base-URL honored: <yes/no + detail>
2. Brokerable auth (strippable header): <yes/no + which header>
3. Headless exit 0: <yes/no>
4. Output shape usable by the runner: <notes>
5. usageMetadata shape captured: <yes + field names>

## VERDICT: GREEN | RED
<one paragraph: if GREEN, the native GoogleVendor build proceeds against these
shapes. If RED, the specific blocker and why native Gemini isn't brokerable
with this CLI; Gemini stays on the openai-compat lane and 3B closes as
not-natively-feasible.>
```

- [ ] **Step 5: Final verification + commit**

Run: `go build ./... && go test ./... -count=1 2>&1 | tail -3` (production still green), `go vet -tags geminispike ./tests/integration/` (harness still compiles after any edits), `gofmt -l tests/integration/gemini_spike_test.go`.

```bash
git add tests/integration/gemini_spike_test.go docs/superpowers/specs/2026-07-02-gemini-spike-findings.md
git commit -m "spike(gemini): record brokering feasibility findings + verdict"
```

---

## Self-review notes

- Spec coverage: the five feasibility questions (§) → Task 1 harness asserts Q1–Q3 (gate) and logs Q4–Q5 (recordings); Task 2 records all five into the findings doc with a verdict. Safety constraint (no live endpoint / no real key) → fake `httptest` server + sentinel. Artifact/lifecycle → Task 2 findings doc; harness kept tag-gated (safe, no real calls). Go/no-go gate → Task 1 GREEN gate + Task 2 verdict.
- The invocation flag (`gemini -p`) and Cloud-auth-neutralizing env are explicitly flagged as spike-discovered and adjustable in Task 2 — this is honest for a spike, not a placeholder in the "TBD" sense; the harness has a concrete starting point that compiles and runs.
- No production code, no new deps, tag-gated out of CI — matches the spec's out-of-scope list.
