# First-run setup wizard — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a new user's first run simple: a host-side API-key store the broker reads, then `drydock setup` as a guided wizard (agent → auth → credential bootstrap → config), so a fresh install reaches a working config with no manual editing.

**Architecture:** Two stages. **Stage 1** (security-reviewable, ships first) adds `~/.drydock/api-keys.env` (0600) — a loader/writer in `internal/config`, brokerd precedence (file as defaults, non-empty env overrides), `drydock doctor` key-source reporting, and the docs reframe. **Stage 2** turns `drydock setup` into an interactive wizard that consumes Stage 1: pure prompt helpers, a config renderer, consented API-key persistence, and subscription bootstrap reusing the `auth claude|codex` cores. `drydock init` stays the non-interactive primitive.

**Tech Stack:** Go; existing `internal/config`, `cmd/brokerd`, `cmd/drydock`. No new dependency (no-echo via `stty`).

## Global Constraints

- **A1 invariant (load-bearing, unchanged):** the real credential never enters the VM. A file-sourced API key takes the exact same `gateway.StaticKey(key)` → minted-per-task-token path as an env key; nothing new crosses the `container run` boundary.
- **No new dependency.** Plain numbered stdin prompts; no-echo via `stty -echo`/`stty echo` (system tool, like the existing `security`/`container` shell-outs). No TUI framework, no `x/term`.
- **API keys: consented persistence, never silent.** The wizard prompts before writing a key to disk; env-only stays a first-class path. Storing is the recommended default.
- **Secrets out of `config.yaml`.** The API key goes in the dedicated `~/.drydock/api-keys.env` (0600); OAuth tokens keep their own json files. `config.yaml` holds no secrets.
- **Precedence:** a **non-empty** env var overrides the file; an env var set to `""` falls through to the file value; blank/`#` lines in the file are ignored.
- **Honest docs:** retire "API keys never go to disk"; reframe to "credentials stay host-side (env or `~/.drydock/` 0600) and never enter the VM." No overclaiming; on-disk exposure is **comparable** to the OAuth token (not "strictly less").
- **Mode/atomicity:** the store is `0600`, written temp+rename, in the already-`0700` `~/.drydock/` (mirror the OAuth json stores).
- **Non-fatal credentials:** a not-logged-in subscription or a skipped API key never aborts the wizard — it writes the config and prints the exact recovery command.
- **Non-interactive unchanged:** non-TTY `setup`, and `drydock init`, behave exactly as today.

## File Structure

| File | Responsibility |
|---|---|
| `internal/config/apikeys.go` (new) | `APIKeysPath()`, `LoadAPIKeys(path) map[string]string`, `WriteAPIKey(path, key, value) error` — the 0600 store loader/writer. |
| `cmd/brokerd/main.go` (modify) | Load the store; `resolveAPIKey(name, fileKeys)` precedence into `anthropicKey`/`openaiKey`. |
| `cmd/drydock/doctor.go` (modify) | Report the API-key source per vendor via `apiKeySource(envName, fileKeys)`. |
| `README.md`, `SECURITY.md`, `THREAT_MODEL.md` (modify) | Reframe the "never to disk" copy; document the store. |
| `cmd/drydock/wizard.go` (new) | `promptChoice`/`promptYesNo`/`promptSecret`; `renderConfig`; `runWizard`. |
| `cmd/drydock/auth.go` (modify) | Extract `bootstrapClaudeCred() error` / `bootstrapCodexCred() error` cores; `runAuthClaude`/`runAuthCodex` call them. |
| `cmd/drydock/setup.go` (modify) | Gate interactive wizard (TTY + first-run / `--reconfigure`); non-TTY → today's path. |

---

## STAGE 1 — API-key store + broker + doctor + docs

### Task 1: `api-keys.env` store (loader + writer)

**Files:**
- Create: `internal/config/apikeys.go`
- Test: `internal/config/apikeys_test.go`

**Interfaces:**
- Produces: `func APIKeysPath() string` (`<~/.drydock>/api-keys.env`, or `""` if home unresolvable); `func LoadAPIKeys(path string) map[string]string`; `func WriteAPIKey(path, key, value string) error`.

