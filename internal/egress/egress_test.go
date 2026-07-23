package egress

import (
	"encoding/json"
	"os"
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
		// IP address literals must be rejected (squid would route direct, bypassing the allowlist)
		{"10.0.0.1", "IP address"},
		{"::1", "IP address"},
		{"192.168.1.1", "IP address"},
		{"2001:db8::1", "IP address"},
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

func TestDomainJSONIsLowercase(t *testing.T) {
	b, err := json.Marshal([]Domain{{Host: "x.test", Ports: []int{443}}})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"host":"x.test","ports":[443]}]`
	if string(b) != want {
		t.Fatalf("Domain JSON = %s, want %s (the web UI reads lowercase host/ports)", b, want)
	}
}

// TestLoad_FailClosed verifies that Load rejects invalid egress configs
// (wildcard hosts, IP literals, malformed YAML) and returns an error so the
// caller never operates with a partial or insecure allowlist.
func TestLoad_FailClosed(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "leading-dot wildcard host rejected",
			yaml:    "version: 1\ndefault:\n  domains:\n    - { host: \".example.com\", ports: [443] }\n",
			wantErr: "wildcard",
		},
		{
			name:    "star wildcard host rejected",
			yaml:    "version: 1\ndefault:\n  domains:\n    - { host: \"*.example.com\", ports: [443] }\n",
			wantErr: "wildcard",
		},
		{
			name:    "IP address literal rejected",
			yaml:    "version: 1\ndefault:\n  domains:\n    - { host: \"10.0.0.1\", ports: [443] }\n",
			wantErr: "IP address",
		},
		{
			name:    "malformed YAML rejected",
			yaml:    "version: 1\ndefault:\n  domains: [\n  - bad: yaml: here",
			wantErr: "parse egress config",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "egress*.yaml")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(tc.yaml); err != nil {
				t.Fatal(err)
			}
			f.Close()

			cfg, err := Load(f.Name())
			if err == nil {
				t.Fatalf("Load must return error for %q, but got cfg = %+v", tc.name, cfg)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
			// Fail-closed: on any error Load must return an empty Config so callers
			// that neglect the error cannot operate on a partial or insecure allowlist.
			if len(cfg.Default.Domains) != 0 {
				t.Errorf("%s: cfg.Default.Domains = %v, want empty on error", tc.name, cfg.Default.Domains)
			}
		})
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
	if !cfg.WideningRequiresApproval() {
		t.Errorf("WideningRequiresApproval() = false, want true")
	}
}

// The widening gate must fail closed: a config that OMITS requires_approval (or
// mistypes the key) must still require approval. Only an explicit false disables.
func TestWideningRequiresApproval_FailsClosed(t *testing.T) {
	var absent Config // per_task_widening block never set → RequiresApproval nil
	if !absent.WideningRequiresApproval() {
		t.Error("absent requires_approval must fail closed (gate ON)")
	}
	no := false
	yes := true
	var explicitFalse, explicitTrue Config
	explicitFalse.PerTaskWidening.RequiresApproval = &no
	explicitTrue.PerTaskWidening.RequiresApproval = &yes
	if explicitFalse.WideningRequiresApproval() {
		t.Error("explicit requires_approval:false must disable the gate")
	}
	if !explicitTrue.WideningRequiresApproval() {
		t.Error("explicit requires_approval:true must enable the gate")
	}
}

func TestLoad_RejectsTrailingYAMLDocument(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "egress*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// First document with a valid default block, second document carries a
	// security-relevant field (requires_approval) the operator believes is active;
	// silently ignoring it would fail open (F-08).
	body := `version: 1
default:
  domains:
    - { host: api.anthropic.com, ports: [443] }
---
requires_approval: false
`
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := Load(f.Name()); err == nil || !strings.Contains(err.Error(), "trailing YAML document") {
		t.Fatalf("Load with trailing document: got %v, want trailing-document rejection", err)
	}
}
