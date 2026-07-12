# drydock roadmap

**North star: security credibility**: make the security claims externally
*believable*, not just true in the code.

> A sandbox is only as trustworthy as the attacks it survives *in front of
> you*. Credibility = (1) every defensive claim is a runnable attack-that-fails,
> (2) the build is verifiable, (3) outsiders are invited to break it. A
> third-party audit is the capstone you *earn*, not the starting move.

This document covers the first two phases. Later phases (external scrutiny /
break-the-sandbox challenge, then a scoped third-party audit) are deferred
until Phases 1–2 make drydock self-evidently testable and reproducible.

Honesty constraint (unchanged): no overclaiming. Credibility comes from
*precise, checkable* claims plus loudly-stated limits: the `THREAT_MODEL.md`
A1–A7 / N1–N6 split and the SECURITY.md residuals are the model, and this
roadmap deepens them rather than papering over them.

**Scope (deliberate non-goal):** drydock is a containment runtime for **coding
agents on your repos**: clone → isolated run → a diff you approve → push.
Generalizing to non-coding agents (browser / computer-use, DevOps, arbitrary
tool execution) is a **post-1.0 non-goal**. The credential-gateway + egress +
per-task-VM substrate would carry over, but the task model (a repo in, a diff
out) is git-shaped by design, so "any agent" is an architectural lift, not a
copy change. Own the coding niche first.

---

## Phase 1: Provable containment

**Goal:** every threat-model claim is a runnable attack-that-fails; one command
runs them all; `THREAT_MODEL.md` links each claim to its enforcing test.

### 1.1 Red-team inventory & gap map

Each A-claim in `THREAT_MODEL.md` mapped to its enforcing test. "Where" is
whether the attack can run host-side (pure Go, CI-able) or needs the sandbox VM
(macOS / Apple-silicon, `integration` build tag).

| Claim | Attack | Status | Where | Test |
|---|---|---|---|---|
| **A1** agent reads the real vendor key | in-VM: inspect the env built from a real gateway grant; assert only `tok_`, never the sentinel `sk-ant-` key | **covered** | VM | `TestRedteam_A1_RealKeyNeverInVM` |
| **A2** agent reaches a hostile/unintended host | in-VM: `curl https://example.com` + raw DNS + direct-IP; assert all blocked | **covered** | VM | `TestRedteam_A2_EgressToHostileHostBlocked` |
| **A3** agent plants a `.git` hook → host RCE | host commits a work tree with a planted `pre-commit`; assert it never runs on the host | **covered** | host | `TestHostCommit_IgnoresPlantedHook` |
| **A4** agent leaks `.task/` (prompt/allowlist) into the PR | write inside `.task/`; assert it is excluded from the captured diff | **covered** | host | `TestCaptureDiff_ExcludesTaskDir` |
| **A5** agent crafts a malicious diff to bypass the gate | submit a backdoor diff without `auto_approve`; deny; assert nothing is pushed | **covered** | host | `TestRedteam_A5_GateBlocksUnapprovedPush` |
| **A6** agent widens its own egress | submit `egress_extra: evil:443`; deny; assert 403 and never reaches staging | **covered** | host | `TestRedteam_A6_EgressWidenDenied` |
| **A7** task state persists between tasks | run two tasks; assert task 2 cannot see task 1's VM/stage; `--rm` + `Cleanup` ran | **covered** | VM (+ host `TestCleanup_*`) | `TestRedteam_A7_NoStatePersistsBetweenTasks` |

**Phase 1 status:** all seven A-claims are now backed by a runnable
attack-that-fails. Host-side (A3–A6) run in CI via `make redteam`; VM-backed
(A1, A2, A7) run via `make redteam-vm` on macOS / Apple silicon. THREAT_MODEL
carries `Verified by:` links for each. Remaining polish: a nicer per-claim
green/red report wrapper (currently raw `go test` output).

### 1.2 Adversarial tests for the gaps: *landed*
The missing tests (A1, A2, A6, A7) are written, each named/labeled to its
claim; A3/A4/A5 were promoted into the same convention.

### 1.3 `make redteam`: *landed*
A target that runs the whole labeled suite. The host-side subset (A3–A6) runs
in CI; the VM subset (A1, A2, A7) runs via `make redteam-vm` on macOS /
Apple-silicon. This is the skeptic demo: *clone it, run it, watch every attack
fail.* Remaining polish: a per-claim green/red report wrapper (currently raw
`go test` output).

### 1.4 `THREAT_MODEL.md` "Verified by:" links: *landed*
Every A-claim cites its enforcing test.