- [ ] **Step 1: Write the failing test** in `internal/config/apikeys_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAPIKeys_ParsesAndIgnoresBlanksAndComments(t *testing.T) {
	p := filepath.Join(t.TempDir(), "api-keys.env")
	body := "# comment\n\nANTHROPIC_API_KEY=sk-ant-x\n  OPENAI_API_KEY = sk-o  \n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := LoadAPIKeys(p)
	if got["ANTHROPIC_API_KEY"] != "sk-ant-x" || got["OPENAI_API_KEY"] != "sk-o" {
		t.Fatalf("LoadAPIKeys = %v", got)
	}
}

func TestLoadAPIKeys_MissingFileIsEmpty(t *testing.T) {
	if got := LoadAPIKeys(filepath.Join(t.TempDir(), "nope.env")); len(got) != 0 {
		t.Errorf("missing file should yield empty map, got %v", got)
	}
}

func TestWriteAPIKey_UpsertsPreservesOtherAnd0600(t *testing.T) {
	p := filepath.Join(t.TempDir(), "api-keys.env")
	if err := WriteAPIKey(p, "ANTHROPIC_API_KEY", "sk-ant-1"); err != nil {
		t.Fatal(err)
	}
	if err := WriteAPIKey(p, "OPENAI_API_KEY", "sk-o-1"); err != nil {
		t.Fatal(err)
	}
	// Re-upsert the first; the second must survive.
	if err := WriteAPIKey(p, "ANTHROPIC_API_KEY", "sk-ant-2"); err != nil {
		t.Fatal(err)
	}
	got := LoadAPIKeys(p)
	if got["ANTHROPIC_API_KEY"] != "sk-ant-2" || got["OPENAI_API_KEY"] != "sk-o-1" {
		t.Fatalf("upsert lost a key: %v", got)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}
```

- [ ] **Step 2: Run — fails to compile** (functions undefined). `go test ./internal/config/ -run 'APIKey'`.

- [ ] **Step 3: Implement** `internal/config/apikeys.go`:

```go
package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// knownAPIKeys are the only env names the store recognizes.
var knownAPIKeys = []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"}

// APIKeysPath is the host-only api-key store — the api_key-mode peer of the
// OAuth json files. Mode 0600; read host-side, never enters the VM. Returns ""
// when the home directory is unresolvable.
func APIKeysPath() string {
	d := Dir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, "api-keys.env")
}

// LoadAPIKeys reads KEY=value lines from path. Blank lines and # comments are
// ignored. A missing or unreadable file yields an empty map (the store is
// optional), not an error.
func LoadAPIKeys(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// WriteAPIKey upserts key=value in the store at path, preserving the other
// recognized key. 0600, atomic temp+rename; the parent dir is created 0700.
func WriteAPIKey(path, key, value string) error {
	keys := LoadAPIKeys(path)
	keys[key] = value
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# drydock API keys — host-only, 0600. Never enters the sandbox VM.\n")
	for _, k := range knownAPIKeys {
		if v := keys[k]; v != "" {
			b.WriteString(k + "=" + v + "\n")
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
```

- [ ] **Step 4: Run — passes.** `go test ./internal/config/ -run 'APIKey'`.

- [ ] **Step 5: Commit.** `git add internal/config/apikeys.go internal/config/apikeys_test.go && git commit -m "config: ~/.drydock/api-keys.env store (loader + 0600 writer)"`

---

### Task 2: brokerd loads the store (precedence)

**Files:**
- Modify: `cmd/brokerd/main.go` (the `anthropicKey`/`openaiKey` reads at ~line 99-100)
- Test: `cmd/brokerd/main_test.go` (add; create if absent)

**Interfaces:**
- Produces: `func resolveAPIKey(name string, fileKeys map[string]string) string`.
- Consumes: `config.LoadAPIKeys`, `config.APIKeysPath` (Task 1).

- [ ] **Step 1: Write the failing test** in `cmd/brokerd/main_test.go`:

```go
package main

import "testing"

func TestResolveAPIKey_Precedence(t *testing.T) {
	file := map[string]string{"ANTHROPIC_API_KEY": "from-file"}

	t.Run("non-empty env overrides file", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "from-env")
		if got := resolveAPIKey("ANTHROPIC_API_KEY", file); got != "from-env" {
			t.Errorf("got %q, want from-env", got)
		}
	})
	t.Run("empty env falls through to file", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		if got := resolveAPIKey("ANTHROPIC_API_KEY", file); got != "from-file" {
			t.Errorf("got %q, want from-file", got)
		}
	})
	t.Run("unset env + no file entry yields empty", func(t *testing.T) {
		if got := resolveAPIKey("OPENAI_API_KEY", file); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}
```

- [ ] **Step 2: Run — fails** (`resolveAPIKey` undefined). `go test ./cmd/brokerd/ -run TestResolveAPIKey`.

- [ ] **Step 3: Implement.** Add to `cmd/brokerd/main.go`:

```go
// resolveAPIKey returns the effective key for env-var name. A non-empty
// exported value wins (so CI `export …` is unchanged); otherwise the value from
// the host-side api-keys.env store; else "". An env var set to "" deliberately
// falls through to the file rather than blanking a good stored key.
func resolveAPIKey(name string, fileKeys map[string]string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fileKeys[name]
}
```

