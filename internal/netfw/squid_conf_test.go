package netfw

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompileSquidConf_AuthAndInclude(t *testing.T) {
	conf := CompileSquidConf("192.168.66.1:3128", "/run/squid-default-acl.conf", "/run",
		"/usr/local/bin/brokerd __squid-authhelper /run/task-tokens")

	wantSubstrings := []string{
		"http_port 192.168.66.1:3128",
		`auth_param basic program /usr/local/bin/brokerd __squid-authhelper /run/task-tokens`,
		"acl SSL_ports port 443",
		"http_access deny CONNECT !SSL_ports",
		"include /run/squid-default-acl.conf",
		"include /run/task-acls/*.conf",
		"http_access deny all",
		"access_log /run/access.log squid", // must write logs into runDir, never "none"
		"logfile_rotate 10",                // bound access/cache log growth on long-running brokers
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(conf, s) {
			t.Errorf("conf missing %q\n---\n%s", s, conf)
		}
	}
	// The include line MUST come before the final deny-all (per-task allows
	// are evaluated before the catch-all deny).
	if strings.Index(conf, "include /run/task-acls/*.conf") > strings.LastIndex(conf, "http_access deny all") {
		t.Errorf("include must precede final deny all")
	}
}

func TestResetTaskState_ClearsStaleButKeepsPlaceholder(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "task-acls"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "task-acls", "task-x.conf"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "task-acls", "task-x.domains"), []byte("evil.com\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "task-tokens"), []byte("task-x s\n"), 0o600)

	if err := ResetTaskState(dir); err != nil {
		t.Fatalf("ResetTaskState: %v", err)
	}
	// Stale per-task fragments and the token file are gone.
	if _, err := os.Stat(filepath.Join(dir, "task-acls", "task-x.conf")); !os.IsNotExist(err) {
		t.Errorf("stale fragment task-x.conf not cleared")
	}
	if _, err := os.Stat(filepath.Join(dir, "task-tokens")); !os.IsNotExist(err) {
		t.Errorf("task-tokens not cleared")
	}
	// But task-acls/ exists with the placeholder so squid's glob include resolves
	// with zero active tasks (squid FATALs on a zero-match include).
	entries, err := os.ReadDir(filepath.Join(dir, "task-acls"))
	if err != nil {
		t.Fatalf("task-acls dir should exist after reset: %v", err)
	}
	var confs int
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".conf" {
			confs++
		}
	}
	if confs == 0 {
		t.Errorf("task-acls must keep a .conf placeholder so the squid include matches a file; got %v", entries)
	}
}

// TestStartSquid_RejectsWhitespaceRunDir guards against a runDir with a space:
// squid.conf's unquoted include/pid/log paths would FATAL. The check fires
// before any squid exec, so no squid binary is needed.
func TestStartSquid_RejectsWhitespaceRunDir(t *testing.T) {
	_, err := StartSquid("/bin/false", "127.0.0.1:1", "", "/tmp/has space/run", "helper __squid-authhelper /tmp/tok")
	if err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Fatalf("expected whitespace error, got %v", err)
	}
}

// TestResetTaskState_CleanDirIsIdempotent covers the nothing-to-remove path.
func TestResetTaskState_CleanDirIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := ResetTaskState(dir); err != nil {
		t.Fatalf("ResetTaskState on clean dir: %v", err)
	}
	if err := ResetTaskState(dir); err != nil {
		t.Fatalf("second ResetTaskState: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "task-acls", taskACLPlaceholder)); err != nil {
		t.Errorf("placeholder missing after reset: %v", err)
	}
}
