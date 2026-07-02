package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAPIKeys_ParsesAndIgnoresBlanksAndComments(t *testing.T) {
	p := filepath.Join(t.TempDir(), "api-keys.env")
	body := "# comment\n\nANTHROPIC_API_KEY=sk-ant-x\n  OPENAI_API_KEY = sk-o  \n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAPIKeys(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["ANTHROPIC_API_KEY"] != "sk-ant-x" || got["OPENAI_API_KEY"] != "sk-o" {
		t.Fatalf("LoadAPIKeys = %v", got)
	}
}

// TestLoadAPIKeys_IgnoresUnknownKeys pins load/write symmetry: an unrecognized
// key in the file must be ignored on load, matching WriteAPIKey which only ever
// writes knownAPIKeys. Otherwise a stray line would be readable one moment and
// silently dropped the next time the store is rewritten.
func TestLoadAPIKeys_IgnoresUnknownKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "api-keys.env")
	body := "ANTHROPIC_API_KEY=sk-ant-x\nGITHUB_TOKEN=ghp_should_be_ignored\nRANDOM=nope\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAPIKeys(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["ANTHROPIC_API_KEY"] != "sk-ant-x" {
		t.Errorf("known key not loaded: %v", got)
	}
	if _, ok := got["GITHUB_TOKEN"]; ok {
		t.Errorf("unknown key GITHUB_TOKEN was loaded: %v", got)
	}
	if len(got) != 1 {
		t.Errorf("expected only the one known key, got %v", got)
	}
}

func TestLoadAPIKeys_MissingFileIsEmpty(t *testing.T) {
	got, err := LoadAPIKeys(filepath.Join(t.TempDir(), "nope.env"))
	if err != nil {
		t.Fatalf("missing file should return nil error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("missing file should yield empty map, got %v", got)
	}
}

func TestWriteAPIKey_UpsertsPreservesOtherAnd0600(t *testing.T) {
	p := filepath.Join(t.TempDir(), "api-keys.env")
	if err := WriteAPIKey(p, "ANTHROPIC_API_KEY", "sk-ant-1"); err != nil {
		t.Fatal(err)
	}
	if err := WriteAPIKey(p, "OPENAI_API_KEY", "sk-o-1"); err != nil {
		t.Fatal(err)
	}
	// Re-upsert the first; the second must survive.
	if err := WriteAPIKey(p, "ANTHROPIC_API_KEY", "sk-ant-2"); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAPIKeys(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["ANTHROPIC_API_KEY"] != "sk-ant-2" || got["OPENAI_API_KEY"] != "sk-o-1" {
		t.Fatalf("upsert lost a key: %v", got)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

// TestLoadAPIKeys_ScannerError verifies that a bufio.Scanner token-too-long
// error is returned rather than silently dropping lines that follow.
func TestLoadAPIKeys_ScannerError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "api-keys.env")
	// First line is a valid key; second line exceeds bufio.MaxScanTokenSize
	// so the scanner will fail mid-file.
	line := "ANTHROPIC_API_KEY=sk-ant-x\n" + strings.Repeat("a", bufio.MaxScanTokenSize+1) + "\n"
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAPIKeys(p)
	if err == nil {
		t.Error("expected scanner error on oversized line, got nil")
	}
	// The key scanned before the error must still be in the result.
	if got["ANTHROPIC_API_KEY"] != "sk-ant-x" {
		t.Errorf("key scanned before error missing; got %v", got)
	}
}
