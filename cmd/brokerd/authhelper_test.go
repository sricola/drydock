package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseHelperLine(t *testing.T) {
	cases := []struct {
		line, user, pass string
	}{
		{"alice s3cret", "alice", "s3cret"},             // plain
		{"  alice   s3cret  ", "alice", "s3cret"},       // extra whitespace trimmed/split
		{"a%20b p%2Fw", "a b", "p/w"},                   // URL-escaped space and slash round-trip
		{"user%40host tok%3Aen", "user@host", "tok:en"}, // escaped @ and :
		{"alice", "", ""},                               // fewer than two fields → empty (deny)
		{"", "", ""},                                    // empty line → empty
	}
	for _, c := range cases {
		u, p := parseHelperLine(c.line)
		if u != c.user || p != c.pass {
			t.Errorf("parseHelperLine(%q) = (%q, %q), want (%q, %q)", c.line, u, p, c.user, c.pass)
		}
	}
}

func TestRunSquidAuthHelper_OKAndERR(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "tokens")
	if err := os.WriteFile(tok, []byte("task-a alpha\ntask-b bravo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader("task-a alpha\ntask-a wrong\ntask-x alpha\ntask-b bravo\n")
	var out strings.Builder
	if err := runSquidAuthHelper(tok, in, &out); err != nil {
		t.Fatalf("helper returned error: %v", err)
	}
	got := out.String()
	want := "OK\nERR\nERR\nOK\n"
	if got != want {
		t.Errorf("responses = %q, want %q", got, want)
	}
}

func TestRunSquidAuthHelper_MissingTokenFileIsAllERR(t *testing.T) {
	in := strings.NewReader("task-a alpha\n")
	var out strings.Builder
	if err := runSquidAuthHelper(filepath.Join(t.TempDir(), "nope"), in, &out); err != nil {
		t.Fatalf("helper returned error: %v", err)
	}
	if out.String() != "ERR\n" {
		t.Errorf("missing token file should ERR, got %q", out.String())
	}
}
