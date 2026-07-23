// Package egress is the single source of truth for what a sandbox VM may reach.
package egress

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
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
		// *bool so an ABSENT key fails closed (nil → gate on). A bare bool would
		// decode a missing/mistyped `requires_approval:` to false and silently
		// disable the human egress-widening gate. Read via WideningRequiresApproval.
		RequiresApproval *bool `yaml:"requires_approval"`
	} `yaml:"per_task_widening"`
}

// WideningRequiresApproval reports whether per-task egress widening must pass the
// human gate. Fail-closed: an absent `requires_approval:` key (nil) returns true,
// so only an EXPLICIT `requires_approval: false` disables the gate.
func (c Config) WideningRequiresApproval() bool {
	return c.PerTaskWidening.RequiresApproval == nil || *c.PerTaskWidening.RequiresApproval
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
		return errors.New("egress: empty host")
	}
	if len(host) > 253 {
		return fmt.Errorf("egress: host too long: %q", host)
	}
	// IP address literals are rejected before the hostname regex: squid's
	// dstdomain ACL treats an IP literal as an exact IP match rather than a
	// hostname, but the egress config is for hostname-based policy only.
	// Allowing an IP would let a user pin a policy entry to a current IP that
	// may rotate, silently widening egress when that IP is reused by a
	// different host.
	if net.ParseIP(host) != nil {
		return fmt.Errorf("egress: IP address literals not allowed: %q", host)
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
		return errors.New("egress: at least one port required")
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
		return Config{}, fmt.Errorf("read egress config: %w", err)
	}
	// KnownFields(true): a misspelled egress key is a hard error, not a silent
	// no-op. The one control that must fail closed on absence (requires_approval)
	// is a *bool that already does; strictness covers the rest of the schema.
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse egress config: %w", err)
	}
	// One document only: a trailing document could carry requires_approval or
	// extra domains the operator believes are active. Fail closed (F-08).
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("parse egress config %s: trailing YAML document; only one document is allowed", path)
	}
	if err := ValidateDomains(cfg.Default.Domains); err != nil {
		return Config{}, fmt.Errorf("egress config %s: %w", path, err)
	}
	return cfg, nil
}
