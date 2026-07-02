package netfw

import (
	"regexp"
	"strings"
	"testing"

	"drydock/internal/egress"
)

// bareAllowRE matches an `http_access allow <acl>` whose single ACL is a
// dstdomain name (dst*/d_*) with NO port ACL following it — i.e. an
// unrestricted-port allow. The whole point of this task is that no such rule is
// ever emitted for a default or widened domain.
var bareAllowRE = regexp.MustCompile(`(?m)^http_access allow (dst\d+|d_[^ ]+)\s*$`)

func TestCompileSquidAllowlist_BindsSinglePort(t *testing.T) {
	var c egress.Config
	c.Default.Domains = []egress.Domain{
		{Host: "registry.npmjs.org", Ports: []int{443}},
	}
	out := CompileSquidAllowlist(c)

	for _, want := range []string{
		"acl dst0 dstdomain registry.npmjs.org",
		"acl prt0 port 443",
		"http_access allow CONNECT SSL_ports dst0 prt0",
		"http_access allow dst0 prt0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("allowlist ACL missing %q:\n%s", want, out)
		}
	}
	// Plain HTTP on :25 must NOT be reachable: the only port ACL lists 443.
	if strings.Contains(out, " 25") || strings.Contains(out, "port 25") {
		t.Errorf("port 25 must not appear in a :443-only domain block:\n%s", out)
	}
	if loc := bareAllowRE.FindString(out); loc != "" {
		t.Errorf("unrestricted (no port ACL) allow rule emitted: %q\n%s", loc, out)
	}
}

func TestCompileSquidAllowlist_BindsMultiPort(t *testing.T) {
	var c egress.Config
	c.Default.Domains = []egress.Domain{
		{Host: "internal.example.com", Ports: []int{443, 8080}},
	}
	out := CompileSquidAllowlist(c)

	if !strings.Contains(out, "acl prt0 port 443 8080") {
		t.Errorf("multi-port ACL not emitted:\n%s", out)
	}
	if !strings.Contains(out, "http_access allow dst0 prt0") ||
		!strings.Contains(out, "http_access allow CONNECT SSL_ports dst0 prt0") {
		t.Errorf("allow rules must pair the dst and port ACLs:\n%s", out)
	}
	// A disallowed port (25) must not be enumerated anywhere in the block.
	if strings.Contains(out, "25") {
		t.Errorf("port 25 leaked into the multi-port block:\n%s", out)
	}
	if loc := bareAllowRE.FindString(out); loc != "" {
		t.Errorf("unrestricted allow rule emitted: %q\n%s", loc, out)
	}
}

// TestCompileSquidAllowlist_Golden pins the exact generated ACL block for a
// two-domain config (with the model API excluded). A byte-for-byte change to
// the access policy should force a conscious test update.
func TestCompileSquidAllowlist_Golden(t *testing.T) {
	var c egress.Config
	c.Default.Domains = []egress.Domain{
		{Host: "api.anthropic.com", Ports: []int{443}}, // gateway-fronted, excluded
		{Host: "registry.npmjs.org", Ports: []int{443}},
		{Host: "internal.example.com", Ports: []int{443, 8080}},
	}
	want := "acl dst0 dstdomain registry.npmjs.org\n" +
		"acl prt0 port 443\n" +
		"http_access allow CONNECT SSL_ports dst0 prt0\n" +
		"http_access allow dst0 prt0\n" +
		"acl dst1 dstdomain internal.example.com\n" +
		"acl prt1 port 443 8080\n" +
		"http_access allow CONNECT SSL_ports dst1 prt1\n" +
		"http_access allow dst1 prt1\n"
	if got := CompileSquidAllowlist(c); got != want {
		t.Errorf("golden mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}
