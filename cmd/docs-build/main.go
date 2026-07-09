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
var order = []string{"index", "quickstart", "authentication", "models", "submitting-tasks", "web-ui", "daemon", "egress", "configuration", "troubleshooting", "threat-model"}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "docs-build:", err)
		os.Exit(1)
	}
}

// source is one input Markdown file and the docs slug it renders to.
type source struct {
	path string
	slug string
}

func run() error {
	tmpl, err := os.ReadFile(filepath.Join(docsDir, "_template.html"))
	if err != nil {
		return err
	}

	var srcs []source
	mds, _ := filepath.Glob(filepath.Join(docsDir, "*.md"))
	for _, f := range mds {
		srcs = append(srcs, source{path: f, slug: strings.TrimSuffix(filepath.Base(f), ".md")})
	}
	// The canonical threat model lives at the repo root — render it onto the site
	// too, single-sourced from THREAT_MODEL.md so it can't drift.
	srcs = append(srcs, source{path: "THREAT_MODEL.md", slug: "threat-model"})

	sort.Slice(srcs, func(i, j int) bool {
		ri, rj := rankSlug(srcs[i].slug), rankSlug(srcs[j].slug)
		if ri != rj {
			return ri < rj
		}
		return srcs[i].slug < srcs[j].slug
	})

	raw := map[string][]byte{}
	var pages []Page
	for _, s := range srcs {
		b, err := os.ReadFile(s.path)
		if err != nil {
			return fmt.Errorf("%s: %w", s.path, err)
		}
		if s.path == "THREAT_MODEL.md" {
			b = rewriteRepoLinks(b) // its relative .md links point at repo files, not site pages
		}
		raw[s.slug] = b
		title := s.slug
		if m := h1Re.FindSubmatch(b); m != nil {
			title = string(m[1])
		}
		pages = append(pages, Page{Slug: s.slug, Title: title, File: s.slug + ".html"})
	}
	// Build a slug→title map once so the render loop is O(n) not O(n²).
	titleFor := make(map[string]string, len(pages))
	for _, p := range pages {
		titleFor[p.Slug] = p.Title
	}
	for _, s := range srcs {
		body, _, err := renderMarkdown(raw[s.slug])
		if err != nil {
			return fmt.Errorf("%s: %w", s.path, err)
		}
		title := titleFor[s.slug]
		if title == "" {
			title = s.slug
		}
		page := renderPage(string(tmpl), body, title, buildSidebar(pages, s.slug), "../")
		out := filepath.Join(docsDir, s.slug+".html")
		if err := os.WriteFile(out, []byte(page), 0o644); err != nil {
			return err
		}
		fmt.Println("==>", out)
	}
	return nil
}

func rankSlug(slug string) int {
	for i, o := range order {
		if o == slug {
			return i
		}
	}
	return len(order) + 1
}

// rewriteRepoLinks turns relative Markdown links to repo files (e.g. the threat
// model's link to docs/ROADMAP.md) into absolute GitHub blob URLs, since the
// rendered page lives on the site, not in the repo tree.
var repoMdLink = regexp.MustCompile(`\]\((?:\./)?([A-Za-z0-9_./-]+\.md)(#[^)]*)?\)`)

func rewriteRepoLinks(b []byte) []byte {
	return repoMdLink.ReplaceAll(b, []byte("](https://github.com/sricola/drydock/blob/main/$1$2)"))
}
