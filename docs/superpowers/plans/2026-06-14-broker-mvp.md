# Broker MVP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the weekend-MVP of the macagent broker: a Go daemon that stages a repo on the host, runs Claude Code headless inside a fresh hardware-isolated `apple/container` VM with deny-by-default egress, pulls the diff back out, and pushes from the host.

**Architecture:** A single Go binary (`brokerd`) exposes `POST /tasks`. Per task it: clones the repo on the host (the VM never gets git egress), writes the task prompt + compiled egress allowlist into the stage dir, runs `container run --rm` with a non-root user and an in-VM `nft` firewall installed pre-priv-drop, streams `stream-json` to an audit log, captures the diff, and (after an approval gate) does the host-side `git push` / `gh pr create`. This is Variant 1 (fully-ephemeral per-task VM) with a static credential — the security spine without the v2 warm-pool/gateway/proxy machinery.

**Tech Stack:** Go 1.23 (stdlib `net/http`, `os/exec`, `gopkg.in/yaml.v3`), `apple/container` CLI, Claude Code CLI in a `node:22-bookworm-slim` image, `nftables`, `git`/`gh` on the host.

**Out of scope (separate follow-up plans):** warm pool + reset/recycle state machine; credential gateway (Option A in spec §7); out-of-VM squid+pf egress proxy (spec §6.2). This plan ships the MVP subset only.

---

## File Structure

```
macagent/
├── go.mod                              module macagent
├── config/
│   └── egress.yaml                     source-of-truth allowlist
├── cmd/brokerd/
│   └── main.go                         HTTP wiring + config load
├── internal/
│   ├── egress/
│   │   ├── egress.go                   Config types, Load, CompileAllowlist
│   │   └── egress_test.go
│   ├── runner/
│   │   ├── runner.go                   BuildRunArgs (container run argv)
│   │   └── runner_test.go
│   ├── creds/
│   │   ├── creds.go                    Provider iface + StaticProvider
│   │   └── creds_test.go
│   ├── stage/
│   │   ├── stage.go                    Clone, WriteTaskFiles, CaptureDiff, Push
│   │   └── stage_test.go
│   └── broker/
│       └── broker.go                   HandleTask wiring + approval gate
└── image/
    ├── Dockerfile
    ├── entrypoint.sh
    └── init-firewall.sh
```

Each `internal/*` package has one responsibility and a small surface so it can be unit-tested without a running container. Only the image and the end-to-end task path need a real macOS 26 + `container` host; those steps are marked **[host integration]** and gated behind manual smoke commands.

---

### Task 0: Scaffold the Go module

**Files:**
- Create: `go.mod`
- Create: `config/egress.yaml`

- [ ] **Step 1: Initialize the module**

Run:
```bash
cd /Users/sray/gits/macagent
go mod init macagent
go get gopkg.in/yaml.v3@v3.0.1
```
Expected: `go.mod` created with `module macagent`, `go 1.23` (or current), and a `require gopkg.in/yaml.v3` line; `go.sum` created.

- [ ] **Step 2: Write the source-of-truth egress config**

Create `config/egress.yaml`:
```yaml
version: 1
default:
  allow_dns: true
  domains:
    - { host: api.anthropic.com,      ports: [443] }
    - { host: registry.npmjs.org,     ports: [443] }
    - { host: pypi.org,               ports: [443] }
    - { host: files.pythonhosted.org, ports: [443] }
  cidrs: []
per_task_widening:
  requires_approval: true
```

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum config/egress.yaml
git commit -m "chore: scaffold go module and egress config"
```

---

### Task 1: Egress config types + allowlist compiler

**Files:**
- Create: `internal/egress/egress.go`
- Test: `internal/egress/egress_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/egress/egress_test.go`:
```go
package egress

import "testing"