**Done when:** `make redteam` is green on a capable host, CI runs the host-side
subset, and every A1–A7 cites a test.

---

## Phase 2: Verifiable supply chain

**Goal:** a third party can independently verify *what* they are running and
*where* it came from. Closes the SECURITY.md "No SBOM and no signed binaries
yet" residual.

**Status:** the `release` GitHub Actions workflow now produces, on every tag,
a CycloneDX SBOM (2.1), keyless cosign signatures, and SLSA build provenance
(2.3), attached to the release; consumer checks are in SECURITY.md "Verifying
a release". Reproducible builds (2.4) and the dependency-pin policy +
`govulncheck` CI gate (2.5) have since landed. Remaining: Apple notarization
(2.2, needs the paid cert).

### 2.3 Keyless signing + provenance: *landed*
`cosign sign-blob` the release tarball; SLSA build provenance via GitHub
Actions OIDC (`actions/attest-build-provenance`). No certificate cost
(Sigstore keyless). Document `cosign verify` + provenance checks for users.

### 2.1 SBOM per release: *landed*
Generate with `syft` over Go modules **and** the sandbox image's apt/npm
packages; attach SPDX/CycloneDX to each GitHub release.

### 2.4 Reproducible builds: *landed*
The release **binaries are byte-for-byte reproducible** (`-trimpath` + the
`go 1.26.5` toolchain on darwin/arm64). Each release publishes a per-binary
`*-bin.sha256`, and `make verify-build SUMS=…` rebuilds and checks against it;
see SECURITY.md "Verifying a release". The tarball itself is not byte-stable
(tar/gzip metadata); making the archive deterministic is a possible follow-up,
but the binaries inside it (what actually runs) are verifiable.

### 2.5 Dependency-pinning policy: *landed*
**Pin policy:** every external input is pinned and bumped deliberately:
- the sandbox base image `node:22-bookworm-slim` is pinned **by digest** in
  `image/Dockerfile` (re-pull + `container image inspect` to bump);
- the agent CLIs (`@anthropic-ai/claude-code`, `@openai/codex`) and the Go
  tarball are version-pinned via `ARG`s in the Dockerfile;
- the Go toolchain is pinned to `go 1.26.5` in `go.mod`;
- `go.sum` pins module checksums.

`govulncheck` runs in CI (`.github/workflows/test.yml`) and **fails the build on
a known vulnerability** in any dependency.

### 2.2 Signed + notarized macOS binaries: *event-driven*
`codesign` (Developer ID) + `notarytool` + staple so brew-installed binaries
clear Gatekeeper without the quarantine dance. **Prerequisite: Apple Developer
Program ($99/yr) + a Developer ID certificate.** Enrollment is planned; this
item holds no backlog slot and fires when the certificate is in hand.

**Done when:** each release ships an SBOM + cosign signature + SLSA provenance,
macOS binaries are notarized, the build is documented-reproducible, and the
SECURITY.md residual is updated.

---

## Phase 3: Provider expansion

**Goal:** add a native Gemini vendor (3B) as the first proof that a new provider
is a single registry row, not a five-file edit.

**Status:** 3A (provider registry refactor) and 3C (generic OpenAI-compatible
agent) are **landed**. The CLI, config validator, wizard, `drydock start`,
`drydock doctor`, and `drydock auth` all read from a single provider registry;
any OpenAI-compatible endpoint (Gemini via its OpenAI-compat API, OpenRouter,
local models) is already runnable via the `opencode` agent + `openai_compat:`
config block. **3B (native Gemini vendor) is built and merged but experimental**:
the code shipped (registry row, `GoogleVendor`, usage parser, pricing, image
install, entrypoint), and A1/A2 red-team + the `--approval-mode` fix are
verified (A1/A2 on real container hardware; the CLI-produces-edits-headless
property via a mocked upstream). It does **not** yet count as landed: the full
end-to-end run through the gateway against the real Gemini API is macOS-gated and
needs a real key. Until that passes, the lane is documented as experimental
and **parked**: no further work is planned until a key exists; the
openai-compat lane is the supported Gemini route meanwhile.

### 3A: Provider registry refactor, *landed*
Introduced one source of truth: a `Provider{Agent, Vendor, AuthModes, …}` table
that `agent.Vendor`, config validation, the wizard menu, `start`, `doctor`, and
`auth` all read from instead of hardcoding pairs. Pure refactor: `claude` and
`codex` still the only native entries, behavior byte-identical, existing tests
green. Adding a row is the *only* edit a new provider needs in this layer.

