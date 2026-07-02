package imagescripts

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const opencodeScriptRel = "../../image/write-opencode-config.sh"

// runWriteOpencodeConfig runs the script with OPENAI_API_KEY set and returns the
// parsed opencode.json. Skips if jq isn't on the host (the script uses jq, which
// the sandbox image ships; CI runners generally have it).
func runWriteOpencodeConfig(t *testing.T, gwBase, model, key string) map[string]any {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("jq must be installed in CI; write-opencode-config.sh requires it")
		}
		t.Skip("jq not installed; write-opencode-config.sh needs it (skip in local dev)")
	}
	home := t.TempDir()
	cmd := exec.Command("bash", opencodeScriptRel, gwBase, model, home)
	cmd.Env = append(os.Environ(), "OPENAI_API_KEY="+key)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write-opencode-config.sh failed: %v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(home, "opencode", "opencode.json"))
	if err != nil {
		t.Fatalf("read opencode.json: %v", err)
	}
	if !json.Valid(b) {
		t.Fatalf("opencode.json is not valid JSON:\n%s", b)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

// dig walks nested map keys, failing the test on any miss.
func dig(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()
	var cur any = m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: %q is not an object", path, k)
		}
		cur, ok = mm[k]
		if !ok {
			t.Fatalf("path %v: key %q missing", path, k)
		}
	}
	return cur
}

// The generated config must define a custom openai-compatible provider pointed
// at the gateway (baseURL + /v1), authed by the per-task bearer (OPENAI_API_KEY)
// as apiKey, with the task model declared. This is what routes opencode through
// the gateway instead of calling the upstream directly.
func TestWriteOpencodeConfig_RoutesThroughGateway(t *testing.T) {
	m := runWriteOpencodeConfig(t, "http://192.168.66.1:8088", "gemini-2.5-pro", "tok_LEASE123")
	opts := dig(t, m, "provider", "drydock", "options").(map[string]any)
	if opts["baseURL"] != "http://192.168.66.1:8088/v1" {
		t.Errorf("baseURL = %v", opts["baseURL"])
	}
	if opts["apiKey"] != "tok_LEASE123" {
		t.Errorf("apiKey = %v (the per-task bearer must reach opencode)", opts["apiKey"])
	}
	if npm := dig(t, m, "provider", "drydock", "npm"); npm != "@ai-sdk/openai-compatible" {
		t.Errorf("npm provider = %v, want @ai-sdk/openai-compatible", npm)
	}
	// The task model must be a declared model key so `-m drydock/<model>` resolves.
	dig(t, m, "provider", "drydock", "models", "gemini-2.5-pro")
}

// The grant injects OPENAI_BASE_URL without a trailing slash, but a trailing
// slash must not produce a double slash before /v1.
func TestWriteOpencodeConfig_TrimsTrailingSlash(t *testing.T) {
	m := runWriteOpencodeConfig(t, "http://gw:8088/", "m", "tok_x")
	opts := dig(t, m, "provider", "drydock", "options").(map[string]any)
	if opts["baseURL"] != "http://gw:8088/v1" {
		t.Errorf("trailing slash not trimmed: baseURL = %v", opts["baseURL"])
	}
}

// Missing positional args or OPENAI_API_KEY must fail closed (set -u + :?
// guards), not write a config that points opencode nowhere or omits the key.
func TestWriteOpencodeConfig_RequiresArgsAndKey(t *testing.T) {
	// No args at all.
	if err := exec.Command("bash", opencodeScriptRel).Run(); err == nil {
		t.Error("expected non-zero exit with no args")
	}
	// Args present but OPENAI_API_KEY unset.
	cmd := exec.Command("bash", opencodeScriptRel, "http://gw:8088", "m", t.TempDir())
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")} // no OPENAI_API_KEY
	if err := cmd.Run(); err == nil {
		t.Error("expected non-zero exit when OPENAI_API_KEY is unset")
	}
}
