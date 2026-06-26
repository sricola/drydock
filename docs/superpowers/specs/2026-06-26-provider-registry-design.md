# Provider registry refactor (Phase 3A) — design

**Roadmap item:** Phase 3A — the foundation for provider expansion (3B Gemini, 3C OpenAI-compat).

## Problem

The gateway layer is already additive — a new upstream is a `gateway.Vendor{}`
value plus a pricing table and a usage parser, and nothing downstream
enumerates vendors. The **CLI/config layer**, by contrast, hardcodes exactly
two providers (`claude`/`anthropic`, `codex`/`openai`) by hand in ~7 places:

1. `internal/agent/agent.go` — the `Vendor(name)` switch mapping agent→vendor.
2. `internal/config/config.go` `validate()` — `default_agent must be claude or codex`.
3. `cmd/brokerd/main.go` — two `switch cfg.*Auth` blocks building backends
   (lines ~230-256), the provider-map subscription detection (`273-274`), and
   the boot guard (`290-296`).
4. `internal/gateway/provider.go` — `EnvVars()` switches vendor →
   `OPENAI_BASE_URL`/`OPENAI_API_KEY` vs `ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN`.
5. `cmd/drydock/wizard.go` — the "Which coding agent?" menu + `wantClaude`/`wantCodex`.
6. `cmd/drydock/auth.go` — `auth claude|codex`.
7. `cmd/drydock/doctor.go` / `start` — per-vendor availability lines.

Adding a third provider today means touching all of these and growing every
two-way branch into a three-way one — the special-case-on-shared-infra smell.

## Goal

One source of truth (`internal/provider.Registry`) the CLI/config layer reads,
so adding a provider becomes **one registry row + one gateway `Vendor`** instead
of edits in 7 files. **3A is behavior-identical**: `claude` and `codex` remain
the only entries, existing config files load unchanged, the wizard/auth/doctor
output is the same, and the whole suite passes without test rewrites beyond the
mechanical (constructor calls that move).

## Scope

In scope: the `internal/provider` package; refactoring the 7 sites to consume
it; keeping behavior byte-identical. Out of scope: any new provider (3B/3C); any
config-schema change (the typed `*_auth` fields stay); unifying the gateway
`Vendor` constructors (already additive).

## Architecture & imports

A new leaf-ish package `internal/provider` holds the registry. Import direction
(no cycles):

- `internal/provider` → imports `internal/gateway` (to hold the static API-key
  `Vendor` constructor + the per-vendor OAuth-backend hook). **Never imports
  `config`.**
- `internal/config` → imports `internal/provider` (to validate `default_agent`
  against `provider.Agents()`), and gains an `AuthMode(vendor string) string`
  accessor returning the right typed field.
- `cmd/brokerd`, `cmd/drydock` → import `provider` (+ existing deps).
- `internal/gateway` imports neither `provider` nor `config` (unchanged) — env
  var names it needs are passed in (below).

## Components

### 1. `internal/provider` — the registry

