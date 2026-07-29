# Independent Verifier Stage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** After the agent VM exits and the host captures the diff, run host-configured verification commands (e.g. `go test ./...`) in fresh, credential-free, network-denied VMs; record exit codes broker-side into the trust brief; surface a `verifying` stage; optionally block push on failure (`required: true`).

**Architecture:** The sealed staged tree (`git write-tree` + `git archive` from the host-only git dir) is exported into a quota-bounded copy and mounted into a fresh VM per command. The VM installs a deny-all nft pin (loopback only) as root, then drops privileges via the existing `drop-agent.sh` before any repo code runs — no image change needed. The broker reads exit codes from the `container run` process (never from VM output), threads results through `taskRun` into `writeBrief`, and re-checks the staged tree hash at push time so pushed tree == verified tree.

**Tech Stack:** Go stdlib only. Reuses existing seams: `b.runAgent`, `outputCap`, bounded-shell-out convention (`ctx` + `WaitDelay`), optional-capability type assertions on `taskStage`.

## Decision record (locked; do not relitigate in-task)

- Verification commands are **host-config only** (`~/.drydock/config.yaml`); repo files cannot propose them.
- Verifier VMs are **network-denied by in-VM deny-all nft pin** + privilege drop (agent user cannot flush; same A2 mechanism). No proxy env, no gateway bearer, no squid credentials are injected.
- **One fresh VM per command**, sequential, fail-fast: first non-zero exit stops the loop; remaining commands are recorded `skipped`.
- `required: true` blocks push unless overall status is exactly `passed` (fail-closed: `failed`, `inconclusive` both block). Default `required: false` = advisory.
- Timeouts/hangs → command `timed_out`, overall `inconclusive` — **never** `passed`.
- Mid-verify daemon restart or kill → task ends `interrupted` (same as mid-run today). No verify-resume machinery.
- Verifier verdicts are broker-authored (F-07): exit codes come from the `container run` process; log text can never flip a verdict.

## Global Constraints

- Every new external command runs under a bounded context derived from the task ctx with `cmd.WaitDelay` set (mirror `internal/stage/stage.go:52-57` / `internal/stage/quota_darwin.go:26-36`).
- Verifier output is capped with the existing `outputCap` (`internal/broker/outputcap.go`, budget `maxTaskOutputBytes`); on breach the verify run is cancelled and the command is `error`, overall `inconclusive`.
- New audit artifacts (`<id>.verify.log`): 0600, `syscall.O_NOFOLLOW`, under `AuditRoot`; `drydock prune` must delete them.
- The verify work dir lives under the stage `Root` (inside the APFS quota image, A8/F-04); never a plain host dir.
- No Brief/metrics field may be derived from agent or verifier VM output; the only VM-derived artifact is the capped log file, which is display-only.
- Metrics: `stage_ms` gains `verifying` additively on BOTH writer (`internal/broker/metrics.go`) and reader (`internal/audit/audit.go` `StageMs`); the row shape IS `audit.Metrics` marshalled directly — keep them one struct.
- `go vet ./...`, `go test -race -count=1 ./...`, `gofmt -l internal/ cmd/` silent, and `go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...` clean before every commit (CI runs staticcheck).
- Comments state constraints/why; commit style `type(scope): summary`; end commit messages with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`; never add a Generated-with footer to PR bodies.

---

### Task 1: `internal/repokey` — canonical repo keys

**Files:**
- Create: `internal/repokey/repokey.go`
- Create: `internal/repokey/repokey_test.go`

**Interfaces:**
- Produces: `repokey.Normalize(ref string) string` — canonical `host/owner/repo` (lowercase host, scheme/userinfo/`.git`/trailing-slash stripped; `git@h:o/r` and `ssh://git@h/o/r` → `h/o/r`). Consumed by config validation (Task 3) and broker lookup (Task 6).

- [ ] **Step 1: Failing test** — create `internal/repokey/repokey_test.go`:

```go
package repokey

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"https://github.com/Owner/Repo.git":      "github.com/Owner/Repo",
		"https://user:pass@github.com/o/r":       "github.com/o/r",
		"git@github.com:o/r.git":                 "github.com/o/r",
		"ssh://git@GitHub.com/o/r":               "github.com/o/r",
		"https://gitlab.example.com/g/p/":        "gitlab.example.com/g/p",
		"github.com/o/r":                         "github.com/o/r",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalize_HostileInputsDoNotPanic(t *testing.T) {
	for _, in := range []string{"", ":", "http://", "git@", "a b c", "https://[::1", "%zz"} {
		_ = Normalize(in)
	}
}
```

- [ ] **Step 2: Run to fail** — `go test ./internal/repokey/` → FAIL (package missing).

- [ ] **Step 3: Implement** — create `internal/repokey/repokey.go`:

