// Package netfw compiles egress config into a userspace squid allowlist and
// derives the stable network's gateway IP. No pf, no per-task network churn.
package netfw

import (
	"fmt"
	"net"
	"strings"

	"drydock/internal/egress"
)

// gatewayHosts are reached via the credential gateway, not squid.
var gatewayHosts = map[string]bool{
	"api.anthropic.com": true,
	"api.openai.com":    true,
}

// CompileSquidAllowlist renders one dstdomain per allowed host, excluding the
// model API (which the gateway handles). Consumed as a squid "dstdomain" file.
func CompileSquidAllowlist(cfg egress.Config) string {
	var b strings.Builder
	for _, d := range cfg.Default.Domains {
		if gatewayHosts[d.Host] {
			continue
		}
		fmt.Fprintf(&b, "%s\n", d.Host)
	}
	return b.String()
}

// GatewayIP returns the .1 host address of the given CIDR (the vmnet gateway).
func GatewayIP(cidr string) (string, error) {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("netfw: bad subnet %q: %w", cidr, err)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return "", fmt.Errorf("netfw: not an IPv4 subnet: %q", cidr)
	}
	ip4[3] = 1
	return ip4.String(), nil
}
