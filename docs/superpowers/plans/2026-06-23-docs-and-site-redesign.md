# Docs & Site Redesign — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Redesign drydock's docs and site into a focused conversion-oriented landing page plus a Markdown-sourced, Go-rendered multi-page docs site, on a warm-editorial visual system with terminal + blueprint accents.

**Architecture:** A small Go program (`cmd/docs-build`, goldmark) renders `site/docs/*.md` into HTML using one shared template + stylesheet; the landing page (`site/index.html`) is hand-authored HTML on the same stylesheet; the README slims to an overview pointing into the docs. GitHub Pages serves `site/` after `make docs`.

**Tech Stack:** Go 1.26, `github.com/yuin/goldmark` (new, pinned), static HTML/CSS, GitHub Pages, Chrome-headless for render verification.

**Spec:** `docs/superpowers/specs/2026-06-23-docs-and-site-redesign-design.md`

## Global Constraints

- Go floor `go 1.26.4`. Exactly **one** new direct dependency: `github.com/yuin/goldmark` (pinned in go.mod/go.sum). No JS toolchain, no SSG.
- **No product/behavior changes** — docs and site only.
- Current version string is **`v0.3.0`**; it must appear nowhere as `v0.2.0`. A guard test enforces this.
- Visual system = **warm-editorial (B)**: `--bg:#fbfaf6`, `--bg2:#f3f1ea`, `--ink:#1b1a17`, `--mut:#6b6862`, `--grn:#1f8f4e`, `--accent:#0f1411`, defined as `:root` CSS custom properties; **plus** A's containment pills (`.vm` red / `.host` green) and C's blueprint architecture diagram + threat table.
- Required polish on every page: `@media (prefers-color-scheme: dark)` (swap root vars), `@media (prefers-reduced-motion: reduce)` (neutralize `.reveal`), `:focus-visible` rings on all interactive elements.
- Docs site is **canonical** for operator content; the README is a thin pointer. No content duplicated between them beyond the 60-second quickstart.
- Generated docs HTML must be **deterministic** (byte-identical on re-run; no timestamps/random ordering).
- `drydock redteam` ("run the real attacks yourself, no API spend, ~5 min") and per-task egress widening (`--egress-extra`) each get a real, surfaced treatment.

## File structure

- Create `cmd/docs-build/main.go` — walks `site/docs/*.md`, renders each to `site/docs/<name>.html`, builds the shared sidebar.
- Create `cmd/docs-build/render.go` — pure render functions (markdown→HTML body, heading anchors, page-into-template).
- Create `cmd/docs-build/render_test.go` — TDD for render.go.
- Create `cmd/docs-build/site_test.go` — version-staleness guard + internal-link/asset check over `site/` and `README.md`.
- Create `site/style.css` — the one shared stylesheet (tokens + components + dark mode + a11y).
- Create `site/docs/_template.html` — shared HTML shell (head, nav, sidebar slot, content slot, footer).
- Create `site/docs/{quickstart,authentication,submitting-tasks,egress,configuration,troubleshooting,threat-model}.md`.
- Create `site/docs/index.md` — docs landing / table of contents.
- Modify `site/index.html` — rebuilt landing page on `style.css`.
- Modify `Makefile` — add `docs` target + `.PHONY`.
- Modify `.github/workflows/pages.yml` — run `make docs` before upload.
- Modify `README.md` — slim to overview.
- Create `CONTRIBUTING.md` — contributor content moved out of README.
- Modify `go.mod` / `go.sum` — add goldmark.

---

## Task 1: Go docs renderer + `make docs` + pages build step

**Files:**
- Create: `cmd/docs-build/render.go`, `cmd/docs-build/main.go`, `cmd/docs-build/render_test.go`
- Create: `site/docs/_template.html`
- Modify: `Makefile`, `.github/workflows/pages.yml`, `go.mod`, `go.sum`

