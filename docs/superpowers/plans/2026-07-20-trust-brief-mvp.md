# Trust Brief MVP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every task that reaches the diff-approval gate gets a broker-observed evidence report (`<id>.brief.json`) — structural diff facts, risk flags, policy snapshot, metered spend — rendered by `drydock review`, `drydock pending`, and a new `drydock inspect` command.

**Architecture:** A new dependency-free package `internal/trustbrief` computes structural facts from the host-git-produced diff (pure functions) and defines the Brief schema + 0600/O_NOFOLLOW persistence. The broker assembles a Brief in `pushAndOpenPR` before the approval gate, from host-observed inputs only (host git, gateway meter, broker config). The CLI reads the artifact client-side from the audit dir, exactly like `.diff` today. No broker HTTP API changes.

**Tech Stack:** Go stdlib only. No new dependencies.

## Decision record (locked by the product/security review; do not relitigate in-task)

- Every Brief field except nothing is broker-observed; the Brief contains **no agent-authored content** in v1 (agent summary deliberately dropped — `missing_evidence` says so).
- Verification is `"not_configured"` in v1 (the verifier VM is the next feature; the schema field exists now so it never needs a migration).
- Diff policy caps / second-look acknowledgment are OUT of this MVP (they are PR6 of the delivery plan).
- Brief write failure must never fail the task — warn and continue (the Brief is advisory evidence in v1).

## Global Constraints

- New audit artifacts: mode `0600`, opened with `syscall.O_NOFOLLOW`, under `Broker.AuditRoot` — exactly like `<id>.diff` (see `internal/broker/gates.go:97-98` and `broker.go:402-404`).
- Never derive a Brief fact from agent output. Broker-observed sources only: host git, `grant.Spent()`, broker config fields, `taskRun` request fields.
- Attacker-shaped input discipline: the diff *content* is attacker-controlled data. The parser must be bounded (no unbounded line buffering, capped path lengths, capped file counts) and must never panic on malformed input.
- Anything printed to a terminal that originates from the diff (file paths) must be control-character-stripped and length-capped before printing.
- Repo URLs in the Brief must have userinfo redacted.
- Go stdlib only; `go vet ./...` clean; all tests pass under `go test -race`.
- Match existing comment style: comments state constraints/why, not what.
- Commit messages: repo convention is `type(scope): summary` or `type: summary` (see `git log`). Never add a Generated-with-Claude-Code footer to PR bodies. End commit messages with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.

---

### Task 1: `internal/trustbrief` diff-structure analyzer

**Files:**
- Create: `internal/trustbrief/difffacts.go`
- Create: `internal/trustbrief/difffacts_test.go`

**Interfaces:**
- Produces: `trustbrief.Analyze(diff string) DiffFacts`, types `DiffFacts{SHA256, Bytes, Truncated, Files []FileChange, FilesOmitted int, Flags []Flag}`, `FileChange{Path string; Adds, Dels int}`, `Flag{Kind string; Paths []string}`, and flag-kind constants `FlagBinary/FlagSymlink/FlagExecBit/FlagDependency/FlagLockfile/FlagCIWorkflow/FlagGitMeta`. Task 2 embeds `DiffFacts` in the Brief; Task 4 renders `Flags`.

- [ ] **Step 1: Write the failing test**

Create `internal/trustbrief/difffacts_test.go`:

