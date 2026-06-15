package egress

import "testing"

func TestCompileAllowlist_DefaultPlusExtra(t *testing.T) {
	cfg := Config{}
	cfg.Default.Domains = []Domain{
		{Host: "api.anthropic.com", Ports: []int{443}},
		{Host: "pypi.org", Ports: []int{443}},
	}
	extra := []Domain{{Host: "internal.example.com", Ports: []int{443, 8443}}}

	got := CompileAllowlist(cfg, extra)
	want := "api.anthropic.com 443\npypi.org 443\ninternal.example.com 443\ninternal.example.com 8443\n"
	if got != want {
		t.Fatalf("CompileAllowlist mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestLoad_ParsesYAML(t *testing.T) {
	cfg, err := Load("testdata/egress.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Default.AllowDNS {
		t.Errorf("AllowDNS = false, want true")
	}
	if len(cfg.Default.Domains) != 1 || cfg.Default.Domains[0].Host != "api.anthropic.com" {
		t.Errorf("Domains = %+v, want one api.anthropic.com", cfg.Default.Domains)
	}
	if !cfg.PerTaskWidening.RequiresApproval {
		t.Errorf("RequiresApproval = false, want true")
	}
}
