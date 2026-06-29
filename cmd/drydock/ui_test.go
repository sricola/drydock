package main

import (
	"regexp"
	"strings"
	"testing"
)

func TestMintToken(t *testing.T) {
	tok, err := mintToken()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(tok) {
		t.Fatalf("token %q is not 32 bytes hex", tok)
	}
	tok2, _ := mintToken()
	if tok == tok2 {
		t.Fatal("tokens must be random")
	}
}

func TestUIURL(t *testing.T) {
	// Token goes in the fragment, never the query.
	if got := uiURL(7878, "abc"); got != "http://127.0.0.1:7878/#t=abc" {
		t.Fatalf("uiURL = %q", got)
	}
	// --no-token: no fragment.
	if got := uiURL(7878, ""); got != "http://127.0.0.1:7878/" || strings.Contains(got, "#") {
		t.Fatalf("no-token uiURL = %q", got)
	}
}
