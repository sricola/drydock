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

// renderPage substitutes the template slots. base is the relative path back to
// site root (so docs pages link assets as {{base}}style.css).
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
