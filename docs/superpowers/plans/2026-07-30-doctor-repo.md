# `drydock doctor --repo` Preflight Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** `drydock doctor --repo <path>` diagnoses — **before spending any API budget** — whether a repo will work in drydock: size vs the stage caps, toolchains vs what the sandbox image ships, registries the repo needs vs the egress allowlist (suggesting `--egress-extra`), and a preview of which files `diff_policy` would flag/block. Read-only, container-free, never executes repo code, byte-bounded.

**Architecture:** A pure `diagnoseRepo(dir, cfg, egCfg) []repoCheck` function (filesystem-only, testable over `t.TempDir()`, no container). `doctor` gains a `--repo` flag that runs *only* the repo diagnosis (not the container smoke checks) and renders results in the existing `step`/`stepWarn` style. Reuses `globmatch.Match`, `egress.Load`, `broker.DefaultMaxStageBytes/Files`, `cfg.StageQuotaGB`, `cfg.DiffPolicy`, and a newly-exported `trustbrief.ClassifyPath` (so the classifier's filename lists stay single-source).

**Tech Stack:** Go stdlib.

## Decision record (locked)

- `doctor --repo` is a SEPARATE mode: with `--repo`, doctor runs the repo diagnosis and exits (no container/VM/API). Without it, doctor is unchanged (the existing smoke test).
- Never execute repo code. Read file *names* always; read a small allowlist of *config* files (`.npmrc`, `.yarnrc`, `.yarnrc.yml`, `pip.conf`/`.pip/pip.conf`, `Cargo.toml` for registry hints) only to extract registry hostnames, each capped at a small byte limit.
- Exit code: `--repo` diagnosis returns non-zero only on a hard blocker (repo exceeds the stage caps, or a `diff_policy.blocked_paths` match that would `policy_blocked` every task). Toolchain/egress/second-look findings are warnings (exit 0) — they inform, they don't fail.
- Toolchain-vs-image uses a small hardcoded language table (Node/Python/Go ship; Rust/Ruby/Java/…, do not), guarded by a Dockerfile-drift test so the table can't silently rot.

## Global Constraints

- Read-only: no writes, no exec of repo content, no container, no network, no API spend. The walk skips `.git`, uses `filepath.SkipAll`/a ceiling so a pathological tree can't hang it, and caps how many bytes it reads from any config file.
- Reuse, don't duplicate: `globmatch.Match` for `diff_policy` path preview; `egress.Load(config.EgressPath())` for the allowlist; `broker.DefaultMaxStageBytes`/`DefaultMaxStageFiles` + `cfg.StageQuotaGB` for size limits; `trustbrief.ClassifyPath` (exported in Task 1) for dependency/lockfile/CI classification — do NOT re-list the classifier filenames.
- The image-language table has a test asserting it matches the Dockerfile (drift guard), mirroring the repo's version-currency guards.
- No secrets in output. A registry host from `.npmrc` is a hostname, print it; never print tokens (`.npmrc` can contain `_authToken` — extract only the `registry=` host, never echo the file).
- Go gate before each commit: `go vet ./...`, `go test -race ./...` (scope per task), `gofmt -l internal/ cmd/` silent, `staticcheck ./...` clean.
- Commit `type(scope): summary`; trailer `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`; no PR footer.

---

### Task 1: export `trustbrief.ClassifyPath` + the repo diagnosis engine

**Files:**
- Modify: `internal/trustbrief/difffacts.go` (export a `ClassifyPath` wrapper) + `difffacts_test.go`
- Create: `cmd/drydock/repodoctor.go` (the pure diagnosis) + `cmd/drydock/repodoctor_test.go`
- Create: `cmd/drydock/imagelangs.go` (the language table) + a drift test in `repodoctor_test.go` or `imagelangs_test.go`

**Interfaces:**
- `trustbrief.ClassifyPath(p string) []string` — returns the flag kinds a path classifies as (dependency-manifest / lockfile / ci-workflow / git-metadata), reusing the existing unexported `classifyPath` logic (wrap it: call `classifyPath(p, func(kind,_ string){ kinds=append... })`, dedupe, sort). Empty for an ordinary source file.
- `repoCheck struct { Label string; OK bool; Warn bool; Detail string }` — OK=true & Warn=false → pass; Warn=true → warning; OK=false → hard blocker (exit non-zero).
- `diagnoseRepo(dir string, cfg *config.Config, eg egress.Config) []repoCheck` — the pure engine. Produces checks:
  1. **size** — walk `dir` (skip `.git`, skip symlinked dirs, early-exit past a ceiling), accumulate total bytes + regular-file count. Compare bytes vs `min(cfg.StageQuotaGB<<30 [when >0], broker.DefaultMaxStageBytes)` and count vs `broker.DefaultMaxStageFiles`. Over either → **blocker** ("repo is N GiB / M files, exceeds the K GiB / L file stage cap — the task will be killed"). Under → pass with the numbers.
  2. **toolchain** — detect languages present (by manifest filename via `trustbrief.ClassifyPath`-adjacent detection OR a small filename check: package.json→node, go.mod→go, requirements.txt/pyproject.toml→python, Cargo.toml→rust, Gemfile→ruby, pom.xml/build.gradle→java). For each detected language, check the image table: shipped → pass note; not shipped → **warn** ("repo uses Rust (Cargo.toml); the sandbox image ships Node/Python/Go only — the agent can't build it without a custom image").
  3. **egress** — for detected package ecosystems, the default registry host each needs (npm→registry.npmjs.org, python→pypi.org+files.pythonhosted.org, go→proxy.golang.org+sum.golang.org, rust→static.crates.io+crates.io, ruby→rubygems.org). If the host is NOT in `eg.Default.Domains` → **warn** ("repo needs crates.io for Cargo; not in the egress allowlist — submit with --egress-extra crates.io:443"). Also read `.npmrc`/`.yarnrc`/`Cargo.toml [source]`/`pip.conf` (byte-capped) for a custom `registry=`/`index-url=` host; if present and not allowlisted → warn naming the host. Never echo the file.
  4. **diff_policy preview** (only when `cfg.DiffPolicy` is non-empty) — walk repo paths, count how many match `BlockedPaths` (→ **blocker** if any: "N files match blocked_paths; every task touching them fails policy_blocked") and how many match `SecondLookPaths` (→ warn: "N files are second-look; approving a diff touching them needs acknowledgment"). Also `MaxFilesChanged` sanity: if set and the repo already has far more than the cap files, note it's fine (caps are about the DIFF not the repo) — informational, not a warn, or omit.
- `imageLanguages() map[string]string` (or a `[]imageLang` slice) — the languages the sandbox image ships: `node` (22), `python` (3.11), `go` (1.26.5). A `dockerfileLanguages(dockerfilePath)` helper for the drift test that greps the Dockerfile for the FROM/ARG lines.

- [ ] **Step 1: Failing tests.**
  - `difffacts_test.go`: `TestClassifyPath_Exported` — `ClassifyPath("go.mod")==["dependency-manifest"]`, `ClassifyPath(".github/workflows/ci.yml")` contains `"ci-workflow"`, `ClassifyPath("src/main.go")` empty.
  - `repodoctor_test.go` (build a `t.TempDir()` repo per case):
    - `TestDiagnoseRepo_SizeUnderCapPasses` — a few small files → size check OK with the byte/file numbers in Detail.
    - `TestDiagnoseRepo_SizeOverFileCapBlocks` — synthesize a check by setting a tiny cap via a test seam OR create a config with a low quota; assert a blocker check (OK=false). (Prefer: diagnoseRepo takes the limits from cfg/consts; to test the over-cap path without creating 200k files, allow a low `cfg.StageQuotaGB`… but StageQuotaGB is GiB. Cleaner: extract the numeric limits into diagnoseRepo params or a small `stageLimits struct` the test can pass. Design diagnoseRepo to accept a `limits` struct so tests can set a 10-byte cap.)
    - `TestDiagnoseRepo_RustToolchainWarns` — a `Cargo.toml` present → toolchain warn (image ships no Rust) AND egress warn (crates.io not allowlisted).
    - `TestDiagnoseRepo_NodeRepoUnderDefaultsPasses` — `package.json` + default egress (npmjs allowed) → toolchain pass, egress pass.
    - `TestDiagnoseRepo_CustomNpmRegistryWarns` — `.npmrc` with `registry=https://nexus.corp/npm/` → warn naming `nexus.corp`, and the `_authToken` line in the same file is NOT echoed.
    - `TestDiagnoseRepo_BlockedPathPreviewBlocks` — cfg.DiffPolicy.BlockedPaths `[".github/workflows/**"]` + repo has `.github/workflows/ci.yml` → blocker check counting 1.
    - `TestDiagnoseRepo_SecondLookPreviewWarns` — SecondLookPaths `["**/Dockerfile"]` + a Dockerfile → warn.
    - `TestDiagnoseRepo_SkipsGitDir` — a big file under `.git/` is not counted.
    - `TestImageLanguages_MatchesDockerfile` — `dockerfileLanguages("../../image/Dockerfile")` (adjust path) ⊇ the hardcoded table's languages (drift guard).
- [ ] **Step 2:** fail. **Step 3:** implement `ClassifyPath` (trustbrief), `imagelangs.go`, `repodoctor.go`. Walk with `filepath.WalkDir`, skip `.git`, `d.Type()&fs.ModeSymlink` dirs, ignore transient errors, ceiling early-exit. Config-file reads via `os.OpenFile`+`io.LimitReader` (cap e.g. 64 KiB). Registry extraction: simple line scan for `registry=`/`index-url=`/`[source.crates-io]` etc., extract host via `url.Parse`.
- [ ] **Step 4:** `go test -race ./internal/trustbrief/ ./cmd/drydock/`. **Step 5:** commit `feat(doctor): repo preflight diagnosis engine (size/toolchain/egress/diff-policy)`.

---

### Task 2: wire `doctor --repo`, render, docs

**Files:** Modify `cmd/drydock/doctor.go` (runDoctor accepts args / --repo), `cmd/drydock/main.go` (dispatch, subHelp, usage); `site/docs/troubleshooting.md` + `submitting-tasks.md`; `CHANGELOG.md`; tests in `doctor_test.go`.

**Behavior:**
- Change dispatch: `case "doctor": runDoctor(subArgs)` (submit-style — parse a FlagSet inside, so `--repo` and `-h` both work). `runDoctor(args []string)`: `fs := flag.NewFlagSet("doctor", flag.ExitOnError); repo := fs.String("repo", "", "diagnose a repo path for drydock readiness (no API spend, no container)"); fs.Parse(args)`. If `*repo != ""` → `runRepoDoctor(*repo)` and return (do NOT run the container smoke checks). Else → the existing smoke-test body.
- `runRepoDoctor(path string)`: resolve/validate the path (must be a dir; clean); load `cfg` (tolerate missing config → use `config.Defaults()`), load egress (`egress.Load(config.EgressPath())`, tolerate missing → the embedded default via `defaults.EgressYAML` parsed, or `egress.Config{}`); call `diagnoseRepo`; render each `repoCheck` via `step`/`stepWarn` (OK→step true, Warn→stepWarn, blocker→step false); print a one-line summary; `os.Exit(1)` if any blocker.
- Update `main.go:21` usage line to mention `[--repo <path>]` and `subHelp["doctor"]` to note the preflight mode.
- Docs: `troubleshooting.md` — "check a repo before submitting" pointing at `doctor --repo`; `submitting-tasks.md` — a line under prerequisites. CHANGELOG `## Unreleased`.

- [ ] **Step 1: Failing tests** — `doctor_test.go`: `TestRunRepoDoctor_RendersChecks` (a `t.TempDir()` repo with a Cargo.toml → captureStdout shows the toolchain + egress warnings and exits 0) and `TestRunRepoDoctor_BlockerExitsNonZero` is hard to assert with os.Exit — instead test `diagnoseRepo`'s blocker classification (Task 1) and have `runRepoDoctor` structured so the render is testable via captureStdout without the exit (factor the exit decision so a helper returns the blocker bool; test that helper). Add `"doctor"` handling to any dispatch coverage test if present (main_test dispatchedCommands already has doctor).
- [ ] **Step 2:** fail. **Step 3:** implement. **Step 4:** FULL gate: `go vet ./...`; `go test -race -count=1 ./...`; `gofmt -l internal/ cmd/` silent; `staticcheck ./...` clean; `go test ./cmd/docs-build/` (docs-drift sentinel). **Step 5:** commit `feat(cli,docs): drydock doctor --repo preflight`.

---

## Final verification (whole branch)

- Full gate green. `doctor --repo` never shells a container / spends API (grep: no `runCmd`/`container`/gateway in the `--repo` path).
- Manual: `drydock doctor --repo .` on this repo (a Go repo) → toolchain pass (Go ships), egress pass (go proxies allowed), size pass; on a temp Rust repo → toolchain + egress warnings, exit 0; with a `diff_policy.blocked_paths` matching a repo file → blocker, exit 1.
- No secrets: a repo `.npmrc` with a token is never echoed (only the registry host).
