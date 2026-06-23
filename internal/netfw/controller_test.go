package netfw

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSquidController_AddRemoveTask(t *testing.T) {
	dir := t.TempDir()
	var reconfigs int
	orig := squidReconfigure
	t.Cleanup(func() { squidReconfigure = orig })
	squidReconfigure = func(_, _ string) error { reconfigs++; return nil }

	c := NewSquidController("/bin/squid", filepath.Join(dir, "squid.conf"), dir)

	if err := c.AddTask("task-abc", "sekret", []string{"api.github.com", "pypi.org"}); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	// Token line present.
	tok, _ := os.ReadFile(filepath.Join(dir, "task-tokens"))
	if !strings.Contains(string(tok), "task-abc sekret") {
		t.Errorf("token file missing entry: %q", tok)
	}
	// Domains file: one host per line.
	dom, _ := os.ReadFile(filepath.Join(dir, "task-acls", "task-abc.domains"))
	if string(dom) != "api.github.com\npypi.org\n" {
		t.Errorf("domains = %q", dom)
	}
	// Fragment: fast ACLs (dstdomain) before slow proxy_auth in the rule.
	frag, _ := os.ReadFile(filepath.Join(dir, "task-acls", "task-abc.conf"))
	fs := string(frag)
	for _, want := range []string{
		`acl u_task-abc proxy_auth task-abc`,
		`acl d_task-abc dstdomain "` + filepath.Join(dir, "task-acls", "task-abc.domains") + `"`,
		`http_access allow CONNECT SSL_ports d_task-abc u_task-abc`,
		`http_access allow d_task-abc u_task-abc`,
	} {
		if !strings.Contains(fs, want) {
			t.Errorf("fragment missing %q\n%s", want, fs)
		}
	}
	// d_ (dstdomain) must appear before u_ (proxy_auth) in the CONNECT rule.
	rule := "http_access allow CONNECT SSL_ports d_task-abc u_task-abc"
	if !strings.Contains(fs, rule) {
		t.Errorf("CONNECT rule must order dstdomain before proxy_auth: %s", fs)
	}
	if reconfigs != 1 {
		t.Errorf("AddTask reconfigs = %d, want 1", reconfigs)
	}

	if err := c.RemoveTask("task-abc"); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "task-acls", "task-abc.conf")); !os.IsNotExist(err) {
		t.Errorf("fragment not removed")
	}
	tok2, _ := os.ReadFile(filepath.Join(dir, "task-tokens"))
	if strings.Contains(string(tok2), "task-abc") {
		t.Errorf("token line not removed: %q", tok2)
	}
	if reconfigs != 2 {
		t.Errorf("RemoveTask reconfigs = %d, want 2", reconfigs)
	}
}

func TestSquidController_RemovePreservesOtherTasks(t *testing.T) {
	dir := t.TempDir()
	orig := squidReconfigure
	t.Cleanup(func() { squidReconfigure = orig })
	squidReconfigure = func(_, _ string) error { return nil }

	c := NewSquidController("/bin/squid", filepath.Join(dir, "squid.conf"), dir)
	_ = c.AddTask("task-1", "s1", []string{"a.com"})
	_ = c.AddTask("task-2", "s2", []string{"b.com"})
	_ = c.RemoveTask("task-1")

	tok, _ := os.ReadFile(filepath.Join(dir, "task-tokens"))
	if strings.Contains(string(tok), "task-1") || !strings.Contains(string(tok), "task-2 s2") {
		t.Errorf("token file after remove = %q", tok)
	}
}
