# Crash-recovery reconciliation — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a `brokerd` restart after a crash leave no orphaned stage dir and no audit row stuck at `running?`, and refuse to start a second daemon that would clobber a live task.

**Architecture:** Boot-time only. A single-instance `flock` is acquired before any reaping (making "no task is live at boot" true by construction); then the existing boot reconciler `pruneOrphanTasks()` — which already reaps orphan `task-*` VMs and squid — gains two more sweeps: remove stale stage dirs, and append a synthetic `interrupted` result line to traces a crash left unterminated. Each sweep is a pure, independently-tested function in the package that already owns that knowledge.

**Tech Stack:** Go 1.26.4, standard library only (`syscall.Flock`, `os`, `encoding/json`). macOS / Apple-silicon (`container` CLI), darwin `flock` semantics.

## Global Constraints

- **No new dependencies.** Standard library only. `syscall` is already imported in `cmd/brokerd/main.go`.
- **Go 1.26.4**; every task ends green on `go build ./... && go vet ./... && go test ./...` with `gofmt` clean.
- **Boot-only safety invariant:** the sweeps `RemoveAll` and rewrite files, so they run only at brokerd startup, after the single-instance lock is held, when no task is live.
- **Best-effort, log-and-continue:** a per-entry failure in a sweep is collected as the first error and skipped, never aborting boot — matching the existing container reaper's style.
- **Audit result-line contract (verbatim):** the synthetic interrupted line is
  `{"type":"result","subtype":"interrupted","is_error":true,"duration_ms":0,"total_cost_usd":0,"num_turns":0}`
  — `subtype:"interrupted"` (distinct from `"error"`), `is_error:true`, `duration_ms:0`.