**Interfaces:**
- Produces: `func renderMarkdown(src []byte) (body string, headings []Heading, err error)` — goldmark render with auto-slugged heading anchors; `Heading{Level int; Text, ID string}`.
- Produces: `func renderPage(tmpl, body, title, sidebar, relPrefix string) string` — substitutes `{{title}}`, `{{content}}`, `{{sidebar}}`, `{{base}}` in the template.
- Produces: `func buildSidebar(pages []Page, current string) string`; `type Page{Slug, Title, File string}`.
- Produces (CLI): `go run ./cmd/docs-build` reads `site/docs/*.md` (front-matter `# Title` = first H1) and writes `site/docs/*.html`.

- [ ] **Step 1: Add goldmark**

Run: `go get github.com/yuin/goldmark@v1.7.8`
Expected: go.mod gains `require github.com/yuin/goldmark v1.7.8`; go.sum updated.

- [ ] **Step 2: Write the failing render test**

Create `cmd/docs-build/render_test.go`:
```go
package main

import "strings"
import "testing"

func TestRenderMarkdown_HeadingsAndCode(t *testing.T) {
	body, headings, err := renderMarkdown([]byte("# Quickstart\n\nText.\n\n## Install it\n\n```\nbrew install x\n```\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `<h2 id="install-it">`) {
		t.Errorf("H2 should get a slug anchor id; got:\n%s", body)
	}
	if !strings.Contains(body, "<pre") || !strings.Contains(body, "brew install x") {
		t.Errorf("code fence should render to <pre>; got:\n%s", body)
	}
	if len(headings) < 2 || headings[1].ID != "install-it" || headings[1].Text != "Install it" {
		t.Errorf("headings = %+v, want H1 Quickstart + H2 Install it (#install-it)", headings)
	}
}

func TestRenderPage_SubstitutesSlots(t *testing.T) {
	out := renderPage("<title>{{title}}</title><nav>{{sidebar}}</nav><main>{{content}}</main><link href=\"{{base}}style.css\">",
		"<p>hi</p>", "Quickstart", "<ul>SIDEBAR</ul>", "../")
	for _, want := range []string{"<title>Quickstart</title>", "<ul>SIDEBAR</ul>", "<p>hi</p>", `href="../style.css"`} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered page missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderMarkdown_Deterministic(t *testing.T) {
	src := []byte("# A\n\ntext\n\n## B\n\nmore\n")
	a, _, _ := renderMarkdown(src)
	b, _, _ := renderMarkdown(src)
	if a != b {
		t.Errorf("render not deterministic")
	}
}
```

- [ ] **Step 3: Run the test (red)**

Run: `go test ./cmd/docs-build/ -run TestRender`
Expected: FAIL — `undefined: renderMarkdown` / `renderPage`.

- [ ] **Step 4: Implement render.go**

Create `cmd/docs-build/render.go`:
```go
package main

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

// Heading is one rendered heading, used to build per-page on-this-page nav.
type Heading struct {
	Level int
	Text  string
	ID    string
}

// Page is one docs page in the sidebar.
type Page struct {
	Slug  string // file base, e.g. "quickstart"
	Title string // H1 text
	File  string // "quickstart.html"
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slug lowercases and hyphenates heading text for a stable anchor id.
func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonSlug.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(gmhtml.WithUnsafe()), // our docs are trusted source
)

var headingRe = regexp.MustCompile(`(?s)<h([1-6])>(.*?)</h[1-6]>`)
var tagRe = regexp.MustCompile(`<[^>]+>`)

// renderMarkdown converts docs Markdown to an HTML body and adds a slug id to
// every heading (so the sidebar/anchors are stable). Deterministic.
func renderMarkdown(src []byte) (string, []Heading, error) {
	var buf bytes.Buffer
	if err := md.Convert(src, &buf); err != nil {
		return "", nil, err
	}
	var heads []Heading
	out := headingRe.ReplaceAllStringFunc(buf.String(), func(m string) string {
		sm := headingRe.FindStringSubmatch(m)
		level := int(sm[1][0] - '0')
		text := strings.TrimSpace(tagRe.ReplaceAllString(sm[2], ""))
		id := slug(text)
		heads = append(heads, Heading{Level: level, Text: text, ID: id})
		return "<h" + sm[1] + ` id="` + id + `">` + sm[2] + "</h" + sm[1] + ">"
	})
	return out, heads, nil
}

// renderPage substitutes the template slots. {{base}} is the relative path back
// to site root (so docs pages link assets as {{base}}style.css).
func renderPage(tmpl, body, title, sidebar, base string) string {
	r := strings.NewReplacer(
		"{{title}}", title,
		"{{content}}", body,
		"{{sidebar}}", sidebar,
		"{{base}}", base,
	)
	return r.Replace(tmpl)
}

// buildSidebar renders the docs nav, marking current.
func buildSidebar(pages []Page, current string) string {
	var b strings.Builder
	b.WriteString("<ul class=\"docnav\">")
	for _, p := range pages {
		cls := ""
		if p.Slug == current {
			cls = ` class="on"`
		}
		b.WriteString("<li><a" + cls + ` href="` + p.File + `">` + p.Title + "</a></li>")
	}
	b.WriteString("</ul>")
	return b.String()
}
```

