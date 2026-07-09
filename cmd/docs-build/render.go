package main

import (
	"bytes"
	"html"
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

// pageData holds the per-page template substitutions.
type pageData struct {
	PageTitle   string // <title>/OG title
	Description string // meta description / OG description (plain text)
	Canonical   string // absolute canonical URL
	Content     string // rendered HTML body
	Sidebar     string // rendered nav
	Base        string // relative path back to site root
}

// renderPage substitutes the template slots. Text that lands in an attribute or
// <title> is HTML-escaped; Content is already trusted HTML and Base/Canonical
// are our own URLs.
func renderPage(tmpl string, d pageData) string {
	r := strings.NewReplacer(
		"{{pagetitle}}", html.EscapeString(d.PageTitle),
		"{{description}}", html.EscapeString(d.Description),
		"{{canonical}}", d.Canonical,
		"{{content}}", d.Content,
		"{{sidebar}}", d.Sidebar,
		"{{base}}", d.Base,
	)
	return r.Replace(tmpl)
}

// buildSidebar renders the docs nav, marking current.
func buildSidebar(pages []Page, current string) string {
	var b strings.Builder
	b.WriteString(`<ul class="docnav">`)
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
