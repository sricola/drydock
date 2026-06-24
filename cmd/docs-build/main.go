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
	sort.Slice(mds, func(i, j int) bool {
		ri, rj := rank(mds[i]), rank(mds[j])
		if ri != rj {
			return ri < rj
		}
		return mds[i] < mds[j]
	})

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
