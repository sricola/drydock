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
	// Phone-home must be off, at the exact 0.49.0 schema paths — a wrong key
	// name/nesting is silently ignored by the CLI, leaving egress to Google's
	// telemetry/update endpoints on (which breaks the sandboxed run).
	tel, _ := s["telemetry"].(map[string]any)
	if tel["enabled"] != false {
		t.Errorf("telemetry.enabled = %v, want false", tel["enabled"])
	}
	priv, _ := s["privacy"].(map[string]any)
	if priv["usageStatisticsEnabled"] != false {
		t.Errorf("privacy.usageStatisticsEnabled = %v, want false", priv["usageStatisticsEnabled"])
	}
	gen, _ := s["general"].(map[string]any)
	if gen["enableAutoUpdate"] != false || gen["enableAutoUpdateNotification"] != false {
		t.Errorf("general auto-update keys not both false: %v", gen)
	}
}