```go
package provider

import "drydock/internal/gateway"

// Provider is everything the CLI/config layer needs to enumerate one coding
// agent and the upstream API it talks to. Static data only — no per-run state.
type Provider struct {
	Agent      string // sandbox CLI name: "claude", "codex"
	Vendor     string // gateway vendor name: "anthropic", "openai"
	Label      string // wizard display: "Claude Code (Anthropic)"
	APIKeyEnv  string // host env holding the real key: "ANTHROPIC_API_KEY"
	AuthCmd    string // remediation hint: "drydock auth claude"
	// BaseURLEnv / TokenEnv are the names of the env vars injected INTO the VM
	// pointing the agent CLI at the gateway with its tok_ lease. (anthropic:
	// ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN; openai: OPENAI_BASE_URL /
	// OPENAI_API_KEY.)
	BaseURLEnv string
	TokenEnv   string
	// APIVendor is the gateway vendor for API-key mode (static, no args).
	APIVendor func() gateway.Vendor
	// OAuthBackend builds the subscription-mode backend (vendor-specific store +
	// OAuth cred wiring). nil = this provider has no subscription mode. cfgDir
	// is ~/.drydock. Returns a ready gateway.Backend or an error if creds are
	// missing/unusable.
	OAuthBackend func(cfgDir string) (gateway.Backend, error)
}

var Registry = []Provider{
	{Agent: "claude", Vendor: "anthropic", Label: "Claude Code (Anthropic)",
	 APIKeyEnv: "ANTHROPIC_API_KEY", AuthCmd: "drydock auth claude",
	 BaseURLEnv: "ANTHROPIC_BASE_URL", TokenEnv: "ANTHROPIC_AUTH_TOKEN",
	 APIVendor: gateway.AnthropicVendor,
	 OAuthBackend: func(dir string) (gateway.Backend, error) { /* FileCredStore + AnthropicOAuthVendor + NewOAuthCred */ }},
	{Agent: "codex", Vendor: "openai", Label: "OpenAI Codex",
	 APIKeyEnv: "OPENAI_API_KEY", AuthCmd: "drydock auth codex",
	 BaseURLEnv: "OPENAI_BASE_URL", TokenEnv: "OPENAI_API_KEY",
	 APIVendor: gateway.OpenAIVendor,
	 OAuthBackend: func(dir string) (gateway.Backend, error) { /* NewCodexStore + OpenAIOAuthVendor(accountID) + NewOAuthCredCodex */ }},
}

// Helpers (table-driven, no hardcoded names):
func ByAgent(agent string) (Provider, bool)   // "" -> claude default resolved by caller
func ByVendor(vendor string) (Provider, bool)
func Agents() []string                          // {"claude","codex"} — for the allowlist
func Labels() []string                          // wizard menu, registry order
```

The two `OAuthBackend` closures are lifted verbatim from `main.go`'s current
`switch` arms (the `FileCredStore`/`NewCodexStore` + `*OAuthVendor` + `NewOAuthCred*`
wiring), so no behavior changes — they just move.

### 2. `internal/agent` — delegate to the registry

`agent.Vendor(name)` becomes: empty/`"claude"` → look up the default; otherwise
`provider.ByAgent(name)` → `(p.Vendor, true)`, unknown → `("", false)`. Keep the
existing exported signature so callers are unchanged. (Decide: agent imports
provider, or this lookup moves into provider and agent re-exports — either keeps
the `agent.Vendor` API stable. Prefer agent importing provider, smallest blast
radius.)

### 3. `internal/config` — registry-driven validation + `AuthMode`

- `validate()`: replace the literal `default_agent must be claude or codex` with
  a check against `provider.Agents()` (error message lists the allowed set from
  the registry). The `anthropic_auth`/`openai_auth` field validations stay
  (still typed fields).
- Add `func (c *Config) AuthMode(vendor string) string` returning the matching
  typed field (`anthropic`→`c.AnthropicAuth`, `openai`→`c.OpenAIAuth`; unknown →
  `""`). This is the generic accessor brokerd uses so it never names a vendor.

### 4. `cmd/brokerd/main.go` — generic backend loop

Replace the two hand-written `switch cfg.*Auth` blocks and the provider-map
subscription detection with one loop over `provider.Registry`:

```go
var backends []gateway.Backend
for _, p := range provider.Registry {
	switch cfg.AuthMode(p.Vendor) {
	case "subscription":
		b, err := p.OAuthBackend(config.Dir())
		if err != nil {
			die(p.Vendor+"_auth=subscription but no usable credentials — run `"+p.AuthCmd+"`", "err", err)
		}
		backends = append(backends, b)
	default: // api_key
		if key := resolveAPIKey(p.APIKeyEnv, fileKeys); key != "" {
			backends = append(backends, gateway.Backend{Vendor: p.APIVendor(), Cred: gateway.StaticKey(key)})
		}
	}
}
```