```go
package trustbrief

import (
	"strings"
	"testing"
)

func flagPaths(f DiffFacts, kind string) []string {
	for _, fl := range f.Flags {
		if fl.Kind == kind {
			return fl.Paths
		}
	}
	return nil
}

func TestAnalyze_CountsAndIdentity(t *testing.T) {
	diff := "diff --git a/main.go b/main.go\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1,3 +1,4 @@\n" +
		" package main\n" +
		"-var old = 1\n" +
		"+var new = 1\n" +
		"+var extra = 2\n"
	f := Analyze(diff)
	if f.Bytes != len(diff) {
		t.Errorf("Bytes = %d, want %d", f.Bytes, len(diff))
	}
	if len(f.SHA256) != 64 {
		t.Errorf("SHA256 = %q, want 64 hex chars", f.SHA256)
	}
	if f.Truncated {
		t.Error("Truncated = true for a complete diff")
	}
	if len(f.Files) != 1 || f.Files[0].Path != "main.go" || f.Files[0].Adds != 2 || f.Files[0].Dels != 1 {
		t.Errorf("Files = %+v, want [{main.go 2 1}]", f.Files)
	}
	if len(f.Flags) != 0 {
		t.Errorf("Flags = %+v, want none for a plain source change", f.Flags)
	}
}

func TestAnalyze_FlagKinds(t *testing.T) {
	cases := []struct {
		name, diff, kind, path string
	}{
		{"binary-differ",
			"diff --git a/img.png b/img.png\nnew file mode 100644\nBinary files /dev/null and b/img.png differ\n",
			FlagBinary, "img.png"},
		{"binary-patch",
			"diff --git a/blob.bin b/blob.bin\nindex 1111111..2222222 100644\nGIT binary patch\ndelta 12\nzcmZ12\n",
			FlagBinary, "blob.bin"},
		{"symlink-new",
			"diff --git a/link b/link\nnew file mode 120000\n--- /dev/null\n+++ b/link\n@@ -0,0 +1 @@\n+/etc/passwd\n",
			FlagSymlink, "link"},
		{"symlink-mode-change",
			"diff --git a/f b/f\nold mode 100644\nnew mode 120000\n",
			FlagSymlink, "f"},
		{"exec-bit-new",
			"diff --git a/run.sh b/run.sh\nnew file mode 100755\n--- /dev/null\n+++ b/run.sh\n@@ -0,0 +1 @@\n+echo hi\n",
			FlagExecBit, "run.sh"},
		{"exec-bit-flip",
			"diff --git a/tool.py b/tool.py\nold mode 100644\nnew mode 100755\n",
			FlagExecBit, "tool.py"},
		{"dependency-manifest",
			"diff --git a/go.mod b/go.mod\nindex 1111111..2222222 100644\n--- a/go.mod\n+++ b/go.mod\n@@ -1 +1,2 @@\n module x\n+require evil.example/pkg v1.0.0\n",
			FlagDependency, "go.mod"},
		{"lockfile",
			"diff --git a/package-lock.json b/package-lock.json\nindex 1111111..2222222 100644\n--- a/package-lock.json\n+++ b/package-lock.json\n@@ -1 +1 @@\n-{}\n+{\"x\":1}\n",
			FlagLockfile, "package-lock.json"},
		{"ci-workflow",
			"diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml\nindex 1111111..2222222 100644\n--- a/.github/workflows/ci.yml\n+++ b/.github/workflows/ci.yml\n@@ -1 +1,2 @@\n on: push\n+run: curl evil\n",
			FlagCIWorkflow, ".github/workflows/ci.yml"},
		{"git-metadata",
			"diff --git a/.gitattributes b/.gitattributes\nnew file mode 100644\n--- /dev/null\n+++ b/.gitattributes\n@@ -0,0 +1 @@\n+* filter=evil\n",
			FlagGitMeta, ".gitattributes"},
		{"githooks-dir",
			"diff --git a/.githooks/pre-commit b/.githooks/pre-commit\nnew file mode 100755\n--- /dev/null\n+++ b/.githooks/pre-commit\n@@ -0,0 +1 @@\n+curl evil\n",
			FlagGitMeta, ".githooks/pre-commit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths := flagPaths(Analyze(tc.diff), tc.kind)
			found := false
			for _, p := range paths {
				if p == tc.path {
					found = true
				}
			}
			if !found {
				t.Errorf("Analyze flags[%s] = %v, want to contain %q", tc.kind, paths, tc.path)
			}
		})
	}
}

func TestAnalyze_TruncationMarker(t *testing.T) {
	// The exact marker stage.gitDiffCapped appends (internal/stage/stage.go).
	diff := "diff --git a/x b/x\n+y\n\n... [diff truncated at 32 MiB; the full change is still committed] ...\n"
	if f := Analyze(diff); !f.Truncated {
		t.Error("Truncated = false, want true when the stage truncation marker is present")
	}
}

func TestAnalyze_QuotedPath(t *testing.T) {
	diff := "diff --git \"a/we ird.sh\" \"b/we ird.sh\"\nnew file mode 100755\n"
	paths := flagPaths(Analyze(diff), FlagExecBit)
	if len(paths) != 1 || paths[0] != "we ird.sh" {
		t.Errorf("quoted path = %v, want [we ird.sh]", paths)
	}
}

// Adversarial shapes: the diff body is attacker-controlled. Analyze must stay
// bounded and never panic, whatever the content lines contain.
func TestAnalyze_HostileInputs(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		f := Analyze("")
		if f.Bytes != 0 || len(f.Files) != 0 {
			t.Errorf("empty diff = %+v", f)
		}
	})
	t.Run("garbage", func(t *testing.T) {
		_ = Analyze("\x00\xff not a diff at all\n+++ dangling\n@@ nonsense")
	})
	t.Run("huge-single-line", func(t *testing.T) {
		// A 10 MiB content line must not be buffered wholesale by the parser.
		diff := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -0,0 +1 @@\n+" +
			strings.Repeat("A", 10<<20) + "\n"
		f := Analyze(diff)
		if len(f.Files) != 1 || f.Files[0].Adds != 1 {
			t.Errorf("huge line: Files = %+v, want one file with 1 add", f.Files)
		}
	})
	t.Run("many-files-capped", func(t *testing.T) {
		var sb strings.Builder
		for i := 0; i < maxTrackedFiles+50; i++ {
			sb.WriteString("diff --git a/f b/f\n+x\n")
		}
		f := Analyze(sb.String())
		if len(f.Files) != maxTrackedFiles {
			t.Errorf("len(Files) = %d, want cap %d", len(f.Files), maxTrackedFiles)
		}
		if f.FilesOmitted != 50 {
			t.Errorf("FilesOmitted = %d, want 50", f.FilesOmitted)
		}
	})
	t.Run("long-path-capped", func(t *testing.T) {
		diff := "diff --git a/" + strings.Repeat("d/", 400) + "x b/" + strings.Repeat("d/", 400) + "x\n+y\n"
		f := Analyze(diff)
		if len(f.Files) == 1 && len(f.Files[0].Path) > maxPathBytes {
			t.Errorf("path len = %d, want <= %d", len(f.Files[0].Path), maxPathBytes)
		}
	})
}

func FuzzAnalyze(f *testing.F) {
	f.Add("diff --git a/x b/x\nnew file mode 120000\n+++ b/x\n+target\n")
	f.Add("Binary files a and b differ\n")
	f.Add("\"a/we ird\"")
	f.Fuzz(func(t *testing.T, diff string) {
		_ = Analyze(diff) // must not panic
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/trustbrief/`
Expected: FAIL — package does not exist / `Analyze` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/trustbrief/difffacts.go`:

```go
// Package trustbrief assembles broker-observed evidence about a completed
// task: what the review diff structurally contains, what policy bounded the
// run, and what was spent. Every fact is computed host-side from host-git
// output and broker state; nothing here is derived from agent claims — that
// separation is the point of the artifact.
package trustbrief

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
)

// FileChange is one changed file with its hunk-line counts.
type FileChange struct {
	Path string `json:"path"`
	Adds int    `json:"adds"`
	Dels int    `json:"dels"`
}

// Flag marks a structural risk pattern a reviewer should look at first.
// Kinds are stable identifiers (they will later drive the acknowledgment
// flow); Paths are capped examples, not an exhaustive list.
type Flag struct {
	Kind  string   `json:"kind"`
	Paths []string `json:"paths"`
}

// Flag kinds. Stable strings: they are persisted in brief.json artifacts.
const (
	FlagBinary     = "binary-changed"
	FlagSymlink    = "symlink"
	FlagExecBit    = "exec-bit"
	FlagDependency = "dependency-manifest"
	FlagLockfile   = "lockfile"
	FlagCIWorkflow = "ci-workflow"
	FlagGitMeta    = "git-metadata"
)

// DiffFacts is the broker-computed structural summary of the review diff.
type DiffFacts struct {
	SHA256       string       `json:"sha256"`
	Bytes        int          `json:"bytes"`
	Truncated    bool         `json:"truncated"`
	Files        []FileChange `json:"files"`
	FilesOmitted int          `json:"files_omitted,omitempty"`
	Flags        []Flag       `json:"flags"`
}

// Bounds. The diff body is attacker-controlled: a hostile task can emit any
// bytes it likes into tracked files. Everything the parser retains is capped
// so a crafted diff cannot balloon the Brief or the broker's memory.
const (
	maxTrackedFiles  = 4096 // FileChange entries retained; the rest are counted
	maxPathBytes     = 512  // stored path length
	maxFlagPaths     = 32   // example paths retained per flag kind
	maxHeaderLine    = 4096 // git header lines are short; longer prefixes are content
)

