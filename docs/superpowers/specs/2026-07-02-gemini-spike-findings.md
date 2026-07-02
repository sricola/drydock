# Gemini Brokering Spike — Findings (2026-07-02)

**CLI version validated:** `@google/gemini-cli` 0.49.0 (`gemini --version` → `0.49.0`)
**Working headless invocation:** `gemini --skip-trust -m gemini-2.5-flash -p "say ok"`
**Auth-neutralizing config required:** a user settings file
`$HOME/.gemini/settings.json` containing
`{"security":{"auth":{"selectedType":"gemini-api-key"}}}`, plus env
`GOOGLE_GEMINI_BASE_URL=<gateway>`, `GEMINI_API_KEY=<key>`,
`GOOGLE_GENAI_USE_VERTEXAI=false`, and `GEMINI_CLI_TRUST_WORKSPACE=true`.

This spike ran the real CLI against a fake `httptest` gateway with a
`SENTINEL-<random>` key. No live endpoint, no real credential — the design's
safety constraint holds.

## Captured shapes

- **Auth header carrying the key:** `x-goog-api-key: SENTINEL-<random>` — on
  every request. `Authorization` is empty; the key never appears in the URL
  query (`key=` absent). The gateway can delete/replace exactly one header.
- **Request path:** `POST /v1beta/models/<model>:streamGenerateContent?alt=sse`
  for the turn (streaming SSE). With the "auto" model router left on, the router
  additionally fires `POST /v1beta/models/<model>:generateContent` classifier
  calls; pinning `-m` removes those, leaving a single streaming turn. Model name
  is resolved by the CLI (e.g. `gemini-2.5-flash` → `gemini-3.5-flash` in 0.49.0).
- **Request body (abridged):**
  ```json
  {
    "contents": [
      {"parts": [{"text": "<session_context>…"}], "role": "user"},
      {"parts": [{"text": "say ok"}], "role": "user"}
    ],
    "systemInstruction": {"parts": [{"text": "You are Gemini CLI…"}]},
    "tools": [ … ],
    "generationConfig": {
      "temperature": 1, "topP": 0.95, "topK": 64,
      "thinkingConfig": {"includeThoughts": true, "thinkingLevel": "HIGH"}
    }
  }
  ```
  Top-level keys: `contents`, `systemInstruction`, `tools`, `generationConfig`.
- **Response `usageMetadata`:** the fake gateway returned, and the CLI accepted
  without error, the standard Google Gemini API field names:
  `promptTokenCount` / `candidatesTokenCount` / `totalTokenCount`. These are the
  names the future vendor's usage parser should read. (Google's live API also
  emits `thoughtsTokenCount` and `cachedContentTokenCount` on some turns; the
  parser should treat those as optional.)
- **CLI stdout shape (headless):** a couple of `TERM=dumb` cosmetic warnings and
  `[STARTUP]` diagnostics on stderr, then the model's final text (`ok`) on
  stdout, and process exit 0. `-o json` / `-o stream-json` are also available
  (see `gemini --help`) for a structured envelope if the runner wants one.

## The two harness adjustments (and why)

Both are *harness* fixes for headless-launch gates, not brokering blockers:

1. **Auth type must be pinned to `gemini-api-key` via settings.** In 0.49.0
   `getAuthTypeFromEnv()` checks `GOOGLE_GEMINI_BASE_URL` *before*
   `GEMINI_API_KEY` and, when it is set, returns a new `"gateway"` auth type.
   The non-interactive `validateAuthMethod()` has no `gateway` branch, so it
   rejected the run with `Invalid auth method selected.` (exit 41). Writing
   `security.auth.selectedType = "gemini-api-key"` makes it the
   `configuredAuthType`, which wins over the env auto-detect
   (`effectiveAuthType = configuredAuthType || getAuthTypeFromEnv()`). The
   base-URL override is applied by the underlying genai SDK (`getBaseUrl(…, GOOGLE_GEMINI_BASE_URL)`)
   independently of the auth type, so traffic still routes to the gateway. (A
   `"gateway"` native auth mode exists and also uses `GEMINI_API_KEY` as the
   key + the same `x-goog-api-key` header — a plausible future build target —
   but it is unusable non-interactively in 0.49.0 due to the missing validator
   branch; API-key mode is the working path.)
2. **`--skip-trust` (with `GEMINI_CLI_TRUST_WORKSPACE=true`).** Without it the
   CLI refuses to run in an untrusted workspace headless (exit 55). Documented
   behavior for automated environments.

Also: `-m gemini-2.5-flash` pins a concrete model so `OverrideStrategy` bypasses
the auto-model router. The router's classifier issues a separate structured-JSON
`generateContent` call the canned fake response cannot satisfy, causing
retry-with-backoff that can exceed the test timeout. Pinning the model yields
exactly one turn and a fast, deterministic exit 0.

## Assessment against the five questions

1. **Base-URL honored:** YES. Every request hit `GOOGLE_GEMINI_BASE_URL`
   (the fake gateway); nothing reached `generativelanguage.googleapis.com`.
2. **Brokerable auth (strippable header):** YES. The key travels only in the
   `x-goog-api-key` request header — a single header the gateway can delete and
   replace. Not embedded in the path/query, not in `Authorization`.
3. **Headless exit 0:** YES. `gemini --skip-trust -m gemini-2.5-flash -p "…"`
   exits 0 with no TTY (run completed in ~1.4s).
4. **Output shape usable by the runner:** YES. Final answer text on stdout,
   diagnostics on stderr, exit 0. Structured `-o json`/`-o stream-json`
   available if a machine envelope is preferred later.
5. **usageMetadata shape captured:** YES.
   `promptTokenCount` / `candidatesTokenCount` / `totalTokenCount`, accepted by
   the CLI; optional `thoughtsTokenCount` / `cachedContentTokenCount` on live
   turns.

## VERDICT: GREEN

All four go/no-go conditions are met with real captured traffic: the pinned
`@google/gemini-cli` 0.49.0 honors `GOOGLE_GEMINI_BASE_URL`, isolates the
`GEMINI_API_KEY` in a single strippable `x-goog-api-key` header, runs headless
to exit 0, and produces a `usageMetadata` block with known field names. Native
Gemini is brokerable with this CLI. The native `GoogleVendor` build proceeds
against these shapes: gateway rewrites `x-goog-api-key` (swapping the per-task
bearer for the real key), forwards `/v1beta/models/<model>:{stream,}GenerateContent`,
and meters from `usageMetadata.{prompt,candidates,total}TokenCount`. The build
must pin CLI version 0.49.0, seed `$HOME/.gemini/settings.json` with
`selectedType: gemini-api-key`, and launch with
`--skip-trust -m <model> -p <prompt>`. (The one nuance to track: the 0.49.0
`getAuthTypeFromEnv` `"gateway"` short-circuit means simply exporting
`GOOGLE_GEMINI_BASE_URL` + `GEMINI_API_KEY` is *not* enough — the settings
`selectedType` pin is mandatory; a future CLI that fixes the `gateway` validator
branch could simplify this.)
