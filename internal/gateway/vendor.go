// Package-level: an upstream API description.
package gateway

import "net/http"

// Vendor describes one upstream API: where it lives, how to authenticate to it
// with the real key, and how to read token usage out of its responses.
type Vendor struct {
	Name       string
	BaseURL    string
	Inject     func(r *http.Request, realKey string)
	ParseUsage func(body []byte, contentType string) (model string, in, out int, ok bool)
	Prices     map[string]Price
	// StripFields are top-level JSON request fields the gateway removes before
	// forwarding. The OAuth/subscription endpoint strict-validates the request
	// and 400s on "extra inputs" that Claude Code sends in API mode (e.g.
	// context_management); the API-key endpoint accepts them. Empty = no rewrite.
	StripFields []string
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
	}
}