### 3B: Gemini (Google) *(first native vendor, proves the seam), built, experimental*
Shipped: the `gemini → google` row, `GoogleVendor()` (base URL + `x-goog-api-key`
inject), the `usageMetadata` parser, a pricing table, the sandbox-image CLI
install, the entrypoint case, and A1/A2 red-team coverage. As predicted, the
CLI/config layer was one registry row (buildBackends/brokerd unchanged). Verified
so far: A1 (real key never in VM) and A2 (deny-by-default egress) on real
container hardware, and the `--approval-mode yolo` fix (the CLI executes edit
tool calls headless) via a mocked Gemini endpoint. **Still open:** the full
end-to-end run against the real Gemini API (macOS + real key); until it passes,
the lane stays experimental, not landed. Native Gemini differs from the
openai-compat lane by using Google's own auth header/wire format.

### 3C: Generic OpenAI-compatible agent, *landed*
A single `openai-compat` provider whose base URL + key come from config covers a
long tail (local models, OpenRouter-style proxies, Gemini's OpenAI-compat
endpoint) without a bespoke vendor each. Shipped as the `opencode` agent +
`openai_compat:` config block; red-team A1 (key isolation) verified for the
lane.

### 3D: Config-declared providers *(stretch; only if demand is real)*
Let an operator declare a vendor entirely in config (name, base URL, auth
header template, price table) with no Go change. YAGNI until 3B shows the
registry's fields are the right ones; don't design the plugin format before
two real native providers have exercised the seam.

**Done when:** a native Gemini vendor runs end-to-end through the gateway with
its own A1/A2 red-team coverage, added via a single registry row.

---

## Phase 4: Reliability & hardening

**Goal:** close the gaps between "works on the happy path" and "survives the
operator's bad day": crashes, partial failures, and the slow drift of pinned
inputs. These are correctness/durability gaps, not new features; each is scoped
so it can ship independently.

- **4.1 Crash recovery.** *Landed.* A `brokerd` killed mid-task left host-side
  orphans the per-task cleanup defers never reaped. Boot reconciliation now:
  enforces single-instance via an `flock` (`~/.drydock/brokerd.lock`) so a
  second daemon can't clobber a live task; reaps orphan `task-*` VMs and squid
  (pre-existing); sweeps stale stage dirs; and resolves tasks a crash
  interrupted to a distinct `interrupted` outcome instead of a stuck
  `running?`. (The in-memory concurrency slot was never the gap; a restart
  rebuilds the semaphore at full capacity; the slot-pinning bug was the
  approval-gate timeout, fixed separately.)
- **4.2 Push partial-failure.** *Landed.* Push failures are classified into
  five reasons (`transient`, `auth`, `protected`, `non_fast_forward`, `unknown`);
  transient failures are retried with exponential backoff (`push_max_retries`,
  default `3`; base delay `push_retry_backoff`, default `1s`) and branch-name
  collisions are retried to fresh remote names (`push_fresh_branch_tries`,
  default `2`). Auth/protected/unknown stop immediately. Every outcome produces
  one clean terminal audit row: success reports the actual branch pushed;
  `push_failed` guarantees nothing landed on the remote (single-ref atomicity)
  and the diff is preserved. `drydock tasks` shows "push failed" for a failed
  push, and `drydock retry <id>` is safe (new id, no collision).
- **4.3 Aggregate budget cap.** *Landed.* `task_budget_usd` caps a single task;
  `aggregate_budget_usd` (env `DRYDOCK_AGGREGATE_BUDGET_USD`) now adds a
  cross-task USD ceiling per `api_key` provider over a configurable rolling
  window (`aggregate_window`, env `DRYDOCK_AGGREGATE_WINDOW`, default `24h`;
  `0` = total since brokerd boot). The gateway enforces the cap in `admit()`,
  rejecting requests with HTTP 402 once a vendor's windowed spend reaches the
  ceiling; the broker pre-checks at submit time so an over-cap task fails cleanly
  rather than starting doomed. In rolling mode, the ledger is seeded from audit
  files at boot so the cap survives a restart. Subscription mode is out of scope
  (bounded per-task by `task_max_requests`). With `aggregate_budget_usd` set
  (opt-in; default disabled), a runaway loop of cheap tasks can no longer drain
  an API key in aggregate; note enforcement is post-hoc unless
  `max_request_cost_usd` is also set, so concurrent in-flight requests can
  overshoot the ceiling by their aggregate cost before the next admission check.
- **4.4 `drydock retry`.** *Landed (v0.6.0).* Re-run a prior task from the
  invocation the broker now records in its trace (repo, prompt, agent, model,
  platform, egress), without reconstructing the `submit` by hand. Re-enters the
  approval gate (`auto_approve` is not carried over). `drydock cancel` shipped
  alongside as a `kill` alias.