- **Result-line detection must PARSE, not substring-match** — `json.Unmarshal` each line and check decoded `type == "result"` (mirrors `cmd/drydock/tasks.go`'s `lastResult`), so a stream event whose text payload contains the literal `"type":"result"` is never mistaken for a real result.
- **`flock` descriptor must stay open for the process's whole life** (a package var); closing it drops the lock.

---

### Task 1: Single-instance boot lock

**Files:**
- Modify: `internal/config/config.go` (add `LockPath()` after `Dir()`, ~line 130)
- Test: `internal/config/config_test.go` (add `TestLockPath`)
- Create: `cmd/brokerd/lock.go`
- Test: `cmd/brokerd/lock_test.go`
- Modify: `cmd/brokerd/main.go` (insert lock guard in `main()` between `checkContainerVersion(...)` and `pruneOrphanTasks()`, ~line 106-107; add package var)

**Interfaces:**
- Produces:
  - `config.LockPath() string` — returns `~/.drydock/brokerd.lock`, or `""` if home is unresolvable.
  - `acquireLock(path string) (*os.File, error)` (package `main`, `cmd/brokerd`) — creates the parent dir, opens+flocks the path; returns the held `*os.File` or `errLockHeld` on contention.
  - `errLockHeld` (package `main`) — sentinel returned when another process holds the lock.

- [ ] **Step 1: Write the failing test for `LockPath`**

Add to `internal/config/config_test.go` (imports `path/filepath` and `testing` are already present):

```go
func TestLockPath(t *testing.T) {
	got := LockPath()
	if got == "" {
		t.Skip("home dir unresolvable in this environment")
	}
	if !filepath.IsAbs(got) {
		t.Errorf("LockPath() = %q, want an absolute path", got)
	}
	if filepath.Base(got) != "brokerd.lock" {
		t.Errorf("LockPath() base = %q, want brokerd.lock", filepath.Base(got))
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/config/ -run TestLockPath`
Expected: FAIL — `undefined: LockPath`.

- [ ] **Step 3: Implement `LockPath`**

Add to `internal/config/config.go` immediately after the `Dir()` function (~line 130):

```go
// LockPath returns ~/.drydock/brokerd.lock — the single-instance lock brokerd
// flocks at boot so only one daemon runs per host. Empty if home is
// unresolvable (mirrors DefaultPath).
func LockPath() string {
	if d := Dir(); d != "" {
		return filepath.Join(d, "brokerd.lock")
	}
	return ""
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/config/ -run TestLockPath`
Expected: PASS.

- [ ] **Step 5: Write the failing tests for `acquireLock`**

Create `cmd/brokerd/lock_test.go`:

```go
package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A fresh install has no ~/.drydock yet — acquireLock must create the parent
// rather than failing on ENOENT.
func TestAcquireLock_CreatesParentAndLocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "brokerd.lock") // parent missing
	f, err := acquireLock(path)
	if err != nil {
		t.Fatalf("acquireLock on fresh path: %v", err)
	}
	defer f.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("lock file not created: %v", err)
	}
}

// flock attaches to the open file description, so a second open+lock of the
// same path — even in the same process — contends and returns errLockHeld,
// distinguishable from a generic open/mkdir failure.
func TestAcquireLock_SecondAcquireIsContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brokerd.lock")
	f1, err := acquireLock(path)
	if err != nil {
		t.Fatalf("first acquireLock: %v", err)
	}
	defer f1.Close()
	if _, err := acquireLock(path); !errors.Is(err, errLockHeld) {
		t.Errorf("second acquireLock err = %v, want errLockHeld", err)
	}
}
```

- [ ] **Step 6: Run them to verify they fail**

Run: `go test ./cmd/brokerd/ -run TestAcquireLock`
Expected: FAIL — `undefined: acquireLock` / `undefined: errLockHeld`.

- [ ] **Step 7: Implement `acquireLock`**

Create `cmd/brokerd/lock.go`:

```go
package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// errLockHeld is returned by acquireLock when another process already holds the
// single-instance lock (flock returned EWOULDBLOCK).
var errLockHeld = errors.New("brokerd lock held by another process")

// acquireLock takes an exclusive, non-blocking flock on path so only one
// brokerd runs per host. The returned *os.File MUST be kept open for the
// process's whole life — closing it drops the lock. The parent dir is created
// if absent (~/.drydock may not exist on a fresh start; the broker otherwise
// creates its state dirs lazily on the first task). Returns errLockHeld on
// contention, or the underlying error on any other failure.
func acquireLock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLockHeld
		}
		return nil, err
	}
	return f, nil
}
```

- [ ] **Step 8: Run the lock tests to verify they pass**

Run: `go test ./cmd/brokerd/ -run TestAcquireLock`
Expected: PASS (both).

- [ ] **Step 9: Wire the guard into `main()`**

In `cmd/brokerd/main.go`, add a package-level var near the other top-level declarations (e.g. just above `func main()`):

```go
// brokerdLock holds the single-instance flock for the process's whole life.
// Kept in a package var so the descriptor isn't garbage-collected/closed —
// closing it would drop the lock.
var brokerdLock *os.File
```

Then in `main()`, insert between `checkContainerVersion(cfg.StrictContainerVersion)` and `pruneOrphanTasks(...)`:

```go
	// Single-instance: refuse to start (and run NO reaper) if another brokerd
	// is live, so its in-flight task's VM/stage/audit can't be clobbered.
	lf, err := acquireLock(config.LockPath())
	if err != nil {
		if errors.Is(err, errLockHeld) {
			die("another brokerd is already running on this host", "lock", config.LockPath())
		}
		die("cannot acquire brokerd lock", "lock", config.LockPath(), "err", err)
	}
	brokerdLock = lf
```

Add `"errors"` to the `cmd/brokerd/main.go` import block (alphabetically, after `"context"`).

- [ ] **Step 10: Build, vet, fmt, and run the package tests**

Run: `gofmt -w cmd/brokerd/main.go cmd/brokerd/lock.go internal/config/config.go && go build ./... && go vet ./... && go test ./internal/config/ ./cmd/brokerd/`
Expected: build/vet clean; both packages PASS.

- [ ] **Step 11: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go cmd/brokerd/lock.go cmd/brokerd/lock_test.go cmd/brokerd/main.go
git commit -m "brokerd: enforce single-instance with a boot flock"
```

---

### Task 2: `stage.ReapOrphans`

**Files:**
- Modify: `internal/stage/stage.go` (add `ReapOrphans` after `Cleanup`, end of file)
- Test: `internal/stage/stage_test.go`

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: `stage.ReapOrphans(root string) (int, error)` — removes every child directory under `root`; refuses an unsafe root; missing root is `(0, nil)`; returns count reaped + first error.

- [ ] **Step 1: Write the failing tests**

Add to `internal/stage/stage_test.go` (imports `os`, `path/filepath`, `testing` already present):

```go
func TestReapOrphans_RemovesChildDirsKeepsFiles(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"task-a", "task-b", "task-c"} {
		if err := os.MkdirAll(filepath.Join(root, d, "work"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stray := filepath.Join(root, "keep.txt")
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := ReapOrphans(root)
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if n != 3 {
		t.Errorf("reaped %d, want 3", n)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("stray file must survive: %v", err)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("dir %q should have been reaped", e.Name())
		}
	}
}

func TestReapOrphans_RefusesUnsafeRoot(t *testing.T) {
	for _, bad := range []string{"", "/", ".", "relative/path"} {
		if _, err := ReapOrphans(bad); err == nil {
			t.Errorf("ReapOrphans(%q) = nil error, want refusal", bad)
		}
	}
}

func TestReapOrphans_MissingRootIsNoop(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if n, err := ReapOrphans(missing); err != nil || n != 0 {
		t.Errorf("missing root = (%d,%v), want (0,nil)", n, err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/stage/ -run TestReapOrphans`
Expected: FAIL — `undefined: ReapOrphans`.

- [ ] **Step 3: Implement `ReapOrphans`**

Add to the end of `internal/stage/stage.go`:

```go
// ReapOrphans removes every child directory under root. Used at brokerd boot to
// clear stage dirs orphaned by a crash (the per-task Cleanup defer never ran).
// SAFE ONLY AT BOOT, when no task is live. Applies the same guard as Cleanup so
// a misconfigured (empty/relative/root-shaped) StageRoot can't widen the blast
// radius. A missing root is a no-op. Non-directory entries are left untouched.
// Returns the count of dirs reaped and the first error (per-entry errors are
// non-fatal and don't abort the sweep).
func ReapOrphans(root string) (int, error) {
	clean := filepath.Clean(root)
	if clean == "" || clean == "/" || clean == "." || !filepath.IsAbs(clean) {
		return 0, fmt.Errorf("stage: refusing to reap unsafe root %q", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	var firstErr error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if rerr := os.RemoveAll(filepath.Join(root, e.Name())); rerr != nil {
			if firstErr == nil {
				firstErr = rerr
			}
			continue
		}
		n++
	}
	return n, firstErr
}
```

- [ ] **Step 4: Run them to verify they pass**

Run: `go test ./internal/stage/ -run TestReapOrphans`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/stage/stage.go
git add internal/stage/stage.go internal/stage/stage_test.go
git commit -m "stage: ReapOrphans to clear crash-orphaned stage dirs at boot"
```

---

### Task 3: `broker.TerminateStuckAudits`

**Files:**
- Create: `internal/broker/reconcile.go`
- Test: `internal/broker/reconcile_test.go`

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: `broker.TerminateStuckAudits(auditRoot string) (int, error)` — appends the interrupted result line to each `*.jsonl` lacking a real result line; idempotent; missing root is `(0, nil)`; returns count terminated + first error. The appended line is exactly:
  `{"type":"result","subtype":"interrupted","is_error":true,"duration_ms":0,"total_cost_usd":0,"num_turns":0}`

- [ ] **Step 1: Write the failing tests**

Create `internal/broker/reconcile_test.go`:

```go
package broker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assertLastLineInterrupted(t *testing.T, path string) {
	t.Helper()
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	last := lines[len(lines)-1]
	var x struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal([]byte(last), &x); err != nil {
		t.Fatalf("last line not JSON: %q", last)
	}
	if x.Type != "result" || x.Subtype != "interrupted" || !x.IsError {
		t.Errorf("last line = %+v, want type=result subtype=interrupted is_error=true", x)
	}
}

func TestTerminateStuckAudits_AppendsInterruptedAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-a.jsonl")
	body := `{"type":"drydock_meta","subscription":false}` + "\n" +
		`{"type":"stream_event"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := TerminateStuckAudits(dir)
	if err != nil || n != 1 {
		t.Fatalf("first pass = (%d,%v), want (1,nil)", n, err)
	}
	assertLastLineInterrupted(t, path)

	// Second pass: the interrupted line is itself a result line → no-op.
	after1, _ := os.ReadFile(path)
	n2, _ := TerminateStuckAudits(dir)
	after2, _ := os.ReadFile(path)
	if n2 != 0 || string(after1) != string(after2) {
		t.Errorf("second pass modified the trace (n=%d)", n2)
	}
}

// Guards the detection rule: a stream event whose TEXT payload contains the
// literal `"type":"result"` must NOT be mistaken for a real result line —
// otherwise a genuinely-crashed task would be skipped and stay "running?".
func TestTerminateStuckAudits_SubstringInPayloadIsNotAResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-b.jsonl")
	body := `{"type":"stream_event","text":"emitted {\"type\":\"result\"} as text"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := TerminateStuckAudits(dir)
	if err != nil || n != 1 {
		t.Fatalf("got (%d,%v), want (1,nil) — substring must not count as a result", n, err)
	}
	assertLastLineInterrupted(t, path)
}

func TestTerminateStuckAudits_LeavesCompletedTraceUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-c.jsonl")
	body := `{"type":"stream_event"}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	n, err := TerminateStuckAudits(dir)
	if err != nil || n != 0 {
		t.Fatalf("completed trace = (%d,%v), want (0,nil)", n, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("completed trace modified:\nbefore=%q\nafter=%q", before, after)
	}
}

func TestTerminateStuckAudits_MissingRootIsNoop(t *testing.T) {
	if n, err := TerminateStuckAudits(filepath.Join(t.TempDir(), "nope")); err != nil || n != 0 {
		t.Errorf("missing root = (%d,%v), want (0,nil)", n, err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/broker/ -run TestTerminateStuckAudits`
Expected: FAIL — `undefined: TerminateStuckAudits`.

- [ ] **Step 3: Implement `TerminateStuckAudits`**

Create `internal/broker/reconcile.go`:

```go
package broker

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// interruptedResultLine is the synthetic terminal event appended to a task
// trace that a brokerd crash left without a result line. subtype "interrupted"
// (distinct from "error") tells `drydock tasks` the daemon died under the task
// rather than the task itself failing; duration_ms is 0 (death time unknown).
const interruptedResultLine = `{"type":"result","subtype":"interrupted","is_error":true,"duration_ms":0,"total_cost_usd":0,"num_turns":0}` + "\n"

// TerminateStuckAudits scans auditRoot for <id>.jsonl traces with no terminal
// result line — tasks that were running when a prior brokerd crashed — and
// appends a synthetic "interrupted" result so `drydock tasks` resolves them
// instead of showing "running?" forever. Idempotent: a trace that already has a
// result line is left untouched. SAFE ONLY AT BOOT, when no task is live.
// Returns the count terminated and the first error (per-file errors are
// non-fatal).
func TerminateStuckAudits(auditRoot string) (int, error) {
	entries, err := os.ReadDir(auditRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	var firstErr error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(auditRoot, e.Name())
		has, herr := hasResultLine(path)
		if herr != nil {
			if firstErr == nil {
				firstErr = herr
			}
			continue
		}
		if has {
			continue
		}
		if aerr := appendLine(path, interruptedResultLine); aerr != nil {
			if firstErr == nil {
				firstErr = aerr
			}
			continue
		}
		n++
	}
	return n, firstErr
}

// hasResultLine reports whether the file's tail contains a JSON line whose
// decoded "type" is "result". Mirrors cmd/drydock's lastResult: it PARSES each
// line rather than substring-matching, so a stream event whose text payload
// contains the literal `"type":"result"` is not mistaken for a real result. A
// completed task's result line is always last, hence always within the tail.
func hasResultLine(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	const tail = 16 * 1024
	if info.Size() > tail {
		if _, err := f.Seek(info.Size()-tail, io.SeekStart); err != nil {
			return false, err
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return false, err
	}
	for _, ln := range bytes.Split(data, []byte("\n")) {
		var x struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(ln, &x) == nil && x.Type == "result" {
			return true, nil
		}
	}
	return false, nil
}

// appendLine appends s to the file at path.
func appendLine(path, s string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(s)
	return err
}
```

- [ ] **Step 4: Run them to verify they pass**

Run: `go test ./internal/broker/ -run TestTerminateStuckAudits`
Expected: PASS (all four).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/broker/reconcile.go
git add internal/broker/reconcile.go internal/broker/reconcile_test.go
git commit -m "broker: TerminateStuckAudits resolves crash-stuck audit rows"
```

---

### Task 4: render `interrupted` in `drydock tasks`

**Files:**
- Modify: `cmd/drydock/tasks.go` (`summarize`, ~lines 125-142)
- Test: `cmd/drydock/tasks_test.go`

**Interfaces:**
- Consumes (from Task 3): an audit trace whose terminal line is
  `{"type":"result","subtype":"interrupted","is_error":true,"duration_ms":0,...}`.
- Produces: `summarize` renders `outcome == "interrupted"` and `dur == "-"` for such a trace.

- [ ] **Step 1: Write the failing test**

Add to `cmd/drydock/tasks_test.go`:

```go
// A brokerd crash leaves the boot reconciler's synthetic interrupted line.
// It must read as a distinct "interrupted" outcome (not "error"), and DUR
// must stay "-" (unknown) rather than the synthetic 0ms.
func TestSummarize_InterruptedShowsInterruptedAndUnknownDur(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-i.jsonl")
	body := `{"type":"drydock_meta","subscription":false}` + "\n" +
		`{"type":"result","subtype":"interrupted","is_error":true,"duration_ms":0,"total_cost_usd":0,"num_turns":0}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	got := summarize("task-i", path, info)
	if got.outcome != "interrupted" {
		t.Errorf("outcome = %q, want interrupted", got.outcome)
	}
	if got.dur != "-" {
		t.Errorf("dur = %q, want %q (unknown, not synthetic 0ms)", got.dur, "-")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cmd/drydock/ -run TestSummarize_InterruptedShows`
Expected: FAIL — outcome is `"error"` (the `IsError` case wins) and dur is `"0ms"`.

- [ ] **Step 3: Implement the render change**

In `cmd/drydock/tasks.go`, `summarize`, change the outcome `switch` (currently starting `case last.IsError:`) to add an `interrupted` case **first**. The full switch becomes:

```go
	switch {
	case last.Subtype == "interrupted":
		// brokerd died under the task (boot reconciler wrote this), distinct
		// from "error" (the task itself failing). Death time is unknown, so
		// keep the "-" placeholder rather than the synthetic 0ms.
		r.outcome = "interrupted"
		r.dur = "-"
	case last.IsError:
		r.outcome = "error"
	case last.Subtype == "success":
		if last.NumTurns > 0 {
			r.outcome = fmt.Sprintf("ok (%d turn)", last.NumTurns)
		} else {
			r.outcome = "ok"
		}
	default:
		r.outcome = last.Subtype
	}
```

(The `r.dur = shortDur(last.DurationMs)` line above the switch is unchanged; the `interrupted` case overrides it back to `"-"`. The trailing `if meta.Sensitive { r.outcome += " · sensitive" }` is unchanged, so an interrupted sensitive task reads `interrupted · sensitive`.)

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./cmd/drydock/ -run TestSummarize`
Expected: PASS (the new test and the existing `TestSummarize_*` all green).

- [ ] **Step 5: Commit**

```bash
gofmt -w cmd/drydock/tasks.go
git add cmd/drydock/tasks.go cmd/drydock/tasks_test.go
git commit -m "drydock tasks: render crash-interrupted runs as 'interrupted'"
```

---

### Task 5: wire the sweeps into the boot reconciler + update roadmap

**Files:**
- Modify: `cmd/brokerd/main.go` (`pruneOrphanTasks` signature + body, ~lines 467-487; call site ~line 107; add `stage` import)
- Modify: `docs/ROADMAP.md` (Phase 4.1 bullet)

**Interfaces:**
- Consumes: `stage.ReapOrphans(root)` (Task 2), `broker.TerminateStuckAudits(auditRoot)` (Task 3), and the lock guard from Task 1 (already acquired before this call).
- Produces: a fully wired boot reconciliation. No new exported symbols.

**Note:** there is no unit test for `pruneOrphanTasks` itself — it shells out to the real `container` CLI and lives in `package main`; the existing function is likewise untested, and its new sub-steps (`ReapOrphans`, `TerminateStuckAudits`) are fully covered by Tasks 2-3. The deliverable here is verified by `go build`/`go vet` plus the reasoning below, consistent with how the existing reaper is maintained.

- [ ] **Step 1: Change `pruneOrphanTasks` signature and body**

In `cmd/brokerd/main.go`, change `func pruneOrphanTasks() {` to `func pruneOrphanTasks(stageRoot, auditRoot string) {` and append the two sweeps after the existing squid-reap line (`_ = exec.Command("pkill", "-f", "squid -N -f").Run()`):

```go
	// Reap host-side leftovers a crash skipped (the per-task defers never ran).
	// ORDER MATTERS — do not reorder: the container delete above must precede
	// the stage reap, because a VM mounts its work tree out of the stage dir;
	// reaping first could RemoveAll a path a still-terminating VM holds. The
	// boot lock (see main) guarantees no other brokerd is concurrently running
	// a task here, so every leftover provably belongs to a dead prior life.
	if n, err := stage.ReapOrphans(stageRoot); err != nil {
		slog.Warn("orphan prune: stage reap refused", "err", err)
	} else if n > 0 {
		slog.Info("orphan prune: reaped stage dirs", "count", n)
	}
	if n, err := broker.TerminateStuckAudits(auditRoot); err != nil {
		slog.Warn("orphan prune: audit terminate error", "err", err)
	} else if n > 0 {
		slog.Info("orphan prune: terminated stuck audit rows", "count", n)
	}
```

- [ ] **Step 2: Update the call site and add the import**

Change the call at ~line 107 from `pruneOrphanTasks()` to:

```go
	pruneOrphanTasks(cfg.StageRoot, cfg.AuditRoot)
```

Add `"drydock/internal/stage"` to the `cmd/brokerd/main.go` import block (in the `drydock/internal/...` group, alphabetically after `"drydock/internal/sockpath"`). `"drydock/internal/broker"` is already imported.

- [ ] **Step 3: Build, vet, fmt**

Run: `gofmt -w cmd/brokerd/main.go && go build ./... && go vet ./...`
Expected: clean. (A leftover `pruneOrphanTasks()` call with no args, or an unused `stage` import, would fail the build — that's the guard that Steps 1-2 are consistent.)

- [ ] **Step 4: Update roadmap 4.1**

In `docs/ROADMAP.md`, replace the Phase 4.1 bullet (the one beginning `**4.1 Crash recovery.**`) with:

```markdown
- **4.1 Crash recovery.** *Landed.* A `brokerd` killed mid-task left host-side
  orphans the per-task cleanup defers never reaped. Boot reconciliation now:
  enforces single-instance via an `flock` (`~/.drydock/brokerd.lock`) so a
  second daemon can't clobber a live task; reaps orphan `task-*` VMs and squid
  (pre-existing); sweeps stale stage dirs; and resolves tasks a crash
  interrupted to a distinct `interrupted` outcome instead of a stuck
  `running?`. (The in-memory concurrency slot was never the gap — a restart
  rebuilds the semaphore at full capacity; the slot-pinning bug was the
  approval-gate timeout, fixed separately.)
```

- [ ] **Step 5: Full suite + commit**

Run: `gofmt -l internal/ cmd/ ; go build ./... && go vet ./... && go test ./...`
Expected: gofmt prints nothing; build/vet clean; all packages PASS.

```bash
git add cmd/brokerd/main.go docs/ROADMAP.md
git commit -m "brokerd: wire stage + audit sweeps into boot reconciler; roadmap 4.1"
```

---

## Self-Review

**Spec coverage:**
- Component 0 (single-instance lock) → Task 1 (`LockPath`, `acquireLock`, main guard, MkdirAll parent, EWOULDBLOCK-vs-other distinction). ✓
- Component 1 (`stage.ReapOrphans`, guard, missing-root no-op, count+firstErr) → Task 2. ✓
- Component 2 (`broker.TerminateStuckAudits`, parse-not-substring detection, idempotency, exact interrupted line) → Task 3. ✓
- Component 3 (`summarize` interrupted case before IsError; dur "-") → Task 4. ✓
- Component 4 (`pruneOrphanTasks` signature + call site + load-bearing ordering comment + slog) → Task 5. ✓
- Testing matrix (stray-file safety, unsafe-root refusal, R2 substring guard, idempotency, lock contention + fresh-parent, interrupted render) → distributed across Tasks 1-4. ✓
- Done-when (no orphan stage dir, no stuck row, second brokerd refuses, roadmap 4.1) → Tasks 1+5. ✓

**Placeholder scan:** no TBD/TODO; every code step shows complete code; every run step shows the exact command + expected result. ✓

**Type consistency:** `acquireLock(string) (*os.File, error)` + `errLockHeld` (Task 1) used consistently in the main guard; `stage.ReapOrphans(string)(int,error)` and `broker.TerminateStuckAudits(string)(int,error)` defined in Tasks 2-3 and called with those exact signatures in Task 5; the interrupted JSON line is byte-identical between Task 3 (`interruptedResultLine`), Task 3's tests, and Task 4's test. ✓
