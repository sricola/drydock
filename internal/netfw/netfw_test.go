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
	out := CompileSquidConf("192.168.66.1:3128", "/run/allow.txt", "/run", "/usr/bin/brokerd __squid-authhelper /run/task-tokens")
	for _, want := range []string{
		"http_port 192.168.66.1:3128",
		`acl default_dst dstdomain "/run/allow.txt"`,
		"http_access deny all",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("squid.conf missing %q:\n%s", want, out)
		}
	}
}

func TestGatewayIP(t *testing.T) {
	cases := []struct {
		cidr    string
		want    string
		wantErr bool
	}{
		{"192.168.64.0/24", "192.168.64.1", false},
		{"192.168.66.0/24", "192.168.66.1", false},
		{"10.0.0.0/8", "10.0.0.1", false},
		// IPv6 must be rejected — drydock pins the .1 host address which only
		// has meaning for IPv4.
		{"fd2c:3221:beea:1b27::/64", "", true},
		{"not-a-cidr", "", true},
		{"", "", true},
	}
	for _, tc := range cases {
		got, err := GatewayIP(tc.cidr)
		if tc.wantErr {
			if err == nil {
				t.Errorf("GatewayIP(%q) want err, got %q", tc.cidr, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("GatewayIP(%q) = %q err=%v, want %q", tc.cidr, got, err, tc.want)
		}
	}
}

func TestCompileSquidAllowlist_EmptyConfig(t *testing.T) {
	var empty egress.Config
	if got := CompileSquidAllowlist(empty); got != "" {
		t.Errorf("empty config must compile to empty allowlist, got %q", got)
	}
}

func TestCompileSquidAllowlist_OneHostPerLineNoPorts(t *testing.T) {
	// Squid's dstdomain file is hostname-only; ports must be irrelevant to
	// the rendered shape regardless of what the YAML says.
	c := cfg()
	out := CompileSquidAllowlist(c)
	if strings.Count(out, "\n") != 2 {
		t.Errorf("want 2 hosts after model-API exclusion, got: %q", out)
	}
	if strings.Contains(out, "443") {
		t.Errorf("allowlist must not include ports: %q", out)
	}
}

func TestCompileSquidConf_DefaultDenyShape(t *testing.T) {
	out := CompileSquidConf("192.168.66.1:3128", "/tmp/allow", "/tmp/run", "/usr/bin/brokerd __squid-authhelper /tmp/run/task-tokens")
	// The order of the http_access lines is the actual access policy.
	// Reorder them and the proxy opens up — guard the order explicitly.
	wantOrder := []string{
		"http_access deny CONNECT !SSL_ports",
		"http_access allow CONNECT default_dst SSL_ports",
		"http_access allow default_dst",
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
	allowlist := "registry.npmjs.org\npypi.org\n"
	s, err := StartSquid(trueBin, "127.0.0.1:13128", allowlist, runDir, "/usr/bin/brokerd __squid-authhelper "+runDir+"/task-tokens")
	if err != nil {
		t.Fatalf("StartSquid: %v", err)
	}
	defer s.Stop()

	if got, err := os.ReadFile(runDir + "/squid-allow.txt"); err != nil {
		t.Fatalf("allowlist not written: %v", err)
	} else if string(got) != allowlist {
		t.Errorf("allowlist = %q, want %q", got, allowlist)
	}
	conf, err := os.ReadFile(runDir + "/squid.conf")
	if err != nil {
		t.Fatalf("conf not written: %v", err)
	}
	if !strings.Contains(string(conf), "127.0.0.1:13128") {
		t.Errorf("conf missing bind addr: %q", conf)
	}
	if !strings.Contains(string(conf), runDir+"/squid-allow.txt") {
		t.Errorf("conf doesn't reference the written allowlist path")
	}
}

func TestStartSquid_BadBinFails(t *testing.T) {
	_, err := StartSquid("/does/not/exist", "127.0.0.1:13128", "", t.TempDir(), "/usr/bin/brokerd __squid-authhelper /tmp/task-tokens")
	if err == nil {
		t.Error("want err for non-existent binary")
	}
}
