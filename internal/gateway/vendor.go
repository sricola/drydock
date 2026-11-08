// Package-level: an upstream API description.
package gateway

import (
	"net/http"
	"net/url"
	"strings"
)

// Route is one permitted (method, path-prefix) pair in a vendor's proxy
// allowlist. An empty Method matches any method; matching is by path prefix.
type Route struct {
	Method string
	Prefix string
	// Actions, when set, turns Prefix into a Google-style
	// `{Prefix}{model}:{action}` matcher: the model must be exactly one path
	// segment and the action must be listed. It exists because a directory
	// prefix alone would also admit `:batchGenerateContent`, an asynchronous
	// batch job whose spend the response meter never observes (F-03).
	Actions []string
}

// Vendor describes one upstream API: where it lives, how to authenticate to it
// with the real key, and how to read token usage out of its responses.
type Vendor struct {
	Name       string
	BaseURL    string
	Inject     func(r *http.Request, realKey string)
	ParseUsage func(body []byte, contentType string) (Usage, bool)
	Prices     map[string]Price
	// StripFields are top-level JSON request fields the gateway removes before
	// forwarding. The OAuth/subscription endpoint strict-validates the request
	// and 400s on "extra inputs" that Claude Code sends in API mode (e.g.
	// context_management); the API-key endpoint accepts them. Empty = no rewrite.
	StripFields []string
	// BasePath is non-empty only for the Codex subscription backend; the director
	// joins it onto the inbound path. Empty (the default) means the path is
	// forwarded byte-identical, preserving existing Anthropic and OpenAI behaviour.
	BasePath string
	// AllowedRoutes scopes the injected host credential to a vendor's inference
	// routes. The gateway forwards a request only if its (method, path) matches
	// one of these; everything else gets a gateway-local 403 without contacting
	// upstream. This stops a task from using the credential for control-plane
	// operations (file upload/download/delete, fine-tuning, model management,
	// org/admin) that response-usage metering does not see. Empty means
	// unrestricted, used for operator-configured OpenAI-compat endpoints where the
	// path grammar is the operator's own server, not a shared vendor origin.
	AllowedRoutes []Route
}

// routeAllowed reports whether (method, path) is permitted by this vendor's
// AllowedRoutes. An empty allowlist is unrestricted (backward compatible).
func (v Vendor) routeAllowed(method, path string) bool {
	if len(v.AllowedRoutes) == 0 {
		return true
	}
	for _, rt := range v.AllowedRoutes {
		if rt.Method != "" && rt.Method != method {
			continue
		}
		if len(rt.Actions) > 0 {
			if actionRouteMatch(path, rt.Prefix, rt.Actions) {
				return true
			}
			continue
		}
		if routeMatch(path, rt.Prefix) {
			return true
		}
	}
	return false
}

// actionRouteMatch matches `{prefix}{model}:{action}` where model is one
// non-empty path segment and action is one of actions. Nothing may follow the
// action.
func actionRouteMatch(path, prefix string, actions []string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := path[len(prefix):]
	model, action, ok := strings.Cut(rest, ":")
	if !ok || model == "" || strings.Contains(model, "/") || strings.Contains(action, "/") {
		return false
	}
	for _, a := range actions {
		if action == a {
			return true
		}
	}
	return false
}

// routeMatch matches path against a route pattern. A pattern with a trailing
// slash is a directory prefix (matches any sub-path). One without matches ONLY
// the exact path: sub-resources are never implied, because a sub-resource can
// carry authority the parent's metering never sees, e.g. POST
// /v1/messages/batches creates up to 100k Message requests whose spend the
// response-usage meter cannot observe (F-03). Sub-resources a pinned CLI
// actually needs are allowlisted explicitly.
func routeMatch(path, prefix string) bool {
	if strings.HasSuffix(prefix, "/") {
		// A directory prefix covers retrieve-style sub-paths only. A ":" in
		// the remainder is a Google-style action (…/model:batchGenerateContent)
		// and is never implied by a directory route; actions are allowlisted
		// explicitly through Route.Actions.
		return strings.HasPrefix(path, prefix) && !strings.Contains(path[len(prefix):], ":")
	}
	return path == prefix
}

