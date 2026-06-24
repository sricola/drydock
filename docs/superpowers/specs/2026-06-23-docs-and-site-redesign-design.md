# Docs & site redesign

**Date:** 2026-06-23
**Status:** Approved (design)

## Problem

drydock's docs and landing site are good but not digestible, and they
under-sell the project's trust story. Concretely (from an engineer +
technical-writer pass, a PM critique, and a designer critique):

- **README is a 506-line operator manual, not a conversion funnel.** No TOC, no
  "who this is for / not for," `Install` appears before any "how it works," and
  the four subscription/API-key auth paths are duplicated prose walls (the same
  5-step pattern repeated, with the "important limits" warning verbatim twice).
- **The landing page asks a skeptic to trust 3 bullets + a GIF.** The
  stylesheet already contains a complete, unused **threat table** (`.tbl`,
  `.tag.host/.vm/.oos`) and **architecture/VM-boundary diagram**
  (`.archdiag`, `.grid2`, `.col.vm/.col.host`, `.flowrow`, `.seam`) — built but
  never placed in the HTML. The hero buries the install command below a 155 KB
  GIF.
- **`drydock redteam` — the strongest, third-party-verifiable trust asset
  ("run the real attacks yourself, no API spend, 5 min")** — is buried in an
  operator subsection and absent from the site.
- **Per-task egress widening** (now functional as of v0.3.0) is under-documented;
  it's the direct answer to the "deny-by-default blocks my workflow" objection.
- **Stale `v0.2.0`** appears in the README status/install and the site
  herostatus/footer; current release is **v0.3.0**. For a security tool this is
  a credibility tax. The README's Homebrew block and the site's install command
  also disagree on the exact `brew` invocation.
- **Polish gaps:** dark-mode CSS variables exist but no `@media
  (prefers-color-scheme: dark)` wires them; no `prefers-reduced-motion`; no
  `:focus-visible` rings; the mobile nav hides the wrong link.

## Goal

Redesign both the documentation and the site so a skeptical, security-minded
Mac developer (1) understands what drydock is and why they'd want it within ~15
seconds, (2) can find proof and operate it without wading through a wall of
text, and (3) trusts it enough to install. Decided scope (from brainstorming):
**full redesign**, structured as a **focused landing page + a real multi-page
docs site**, on a **warm-editorial visual system with terminal + blueprint
accents**, with docs authored in **Markdown rendered Go-natively to HTML**.

## Non-goals

- No new product features or behavior changes — this is docs/site only.
- No JS framework / heavyweight SSG. Stay dependency-light, matching the repo's
  "Go-native, no external binary" ethos (`make sbom`, `make dist`).
- No rewrite of `THREAT_MODEL.md` / `SECURITY.md` content (they are linked,
  surfaced, and a compact threat *table* is derived from the threat model — but
  the canonical documents stay as-is).

## Information architecture

### Landing page — `site/index.html` (conversion funnel)

Sections, in order:

