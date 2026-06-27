// Package provider is the single registry of coding-agent CLIs and the upstream
// API each talks to. The CLI/config layer enumerates providers from here so a
// new provider is one row, not edits across the codebase. Imports gateway only
// (never config — the OAuth hook takes cfgDir as a parameter).
package provider

import (
	"path/filepath"

	"drydock/internal/gateway"
)

// Provider is the static description of one agent + its upstream vendor.
type Provider struct {
	Agent        string                                       // sandbox CLI: "claude", "codex"
	Vendor       string                                       // gateway vendor name: "anthropic", "openai"
	Label        string                                       // wizard display
	APIKeyEnv    string                                       // host env holding the real key
	AuthCmd      string                                       // remediation hint, e.g. "drydock auth claude"
	BaseURLEnv   string                                       // env injected into the VM: base URL var
	TokenEnv     string                                       // env injected into the VM: token var
	APIVendor    func() gateway.Vendor                        // API-key-mode vendor (static)
	OAuthBackend func(cfgDir string) (gateway.Backend, error) // subscription mode; nil if unsupported
}

var Registry = []Provider{
	{
		Agent: "claude", Vendor: "anthropic", Label: "Claude Code (Anthropic)",
		APIKeyEnv: "ANTHROPIC_API_KEY", AuthCmd: "drydock auth claude",
		BaseURLEnv: "ANTHROPIC_BASE_URL", TokenEnv: "ANTHROPIC_AUTH_TOKEN",
		APIVendor: gateway.AnthropicVendor,
		OAuthBackend: func(cfgDir string) (gateway.Backend, error) {
			store := gateway.FileCredStore(filepath.Join(cfgDir, "claude-oauth.json"))
			snap, err := store.Load()
			if err != nil {
				return gateway.Backend{}, err
			}
			return gateway.Backend{Vendor: gateway.AnthropicOAuthVendor(), Cred: gateway.NewOAuthCred(snap, store)}, nil
		},
	},
	{
		Agent: "codex", Vendor: "openai", Label: "OpenAI Codex",
		APIKeyEnv: "OPENAI_API_KEY", AuthCmd: "drydock auth codex",
		BaseURLEnv: "OPENAI_BASE_URL", TokenEnv: "OPENAI_API_KEY",
		APIVendor: gateway.OpenAIVendor,
		OAuthBackend: func(cfgDir string) (gateway.Backend, error) {
			store := gateway.NewCodexStore(filepath.Join(cfgDir, "codex-oauth.json"))
			snap, err := store.Load()
			if err != nil {
				return gateway.Backend{}, err
			}
			return gateway.Backend{Vendor: gateway.OpenAIOAuthVendor(store.AccountID()), Cred: gateway.NewOAuthCredCodex(snap, store)}, nil
		},
	},
}

func ByAgent(agent string) (Provider, bool) {
	for _, p := range Registry {
		if p.Agent == agent {
			return p, true
		}
	}
	return Provider{}, false
}

func ByVendor(vendor string) (Provider, bool) {
	for _, p := range Registry {
		if p.Vendor == vendor {
			return p, true
		}
	}
	return Provider{}, false
}

func Agents() []string {
	out := make([]string, len(Registry))
	for i, p := range Registry {
		out[i] = p.Agent
	}
	return out
}

func Labels() []string {
	out := make([]string, len(Registry))
	for i, p := range Registry {
		out[i] = p.Label
	}
	return out
}
