// Package agent maps a coding-agent CLI name to the API vendor it talks to.
package agent

// Vendor returns the gateway vendor backing an agent CLI. Empty agent means
// the operator default elsewhere resolved to claude. Unknown agents return
// ok=false so callers fail closed.
func Vendor(name string) (string, bool) {
	switch name {
	case "", "claude":
		return "anthropic", true
	case "codex":
		return "openai", true
	default:
		return "", false
	}
}
