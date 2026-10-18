package main

import (
	"regexp"
	"strings"
	"testing"
	"time"
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

// Pins the documented listener shape: loopback only (web-ui.md) and a
// 10s header timeout (CHANGELOG v0.8.2) with no body timeouts.
func TestUIServer_LoopbackBindAndHeaderTimeout(t *testing.T) {
	if got := uiAddr(7878); got != "127.0.0.1:7878" {
		t.Errorf("uiAddr = %q, want 127.0.0.1:7878", got)
	}
	hs := newUIServer(nil)
	if hs.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s", hs.ReadHeaderTimeout)
	}
	if hs.ReadTimeout != 0 || hs.WriteTimeout != 0 {
		t.Errorf("body timeouts set (%v/%v); /api/submit must be allowed to wait on brokerd", hs.ReadTimeout, hs.WriteTimeout)
	}
}
