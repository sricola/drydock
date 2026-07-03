package gateway

import (
	"net/http"
	"testing"
)

func TestGoogleVendor_InjectSwapsKey(t *testing.T) {
	v := GoogleVendor()
	if v.Name != "google" || v.BaseURL != "https://generativelanguage.googleapis.com" {
		t.Fatalf("unexpected vendor identity: %+v", v)
	}
	r, _ := http.NewRequest("POST",
		"https://gw/v1beta/models/gemini-2.5-pro:generateContent?key=tok_bearer&alt=sse", nil)
	r.Header.Set("X-Goog-Api-Key", "tok_bearer") // inbound = per-task bearer
	r.Header.Set("Authorization", "Bearer tok_bearer")

	v.Inject(r, "REAL-KEY")

	if got := r.Header.Get("X-Goog-Api-Key"); got != "REAL-KEY" {
		t.Errorf("x-goog-api-key = %q, want REAL-KEY", got)
	}
	if r.Header.Get("Authorization") != "" {
		t.Errorf("inbound Authorization must be removed, got %q", r.Header.Get("Authorization"))
	}
	if r.URL.Query().Has("key") {
		t.Errorf("key= query param must be stripped, got %q", r.URL.RawQuery)
	}
	if !r.URL.Query().Has("alt") {
		t.Errorf("other query params must be preserved (alt=sse)")
	}
}

func TestStaticKey_Current(t *testing.T) {
	var c Credential = StaticKey("sk-ant-abc")
	got, err := c.Current()
	if err != nil || got != "sk-ant-abc" {
		t.Fatalf("Current() = %q, %v; want sk-ant-abc, nil", got, err)
	}
}

func TestAnthropicOAuthVendor_Inject(t *testing.T) {
	r, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	r.Header.Set("X-Api-Key", "leftover")
	AnthropicOAuthVendor().Inject(r, "oauth-access-123")
	if r.Header.Get("X-Api-Key") != "" {
		t.Error("X-Api-Key not removed")
	}
	if r.Header.Get("Authorization") != "Bearer oauth-access-123" {
		t.Errorf("Authorization=%q", r.Header.Get("Authorization"))
	}
	if r.Header.Get("anthropic-beta") != anthropicOAuthBeta {
		t.Errorf("beta=%q", r.Header.Get("anthropic-beta"))
	}
}

func TestOpenAIOAuthVendor_Inject(t *testing.T) {
	r, _ := http.NewRequest("POST", "https://chatgpt.com/backend-api/codex/responses", nil)
	r.Header.Set("X-Api-Key", "leftover")
	r.Header.Set("originator", "codex_cli_rs")
	r.Header.Set("User-Agent", "codex_cli_rs/0.141.0")
	OpenAIOAuthVendor("acc-123").Inject(r, "oauth-access-xyz")
	if r.Header.Get("X-Api-Key") != "" {
		t.Error("X-Api-Key not removed")
	}
	if r.Header.Get("Authorization") != "Bearer oauth-access-xyz" {
		t.Errorf("Authorization=%q", r.Header.Get("Authorization"))
	}
	if r.Header.Get("chatgpt-account-id") != "acc-123" {
		t.Errorf("account id=%q", r.Header.Get("chatgpt-account-id"))
	}
	if r.Header.Get("originator") != "codex_cli_rs" || r.Header.Get("User-Agent") != "codex_cli_rs/0.141.0" {
		t.Error("originator/User-Agent must be preserved (403 risk)")
	}
}
