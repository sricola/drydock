package main

import (
	"os"
	"path/filepath"
	"testing"
)

// withPipedStdin replaces os.Stdin with a pipe carrying content (not a char
// device, so readInstruction treats it as piped input) and restores it after.
func withPipedStdin(t *testing.T, content string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = w.WriteString(content); w.Close() }()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; r.Close() })
}

// readInstruction resolves instruction text by precedence: file, then inline,
// then stdin ("-" or piped), else an error. These lock that precedence.

func TestReadInstruction_FileWinsAndTrims(t *testing.T) {
	p := filepath.Join(t.TempDir(), "instr.txt")
	if err := os.WriteFile(p, []byte("from file\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// inline is also set: file must still win.
	got, err := readInstruction("inline value", p, nil)
	if err != nil {
		t.Fatalf("readInstruction: %v", err)
	}
	if got != "from file" {
		t.Errorf("got %q, want %q (file wins, trailing newlines trimmed)", got, "from file")
	}
}

func TestReadInstruction_FileReadErrorSurfaces(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.txt")
	if _, err := readInstruction("", missing, nil); err == nil {
		t.Error("readInstruction with an unreadable file returned nil error")
	}
}

func TestReadInstruction_InlineReturnedVerbatim(t *testing.T) {
	got, err := readInstruction("just do it", "", nil)
	if err != nil {
		t.Fatalf("readInstruction: %v", err)
	}
	if got != "just do it" {
		t.Errorf("got %q, want the inline value", got)
	}
}

func TestReadInstruction_DashInlineReadsStdin(t *testing.T) {
	withPipedStdin(t, "piped instruction\n")
	got, err := readInstruction("-", "", nil)
	if err != nil {
		t.Fatalf("readInstruction: %v", err)
	}
	if got != "piped instruction" {
		t.Errorf("got %q, want the piped stdin (trailing newline trimmed)", got)
	}
}

func TestReadInstruction_DashPositionalReadsStdin(t *testing.T) {
	withPipedStdin(t, "positional dash\n")
	got, err := readInstruction("", "", []string{"-"})
	if err != nil {
		t.Fatalf("readInstruction: %v", err)
	}
	if got != "positional dash" {
		t.Errorf("got %q, want the piped stdin", got)
	}
}

func TestReadInstruction_NoSourcesReadsPipedStdin(t *testing.T) {
	withPipedStdin(t, "implicit pipe\n")
	got, err := readInstruction("", "", nil)
	if err != nil {
		t.Fatalf("readInstruction: %v", err)
	}
	if got != "implicit pipe" {
		t.Errorf("got %q, want the implicitly piped stdin", got)
	}
}