// Analyze computes DiffFacts from a unified diff produced by the host-side
// `git diff --cached` (stage.CaptureDiff). The diff FRAMING (headers, mode
// lines) is trusted git output; the CONTENT lines are attacker data and are
// only ever counted, never interpreted.
func Analyze(diff string) DiffFacts {
	sum := sha256.Sum256([]byte(diff))
	facts := DiffFacts{SHA256: hex.EncodeToString(sum[:]), Bytes: len(diff), Flags: []Flag{}}

	flagged := map[string][]string{} // kind -> capped example paths
	seen := map[string]map[string]bool{}
	addFlag := func(kind, p string) {
		if p == "" {
			p = "(unknown path)"
		}
		if seen[kind] == nil {
			seen[kind] = map[string]bool{}
		}
		if seen[kind][p] {
			return
		}
		seen[kind][p] = true
		if len(flagged[kind]) < maxFlagPaths {
			flagged[kind] = append(flagged[kind], p)
		}
	}

	r := bufio.NewReaderSize(strings.NewReader(diff), 64<<10)
	var cur *FileChange
	curPath := ""
	for {
		line, err := boundedLine(r)
		if line == "" && err != nil {
			break
		}
		switch {
		case strings.HasPrefix(line, "diff --git "):
			curPath = capPath(parseGitHeaderPath(line))
			if len(facts.Files) < maxTrackedFiles {
				facts.Files = append(facts.Files, FileChange{Path: curPath})
				cur = &facts.Files[len(facts.Files)-1]
			} else {
				facts.FilesOmitted++
				cur = nil
			}
			classifyPath(curPath, addFlag)
		case strings.HasPrefix(line, "new file mode "), strings.HasPrefix(line, "new mode "):
			mode := line[strings.LastIndexByte(line, ' ')+1:]
			if strings.HasPrefix(mode, "120000") {
				addFlag(FlagSymlink, curPath)
			}
			if strings.HasPrefix(mode, "100755") {
				addFlag(FlagExecBit, curPath)
			}
		case strings.HasPrefix(line, "Binary files ") && strings.HasSuffix(line, " differ"),
			line == "GIT binary patch":
			addFlag(FlagBinary, curPath)
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			// hunk file headers, not content
		case strings.HasPrefix(line, "+"):
			if cur != nil {
				cur.Adds++
			}
		case strings.HasPrefix(line, "-"):
			if cur != nil {
				cur.Dels++
			}
		case strings.HasPrefix(line, "... [diff truncated at "):
			// The exact marker stage.gitDiffCapped appends when the review
			// diff exceeded its cap. The committed change is complete; the
			// reviewer must know this summary is not.
			facts.Truncated = true
		}
		if err != nil {
			break
		}
	}

	kinds := make([]string, 0, len(flagged))
	for k := range flagged {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		facts.Flags = append(facts.Flags, Flag{Kind: k, Paths: flagged[k]})
	}
	return facts
}

// boundedLine reads one line, retaining at most maxHeaderLine bytes and
// discarding the remainder. Every line the parser *interprets* (git headers,
// mode lines) is short; content lines only need their first byte for +/-
// counting, so dropping long tails loses nothing and keeps memory bounded.
func boundedLine(r *bufio.Reader) (string, error) {
	frag, isPrefix, err := r.ReadLine()
	line := string(frag)
	if len(line) > maxHeaderLine {
		line = line[:maxHeaderLine]
	}
	for isPrefix && err == nil {
		var rest []byte
		rest, isPrefix, err = r.ReadLine()
		_ = rest // discard: only the line's prefix is ever interpreted
	}
	if err == io.EOF {
		return line, io.EOF
	}
	return line, err
}

// parseGitHeaderPath extracts the post-change (b/) path from a
// `diff --git a/X b/Y` header. Git quotes paths containing spaces or
// non-ASCII (`"b/we ird"`); those are unquoted with strconv. A pathological
// a-path that itself contains ` "b/` or ` b/` can misattribute the path —
// that degrades a flag's example path, never the parse (advisory evidence,
// not an enforcement input).
func parseGitHeaderPath(line string) string {
	rest := strings.TrimPrefix(line, "diff --git ")
	if i := strings.Index(rest, ` "b/`); i >= 0 {
		if unq, err := strconv.Unquote(rest[i+1:]); err == nil {
			return strings.TrimPrefix(unq, "b/")
		}
	}
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		return rest[i+3:]
	}
	return ""
}

func capPath(p string) string {
	if len(p) > maxPathBytes {
		return p[:maxPathBytes]
	}
	return p
}

// classifyPath adds path-based flags. The lists are deliberately small and
// high-signal: they mark files where a malicious change has outsized blast
// radius (CI executes on the host org's runners; manifests/lockfiles pull
// remote code; git metadata alters host git behavior).
func classifyPath(p string, add func(kind, path string)) {
	if p == "" {
		return
	}
	base := path.Base(p)
	switch {
	case strings.HasPrefix(p, ".github/workflows/"),
		base == ".gitlab-ci.yml", base == "Jenkinsfile",
		strings.HasPrefix(p, ".circleci/"), base == "azure-pipelines.yml":
		add(FlagCIWorkflow, p)
	}
	switch base {
	case "package.json", "go.mod", "requirements.txt", "pyproject.toml",
		"Cargo.toml", "Gemfile", "pom.xml", "build.gradle", "build.gradle.kts":
		add(FlagDependency, p)
	case "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "go.sum",
		"Cargo.lock", "Gemfile.lock", "poetry.lock", "uv.lock", "composer.lock":
		add(FlagLockfile, p)
	case ".gitattributes", ".gitmodules":
		add(FlagGitMeta, p)
	}
	if strings.HasPrefix(p, ".githooks/") {
		add(FlagGitMeta, p)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/trustbrief/`
Expected: PASS (all tests including hostile inputs).

Run: `go test -run=NONE -fuzz=FuzzAnalyze -fuzztime=30s ./internal/trustbrief/`
Expected: no crashers in 30s.

- [ ] **Step 5: Vet and commit**

Run: `go vet ./internal/trustbrief/`
```bash
git add internal/trustbrief/
git commit -m "feat(trustbrief): structural diff analyzer with risk flags"
```

---

### Task 2: Brief schema, policy fingerprint, and persistence

**Files:**
- Create: `internal/trustbrief/brief.go`
- Create: `internal/trustbrief/brief_test.go`

**Interfaces:**
- Consumes: `DiffFacts` from Task 1.
- Produces (used by Task 3's broker wiring and Task 4's CLI):
  - `type Brief struct { SchemaVersion int; TaskID string; GeneratedAt time.Time; Task TaskFacts; Runtime RuntimeFacts; Policy PolicyFacts; Spend SpendFacts; Diff DiffFacts; Verification Verification; MissingEvidence []string }`
  - `TaskFacts{InstructionSHA256, RepoRef, BaseCommit string; Sensitive, AutoApprove bool}`
  - `RuntimeFacts{ImageRef, Agent, Vendor, Model string}`
  - `PolicyFacts{SnapshotSHA256 string; BudgetUSD float64; BudgetHard bool; MaxRequests int; TimeoutSeconds int; EgressDefault, EgressWidened []string}`
  - `SpendFacts{USDBrokerMetered float64; DurationMs int64}`
  - `Verification{Status string}` with const `VerificationNotConfigured = "not_configured"`
  - `func HashInstruction(s string) string`
  - `func RedactRepoRef(ref string) string`
  - `func (p PolicyFacts) Fingerprint() string`
  - `func Write(dir, id string, b Brief) error` → writes `<dir>/<id>.brief.json`, 0600, O_NOFOLLOW
  - `func Read(dir, id string) (Brief, error)` → id must match `^[0-9a-f]{32}$` (same grammar as webui), O_NOFOLLOW open
  - `const Suffix = ".brief.json"`

- [ ] **Step 1: Write the failing test**

Create `internal/trustbrief/brief_test.go`:

```go
package trustbrief

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func sampleBrief() Brief {
	return Brief{
		SchemaVersion: 1,
		TaskID:        "0123456789abcdef0123456789abcdef",
		GeneratedAt:   time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
		Task: TaskFacts{
			InstructionSHA256: HashInstruction("fix the race"),
			RepoRef:           "https://github.com/o/r.git",
			BaseCommit:        "deadbeef",
			Sensitive:         true,
		},
		Runtime: RuntimeFacts{ImageRef: "drydock-sandbox:latest", Agent: "claude", Vendor: "anthropic", Model: "model-x"},
		Policy: PolicyFacts{
			BudgetUSD: 2, TimeoutSeconds: 1800,
			EgressDefault: []string{"api.anthropic.com:443"},
			EgressWidened: []string{"api.example.com:443"},
		},
		Spend:           SpendFacts{USDBrokerMetered: 0.12, DurationMs: 60000},
		Diff:            Analyze("diff --git a/x b/x\n+y\n"),
		Verification:    Verification{Status: VerificationNotConfigured},
		MissingEvidence: []string{"verification not configured"},
	}
}

func TestWriteRead_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	b := sampleBrief()
	b.Policy.SnapshotSHA256 = b.Policy.Fingerprint()
	if err := Write(dir, b.TaskID, b); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, b.TaskID+Suffix))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
	got, err := Read(dir, b.TaskID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !reflect.DeepEqual(got, b) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, b)
	}
}

