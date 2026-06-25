package egress

import (
	"strings"
	"testing"
)

func TestValidateHost(t *testing.T) {
	cases := []struct {
		in      string
		wantErr string // substring of expected error, "" means valid
	}{
		// Valid
		{"api.anthropic.com", ""},
		{"a.b.c.d.example.com", ""},
		{"single", ""},
		{"127-1.example.com", ""},
		// Wildcards rejected (squid dstdomain would silently widen)
		{".example.com", "wildcard"},
		{"*.example.com", "wildcard"},
		{"*", "wildcard"},
		// Malformed
		{"", "empty"},
		{"-leadinghyphen.com", "invalid"},
		{"trailinghyphen-.com", "invalid"},
		{"under_score.com", "invalid"},
		{"space in.host.com", "invalid"},
		{"<script>alert</script>", "invalid"},
		{strings.Repeat("a", 254), "too long"},
	}
	for _, tc := range cases {
		err := ValidateHost(tc.in)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("ValidateHost(%q) unexpected err: %v", tc.in, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("ValidateHost(%q) err = %v, want %q", tc.in, err, tc.wantErr)
		}
	}
}

func TestValidatePorts(t *testing.T) {
	if err := ValidatePorts(nil); err == nil {
		t.Errorf("empty ports must error")
	}
	if err := ValidatePorts([]int{0, 443}); err == nil {
		t.Errorf("port 0 must error")
	}
	if err := ValidatePorts([]int{443, 65536}); err == nil {
		t.Errorf("port > 65535 must error")
	}
	if err := ValidatePorts([]int{443, 8443}); err != nil {
		t.Errorf("valid ports rejected: %v", err)
	}
}

func TestValidateDomains_DuplicatesRejected(t *testing.T) {
	err := ValidateDomains([]Domain{
		{Host: "a.example.com", Ports: []int{443}},
		{Host: "a.example.com", Ports: []int{8443}},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("want duplicate err, got %v", err)
	}
}

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
	if len(cfg.Default.Domains) != 2 ||
		cfg.Default.Domains[0].Host != "api.anthropic.com" ||
		cfg.Default.Domains[1].Host != "api.openai.com" {
		t.Errorf("Domains = %+v, want [api.anthropic.com, api.openai.com]", cfg.Default.Domains)
	}
	if !cfg.PerTaskWidening.RequiresApproval {
		t.Errorf("RequiresApproval = false, want true")
	}
}
