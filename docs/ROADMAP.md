# drydock roadmap

**North star: security credibility** — make the security claims externally
*believable*, not just true in the code.

> A sandbox is only as trustworthy as the attacks it survives *in front of
> you*. Credibility = (1) every defensive claim is a runnable attack-that-fails,
> (2) the build is verifiable, (3) outsiders are invited to break it. A
> third-party audit is the capstone you *earn*, not the starting move.

This document covers the first two phases. Later phases (external scrutiny /
break-the-sandbox challenge, then a scoped third-party audit) are deferred
until Phases 1–2 make drydock self-evidently testable and reproducible.

Honesty constraint (unchanged): no overclaiming. Credibility comes from
*precise, checkable* claims plus loudly-stated limits — the `THREAT_MODEL.md`
A1–A7 / N1–N6 split and the SECURITY.md residuals are the model, and this
roadmap deepens them rather than papering over them.

**Scope (deliberate non-goal):** drydock is a containment runtime for **coding
agents on your repos** — clone → isolated run → a diff you approve → push.
Generalizing to non-coding agents (browser / computer-use, DevOps, arbitrary
tool execution) is a **post-1.0 non-goal**. The credential-gateway + egress +
per-task-VM substrate would carry over, but the task model (a repo in, a diff
out) is git-shaped by design, so "any agent" is an architectural lift, not a
copy change. Own the coding niche first.

---

## Phase 1 — Provable containment

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

### 1.2 Adversarial tests for the gaps
Write the missing tests (A1, A2, A6, A7), each named/labeled to its claim so
the harness can collect them. Promote A3/A4/A5 into the same convention.

### 1.3 `make redteam`
A target that runs the whole labeled suite and prints a per-claim green/red
**containment report**. The host-side subset (A3–A6) runs in CI; the VM subset
(A1, A2, A7) runs on macOS / Apple-silicon. This is the skeptic demo: *clone
it, run it, watch every attack fail.*

### 1.4 `THREAT_MODEL.md` "Verified by:" links
Each A-claim cites its enforcing test (A3/A4 already do) and the doc gains a
"Reproduce: `make redteam`" header.

**Done when:** `make redteam` is green on a capable host, CI runs the host-side
subset, and every A1–A7 cites a test.

---

## Phase 2 — Verifiable supply chain

**Goal:** a third party can independently verify *what* they are running and
*where* it came from. Closes the SECURITY.md "No SBOM and no signed binaries
yet" residual.

**Status:** the `release` GitHub Actions workflow now produces, on every tag,
a CycloneDX SBOM (2.1), keyless cosign signatures, and SLSA build provenance
(2.3) — attached to the release; consumer checks are in SECURITY.md "Verifying
a release". Remaining: reproducible-build docs + `govulncheck` (2.4), a
dependency-pin policy (2.5), and Apple notarization (2.2, needs the paid cert).

### 2.3 Keyless signing + provenance — *do first (free, high signal)*
`cosign sign-blob` the release tarball; SLSA build provenance via GitHub
Actions OIDC (`actions/attest-build-provenance`). No certificate cost
(Sigstore keyless). Document `cosign verify` + provenance checks for users.

### 2.1 SBOM per release
Generate with `syft` over Go modules **and** the sandbox image's apt/npm
packages; attach SPDX/CycloneDX to each GitHub release.

### 2.4 Reproducible builds — *landed*
The release **binaries are byte-for-byte reproducible** (`-trimpath` + the
`go 1.26.4` toolchain on darwin/arm64). Each release publishes a per-binary
`*-bin.sha256`, and `make verify-build SUMS=…` rebuilds and checks against it —
see SECURITY.md "Verifying a release". The tarball itself is not byte-stable
(tar/gzip metadata); making the archive deterministic is a possible follow-up,
but the binaries inside it — what actually runs — are verifiable.

### 2.5 Dependency-pinning policy — *landed*
**Pin policy:** every external input is pinned and bumped deliberately —
- the sandbox base image `node:22-bookworm-slim` is pinned **by digest** in
  `image/Dockerfile` (re-pull + `container image inspect` to bump);