The provider-map build loop sets `budget = MaxFloat64` when
`cfg.AuthMode(b.Vendor.Name) == "subscription"` (replacing `subAnthropic ||
subOpenAI`), and passes the registry's `BaseURLEnv`/`TokenEnv` into the
`gateway.Provider` (new fields, see component 5). The boot guard uses
`provider.ByAgent(cfg.DefaultAgent)` + `p.APIKeyEnv` for its message.

### 5. `internal/gateway/provider.go` — env names passed in, not switched

Add `BaseURLEnv, TokenEnv string` to `gateway.Provider` (set by brokerd from the
registry). `grant.EnvVars()` becomes:

```go
func (g *grant) EnvVars() []string {
	return []string{g.baseURLEnv + "=" + g.baseURL, g.tokenEnv + "=" + g.token}
}
```

`grant` carries the two env names (threaded from `Provider.Mint`). The `switch
g.vendor` is gone; gateway stays free of any provider/config import. (Anthropic's
`TokenEnv` is `ANTHROPIC_AUTH_TOKEN`, not `..._API_KEY` — preserved exactly.)

### 6. `cmd/drydock/wizard.go` — menu from `provider.Labels()`

Build the "Which coding agent?" choices from `provider.Labels()` + a trailing
"both"/"all" option, mapping the selection back to which providers the operator
wants. Behavior-identical for the two-entry registry (the menu reads
`["Claude Code (Anthropic)", "OpenAI Codex", "both"]` exactly as today).

### 7. `cmd/drydock/auth.go`, `doctor.go` — registry-driven

- `auth`: the `claude|codex` dispatch and usage string enumerate the registry's
  agents instead of literals (the per-agent OAuth *implementations* stay — only
  the dispatch/usage generalizes).
- `doctor`/`start`: availability lines iterate the registry (`p.Vendor`,
  `p.APIKeyEnv`) rather than naming anthropic/openai.

## Error handling

No behavioral change. Same fail-loud messages (subscription-without-creds dies
with the same `drydock auth …` hint, now sourced from `p.AuthCmd`; boot guard
warns the same way). Unknown `default_agent` still errors at config load, now
listing the registry's agents.

## Testing

The win is testability of the registry seam:
- `internal/provider`: `Agents()`/`Labels()` return the registry in order;
  `ByAgent`/`ByVendor` hit and miss; every entry has non-empty required fields
  (a table test that guards a future malformed row).
- `internal/agent`: `Vendor("")`/`Vendor("claude")`→`("anthropic",true)`,
  `"codex"`→`("openai",true)`, unknown→`("",false)` — unchanged assertions,
  proving the delegation is behavior-identical.
- `internal/config`: `AuthMode("anthropic")`/`("openai")` reflect the typed
  fields; `AuthMode("nope")` → `""`; `validate()` rejects an unknown
  `default_agent` and accepts `claude`/`codex` (the existing tests should pass
  unchanged — that IS the behavior-identical proof).
- `internal/gateway`: `grant.EnvVars()` emits `ANTHROPIC_BASE_URL`/
  `ANTHROPIC_AUTH_TOKEN` for an anthropic provider and `OPENAI_BASE_URL`/
  `OPENAI_API_KEY` for openai — byte-identical to today (this is the field most
  at risk in the refactor; pin it).
- The existing `cmd/brokerd` / `cmd/drydock` tests pass unchanged.

## Done when

Adding a provider is a `provider.Registry` row + a gateway `Vendor`
(+pricing/usage), with no edits to `config.validate`, the brokerd backend loop,
`grant.EnvVars`, the wizard menu, or the boot guard; `claude`/`codex` behavior,
config files, and all tests are unchanged; and `internal/provider` is the single
place 3B (Gemini) and 3C (OpenAI-compat) will add their rows.
