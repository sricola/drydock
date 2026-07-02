// Package netfw compiles egress config into a userspace squid allowlist.
// No pf, no per-task network churn.
package netfw

import (
	"fmt"
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