- **4.5 Sandbox-image CVE scanning.** *Landed.* grype scans the built image
  daily in CI and on every image-touching PR; the gate fails on fixable
  High/Critical CVEs not covered by an active allowlist entry. Exceptions live
  in `image/cve-allowlist.yaml` with a mandatory reason and expiry date;
  expired entries fail CI again rather than rotting silently. Re-baselined to
  35 entries after the first CI run: 33 gosu stdlib advisories + 2 npm
  transitives. The 33-entry gosu cluster was retired in v0.6.0 when 4.12 removed
  gosu, leaving 2 npm transitives (the local baseline's toolchain-GOROOT and jq
  clusters were already dead in CI: image-mode scanning skips GOROOT src, and a
  fresh apt pulled the jq fix).
- **4.6 Agent-CLI bump automation.** *Landed.* A weekly scheduled workflow
  (`agent-cli-bump.yml`) checks the four pinned CLIs
  (`@anthropic-ai/claude-code`, `@openai/codex`, `opencode-ai`,
  `@google/gemini-cli`) against npm and proposes a bump PR when any version
  is strictly newer; the red-team suite (`make redteam` and
  `make redteam-vm`) is the gate before merging.
- **4.7 Observability.** Structured run metrics (durations, gate latencies,
  egress-widen frequency, budget burn) beyond the per-task JSONL, enough to
  answer "what is drydock doing across many runs" without grepping audit files.
- **4.8 Runtime abstraction.** The VM backend is Apple `container`-specific.
  Factor the container operations behind an interface so an alternative backend
  (e.g. Linux microVM) is a port, not a rewrite. *Stretch: only once a second
  backend is actually wanted; don't abstract for one implementation.*
- **4.9 Web UI surface.** *Shipped.* `drydock ui` serves a loopback SPA
  (board, diff review, submit, approve/deny/kill, history). It can drive the
  approval gate, so it is attack surface, and is treated as such: loopback-only
  bind, per-session bearer token (constant-time compare, URL-fragment
  transport), `Host`/`Origin` checks against DNS rebinding, symlink-rejecting
  audit reads, and UI submissions refuse `auto_approve`. Documented under
  THREAT_MODEL N6 with its enforcing tests. `--no-token` exists for trusted
  single-user machines and warns loudly.
- **4.10 Egress depth (IPv6 / plain-HTTP).** *Landed.* The firewall
  rewrite made the in-VM nft ruleset a single `inet` table with
  input/forward/output `policy drop`, so IPv6 egress is fail-closed (verified by
  the A2 red-team), and the code review added a squid SSRF guard that denies
  private/loopback/link-local/metadata destinations before the allowlist. The
  plain-HTTP vs HTTPS-CONNECT edge (CONNECT locked to port 443; plain HTTP
  denied by default; host-level tunnel trust; host+port granularity) is
  documented in the egress doc under "Plain HTTP vs HTTPS (the CONNECT edge)".
- **4.11 Unattended operation (launchd daemon).** *Landed.*
  `drydock daemon install|uninstall|status` manages a launchd LaunchAgent
  (RunAtLoad; KeepAlive restarts on crash and composes with 4.1's boot
  reconciliation; the 4.1 `flock` keeps a second brokerd safe). Install
  preflights credentials **as launchd sees them** (shell env is invisible;
  api-keys.env / OAuth files only) and refuses to claim success unless the
  launchd job itself is running; a foreground `drydock start` holding the
  lock is diagnosed, not masked. brokerd boot now ensures the `container`
  system service, so a reboot needs no manual step. Gates queue by default
  (`approval_timeout: 0s`); pickup stays manual (`drydock ui`). The daemon
  docs document the aggregate cap (4.3, now landed) and the subscription
  exclusion: set `aggregate_budget_usd` to bound cross-task spend per provider.
- **4.12 Agent capability clamp (was: gosu hardening).** *Landed (v0.6.0).*
  Reframed from CVE hygiene to containment during the code review: two
  independent reviewers found that the agent's inability to regain
  `CAP_NET_ADMIN` after the drop is the property the whole egress boundary rests
  on, and it was neither explicitly enforced nor tested (the A2 red-team even
  ran the probe as root+CAP_NET_ADMIN). The drop now uses util-linux `setpriv`
  with `--no-new-privs` and an emptied capability bounding/inheritable set, all
  SUID/SGID bits are stripped from the image, and the in-VM firewall is applied
  as one atomic `nft -f` with input/forward `policy drop`. A new red-team test
  runs *as the dropped agent* and asserts `nft flush` returns EPERM and egress
  stays blocked. `gosu` is removed, retiring its go1.19 CVE cluster.
