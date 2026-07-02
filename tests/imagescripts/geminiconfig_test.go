package imagescripts

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Verifies write-gemini-config.sh emits a settings.json that pins API-key auth
// and disables phone-home, so the CLI runs headless inside deny-by-default egress.
func TestWriteGeminiConfig(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("jq required in CI")
		}
		t.Skip("jq not installed")
	}
	dir := t.TempDir()
	cmd := exec.Command("bash", "../../image/write-gemini-config.sh", filepath.Join(dir, ".gemini"))
	cmd.Env = append(os.Environ(), "GEMINI_API_KEY=tok_test")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".gemini", "settings.json"))
	if err != nil {
		t.Fatalf("settings.json not written: %v", err)
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("settings.json not valid JSON: %v\n%s", err, raw)
	}
	sec, _ := s["security"].(map[string]any)
	auth, _ := sec["auth"].(map[string]any)
	if auth["selectedType"] != "gemini-api-key" {
		t.Errorf("security.auth.selectedType = %v, want gemini-api-key", auth["selectedType"])
	}
}
