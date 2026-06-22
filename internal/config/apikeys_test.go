package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAPIKeys_ParsesAndIgnoresBlanksAndComments(t *testing.T) {
	p := filepath.Join(t.TempDir(), "api-keys.env")
	body := "# comment\n\nANTHROPIC_API_KEY=sk-ant-x\n  OPENAI_API_KEY = sk-o  \n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := LoadAPIKeys(p)
	if got["ANTHROPIC_API_KEY"] != "sk-ant-x" || got["OPENAI_API_KEY"] != "sk-o" {
		t.Fatalf("LoadAPIKeys = %v", got)
	}
}

func TestLoadAPIKeys_MissingFileIsEmpty(t *testing.T) {
	if got := LoadAPIKeys(filepath.Join(t.TempDir(), "nope.env")); len(got) != 0 {
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
	got := LoadAPIKeys(p)
	if got["ANTHROPIC_API_KEY"] != "sk-ant-2" || got["OPENAI_API_KEY"] != "sk-o-1" {
		t.Fatalf("upsert lost a key: %v", got)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}
