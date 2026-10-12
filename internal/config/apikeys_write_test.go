package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WriteAPIKey rewrites the whole store from what LoadAPIKeys returned. If the
// load stopped early on a scanner error, rewriting would silently drop every
// key after the bad line. The upsert must refuse instead and leave the file
// untouched.
func TestWriteAPIKey_RefusesToRewriteAfterLoadError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "api-keys.env")
	orig := strings.Repeat("a", bufio.MaxScanTokenSize+1) + "\nOPENAI_API_KEY=sk-openai\n"
	if err := os.WriteFile(p, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteAPIKey(p, "ANTHROPIC_API_KEY", "sk-ant"); err == nil {
		t.Fatal("WriteAPIKey succeeded after a partial load; want an error")
	}
	got, _ := os.ReadFile(p)
	if string(got) != orig {
		t.Errorf("file was rewritten after a partial load:\n%s", got)
	}
}
