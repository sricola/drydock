// Package agent maps a coding-agent CLI name to the API vendor it talks to,
// reading the single source of truth in internal/provider.
package agent

import "drydock/internal/provider"

// Vendor returns the gateway vendor backing an agent CLI. Empty agent means the
// claude default. Unknown agents return ok=false so callers fail closed.
func Vendor(name string) (string, bool) {
	if name == "" {
		name = "claude" // empty default is claude specifically, not Registry[0]
	}
	if p, ok := provider.ByAgent(name); ok {
		return p.Vendor, true
	}
	return "", false
}