// Credential is the host-held secret the gateway injects upstream. Never seen by the VM.
type Credential interface{ Current() (string, error) }

// StaticKey is a fixed API key (the existing path).
type StaticKey string

func (k StaticKey) Current() (string, error) { return string(k), nil }

// Backend pairs a Vendor with the credential the gateway resolves per request (host-only).
type Backend struct {
	Vendor Vendor
	Cred   Credential
}

// AnthropicVendor is the api.anthropic.com upstream: X-Api-Key auth +
// anthropic-version header, Claude usage shapes, Claude prices.
func AnthropicVendor() Vendor {
	return Vendor{
		Name:    "anthropic",
		BaseURL: "https://api.anthropic.com",
		Inject: func(r *http.Request, realKey string) {
			r.Header.Del("Authorization")
			r.Header.Set("X-Api-Key", realKey)
			if r.Header.Get("anthropic-version") == "" {
				r.Header.Set("anthropic-version", "2023-06-01")
			}
		},
		ParseUsage: parseAnthropicUsage,
		Prices:     AnthropicPrices(),
		// Claude Code posts inference to /v1/messages, counts context with
		// /v1/messages/count_tokens, and may list or retrieve models.
		// /v1/messages/batches is deliberately NOT here (F-03).
		AllowedRoutes: []Route{
			{Method: "POST", Prefix: "/v1/messages"},
			{Method: "POST", Prefix: "/v1/messages/count_tokens"},
			{Method: "GET", Prefix: "/v1/models"},
			{Method: "GET", Prefix: "/v1/models/"},
		},
	}
}

// AnthropicOAuthVendor is the api.anthropic.com upstream using Claude subscription
// OAuth auth: Bearer auth + anthropic-beta header, removes X-Api-Key, Claude usage
// shapes and prices (reused from AnthropicVendor).
func AnthropicOAuthVendor() Vendor {
	v := AnthropicVendor()
	v.Inject = func(r *http.Request, secret string) {
		r.Header.Del("X-Api-Key")
		r.Header.Set("Authorization", "Bearer "+secret)
		r.Header.Set("anthropic-beta", anthropicOAuthBeta)
		if r.Header.Get("anthropic-version") == "" {
			r.Header.Set("anthropic-version", "2023-06-01")
		}
	}
	// Claude Code (API mode) sends context_management, which the OAuth endpoint
	// rejects ("Extra inputs are not permitted"); strip it for subscription
	// requests. Validated against a real Max account: removing this one field
	// flips the request from 400 to 200.
	v.StripFields = []string{"context_management"}
	return v
}

// OpenAIVendor is the api.openai.com upstream: bearer auth, OpenAI usage
// shapes (Responses + Chat Completions), OpenAI prices.
func OpenAIVendor() Vendor {
	return Vendor{
		Name:    "openai",
		BaseURL: "https://api.openai.com",
		Inject: func(r *http.Request, realKey string) {
			r.Header.Del("X-Api-Key")
			r.Header.Set("Authorization", "Bearer "+realKey)
		},
		ParseUsage: parseOpenAIUsage,
		Prices:     OpenAIPrices(),
		// Codex/Chat clients post to /v1/responses or /v1/chat/completions and
		// may list /v1/models. File/upload/fine-tuning/etc. are not forwarded.
		AllowedRoutes: []Route{
			{Method: "POST", Prefix: "/v1/chat/completions"},
			{Method: "POST", Prefix: "/v1/responses"},
			{Method: "GET", Prefix: "/v1/models"},
			{Method: "GET", Prefix: "/v1/models/"},
		},
	}
}