Then change the key reads (currently `anthropicKey := os.Getenv("ANTHROPIC_API_KEY")` / `openaiKey := os.Getenv("OPENAI_API_KEY")`):

```go
fileKeys := config.LoadAPIKeys(config.APIKeysPath())
anthropicKey := resolveAPIKey("ANTHROPIC_API_KEY", fileKeys)
openaiKey := resolveAPIKey("OPENAI_API_KEY", fileKeys)
```

(`config` is already imported in main.go. The rest of the backend wiring — `gateway.StaticKey(anthropicKey)` etc. — is unchanged, so the A1 path is identical.)

- [ ] **Step 4: Run — passes.** `go test ./cmd/brokerd/`. `go build ./...`.

- [ ] **Step 5: Commit.** `git add cmd/brokerd && git commit -m "brokerd: load ~/.drydock/api-keys.env (non-empty env overrides)"`

---

### Task 3: `doctor` reports the API-key source

**Files:**
- Modify: `cmd/drydock/doctor.go`
- Test: `cmd/drydock/doctor_test.go` (add)

**Interfaces:**
- Produces: `func apiKeySource(envName string, fileKeys map[string]string) string` → `"env"` / `"~/.drydock/api-keys.env"` / `"none"`.
- Consumes: `config.LoadAPIKeys`, `config.APIKeysPath` (Task 1).

- [ ] **Step 1: Write the failing test** in `cmd/drydock/doctor_test.go`:

```go
package main

import "testing"

func TestAPIKeySource(t *testing.T) {
	file := map[string]string{"OPENAI_API_KEY": "sk-o"}

	t.Run("env", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "sk-ant")
		if got := apiKeySource("ANTHROPIC_API_KEY", file); got != "env" {
			t.Errorf("got %q, want env", got)
		}
	})
	t.Run("file", func(t *testing.T) {
		t.Setenv("OPENAI_API_KEY", "")
		if got := apiKeySource("OPENAI_API_KEY", file); got != "~/.drydock/api-keys.env" {
			t.Errorf("got %q, want file path", got)
		}
	})
	t.Run("none", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		if got := apiKeySource("ANTHROPIC_API_KEY", file); got != "none" {
			t.Errorf("got %q, want none", got)
		}
	})
}
```

- [ ] **Step 2: Run — fails** (`apiKeySource` undefined). `go test ./cmd/drydock/ -run TestAPIKeySource`.

- [ ] **Step 3: Implement.** Add to `cmd/drydock/doctor.go`:

```go
// apiKeySource names where an api_key for envName would come from, so the
// operator can see whether a stored file or the shell env is in effect.
func apiKeySource(envName string, fileKeys map[string]string) string {
	if os.Getenv(envName) != "" {
		return "env"
	}
	if fileKeys[envName] != "" {
		return "~/.drydock/api-keys.env"
	}
	return "none"
}
```

Then in `runDoctor`, after the existing subscription checks, add a key-source step for each vendor whose auth is `api_key` (use the loaded config `cfg`):

```go
fileKeys := config.LoadAPIKeys(config.APIKeysPath())
if cfg.AnthropicAuth != "subscription" {
	step("anthropic api key", true, "source: "+apiKeySource("ANTHROPIC_API_KEY", fileKeys))
}
if cfg.OpenAIAuth != "subscription" {
	step("openai api key", true, "source: "+apiKeySource("OPENAI_API_KEY", fileKeys))
}
```

(`config` and `os` are already imported in doctor.go. `step(label, ok, detail)` is the existing helper; pass `true` — this is informational, never a hard fail.)

- [ ] **Step 4: Run — passes.** `go test ./cmd/drydock/`. `go build ./...`.

- [ ] **Step 5: Commit.** `git add cmd/drydock && git commit -m "doctor: report api-key source (env / api-keys.env / none)"`

---

### Task 4: Docs reframe ("never to disk" → "never in the VM")

**Files:**
- Modify: `README.md`, `SECURITY.md`, `THREAT_MODEL.md`

No tests (docs). Honesty rule binds: no overclaim; on-disk exposure is **comparable** to the OAuth token, not "strictly less."

- [ ] **Step 1: Find every "never to disk" claim.** `grep -nE 'never go to disk|never goes to disk|stay in your shell env|never touch disk' README.md SECURITY.md THREAT_MODEL.md`.

- [ ] **Step 2: README.** Replace each "the vendor keys … never go to disk" assertion with the reframed claim, e.g.:

> Credentials stay **host-side** and **never enter the sandbox VM** — the agent only ever sees a short-lived, budget-capped token. An API key can live in your shell env, or (the wizard offers to store it) at `~/.drydock/api-keys.env` (mode `0600`); subscription OAuth tokens live there too. None of them is ever passed into the VM.