```go
// Package repokey canonicalizes git repo references into a stable
// "host/owner/repo" key so operator config (verify.repos keys) matches
// however a task happens to spell the same repository (https vs scp-style
// vs ssh://, credentials, .git suffix, host case).
package repokey

import (
	"net/url"
	"strings"
)

// Normalize returns the canonical key for a repo reference. Unrecognized
// shapes come back trimmed but otherwise unchanged — matching then simply
// fails, which is the safe direction (no verification configured).
func Normalize(ref string) string {
	ref = strings.TrimSpace(ref)
	// scp-style: git@host:owner/repo(.git)
	if at := strings.Index(ref, "@"); at >= 0 && !strings.Contains(ref, "://") {
		if colon := strings.Index(ref[at:], ":"); colon > 0 {
			host := ref[at+1 : at+colon]
			path := ref[at+colon+1:]
			return join(host, path)
		}
	}
	if strings.Contains(ref, "://") {
		if u, err := url.Parse(ref); err == nil && u.Host != "" {
			return join(u.Host, u.Path)
		}
		return ref
	}
	// already host/path shaped
	if i := strings.IndexByte(ref, '/'); i > 0 {
		return join(ref[:i], ref[i+1:])
	}
	return ref
}

func join(host, path string) string {
	host = strings.ToLower(host)
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	if path == "" {
		return host
	}
	return host + "/" + path
}
```

- [ ] **Step 4: Pass** — `go test -race ./internal/repokey/` → PASS.
- [ ] **Step 5: Commit** — `git add internal/repokey/ && git commit -m "feat(repokey): canonical host/owner/repo keys for verify config matching"`

---

### Task 2: trustbrief — Verification schema + submodule-gitlink flag

**Files:**
- Modify: `internal/trustbrief/brief.go` (extend `Verification`, add `VerifyCommand`, status/command-status constants)
- Modify: `internal/trustbrief/difffacts.go` (new flag kind `submodule-gitlink`)
- Test: `internal/trustbrief/brief_test.go`, `internal/trustbrief/difffacts_test.go`

**Interfaces (Tasks 6–7 depend on exact names):**
- `const VerificationPassed = "passed"`, `VerificationFailed = "failed"`, `VerificationInconclusive = "inconclusive"` (alongside existing `VerificationNotConfigured`).
- `const VerifyCmdPassed = "passed"`, `VerifyCmdFailed = "failed"`, `VerifyCmdTimedOut = "timed_out"`, `VerifyCmdError = "error"`, `VerifyCmdSkipped = "skipped"`.
- `type VerifyCommand struct { Argv []string; Status string; ExitCode int; DurationMs int64 }` (json: `argv`, `status`, `exit_code`, `duration_ms`).
- `Verification` becomes `{ Status string; Network string; Credentials string; TreeSHA string; LogSHA256 string; Commands []VerifyCommand }` (json: `status`, `network,omitempty`, `credentials,omitempty`, `tree_sha,omitempty`, `log_sha256,omitempty`, `commands,omitempty`). Existing zero-value/`not_configured` briefs stay valid — all new fields omitempty except `status`.
- `const FlagSubmodule = "submodule-gitlink"` in difffacts.

- [ ] **Step 1: Failing tests.** In `brief_test.go` add:

```go
func TestVerification_RoundTripAndOmitEmpty(t *testing.T) {
	dir := t.TempDir()
	b := sampleBrief()
	b.Verification = Verification{
		Status: VerificationFailed, Network: "denied", Credentials: "none",
		TreeSHA: "abc123", LogSHA256: HashInstruction("log"),
		Commands: []VerifyCommand{
			{Argv: []string{"go", "test", "./..."}, Status: VerifyCmdFailed, ExitCode: 1, DurationMs: 1200},
			{Argv: []string{"go", "vet", "./..."}, Status: VerifyCmdSkipped},
		},
	}
	if err := Write(dir, b.TaskID, b); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir, b.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Verification, b.Verification) {
		t.Errorf("verification round-trip:\n got %+v\nwant %+v", got.Verification, b.Verification)
	}
	// A not_configured brief must keep its minimal v1 shape on the wire.
	minimal, _ := json.Marshal(Verification{Status: VerificationNotConfigured})
	if string(minimal) != `{"status":"not_configured"}` {
		t.Errorf("minimal verification = %s, want status-only object", minimal)
	}
}
```

In `difffacts_test.go` add (real git shapes for gitlink changes):

```go
func TestAnalyze_SubmoduleGitlinkFlagged(t *testing.T) {
	cases := map[string]string{
		"new-gitlink": "diff --git a/vendor/dep b/vendor/dep\n" +
			"new file mode 160000\nindex 0000000..1111111\n" +
			"--- /dev/null\n+++ b/vendor/dep\n@@ -0,0 +1 @@\n+Subproject commit 1111111\n",
		"retargeted-gitlink": "diff --git a/vendor/dep b/vendor/dep\n" +
			"index 1111111..2222222 160000\n" +
			"--- a/vendor/dep\n+++ b/vendor/dep\n@@ -1 +1 @@\n-Subproject commit 1111111\n+Subproject commit 2222222\n",
	}
	for name, diff := range cases {
		t.Run(name, func(t *testing.T) {
			paths := flagPaths(Analyze(diff), FlagSubmodule)
			if len(paths) != 1 || paths[0] != "vendor/dep" {
				t.Errorf("submodule flag paths = %v, want [vendor/dep]", paths)
			}
		})
	}
}
```

- [ ] **Step 2: Run to fail**, then **Step 3: Implement.**

`brief.go`: replace the `Verification` struct and add beside `VerificationNotConfigured`:

