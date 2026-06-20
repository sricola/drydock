package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxImageNudge(t *testing.T) {
	const built = "drydock-sandbox:latest"
	cases := []struct {
		name       string
		configured string
		wantWarn   bool
		wantSub    string // substring the message must contain when warning
	}{
		{"matches built default", built, false, ""},
		{"empty falls back to default", "", false, ""},
		{"stale claude-sandbox name", "claude-sandbox:latest", true, "claude-sandbox:latest"},
		{"intentional custom image", "my-sandbox:dev", true, "my-sandbox:dev"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			warn, msg := sandboxImageNudge(c.configured, built)
			if warn != c.wantWarn {
				t.Fatalf("warn=%v, want %v", warn, c.wantWarn)
			}
			if warn {
				if !strings.Contains(msg, c.wantSub) {
					t.Errorf("msg %q missing %q", msg, c.wantSub)
				}
				if !strings.Contains(msg, built) {
					t.Errorf("msg %q should name the built image %q as the fix", msg, built)
				}
			}
		})
	}
}

func TestFindImageDir_RespectsEnvOverride(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "image")
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imageDir, "Dockerfile"), []byte("FROM scratch"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DRYDOCK_IMAGE_DIR", root)
	got, err := findImageDir("image")
	if err != nil {
		t.Fatalf("findImageDir: %v", err)
	}
	if got != imageDir {
		t.Errorf("got %q, want %q", got, imageDir)
	}
}

func TestFindImageDir_NotFoundReportsCandidates(t *testing.T) {
	// Use a unique path that no candidate could match.
	t.Setenv("DRYDOCK_IMAGE_DIR", "/this/path/does/not/exist")
	t.Setenv("HOMEBREW_PREFIX", "/another/missing/path")
	// Move cwd to a place where ./image won't exist.
	dir := t.TempDir()
	prev, _ := os.Getwd()
	defer os.Chdir(prev)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	_, err := findImageDir("image-no-such-thing")
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "searched:") {
		t.Errorf("error should list candidates: %v", err)
	}
}

// captureStdout runs f and returns whatever it wrote to stdout. Needed
// because nudgeEgressRecommendations prints directly with fmt.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	f()
	w.Close()
	os.Stdout = orig
	return <-done
}

func TestNudgeEgressRecommendations_PrintsOnlyMissingHosts(t *testing.T) {
	// Existing operator config that has anthropic+pypi but NOT the new
	// Go proxies. The nudge should call out exactly the missing ones.
	path := filepath.Join(t.TempDir(), "egress.yaml")
	if err := os.WriteFile(path, []byte(`version: 1
default:
  domains:
    - { host: api.anthropic.com, ports: [443] }
    - { host: pypi.org,          ports: [443] }
`), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { nudgeEgressRecommendations(path) })

	for _, want := range recommendedEgressHosts {
		if !strings.Contains(out, want) {
			t.Errorf("nudge missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "missing recommended entries") {
		t.Errorf("nudge should explain itself:\n%s", out)
	}
}

func TestNudgeEgressRecommendations_SilentWhenAllPresent(t *testing.T) {
	// All recommended hosts already in the config -> no output.
	path := filepath.Join(t.TempDir(), "egress.yaml")
	body := "version: 1\ndefault:\n  domains:\n"
	for _, h := range recommendedEgressHosts {
		body += "    - { host: " + h + ", ports: [443] }\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { nudgeEgressRecommendations(path) })
	if out != "" {
		t.Errorf("expected silence when nothing missing; got:\n%s", out)
	}
}

// The shipped fallback (used when the share-dir template is unreachable)
// must contain every recommended host, otherwise a fresh install would
// land in the same upgrade hole the nudge exists to address.
func TestDefaultEgressYAML_ContainsRecommended(t *testing.T) {
	for _, h := range recommendedEgressHosts {
		if !strings.Contains(defaultEgressYAML, h) {
			t.Errorf("defaultEgressYAML missing %q — fresh installs without share-dir template would lack it", h)
		}
	}
}

// findShareFile must resolve both templates from a freshly-laid share dir.
// Catches the failure mode where the share-dir lookup pattern changes for
// one file but not the other — the exact asymmetry this scaffolding fix
// exists to address.
func TestFindShareFile_ResolvesBothTemplates(t *testing.T) {
	root := t.TempDir()
	shareDir := filepath.Join(root, "share", "drydock", "config")
	if err := os.MkdirAll(shareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.yaml", "egress.yaml"} {
		if err := os.WriteFile(filepath.Join(shareDir, name), []byte("placeholder"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("HOMEBREW_PREFIX", root)

	got, err := findShareConfigTemplate()
	if err != nil {
		t.Errorf("findShareConfigTemplate: %v", err)
	}
	if !strings.HasSuffix(got, "config/config.yaml") {
		t.Errorf("config.yaml = %q, want suffix config/config.yaml", got)
	}
	got, err = findShareEgressTemplate()
	if err != nil {
		t.Errorf("findShareEgressTemplate: %v", err)
	}
	if !strings.HasSuffix(got, "config/egress.yaml") {
		t.Errorf("egress.yaml = %q, want suffix config/egress.yaml", got)
	}
}