- [ ] **Step 5: Run the test (green)**

Run: `go test ./cmd/docs-build/ -run TestRender`
Expected: PASS (3 tests).

- [ ] **Step 6: Implement main.go (the build driver)**

Create `cmd/docs-build/main.go`:
```go
// Command docs-build renders site/docs/*.md into site/docs/*.html using one
// shared template + stylesheet. Go-native, deterministic, no external binary.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const docsDir = "site/docs"

var h1Re = regexp.MustCompile(`(?m)^#\s+(.+?)\s*$`)

// order pins sidebar order; files not listed sort after, alphabetically.
var order = []string{"index", "quickstart", "authentication", "submitting-tasks", "egress", "configuration", "troubleshooting", "threat-model"}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "docs-build:", err)
		os.Exit(1)
	}
}

func run() error {
	tmpl, err := os.ReadFile(filepath.Join(docsDir, "_template.html"))
	if err != nil {
		return err
	}
	mds, _ := filepath.Glob(filepath.Join(docsDir, "*.md"))
	sort.Slice(mds, func(i, j int) bool { return rank(mds[i]) < rank(mds[j]) })

	var pages []Page
	for _, f := range mds {
		slugName := strings.TrimSuffix(filepath.Base(f), ".md")
		src, _ := os.ReadFile(f)
		title := slugName
		if m := h1Re.FindSubmatch(src); m != nil {
			title = string(m[1])
		}
		pages = append(pages, Page{Slug: slugName, Title: title, File: slugName + ".html"})
	}
	for _, f := range mds {
		slugName := strings.TrimSuffix(filepath.Base(f), ".md")
		src, _ := os.ReadFile(f)
		body, _, err := renderMarkdown(src)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		title := slugName
		for _, p := range pages {
			if p.Slug == slugName {
				title = p.Title
			}
		}
		page := renderPage(string(tmpl), body, title, buildSidebar(pages, slugName), "../")
		out := filepath.Join(docsDir, slugName+".html")
		if err := os.WriteFile(out, []byte(page), 0o644); err != nil {
			return err
		}
		fmt.Println("==>", out)
	}
	return nil
}

func rank(path string) int {
	base := strings.TrimSuffix(filepath.Base(path), ".md")
	for i, o := range order {
		if o == base {
			return i
		}
	}
	return len(order) + 1
}
```

- [ ] **Step 7: Create a minimal template so the build runs**

Create `site/docs/_template.html` (the design-system styling lands in Task 2's `style.css`; this is the structural shell):
```html
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{title}} · drydock docs</title>
<link rel="stylesheet" href="{{base}}style.css">
<link rel="icon" href="{{base}}favicon.svg">
</head>
<body class="docs">
<header class="topbar"><div class="wrap"><a class="brand" href="{{base}}index.html">drydock</a>
<nav class="nav"><a href="{{base}}index.html">Home</a><a href="index.html">Docs</a><a href="https://github.com/sricola/drydock">GitHub</a></nav></div></header>
<div class="docwrap">
<aside class="docside">{{sidebar}}</aside>
<main class="doccontent">{{content}}</main>
</div>
<footer class="foot"><div class="wrap">drydock · MIT · <a href="{{base}}index.html#install">install</a></div></footer>
</body>
</html>
```

- [ ] **Step 8: Create a placeholder docs page so the build has input**

Create `site/docs/index.md`:
```markdown
# drydock docs

