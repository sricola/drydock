package netfw

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/egress"
)

func TestSquidController_Rotate(t *testing.T) {
	dir := t.TempDir()
	var gotBin, gotConf string
	var calls int
	orig := squidRotate
	t.Cleanup(func() { squidRotate = orig })
	squidRotate = func(bin, conf string) error { calls++; gotBin, gotConf = bin, conf; return nil }

	confPath := filepath.Join(dir, "squid.conf")
	c := NewSquidController("/bin/squid", confPath, dir)
	if err := c.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if calls != 1 || gotBin != "/bin/squid" || gotConf != confPath {
		t.Errorf("Rotate invoked squid rotate wrong: calls=%d bin=%q conf=%q", calls, gotBin, gotConf)
	}
}

func TestSquidController_AddRemoveTask(t *testing.T) {
	dir := t.TempDir()
	var reconfigs int
	orig := squidReconfigure
	t.Cleanup(func() { squidReconfigure = orig })
	squidReconfigure = func(_, _ string) error { reconfigs++; return nil }

	c := NewSquidController("/bin/squid", filepath.Join(dir, "squid.conf"), dir)

	if err := c.AddTask("task-abc", "sekret", []egress.Domain{
		{Host: "api.github.com", Ports: []int{443}},
		{Host: "pypi.org", Ports: []int{443, 8080}},
	}); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	// Token line present.
	tok, _ := os.ReadFile(filepath.Join(dir, "task-tokens"))
	if !strings.Contains(string(tok), "task-abc sekret") {
		t.Errorf("token file missing entry: %q", tok)
	}
	// Fragment: per-domain dstdomain+port ACL pairs, each allow rule requiring
	// the port ACL, with the slow proxy_auth (u_) ACL LAST so a non-matching
	// host/port short-circuits before any 407.
	frag, _ := os.ReadFile(filepath.Join(dir, "task-acls", "task-abc.conf"))
	fs := string(frag)
	for _, want := range []string{
		`acl u_task-abc proxy_auth task-abc`,
		`acl d_task-abc_0 dstdomain api.github.com`,
		`acl p_task-abc_0 port 443`,
		`http_access allow CONNECT SSL_ports d_task-abc_0 p_task-abc_0 u_task-abc`,
		`http_access allow d_task-abc_0 p_task-abc_0 u_task-abc`,
		`acl d_task-abc_1 dstdomain pypi.org`,
		`acl p_task-abc_1 port 443 8080`,
		`http_access allow CONNECT SSL_ports d_task-abc_1 p_task-abc_1 u_task-abc`,
		`http_access allow d_task-abc_1 p_task-abc_1 u_task-abc`,
	} {
		if !strings.Contains(fs, want) {
			t.Errorf("fragment missing %q\n%s", want, fs)
		}
	}
	// No unrestricted (port-less) allow rule may survive for a widened domain.
	if bareAllowRE.MatchString(fs) {
		t.Errorf("widened fragment emitted an unrestricted allow rule:\n%s", fs)
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
	_ = c.AddTask("task-1", "s1", []egress.Domain{{Host: "a.com", Ports: []int{443}}})
	_ = c.AddTask("task-2", "s2", []egress.Domain{{Host: "b.com", Ports: []int{443}}})
	_ = c.RemoveTask("task-1")

	tok, _ := os.ReadFile(filepath.Join(dir, "task-tokens"))
	if strings.Contains(string(tok), "task-1") || !strings.Contains(string(tok), "task-2 s2") {
		t.Errorf("token file after remove = %q", tok)
	}
}

// TestSquidController_RemoveMissingIsIdempotent confirms RemoveTask does not
// error when the per-task artifacts are absent (the !os.IsNotExist guard) — a
// double-remove or a remove with no prior AddTask is a clean no-op.
func TestSquidController_RemoveMissingIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	orig := squidReconfigure
	t.Cleanup(func() { squidReconfigure = orig })
	squidReconfigure = func(_, _ string) error { return nil }

	c := NewSquidController("/bin/squid", filepath.Join(dir, "squid.conf"), dir)
	if err := c.RemoveTask("never-added"); err != nil {
		t.Errorf("RemoveTask on a missing user should be a no-op, got %v", err)
	}
	_ = c.AddTask("task-1", "s1", []egress.Domain{{Host: "a.com", Ports: []int{443}}})
	if err := c.RemoveTask("task-1"); err != nil {
		t.Fatalf("first RemoveTask: %v", err)
	}
	if err := c.RemoveTask("task-1"); err != nil {
		t.Errorf("second RemoveTask (already gone) should be a no-op, got %v", err)
	}
}
