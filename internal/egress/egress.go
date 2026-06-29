// Package egress is the single source of truth for what a sandbox VM may reach.
package egress

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type Domain struct {
	Host  string `yaml:"host" json:"host"`
	Ports []int  `yaml:"ports" json:"ports"`
}

type Config struct {
	Version int `yaml:"version"`
	Default struct {
		Domains []Domain `yaml:"domains"`
	} `yaml:"default"`
	PerTaskWidening struct {
		RequiresApproval bool `yaml:"requires_approval"`
	} `yaml:"per_task_widening"`
}

// hostnameRE accepts an RFC-1035-shaped hostname: dot-separated labels, each
// label is letters/digits/hyphens (no leading/trailing hyphen), labels are
// 1-63 chars, total length 1-253. Wildcards (leading dot, '*') are rejected
// because squid's dstdomain treats `.example.com` as a wildcard for the
// whole apex; the visible YAML wouldn't show that scope.
var hostnameRE = regexp.MustCompile(
	`^(?:[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)(?:\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`,
)

// ValidateHost returns an error if host isn't an exact-match hostname squid
// would treat as a single allowlist entry. Wildcards and otherwise-malformed
// names are rejected. Empty input is rejected.
func ValidateHost(host string) error {
	if host == "" {
		return fmt.Errorf("egress: empty host")
	}
	if len(host) > 253 {
		return fmt.Errorf("egress: host too long: %q", host)
	}
	if strings.HasPrefix(host, ".") || strings.HasPrefix(host, "*") {
		return fmt.Errorf("egress: wildcard host not allowed: %q", host)
	}
	if !hostnameRE.MatchString(host) {
		return fmt.Errorf("egress: invalid hostname: %q", host)
	}
	return nil
}

// ValidatePorts requires at least one port and each in 1..65535.
func ValidatePorts(ports []int) error {
	if len(ports) == 0 {
		return fmt.Errorf("egress: at least one port required")
	}
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("egress: port out of range: %d", p)
		}
	}
	return nil
}

// ValidateDomains runs ValidateHost + ValidatePorts on each entry and rejects
// duplicate hosts. Errors include the offending host so misconfigurations
// surface clearly.
func ValidateDomains(domains []Domain) error {
	seen := make(map[string]bool, len(domains))
	for _, d := range domains {
		if err := ValidateHost(d.Host); err != nil {
			return err
		}
		if err := ValidatePorts(d.Ports); err != nil {
			return fmt.Errorf("%s: %w", d.Host, err)
		}
		if seen[d.Host] {
			return fmt.Errorf("egress: duplicate host: %q", d.Host)
		}
		seen[d.Host] = true
	}
	return nil
}

func Load(path string) (Config, error) {
	var cfg Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read egress config: %w", err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse egress config: %w", err)
	}
	if err := ValidateDomains(cfg.Default.Domains); err != nil {
		return cfg, fmt.Errorf("egress config %s: %w", path, err)
	}
	return cfg, nil
}

// CompileAllowlist renders "<host> <port>" lines consumed by init-firewall.sh.
// Default domains come first, then approved per-task extras.
func CompileAllowlist(cfg Config, extra []Domain) string {
	var b strings.Builder
	for _, d := range append(append([]Domain{}, cfg.Default.Domains...), extra...) {
		for _, p := range d.Ports {
			fmt.Fprintf(&b, "%s %d\n", d.Host, p)
		}
	}
	return b.String()
}
