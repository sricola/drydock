// Package gwcreds holds the OAuth credential store and refresh engine for
// drydock's subscription-auth providers (Claude Max, Codex/ChatGPT). The
// gateway proxy imports this package for the Credential implementations; the
// auth CLI imports it for persistence. gwcreds itself has no dependencies on
// internal/gateway, preventing import cycles.
package gwcreds

import "time"

// Anthropic OAuth constants, validated by the Task 1 spike (2026-06-20):
//
//   - The token endpoint + client_id + JSON refresh_token grant was confirmed
//     reachable (rate_limit_error 429, meaning the request shape was understood).
//   - A real Claude Max account's access token forwarded to api.anthropic.com
//     with anthropic-beta:oauth-2025-04-20 returned a completion (HTTP 200).
//
// Not a stable public API — treat upstream changes as expected maintenance.
const (
	anthropicOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	oauthRefreshMargin     = 2 * time.Minute
)

// anthropicOAuthTokenURL is the Anthropic OAuth token endpoint. Declared as a
// var (not const) so tests can redirect to a local httptest server. Never
// mutated outside of tests.
var anthropicOAuthTokenURL = "https://console.anthropic.com/v1/oauth/token"

// OpenAI / Codex OAuth constants, validated by the Task 1 spike (2026-06-20):
//
//   - The token endpoint + client_id + refresh_token grant was confirmed; a
//     refreshed access token + original account_id continued to return 200
//     (the documented "refresh strips account_id" failure mode does not bite
//     when account_id is preserved from the credential file).
//
// Not a stable public API — treat upstream changes as expected maintenance.
const openaiOAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann" // confirmed Task 1

// openaiOAuthTokenURL is the OpenAI OAuth token endpoint. Declared as a var
// (not const) so tests can redirect to a local httptest server. Never mutated
// outside of tests.
var openaiOAuthTokenURL = "https://auth.openai.com/oauth/token" // confirmed Task 1