- **4.13 Image package currency.** *Landed.* `apt-get upgrade -y` now runs in
  the Dockerfile before the package install block, so every rebuild (via
  `drydock setup` or the daily `image-scan` CI) picks up current Debian
  point-release security fixes without waiting for a base-digest bump. The
  base image remains digest-pinned (bumped deliberately per the Phase 2.5
  convention); bit-reproducibility of the package set is traded for security
  currency, appropriate for a locally-built sandbox runtime. The daily CVE
  scan (grype + `cmd/cve-gate`) gates each rebuilt image. The anchor image is
  unchanged (`FROM scratch`, single static binary, nothing to upgrade).
- **4.14 Resume awaiting-approval tasks across restart.** *Landed.* A durable
  gate marker (`<id>.gate.json`) is written when a task enters the push gate;
  the stage dir is preserved across a graceful shutdown (cleanup skipped, boot
  reap skips marked dirs). At boot, brokerd re-registers each marked task as
  pending and resumes it: `drydock approve <id>` after a restart pushes the
  surviving branch with no agent re-run and no re-spend. If the stage did not
  survive, an `interrupted` terminal line is written to the audit (never a false
  `ok`) and the `.diff` is preserved for `drydock retry <id>`. Idempotent across
  successive restarts.
- **4.15 Precise gateway metering.** *Landed.* Closes the concurrent-bypass
  hole from #139: N pipelined requests could all admit at spend=0 before any
  stream completed. A new `max_request_cost_usd` config field (env
  `DRYDOCK_MAX_REQUEST_COST_USD`, default `0`) reserves a worst-case USD amount
  against the lease budget while each request is in flight; the reservation is
  released and reconciled with actual metered cost at stream end. Default `0`
  keeps the existing post-hoc metering behavior (backward compatible).

**Done when:** a `brokerd` crash leaves no orphaned VM or wedged slot, spend is
bounded in aggregate (landed: per-task via `task_budget_usd`/`task_max_requests`
and cross-task via `aggregate_budget_usd`), brokerd runs unattended across
login/reboot, the sandbox image is CVE-scanned in CI, and each remaining edge
is either enforced or documented as a stated limit.

---

## Backlog (ordered)

Phases 1–2 are complete (2.2 excepted, below); 3A/3C landed, 3B parked. What
remains is one ranked list, updated as items land. The interleave is
deliberate: correctness and operator items alternate with credibility items;
Phases 1–2 bought a lot of external credibility while the operator side got
little, so the top of the list leans operator.

**v0.6.0 (2026-07-09) was a security-hardening release** from a full code +
product review. It landed the capability clamp (4.12), a first step on the
budget cap (4.3: a per-task request cap for uncapped lanes), IPv6 fail-close and
a squid SSRF guard (4.10), plus 4.4 (`retry`), alongside audit durability, a
loopback-only admin bind, and supply-chain nits. Since then: 4.14 (resume
awaiting-approval across restart), the aggregate budget cap (4.3, now fully
landed), 4.10 (plain-HTTP vs HTTPS-CONNECT edge documented in the egress doc),
and 4.15 (precise gateway metering: per-request in-flight reservation) have all
landed. See the Unreleased CHANGELOG entry for details.

1. **4.7 Observability**: wants real multi-run usage first, which unattended
   operation generates.

[#139]: https://github.com/sricola/drydock/issues/139

**Event-driven (no backlog slot):**
- **2.2 Notarization**: fires when the Apple Developer ID certificate is in
  hand.

**Parked:**
- **3B native Gemini**: experimental until one end-to-end run against the
  real Gemini API passes (macOS + a real `GEMINI_API_KEY`); its A1/A2
  red-team coverage is already in place. Details in the Phase 3 status.
- **3D config-declared providers, 4.8 runtime abstraction**: stretch, by
  this doc's own YAGNI rule.

**Standing note:** the A1/A2/A7 red-team tests need the VM (Apple `container`,
macOS 26 + Apple silicon), which hosted CI cannot provide, so CI runs only the
host-side subset (A3–A6). To keep a release from shipping without the isolation
tests behind its headline claims, `make release-preflight` (run automatically by
`make tag-release VERSION=vX.Y.Z`) is the enforced local gate: it rebuilds the
images and runs the unit suite, the host red-team (A3–A6), and the VM-backed
red-team (A1/A2/A7) before the tag is created.
