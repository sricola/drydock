package gateway

// Codex backend routing constants used by OpenAIOAuthVendor. OAuth credential
// constants (client_id, token endpoint) moved to internal/gwcreds.
const (
	codexBackendBaseURL  = "https://chatgpt.com" // host only; path via BasePath
	codexBackendBasePath = "/backend-api/codex"  // confirmed Task 1
)
