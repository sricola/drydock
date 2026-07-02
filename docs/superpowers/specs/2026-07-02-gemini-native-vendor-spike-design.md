# Gemini Native Vendor — Validation Spike (3B, phase 1)

**Status:** design approved 2026-07-02
**Scope:** de-risking spike only. The native `GoogleVendor` build (registry row, image install, red-team) is a *separate* spec, written after this spike returns GREEN.

## Why

ROADMAP 3B adds a native Gemini vendor (`gemini → google`) as the proof that a
new provider is one registry row. Gemini is already reachable via the
openai-compat lane; the native path uses Google's own API + the native Gemini
CLI. drydock's whole model requires the sandboxed agent CLI to talk to the
in-broker gateway (which swaps a per-task bearer for the real key). Web research
shows `@google/gemini-cli` supports a base-URL override (`GOOGLE_GEMINI_BASE_URL`
/ `GEMINI_BASEURL`) but has open bugs where it ignores the override and forces
Cloud auth ([gemini-cli#15430], [gemini-cli#2168]). So brokering feasibility is
unproven. drydock has an established pattern of validating a new agent CLI with
a throwaway spike before building the vendor (it did this for codex and
opencode).

This spike answers, with real captured traffic, whether a native Gemini agent
can be brokered — before any vendor code is written.

## Decisions (from brainstorming)

- **Auth: API-key only.** The spike proves the `GEMINI_API_KEY` → gateway →
  real-key path. Google OAuth (Code Assist free tier) is explicitly out of
  scope for 3B.
- **Spike first**, then design/build the native vendor against the shapes this
  spike captures.
- **Native metering** (Gemini pricing table + `usageMetadata` parser) is in the
  *build* scope; this spike only *captures* the `usageMetadata` shape so the
  parser can be written against real bytes.

## The five questions the spike must answer

1. **Base-URL honored** — a pinned `@google/gemini-cli` routes API traffic to
   `GOOGLE_GEMINI_BASE_URL` instead of `generativelanguage.googleapis.com`.
2. **Brokerable auth** — in API-key mode the key travels in a request header the
   gateway can delete and replace (expected: `x-goog-api-key`), not embedded in
   a Cloud-only URL/path the gateway can't intercept.
3. **Headless run** — the CLI accepts a one-shot, non-interactive prompt and
   exits 0 without a TTY (analogous to `claude -p` / `codex exec`).
4. **Output shape** — what the CLI emits to stdout for a one-shot run (so the
   runner can later surface progress/results).
5. **Request/usage shapes** — the request path (`/v1beta/models/<model>:generateContent`
   vs `:streamGenerateContent`) and the response `usageMetadata`
   (`promptTokenCount` / `candidatesTokenCount` / `totalTokenCount`), captured
   verbatim for the future vendor's usage parser + price table.

## Design

### Form
A single tag-gated Go test: `tests/integration/gemini_spike_test.go`, build tag
`//go:build geminispike`. It is NOT in CI (CI runs only untagged tests). It is
run manually via a new Makefile target (`make test-gemini-spike`).

### Safety — the anti-pattern this explicitly avoids
The deleted codex/oauth spikes hit **live paid endpoints with real
credentials**; that is why they were removed. This spike does the opposite:
- **No live endpoint.** An `httptest.Server` stands in for the drydock gateway.
- **No real key.** A `SENTINEL-<random>` string stands in for the API key; the
  test asserts the sentinel arrives where the gateway would strip it.
Because it touches nothing real, it is safe to KEEP tag-gated afterward as a
regression guard on the brokering contract, rather than deleted.

### Mechanics
1. **Fake gateway** (`httptest.Server`): records every inbound request
   (`method`, `path`, `header`, `body`) into a slice, and responds to
   `…:generateContent` (and `…:streamGenerateContent` if the CLI uses it) with a
   minimal *valid* Gemini response containing one text candidate and a
   `usageMetadata` block, so the CLI treats the turn as successful. If the CLI
   rejects the canned response, that itself is a recorded finding.
2. **Environment**: `GOOGLE_GEMINI_BASE_URL` and `GEMINI_BASEURL` both set to the
   fake server; `GEMINI_API_KEY=SENTINEL-<random>`; auth forced to API-key mode
   (e.g. unset/neutralize the Cloud-auth path env the CLI honors —
   `GOOGLE_GENAI_USE_GCA`/equivalent, discovered during the spike). A short
   settings file under a temp `HOME`/`GEMINI_DIR` if the CLI requires one to skip
   first-run interactive setup.
3. **Invocation**: exec the pinned `gemini` with a one-shot prompt using the
   documented non-interactive flag (candidate: `gemini -p "<prompt>"` or
   `--prompt`; stdin fallback). The test records the exact invocation that
   worked. A short context timeout guards a hang (interactive-wait ⇒ RED).
4. **Assertions / recordings**:
   - PASS gate: ≥1 request reached the fake gateway (Q1); the request carried the
     sentinel in a single strippable header, recorded by name (Q2); the process
     exited 0 (Q3); at least one recorded request path + one response was
     produced (Q5).
   - Recordings (not pass/fail, but captured to the artifact): exact header name
     carrying the key, request path shape, request body shape, stdout shape (Q4),
     and the `usageMetadata` field names the CLI/endpoint use.

### Prerequisites & skip behavior
- Requires `gemini` on `PATH`. If absent: `t.Skip("gemini CLI not installed; go install/npm i -g @google/gemini-cli@<PIN>")`. Tag-gating already keeps it out of CI.
- Pins the validated version: the spike records the `gemini --version` it ran
  against; the findings doc names it so the build phase pins the same.

### Artifact
The test emits every captured shape via `t.Log` (and, for large bodies, writes
them to files under `t.TempDir()` whose paths it logs) — it does NOT write into
tracked `docs/`, so a run never dirties the tree. From that logged output, the
findings doc `docs/superpowers/specs/2026-07-02-gemini-spike-findings.md` is
then authored and committed by hand, containing: the validated CLI version, the
working invocation, the captured header name, request path, request body,
stdout sample, `usageMetadata` shape, and a **VERDICT: GREEN | RED** line with
reasoning. This doc is the input to the native-vendor build spec.

## Go/No-Go gate

**GREEN** (proceed to build the native `GoogleVendor`): base-URL honored AND key
isolated in a replaceable header AND headless exit 0 AND a `usageMetadata` shape
captured.

**RED** (do not build native; native Gemini isn't brokerable with this CLI):
the CLI ignores the base URL, forces Cloud auth, embeds the key
un-interceptably, or cannot run headless. Report the specific failure; Gemini
remains available via the existing openai-compat lane, and 3B is closed as
"not feasible natively with the current CLI" rather than shipped half-built.

## Out of scope (this spike)

- Any `internal/gateway` vendor code, `provider.Registry` row, config schema,
  Dockerfile change, entrypoint case, or red-team test — those are the *build*
  spec, gated on GREEN.
- Google OAuth / Code Assist auth.
- USD price values (only the `usageMetadata` *shape* is captured here).
- Running the CLI inside the drydock sandbox image (the spike runs it on the
  host against the fake gateway; image integration is build-phase work).

## Testing

The spike *is* a test; its assertions on captured fake-gateway traffic are the
verification. No further tests are in this phase.

## References
- [gemini-cli#15430] CLI ignores GOOGLE_GEMINI_BASE_URL and forces Cloud Auth
- [gemini-cli#2168] GOOGLE_*_BASE_URL not propagated to sandbox
- [gemini-cli#6380] Support configuring Gemini API base URL via env var
- drydock precedent: the (now-removed) `tests/integration/codex_spike_test.go`
  and `oauth_spike_test.go`; findings-doc pattern `docs(spike): opencode
  validation passed`.
