package main

import (
	"regexp"
	"strings"
)

// siteBase is the deployed site root (GitHub Pages project site).
const siteBase = "https://sricola.github.io/drydock/"

// pageTitle is the <title>/OG title for a docs page. The index page's H1 is
// already "drydock docs", so suffixing it would double up — special-case it.
func pageTitle(slug, h1 string) string {
	if slug == "index" {
		return "drydock docs"
	}
	return h1 + " · drydock docs"
}

// canonicalURL is the absolute URL a docs page should declare as canonical.
func canonicalURL(slug string) string {
	return siteBase + "docs/" + slug + ".html"
}

var (
	mdImage    = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	mdLink     = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	mdEmphasis = regexp.MustCompile(`\*\*|__|` + "`" + `|\*|_|~~`)
	wsRun      = regexp.MustCompile(`\s+`)
)

// metaDescription derives a plain-text meta description from the first body
// paragraph of a docs Markdown file: skips the H1/headings/blockquote/table/
// fence markers, strips inline Markdown, collapses whitespace, and truncates at
// a word boundary. A real per-page summary beats a templated "documentation —
// {title}." for SEO and social previews.
func metaDescription(src []byte) string {
	var para []string
	started := false
	for _, ln := range strings.Split(string(src), "\n") {
		t := strings.TrimSpace(ln)
		if !started {
			if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ">") ||
				strings.HasPrefix(t, "|") || strings.HasPrefix(t, "```") ||
				strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") {
				continue // not prose — keep scanning for the first paragraph
			}
			started = true
		} else if t == "" {
			break // paragraph ends at the first blank line
		}
		para = append(para, t)
	}
	text := strings.Join(para, " ")
	text = mdImage.ReplaceAllString(text, "")
	text = mdLink.ReplaceAllString(text, "$1")
	text = mdEmphasis.ReplaceAllString(text, "")
	text = strings.TrimSpace(wsRun.ReplaceAllString(text, " "))
	return truncateWords(text, 155)
}

// truncateWords caps text at max runes, cutting at the last word boundary and
// appending an ellipsis.
func truncateWords(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	cut := string(r[:max])
	if i := strings.LastIndex(cut, " "); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:") + "…"
}

// sitemapXML builds a sitemap covering the landing page and every docs slug.
// Generated (not hand-maintained) so it can't fall behind the page set.
func sitemapXML(slugs []string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	b.WriteString("  <url><loc>" + siteBase + "</loc><changefreq>weekly</changefreq><priority>1.0</priority></url>\n")
	for _, s := range slugs {
		b.WriteString("  <url><loc>" + canonicalURL(s) + "</loc><changefreq>weekly</changefreq><priority>0.7</priority></url>\n")
	}
	b.WriteString("</urlset>\n")
	return b.String()
}
