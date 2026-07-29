# Policy Explain Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** `drydock policy explain` shows every effective config value with the source that supplied it (default / config.yaml / env var), and flags when the running daemon's effective policy differs from the local one — the "declared vs effective policy" gap.

**Architecture:** A new `config.Explain` computes per-field provenance by re-deriving each field's source (re-running the same guards `applyEnvOverrides` uses, so a failed-guard env var is correctly attributed to yaml/default, not env). brokerd stashes its resolved `*config.Config` at boot and serves it read-only at `GET /admin/policy` with a canonical effective-policy hash. The CLI renders local provenance and, if the daemon is reachable, compares hashes and reports divergence. Shares `trustbrief.PolicyFacts.Fingerprint()` as the policy identity.

**Tech Stack:** Go stdlib.

## Decision record (locked)

- Provenance source is re-derived by re-running each field's guard, never by a bare `os.Getenv() != ""` (a non-parseable/out-of-range env var falls through in `applyEnvOverrides`, so its field's real source is yaml/default).
- `/admin/policy` is read-only, registered on the same mux as the other admin GETs (inherits unix-socket / TCP-loopback auth); brokerd stashes the resolved config at boot rather than reconstructing.
- Divergence is reported, never auto-reconciled. Daemon unreachable → local-only view with a loud "LIVE POLICY UNVERIFIED" banner (never silently imply the file is what's running).
- No secrets in output: API keys / OAuth tokens are never in `Config` (already true); `openai_compat.api_key_env` prints the VAR NAME, never its value. `policy explain` prints only config-derived values.
- Scope: `policy explain` + `/admin/policy` only. `doctor --repo` and repo-preflight are a separate later slice.

## Global Constraints

- Go stdlib only; `go vet ./...`, `go test -race -count=1 ./...`, `gofmt -l internal/ cmd/` silent, `staticcheck ./...` clean before each commit.
- The provenance field list must stay in lockstep with `applyEnvOverrides`: a test asserts every env var handled there is represented, so a future env knob can't ship without provenance.
- Any string printed to the terminal that could carry odd bytes (repo keys in `verify.repos`, `openai_compat` values) is control-char-safe; reuse `cmd/drydock`'s existing `safeCell` for cell rendering.
- Commit style `type(scope): summary`; trailer `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`; no PR footer.

---

### Task 1: `config.Explain` — per-field provenance

**Files:**
- Create: `internal/config/explain.go`
- Create: `internal/config/explain_test.go`

**Interfaces (consumed by Tasks 2–3):**
- `type Source string` with consts `SourceDefault = "default"`, `SourceYAML = "config.yaml"`, `SourceEnv = "env"`.
- `type Field struct { Name string; YAMLKey string; EnvVar string; Value string; Source Source }` (EnvVar empty for yaml-only fields).
- `func Explain(path string) ([]Field, *Config, error)` — loads defaults, loads the yaml at `path` (missing = defaults, not an error, matching `Load`), and for each field determines Source by comparing: does the guarded env override apply? → `SourceEnv`; else does the yaml file set it to a non-default? → `SourceYAML`; else `SourceDefault`. Returns the fields in a stable declaration order plus the fully-resolved `*Config` (via `Load`, so validation still runs).
- `func EffectiveHash(fields []Field) string` — sha256 over the canonical `Name=Value` lines (sorted by Name), hex. The divergence comparison unit.

**Design notes for the implementer (read `internal/config/config.go` first):**
- The env↔field↔default↔guard table is enumerated in `applyEnvOverrides` (config.go ~316-421) and `Defaults()` (~174-203). Build a parallel table of descriptors, one per field, each with: Name, YAMLKey, EnvVar (or ""), a `value(*Config) string` renderer, and — for env-overridable fields — a `guardedEnv() (string, bool)` that re-runs the EXACT guard (e.g. `DRYDOCK_TASK_BUDGET_USD` only counts if `ParseFloat` succeeds AND `>0`). For boolean `=="1"` env vars, the guard is `== "1"`.
- Source determination per field:
  1. If the field has an EnvVar and its `guardedEnv()` returns ok → `SourceEnv`.
  2. Else load the yaml alone (decode into a fresh `Defaults()` copy, no env) and compare the field to a pristine `Defaults()`; if it differs → `SourceYAML`.
  3. Else `SourceDefault`.
  This correctly handles the guard-fallthrough case (env set but invalid → yaml or default wins).
- Yaml-only fields (`task_timeout`, `approval_timeout`, `openai_compat.*`, `verify.repos`) have EnvVar "" and only steps 2–3 apply.
- Value rendering: durations via `.String()`, the egress default domains are NOT config fields (they live in egress.yaml) — out of scope, don't include them. For the `openai_compat` block render a compact summary (`base_url set / disabled`); for `verify.repos` render the repo count and keys. Keep every value a single line.

- [ ] **Step 1: Failing test** — `internal/config/explain_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fieldByName(fs []Field, name string) (Field, bool) {
	for _, f := range fs {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

func TestExplain_SourceAttribution(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A yaml that sets task_timeout (yaml-only) and network (also env-overridable).
	yaml := "network: from-yaml\ngateway_ip: 1.2.3.4\ntask_timeout: 45m\n"
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	// Env overrides gateway_ip with a valid value, and task_budget with an
	// INVALID value (<=0) that must fall through to the default.
	t.Setenv("DRYDOCK_GW_IP", "10.0.0.9")
	t.Setenv("DRYDOCK_TASK_BUDGET_USD", "-5") // fails the f>0 guard

	fields, cfg, err := Explain(path)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if cfg.GatewayIP != "10.0.0.9" {
		t.Errorf("resolved gateway = %q, want env value", cfg.GatewayIP)
	}
	check := func(name string, wantSrc Source, wantVal string) {
		f, ok := fieldByName(fields, name)
		if !ok {
			t.Fatalf("field %q missing from Explain", name)
		}
		if f.Source != wantSrc {
			t.Errorf("%s source = %q, want %q", name, f.Source, wantSrc)
		}
		if wantVal != "" && f.Value != wantVal {
			t.Errorf("%s value = %q, want %q", name, f.Value, wantVal)
		}
	}
	check("GatewayIP", SourceEnv, "10.0.0.9")
	check("Network", SourceYAML, "from-yaml")
	check("TaskTimeout", SourceYAML, "45m0s")
	check("TaskBudgetUSD", SourceDefault, "2") // env was invalid → default
	check("MaxConcurrent", SourceDefault, "")
}

// The provenance table must not silently drift from applyEnvOverrides: every
// env var handled there must be represented by exactly one Field.EnvVar.
func TestExplain_CoversEveryEnvOverride(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fields, _, err := Explain(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range fields {
		if f.EnvVar != "" {
			got[f.EnvVar] = true
		}
	}
	// Keep this list identical to the env vars applyEnvOverrides reads.
	want := []string{
		"DRYDOCK_NETWORK", "DRYDOCK_GW_IP", "SANDBOX_IMAGE", "DRYDOCK_ANCHOR_IMAGE",
		"DRYDOCK_TASK_BUDGET_USD", "DRYDOCK_MAX_CONCURRENT_TASKS", "DRYDOCK_DEFAULT_MODEL",
		"DRYDOCK_DEFAULT_AGENT", "DRYDOCK_ANTHROPIC_AUTH", "DRYDOCK_OPENAI_AUTH",
		"DRYDOCK_TASK_MAX_REQUESTS", "DRYDOCK_TASK_MAX_INFLIGHT", "DRYDOCK_STAGE_QUOTA_GB",
		"DRYDOCK_AGGREGATE_BUDGET_USD", "DRYDOCK_MAX_REQUEST_COST_USD", "DRYDOCK_AGGREGATE_WINDOW",
		"DRYDOCK_PUSH_MAX_RETRIES", "DRYDOCK_PUSH_RETRY_BACKOFF", "DRYDOCK_PUSH_FRESH_BRANCH_TRIES",
		"STAGE_ROOT", "AUDIT_ROOT", "SQUID_RUN_DIR", "BROKER_SOCKET", "BROKER_ADDR",
		"DRYDOCK_NO_NOTIFY", "DRYDOCK_LOG_JSON", "DRYDOCK_STRICT_CONTAINER_VERSION",
	}
	for _, ev := range want {
		if !got[ev] {
			t.Errorf("env var %q handled by applyEnvOverrides but not in Explain provenance", ev)
		}
	}
}

func TestEffectiveHash_StableAndSensitive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f1, _, _ := Explain(filepath.Join(t.TempDir(), "m.yaml"))
	f2, _, _ := Explain(filepath.Join(t.TempDir(), "m.yaml"))
	if EffectiveHash(f1) != EffectiveHash(f2) {
		t.Error("hash not stable across identical loads")
	}
	if len(EffectiveHash(f1)) != 64 {
		t.Error("hash not 64 hex chars")
	}
	t.Setenv("DRYDOCK_NETWORK", "different")
	f3, _, _ := Explain(filepath.Join(t.TempDir(), "m.yaml"))
	if EffectiveHash(f3) == EffectiveHash(f1) {
		t.Error("hash unchanged after a value changed")
	}
}
```

- [ ] **Step 2:** run to fail. **Step 3:** implement `explain.go` per the design notes — a `[]fieldDesc` table, `Explain` doing the 3-step source determination (with a yaml-only decode helper that decodes into `Defaults()` without env, reusing the same `KnownFields(true)` decoder as `Load` — factor a small `decodeYAMLOnly(path) (*Config, error)` if it keeps `Load` clean, else inline), and `EffectiveHash`. **Step 4:** `go test -race ./internal/config/`. **Step 5:** commit `feat(config): per-field policy provenance (Explain) and effective hash`.

---

### Task 2: `GET /admin/policy` endpoint

**Files:**
- Modify: `cmd/brokerd/main.go` (stash resolved config; register route)
- Modify: `internal/broker/broker.go` (a `ResolvedConfig *config.Config` field, or a `PolicyReport()` method) + `internal/broker/admin.go` (`HandlePolicy`)
- Test: `internal/broker/admin_test.go` (or the existing admin test file)

**Interfaces:**
- `Broker` gains an exported field carrying the resolved policy view. Simplest faithful option: `PolicyFields []config.Field` and `PolicyHash string`, set by brokerd at boot from `config.Explain`. (Avoids importing config broadly into broker beyond the Field type.)
- `GET /admin/policy` → `HandlePolicy` returns `{"fields":[{name,yaml_key,env_var,value,source}...],"hash":"<effectivehash>","policy_fingerprint":"<PolicyFacts.Fingerprint>"}` via `writeJSON`. Read-only.

- [ ] **Step 1: Failing test** — in the broker admin test file, add `TestHandlePolicy_ReturnsFieldsAndHash`: construct a `Broker{PolicyFields: []config.Field{{Name:"Network",Value:"x",Source:config.SourceYAML}}, PolicyHash:"abc"}`, serve `HandlePolicy` via httptest, assert JSON decodes with the field, hash, and content-type application/json. Mirror `TestHandleHealth_*` structure.
- [ ] **Step 2:** fail. **Step 3:** implement `HandlePolicy` in admin.go (pure marshal of the stashed fields+hash, no recomputation — the daemon reports what it loaded); in brokerd `main.go`, after `cfg` is resolved (~main.go:260) and before it goes out of scope, compute `fields, _, _ := config.Explain(config.DefaultPath())` (or thread the already-loaded cfg — but Explain re-attributes source, so call it once at boot), set `b.PolicyFields`/`b.PolicyHash`, and register `mux.HandleFunc("GET /admin/policy", b.HandlePolicy)` at main.go:534-542. Also set `b.PolicyFingerprint` from the same `PolicyFacts` the brief builds if cheap; else omit that field in v1 and note it.
- [ ] **Step 4:** `go test -race ./internal/broker/ ./cmd/brokerd/`. **Step 5:** commit `feat(brokerd): read-only GET /admin/policy reporting the daemon's effective policy`.

---

### Task 3: `drydock policy explain` CLI

**Files:**
- Create: `cmd/drydock/policy.go` + `cmd/drydock/policy_test.go`
- Modify: `cmd/drydock/main.go` (dispatch case, subHelp, usage)

**Interfaces / behavior:**
- `runPolicy(args []string)` handling subcommand `explain` (only one for now; unknown subcommand → usage/die). Flags: `--json`.
- Local view: `fields, _, err := config.Explain(config.DefaultPath())`; render an aligned table `SETTING  VALUE  SOURCE` using the existing `tty`/column style (reuse `safeCell` for value/key cells). Group or annotate env-sourced rows with the env var name (`env:DRYDOCK_GW_IP`).
- Live divergence: attempt `c, base := brokerClient(); c.Get(base+"/admin/policy")` with a short timeout. If reachable: decode, compare `EffectiveHash(local)` vs the daemon's `hash`. Equal → print `daemon: in sync`. Differ → print `daemon: DIVERGENT` and list the fields whose (value) differs between local and live. If unreachable (`brokerdDown`) → print the local table plus a loud `LIVE POLICY UNVERIFIED — brokerd not reachable; showing local config only` banner (do NOT imply the file is live).
- `--json`: emit `{"local":{fields,hash}, "live":{...}|null, "in_sync":bool|null}`.
- Never print secrets (there are none in Field values by construction; assert in a test that no value looks like a key).

- [ ] **Step 1: Failing tests** — `policy_test.go` using the `useBrokerServer`/`captureStdout` harness:
  - `TestPolicyExplain_LocalTableShowsSourcesAndValues` (set an env override + a temp config via HOME, assert the rendered table names the setting, value, and `env:`/`config.yaml`/`default`).
  - `TestPolicyExplain_InSyncWhenHashMatches` (fake `/admin/policy` returns the same hash `config.EffectiveHash` produces locally → output contains "in sync").
  - `TestPolicyExplain_DivergentWhenHashDiffers` (fake returns a different hash + a differing field → output contains "DIVERGENT" and names the field).
  - `TestPolicyExplain_UnreachableDaemonShowsBanner` (no BROKER_ADDR / unreachable → "LIVE POLICY UNVERIFIED", still prints local table, exit 0).
  - `TestPolicyExplain_JSON` (`--json` decodes with local/live/in_sync keys).
- [ ] **Step 2:** fail. **Step 3:** implement. Dispatch: `case "policy": consumeHelpFlag(cmd, subArgs); runPolicy(subArgs)`; `subHelp["policy"] = "explain — show the resolved effective policy and its per-value source; flags daemon divergence."`; usage line under "Other:". Add `"policy"` to `dispatchedCommands` in main_test.go.
- [ ] **Step 4:** `go test -race ./cmd/drydock/`. **Step 5:** commit `feat(cli): drydock policy explain — effective policy, provenance, daemon-divergence check`.

---

### Task 4: docs + claims + changelog

**Files:**
- Modify: `site/docs/configuration.md` (document `policy explain`, provenance, the divergence check), `site/docs/troubleshooting.md` (a "why is my config not taking effect?" pointer to `policy explain`), `cmd/drydock` usage already covered in Task 3.
- Modify: `CHANGELOG.md` (`## Unreleased`).
- Modify: `THREAT_MODEL.md` if warranted — a one-line note under the relevant residual that `policy explain` surfaces declared-vs-effective divergence (do NOT add an A-code; this is an operator-visibility aid, not a containment boundary). Only if it fits cleanly; otherwise skip.
- Check the docs-drift sentinel (`cmd/docs-build/claims_test.go` forbidden phrases) — read the list, ensure no new doc text trips it; regenerate `security-defaults.md` only if `cmd/docs-build` inputs changed (they don't here).

- [ ] **Step 1:** make the doc edits (accurate to what shipped — provenance sources, the LIVE POLICY UNVERIFIED behavior, `--json`). **Step 2:** `go test ./cmd/docs-build/` (sentinel + currency) and `go test -race -count=1 ./...` whole repo. **Step 3:** commit `docs: document drydock policy explain and the declared-vs-effective divergence check`.

---

## Final verification (whole branch)

- `go vet ./...`; `go test -race -count=1 ./...`; `gofmt -l internal/ cmd/` silent; `staticcheck ./...` clean.
- Manual: `drydock policy explain` against a running brokerd shows in-sync; with a diverging env in the shell, shows DIVERGENT naming the field; with brokerd down, shows the UNVERIFIED banner.
- Grep gate: no config value rendered without going through the provenance table; the CoversEveryEnvOverride test passes (no env knob without provenance).
