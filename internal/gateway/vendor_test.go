package gateway

import (
	"net/http"
	"testing"
)

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
