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

### 2.4 Reproducible builds
`-trimpath` is already set. Pin the Go toolchain (go.mod `go` directive +
documented version), document the exact build env, and add `make verify-build`
(rebuild → compare to the published sha256).

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

## Sequencing

1. **Phase 1 first** (1.1 → 1.2 → 1.3 → 1.4) — highest credibility, fully in
   our control, makes every later phase (and an eventual audit) cheaper.
2. **Phase 2 in parallel where it's free:** 2.3 (cosign/SLSA) + 2.1 (SBOM)
   first, then 2.4 / 2.5 (build docs + CI guards), then 2.2 (notarization) once
   the Apple cert is in hand.

**Prerequisites to flag:**
- The A1 / A2 / A7 red-team tests need the VM, so full `make redteam` is a
  macOS / Apple-silicon gate; CI runs only the host-side subset (A3–A6).
- 2.2 notarization requires a paid Apple Developer ID certificate.

**First concrete deliverable:** the `make redteam` skeleton + the A1 key-exfil
test — the single most convincing artifact.
