package netfw

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"drydock/internal/egress"
)

func cfg() egress.Config {
	var c egress.Config
	c.Default.Domains = []egress.Domain{
		{Host: "api.anthropic.com", Ports: []int{443}},
		{Host: "api.openai.com", Ports: []int{443}},
		{Host: "registry.npmjs.org", Ports: []int{443}},
		{Host: "pypi.org", Ports: []int{443}},
	}
	return c
}

func TestCompileSquidAllowlist_ExcludesModelAPI(t *testing.T) {
	out := CompileSquidAllowlist(cfg())
	if strings.Contains(out, "anthropic.com") {
		t.Errorf("api.anthropic.com must go via the gateway, not squid:\n%s", out)
	}
	if strings.Contains(out, "openai.com") {
		t.Errorf("api.openai.com must go via the gateway, not squid:\n%s", out)
	}
	if !strings.Contains(out, "registry.npmjs.org") {
		t.Errorf("registry.npmjs.org missing from squid allowlist:\n%s", out)
	}
	if !strings.Contains(out, "pypi.org") {
		t.Errorf("pypi.org missing from squid allowlist:\n%s", out)
	}
}

func TestCompileSquidConf(t *testing.T) {
	out := CompileSquidConf("192.168.66.1:3128", "/run/squid-default-acl.conf", "/run", "/usr/bin/brokerd __squid-authhelper /run/task-tokens")
	for _, want := range []string{
		"http_port 192.168.66.1:3128",
		"include /run/squid-default-acl.conf",
		"http_access deny all",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("squid.conf missing %q:\n%s", want, out)
		}
	}
}

func TestCompileSquidAllowlist_EmptyConfig(t *testing.T) {
	var empty egress.Config
	if got := CompileSquidAllowlist(empty); got != "" {
		t.Errorf("empty config must compile to empty allowlist, got %q", got)
	}
}

func TestCompileSquidAllowlist_OneBlockPerHost(t *testing.T) {
	// After the model-API exclusion two hosts remain; each renders a
	// dstdomain+port ACL pair, so we expect two dstdomain ACLs (dst0, dst1).
	c := cfg()
	out := CompileSquidAllowlist(c)
	if got := strings.Count(out, "dstdomain "); got != 2 {
		t.Errorf("want 2 dstdomain ACLs after model-API exclusion, got %d:\n%s", got, out)
	}
	// Ports ARE now part of the enforced shape (that's the whole point).
	if !strings.Contains(out, "port 443") {
		t.Errorf("allowlist must bind the configured port: %q", out)
	}
}

func TestCompileSquidConf_DefaultDenyShape(t *testing.T) {
	out := CompileSquidConf("192.168.66.1:3128", "/tmp/run/squid-default-acl.conf", "/tmp/run", "/usr/bin/brokerd __squid-authhelper /tmp/run/task-tokens")
	// The order of the http_access lines is the actual access policy.
	// Reorder them and the proxy opens up — guard the order explicitly. The
	// per-domain allow rules live in the included default-ACL file, which must
	// sit between the CONNECT guard and the final deny-all.
	wantOrder := []string{
		"http_access deny CONNECT !SSL_ports",
		"include /tmp/run/squid-default-acl.conf",
		"include /tmp/run/task-acls/*.conf",
		"http_access deny all",
	}
	prev := -1
	for _, line := range wantOrder {
		i := strings.Index(out, line)
		if i < 0 {
			t.Fatalf("missing rule %q\n%s", line, out)
		}
		if i <= prev {
			t.Errorf("rule %q is out of order; the access policy depends on this", line)
		}
		prev = i
	}
	for _, want := range []string{
		"http_port 192.168.66.1:3128",
		"cache_log /tmp/run/cache.log",
		"pid_filename /tmp/run/squid.pid",
		"forwarded_for delete",
		"via off",
		"cache deny all",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("squid.conf missing %q:\n%s", want, out)
		}
	}
}

func TestSquid_StopNilSafe(t *testing.T) {
	// Stop on a nil receiver, an uninitialized handle, and a handle with no
	// process must all be no-ops — brokerd's cleanup path is best-effort.
	var nilSquid *Squid
	if err := nilSquid.Stop(); err != nil {
		t.Errorf("nil Squid.Stop: %v", err)
	}
	if err := (&Squid{}).Stop(); err != nil {
		t.Errorf("zero Squid.Stop: %v", err)
	}
}

func TestStartSquid_WritesConfAndAllowlist(t *testing.T) {
	// `true` exits 0 immediately. We can't test "squid running"; we CAN
	// test that StartSquid wrote the conf and allowlist before exec.
	trueBin, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("no `true` on PATH: %v", err)
	}
	runDir := t.TempDir()
	allowlist := "acl dst0 dstdomain registry.npmjs.org\nacl prt0 port 443\n"
	s, err := StartSquid(trueBin, "127.0.0.1:13128", allowlist, runDir, "/usr/bin/brokerd __squid-authhelper "+runDir+"/task-tokens")
	if err != nil {
		t.Fatalf("StartSquid: %v", err)
	}
	defer s.Stop()

	if got, err := os.ReadFile(runDir + "/squid-default-acl.conf"); err != nil {
		t.Fatalf("default ACL not written: %v", err)
	} else if string(got) != allowlist {
		t.Errorf("default ACL = %q, want %q", got, allowlist)
	}
	conf, err := os.ReadFile(runDir + "/squid.conf")
	if err != nil {
		t.Fatalf("conf not written: %v", err)
	}
	if !strings.Contains(string(conf), "127.0.0.1:13128") {
		t.Errorf("conf missing bind addr: %q", conf)
	}
	if !strings.Contains(string(conf), "include "+runDir+"/squid-default-acl.conf") {
		t.Errorf("conf doesn't include the written default-ACL path")
	}
}

func TestStartSquid_BadBinFails(t *testing.T) {
	_, err := StartSquid("/does/not/exist", "127.0.0.1:13128", "", t.TempDir(), "/usr/bin/brokerd __squid-authhelper /tmp/task-tokens")
	if err == nil {
		t.Error("want err for non-existent binary")
	}
}