```go
// Overall verification statuses. "inconclusive" covers timeouts and infra
// errors — evidence is absent, which must never read as passing.
const (
	VerificationPassed       = "passed"
	VerificationFailed       = "failed"
	VerificationInconclusive = "inconclusive"
)

// Per-command statuses.
const (
	VerifyCmdPassed   = "passed"
	VerifyCmdFailed   = "failed"
	VerifyCmdTimedOut = "timed_out"
	VerifyCmdError    = "error"
	VerifyCmdSkipped  = "skipped"
)

// VerifyCommand is one verification command's broker-observed result. The
// exit code is read from the container process by the broker; nothing in
// the verifier VM's output can influence these fields.
type VerifyCommand struct {
	Argv       []string `json:"argv"`
	Status     string   `json:"status"`
	ExitCode   int      `json:"exit_code"`
	DurationMs int64    `json:"duration_ms"`
}

// Verification is the independent-verifier evidence block. Network and
// Credentials record the verifier VM's capability posture ("denied"/"none");
// TreeSHA is the staged tree the commands ran against — the same tree the
// push-time guard re-checks so pushed tree == verified tree.
type Verification struct {
	Status      string          `json:"status"`
	Network     string          `json:"network,omitempty"`
	Credentials string          `json:"credentials,omitempty"`
	TreeSHA     string          `json:"tree_sha,omitempty"`
	LogSHA256   string          `json:"log_sha256,omitempty"`
	Commands    []VerifyCommand `json:"commands,omitempty"`
}
```

`difffacts.go`: add `FlagSubmodule = "submodule-gitlink"` to the flag-kind const block; in the mode-line handling add `160000` detection to BOTH the `new file mode `/`new mode ` case and the trailing-mode-on-`index`-line case (mirror how `120000` is handled in each — same guards, same `addFlag(FlagSubmodule, curPath)`).

- [ ] **Step 4: Pass** — `go test -race ./internal/trustbrief/` + `go test -run=NONE -fuzz=FuzzAnalyze -fuzztime=20s ./internal/trustbrief/`.
- [ ] **Step 5: Commit** — `git commit -m "feat(trustbrief): verification result schema + submodule-gitlink flag"`

---

### Task 3: config — `verify:` block

**Files:**
- Modify: `internal/config/config.go` (struct field, validation, seed template)
- Test: `internal/config/config_test.go`

