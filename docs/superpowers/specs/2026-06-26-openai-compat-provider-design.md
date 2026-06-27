# OpenAI-compatible provider (bring-your-own-model) — design

**Roadmap item:** Phase 3 provider expansion, B+C merged. Supersedes the
separate "native Gemini (3B)" and "openai-compat (3C)" plan: the spike found the
native Gemini CLI's base-URL override is undocumented and buggy, while Gemini
ships a solid OpenAI-compatible endpoint — so **Gemini is reached AS an
openai-compat provider**, not via a bespoke CLI. One cycle, not two.

## Depends on

**Phase 3A (the provider registry, PR #76) must merge first.** This design
builds directly on `internal/provider.Registry` (a new `openai-compat` row),
`agent.Vendor` delegation, and `config.AuthMode` — none of which exist on `main`
until #76 lands. Implementation is blocked until then; the spec/plan can be
finalized now, but Task 1 (the spike) is the only thing that can start against
the current tree (it's a standalone hands-on opencode investigation).

## Spike findings (what shaped this)

- Native `gemini` CLI base-URL override (`GOOGLE_GEMINI_BASE_URL`) is an
  undocumented `@google/genai` var with open bugs (sandbox non-propagation,
  custom-header gaps). Rejected as the integration path.
- Gemini exposes `https://generativelanguage.googleapis.com/v1beta/openai/`
  (chat/completions) — a drop-in OpenAI-compatible endpoint.
- That endpoint is **chat/completions, not the Responses API**, so drydock's
  `codex` agent (Responses API) can't drive it. The openai-compat lane needs a
  **chat/completions** agent CLI → `opencode` (chosen: single static binary,
  native OpenAI-compatible endpoint support, headless mode, lightest image add).

## Architecture

The existing gateway model, one new lane (no change to the trust boundary):

```
opencode (in VM) ──/v1/chat/completions──▶ drydock gateway :8088 ──▶ operator upstream
  OPENAI_BASE_URL = gateway                  (injects real key,        (Gemini /v1beta/openai,
  OPENAI_API_KEY  = tok_ lease                rewrites host+path,        OpenRouter, local, …)
                                              meters usage)
```

The real key stays host-only; the VM sees only a `tok_` lease + the gateway
base URL. The operator points the gateway's openai-compat upstream wherever
they want via config.

**Verified mechanisms (no new gateway machinery):**
- Forwarding: the gateway's `httputil.ReverseProxy` director (`gateway.go:215-222`)
  already sets scheme/host from `Vendor.BaseURL` and joins `Vendor.BasePath`,
  trimming a leading `/v1`. So `BaseURL=https://generativelanguage.googleapis.com`
  + `BasePath=/v1beta/openai` rewrites an incoming `/v1/chat/completions` to
  `/v1beta/openai/chat/completions`. Same mechanism codex already uses.
- Usage: `parseOpenAIUsage` already handles the chat/completions usage shape.
- Egress: `netfw.go:13` — "gatewayHosts are reached via the credential gateway,
  not squid." The VM reaches only the gateway IP for model traffic; the gateway
  dials the upstream **host-side**. So the new upstream needs **no VM-side squid
  allowlist entry** — the egress model is unchanged.

## The config-parameterized caveat (honest scope)

Unlike anthropic/openai (fixed endpoints, static `Vendor`), openai-compat's
upstream URL + key are operator config. Since `internal/provider` must not
import `config` (the 3A cycle rule), openai-compat **cannot** be a pure static
registry row. The split:

- The **registry** still enumerates it (agent name → vendor, wizard label, the
  injected `BaseURLEnv`/`TokenEnv`) — its `APIVendor`/`OAuthBackend` hooks are
  `nil`, plus a `ConfigBuilt bool` marking "brokerd builds this from config."
- **brokerd** constructs the openai-compat backend from the new config section
  (a small, deliberate branch in the backend loop — `if p.ConfigBuilt { … }`).

This is inherent to a bring-your-own-endpoint provider, not a 3A regression.
Everything else (enumeration, env injection, wizard, doctor) stays
registry-driven.

## Components

### 1. Sandbox image — `opencode`

Add the `opencode` binary to `image/Dockerfile` alongside claude-code/codex,
pinned by version + checksum (matching the existing pin policy). The
agent-entrypoint script gains an `opencode` branch that runs it
**non-interactively** (prompt + repo, reading `OPENAI_BASE_URL`/`OPENAI_API_KEY`
+ the model from env), parallel to the claude/codex launch.

### 2. Gateway openai-compat `Vendor`

`OpenAICompatVendor(baseURL, basePath string) gateway.Vendor`:
- `Name: "openai-compat"`, `BaseURL: baseURL`, `BasePath: basePath`.
- `Inject`: bearer auth with the real key (`Authorization: Bearer <key>`,
  delete `X-Api-Key`) — identical to `OpenAIVendor`'s inject.
- `ParseUsage: parseOpenAIUsage`, `Prices`: from config (see §3) or empty.

Constructed by brokerd (not the registry) from config.

### 3. Config — a new `openai_compat:` section

```yaml
openai_compat:
  base_url: ""        # e.g. https://generativelanguage.googleapis.com
  base_path: ""       # e.g. /v1beta/openai  (joined onto the inbound path)
  api_key_env: ""     # name of the host env var holding the real key (NEVER the key itself)
  model: ""           # model id passed to opencode, e.g. gemini-2.5-pro
  prices: {}          # optional: {"<model>": {input: <usd/Mtok>, output: <usd/Mtok>}} for USD metering
```

Validation: if `base_url` is set, `api_key_env` and `model` are required;
`base_url` must be an absolute https URL (or http only for an explicit localhost,
to allow a local endpoint). The key is read from `api_key_env` at boot like the
other keys — never stored in the file.

### 4. Registry row (3A)

`{Agent: "opencode", Vendor: "openai-compat", Label: "OpenAI-compatible (bring your own)",
  APIKeyEnv: "" /* config-driven */, BaseURLEnv: "OPENAI_BASE_URL", TokenEnv: "OPENAI_API_KEY",
  ConfigBuilt: true, APIVendor: nil, OAuthBackend: nil}`. `agent.Vendor("opencode")
→ "openai-compat"`. No OAuth, so no `auth` subcommand; `config.AuthMode` returns
`""` for this vendor (it's API-key-only, config-driven).

### 5. brokerd backend construction

In the registry loop, a branch for `p.ConfigBuilt`: if `cfg.OpenAICompat.BaseURL
== ""` skip (provider not configured); else resolve the key from
`cfg.OpenAICompat.APIKeyEnv`, build `OpenAICompatVendor(base_url, base_path)`
with `Prices` from config, and append the backend. The boot guard /
`len(backends)==0` check already generalizes.

### 6. Budget / usage

If `openai_compat.prices` supplies the model's rates, meter in USD exactly as
today. Otherwise (unknown cost for an arbitrary endpoint), fall back to the
**request-count cap** (`task_max_requests`) like subscription mode does with its
`MaxFloat64` budget — the gateway already supports this. The design does NOT
guess prices.

### 7. Wizard / doctor

- Wizard: add the openai-compat entry to the registry-driven menu; when chosen,
  prompt for `base_url`, `model`, and the `api_key_env` name (or accept a key and
  tell the operator which env var to export). Writes the `openai_compat:` section.
- Doctor: a non-fatal line reporting whether openai-compat is configured + its
  key env is set (no network call).

## Phasing — validation spike gates the build

**Task 1 is a hands-on spike** (mirroring the Codex-subscription spec's gating
spike), run before any integration code: drive `opencode` non-interactively
against an OpenAI-compatible endpoint **through a local gateway-style proxy**,
proving it (a) reads `OPENAI_BASE_URL` + `OPENAI_API_KEY` (or documents the
actual env/flags it needs), (b) speaks `/chat/completions`, (c) runs headless to
completion on a trivial repo task, and (d) the exact non-interactive invocation
+ install method (binary URL/checksum or npm pin). The remaining components are
written against the spike's findings; if opencode needs different env/flags than
assumed, §1/§4 adjust before the integration lands. If the spike fails outright,
we reassess the CLI choice (aider) before sinking integration cost.

## Error handling

- Unconfigured openai-compat (`base_url` empty) → the provider simply isn't
  available; a task `--agent opencode` is rejected by the existing boot/agent
  guard with a clear message (no key/endpoint configured).
- Missing `api_key_env` value at boot → same fail-loud path as the other
  providers' missing keys.
- Upstream errors (4xx/5xx from Gemini/OpenRouter) propagate back through the
  gateway to opencode as today; the task surfaces them in its stream/audit.

## Testing

- `OpenAICompatVendor`: `Inject` sets bearer + deletes `X-Api-Key`; a stub
  upstream confirms host+path rewrite (`/v1/chat/completions` →
  `<basePath>/chat/completions`) and that `parseOpenAIUsage` meters the response.
- Config: `openai_compat` round-trips; validation rejects base_url-without-key,
  non-absolute/non-https base_url; key is read from the env var, never the file.
- Registry: the `opencode → openai-compat` row; `agent.Vendor("opencode")`.
- brokerd: with `openai_compat.base_url` set + key present, a backend is built
  with the configured BaseURL/BasePath/prices; with base_url empty, none.
- Red-team: extend the A1 pattern — the real openai-compat key never enters the
  VM for this lane (VM sees only `tok_` + the gateway URL).
- The opencode end-to-end (real VM run) is environment-gated like the existing
  agent integration tests (needs the image + a real endpoint/key) — the spike
  (Task 1) is the mechanism-level proof.

## Done when

An operator can set `openai_compat: {base_url, api_key_env, model}` (e.g.
Gemini's `/v1beta/openai`), run `drydock submit --agent opencode`, and get a
sandboxed run whose model traffic flows VM → gateway → the configured upstream
with the real key host-only; budgeting works via config prices or the request
cap; and adding another openai-compat endpoint is a config change, not code.