func TestCompileAllowlist_DefaultPlusExtra(t *testing.T) {
	cfg := Config{}
	cfg.Default.Domains = []Domain{
		{Host: "api.anthropic.com", Ports: []int{443}},
		{Host: "pypi.org", Ports: []int{443}},
	}
	extra := []Domain{{Host: "internal.example.com", Ports: []int{443, 8443}}}

	got := CompileAllowlist(cfg, extra)
	want := "api.anthropic.com 443\npypi.org 443\ninternal.example.com 443\ninternal.example.com 8443\n"
	if got != want {
		t.Fatalf("CompileAllowlist mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestLoad_ParsesYAML(t *testing.T) {
	cfg, err := Load("testdata/egress.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Default.AllowDNS {
		t.Errorf("AllowDNS = false, want true")
	}
	if len(cfg.Default.Domains) != 1 || cfg.Default.Domains[0].Host != "api.anthropic.com" {
		t.Errorf("Domains = %+v, want one api.anthropic.com", cfg.Default.Domains)
	}
	if !cfg.PerTaskWidening.RequiresApproval {
		t.Errorf("RequiresApproval = false, want true")
	}
}
```

Create `internal/egress/testdata/egress.yaml`:
```yaml
version: 1
default:
  allow_dns: true
  domains:
    - { host: api.anthropic.com, ports: [443] }
  cidrs: []
per_task_widening:
  requires_approval: true
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/egress/`
Expected: FAIL — `undefined: Config` / `undefined: CompileAllowlist` / `undefined: Load`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/egress/egress.go`:
```go
// Package egress is the single source of truth for what a sandbox VM may reach.
package egress

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Domain struct {
	Host  string `yaml:"host"`
	Ports []int  `yaml:"ports"`
}

type Config struct {
	Version int `yaml:"version"`
	Default struct {
		AllowDNS bool     `yaml:"allow_dns"`
		Domains  []Domain `yaml:"domains"`
		CIDRs    []string `yaml:"cidrs"`
	} `yaml:"default"`
	PerTaskWidening struct {
		RequiresApproval bool `yaml:"requires_approval"`
	} `yaml:"per_task_widening"`
}

func Load(path string) (Config, error) {
	var cfg Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read egress config: %w", err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse egress config: %w", err)
	}
	return cfg, nil
}

// CompileAllowlist renders "<host> <port>" lines consumed by init-firewall.sh.
// Default domains come first, then approved per-task extras.
func CompileAllowlist(cfg Config, extra []Domain) string {
	var b strings.Builder
	for _, d := range append(append([]Domain{}, cfg.Default.Domains...), extra...) {
		for _, p := range d.Ports {
			fmt.Fprintf(&b, "%s %d\n", d.Host, p)
		}
	}
	return b.String()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/egress/`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add internal/egress/
git commit -m "feat(egress): config types, loader, and allowlist compiler"
```

---

### Task 2: container run argv builder

**Files:**
- Create: `internal/runner/runner.go`
- Test: `internal/runner/runner_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/runner/runner_test.go`:
```go
package runner

import (
	"slices"
	"testing"
)

func TestBuildRunArgs_Contains(t *testing.T) {
	args := BuildRunArgs(Spec{
		TaskID:     "abc123",
		Network:    "egress-abc123",
		ImageRef:   "claude-sandbox:latest",
		APIKey:     "sk-test",
		StageDir:   "/tmp/broker/stage/abc123",
		PromptFile: "/work/.task/prompt.txt",
		MemoryGB:   4,
		CPUs:       4,
	})

	for _, want := range [][]string{
		{"run", "--rm"},
		{"--name", "task-abc123"},
		{"--user", "agent"},
		{"--memory", "4G"},
		{"--cpus", "4"},
		{"--network", "egress-abc123"},
		{"--env", "ANTHROPIC_API_KEY=sk-test"},
		{"--env", "TASK_PROMPT_FILE=/work/.task/prompt.txt"},
		{"--mount", "type=bind,source=/tmp/broker/stage/abc123,target=/work,readonly=false"},
	} {
		if !containsPair(args, want[0], want[1]) {
			t.Errorf("args missing %q %q\n got: %v", want[0], want[1], args)
		}
	}
	if args[len(args)-1] != "/usr/local/bin/entrypoint.sh" {
		t.Errorf("last arg = %q, want entrypoint.sh", args[len(args)-1])
	}
	if !slices.Contains(args, "claude-sandbox:latest") {
		t.Errorf("args missing image ref")
	}
}

func containsPair(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/runner/`
Expected: FAIL — `undefined: BuildRunArgs` / `undefined: Spec`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/runner/runner.go`:
```go
// Package runner builds the `container` CLI argv for a sandbox task.
package runner

import "fmt"

type Spec struct {
	TaskID     string
	Network    string
	ImageRef   string
	APIKey     string
	StageDir   string
	PromptFile string
	MemoryGB   int
	CPUs       int
}

// BuildRunArgs returns the argv that follows the `container` binary name.
func BuildRunArgs(s Spec) []string {
	return []string{
		"run", "--rm",
		"--name", "task-" + s.TaskID,
		"--user", "agent",
		"--memory", fmt.Sprintf("%dG", s.MemoryGB),
		"--cpus", fmt.Sprintf("%d", s.CPUs),
		"--network", s.Network,
		"--env", "ANTHROPIC_API_KEY=" + s.APIKey,
		"--env", "TASK_PROMPT_FILE=" + s.PromptFile,
		"--mount", fmt.Sprintf("type=bind,source=%s,target=/work,readonly=false", s.StageDir),
		s.ImageRef,
		"/usr/local/bin/entrypoint.sh",
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/runner/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/runner/
git commit -m "feat(runner): build container run argv"
```

---

### Task 3: Credential provider (static, behind an interface)

**Files:**
- Create: `internal/creds/creds.go`
- Test: `internal/creds/creds_test.go`

The interface exists so the v2 gateway provider (spec §7 Option A) drops in without touching callers. MVP uses the static provider.

- [ ] **Step 1: Write the failing test**

Create `internal/creds/creds_test.go`:
```go
package creds

import (
	"testing"
	"time"
)

func TestStaticProvider_MintReturnsKey(t *testing.T) {
	var p Provider = StaticProvider{Key: "sk-static"}
	tok, err := p.Mint(15 * time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if tok.Value != "sk-static" {
		t.Errorf("Value = %q, want sk-static", tok.Value)
	}
	if err := p.Revoke(tok); err != nil {
		t.Errorf("Revoke: %v", err)
	}
}

func TestStaticProvider_EmptyKeyErrors(t *testing.T) {
	p := StaticProvider{Key: ""}
	if _, err := p.Mint(time.Minute); err == nil {
		t.Errorf("Mint with empty key: want error, got nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/creds/`
Expected: FAIL — `undefined: Provider` / `undefined: StaticProvider`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/creds/creds.go`:
```go
// Package creds issues credentials for a task. MVP: a static key read from
// the broker host. The Provider interface lets a gateway-backed, per-task
// token provider replace it later without changing callers.
package creds

import (
	"errors"
	"time"
)

type Token struct {
	Value string
}

type Provider interface {
	Mint(ttl time.Duration) (Token, error)
	Revoke(Token) error
}

type StaticProvider struct {
	Key string
}

func (p StaticProvider) Mint(time.Duration) (Token, error) {
	if p.Key == "" {
		return Token{}, errors.New("creds: empty static key")
	}
	return Token{Value: p.Key}, nil
}

func (StaticProvider) Revoke(Token) error { return nil }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/creds/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/creds/
git commit -m "feat(creds): static credential provider behind Provider interface"
```

---

### Task 4: Stager — clone in, write task files, capture diff, push

**Files:**
- Create: `internal/stage/stage.go`
- Test: `internal/stage/stage_test.go`

These functions shell out to `git`, which is available on the host. Tests use a local bare repo as the "remote" so no network is needed.

- [ ] **Step 1: Write the failing test**

Create `internal/stage/stage_test.go`:
```go
package stage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeOriginRepo creates a bare repo with one commit and returns its path.
func makeOriginRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	gitRun(t, work, "init", "-q")
	gitRun(t, work, "config", "user.email", "t@example.com")
	gitRun(t, work, "config", "user.name", "t")
	os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644)
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-q", "-m", "init")

	bare := t.TempDir()
	gitRun(t, bare, "init", "-q", "--bare")
	gitRun(t, work, "remote", "add", "origin", bare)
	gitRun(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	return bare
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestClone_BringsFilesIn(t *testing.T) {
	origin := makeOriginRepo(t)
	dest := filepath.Join(t.TempDir(), "stage")
	if err := Clone(origin, dest); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Errorf("README.md not present after clone: %v", err)
	}
}

func TestWriteTaskFiles(t *testing.T) {
	dest := t.TempDir()
	if err := WriteTaskFiles(dest, "do the thing", "api.anthropic.com 443\n"); err != nil {
		t.Fatalf("WriteTaskFiles: %v", err)
	}
	p, _ := os.ReadFile(filepath.Join(dest, ".task", "prompt.txt"))
	if string(p) != "do the thing" {
		t.Errorf("prompt.txt = %q", p)
	}
	a, _ := os.ReadFile(filepath.Join(dest, ".task", "allowlist.txt"))
	if string(a) != "api.anthropic.com 443\n" {
		t.Errorf("allowlist.txt = %q", a)
	}
}

func TestCaptureDiff_SeesChange(t *testing.T) {
	origin := makeOriginRepo(t)
	dest := filepath.Join(t.TempDir(), "stage")
	if err := Clone(origin, dest); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	os.WriteFile(filepath.Join(dest, "README.md"), []byte("hello\nworld\n"), 0o644)
	diff, err := CaptureDiff(dest)
	if err != nil {
		t.Fatalf("CaptureDiff: %v", err)
	}
	if !strings.Contains(diff, "+world") {
		t.Errorf("diff missing +world:\n%s", diff)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/stage/`
Expected: FAIL — `undefined: Clone` / `undefined: WriteTaskFiles` / `undefined: CaptureDiff`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/stage/stage.go`:
```go
// Package stage handles host-side repo I/O: clone the repo in, write the task
// inputs, capture the resulting diff, and push from the host. The VM never
// gets git remote access — only data (the diff) crosses the trust boundary.
package stage

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %v: %w\n%s", args, err, out)
	}
	return string(out), nil
}

func Clone(repoRef, dest string) error {
	_, err := git("", "clone", "--depth", "1", repoRef, dest)
	return err
}

func WriteTaskFiles(dest, prompt, allowlist string) error {
	dir := filepath.Join(dest, ".task")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte(prompt), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "allowlist.txt"), []byte(allowlist), 0o644)
}

// CaptureDiff stages all changes and returns the unified diff (without committing).
// Excludes the .task/ control dir (prompt + compiled allowlist live in the repo
// root so the in-VM entrypoint can read them) so it never leaks into the diff,
// the approval gate, or the pushed PR.
func CaptureDiff(dir string) (string, error) {
	if _, err := git(dir, "add", "-A", "--", ".", ":(exclude).task"); err != nil {
		return "", err
	}
	return git(dir, "diff", "--cached")
}

// Push creates a branch, commits the staged work, pushes with host creds, and
// opens a PR. Called only after the broker's approval gate.
func Push(dir, branch, message string) error {
	if _, err := git(dir, "checkout", "-b", branch); err != nil {
		return err
	}
	if _, err := git(dir, "commit", "-m", message); err != nil {
		return err
	}
	if _, err := git(dir, "push", "origin", branch); err != nil {
		return err
	}
	cmd := exec.Command("gh", "pr", "create", "--head", branch, "--fill")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr create: %w\n%s", err, out)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/stage/`
Expected: PASS (Clone, WriteTaskFiles, CaptureDiff). `Push` is exercised by the host integration smoke test in Task 8, not unit-tested.

- [ ] **Step 5: Commit**

```bash
git add internal/stage/
git commit -m "feat(stage): clone-in, task files, diff capture, host push"
```

---

### Task 5: Sandbox image (Dockerfile + entrypoint + firewall)

**Files:**
- Create: `image/Dockerfile`
- Create: `image/entrypoint.sh`
- Create: `image/init-firewall.sh`

**[host integration]** — building and smoke-testing this requires macOS 26+, Apple silicon, and `container` installed. The unit steps below just create the files; the build/smoke is gated.

- [ ] **Step 1: Write the firewall script**

Create `image/init-firewall.sh`:
```bash
#!/usr/bin/env bash
# nft default-deny output policy; allow DNS + allowlisted domains' IPs on :443.
# Run as ROOT in the entrypoint BEFORE dropping to the non-root agent.
set -euo pipefail
ALLOW="${1:?usage: init-firewall.sh <allowlist-file>}"
nft flush ruleset
nft add table inet fw
nft add chain inet fw out '{ type filter hook output priority 0; policy drop; }'
nft add rule inet fw out ct state established,related accept
nft add rule inet fw out oifname "lo" accept
nft add rule inet fw out udp dport 53 accept
nft add rule inet fw out tcp dport 53 accept
nft add set inet fw allow4 '{ type ipv4_addr; flags interval; }'
while read -r host port; do
  [ -z "${host:-}" ] && continue
  for ip in $(getent ahostsv4 "$host" | awk '{print $1}' | sort -u); do
    nft add element inet fw allow4 "{ $ip }"
  done
done < "$ALLOW"
nft add rule inet fw out ip daddr @allow4 tcp dport '{ 443 }' accept
```

- [ ] **Step 2: Write the entrypoint**

Create `image/entrypoint.sh`:
```bash
#!/usr/bin/env bash
# Root installs the egress firewall, THEN drops privileges to run Claude.
# A non-root agent cannot flush nft, so the firewall holds for the task.
set -euo pipefail
/usr/local/bin/init-firewall.sh /work/.task/allowlist.txt
cd /work
exec gosu agent claude --bare -p "$(cat /work/.task/prompt.txt)" \
     --dangerously-skip-permissions \
     --output-format stream-json --verbose --include-partial-messages
```

- [ ] **Step 3: Write the Dockerfile**

Create `image/Dockerfile`:
```dockerfile
FROM node:22-bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates curl jq nftables dnsutils ipset gosu \
 && rm -rf /var/lib/apt/lists/*
RUN npm install -g @anthropic-ai/claude-code
RUN useradd -m -u 10001 -s /bin/bash agent
COPY init-firewall.sh /usr/local/bin/init-firewall.sh
COPY entrypoint.sh    /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/init-firewall.sh /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
```

> Note: the allowlist is read from the mounted `/work/.task/allowlist.txt` (written by the broker per task), not baked into the image — so the image is task-agnostic.

- [ ] **Step 4: Build the image [host integration]**

Run:
```bash
cd image && container build -t claude-sandbox:latest .
```
Expected: build succeeds; `container images` lists `claude-sandbox:latest`.
If `container build` is unavailable in your `container` version, fall back to building an OCI image with `docker buildx build -o type=oci` and `container image load`. Flag if neither works.

- [ ] **Step 5: Smoke-test egress deny-by-default [host integration]**

Run (expect the firewall to BLOCK a non-allowlisted host and ALLOW api.anthropic.com):
```bash
printf 'api.anthropic.com 443\n' > /tmp/allow.txt
container run --rm --user root \
  --mount type=bind,source=/tmp,target=/work/.task,readonly=false \
  claude-sandbox:latest bash -lc '
    /usr/local/bin/init-firewall.sh /work/.task/allow.txt
    curl -sS -m 5 https://api.anthropic.com/ -o /dev/null -w "anthropic:%{http_code}\n" || echo "anthropic:blocked"
    curl -sS -m 5 https://example.com/ -o /dev/null -w "example:%{http_code}\n" || echo "example:blocked"
  '
```
Expected: a line for `anthropic:` with an HTTP code (reachable), and `example:blocked` (timed out / refused). If `example` is reachable, the firewall is not enforcing — stop and debug before proceeding.

- [ ] **Step 6: Commit**

```bash
git add image/
git commit -m "feat(image): sandbox Dockerfile, entrypoint, nft egress firewall"
```

---

### Task 6: Broker handler wiring

**Files:**
- Create: `internal/broker/broker.go`

This wires the packages together. The HTTP/exec path is exercised by the Task 8 smoke test; the unit-testable logic already lives in Tasks 1–4.

- [ ] **Step 1: Write the broker**

Create `internal/broker/broker.go`:
```go
// Package broker wires staging, egress compilation, credential minting, the
// container run, diff capture, the approval gate, and the host-side push.
package broker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"macagent/internal/creds"
	"macagent/internal/egress"
	"macagent/internal/runner"
	"macagent/internal/stage"
)

type Task struct {
	RepoRef     string          `json:"repo_ref"`
	Instruction string          `json:"instruction"`
	EgressExtra []egress.Domain `json:"egress_extra"`
	Sensitive   bool            `json:"sensitive"`
}

// ApprovalFn returns true to allow the action. MVP default may auto-approve.
type ApprovalFn func(kind string, payload any) bool

type Broker struct {
	Cfg       egress.Config
	Creds     creds.Provider
	Approve   ApprovalFn
	ImageRef  string
	StageRoot string
	AuditRoot string
	Timeout   time.Duration
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (b *Broker) HandleTask(w http.ResponseWriter, r *http.Request) {
	var t Task
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(t.EgressExtra) > 0 && b.Cfg.PerTaskWidening.RequiresApproval {
		if !b.Approve("widen egress", t.EgressExtra) {
			http.Error(w, "egress widening denied", http.StatusForbidden)
			return
		}
	}

	taskID := newID()
	stageDir := filepath.Join(b.StageRoot, taskID)

	if err := stage.Clone(t.RepoRef, stageDir); err != nil {
		http.Error(w, "clone failed", http.StatusBadGateway)
		return
	}
	allowlist := egress.CompileAllowlist(b.Cfg, t.EgressExtra)
	if err := stage.WriteTaskFiles(stageDir, t.Instruction, allowlist); err != nil {
		http.Error(w, "stage failed", http.StatusInternalServerError)
		return
	}

	tok, err := b.Creds.Mint(15 * time.Minute)
	if err != nil {
		http.Error(w, "credential mint failed", http.StatusInternalServerError)
		return
	}
	defer b.Creds.Revoke(tok)

	if err := os.MkdirAll(b.AuditRoot, 0o755); err != nil {
		http.Error(w, "audit dir failed", http.StatusInternalServerError)
		return
	}
	auditPath := filepath.Join(b.AuditRoot, taskID+".jsonl")
	logf, err := os.Create(auditPath)
	if err != nil {
		http.Error(w, "audit file failed", http.StatusInternalServerError)
		return
	}
	defer logf.Close()

	args := runner.BuildRunArgs(runner.Spec{
		TaskID:     taskID,
		Network:    "", // MVP: default per-VM network; v2 sets a named egress net
		ImageRef:   b.ImageRef,
		APIKey:     tok.Value,
		StageDir:   stageDir,
		PromptFile: "/work/.task/prompt.txt",
		MemoryGB:   4,
		CPUs:       4,
	})
	// MVP runs on the default network; drop the empty --network pair.
	args = dropEmptyNetwork(args)

	ctx, cancel := context.WithTimeout(r.Context(), b.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "container", args...)
	cmd.Stdout = io.MultiWriter(logf, os.Stdout)
	cmd.Stderr = logf
	if err := cmd.Run(); err != nil {
		http.Error(w, "task failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	diff, err := stage.CaptureDiff(stageDir)
	if err != nil {
		http.Error(w, "diff capture failed", http.StatusInternalServerError)
		return
	}
	if diff == "" {
		writeJSON(w, map[string]any{"task_id": taskID, "diff": "", "pushed": false})
		return
	}
	if !b.Approve("push diff", diff) {
		writeJSON(w, map[string]any{"task_id": taskID, "diff": diff, "pushed": false})
		return
	}

	branch := "agent/" + taskID
	if err := stage.Push(stageDir, branch, "agent: "+t.Instruction); err != nil {
		http.Error(w, "push failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"task_id": taskID, "branch": branch, "pushed": true})
}

func dropEmptyNetwork(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--network" && i+1 < len(args) && args[i+1] == "" {
			i++ // skip flag and its empty value
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
```

- [ ] **Step 2: Verify it compiles**

Run: `go build ./...`
Expected: builds with no errors.

- [ ] **Step 3: Add a test for dropEmptyNetwork**

Append to a new file `internal/broker/broker_test.go`:
```go
package broker

import (
	"slices"
	"testing"
)

func TestDropEmptyNetwork(t *testing.T) {
	in := []string{"run", "--network", "", "--user", "agent"}
	got := dropEmptyNetwork(in)
	want := []string{"run", "--user", "agent"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestDropEmptyNetwork_KeepsNamed(t *testing.T) {
	in := []string{"run", "--network", "egress-x", "--rm"}
	got := dropEmptyNetwork(in)
	if !slices.Contains(got, "egress-x") {
		t.Errorf("named network dropped: %v", got)
	}
}
```

- [ ] **Step 4: Run test**

Run: `go test ./internal/broker/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/broker/
git commit -m "feat(broker): wire staging, egress, creds, run, diff, gated push"
```

---

### Task 7: HTTP entrypoint (cmd/brokerd)

**Files:**
- Create: `cmd/brokerd/main.go`

- [ ] **Step 1: Write main**

Create `cmd/brokerd/main.go`:
```go
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"macagent/internal/broker"
	"macagent/internal/creds"
	"macagent/internal/egress"
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

	b := &broker.Broker{
		Cfg:       cfg,
		Creds:     creds.StaticProvider{Key: apiKey},
		Approve:   func(kind string, _ any) bool { log.Printf("approval gate: %s -> auto-approve (MVP)", kind); return true },
		ImageRef:  env("SANDBOX_IMAGE", "claude-sandbox:latest"),
		StageRoot: env("STAGE_ROOT", "/tmp/broker/stage"),
		AuditRoot: env("AUDIT_ROOT", "/tmp/broker/audit"),
		Timeout:   30 * time.Minute,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /tasks", b.HandleTask)

	addr := env("BROKER_ADDR", "127.0.0.1:8765")
	log.Printf("brokerd listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```

> The MVP approval gate auto-approves and logs. Replace with a real gate (CLI prompt / webhook) before any unattended run that can push to a shared remote.

- [ ] **Step 2: Verify it builds and vet passes**

Run:
```bash
go build ./... && go vet ./...
```
Expected: no errors.

- [ ] **Step 3: Run the full unit suite**

Run: `go test ./...`
Expected: all packages PASS (egress, runner, creds, stage, broker).

- [ ] **Step 4: Commit**

```bash
git add cmd/brokerd/
git commit -m "feat(brokerd): http entrypoint with POST /tasks"
```

---

### Task 8: End-to-end smoke test [host integration]

**Files:** none (verification only)

Requires macOS 26+, Apple silicon, `container` installed, the image built (Task 5), `gh` authenticated on the host, and a throwaway test repo you control. **Keep the broker binary and stage dir OUT of `~/Documents` and `~/Desktop`** (known vmnet-creation bug — use `/tmp` or a path under the project).

- [ ] **Step 1: Start the broker**

Run:
```bash
export ANTHROPIC_API_KEY=sk-...      # a workspace key with a spend cap set in the Console
go run ./cmd/brokerd
```
Expected: `brokerd listening on 127.0.0.1:8765`.

- [ ] **Step 2: Submit a task against a throwaway repo**

Run (in another shell):
```bash
curl -sS -X POST http://127.0.0.1:8765/tasks \
  -H 'content-type: application/json' \
  -d '{"repo_ref":"https://github.com/<you>/<throwaway>.git","instruction":"Add a line saying HELLO to README.md"}' | jq .
```
Expected JSON: `{"task_id":"...","branch":"agent/...","pushed":true}` (auto-approve MVP gate). On the GitHub side, a new branch `agent/<task_id>` and a PR exist.

- [ ] **Step 3: Verify the audit log captured stream-json**

Run:
```bash
ls -la /tmp/broker/audit/ && head -3 /tmp/broker/audit/*.jsonl
```
Expected: a `<task_id>.jsonl` file whose lines are JSON events (a `system`/`init` event first).

- [ ] **Step 4: Verify the VM had no host-secret access and no git egress**

Confirm by inspection:
- The `container run` argv (Task 2/6) mounts only the stage dir — no `~/.ssh`, no host creds.
- The push branch was created by the **host** `git` (Task 4 `Push`), not inside the VM.
- The egress smoke test (Task 5 Step 5) already proved non-allowlisted hosts are blocked.

Document the result. If any check fails, the boundary is not intact — stop and fix before declaring the MVP done.

---

## Self-Review

**Spec coverage (MVP subset of the spec):**
- Req #1 host-side push — Task 4 `Push` + Task 6 gated call. ✓
- Req #2 deny-by-default egress from source-of-truth config — Task 0 `egress.yaml` + Task 1 compiler + Task 5 `init-firewall.sh`. ✓
- Req #3 bypass mode headless — Task 5 `entrypoint.sh` (`--dangerously-skip-permissions --bare --output-format stream-json`). ✓
- Threat model: no host secrets mounted (Task 2/6 argv), non-root agent (Task 5 `gosu agent`, image `useradd`), ephemeral `--rm` (Task 2). ✓
- Credentials: static provider behind an interface for the v2 gateway (Task 3). ✓
- Observability: stream-json audit log (Task 6 `MultiWriter`, Task 8 Step 3). ✓
- Approval gates: egress widening + pre-push (Task 6, auto-approve in MVP with explicit warning). ✓
- **Deferred (named follow-up plans, intentionally not covered):** warm pool + reset/recycle state machine; credential gateway; out-of-VM squid+pf proxy. Tracked in spec §5/§6.2/§7 and §14.9.

**Placeholder scan:** no TBD/TODO; every code step shows complete code; host-integration steps give exact commands + expected output. ✓

**Type consistency:** `egress.Domain`/`egress.Config`/`egress.CompileAllowlist`, `runner.Spec`/`runner.BuildRunArgs`, `creds.Provider`/`creds.Token`/`creds.StaticProvider`, `stage.Clone`/`WriteTaskFiles`/`CaptureDiff`/`Push`, `broker.Broker`/`Task`/`HandleTask` are used consistently across tasks. The `runner.Spec.Network` field is set empty in MVP and stripped by `broker.dropEmptyNetwork`; v2 will populate it. ✓

---

## Open risks carried from the spec (verify during execution)

1. `--bare` + `ANTHROPIC_API_KEY` is the documented auth path — confirm the installed Claude Code version honors it and emits `stream-json` as expected.
2. `container build` may not exist in your `container` version — fallback noted in Task 5 Step 4.
3. Per-VM bind-mount may be flaky in v0.x — fallback is `container cp` after `create`/`start` (spec §12.1).
4. The MVP in-VM `nft` firewall trusts root-during-init; a kernel LPE defeats it. The v2 out-of-VM proxy (separate plan) removes that trust. Do not run with a *widened* allowlist until then.