- [ ] **Step 3: SECURITY.md.** Add `~/.drydock/api-keys.env` to the host-side credential inventory next to the OAuth json files: a 0600 file holding the API key(s), read by the broker, never in the VM. State plainly: a stored API key is a long-lived host-side secret of **comparable** exposure to the OAuth token already stored there (it is not per-task revocable; revoke it in the vendor console). The wizard only writes it with explicit consent; env-only remains supported.

- [ ] **Step 4: THREAT_MODEL.md.** Where the budget/credential section describes where the real key lives, note both sources (env or `~/.drydock/api-keys.env` 0600) and that neither enters the VM — the A1 control is unchanged.

- [ ] **Step 5: Verify no stale claim remains.** `grep -nE 'never go to disk|never goes to disk' README.md SECURITY.md THREAT_MODEL.md` → no results. `go build ./...` (sanity; docs only).

- [ ] **Step 6: Commit.** `git add README.md SECURITY.md THREAT_MODEL.md && git commit -m "docs: reframe key handling — host-side (env or api-keys.env 0600), never in the VM"`

---

**End of Stage 1.** Ship as PR 1 (the security-reviewable core).

---

## STAGE 2 — the wizard

### Task 5: pure prompt helpers

**Files:**
- Create: `cmd/drydock/wizard.go`
- Test: `cmd/drydock/wizard_test.go`

**Interfaces:**
- Produces: `func promptChoice(in io.Reader, out io.Writer, q string, opts []string, dflt int) int` (1-based; empty input → dflt; invalid re-prompts); `func promptYesNo(in io.Reader, out io.Writer, q string, dflt bool) bool`; `func promptSecret(prompt string) (string, error)` (reads one line from os.Stdin with terminal echo disabled via `stty`).

- [ ] **Step 1: Write the failing test** in `cmd/drydock/wizard_test.go`:

```go
package main

import (
	"strings"
	"testing"
)

func TestPromptChoice(t *testing.T) {
	opts := []string{"Claude Code", "OpenAI Codex", "both"}
	cases := []struct {
		in   string
		want int
	}{
		{"2\n", 2},      // explicit
		{"\n", 1},       // empty → default
		{"x\n5\n3\n", 3}, // invalid, out-of-range, then valid
	}
	for _, c := range cases {
		var out strings.Builder
		if got := promptChoice(strings.NewReader(c.in), &out, "Which agent?", opts, 1); got != c.want {
			t.Errorf("in=%q → %d, want %d", c.in, got, c.want)
		}
	}
}

func TestPromptYesNo(t *testing.T) {
	cases := []struct {
		in   string
		dflt bool
		want bool
	}{
		{"y\n", false, true},
		{"n\n", true, false},
		{"\n", true, true}, // empty → default
		{"\n", false, false},
	}
	for _, c := range cases {
		var out strings.Builder
		if got := promptYesNo(strings.NewReader(c.in), &out, "ok?", c.dflt); got != c.want {
			t.Errorf("in=%q dflt=%v → %v, want %v", c.in, c.dflt, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run — fails** (undefined). `go test ./cmd/drydock/ -run 'PromptChoice|PromptYesNo'`.

- [ ] **Step 3: Implement** `cmd/drydock/wizard.go`:

```go
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// promptChoice prints a numbered menu and returns the chosen 1-based index.
// Empty input returns dflt; invalid input re-prompts.
func promptChoice(in io.Reader, out io.Writer, q string, opts []string, dflt int) int {
	r := bufio.NewReader(in)
	for {
		fmt.Fprintln(out, q)
		for i, o := range opts {
			tag := ""
			if i+1 == dflt {
				tag = "  (default)"
			}
			fmt.Fprintf(out, "  [%d] %s%s\n", i+1, o, tag)
		}
		fmt.Fprint(out, "> ")
		line, _ := r.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return dflt
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(opts) {
			return n
		}
		fmt.Fprintf(out, "  please enter 1–%d\n", len(opts))
	}
}

// promptYesNo returns the y/n answer; empty input returns dflt.
func promptYesNo(in io.Reader, out io.Writer, q string, dflt bool) bool {
	suffix := " [y/N] "
	if dflt {
		suffix = " [Y/n] "
	}
	r := bufio.NewReader(in)
	for {
		fmt.Fprint(out, q+suffix)
		line, _ := r.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return dflt
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
	}
}

