package gateway

import "time"

// OAuth constants for Claude subscription (Pro/Max) auth, validated by the
// Task 1 spike (2026-06-20) against a real Claude Max account:
//
//   - ACCESS (confirmed, HTTP 200): a forwarded POST /v1/messages with
//     `Authorization: Bearer <oauth access token>` + `anthropic-beta:
//     oauth-2025-04-20` + the Claude Code system-prompt identity returned a real
//     completion. This resolves the feature's critical unknown — subscription
//     access works through a gateway-forwarded request.
//
//   - REFRESH (endpoint/shape confirmed): the token endpoint + client_id +
//     JSON `refresh_token` grant returned a typed Anthropic response
//     (`rate_limit_error` 429 during the spike — i.e. the request was understood,
//     not a routing/credential error). A clean 200 refresh was not captured due
//     to a transient rate limit; it is exercised in normal operation by
//     `OAuthCred.Current` and surfaced by `drydock doctor`.
//
// These values are reverse-engineered from Claude Code and are NOT a stable
// public API. Treat upstream changes as expected maintenance (doctor surfaces
// breakage fast).
const (
	anthropicOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	anthropicOAuthBeta     = "oauth-2025-04-20"
)

// anthropicOAuthTokenURL is the Anthropic OAuth token endpoint. Declared as a
// var (not const) so tests can redirect to a local httptest server without
// hitting the real endpoint. The value is never mutated outside of tests.
var anthropicOAuthTokenURL = "https://console.anthropic.com/v1/oauth/token"

// oauthRefreshMargin is how close to expiry the gateway refreshes the access
// token. The credential file stores expiry; OAuthCred.Current refreshes when
// the remaining lifetime drops below this.
const oauthRefreshMargin = 2 * time.Minute
