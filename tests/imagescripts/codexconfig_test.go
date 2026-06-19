// Package imagescripts tests the shell helpers shipped in the sandbox image
// without needing to build or run the image. They run on any host with bash
// (including CI), so the Codex gateway-routing config is regression-covered.
package imagescripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const scriptRel = "../../image/write-codex-config.sh"

func runWriteConfig(t *testing.T, gwBase string) string {
	t.Helper()
	home := t.TempDir()
	out, err := exec.Command("bash", scriptRel, gwBase, home).CombinedOutput()
	if err != nil {
		t.Fatalf("write-codex-config.sh failed: %v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	return string(b)
}

// The generated config must point codex's provider at the gateway (base_url +
// /v1), authed by the per-task bearer token in OPENAI_API_KEY, using the
// responses wire API. This is exactly what makes codex route through the
// gateway instead of calling api.openai.com directly.
func TestWriteCodexConfig_RoutesThroughGateway(t *testing.T) {
	cfg := runWriteConfig(t, "http://192.168.66.1:8088")
	for _, want := range []string{
		`model_provider = "drydock"`,
		`[model_providers.drydock]`,
		`base_url = "http://192.168.66.1:8088/v1"`,
		`env_key = "OPENAI_API_KEY"`,
		`wire_api = "responses"`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config.toml missing %q\n--- got ---\n%s", want, cfg)
		}
	}
}

// The grant injects OPENAI_BASE_URL without a trailing slash, but be robust:
// a trailing slash must not produce a double slash before /v1.
func TestWriteCodexConfig_TrimsTrailingSlash(t *testing.T) {
	cfg := runWriteConfig(t, "http://gw:8088/")
	if !strings.Contains(cfg, `base_url = "http://gw:8088/v1"`) {
		t.Errorf("trailing slash not trimmed; got:\n%s", cfg)
	}
	if strings.Contains(cfg, "8088//v1") {
		t.Errorf("double slash in base_url:\n%s", cfg)
	}
}

// Missing args must fail closed (set -u + :? guards), not write a broken
// config that silently points codex nowhere.
func TestWriteCodexConfig_RequiresArgs(t *testing.T) {
	if err := exec.Command("bash", scriptRel).Run(); err == nil {
		t.Error("expected non-zero exit with no args")
	}
}