// promptSecret reads one line from stdin with terminal echo disabled, so a
// pasted API key doesn't render on screen. Uses the system `stty` (no new
// dependency); echo is restored even on error.
func promptSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stdout, prompt)
	_ = exec.Command("stty", "-echo").Run()
	defer func() { _ = exec.Command("stty", "echo").Run(); fmt.Fprintln(os.Stdout) }()
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
```

(Note: `stty` reads/writes the controlling terminal via fd 0/1, so it only takes effect on a real TTY — which is the only place `promptSecret` is called, behind the wizard's TTY gate.)

- [ ] **Step 4: Run — passes.** `go test ./cmd/drydock/ -run 'PromptChoice|PromptYesNo'`. `go build ./...`.

- [ ] **Step 5: Commit.** `git add cmd/drydock/wizard.go cmd/drydock/wizard_test.go && git commit -m "wizard: pure prompt helpers (choice/yesno/secret)"`

---

### Task 6: `renderConfig` — config.yaml from choices

**Files:**
- Modify: `cmd/drydock/wizard.go`
- Test: `cmd/drydock/wizard_test.go`

**Interfaces:**
- Produces: `type wizardChoices struct { DefaultAgent string; AnthropicAuth string; OpenAIAuth string }`; `func renderConfig(c wizardChoices) string` — returns a complete `config.yaml` body. Empty auth fields fall back to `api_key`.
- Consumes: `config.SeedTemplate` (the existing comment-rich template).

- [ ] **Step 1: Write the failing test:**

```go
import (
	"os"
	"path/filepath"

	"drydock/internal/config"
)

func TestRenderConfig_SetsChosenKeys(t *testing.T) {
	cases := []wizardChoices{
		{DefaultAgent: "claude", AnthropicAuth: "subscription", OpenAIAuth: "api_key"},
		{DefaultAgent: "codex", AnthropicAuth: "api_key", OpenAIAuth: "subscription"},
	}
	for _, c := range cases {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(renderConfig(c)), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(p)
		if err != nil {
			t.Fatalf("rendered config does not parse: %v", err)
		}
		if cfg.DefaultAgent != c.DefaultAgent || cfg.AnthropicAuth != c.AnthropicAuth || cfg.OpenAIAuth != c.OpenAIAuth {
			t.Errorf("choices=%+v → cfg default=%q anthropic=%q openai=%q", c, cfg.DefaultAgent, cfg.AnthropicAuth, cfg.OpenAIAuth)
		}
	}
}
```

- [ ] **Step 2: Run — fails** (`renderConfig`/`wizardChoices` undefined).

- [ ] **Step 3: Implement** in `wizard.go`. Start from `config.SeedTemplate` and replace the three keys' values with a regexp so the comment-rich template (and all other defaults) are preserved:

```go
import "regexp" // add to the import block

type wizardChoices struct {
	DefaultAgent  string // "claude" | "codex"
	AnthropicAuth string // "api_key" | "subscription"
	OpenAIAuth    string // "api_key" | "subscription"
}

// renderConfig returns a complete config.yaml body: the seeded template with
// default_agent / anthropic_auth / openai_auth set to the wizard's choices;
// every other key keeps its template default.
func renderConfig(c wizardChoices) string {
	if c.DefaultAgent == "" {
		c.DefaultAgent = "claude"
	}
	if c.AnthropicAuth == "" {
		c.AnthropicAuth = "api_key"
	}
	if c.OpenAIAuth == "" {
		c.OpenAIAuth = "api_key"
	}
	body := config.SeedTemplate
	body = setYAMLKey(body, "default_agent", c.DefaultAgent)
	body = setYAMLKey(body, "anthropic_auth", c.AnthropicAuth)
	body = setYAMLKey(body, "openai_auth", c.OpenAIAuth)
	return body
}

// setYAMLKey rewrites the value of a top-level `key:` line, preserving the rest
// of the line's trailing comment alignment as written in the template.
func setYAMLKey(body, key, value string) string {
	re := regexp.MustCompile(`(?m)^(` + regexp.QuoteMeta(key) + `:\s*)\S+`)
	return re.ReplaceAllString(body, "${1}"+value)
}
```

(Confirm `config.SeedTemplate` already contains `default_agent:`, `anthropic_auth:`, `openai_auth:` lines — it does, per `internal/config/config.go`. The regexp matches the `key:` plus whitespace, then replaces the first non-space token, leaving the inline `# comment` intact.)

- [ ] **Step 4: Run — passes.** `go test ./cmd/drydock/ -run TestRenderConfig`.

- [ ] **Step 5: Commit.** `git add cmd/drydock/wizard.go cmd/drydock/wizard_test.go && git commit -m "wizard: renderConfig writes chosen agent/auth into the template"`

---

### Task 7: extract the auth bootstrap cores

**Files:**
- Modify: `cmd/drydock/auth.go`
- Test: `cmd/drydock/auth_test.go` (existing tests must still pass; add a thin call-through check)

**Interfaces:**
- Produces: `func bootstrapClaudeCred() error` (reads the macOS Keychain, writes `~/.drydock/claude-oauth.json`); `func bootstrapCodexCred() error` (reads `~/.codex/auth.json`, writes `~/.drydock/codex-oauth.json`). Return a descriptive error (CLI not logged in, etc.) instead of calling `os.Exit`.
- Consumes: existing `parseClaudeCreds`, `parseCodexCreds`, `gateway.FileCredStore`, `gateway.NewCodexStore`.