- the agent CLIs (`@anthropic-ai/claude-code`, `@openai/codex`) and the Go
  tarball are version-pinned via `ARG`s in the Dockerfile;
- the Go toolchain is pinned to `go 1.26.4` in `go.mod`;
- `go.sum` pins module checksums.

`govulncheck` runs in CI (`.github/workflows/test.yml`) and **fails the build on
a known vulnerability** in any dependency.

### 2.2 Signed + notarized macOS binaries — *needs a paid prereq*
`codesign` (Developer ID) + `notarytool` + staple so brew-installed binaries
clear Gatekeeper without the quarantine dance. **Prerequisite: Apple Developer
Program ($99/yr) + a Developer ID certificate.**

**Done when:** each release ships an SBOM + cosign signature + SLSA provenance,
macOS binaries are notarized, the build is documented-reproducible, and the
SECURITY.md residual is updated.

---

## Phase 3 — Provider expansion

**Goal:** add a third coding agent (Gemini CLI) without forking the gateway,
by first paying down the one place provider support is hardcoded — the CLI/config
layer — then proving the seam with a real new vendor.

**Why now:** the *gateway* layer is already cleanly additive — a vendor is a
`Vendor{Name, BaseURL, Inject, ParseUsage, Prices}` value
(`internal/gateway/vendor.go`) plus a pricing table and a usage parser, and
nothing downstream enumerates vendors. The *CLI/config* layer is where "exactly
two providers" is wired in by hand, in five places:

- `internal/agent/agent.go` — a literal `switch` mapping `claude→anthropic`,
  `codex→openai`; an unknown agent is rejected.
- `internal/config/config.go` — `default_agent must be claude or codex`
  validation, plus one `*_auth` key per vendor (`anthropic_auth`, `openai_auth`).
- the setup wizard's "which agents?" menu (claude / codex / both).
- `drydock start`'s per-agent credential preflight.
- `drydock doctor` and `drydock auth`'s per-vendor branches.

Adding Gemini by copy-paste would mean touching all five and growing every
two-way branch into a three-way one — the classic special-case-on-shared-infra
smell. Generalize the seam first.

### 3A — Provider registry refactor *(do first; no new provider yet)*
Introduce one source of truth: a `Provider{Agent, Vendor, AuthModes, …}` table
that `agent.Vendor`, config validation, the wizard menu, `start`, `doctor`, and
`auth` all read from instead of hardcoding pairs. Pure refactor — `claude` and
`codex` still the only entries, behavior byte-identical, existing tests green.
The deliverable is that adding a row is the *only* edit a new provider needs in
this layer.

### 3B — Gemini (Google) *(first new provider — proves the seam)*
Add the `gemini → google` row: a `GoogleVendor()` (base URL + `Inject` for the
Google auth header), a pricing table, a usage parser, the sandbox-image CLI
install, and red-team coverage (A1 key-exfil, A2 egress) for the new vendor.
If 3A is right, the CLI/config layer change is one registry row.

### 3C — Generic OpenAI-compatible agent *(broadens reach cheaply)*
Many third-party model gateways speak the OpenAI wire format. A single
`openai-compat` provider whose base URL + key come from config covers a long
tail (local models, OpenRouter-style proxies) without a bespoke vendor each.
Gated on 3A's registry and 3B validating the shape.

### 3D — Config-declared providers *(stretch; only if demand is real)*
Let an operator declare a vendor entirely in config (name, base URL, auth
header template, price table) with no Go change. YAGNI until 3B/3C show the
registry's fields are the right ones — don't design the plugin format before
two real providers have exercised the seam.

**Done when:** a third agent (Gemini) runs end-to-end through the gateway with
its own red-team coverage, and the CLI/config layer gained it via a registry
row rather than a new branch in five files.

---

## Phase 4 — Reliability & hardening

**Goal:** close the gaps between "works on the happy path" and "survives the
operator's bad day" — crashes, partial failures, and the slow drift of pinned
inputs. These are correctness/durability gaps, not new features; each is scoped
so it can ship independently.

