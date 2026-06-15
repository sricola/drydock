package netfw

import (
	"strings"
	"testing"

	"drydock/internal/egress"
)

func cfg() egress.Config {
	var c egress.Config
	c.Default.Domains = []egress.Domain{
		{Host: "api.anthropic.com", Ports: []int{443}},
		{Host: "registry.npmjs.org", Ports: []int{443}},
		{Host: "pypi.org", Ports: []int{443}},
	}
	return c
}

func TestCompileSquidAllowlist_ExcludesModelAPI(t *testing.T) {
	out := CompileSquidAllowlist(cfg())
	if strings.Contains(out, "anthropic.com") {
		t.Errorf("model API must go via the gateway, not squid:\n%s", out)
	}
	if !strings.Contains(out, "registry.npmjs.org") || !strings.Contains(out, "pypi.org") {
		t.Errorf("registries missing from squid allowlist:\n%s", out)
	}
}

func TestCompileSquidConf(t *testing.T) {
	out := CompileSquidConf("192.168.66.1:3128", "/run/allow.txt", "/run")
	for _, want := range []string{
		"http_port 192.168.66.1:3128",
		`acl allowed dstdomain "/run/allow.txt"`,
		"http_access deny CONNECT !allowed",
		"http_access deny all",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("squid.conf missing %q:\n%s", want, out)
		}
	}
}

func TestGatewayIP(t *testing.T) {
	got, err := GatewayIP("192.168.64.0/24")
	if err != nil || got != "192.168.64.1" {
		t.Fatalf("GatewayIP = %q err=%v, want 192.168.64.1", got, err)
	}
	if _, err := GatewayIP("not-a-cidr"); err == nil {
		t.Errorf("want error for bad CIDR")
	}
}