- [ ] **Step 1: Write the failing test** (the cores exist and are callable; a not-logged-in environment returns an error, not an exit):

```go
func TestBootstrapCores_Exist(t *testing.T) {
	// These call the real Keychain / ~/.codex; in CI they error (not logged in),
	// which is the contract — they must return an error, never os.Exit.
	_ = bootstrapClaudeCred
	_ = bootstrapCodexCred
}
```

- [ ] **Step 2: Run — fails** (undefined). `go test ./cmd/drydock/ -run TestBootstrapCores`.

- [ ] **Step 3: Refactor.** Move the credential-fetch+store body out of `runAuthClaude`/`runAuthCodex` into cores that return errors:

```go
// bootstrapClaudeCred copies the Claude subscription credential from the macOS
// Keychain into drydock's store. Returns an error (never exits) so callers —
// the auth subcommand and the setup wizard — can react.
func bootstrapClaudeCred() error {
	out, err := exec.Command("security", "find-generic-password", "-s", keychainService, "-w").Output()
	if err != nil {
		return fmt.Errorf("could not read Claude credentials from Keychain — run `claude login` first")
	}
	snap, err := parseClaudeCreds(out)
	if err != nil {
		return err
	}
	return gateway.FileCredStore(filepath.Join(config.Dir(), "claude-oauth.json")).Save(snap)
}

// bootstrapCodexCred copies the ChatGPT/Codex credential from ~/.codex/auth.json
// into drydock's store. Returns an error (never exits).
func bootstrapCodexCred() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("could not determine home directory: %w", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		return fmt.Errorf("could not read ~/.codex/auth.json — run `codex login` first")
	}
	snap, account, err := parseCodexCreds(raw)
	if err != nil {
		return err
	}
	store := gateway.NewCodexStore(filepath.Join(config.Dir(), "codex-oauth.json"))
	return store.Put(snap, account)
}
```

Rewrite `runAuthClaude` / `runAuthCodex` so the non-`--status` path calls the core and prints/exits on its error:

```go
// inside runAuthClaude, replacing the inline Keychain+Save block:
if err := bootstrapClaudeCred(); err != nil {
	fmt.Fprintln(os.Stderr, "auth:", err)
	os.Exit(1)
}
snap, _ := gateway.FileCredStore(filepath.Join(config.Dir(), "claude-oauth.json")).Load()
printValidity(snap)
```

(Apply the analogous change to `runAuthCodex`, using `gateway.NewCodexStore(...).Load()` + `printCodexValidity`. Keep the `--status` branches unchanged.)

- [ ] **Step 4: Run — passes.** `go test ./cmd/drydock/` — the existing `auth` parser tests stay green; `go build ./...`.

- [ ] **Step 5: Commit.** `git add cmd/drydock/auth.go cmd/drydock/auth_test.go && git commit -m "auth: extract bootstrapClaudeCred/bootstrapCodexCred cores (return errors)"`

---

### Task 8: `runWizard` orchestration + the API-key step

**Files:**
- Modify: `cmd/drydock/wizard.go`
- Test: `cmd/drydock/wizard_test.go`

**Interfaces:**
- Produces: `func runWizard(w *wizardDeps) wizardChoices` — drives agent → per-agent auth → per-agent credential bootstrap → writes `~/.drydock/config.yaml`, returns the chosen config. `wizardDeps` injects the I/O streams and credential/store hooks for testing.
- Consumes: `promptChoice`/`promptYesNo`/`promptSecret` (T5), `renderConfig` (T6), `bootstrapClaudeCred`/`bootstrapCodexCred` (T7), `config.WriteAPIKey`/`config.APIKeysPath` (T1).

- [ ] **Step 1: Write the failing test** (scripted stdin, stubbed credential hooks, capture the rendered config):

