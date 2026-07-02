// Package creds issues a credential Grant per task. A Grant exposes the env vars
// to inject into the sandbox and a Revoke hook. This lets a gateway-backed,
// per-task-token provider replace the static-key provider without changing callers.
package creds

type Grant interface {
	EnvVars() []string
	Revoke() error
	// Spent returns USD metered against this grant so far (0 for providers
	// that don't meter).
	Spent() float64
}

type Provider interface {
	// Mint issues a grant; budgetUSD is advisory for providers that meter spend.
	Mint(budgetUSD float64) (Grant, error)
}