// GoogleVendor is the generativelanguage.googleapis.com upstream: Google's
// x-goog-api-key auth, Gemini usageMetadata shapes, Gemini prices. The VM's
// Gemini CLI (API-key mode) sends the per-task bearer in x-goog-api-key; the
// gateway admits it (see ServeHTTP) and this Inject swaps in the real key.
func GoogleVendor() Vendor {
	return Vendor{
		Name:    "google",
		BaseURL: "https://generativelanguage.googleapis.com",
		Inject: func(r *http.Request, realKey string) {
			r.Header.Del("Authorization")
			r.Header.Del("X-Goog-Api-Key")
			// Defensive: the CLI uses the header, but strip any ?key= so a
			// per-task bearer can never leak upstream in the query string.
			stripQueryParam(r.URL, "key")
			r.Header.Set("X-Goog-Api-Key", realKey)
		},
		ParseUsage: parseGoogleUsage,
		Prices:     GooglePrices(),
		// The Gemini CLI posts to /v1beta/models/{model}:generateContent (and
		// :streamGenerateContent, :countTokens, :embedContent), and may list or
		// retrieve models. The POST actions are explicit so the batch job
		// routes (:batchGenerateContent and friends) are refused locally.
		AllowedRoutes: []Route{
			{Method: "POST", Prefix: "/v1beta/models/", Actions: []string{
				"generateContent", "streamGenerateContent", "countTokens", "embedContent"}},
			{Method: "GET", Prefix: "/v1beta/models"},
			{Method: "GET", Prefix: "/v1beta/models/"},
			{Method: "GET", Prefix: "/v1/models"},
		},
	}
}

// stripQueryParam removes one query key from u in place, preserving the rest.
func stripQueryParam(u *url.URL, key string) {
	if u == nil || u.RawQuery == "" {
		return
	}
	q := u.Query()
	if q.Has(key) {
		q.Del(key)
		u.RawQuery = q.Encode()
	}
}

// OpenAICompatVendor is a config-driven OpenAI-compatible upstream (Gemini's
// /v1beta/openai endpoint, OpenRouter, a local server, …). It reuses OpenAI's
// bearer auth and usage parsing; the operator supplies the base URL, an
// optional base path joined onto the inbound path, and optional prices for USD
// metering. Constructed by brokerd from config (not a static registry row).
func OpenAICompatVendor(name, baseURL, basePath string, prices map[string]Price) Vendor {
	v := OpenAIVendor()
	v.Name = name
	v.BaseURL = baseURL
	v.BasePath = basePath
	v.Prices = prices
	// Unrestricted: the operator points this at their own OpenAI-compatible
	// endpoint (Gemini's /v1beta/openai, OpenRouter's /api/v1, a local server),
	// whose path grammar varies and is not a shared vendor origin with
	// control-plane routes. The route allowlist targets the shared origins.
	v.AllowedRoutes = nil
	return v
}

// OpenAIOAuthVendor is the ChatGPT-subscription Codex backend: Bearer OAuth +
// chatgpt-account-id, served at chatgpt.com/backend-api/codex. accountID is
// captured once at bootstrap and is constant across token refreshes. The VM's
// real Codex CLI supplies originator/User-Agent and store:false/stream:true;
// this vendor must not disturb them.
func OpenAIOAuthVendor(accountID string) Vendor {
	v := OpenAIVendor()
	v.BaseURL = codexBackendBaseURL
	v.BasePath = codexBackendBasePath
	v.Inject = func(r *http.Request, secret string) {
		r.Header.Del("X-Api-Key")
		r.Header.Set("Authorization", "Bearer "+secret)
		r.Header.Set("chatgpt-account-id", accountID)
	}
	// The VM's Codex posts to {gateway}/responses (the director joins BasePath
	// onto it). Scope to that inbound path; the inherited OpenAI routes do not
	// cover bare /responses.
	v.AllowedRoutes = []Route{{Method: "POST", Prefix: "/responses"}, {Method: "POST", Prefix: "/v1/responses"}}
	return v
}