```go
func TestRunWizard_ClaudeSubscription(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir) // APIKeysPath/config.Dir resolve under here
	var bootstrapped bool
	deps := &wizardDeps{
		in:  strings.NewReader("1\n1\n"), // agent: Claude, auth: subscription
		out: &strings.Builder{},
		bootstrapClaude: func() error { bootstrapped = true; return nil },
		bootstrapCodex:  func() error { return nil },
		configPath:      filepath.Join(dir, ".drydock", "config.yaml"),
	}
	got := runWizard(deps)
	if got.DefaultAgent != "claude" || got.AnthropicAuth != "subscription" {
		t.Fatalf("choices = %+v", got)
	}
	if !bootstrapped {
		t.Error("subscription path should call the claude bootstrap core")
	}
	if _, err := config.Load(deps.configPath); err != nil {
		t.Errorf("config not written/parseable: %v", err)
	}
}

func TestRunWizard_CodexApiKeyConsentStores(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("OPENAI_API_KEY", "sk-from-env")
	deps := &wizardDeps{
		in:  strings.NewReader("2\n2\ny\n"), // agent: Codex, auth: API key, persist? yes
		out: &strings.Builder{},
		bootstrapClaude: func() error { return nil },
		bootstrapCodex:  func() error { return nil },
		configPath:      filepath.Join(dir, ".drydock", "config.yaml"),
	}
	got := runWizard(deps)
	if got.DefaultAgent != "codex" || got.OpenAIAuth != "api_key" {
		t.Fatalf("choices = %+v", got)
	}
	// Consented persist of an already-exported env key → stored, no re-prompt.
	if config.LoadAPIKeys(config.APIKeysPath())["OPENAI_API_KEY"] != "sk-from-env" {
		t.Error("consented API key not persisted to api-keys.env")
	}
}
```

- [ ] **Step 2: Run — fails** (`runWizard`/`wizardDeps` undefined).

- [ ] **Step 3: Implement** in `wizard.go`. The struct injects every side-effecting seam; the env-key persist path avoids re-entry:

```go
type wizardDeps struct {
	in              io.Reader
	out             io.Writer
	bootstrapClaude func() error
	bootstrapCodex  func() error
	configPath      string
}

// runWizard drives the interactive config flow and writes config.yaml.
func runWizard(d *wizardDeps) wizardChoices {
	var c wizardChoices

	agent := promptChoice(d.in, d.out, "Which coding agent?",
		[]string{"Claude Code (Anthropic)", "OpenAI Codex", "both"}, 1)
	wantClaude := agent == 1 || agent == 3
	wantCodex := agent == 2 || agent == 3
	if agent == 2 {
		c.DefaultAgent = "codex"
	} else {
		c.DefaultAgent = "claude" // 1 or 3 ("both" defaults to claude)
	}

	if wantClaude {
		c.AnthropicAuth = authStep(d, "Claude Code", "claude login", "ANTHROPIC_API_KEY", d.bootstrapClaude)
	}
	if wantCodex {
		c.OpenAIAuth = authStep(d, "OpenAI Codex", "codex login", "OPENAI_API_KEY", d.bootstrapCodex)
	}

	if err := os.MkdirAll(filepath.Dir(d.configPath), 0o700); err == nil {
		_ = os.WriteFile(d.configPath, []byte(renderConfig(c)), 0o644)
	}
	fmt.Fprintf(d.out, "\nwrote %s · default_agent: %s\n", d.configPath, c.DefaultAgent)
	fmt.Fprintln(d.out, "start:  drydock start      first task:  drydock submit --repo <url> --instruction \"…\"")
	return c
}

// authStep asks the auth mode for one agent and bootstraps the credential.
// Returns "subscription" or "api_key". All credential failures are non-fatal.
func authStep(d *wizardDeps, label, loginCmd, envName string, bootstrap func() error) string {
	mode := promptChoice(d.in, d.out, "How will "+label+" authenticate?",
		[]string{"subscription — no API key", "API key (" + envName + ")"}, 1)
	if mode == 1 {
		if err := bootstrap(); err != nil {
			fmt.Fprintf(d.out, "  ! %v — run `%s`, then re-run `drydock setup`\n", err, loginCmd)
		} else {
			fmt.Fprintf(d.out, "  ✓ %s credential stored\n", label)
		}
		return "subscription"
	}
	// API key: consented persistence; env-only preserved.
	if promptYesNo(d.in, d.out, "Store the API key at ~/.drydock/api-keys.env (0600) so the broker finds it across shells?", true) {
		val := os.Getenv(envName)
		if val == "" {
			v, err := promptSecret("  paste " + envName + ": ")
			if err == nil {
				val = v
			}
		}
		if val != "" {
			if err := config.WriteAPIKey(config.APIKeysPath(), envName, val); err != nil {
				fmt.Fprintf(d.out, "  ! could not store key: %v\n", err)
			} else {
				fmt.Fprintf(d.out, "  ✓ stored %s\n", envName)
			}
		}
	} else if os.Getenv(envName) == "" {
		fmt.Fprintf(d.out, "  ! before `drydock start`: export %s=…\n", envName)
	}
	return "api_key"
}
```

(`config` must be imported in `wizard.go`.)

- [ ] **Step 4: Run — passes.** `go test ./cmd/drydock/ -run TestRunWizard`. `go build ./...`.

- [ ] **Step 5: Commit.** `git add cmd/drydock/wizard.go cmd/drydock/wizard_test.go && git commit -m "wizard: runWizard orchestration + consented api-key step"`

---

