package gateway

// OAuth + backend constants for Codex (ChatGPT subscription) auth, validated by
// the Task 1 spike (2026-06-20) against a real ChatGPT/Codex account:
//
//   - ACCESS (confirmed, HTTP 200): a forwarded POST to
//     <codexBackendBaseURL><codexBackendBasePath>/responses with
//     `Authorization: Bearer <oauth access token>` + `chatgpt-account-id` +
//     `originator: codex_cli_rs` + the Codex CLI User-Agent + `OpenAI-Beta:
//     responses=experimental`, body `store:false`/`stream:true`, returned a
//     streamed completion (`response.completed`). Subscription access works
//     through a gateway-forwarded request.
//
//   - REFRESH (endpoint/shape confirmed): the token endpoint + client_id +
//     `refresh_token` grant exchanges the refresh token for a new access token;
//     repeating the access request with the NEW access token and the ORIGINAL
//     captured account_id still returns 200 (the documented "refresh strips
//     account_id" failure mode does NOT bite when account_id is preserved from
//     the credential file rather than re-derived from the refreshed token).
//
// These values are reverse-engineered from the Codex CLI and are NOT a stable
// public API. Treat upstream changes as expected maintenance.
const (
	openaiOAuthClientID  = "app_EMoamEEZ73f0CkXaXp7hrann" // confirmed Task 1
	codexBackendBaseURL  = "https://chatgpt.com"          // host only; path via BasePath
	codexBackendBasePath = "/backend-api/codex"           // confirmed Task 1
)

// openaiOAuthTokenURL is the OpenAI OAuth token endpoint. Declared as a var (not
// const) so tests can redirect to a local httptest server without hitting the
// real endpoint, mirroring anthropicOAuthTokenURL. Never mutated outside tests.
var openaiOAuthTokenURL = "https://auth.openai.com/oauth/token" // confirmed Task 1
