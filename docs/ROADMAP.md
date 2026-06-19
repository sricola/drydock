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
| **A1** agent reads the real vendor key | in-VM: grep `/proc/self/environ`, fs, env for `sk-ant`/`sk-`; assert only `tok_` present | **gap** | VM | _to add_ |
| **A2** agent reaches a hostile/unintended host | in-VM: `curl https://evil.example` + raw DNS; assert both blocked | **gap** | VM | _to add_ (doctor covers a slice) |
| **A3** agent plants a `.git` hook → host RCE | host commits a work tree with a planted `pre-commit`; assert it never runs on the host | **covered** | host | `TestHostCommit_IgnoresPlantedHook` |
| **A4** agent leaks `.task/` (prompt/allowlist) into the PR | write inside `.task/`; assert it is excluded from the captured diff | **covered** | host | `TestCaptureDiff_ExcludesTaskDir` |
| **A5** agent crafts a malicious diff to bypass the gate | submit without `auto_approve`; deny; assert nothing is pushed | **partial** | host | `TestHandleTask_GatedDeny_NoPush` (promote/label) |
| **A6** agent widens its own egress | submit `egress_extra: evil:443`; deny; assert it never compiled into squid | **gap** | host | _to add_ (`gateEgressWiden`) |
| **A7** task state persists between tasks | run two tasks; assert task 2 cannot see task 1's VM/stage; `--rm` + `Cleanup` ran | **gap** | VM (+ host `TestCleanup_*`) | _to add_ |

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

### 2.5 Dependency-pinning policy
Pin the Debian base image by digest; document the pin/bump policy (claude-code,
codex, Go are already `ARG`-pinned); add `govulncheck` to CI.

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
