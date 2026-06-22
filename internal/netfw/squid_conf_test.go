package netfw

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompileSquidConf_AuthAndInclude(t *testing.T) {
	conf := CompileSquidConf("192.168.66.1:3128", "/run/squid-allow.txt", "/run",
		"/usr/local/bin/brokerd __squid-authhelper /run/task-tokens")

	wantSubstrings := []string{
		"http_port 192.168.66.1:3128",
		`auth_param basic program /usr/local/bin/brokerd __squid-authhelper /run/task-tokens`,
		"acl default_dst dstdomain \"/run/squid-allow.txt\"",
		"http_access allow CONNECT default_dst SSL_ports",
		"http_access allow default_dst",
		"include /run/task-acls/*.conf",
		"http_access deny all",
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

func TestResetTaskState_ClearsStale(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "task-acls"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "task-acls", "task-x.conf"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "task-tokens"), []byte("task-x s\n"), 0o600)

	if err := ResetTaskState(dir); err != nil {
		t.Fatalf("ResetTaskState: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "task-acls")); !os.IsNotExist(err) {
		t.Errorf("task-acls not cleared")
	}
	if _, err := os.Stat(filepath.Join(dir, "task-tokens")); !os.IsNotExist(err) {
		t.Errorf("task-tokens not cleared")
	}
}
