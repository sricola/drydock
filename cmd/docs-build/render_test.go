package main

import (
	"strings"
	"testing"
)

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

func TestBuildSidebar_MarksCurrent(t *testing.T) {
	pages := []Page{{Slug: "quickstart", Title: "Quickstart", File: "quickstart.html"}, {Slug: "egress", Title: "Egress", File: "egress.html"}}
	out := buildSidebar(pages, "egress")
	if !strings.Contains(out, `<li><a class="on" href="egress.html">Egress</a>`) {
		t.Errorf("current page not marked: %s", out)
	}
	if !strings.Contains(out, `<li><a href="quickstart.html">Quickstart</a>`) {
		t.Errorf("non-current page should have no class: %s", out)
	}
}