1. **Hero** — headline ("Let a coding agent run wild on your Mac — without
   trusting it"), one-line lede, **install command up high**, two CTAs
   (Install / Read the threat model). The red-team GIF moves **below** install.
2. **Containment strip** — `sealed VM → gated → host` with red `.vm` / green
   `.host` pills (accent borrowed from direction A).
3. **How it works** — three steps: point it at a repo → it runs sealed → you
   approve the diff.
4. **Architecture diagram** — the sealed-VM ↔ credential gateway (:8088) +
   egress proxy (:3128) ↔ host boundary, built with the existing `.archdiag` /
   `.grid2` / `.col.vm` / `.col.host` / `.seam` CSS (direction C, full build).
5. **Threat table** — columns *Attack vector / What drydock does / Residual
   risk*, using the existing `.tbl` + `.tag.host/.vm/.oos` CSS; rows derived
   from the threat model (at minimum A1 key isolation, A2 egress, A7 VM
   teardown).
6. **Prove it yourself** — dedicated `drydock redteam` callout: runs the real
   attacks locally, **no API spend**, under 5 minutes.
7. **Works with** (Claude Code / Codex) + a one-line auth summary with a ToS
   caveat link · **Honest limits** (alpha, no third-party audit, macOS 26+
   Apple-silicon requirement) · **Install** (full block: Homebrew + from
   source).

The GIF below install keeps a short `figcaption`; nav gains a **Threat model**
link and a **Docs** link.

### Docs site — new `site/docs/`

The operator manual, lifted out of the README into focused Markdown pages, each
rendered into the shared site template with a docs sidebar/nav:

- `quickstart.md` — install → first task in ~60 seconds.
- `authentication.md` — the 2×2 auth matrix (Claude/Codex × API-key/
  subscription) as one table, with the OAuth/ToS caveats in `<details>` /
  callouts (symmetric for both subscription paths).
- `submitting-tasks.md` — `drydock submit`, the approval gate
  (`review`/`approve`/`deny`), output example, flags/scripting.
- `egress.md` — default allowlist, how enforcement works (gateway + squid),
  and **per-task widening** (`--egress-extra`, the `per_task_widening` gate) as
  the answer to "deny-by-default blocks my workflow."
- `configuration.md` — the config reference, split into a common-case table and
  an advanced/path-overrides table.
- `troubleshooting.md` — `drydock doctor`, common failure modes.
- `threat-model.md` — a docs-site-styled entry point that summarizes and links
  the canonical `THREAT_MODEL.md`.

### README — slim overview

Shrinks to: logo + pitch + status callout (v0.3.0) + **"who this is for / not
for"** + a 60-second quickstart + links into the docs site and threat model.
Contributor-only content (the `Layout` codebase map, `Build/test/CI`, `Known
gaps`) moves to a new **`CONTRIBUTING.md`**.

## Visual system

Core is direction **B (warm editorial)** with accents from A and C:

- **Tokens:** warm-paper light (`--bg:#fbfaf6`, `--bg2`, `--ink:#1b1a17`,
  `--mut`, single green `--grn`, dark accent `--accent:#0f1411`), defined as CSS
  custom properties on `:root`.
- **Type:** confident editorial headline scale; system sans for body; mono for
  terminal/code and labels.
- **Recurring components:** the dark **terminal card** (install + sample run),
  **containment pills** (`.vm` red / `.host` green), the **blueprint
  architecture diagram**, the **threat table**, trust **cards**.
- **One shared stylesheet** drives both the landing page and the docs template.
- **Polish (required):** `@media (prefers-color-scheme: dark)` swapping the
  ~8–10 root vars to wire the already-shipped dark logo/assets;
  `@media (prefers-reduced-motion: reduce)` neutralizing `.reveal`;
  `:focus-visible` rings on all interactive elements (buttons, nav, copy,
  cards); fix the mobile nav to drop a low-value link instead of `#limits`;
  shorter GIF `alt` with the explanation in the `figcaption`.

## Build & deploy

Docs are **Markdown rendered to HTML Go-natively**:

- New `cmd/docs-build/` — a small Go program using **goldmark** (pinned; the
  standard Go markdown library) that reads `site/docs/*.md` + a shared HTML
  template + the shared stylesheet and writes `site/docs/*.html` (plus an index
  / sidebar). Deterministic output (no timestamps) so the build is reproducible.
- **`make docs`** target runs it; output lands under `site/` so GitHub Pages
  serves it.
- **`pages.yml`** gains a build step (`make docs`) before
  `upload-pages-artifact`. The workflow already pins `actions/*` to current
  node24 majors.
- The landing page (`index.html`) stays hand-authored HTML (it's bespoke); only
  the *docs pages* are generated.
- One new Go dependency (`github.com/yuin/goldmark`), pinned in `go.mod`; no JS
  toolchain.

## Testing

- **Build:** `make docs` produces the expected `site/docs/*.html` from the `.md`
  sources; `go build ./...` / `go vet ./...` stay green; the generated HTML is
  deterministic (running twice yields no diff).
- **Unit:** `cmd/docs-build` has tests for the render pipeline — a known
  Markdown input produces the expected HTML fragments (headings → anchors, code
  fences → `<pre>`, the template wraps content with the shared nav/CSS).
- **Link/asset check:** a test (or `make` check) that every internal link in the
  generated docs + README + landing page resolves to an existing file/anchor,
  and that referenced assets exist — catches the stale-link / missing-asset
  class of bug.
- **Content correctness:** a guard test that no shipped doc/site file still
  contains `v0.2.0` (or, better, that the version string matches the latest tag
  / a single source), so the version never silently goes stale again.
- **Manual:** render the landing page + one docs page headless (the same
  Chrome-headless screenshot path used to produce the design mockups) and
  eyeball hero, diagram, threat table, dark mode, and mobile width before
  shipping.

## Rollout / sequencing (for the plan)

1. Shared design system (stylesheet + tokens + dark mode + a11y) and the
   `cmd/docs-build` renderer + `make docs` + `pages.yml` step — the foundation.
2. Landing-page rebuild (hero → containment → how-it-works → **architecture
   diagram** → **threat table** → **redteam** → works/limits/install), wiring
   the existing unused CSS components.
3. Docs pages authored in Markdown (quickstart, auth, submitting, egress,
   config, troubleshooting, threat-model), content lifted + de-duplicated from
   the README.
4. README slim-down + `CONTRIBUTING.md`; version + brew-command fixes
   everywhere; redteam + egress-widening + audience content; the version guard
   test.

Each step leaves the site building and deployable.

## Risks

- **Content drift between README and docs site** — mitigated by making the docs
  site canonical for operator content and the README a thin pointer, plus the
  internal-link check.
- **goldmark output churn** — pin the version; assert deterministic output in a
  test so a dependency bump can't silently reformat every page.
- **Design regressions on mobile / dark mode** — covered by the manual headless
  render check at the two breakpoints and the dark scheme.