- **4.1 Crash recovery.** *Landed.* A `brokerd` killed mid-task left host-side
  orphans the per-task cleanup defers never reaped. Boot reconciliation now:
  enforces single-instance via an `flock` (`~/.drydock/brokerd.lock`) so a
  second daemon can't clobber a live task; reaps orphan `task-*` VMs and squid
  (pre-existing); sweeps stale stage dirs; and resolves tasks a crash
  interrupted to a distinct `interrupted` outcome instead of a stuck
  `running?`. (The in-memory concurrency slot was never the gap — a restart
  rebuilds the semaphore at full capacity; the slot-pinning bug was the
  approval-gate timeout, fixed separately.)
- **4.2 Push partial-failure.** The approved-diff push is a multi-step git
  sequence with no defined rollback if it fails partway (e.g. push rejected
  after a local commit). Define the failure contract and surface it in the
  audit row instead of leaving an ambiguous state.
- **4.3 Aggregate budget cap.** `task_budget_usd` caps a *single* task; nothing
  bounds the daily/total spend across tasks. Add an aggregate ceiling the
  gateway enforces, so a runaway loop of cheap tasks can't drain a key.
- **4.4 `drydock retry`.** Re-run a prior task from its audit record (same repo
  ref, prompt, allowlist) without reconstructing the invocation by hand —
  closes the loop with the per-task audit trail Phase 1 already produces.
- **4.5 Sandbox-image vulnerability scanning.** The SBOM (2.1) lists the
  image's packages; nothing yet *scans* them. Run `grype`/`trivy` over the
  built image in CI and fail on known-critical CVEs — the image-side analogue
  of `govulncheck` (2.5) for Go deps.
- **4.6 Agent-CLI bump automation.** The pinned agent CLIs
  (`@anthropic-ai/claude-code`, `@openai/codex`) drift; a scheduled job that
  proposes a pinned-version bump PR (with the red-team suite as the gate) keeps
  them current without unpinning.
- **4.7 Observability.** Structured run metrics (durations, gate latencies,
  egress-widen frequency, budget burn) beyond the per-task JSONL — enough to
  answer "what is drydock doing across many runs" without grepping audit files.
- **4.8 Runtime abstraction.** The VM backend is Apple `container`-specific.
  Factor the container operations behind an interface so an alternative backend
  (e.g. Linux microVM) is a port, not a rewrite. *Stretch — only once a second
  backend is actually wanted; don't abstract for one implementation.*
- **4.9 Egress depth (IPv6 / plain-HTTP).** The allowlist proxy is HTTPS/CONNECT
  and IPv4-centric; audit and document behavior for IPv6 literals and plain-HTTP
  CONNECT, and either enforce or explicitly state the limit (no silent gaps —
  the honesty constraint applies to egress edges too).

**Done when:** a `brokerd` crash leaves no orphaned VM or wedged slot, spend is
bounded in aggregate, the sandbox image is CVE-scanned in CI, and each
remaining edge is either enforced or documented as a stated limit.

---

## Sequencing

1. **Phase 1 first** (1.1 → 1.2 → 1.3 → 1.4) — highest credibility, fully in
   our control, makes every later phase (and an eventual audit) cheaper.
2. **Phase 2 in parallel where it's free:** 2.3 (cosign/SLSA) + 2.1 (SBOM)
   first, then 2.4 / 2.5 (build docs + CI guards), then 2.2 (notarization) once
   the Apple cert is in hand.
3. **Phase 4 reliability is interleaved, not deferred** — 4.1 (crash recovery)
   and 4.3 (aggregate budget) are correctness gaps and rank ahead of new
   providers; 4.5 (image CVE scan) rides alongside Phase 2's supply-chain work.
4. **Phase 3 providers gate on 3A** — the registry refactor lands before any
   new vendor, so Gemini (3B) and the rest are registry rows, not five-file
   branches.

**Prerequisites to flag:**
- The A1 / A2 / A7 red-team tests need the VM, so full `make redteam` is a
  macOS / Apple-silicon gate; CI runs only the host-side subset (A3–A6).
- 2.2 notarization requires a paid Apple Developer ID certificate.
- 3B Gemini and 3C openai-compat need their own A1/A2 red-team coverage before
  they count as shipped — a new credential path is a new attack surface.

**First concrete deliverable:** the `make redteam` skeleton + the A1 key-exfil
test — the single most convincing artifact.