Operator documentation for drydock. Start with the [Quickstart](quickstart.html).
```
Create `site/docs/quickstart.md`:
```markdown
# Quickstart

Placeholder — real content lands in Task 4.
```

- [ ] **Step 9: Add the `docs` Makefile target**

In `Makefile`, add `docs` to the `.PHONY` line and add the target (mirrors the Go-native `sbom`/`dist` style):
```makefile
# docs renders site/docs/*.md into site/docs/*.html for GitHub Pages.
# Go-native (no external binary); deterministic output.
docs:
	go run ./cmd/docs-build
	@echo "==> site/docs built"
```

- [ ] **Step 10: Run the build**

Run: `make docs`
Expected: prints `==> site/docs/index.html`, `==> site/docs/quickstart.html`; both files exist.

Run: `make docs && git diff --stat site/docs/*.html` (run twice)
Expected: second run produces no diff (deterministic).

- [ ] **Step 11: Wire the Pages workflow**

In `.github/workflows/pages.yml`, add a build step before `upload-pages-artifact`:
```yaml
      - uses: actions/setup-go@v6
        with:
          go-version-file: go.mod
          cache: true
      - run: make docs
```
(Place the two steps right after `actions/checkout@v5` and before `actions/configure-pages@v6`.)

- [ ] **Step 12: Full build + commit**

Run: `go build ./... && go vet ./... && go test ./cmd/docs-build/`
Expected: PASS.
```bash
git add cmd/docs-build go.mod go.sum site/docs Makefile .github/workflows/pages.yml
git commit -m "docs-build: Go-native markdown->html renderer + make docs + pages step"
```

---

## Task 2: Shared design-system stylesheet (`site/style.css`)

**Files:**
- Create: `site/style.css`
- Reference: the inline `<style>` currently in `site/index.html` (source of the existing components to preserve: `.term`, `.tbl`, `.tag.*`, `.archdiag`, `.grid2`, `.col.vm/.host`, `.seam`, `.btn`, `.reveal`)

**Verification:** this is a visual artifact — verified by headless render (Step 4) and consumed by Tasks 1/3. No unit test.

- [ ] **Step 1: Extract + author the stylesheet**

Create `site/style.css` containing one design system used by the landing page and the docs template:
- `:root` tokens exactly per Global Constraints (warm-editorial), plus spacing/radius/shadow scale and a mono + editorial type scale.
- Components: `.topbar/.wrap/.brand/.nav`, hero (`.hero/.kicker/.lede/.btn/.btn2`), the dark **terminal** card (`.term/.bar/.dot`), **containment** pills (`.pill.vm` red / `.pill.host` green) + the `.seam`/arrow strip, the **blueprint architecture diagram** (`.archdiag/.grid2/.col.vm/.col.host/.node/.gw/.seam`), the **threat table** (`.tbl/.tag.host/.tag.vm/.tag.oos`), trust **cards** (`.card`), and the **docs** layout (`.docwrap/.docside/.docnav/.doccontent` with readable measure, styled headings/code/tables for rendered markdown).
- Required: `@media (prefers-color-scheme: dark)` swapping the ~10 root vars (wire the shipped dark logo); `@media (prefers-reduced-motion: reduce){ .reveal{opacity:1;transform:none;transition:none} }`; `:focus-visible` ring (green, offset) on `a, button, .btn, .copy, .card`.
- Responsive: docs collapses the sidebar under ~860px; the landing grid stacks under ~720px; mobile nav drops `#works`/`#how` rather than the limits/threat link.

**Acceptance criteria (checked at Step 4):** light + dark both legible; the terminal, containment strip, architecture diagram, and threat table all styled; focus rings visible on keyboard nav; no layout break at 390px and 1280px widths.

- [ ] **Step 2: Point the docs template at it**

The template from Task 1 already links `{{base}}style.css`; rebuild: `make docs`.

- [ ] **Step 3: Add a render probe**

Create a throwaway `/tmp/probe.html` that links `site/style.css` and exercises each component (hero, term, pills, archdiag, tbl, card) so the stylesheet can be eyeballed in isolation.

- [ ] **Step 4: Headless render check (light + dark + mobile)**

Run (mirrors the mockup pipeline):
```bash
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
"$CHROME" --headless=new --force-device-scale-factor=2 --screenshot=/tmp/probe.png --window-size=1280,1400 file:///tmp/probe.html
"$CHROME" --headless=new --force-device-scale-factor=2 --screenshot=/tmp/probe-m.png --window-size=390,1400 file:///tmp/probe.html
```
Expected: open the PNGs; confirm the acceptance criteria. Iterate on `style.css` until they pass.

- [ ] **Step 5: Commit**

```bash
git add site/style.css
git commit -m "site: shared warm-editorial design system stylesheet (dark mode + a11y)"
```

---

## Task 3: Landing page rebuild (`site/index.html`)

**Files:**
- Modify: `site/index.html` (replace inline `<style>` with `<link rel="stylesheet" href="style.css">`; rebuild the body in the spec's section order)

**Verification:** headless render screenshot + the internal-link/version checks (Task 5). No unit test.

- [ ] **Step 1: Rebuild the page**

Replace `site/index.html`'s body with sections in this exact order, on `style.css`:
1. **Topbar/nav** — links: How it works, Architecture, Threat model, Docs (`docs/index.html`), GitHub.
2. **Hero** — H1 "Let a coding agent run wild on your Mac — without trusting it"; one-line lede; the **install command** (terminal card) high up; CTAs *Install* + *Read the threat model*.
3. **Containment strip** — `sealed VM → gated → host`, red `.pill.vm` (poisoned repo / prompt injection) → green `.pill.host` (key safe / repo safe).
4. **How it works** — 3 steps (point → runs sealed → approve diff).
5. **Architecture diagram** (`#architecture`) — VM ↔ credential gateway (:8088) + egress proxy (:3128) ↔ host, using `.archdiag/.grid2/.col.vm/.col.host/.seam`.
6. **Threat table** (`.tbl`) — columns *Attack / What drydock does / Residual risk*, rows A1 (key isolation), A2 (egress), A7 (VM teardown), tagged `.tag.host/.vm/.oos`.
7. **Prove it yourself** — `drydock redteam` callout: real attacks, **no API spend**, ~5 min; below it the **breach GIF** with a short caption.
8. **Works with** (Claude Code / Codex) + one-line auth summary linking `docs/authentication.html` (with the ToS caveat); **Honest limits** (alpha, no third-party audit, macOS 26+ Apple silicon); **Install** (Homebrew + from source, exact commands matching the README).

**Required content:** every version string is `v0.3.0`; the `brew` command matches the README's exactly; GIF `alt` is short with the explanation in `figcaption`.

- [ ] **Step 2: Headless render (desktop + mobile)**

Run the Chrome-headless screenshot on `site/index.html` at `1280` and `390` widths (and toggle dark via `--force-dark-mode` or a dark probe). Confirm: install visible without scrolling past the GIF; the architecture diagram and threat table render; nav + focus states work.

- [ ] **Step 3: Commit**

```bash
git add site/index.html
git commit -m "site: rebuild landing page (funnel order, architecture diagram, threat table, redteam)"
```

---

## Task 4: Docs content (Markdown pages)

**Files:**
- Create/replace: `site/docs/{index,quickstart,authentication,submitting-tasks,egress,configuration,troubleshooting,threat-model}.md`
- Source: lift + de-duplicate from the current `README.md` sections.

**Verification:** `make docs` renders all pages; the internal-link check (Task 5) passes; headless eyeball of one rendered page.

- [ ] **Step 1: Author the pages**

Each `.md` starts with a single `# Title` H1 (drives sidebar + `<title>`). Content map (lifted from README, de-duplicated, brought to v0.3.0):
- `index.md` — what the docs cover + links to each page.
- `quickstart.md` — install → first task in ~60s (Homebrew, `drydock setup`, `drydock submit`, approve, see the diff).
- `authentication.md` — the **2×2 auth matrix as one table** (Claude/Codex × API-key/subscription); the OAuth/ToS caveats in `<details>` or a callout, **symmetric for both subscription paths**.
- `submitting-tasks.md` — `drydock submit`, the approval gate (`review`/`approve`/`deny`) prominently, the live-output example, flags + scripting (`--json`, `--quiet`, `--auto-approve`).
- `egress.md` — default allowlist, how enforcement works (gateway :8088 + squid :3128), and **per-task widening** (`--egress-extra host:port`, the `per_task_widening` approval gate) framed as the answer to "deny-by-default blocks my workflow."
- `configuration.md` — config reference split into a common-case table (agent, budget, timeout, concurrency, auth mode) and an advanced/path-overrides table.
- `troubleshooting.md` — `drydock doctor`, common failures + fixes.
- `threat-model.md` — short orientation + the compact threat table, linking the canonical `THREAT_MODEL.md`.

- [ ] **Step 2: Build + spot-check**

Run: `make docs`
Expected: 8 `==>` lines; open `site/docs/quickstart.html` headless and confirm the shared template + styling render.

- [ ] **Step 3: Commit**

```bash
git add site/docs/*.md site/docs/*.html
git commit -m "docs: author quickstart/auth/submitting/egress/config/troubleshooting/threat-model pages"
```

---

## Task 5: README slim-down, CONTRIBUTING.md, version + link guards

**Files:**
- Modify: `README.md` (slim to overview)
- Create: `CONTRIBUTING.md`
- Create: `cmd/docs-build/site_test.go` (version guard + internal-link/asset check)

**Interfaces:**
- Consumes: the docs pages (Task 4) and landing page (Task 3) for the link check.

- [ ] **Step 1: Write the failing guard tests**

Create `cmd/docs-build/site_test.go`:
```go
package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot walks up from the test's cwd to the module root (where go.mod is).
func repoRoot(t *testing.T) string {
	t.Helper()
	d, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		p := filepath.Dir(d)
		if p == d {
			t.Fatal("go.mod not found")
		}
		d = p
	}
}

// TestNoStaleVersion fails if any shipped doc/site file mentions an old version.
func TestNoStaleVersion(t *testing.T) {
	root := repoRoot(t)
	files := []string{"README.md", "site/index.html"}
	docs, _ := filepath.Glob(filepath.Join(root, "site/docs/*.md"))
	for _, d := range docs {
		files = append(files, d[len(root)+1:])
	}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			continue
		}
		if strings.Contains(string(b), "v0.2.0") {
			t.Errorf("%s still references v0.2.0 (current is v0.3.0)", f)
		}
	}
}

var hrefRe = regexp.MustCompile(`(?:href|src)="([^"#:?][^":]*?)(?:#[^"]*)?"`)

// TestLandingInternalLinksResolve checks every relative href/src on the landing
// page points at a file that exists under site/.
func TestLandingInternalLinksResolve(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "site/index.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range hrefRe.FindAllStringSubmatch(string(b), -1) {
		target := m[1]
		if strings.HasPrefix(target, "//") || strings.HasPrefix(target, "http") || strings.HasPrefix(target, "mailto") {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, "site", target)); err != nil {
			t.Errorf("landing page links missing asset/page: %q", target)
		}
	}
}
```

- [ ] **Step 2: Run the tests (red)**

Run: `go test ./cmd/docs-build/ -run 'TestNoStaleVersion|TestLanding'`
Expected: FAIL — `README.md`/`site/index.html` still say `v0.2.0`, and/or landing links don't resolve yet (depending on Task 3 state).

- [ ] **Step 3: Slim the README**

Rewrite `README.md` to: logo + pitch + status callout (**v0.3.0**) + **"who this is for / not for"** (3 bullets each) + a 60-second quickstart + links into `site/docs/` and `THREAT_MODEL.md`. Remove the `Layout`, `Build/test/CI`, and `Known gaps` sections (they move to CONTRIBUTING.md) and the long per-auth walls (now in `docs/authentication.html`). Ensure no `v0.2.0` and the `brew` command matches the landing page.

- [ ] **Step 4: Create CONTRIBUTING.md**

Create `CONTRIBUTING.md` with the moved contributor content: the codebase `Layout` map (with a 2-line annotation for `internal/broker/`, `internal/gateway/`, `image/`), `Build/test/CI` (incl. `make docs`), and the "Known gaps"/roadmap pointer.

- [ ] **Step 5: Run the tests (green)**

Run: `go test ./cmd/docs-build/`
Expected: PASS (render + guard tests).

- [ ] **Step 6: Full verification + commit**

Run: `make docs && go build ./... && go vet ./... && go test ./... && (gofmt -l cmd/ | grep . && echo FMT || echo fmt-clean)`
Expected: all green; `fmt-clean`.
```bash
git add README.md CONTRIBUTING.md cmd/docs-build/site_test.go site/docs
git commit -m "docs: slim README to overview + CONTRIBUTING.md; version + internal-link guards"
```

---

## Self-Review

**Spec coverage:**
- Landing funnel (hero→containment→how→arch→threat→redteam→works/limits/install) → Task 3 Step 1.
- Docs site pages (quickstart/auth/submitting/egress/config/troubleshooting/threat-model) → Task 4.
- README slim + CONTRIBUTING.md → Task 5 Steps 3–4.
- Visual system B + A pills + C diagram/table; dark mode + reduced-motion + focus → Task 2.
- Go-native markdown→HTML (goldmark, cmd/docs-build, make docs, pages.yml) → Task 1.
- Deterministic build → Task 1 Step 10 + `TestRenderMarkdown_Deterministic`.
- Version v0.3.0 everywhere + guard → Task 5 (`TestNoStaleVersion`).
- Internal-link/asset check → Task 5 (`TestLandingInternalLinksResolve`).
- redteam promotion → Task 3 Step 1 (§7); egress widening → Task 4 (`egress.md`); audience block → Task 5 Step 3; de-duplicated auth → Task 4 (`authentication.md`).

**Placeholder scan:** The Go renderer, guard tests, Makefile target, pages step, and template are complete code. The CSS (Task 2), landing HTML (Task 3), and docs prose (Task 4) are visual/content **artifacts**, specified by exact section order, required components/classes, content sources, and acceptance criteria, and verified by headless render + the automated link/version guards rather than unit tests — this is intentional and stated, not a deferred placeholder.

**Type consistency:** `renderMarkdown([]byte) (string, []Heading, error)`, `renderPage(tmpl, body, title, sidebar, base string) string`, `buildSidebar([]Page, string) string`, `Page{Slug,Title,File}`, `Heading{Level,Text,ID}` — consistent across Task 1 (render.go, main.go) and the tests. `make docs` = `go run ./cmd/docs-build` consistent across Tasks 1/2/4. Template slots `{{title}}/{{content}}/{{sidebar}}/{{base}}` consistent between render.go and `_template.html`.

**Verified against the codebase:** module `drydock`, go 1.26.4 (goldmark is the first real direct dep); Makefile uses `.PHONY` + Go-native `go run` targets (sbom); `pages.yml` already on `checkout@v5`/`setup-go@v6`/`configure-pages@v6`/`upload-pages-artifact@v5`/`deploy-pages@v5` and uploads `site/`.
