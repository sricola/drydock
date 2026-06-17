# Codex agent support

**Date:** 2026-06-17
**Status:** approved, pre-implementation

## Goal

Let a drydock task run **OpenAI Codex** as its coding agent instead of Claude
Code, with the same security properties drydock already gives Claude: the real
API key never enters the sandbox, egress stays deny-by-default, and the per-task
USD budget / revoke still applies.

## Decisions (locked)

1. **Auth model:** gateway-brokered API key. `brokerd` holds the real
   `OPENAI_API_KEY` host-only; the gateway gains an OpenAI upstream and mints
   per-task budget-capped bearer tokens. Full security parity with Claude. No
   key in the VM, no ChatGPT OAuth.
2. **Agent selection:** per-task `--agent {claude|codex}` flag plus a
   `default_agent` config key (default `claude`). Tasks can mix agents
   concurrently.
3. **Image:** one image hosts both CLIs; `entrypoint.sh` dispatches on
   `DRYDOCK_AGENT`. Image renamed `claude-sandbox` -> `drydock-sandbox`.
4. **Budget parity:** full. OpenAI pricing table + an OpenAI usage parser
   (streaming + non-streaming) so Codex tasks get the same USD cap, metering,
   and revoke as Claude.

## Architecture (Approach A: vendor registry in one gateway)

An **agent** is the CLI in the VM (`claude`, `codex`); a **vendor** is the API
it calls (`anthropic`, `openai`). 1:1 in v1 but kept distinct in the code.

### Gateway (`internal/gateway`)
- New `Vendor` value: `{name, baseURL, injectAuth(req, realKey), parseUsage(body, ct), prices}`.
- Registry of two vendors:
  - `anthropic` — strip `Authorization`, set `X-Api-Key` + `anthropic-version`;
    Claude usage shapes; Claude price table (unchanged behavior).
  - `openai` — strip incoming, set `Authorization: Bearer <realKey>`; OpenAI
    usage shape; OpenAI price table.
- `Lease` carries its vendor; `Mint` takes a vendor; `director` and `meter`
  dispatch through the lease's vendor. The existing `usageReader` buffering /
  metering machinery is reused as-is.

### Credentials (`provider.go` + `brokerd`)
- `grant.EnvVars()` becomes vendor-aware: `openai` emits
  `OPENAI_BASE_URL` + `OPENAI_API_KEY=<token>` (mirrors the `ANTHROPIC_*` pair;
  Codex sends `Authorization: Bearer $OPENAI_API_KEY`, which the gateway already
  validates).
- `brokerd` reads both `ANTHROPIC_API_KEY` and `OPENAI_API_KEY`. A vendor with
  no key present is unavailable; a task requesting it is rejected with a clear
  error. Existing Claude-only setups are unaffected (Anthropic key still
  required as today).

### Image + entrypoint
- `Dockerfile` adds `npm i -g @openai/codex@<pinned ARG>` alongside the pinned
  claude-code.
- `entrypoint.sh` dispatches on `DRYDOCK_AGENT`: `claude` keeps today's
  invocation; `codex` runs `codex exec` in bypass-approvals / no-internal-sandbox
  mode (the VM is the sandbox), mapping `DRYDOCK_MODEL` to codex's model flag.
- `broker.go` injects `DRYDOCK_AGENT`; `runner` is unchanged (already generic).

### Egress (`internal/netfw` + egress config)
- Generalize the single `modelAPIHost` const into a gateway-hosted host set
  `{api.anthropic.com, api.openai.com}` — both skipped from squid (reached via
  the gateway). The nft pin (gateway:8088 + squid:3128) is unchanged.
- Add `api.openai.com:443` to the three egress allowlist copies for consistency.

### Misc
- `doctor.go` also checks `codex --version`.
- Image rename touches `init.go` (`ensureNamedImage`), config defaults, and
  tests.
- Docs (README, site, `THREAT_MODEL.md`) note the second agent and that the
  OpenAI key is also host-only. Described honestly — no implied audit, no
  overclaiming of maturity.

## Risk / verification

- **OpenAI usage parsing** is the one unknown. The parser targets the Responses
  API shape (`input_tokens`/`output_tokens`), but the exact streaming event and
  field names must be verified against a **real Codex capture** during
  implementation (it may be chat/completions with
  `prompt_tokens`/`completion_tokens`). Metering will be validated against
  captured traffic, not assumed — so "full budget parity" is honest about this
  step.

## Testing

- Unit: openai auth injection; openai usage parser (table-driven, streaming +
  non-streaming); gateway vendor routing; agent validation (fail-closed on
  unknown); `entrypoint.sh` dispatch.
- Integration (macOS-only, not in CI): a Codex variant of the brokerd task test.
