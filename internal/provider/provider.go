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
	// ConfigBuilt marks a provider whose backend brokerd builds from config
	// (operator-parameterized endpoint), rather than the static APIVendor /
	// OAuthBackend hooks. Such a provider has nil APIVendor and OAuthBackend.
	ConfigBuilt bool

	// OAuthFile is the filename (not full path) of the stored OAuth credential
	// inside ~/.drydock/. It is the canonical source: auth.go derives the full
	// path as filepath.Join(cfgDir, p.OAuthFile). OAuthBackend and LoadOAuthSnap
	// use the same underlying constant (see oauthFile* below) so all three are
	// always in sync.
	OAuthFile string

	// LoadOAuthSnap loads the currently stored OAuth credential snapshot from
	// cfgDir for display purposes (drydock auth --status). Unlike OAuthBackend,
	// it does not construct a refreshable Cred — it just reads the file.
	LoadOAuthSnap func(cfgDir string) (gateway.CredSnapshot, error)

	// AuthLabel is the human-readable entity shown in auth status lines, e.g.
	// "Claude subscription" or "Codex (ChatGPT) subscription".
	AuthLabel string

	// RefreshOnExpiry, when true, appends "(will refresh on next use)" to the
	// expired-token status line. Set for providers that auto-refresh.
	RefreshOnExpiry bool
}

// oauthFile* are the single source of truth for each provider's OAuth
// credential filename. OAuthFile in the registry entry, the OAuthBackend
// closure, and the LoadOAuthSnap closure all derive from these constants so
// renaming one is a one-line change, not a search-and-replace across files.
const (
	oauthFileClaud = "claude-oauth.json"
	oauthFileCodex = "codex-oauth.json"
)

var Registry = []Provider{
	{
		Agent: "claude", Vendor: "anthropic", Label: "Claude Code (Anthropic)",
		APIKeyEnv: "ANTHROPIC_API_KEY", AuthCmd: "drydock auth claude",
		BaseURLEnv: "ANTHROPIC_BASE_URL", TokenEnv: "ANTHROPIC_AUTH_TOKEN",
		OAuthFile: oauthFileClaud,
		AuthLabel: "Claude subscription",
		APIVendor: gateway.AnthropicVendor,
		OAuthBackend: func(cfgDir string) (gateway.Backend, error) {
			store := gateway.FileCredStore(filepath.Join(cfgDir, oauthFileClaud))
			snap, err := store.Load()
			if err != nil {
				return gateway.Backend{}, err
			}
			return gateway.Backend{Vendor: gateway.AnthropicOAuthVendor(), Cred: gateway.NewOAuthCred(snap, store)}, nil
		},
		LoadOAuthSnap: func(cfgDir string) (gateway.CredSnapshot, error) {
			return gateway.FileCredStore(filepath.Join(cfgDir, oauthFileClaud)).Load()
		},
	},
	{
		Agent: "codex", Vendor: "openai", Label: "OpenAI Codex",
		APIKeyEnv: "OPENAI_API_KEY", AuthCmd: "drydock auth codex",
		BaseURLEnv: "OPENAI_BASE_URL", TokenEnv: "OPENAI_API_KEY",
		OAuthFile:       oauthFileCodex,
		AuthLabel:       "Codex (ChatGPT) subscription",
		RefreshOnExpiry: true,
		APIVendor:       gateway.OpenAIVendor,
		OAuthBackend: func(cfgDir string) (gateway.Backend, error) {
			store := gateway.NewCodexStore(filepath.Join(cfgDir, oauthFileCodex))
			snap, err := store.Load()
			if err != nil {
				return gateway.Backend{}, err
			}
			return gateway.Backend{Vendor: gateway.OpenAIOAuthVendor(store.AccountID()), Cred: gateway.NewOAuthCredCodex(snap, store)}, nil
		},
		LoadOAuthSnap: func(cfgDir string) (gateway.CredSnapshot, error) {
			return gateway.NewCodexStore(filepath.Join(cfgDir, oauthFileCodex)).Load()
		},
	},
	{
		Agent: "opencode", Vendor: "openai-compat", Label: "OpenAI-compatible (bring your own)",
		APIKeyEnv: "", AuthCmd: "",
		BaseURLEnv: "OPENAI_BASE_URL", TokenEnv: "OPENAI_API_KEY",
		ConfigBuilt: true,
		// APIVendor / OAuthBackend intentionally nil — brokerd builds from config.
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