### Task 9: wire the wizard into `drydock setup`

**Files:**
- Modify: `cmd/drydock/setup.go`
- Test: covered by `runWizard` tests + a manual first-run; `go build`/suite green.

**Interfaces:**
- Consumes: `runWizard`/`wizardDeps` (T8), `bootstrapClaudeCred`/`bootstrapCodexCred` (T7), `tty` (init.go), `config.DefaultPath`/`config.Dir` (existing).

- [ ] **Step 1: Add the `--reconfigure` flag + gate** in `runSetup`. After the existing prereqs + `runInit()` call, add:

```go
// First-run / explicit reconfigure → interactive wizard. Non-TTY or an
// existing config without --reconfigure keeps init's static seed + "next:".
reconfigure := false
for _, a := range args {
	if a == "--reconfigure" {
		reconfigure = true
	}
}
cfgPath := config.DefaultPath()
_, statErr := os.Stat(cfgPath)
firstRun := os.IsNotExist(statErr)
if tty && (firstRun || reconfigure) {
	fmt.Println()
	fmt.Println("── configure ───────────────────────────────")
	runWizard(&wizardDeps{
		in:              os.Stdin,
		out:             os.Stdout,
		bootstrapClaude: bootstrapClaudeCred,
		bootstrapCodex:  bootstrapCodexCred,
		configPath:      cfgPath,
	})
}
```

(Note: `runInit()` seeds a static `config.yaml` when none exists. So when the wizard runs on first-run, it OVERWRITES that static seed with the choice-driven config — the wizard write is last, which is correct. `config` and `os` are already imported in setup.go.)

- [ ] **Step 2: Document `--reconfigure`.** Update `subHelp["setup"]` in `cmd/drydock/main.go` to mention it: `"… first run: install prerequisites, then the setup wizard. --reconfigure re-runs the wizard; --yes to skip install prompts."`

- [ ] **Step 3: Build + non-TTY sanity.** `go build ./...`. Run `drydock setup </dev/null` style is covered by the gate; confirm a non-TTY invocation does NOT enter the wizard (the `tty` var is false when stdout isn't a char device).

- [ ] **Step 4: Run the suite.** `go test ./...` green.

- [ ] **Step 5: Manual first-run** (on a real terminal, before merge): fresh `~/.drydock` → `drydock setup` → choose Claude + subscription (with `claude login` done) → confirm `config.yaml` written with `default_agent: claude` / `anthropic_auth: subscription`, the credential stored, and `drydock doctor` shows it. Then API-key path: choose Codex + API key, consent to store, confirm `~/.drydock/api-keys.env` (0600) holds the key and `drydock doctor` reports `source: ~/.drydock/api-keys.env`.

- [ ] **Step 6: Commit.** `git add cmd/drydock/setup.go cmd/drydock/main.go && git commit -m "setup: interactive wizard on first run (TTY); --reconfigure; non-TTY unchanged"`

---

**End of Stage 2.** Ship as PR 2.

---

## Self-Review

**Spec coverage:** api-keys.env store loader+writer (T1) ✓; brokerd precedence incl. empty-env-falls-through (T2) ✓; doctor key-source reporting (T3) ✓; docs reframe (T4) ✓; pure prompt helpers incl. no-echo secret (T5) ✓; renderConfig per agent×auth (T6) ✓; reuse of auth cores via extraction (T7) ✓; runWizard flow incl. consented api-key persistence + non-fatal not-logged-in (T8) ✓; setup TTY gate + --reconfigure + non-TTY fallback (T9) ✓; A1 invariant preserved (T2 keeps the `StaticKey` path) ✓; two-stage sequencing (Stage 1 = T1-T4, Stage 2 = T5-T9) ✓. The "both agents" path is covered (T8 `authStep` runs for each). No spec requirement is unaddressed.

**Placeholder scan:** No TBD/TODO. The manual first-run (T9 Step 5) is an explicit live verification, not a deferred task. `promptSecret`'s no-echo path is exercised manually (it reads the real TTY); its callers and the consent logic are unit-tested.

**Type consistency:** `config.LoadAPIKeys`/`WriteAPIKey`/`APIKeysPath` defined T1, used in T2/T3/T8; `resolveAPIKey(name, fileKeys)` T2; `apiKeySource(envName, fileKeys)` T3; `promptChoice/promptYesNo/promptSecret` signatures T5 match their callers in T8; `wizardChoices{DefaultAgent,AnthropicAuth,OpenAIAuth}` + `renderConfig` T6 consumed by T8; `bootstrapClaudeCred()/bootstrapCodexCred() error` T7 injected into `wizardDeps` (T8) and passed in T9; `wizardDeps{in,out,bootstrapClaude,bootstrapCodex,configPath}` consistent T8↔T9. Consistent.