**Interfaces:**
- `type VerifyRepo struct { Commands [][]string \`yaml:"commands"\`; Timeout time.Duration \`yaml:"timeout"\`; Required bool \`yaml:"required"\` }`
- `Config.Verify struct { Repos map[string]VerifyRepo \`yaml:"repos"\` } \`yaml:"verify"\`` — placed after `OpenAICompat`.
- Validation: every key must equal `repokey.Normalize(key)` (fail-closed on sloppy keys — the exact silent-no-op failure claude-code#6699 had); each repo needs ≥1 command; each command argv non-empty with non-empty argv[0]; timeout ≥ 0.
- No env override (structured map; yaml-only, like `openai_compat`).

- [ ] **Step 1: Failing tests** — add to `config_test.go`:

```go
func TestVerifyConfig_LoadsAndValidates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	yaml := "network: x\ngateway_ip: 1.2.3.4\n" +
		"verify:\n  repos:\n    \"github.com/o/r\":\n" +
		"      commands:\n        - [\"go\", \"test\", \"./...\"]\n" +
		"      timeout: 5m\n      required: true\n"
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	vr, ok := c.Verify.Repos["github.com/o/r"]
	if !ok || !vr.Required || vr.Timeout != 5*time.Minute ||
		len(vr.Commands) != 1 || strings.Join(vr.Commands[0], " ") != "go test ./..." {
		t.Errorf("verify repo = %+v ok=%v", vr, ok)
	}
}

func TestVerifyConfig_Rejects(t *testing.T) {
	base := "network: x\ngateway_ip: 1.2.3.4\nverify:\n  repos:\n"
	cases := map[string]string{
		// Non-canonical key would silently never match a task — fail loudly at load.
		base + "    \"https://github.com/o/r.git\":\n      commands: [[\"go\", \"test\"]]\n": "verify",
		base + "    \"github.com/o/r\":\n      commands: []\n":                              "verify",
		base + "    \"github.com/o/r\":\n      commands: [[]]\n":                            "verify",
		base + "    \"github.com/o/r\":\n      commands: [[\"go\"]]\n      timeout: -1s\n":  "verify",
	}
	for yaml, wantSubstr := range cases {
		path := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), wantSubstr) {
			t.Errorf("Load(%q) err = %v, want %q", yaml, err, wantSubstr)
		}
	}
}
```

- [ ] **Step 2: fail**, **Step 3: Implement.** Add to `Config` after the `OpenAICompat` field:

```go
	// Verify configures the independent post-run verifier: host-approved
	// commands run against the agent's staged tree in a fresh, credential-
	// free, network-denied VM before the approval gate. Keys are canonical
	// "host/owner/repo" (repokey.Normalize); non-canonical keys are a config
	// error rather than a silent never-match. Empty map = verifier off.
	Verify struct {
		Repos map[string]VerifyRepo `yaml:"repos"`
	} `yaml:"verify"`
```

with, above `Config`:

```go
// VerifyRepo is one repository's verification recipe. Timeout 0 uses the
// built-in default (broker.DefaultVerifyTimeout). Required=true blocks the
// push unless verification status is exactly "passed" (fail-closed —
// inconclusive evidence blocks too).
type VerifyRepo struct {
	Commands [][]string    `yaml:"commands"`
	Timeout  time.Duration `yaml:"timeout"`
	Required bool          `yaml:"required"`
}
```

In `validate()` (near the other loops), add:

```go
	for key, vr := range c.Verify.Repos {
		if canon := repokey.Normalize(key); canon != key {
			return fmt.Errorf("config: verify.repos key %q is not canonical; use %q", key, canon)
		}
		if len(vr.Commands) == 0 {
			return fmt.Errorf("config: verify.repos[%q] needs at least one command", key)
		}
		for i, argv := range vr.Commands {
			if len(argv) == 0 || argv[0] == "" {
				return fmt.Errorf("config: verify.repos[%q].commands[%d] is empty", key, i)
			}
		}
		if vr.Timeout < 0 {
			return fmt.Errorf("config: verify.repos[%q].timeout must be >= 0", key)
		}
	}
```

Import `drydock/internal/repokey`. Add a commented block to `SeedTemplate` (match surrounding comment style, placed near `openai_compat`):

```
# verify: independent post-run verification. Commands run against the agent's
# exact staged tree in a fresh VM with no credentials and no network; exit
# codes are recorded in the task's trust brief. required: true blocks the
# push unless every command passes. Keys are canonical host/owner/repo.
# verify:
#   repos:
#     "github.com/you/yourrepo":
#       commands:
#         - ["go", "test", "./..."]
#       timeout: 10m      # 0 = default (10m)
#       required: false   # true = failure/inconclusive blocks push
```

Check `TestSeedTemplate_MatchesOnDiskTemplate` / `knownfields_test.go`: the seed and `config/config.yaml` are pinned to each other — update `config/config.yaml` with the same block.

- [ ] **Step 4: Pass** — `go test -race ./internal/config/ ./cmd/docs-build/` (docs-build pins config claims; if `TestSecurityDefaultsPageCurrent` fails here, defer — Task 7 regenerates the page; note it in the report instead of hacking around it).
- [ ] **Step 5: Commit** — `git commit -m "feat(config): host-side verify.repos block with canonical-key validation"`

---

### Task 4: stage — sealed-tree export

**Files:**
- Modify: `internal/stage/stage.go`
- Test: `internal/stage/stage_test.go`

**Interfaces:**
- `(s *Stage) StagedTreeHash() (string, error)` — `git write-tree` of the staged index (the index `CaptureDiff` built via `stageAll`); re-runs `stageAll` first so it is self-contained and idempotent.
- `(s *Stage) ExportStaged(dst string) error` — materializes exactly that tree into `dst` (created 0700 under the stage root) via `git archive --format=tar <tree> | tar -x -C dst`, both ends bounded by the stage ctx + `WaitDelay`.

- [ ] **Step 1: Failing test** — add to `stage_test.go` (mirror the local-source-repo helper used by `TestBaseCommit_ReturnsCloneHead`):

```go
func TestExportStaged_MatchesStagedContentAndExcludesTaskDir(t *testing.T) {
	src := newLocalSourceRepo(t) // reuse the existing helper pattern in this file
	st, err := Prepare(context.Background(), t.TempDir(), src)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// Simulate agent output: a tracked change, plus .task noise that must
	// never reach the verified tree (A4 parity).
	if err := os.WriteFile(filepath.Join(st.WorkDir, "new.txt"), []byte("agent"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(st.WorkDir, ".task"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(st.WorkDir, ".task", "prompt.txt"), []byte("p"), 0o644); err != nil {
		t.Fatal(err)
	}

	h1, err := st.StagedTreeHash()
	if err != nil {
		t.Fatalf("StagedTreeHash: %v", err)
	}
	if len(h1) != 40 {
		t.Errorf("tree hash = %q, want 40 hex", h1)
	}
	// Idempotent: same content, same hash.
	if h2, _ := st.StagedTreeHash(); h2 != h1 {
		t.Errorf("hash not stable: %q vs %q", h2, h1)
	}

	dst := filepath.Join(st.Root, "verify")
	if err := st.ExportStaged(dst); err != nil {
		t.Fatalf("ExportStaged: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dst, "new.txt")); err != nil || string(data) != "agent" {
		t.Errorf("exported new.txt = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".task")); !os.IsNotExist(err) {
		t.Error(".task leaked into the exported verify tree")
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); !os.IsNotExist(err) {
		t.Error(".git leaked into the exported verify tree")
	}
}
```

(If no `newLocalSourceRepo`-style helper exists, extract one from `TestBaseCommit_ReturnsCloneHead`'s setup rather than duplicating it.)

- [ ] **Step 2: fail**, **Step 3: Implement** — append to `stage.go`:

```go
// StagedTreeHash stages the work tree (same exclusions as CaptureDiff) and
// returns the git tree hash of the staged index. This is the identity of
// what the reviewer sees, what the verifier runs against, and what the
// push-time guard re-checks — one hash, three uses.
func (s *Stage) StagedTreeHash() (string, error) {
	if err := s.stageAll(); err != nil {
		return "", err
	}
	out, err := s.git("write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ExportStaged materializes the staged tree into dst (created fresh) via
// git archive | tar. The copy — not the live work tree — is what gets
// mounted into the verifier VM, so verification cannot mutate what will be
// committed, and the work tree cannot change what was verified.
func (s *Stage) ExportStaged(dst string) error {
	tree, err := s.StagedTreeHash()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	ctx := s.execCtx()
	arch := exec.CommandContext(ctx, "git",
		"--git-dir="+s.gitDir, "archive", "--format=tar", tree)
	arch.Dir = s.WorkDir
	arch.WaitDelay = gitWaitDelay
	untar := exec.CommandContext(ctx, "tar", "-x", "-C", dst)
	untar.WaitDelay = gitWaitDelay
	pipe, err := arch.StdoutPipe()
	if err != nil {
		return err
	}
	untar.Stdin = pipe
	if err := arch.Start(); err != nil {
		return err
	}
	if err := untar.Start(); err != nil {
		_ = arch.Wait()
		return err
	}
	archErr := arch.Wait()
	untarErr := untar.Wait()
	if archErr != nil {
		return fmt.Errorf("stage: git archive: %w", archErr)
	}
	if untarErr != nil {
		return fmt.Errorf("stage: untar export: %w", untarErr)
	}
	return nil
}
```

(Adjust to the file's actual `git`/ctx helpers — `s.git` already binds `--git-dir`/`--work-tree`/hook-neutralization; `write-tree` can go through it. `archive` needs the explicit form shown because `s.git` adds `--work-tree`, which `git archive` ignores harmlessly — prefer `s.git("archive", ...)`-style via a variant only if the existing helper returns stdout unmixed with stderr; the diff-capped path shows stdout/stderr are combined in `runGit`, which would corrupt the tar. Keep the explicit `exec.CommandContext` form above for the tar pipeline.)

- [ ] **Step 4: Pass** — `go test -race ./internal/stage/`.
- [ ] **Step 5: Commit** — `git commit -m "feat(stage): staged-tree hash + sealed export for the verifier"`

---

### Task 5: runner — verifier VM argv

**Files:**
- Modify: `internal/runner/runner.go`
- Test: `internal/runner/runner_test.go`

**Interfaces:**
- `type VerifySpec struct { TaskID, Network, ImageRef, VerifyDir string; Argv []string; MemoryGB, CPUs int }`
- `runner.BuildVerifyArgs(s VerifySpec) []string` — container name `verify-<id>`; NO grant/proxy env; mounts VerifyDir at /work; command = deny-all pin, HOME export, privilege drop, then the argv.
- `runner.VerifyContainerName(taskID string) string` = `"verify-" + taskID` (broker uses it for force-delete; brokerd reaper matches it).

- [ ] **Step 1: Failing test** — create/extend `internal/runner/runner_test.go`:

```go
func TestBuildVerifyArgs_ShapeAndContainment(t *testing.T) {
	args := BuildVerifyArgs(VerifySpec{
		TaskID: "0123456789abcdef0123456789abcdef", Network: "drydock-egress",
		ImageRef: "drydock-sandbox:latest", VerifyDir: "/stage/verify",
		Argv: []string{"go", "test", "./..."}, MemoryGB: 4, CPUs: 4,
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--name verify-0123456789abcdef0123456789abcdef",
		"--cap-add CAP_NET_ADMIN", // root installs the deny-all pin, then drops
		"--mount type=bind,source=/stage/verify,target=/work",
		"policy drop",                     // the inline nft pin
		"/usr/local/bin/drop-agent.sh",    // privilege drop before repo code
		"HOME=/home/agent",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("verify argv missing %q:\n%s", want, joined)
		}
	}
	// The command argv must arrive as positional args after the sh -c script
	// (never interpolated into the script string — shell-injection surface).
	// Exact tail shape: ..., "-c", <script>, "sh", "go", "test", "./..."
	n := len(args)
	if n < 4 || args[n-4] != "sh" || args[n-3] != "go" || args[n-2] != "test" || args[n-1] != "./..." {
		t.Errorf("argv tail = %v, want [... sh go test ./...]", args[max(0, n-4):])
	}
	for _, a := range args {
		if strings.Contains(a, "tok_") || strings.Contains(a, "PROXY") ||
			strings.Contains(a, "AUTH_TOKEN") || strings.Contains(a, "API_KEY") {
			t.Errorf("verify argv leaks credential/proxy material: %q", a)
		}
	}
}
```

- [ ] **Step 2: fail**, **Step 3: Implement** — append to `runner.go`:

```go
// verifyScript is the in-VM bootstrap for a verification command. Root
// installs a DENY-ALL nft pin (loopback only — no gateway, no squid: the
// verifier's claim is "no network", strictly tighter than the agent's
// allowlist), then execs the command through drop-agent.sh so repo code
// runs unprivileged and cannot flush the pin (same A2 mechanism the agent
// VM relies on). HOME must be the agent user's writable home (v0.6.6 #198)
// or toolchains that write under $HOME fail spuriously.
const verifyScript = `set -e
nft -f - <<'EOF'
table inet verify_pin {
  chain input   { type filter hook input   priority 0; policy drop; iif "lo" accept; }
  chain forward { type filter hook forward priority 0; policy drop; }
  chain output  { type filter hook output  priority 0; policy drop; oif "lo" accept; }
}
EOF
export HOME=/home/agent
cd /work
exec /usr/local/bin/drop-agent.sh "$@"
`

// VerifySpec describes one verification command's VM run.
type VerifySpec struct {
	TaskID    string
	Network   string
	ImageRef  string
	VerifyDir string   // sealed staged-tree export, mounted rw at /work
	Argv      []string // the verification command, passed as positionals
	MemoryGB  int
	CPUs      int
}

// VerifyContainerName is the container name for a task's verifier VMs.
// Distinct from "task-<id>" so kill/reap paths for the two never collide.
func VerifyContainerName(taskID string) string { return "verify-" + taskID }

// BuildVerifyArgs returns the argv (after the `container` binary) for one
// verification command. No credential or proxy env is ever injected here —
// the verifier's evidence value depends on it having nothing to leak.
func BuildVerifyArgs(s VerifySpec) []string {
	args := []string{
		"run", "--rm",
		"--name", VerifyContainerName(s.TaskID),
		"--cap-add", "CAP_NET_ADMIN",
		"--memory", fmt.Sprintf("%dG", s.MemoryGB),
		"--cpus", fmt.Sprintf("%d", s.CPUs),
		"--network", s.Network,
		"--env", "HOME=/home/agent",
		"--mount", fmt.Sprintf("type=bind,source=%s,target=/work", s.VerifyDir),
		"--entrypoint", "/bin/sh",
		s.ImageRef,
		"-c", verifyScript, "sh",
	}
	return append(args, s.Argv...)
}
```

- [ ] **Step 4: Pass** — `go test -race ./internal/runner/`.
- [ ] **Step 5: Commit** — `git commit -m "feat(runner): verifier VM argv — deny-all pin, privilege drop, sealed mount"`

---

### Task 6: broker — the verifying stage

**Files:**
- Modify: `internal/broker/taskstate.go` (add `StageVerifying TaskStage = "verifying"`)
- Modify: `internal/broker/broker.go` (Broker fields, taskRun fields, `runVerify`, HandleTask wiring at the `broker.go:601-609` seam, `writeBrief` integration, push-time tree guard in `finishPush`)
- Create: `internal/broker/verify.go` + `internal/broker/verify_test.go` (keep the new logic in its own file)
- Modify: `internal/broker/metrics.go` + `internal/audit/audit.go` (`StageMs.Verifying`)
- Modify: `internal/broker/admin.go` (`HandleHealth` verifying count)
- Modify: `cmd/brokerd/main.go` (wire `Broker.Verify` + `DefaultVerifyTimeout` docs; extend orphan reaper to `verify-<32hex>`)
- Modify: `cmd/drydock/prune.go` (`.verify.log` suffix) + its test
- Modify: `internal/broker/brief_test.go` (drop the dead `filepath.Join(...) == ""` assertion while touching the file)

**Interfaces:**
- `Broker.Verify map[string]VerifyRepo` with `type VerifyRepo struct { Commands [][]string; Timeout time.Duration; Required bool }` (broker-local mirror; brokerd maps from config).
- `const DefaultVerifyTimeout = 10 * time.Minute` (exported — claims table cites it in Task 7).
- `taskRun` gains: `verify *trustbrief.Verification`, `verifyDur time.Duration`.
- `(tr *taskRun) runVerify() bool` — returns false only when it already emitted the terminal event (required-and-not-passed, or cancelled).
- New outcome string `"verify_failed"` (metrics `outcome`, stream result event).

**Behavior spec for `runVerify` (implement in `internal/broker/verify.go`):**

1. `cfg, ok := b.Verify[repokey.Normalize(tr.repoRef)]`; if `!ok`: `tr.verify = &trustbrief.Verification{Status: trustbrief.VerificationNotConfigured}`; return true. (Import `drydock/internal/repokey`.)
2. `b.setStage(tr.id, StageVerifying)`; emit `{"event":"stage","stage":"verifying","task_id":id,"commands":len(cfg.Commands)}`; record start time.
3. Export the sealed tree via optional capability on `tr.st`:
   `es, ok := tr.st.(interface { StagedTreeHash() (string, error); ExportStaged(string) error })` — if unavailable or either call errors → `tr.verify = &{Status: Inconclusive, Network: "denied", Credentials: "none"}` plus a `slog.Warn`, and continue per required-gating below (inconclusive blocks only when Required). Export dir: `filepath.Join(stageRootOf(tr), "verify")` — the stage's Root; since `taskStage` only exposes `WorkDir()`, derive as `filepath.Dir(st.WorkDir())` and note WHY in a comment (Prepare lays out `<root>/work` + `<root>/git`; the verify export sits beside them inside the same quota image).
4. Open `<AuditRoot>/<id>.verify.log` with `os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600`. Wrap with a fresh `newOutputCap(maxTaskOutputBytes, cancelThisVerify)`.
5. For each command, sequential: child ctx `context.WithTimeout(tr.ctx, timeout)` where `timeout = cfg.Timeout` or `DefaultVerifyTimeout` when 0; build argv via `runner.BuildVerifyArgs`; run via the existing `b.runAgent` seam (same signature) with stdout+stderr both going to the capped log writer; measure duration.
   - Exit 0 → `VerifyCmdPassed`.
   - `exec.ExitError` → `VerifyCmdFailed` with its code; **fail fast**: mark all remaining commands `VerifyCmdSkipped` and stop.
   - ctx deadline exceeded → `VerifyCmdTimedOut`; force-delete `runner.VerifyContainerName(tr.id)` through the same bounded container-delete path `runSandbox` uses for `task-<id>` (search `"delete", "--force"` in broker.go and reuse that helper/pattern verbatim); stop.
   - Any other error (CLI missing, image missing) → `VerifyCmdError`; stop.
   - `tr.ctx` cancelled (kill/shutdown) → force-delete the VM, emit the standard cancelled terminal (mirror how `runSandbox` handles `tr.ctx.Err() != nil`), return false.
   - Output cap tripped → treat as `VerifyCmdError` with a log-flood reason; stop.
6. Overall status: all passed → `VerificationPassed`; any `failed` → `VerificationFailed`; else (timed_out/error present) → `VerificationInconclusive`. Populate `tr.verify = &trustbrief.Verification{Status, Network: "denied", Credentials: "none", TreeSHA: <hash>, LogSHA256: <sha256 of the log file's bytes, computed by re-reading the closed file>, Commands: [...]}`; `tr.verifyDur = time.Since(start)`.
7. Required gating: if `cfg.Required && tr.verify.Status != trustbrief.VerificationPassed`: `tr.outcome = "verify_failed"`; append the broker-authored result line the same way other terminal paths do (reuse `appendBrokerResult`-equivalent — match the current exit-path pattern in `runSandbox`); emit `{"event":"result","outcome":"verify_failed","task_id":id,"verify_status":status,"hint":"drydock inspect <id> · verification log: <path>"}`; return false. Otherwise return true.

**HandleTask wiring** — at `broker.go:609`, replace `tr.pushAndOpenPR(diff)` with:

```go
	if !tr.runVerify() {
		return // runVerify emitted the terminal event
	}
	tr.pushAndOpenPR(diff)
```

**writeBrief integration** — where `writeBrief` currently hardcodes `Verification{Status: VerificationNotConfigured}` and the missing-evidence line: use `*tr.verify` when set (it always is after runVerify — not_configured is set on the no-config path); include the "verification not configured…" missing-evidence line ONLY when status is `not_configured`; when inconclusive add `"verification inconclusive — treat as unverified"`.

**Push-time tree guard** — in `finishPush`, before the push machinery: if `tr.verify != nil && tr.verify.TreeSHA != ""`, re-check via the same optional capability: `h, err := es.StagedTreeHash()`; on error or `h != tr.verify.TreeSHA`, fail the push exactly like a push error (reuse the `push_failed` emission path) with reason `"verified-tree mismatch: work tree changed after verification"`. This is what makes "pushed tree == verified tree" enforced, not asserted.

**Metrics** — `audit.StageMs` gains `Verifying int64 \`json:"verifying,omitempty"\`` (omitempty: pre-verifier rows unchanged); `appendMetrics` sets `m.StageMs.Verifying = tr.verifyDur.Milliseconds()`.

**HandleHealth** (`internal/broker/admin.go`) — add a `StageVerifying` case and a `"verifying"` key to the JSON.

**brokerd** (`cmd/brokerd/main.go`) — map `cfg.Verify.Repos` → `broker.Verify` (`map[string]broker.VerifyRepo`) in the Broker literal; extend the orphan reaper: alongside `taskContainerRE` add `verifyContainerRE = regexp.MustCompile(`^verify-[0-9a-f]{32}$`)` and delete matches of either (same loop).

**prune** — `knownSuffixes` gains `".verify.log"`; usage text updated; test fixture extended (same pattern as `.brief.json` was added).

- [ ] **Step 1: Write failing tests** in `internal/broker/verify_test.go`, driving `runVerify` through the `b.runAgent` seam (mirror `handle_task_test.go` harness: `testBroker`, `fakeStage`, `fakeGrant`, `submit`). Required cases (write them all, complete, using the harness's real helpers):
  - `TestRunVerify_NotConfigured_PassesThrough` — no `Verify` entry → brief gets `not_configured`, task pushes as before (auto-approve flow), stage never shows `verifying`.
  - `TestRunVerify_AllPass_RecordedInBrief` — fake runAgent writes noise to stdout and returns nil for each command → brief `Verification.Status=="passed"`, per-command `exit_code==0`, `network=="denied"`, `credentials=="none"`, `stage_ms.verifying > 0` in the final metrics row, `<id>.verify.log` exists 0600 and contains the noise, `LogSHA256` matches the file's sha256. fakeStage must implement `StagedTreeHash`/`ExportStaged` for this test (add methods to a wrapper fake, NOT to the shared `fakeStage` — the brief tests rely on the shared fake lacking `BaseCommit`).
  - `TestRunVerify_FailFastAndAdvisory` — first command exits 1 (`writesResult`-style fake returning `&exec.ExitError{}` is not constructible; have the fake return an error built via a real `exec.Command("false")` run, or a stub error implementing `ExitCode() int` — check how `runSandbox` extracts codes and match it) → statuses `[failed, skipped]`, overall `failed`, task STILL reaches the gate (advisory), awaiting_approval event carries `"verify":"failed"`.
  - `TestRunVerify_RequiredBlocksOnFailure` — `Required: true`, command fails → terminal `outcome=="verify_failed"`, no gate entered, broker-authored result line present, metrics `outcome=="verify_failed"`.
  - `TestRunVerify_RequiredBlocksOnInconclusive` — export capability absent → inconclusive → with `Required: true` the task is blocked (fail-closed), with `Required: false` it proceeds and the brief says inconclusive.
  - `TestRunVerify_TimeoutIsInconclusiveNeverPassed` — fake runAgent blocks until ctx done, per-command timeout 50ms → command `timed_out`, overall `inconclusive`, the force-delete path was invoked (assert via the runCmd/container-delete seam the broker uses — inject/observe the same way existing kill tests do).
  - `TestRunVerify_ForgedPassOutputCannotFlipVerdict` — fake runAgent writes `ALL TESTS PASS ✓ exit 0` to stdout but returns a non-zero-exit error → overall `failed`. (This is the F-07 headline test — name it in the commit message.)
  - `TestFinishPush_TreeMismatchFailsClosed` — verify ran with TreeSHA "aaa", fake's `StagedTreeHash` now returns "bbb" → outcome `push_failed`, reason mentions mismatch, nothing pushed.
- [ ] **Step 2: fail** — `go test ./internal/broker/ -run Verify` fails.
- [ ] **Step 3: Implement** per the behavior spec above. Keep `runVerify` + helpers in `verify.go`; only the seam wiring touches `broker.go`.
- [ ] **Step 4: Pass** — `go test -race -count=1 ./internal/broker/ ./internal/audit/ ./cmd/brokerd/ ./cmd/drydock/` all green; then full `go test -race -count=1 ./...`.
- [ ] **Step 5: Commit** — `git commit -m "feat(broker): independent verifier stage — sealed tree, deny-all VM, broker-observed verdicts"`

---

### Task 7: CLI, Web UI, docs, claims

**Files:**
- Modify: `cmd/drydock/inspect.go` (+test) — render the verification block: status line (`verify   passed · network denied · no credentials · tree abc123…` / failed with per-command `argv → exit N (duration)` lines / `not_configured` as today via the gap list), log path hint.
- Modify: `cmd/drydock/status.go` (+ its test) — `healthBody` gains `Verifying int` (json `verifying`); render segment `· N verifying` in the breakdown line.
- Modify: `internal/webui/assets/app.js` — add `verifying: "verifying"` to the stage label map (line ~317) and include `"verifying"` in the active-stages array (line ~177).
- Modify: `site/docs/submitting-tasks.md` — stage list (add `verifying` between `running` and `awaiting_approval`), metrics `stage_ms` field list, `verify_failed` outcome, a "Verification" section documenting the config block + advisory/required semantics + the sealed-tree/no-network/no-credentials guarantees (state them exactly as enforced, no stronger).
- Modify: `site/docs/daemon.md` — restart section: a task killed mid-verify ends `interrupted` (like mid-run); verification re-runs only via `drydock retry`.
- Modify: `THREAT_MODEL.md` — extend the A5 evidence paragraph: the reviewer now gets diff + trace + broker-observed verification results; add the verifier's properties (fresh VM, deny-all pin behind the A2 privilege drop, no credentials, sealed tree) WITHOUT claiming a new A-code (the VM-backed adversarial test ships in Task 8; promote to a lettered claim only when `make redteam-vm` covers it).
- Modify: `cmd/docs-build/claims.go` — add a row for `DefaultVerifyTimeout` (cite `TestRunVerify_TimeoutIsInconclusiveNeverPassed`) and note verifier output shares the existing 256 MiB cap row; regenerate `site/docs/security-defaults.md` via the docs-build path so `TestSecurityDefaultsPageCurrent` passes.
- Modify: `CHANGELOG.md` — `## Unreleased` entry describing the feature in the file's established voice.

- [ ] **Step 1** failing tests: inspect rendering test (extend `writeTestBrief` with a populated Verification; assert status/argv/exit-code/`network denied` strings appear and that a `not_configured` brief renders no verification section beyond the gap line); status test (health JSON with `"verifying":1` renders `1 verifying`).
- [ ] **Step 2** fail → **Step 3** implement → **Step 4** `go test -race -count=1 ./... && go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...`.
- [ ] **Step 5: Commit** — `git commit -m "feat(cli,docs): surface the verifying stage — inspect, status, web UI, operator docs, claims"`

---

### Task 8: VM-backed adversarial test + make wiring

**Files:**
- Modify: `tests/integration/redteam_test.go` (or a sibling `verify_test.go` in that package, same build tag)
- Modify: `Makefile` (`redteam-vm` target includes the new test)

Add `TestRedteam_V1_VerifierVMHasNoNetworkAndNoCredentials` following the existing A1/A2 test structure in that file (build-tagged, macOS+container-runtime only, cannot run in hosted CI):
- Build the exact argv `runner.BuildVerifyArgs` produces for a scratch dir and a probe command that: (a) dumps `/proc/self/environ` and asserts no `tok_`/`sk-`/`PROXY` material, (b) attempts HTTPS to example.com, raw DNS to 1.1.1.1, and a direct-IP connect **to the gateway IP itself** — all must fail (the verifier pin is tighter than the agent pin: even the gateway is unreachable), (c) attempts `nft flush ruleset` as the dropped user and asserts EPERM, (d) asserts `$HOME` is writable.
- Mirror the assertion helpers the A2 test uses; the test passes only when every probe is blocked/clean.
- Wire into the `redteam-vm` Makefile target beside A1/A2/A7. Verify the file compiles under the build tag: `go vet -tags integration ./tests/integration/`.

- [ ] Commit — `git commit -m "test(redteam): V1 — verifier VM has no network, no credentials, no privilege"`

---

## Final verification (whole branch)

- `go vet ./...`; `go test -race -count=1 ./...`; `gofmt -l internal/ cmd/ tests/` silent; `staticcheck ./...` clean; `go vet -tags integration ./tests/integration/`.
- `go test -run=NONE -fuzz=FuzzAnalyze -fuzztime=30s ./internal/trustbrief/` (difffacts changed).
- Grep gates: no credential/proxy env in any `BuildVerifyArgs` path; every new artifact writer uses `O_NOFOLLOW`; no Brief/metrics field sourced from VM output; `verify_failed` present in metrics-outcome docs and code both.
