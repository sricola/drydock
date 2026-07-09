package main

import (
	"strings"
	"testing"
)

func TestPageTitle(t *testing.T) {
	// index must not double up as "drydock docs · drydock docs".
	if got := pageTitle("index", "drydock docs"); got != "drydock docs" {
		t.Errorf("index pageTitle = %q, want %q", got, "drydock docs")
	}
	if got := pageTitle("quickstart", "Quickstart"); got != "Quickstart · drydock docs" {
		t.Errorf("pageTitle = %q, want %q", got, "Quickstart · drydock docs")
	}
}

func TestCanonicalURL(t *testing.T) {
	want := "https://sricola.github.io/drydock/docs/quickstart.html"
	if got := canonicalURL("quickstart"); got != want {
		t.Errorf("canonicalURL = %q, want %q", got, want)
	}
}

func TestMetaDescription(t *testing.T) {
	src := []byte("# Egress\n\n" +
		"Egress is **deny-by-default**: the agent reaches only the hosts in your " +
		"[allowlist](configuration.html), through a host-side proxy. `widening` is per-task.\n\n" +
		"## Details\n\nmore text here")
	got := metaDescription(src)
	if strings.Contains(got, "#") || strings.Contains(got, "**") ||
		strings.Contains(got, "[") || strings.Contains(got, "`") {
		t.Errorf("description still has markdown syntax: %q", got)
	}
	if !strings.HasPrefix(got, "Egress is deny-by-default") {
		t.Errorf("description = %q, want it to start with the first paragraph", got)
	}
	if strings.Contains(got, "more text here") {
		t.Errorf("description leaked past the first paragraph: %q", got)
	}
}

func TestMetaDescription_TruncatesAtWordBoundary(t *testing.T) {
	long := "# T\n\n" + strings.Repeat("word ", 80) // 400 chars
	got := metaDescription([]byte(long))
	if len([]rune(got)) > 160 {
		t.Errorf("description too long: %d runes", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated description should end with an ellipsis: %q", got)
	}
	if strings.HasSuffix(strings.TrimSuffix(got, "…"), " ") == false && !strings.Contains(got, "word") {
		t.Errorf("expected word-boundary truncation: %q", got)
	}
}

func TestSitemapXML(t *testing.T) {
	xml := sitemapXML([]string{"index", "quickstart"})
	for _, want := range []string{
		`<?xml version="1.0"`,
		"<loc>https://sricola.github.io/drydock/</loc>",
		"<loc>https://sricola.github.io/drydock/docs/quickstart.html</loc>",
		"</urlset>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("sitemap missing %q\n%s", want, xml)
		}
	}
}
