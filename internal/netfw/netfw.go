// Package netfw compiles egress config into a userspace squid allowlist.
// No pf, no per-task network churn.
package netfw

import (
	"fmt"
	"strconv"
	"strings"

	"drydock/internal/egress"
)

// gatewayHosts are reached via the credential gateway, not squid.
var gatewayHosts = map[string]bool{
	"api.anthropic.com": true,
	"api.openai.com":    true,
}

// CompileSquidAllowlist renders the default-domain ACL block: for each allowed
// host (excluding the model API, which the gateway handles) a dstdomain ACL
// paired with a port ACL built from that host's configured ports, plus the
// http_access allow rules that require BOTH — so squid enforces the per-domain
// ports, not just the hostname. The result is a squid config fragment the conf
// pulls in via `include` (after it defines the SSL_ports and CONNECT ACLs the
// rules reference). CONNECT stays 443-only via the conf's global deny guard.
func CompileSquidAllowlist(cfg egress.Config) string {
	var b strings.Builder
	i := 0
	for _, d := range cfg.Default.Domains {
		if gatewayHosts[d.Host] {
			continue
		}
		writeDomainACL(&b, fmt.Sprintf("dst%d", i), fmt.Sprintf("prt%d", i), d, "")
		i++
	}
	return b.String()
}

// writeDomainACL emits a dstdomain + port ACL pair (named dstName/portName) for
// one domain and the two http_access allow rules (the CONNECT-on-443 tunnel and
// the plain request) that require both ACLs match. authACL, when non-empty, is
// an extra ACL name (a per-task proxy_auth ACL) appended LAST to each rule so a
// non-matching host or port short-circuits before the slow proxy_auth lookup —
// no spurious 407 for hosts a task never widened.
func writeDomainACL(b *strings.Builder, dstName, portName string, d egress.Domain, authACL string) {
	fmt.Fprintf(b, "acl %s dstdomain %s\n", dstName, d.Host)
	fmt.Fprintf(b, "acl %s port %s\n", portName, portList(d.Ports))
	tail := ""
	if authACL != "" {
		tail = " " + authACL
	}
	fmt.Fprintf(b, "http_access allow CONNECT SSL_ports %s %s%s\n", dstName, portName, tail)
	fmt.Fprintf(b, "http_access allow %s %s%s\n", dstName, portName, tail)
}

// portList renders squid port-ACL args, e.g. []int{443, 8080} -> "443 8080".
func portList(ports []int) string {
	ss := make([]string, len(ports))
	for i, p := range ports {
		ss[i] = strconv.Itoa(p)
	}
	return strings.Join(ss, " ")
}