func TestRead_RejectsHostileIDs(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"../../etc/passwd", "abc", "ABCDEF6789abcdef0123456789abcdef", ""} {
		if _, err := Read(dir, id); err == nil {
			t.Errorf("Read(%q) succeeded, want id-shape rejection", id)
		}
	}
}

func TestWrite_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, id+Suffix)); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, id, sampleBrief()); err == nil {
		t.Error("Write through a planted symlink succeeded, want O_NOFOLLOW failure")
	}
	if data, _ := os.ReadFile(victim); string(data) != "keep" {
		t.Error("symlink target was overwritten")
	}
}

func TestPolicyFingerprint_StableAndSelfExcluding(t *testing.T) {
	p := sampleBrief().Policy
	f1 := p.Fingerprint()
	p.SnapshotSHA256 = f1
	if f2 := p.Fingerprint(); f2 != f1 {
		t.Errorf("Fingerprint changed after embedding itself: %s != %s", f2, f1)
	}
	p.BudgetUSD = 3
	if p.Fingerprint() == f1 {
		t.Error("Fingerprint identical after a policy change")
	}
	if len(f1) != 64 {
		t.Errorf("Fingerprint = %q, want 64 hex chars", f1)
	}
}

func TestRedactRepoRef(t *testing.T) {
	cases := map[string]string{
		"https://user:sekret@github.com/o/r.git": "https://github.com/o/r.git",
		"https://github.com/o/r.git":             "https://github.com/o/r.git",
		"git@github.com:o/r.git":                 "git@github.com:o/r.git",
	}
	for in, want := range cases {
		if got := RedactRepoRef(in); got != want {
			t.Errorf("RedactRepoRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// The Brief must be credential-free by construction. Two tripwires: no field
// in the struct tree may be named like a secret carrier, and a marshaled
// sample must not contain secret-shaped strings.
func TestBrief_NoSecretShapedFields(t *testing.T) {
	var check func(t *testing.T, typ reflect.Type, path string)
	check = func(t *testing.T, typ reflect.Type, path string) {
		switch typ.Kind() {
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				f := typ.Field(i)
				lower := strings.ToLower(f.Name)
				for _, bad := range []string{"token", "secret", "password", "credential", "apikey"} {
					if strings.Contains(lower, bad) {
						t.Errorf("field %s.%s is secret-shaped; the Brief must stay credential-free", path, f.Name)
					}
				}
				check(t, f.Type, path+"."+f.Name)
			}
		case reflect.Slice, reflect.Ptr, reflect.Array:
			check(t, typ.Elem(), path)
		}
	}
	check(t, reflect.TypeOf(Brief{}), "Brief")

	out, err := json.Marshal(sampleBrief())
	if err != nil {
		t.Fatal(err)
	}
	for _, pat := range []string{"tok_", "sk-ant", "ghp_", "Bearer "} {
		if strings.Contains(string(out), pat) {
			t.Errorf("marshaled brief contains secret-shaped string %q", pat)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/trustbrief/`
Expected: FAIL — `Brief` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/trustbrief/brief.go`:

```go
package trustbrief

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

// Suffix is the audit-dir filename suffix for persisted briefs, alongside
// the existing .jsonl/.diff/.widen.json per-task artifacts.
const Suffix = ".brief.json"

// VerificationNotConfigured is the v1 status: no verifier stage exists yet.
// The field is present from schema v1 so adding the verifier is a data
// change, not a schema migration.
const VerificationNotConfigured = "not_configured"

// Brief is the broker-observed evidence report for one task, generated at
// the diff-approval gate. Every field is computed host-side; by design the
// schema has nowhere to put an agent claim.
type Brief struct {
	SchemaVersion   int          `json:"schema_version"`
	TaskID          string       `json:"task_id"`
	GeneratedAt     time.Time    `json:"generated_at"`
	Task            TaskFacts    `json:"task"`
	Runtime         RuntimeFacts `json:"runtime"`
	Policy          PolicyFacts  `json:"policy"`
	Spend           SpendFacts   `json:"spend"`
	Diff            DiffFacts    `json:"diff"`
	Verification    Verification `json:"verification"`
	MissingEvidence []string     `json:"missing_evidence"`
}

type TaskFacts struct {
	InstructionSHA256 string `json:"instruction_sha256"`
	RepoRef           string `json:"repo_ref"`
	BaseCommit        string `json:"base_commit,omitempty"`
	Sensitive         bool   `json:"sensitive"`
	AutoApprove       bool   `json:"auto_approve"`
}

type RuntimeFacts struct {
	ImageRef string `json:"image_ref"`
	Agent    string `json:"agent"`
	Vendor   string `json:"vendor"`
	Model    string `json:"model,omitempty"`
}

// PolicyFacts is the effective policy that bounded this task. BudgetHard
// records whether per-request reservation (max_request_cost_usd > 0) made
// the USD cap a hard ceiling — without it the cap is post-hoc/soft, and the
// reviewer should know which guarantee they got.
type PolicyFacts struct {
	SnapshotSHA256 string   `json:"snapshot_sha256"`
	BudgetUSD      float64  `json:"budget_usd"`
	BudgetHard     bool     `json:"budget_hard"`
	MaxRequests    int      `json:"max_requests"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	EgressDefault  []string `json:"egress_default"`
	EgressWidened  []string `json:"egress_widened"`
}

type SpendFacts struct {
	USDBrokerMetered float64 `json:"usd_broker_metered"`
	DurationMs       int64   `json:"duration_ms"`
}

type Verification struct {
	Status string `json:"status"`
}

// HashInstruction returns the sha256 of the task instruction. The Brief
// stores the hash, not the text: the instruction may embed sensitive
// context, and the full text is already in the 0600 audit trace for those
// who need it.
func HashInstruction(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// RedactRepoRef strips userinfo from URL-shaped repo refs so an embedded
// clone credential (https://user:pass@host/...) never lands in the Brief.
// scp-style refs (git@host:path) carry no password and pass through.
func RedactRepoRef(ref string) string {
	if u, err := url.Parse(ref); err == nil && u.Scheme != "" && u.User != nil {
		u.User = nil
		return u.String()
	}
	return ref
}

// Fingerprint hashes the policy block (with the embedded hash field zeroed)
// so briefs, `policy explain`, and a future /admin/policy endpoint can all
// agree on one policy identity string.
func (p PolicyFacts) Fingerprint() string {
	p.SnapshotSHA256 = ""
	data, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// taskIDRE is the task-id grammar (matches the broker's 128-bit hex ids and
// the webui's validation). Read builds a path from its id argument, so the
// shape check is what keeps a hostile id from traversing out of the dir.
var taskIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Write persists the brief as <dir>/<id>.brief.json with the same defenses
// as the other audit artifacts: 0600, O_NOFOLLOW (a planted symlink fails
// instead of redirecting the write).
func Write(dir, id string, b Brief) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, id+Suffix),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

// Read loads a persisted brief. The id must match the task-id grammar —
// callers pass operator-typed input straight in.
func Read(dir, id string) (Brief, error) {
	if !taskIDRE.MatchString(id) {
		return Brief{}, fmt.Errorf("trustbrief: invalid task id %q", id)
	}
	f, err := os.OpenFile(filepath.Join(dir, id+Suffix), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return Brief{}, err
	}
	defer f.Close()
	var b Brief
	if err := json.NewDecoder(f).Decode(&b); err != nil {
		return Brief{}, fmt.Errorf("trustbrief: parse %s%s: %w", id, Suffix, err)
	}
	return b, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/trustbrief/`
Expected: PASS.

- [ ] **Step 5: Vet and commit**

Run: `go vet ./internal/trustbrief/`
```bash
git add internal/trustbrief/
git commit -m "feat(trustbrief): brief schema, policy fingerprint, 0600/O_NOFOLLOW persistence"
```

---

### Task 3: Broker assembles and persists the Brief at the gate

**Files:**
- Modify: `internal/broker/broker.go` (new Broker fields; `taskRun.sensitive`; `writeBrief`; call in `pushAndOpenPR`)
- Modify: `internal/stage/stage.go` (add `BaseCommit()`)
- Modify: `cmd/brokerd/main.go` (wire two new Broker fields from config, in the `&broker.Broker{...}` literal around line 384)
- Modify: `cmd/drydock/prune.go` (add `.brief.json` to `knownSuffixes` at line 25 and to the usage text at line ~141)
- Test: `internal/broker/brief_test.go` (create), `internal/stage/stage_test.go` (extend), `cmd/drydock/prune_test.go` (extend existing patterns)

**Interfaces:**
- Consumes: `trustbrief.Brief`/`Write`/`Analyze`/`HashInstruction`/`RedactRepoRef`/`PolicyFacts.Fingerprint`/`VerificationNotConfigured` from Tasks 1–2; `summariseExtras` (exists, `internal/broker/gates.go:153`); `egress.Domain`.
- Produces: `<AuditRoot>/<id>.brief.json` written for every task that reaches `pushAndOpenPR` (including auto-approve). New exported Broker fields `MaxRequestCostUSD float64` and `TaskMaxRequests int`. New `(*stage.Stage).BaseCommit() (string, error)`.

- [ ] **Step 1: Write the failing stage test**

Append to `internal/stage/stage_test.go` (match the file's existing helper style — it prepares real git repos in temp dirs; reuse its existing repo-setup helper if one exists, otherwise this self-contained test):

```go
func TestBaseCommit_ReturnsCloneHead(t *testing.T) {
	// Build a source repo with one commit.
	src := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"},
		{"commit", "--allow-empty", "-q", "-m", "base"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	want, err := exec.Command("git", "-C", src, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}

	st, err := Prepare(t.TempDir(), src)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	got, err := st.BaseCommit()
	if err != nil {
		t.Fatalf("BaseCommit: %v", err)
	}
	if got != strings.TrimSpace(string(want)) {
		t.Errorf("BaseCommit = %q, want %q", got, strings.TrimSpace(string(want)))
	}
}
```

(Add `"os/exec"` / `"strings"` to the test file's imports if absent. If `Prepare` in this repo rejects local paths, check how existing stage tests create a cloneable source repo and mirror that — they exist, `internal/stage/stage_test.go` clones from local fixture repos.)

- [ ] **Step 2: Implement `BaseCommit`**

Append to `internal/stage/stage.go` (add `"strings"` to imports):

```go
// BaseCommit returns the commit the work tree was cloned at — the base the
// captured diff applies to. Read from the host-only git dir, so the value
// is host-observed evidence a VM cannot influence.
func (s *Stage) BaseCommit() (string, error) {
	out, err := s.git("rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}
```

Run: `go test -race ./internal/stage/` — expected PASS.

- [ ] **Step 3: Write the failing broker test**

Create `internal/broker/brief_test.go`:

```go
package broker

import (
	"path/filepath"
	"testing"
	"time"

	"drydock/internal/egress"
	"drydock/internal/trustbrief"
)

// writeBrief is exercised directly (deterministic inputs) and via the full
// HandleTask flow below (artifact appears at the gate for real submissions).
func TestWriteBrief_BrokerObservedFields(t *testing.T) {
	auditRoot := t.TempDir()
	b := &Broker{
		AuditRoot: auditRoot, ImageRef: "sandbox:test", TaskBudget: 2,
		MaxRequestCostUSD: 0.5, TaskMaxRequests: 100, Timeout: 30 * time.Minute,
	}
	b.Cfg.Default.Domains = []egress.Domain{{Host: "api.anthropic.com", Ports: []int{443}}}
	tr := &taskRun{
		b: b, id: "0123456789abcdef0123456789abcdef",
		repoRef:     "https://user:sekret@github.com/o/r.git",
		instruction: "fix the race",
		sensitive:   true,
		autoApprove: false,
		egressExtra: []egress.Domain{{Host: "api.example.com", Ports: []int{443}}},
		agentName:   "claude", taskVendor: "anthropic", model: "model-x",
		grant:     &fakeGrant{spent: 0.12},
		taskStart: time.Now().Add(-time.Minute),
		st:        &fakeStage{workDir: t.TempDir()},
	}

	b.writeBrief(tr, "diff --git a/x b/x\nnew file mode 100755\n+y\n")

	got, err := trustbrief.Read(auditRoot, tr.id)
	if err != nil {
		t.Fatalf("brief not written: %v", err)
	}
	if got.SchemaVersion != 1 || got.TaskID != tr.id {
		t.Errorf("identity = %d/%q", got.SchemaVersion, got.TaskID)
	}
	if got.Task.RepoRef != "https://github.com/o/r.git" {
		t.Errorf("repo not redacted: %q", got.Task.RepoRef)
	}
	if got.Task.InstructionSHA256 != trustbrief.HashInstruction("fix the race") {
		t.Error("instruction hash mismatch")
	}
	if !got.Task.Sensitive || got.Task.AutoApprove {
		t.Errorf("task facts = %+v", got.Task)
	}
	if got.Runtime.Agent != "claude" || got.Runtime.Vendor != "anthropic" ||
		got.Runtime.Model != "model-x" || got.Runtime.ImageRef != "sandbox:test" {
		t.Errorf("runtime = %+v", got.Runtime)
	}
	if got.Policy.BudgetUSD != 2 || !got.Policy.BudgetHard || got.Policy.MaxRequests != 100 ||
		got.Policy.TimeoutSeconds != 1800 {
		t.Errorf("policy = %+v", got.Policy)
	}
	if len(got.Policy.EgressDefault) != 1 || got.Policy.EgressDefault[0] != "api.anthropic.com:443" {
		t.Errorf("egress default = %v", got.Policy.EgressDefault)
	}
	if len(got.Policy.EgressWidened) != 1 || got.Policy.EgressWidened[0] != "api.example.com:443" {
		t.Errorf("egress widened = %v", got.Policy.EgressWidened)
	}
	if got.Policy.SnapshotSHA256 == "" || got.Policy.SnapshotSHA256 != got.Policy.Fingerprint() {
		t.Errorf("policy fingerprint = %q, want self-consistent hash", got.Policy.SnapshotSHA256)
	}
	if got.Spend.USDBrokerMetered != 0.12 || got.Spend.DurationMs <= 0 {
		t.Errorf("spend = %+v", got.Spend)
	}
	if len(got.Diff.Flags) == 0 {
		t.Errorf("diff flags = %+v, want exec-bit flagged", got.Diff)
	}
	if got.Verification.Status != trustbrief.VerificationNotConfigured {
		t.Errorf("verification = %+v", got.Verification)
	}
	// fakeStage has no BaseCommit method — that absence must be surfaced,
	// not silently dropped.
	foundGap := false
	for _, m := range got.MissingEvidence {
		if m == "base_commit unavailable" {
			foundGap = true
		}
	}
	if !foundGap {
		t.Errorf("missing_evidence = %v, want base_commit gap recorded", got.MissingEvidence)
	}
}

// A soft budget (no per-request reservation) must be reported as such.
func TestWriteBrief_SoftBudgetReported(t *testing.T) {
	auditRoot := t.TempDir()
	b := &Broker{AuditRoot: auditRoot, TaskBudget: 2, Timeout: time.Minute}
	tr := &taskRun{
		b: b, id: "0123456789abcdef0123456789abcdef",
		repoRef: "https://github.com/o/r.git", agentName: "claude",
		grant: &fakeGrant{}, taskStart: time.Now(), st: &fakeStage{workDir: t.TempDir()},
	}
	b.writeBrief(tr, "diff --git a/x b/x\n+y\n")
	got, err := trustbrief.Read(auditRoot, tr.id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Policy.BudgetHard {
		t.Error("BudgetHard = true with MaxRequestCostUSD unset; the soft cap must be reported honestly")
	}
}

func TestHandleTask_WritesBriefAtGate(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{spent: 0.02}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	if len(events) == 0 {
		t.Fatal("no events")
	}
	id, _ := events[0]["task_id"].(string)
	if id == "" {
		t.Fatalf("no task_id in accepted event: %v", events[0])
	}
	got, err := trustbrief.Read(b.AuditRoot, id)
	if err != nil {
		t.Fatalf("no brief after auto-approved flow: %v", err)
	}
	if !got.Task.AutoApprove {
		t.Error("auto_approve not recorded in brief")
	}
	if filepath.Join(b.AuditRoot, id+trustbrief.Suffix) == "" {
		t.Fatal("unreachable")
	}
}
```

Run: `go test ./internal/broker/ -run 'Brief|WritesBrief'` — expected FAIL (`writeBrief`, `sensitive`, new fields undefined).

- [ ] **Step 4: Implement broker wiring**

In `internal/broker/broker.go`:

1. Add to imports: `"drydock/internal/trustbrief"`.
2. Add two fields to the `Broker` struct, after `TaskBudget` (`broker.go:89`):

```go
	// MaxRequestCostUSD and TaskMaxRequests mirror the gateway's admission
	// policy (config max_request_cost_usd / task_max_requests). The broker
	// itself does not enforce them — the gateway does — but the Brief must
	// report the effective policy, in particular whether the USD budget was
	// a hard ceiling (reservation on) or the default post-hoc soft cap.
	MaxRequestCostUSD float64
	TaskMaxRequests   int
```

3. Add a field to `taskRun` after `autoApprove bool`:

```go
	sensitive   bool
```

and set it where the `taskRun` literal is built in `HandleTask` (search for `autoApprove: t.AutoApprove` — add `sensitive: t.Sensitive` beside it).

4. Add the assembly method (place it directly above `pushAndOpenPR`):

```go
// writeBrief assembles and persists the broker-observed evidence report at
// the diff-approval gate, for every task that produced a diff — including
// auto-approved ones, where the Brief is the only pre-push record a human
// can later audit. Best-effort: the Brief is advisory evidence in v1, so a
// write failure warns and the gate proceeds rather than failing the task.
func (b *Broker) writeBrief(tr *taskRun, diff string) {
	policy := trustbrief.PolicyFacts{
		BudgetUSD:      b.TaskBudget,
		BudgetHard:     b.MaxRequestCostUSD > 0,
		MaxRequests:    b.TaskMaxRequests,
		TimeoutSeconds: int(b.Timeout.Seconds()),
		EgressDefault:  domainStrings(b.Cfg.Default.Domains),
		EgressWidened:  domainStrings(tr.egressExtra),
	}
	policy.SnapshotSHA256 = policy.Fingerprint()

	brief := trustbrief.Brief{
		SchemaVersion: 1,
		TaskID:        tr.id,
		GeneratedAt:   time.Now().UTC(),
		Task: trustbrief.TaskFacts{
			InstructionSHA256: trustbrief.HashInstruction(tr.instruction),
			RepoRef:           trustbrief.RedactRepoRef(tr.repoRef),
			Sensitive:         tr.sensitive,
			AutoApprove:       tr.autoApprove,
		},
		Runtime: trustbrief.RuntimeFacts{
			ImageRef: b.ImageRef, Agent: tr.agentName, Vendor: tr.taskVendor, Model: tr.model,
		},
		Policy: policy,
		Spend: trustbrief.SpendFacts{
			USDBrokerMetered: tr.grant.Spent(),
			DurationMs:       time.Since(tr.taskStart).Milliseconds(),
		},
		Diff:         trustbrief.Analyze(diff),
		Verification: trustbrief.Verification{Status: trustbrief.VerificationNotConfigured},
		MissingEvidence: []string{
			"verification not configured (no verifier stage in this version)",
			"agent summary not captured (broker records no agent claims in v1)",
		},
	}
	// Optional capability: the production stage knows its clone commit; test
	// fakes may not. Absence is recorded, never silently dropped.
	if bc, ok := tr.st.(interface{ BaseCommit() (string, error) }); ok {
		if c, err := bc.BaseCommit(); err == nil {
			brief.Task.BaseCommit = c
		}
	}
	if brief.Task.BaseCommit == "" {
		brief.MissingEvidence = append(brief.MissingEvidence, "base_commit unavailable")
	}
	if brief.Diff.Truncated {
		brief.MissingEvidence = append(brief.MissingEvidence,
			"review diff truncated at its cap; file counts cover the truncated portion only")
	}
	if err := trustbrief.Write(b.AuditRoot, tr.id, brief); err != nil {
		slog.Warn("could not persist trust brief", "task_id", tr.id, "err", err)
	}
}

// domainStrings renders egress domains as "host:p1,p2" strings for the Brief.
func domainStrings(ds []egress.Domain) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		ports := make([]string, 0, len(d.Ports))
		for _, p := range d.Ports {
			ports = append(ports, strconv.Itoa(p))
		}
		out = append(out, d.Host+":"+strings.Join(ports, ","))
	}
	return out
}
```

(Add `"strconv"` to imports.)

5. Call it at the top of `pushAndOpenPR` (`broker.go:640`), before `b.setStage(tr.id, StagePending)`:

```go
	b.writeBrief(tr, diff)
```

6. `realStage` (broker.go:153-164) already delegates to `*stage.Stage`; add:

```go
func (r realStage) BaseCommit() (string, error) { return r.s.BaseCommit() }
```

7. In `cmd/brokerd/main.go`, in the `&broker.Broker{` literal (line ~384), add:

```go
		MaxRequestCostUSD:    cfg.MaxRequestCostUSD,
		TaskMaxRequests:      cfg.TaskMaxRequests,
```

8. In `cmd/drydock/prune.go` line 25, change:

```go
var knownSuffixes = []string{".jsonl", ".diff", ".widen.json", ".brief.json"}
```

and update the usage text at line ~141 to read `(<id>.jsonl/.diff/.widen.json/.brief.json)`.

- [ ] **Step 5: Extend the prune test**

In `cmd/drydock/prune_test.go`, find the test that creates per-task artifact files (it writes `.jsonl`/`.diff` fixtures) and add a `.brief.json` fixture to the same task, asserting it is deleted with the rest. Follow the file's existing fixture helper exactly; the assertion is that after `deleteTasks`, `<id>.brief.json` no longer exists.

- [ ] **Step 6: Run tests**

Run: `go test -race ./internal/broker/ ./internal/stage/ ./cmd/brokerd/ ./cmd/drydock/`
Expected: PASS, including the three new brief tests and the extended prune test.

- [ ] **Step 7: Vet and commit**

Run: `go vet ./...`
```bash
git add internal/broker/ internal/stage/ cmd/brokerd/ cmd/drydock/prune.go cmd/drydock/prune_test.go
git commit -m "feat(broker): persist a broker-observed trust brief at the approval gate"
```

---

### Task 4: CLI rendering — `inspect`, review header, pending FLAGS column

**Files:**
- Create: `cmd/drydock/inspect.go`
- Modify: `cmd/drydock/main.go` (dispatch case, `subHelp` entry, `usage()` line)
- Modify: `cmd/drydock/review.go` (print brief above the pager)
- Modify: `cmd/drydock/client.go` (`listPending` FLAGS column)
- Modify: `cmd/drydock/main_test.go` (add `"inspect"` to `dispatchedCommands`)
- Test: `cmd/drydock/inspect_test.go` (create)

**Interfaces:**
- Consumes: `trustbrief.Read`, `trustbrief.Brief`, `trustbrief.Suffix`; `auditDir()` (`cmd/drydock/util.go:22`); `captureStdout` test helper (`cmd/drydock/init_test.go:138`).
- Produces: `drydock inspect <id> [--json]`; `printBrief(b trustbrief.Brief)`; `safeCell(s string) string`; `briefFlagKinds(dir, id string) string`.

- [ ] **Step 1: Write the failing test**

Create `cmd/drydock/inspect_test.go`:

```go
package main

import (
	"strings"
	"testing"
	"time"

	"drydock/internal/trustbrief"
)

func writeTestBrief(t *testing.T, dir, id string) trustbrief.Brief {
	t.Helper()
	b := trustbrief.Brief{
		SchemaVersion: 1, TaskID: id, GeneratedAt: time.Now().UTC(),
		Task: trustbrief.TaskFacts{
			InstructionSHA256: trustbrief.HashInstruction("x"),
			RepoRef:           "https://github.com/o/r.git",
			BaseCommit:        "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			Sensitive:         true,
		},
		Runtime: trustbrief.RuntimeFacts{ImageRef: "sandbox:test", Agent: "claude", Vendor: "anthropic"},
		Policy: trustbrief.PolicyFacts{
			BudgetUSD: 2, TimeoutSeconds: 1800,
			EgressDefault: []string{"api.anthropic.com:443"},
		},
		Spend: trustbrief.SpendFacts{USDBrokerMetered: 0.1234, DurationMs: 65000},
		Diff: trustbrief.Analyze("diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml\n" +
			"--- a/.github/workflows/ci.yml\n+++ b/.github/workflows/ci.yml\n@@ -1 +1 @@\n-a\n+b\n"),
		Verification:    trustbrief.Verification{Status: trustbrief.VerificationNotConfigured},
		MissingEvidence: []string{"verification not configured"},
	}
	b.Policy.SnapshotSHA256 = b.Policy.Fingerprint()
	if err := trustbrief.Write(dir, id, b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRunInspect_RendersBrokerObservedFacts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIT_ROOT", dir)
	id := "0123456789abcdef0123456789abcdef"
	writeTestBrief(t, dir, id)

	out := captureStdout(t, func() { runInspect([]string{id}) })
	for _, want := range []string{
		id,
		"github.com/o/r",
		"deadbeefdead", // truncated base commit prefix
		"claude",
		"$2.00 (soft)",
		"ci-workflow",
		".github/workflows/ci.yml",
		"$0.1234",
		"sensitive",
		"verification not configured",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "AGENT") {
		t.Errorf("v1 brief has no agent claims; output should not invent an agent section:\n%s", out)
	}
}

func TestRunInspect_JSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIT_ROOT", dir)
	id := "0123456789abcdef0123456789abcdef"
	writeTestBrief(t, dir, id)
	out := captureStdout(t, func() { runInspect([]string{"--json", id}) })
	if !strings.Contains(out, `"schema_version": 1`) || !strings.Contains(out, `"snapshot_sha256"`) {
		t.Errorf("--json output not raw brief:\n%s", out)
	}
}

func TestSafeCell_StripsControlAndCaps(t *testing.T) {
	in := "evil\x1b[31mred\x1b[0m\npath" + strings.Repeat("A", 500)
	got := safeCell(in)
	if strings.ContainsAny(got, "\x1b\n\r") {
		t.Errorf("control chars survived: %q", got)
	}
	if len(got) > 203 { // 200 runes + ellipsis
		t.Errorf("len = %d, want capped near 200", len(got))
	}
}

func TestBriefFlagKinds_ForPendingColumn(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	writeTestBrief(t, dir, id)
	if got := briefFlagKinds(dir, id); got != "ci-workflow" {
		t.Errorf("briefFlagKinds = %q, want ci-workflow", got)
	}
	if got := briefFlagKinds(dir, "ffffffffffffffffffffffffffffffff"); got != "" {
		t.Errorf("briefFlagKinds for missing brief = %q, want empty", got)
	}
}
```

Run: `go test ./cmd/drydock/ -run 'Inspect|SafeCell|BriefFlagKinds'` — expected FAIL.

- [ ] **Step 2: Implement `inspect.go`**

Create `cmd/drydock/inspect.go`:

```go
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"drydock/internal/trustbrief"
)

// runInspect renders a task's trust brief: the broker-observed evidence a
// reviewer triages before opening the diff. --json prints the raw artifact.
func runInspect(args []string) {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print the raw trust-brief JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: drydock inspect <id> [--json]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		die("usage: drydock inspect <id> [--json]")
	}
	id := fs.Arg(0)
	b, err := trustbrief.Read(auditDir(), id)
	if err != nil {
		die("no trust brief for task %s: %v", id, err)
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(b)
		return
	}
	printBrief(b)
}

// printBrief renders the human summary. Every value here is broker-observed;
// file paths originate from the (attacker-influenceable) work tree, so they
// pass through safeCell before reaching the terminal.
func printBrief(b trustbrief.Brief) {
	labels := ""
	if b.Task.Sensitive {
		labels += " · sensitive"
	}
	if b.Task.AutoApprove {
		labels += " · auto-approve"
	}
	fmt.Printf("task     %s%s\n", b.TaskID, labels)
	base := b.Task.BaseCommit
	if len(base) > 12 {
		base = base[:12]
	}
	repoLine := safeCell(b.Task.RepoRef)
	if base != "" {
		repoLine += " @ " + base
	}
	fmt.Printf("repo     %s\n", repoLine)
	fmt.Printf("runtime  agent=%s vendor=%s model=%s image=%s\n",
		safeCell(b.Runtime.Agent), safeCell(b.Runtime.Vendor),
		orDash(safeCell(b.Runtime.Model)), safeCell(b.Runtime.ImageRef))
	budgetKind := "soft"
	if b.Policy.BudgetHard {
		budgetKind = "hard"
	}
	fmt.Printf("policy   budget $%.2f (%s) · timeout %ds · policy sha %.12s\n",
		b.Policy.BudgetUSD, budgetKind, b.Policy.TimeoutSeconds, b.Policy.SnapshotSHA256)
	fmt.Printf("egress   default: %s · widened: %s\n",
		orDash(strings.Join(b.Policy.EgressDefault, " ")),
		orDash(strings.Join(b.Policy.EgressWidened, " ")))
	fmt.Printf("spend    $%.4f · %s\n", b.Spend.USDBrokerMetered, shortDur(b.Spend.DurationMs))
	adds, dels := 0, 0
	for _, f := range b.Diff.Files {
		adds += f.Adds
		dels += f.Dels
	}
	trunc := ""
	if b.Diff.Truncated {
		trunc = " · TRUNCATED"
	}
	fmt.Printf("diff     sha %.12s · %d bytes · %d files (+%d -%d)%s\n",
		b.Diff.SHA256, b.Diff.Bytes, len(b.Diff.Files)+b.Diff.FilesOmitted, adds, dels, trunc)
	for _, fl := range b.Diff.Flags {
		paths := make([]string, 0, len(fl.Paths))
		for _, p := range fl.Paths {
			paths = append(paths, safeCell(p))
		}
		fmt.Printf("FLAG     %s: %s\n", fl.Kind, strings.Join(paths, ", "))
	}
	fmt.Printf("verify   %s\n", b.Verification.Status)
	for _, m := range b.MissingEvidence {
		fmt.Printf("gap      %s\n", safeCell(m))
	}
}

func orDash(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// safeCell strips control characters (terminal-escape defense: paths come
// from the untrusted work tree) and caps length for column sanity.
func safeCell(s string) string {
	var sb strings.Builder
	n := 0
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		sb.WriteRune(r)
		n++
		if n >= 200 {
			sb.WriteString("…")
			break
		}
	}
	return sb.String()
}

// briefFlagKinds returns the comma-joined flag kinds from a task's brief,
// or "" when no brief exists (older task) or it doesn't parse. Used by the
// pending list's FLAGS column — advisory triage, so absence is silent.
func briefFlagKinds(dir, id string) string {
	b, err := trustbrief.Read(dir, id)
	if err != nil {
		return ""
	}
	kinds := make([]string, 0, len(b.Diff.Flags))
	for _, f := range b.Diff.Flags {
		kinds = append(kinds, f.Kind)
	}
	return strings.Join(kinds, ",")
}
```

- [ ] **Step 3: Wire the dispatcher**

In `cmd/drydock/main.go`:

1. Add to `subHelp` (after `"review"`):

```go
	"inspect": "<id> [--json] — show the task's trust brief: broker-observed diff facts, risk flags, policy, and spend.",
```

2. Add a dispatch case (after `case "review":`):

```go
	case "inspect":
		consumeHelpFlag(cmd, subArgs)
		runInspect(subArgs)
```

3. Add to the `usage()` Tasks section, after the `review` line:

```
  drydock inspect <id> [--json]  show the trust brief (diff facts, risk flags, policy, spend)
```

4. In `cmd/drydock/main_test.go`, add `"inspect"` to `dispatchedCommands` (keep the list order matching main's switch: after `"review"`).

- [ ] **Step 4: Review header and pending FLAGS column**

In `cmd/drydock/review.go`, at the top of `runReview` after the `os.Stat` check, add (with import `"drydock/internal/trustbrief"`):

```go
	// Evidence before content: show the broker-observed brief, then page the
	// diff. Older tasks (pre-brief) simply skip the header.
	if b, err := trustbrief.Read(auditDir(), id); err == nil {
		printBrief(b)
		fmt.Println()
	}
```

In `cmd/drydock/client.go` `listPending` (line ~116), widen the table with a FLAGS column:

```go
	fmt.Printf("%-14s  %5s  %-7s  %-24s  %-20s  %s\n", "ID", "AGE", "GATE", "REPO", "FLAGS", "DETAIL")
	dir := auditDir()
	for _, t := range pending {
		repo := shorten(t.Repo, 24)
		var gate, detail string
		switch t.Stage {
		case "awaiting_egress":
			gate = "egress"
			detail = formatExtras(t.EgressExtra)
		default: // awaiting_approval
			gate = "diff"
			detail = singleLine(t.Instruction)
		}
		flags := briefFlagKinds(dir, t.ID)
		if len(flags) > 20 {
			flags = flags[:19] + "…"
		}
		fmt.Printf("%-14s  %5s  %-7s  %-24s  %-20s  %s\n", t.ID, relAge(t.StartedAt), gate, repo, flags, detail)
	}
```

(The existing `TestListPendingRendersBothGateTypes` asserts substrings only and keeps passing; do not weaken it.)

- [ ] **Step 5: Run tests**

Run: `go test -race ./cmd/drydock/`
Expected: PASS — new inspect tests, `TestSubHelp_CoversEveryAdvertisedCommand` (now includes inspect), and all existing workflow tests.

- [ ] **Step 6: Vet, full suite, commit**

Run: `go vet ./... && go test -race ./...`
Expected: PASS across the repo.

```bash
git add cmd/drydock/
git commit -m "feat(cli): drydock inspect + trust-brief header in review and pending"
```

---

## Final verification (whole branch)

- `go vet ./...` and `go test -race -count=1 ./...` green.
- `go test -run=NONE -fuzz=FuzzAnalyze -fuzztime=60s ./internal/trustbrief/` — no crashers.
- Grep check: no new artifact writer without `O_NOFOLLOW`; no Brief field sourced from agent output.
