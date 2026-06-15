// Package egress is the single source of truth for what a sandbox VM may reach.
package egress

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Domain struct {
	Host  string `yaml:"host"`
	Ports []int  `yaml:"ports"`
}

type Config struct {
	Version int `yaml:"version"`
	Default struct {
		AllowDNS bool     `yaml:"allow_dns"`
		Domains  []Domain `yaml:"domains"`
		CIDRs    []string `yaml:"cidrs"`
	} `yaml:"default"`
	PerTaskWidening struct {
		RequiresApproval bool `yaml:"requires_approval"`
	} `yaml:"per_task_widening"`
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
