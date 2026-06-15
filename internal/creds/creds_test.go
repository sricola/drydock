package creds

import (
	"testing"
	"time"
)

func TestStaticProvider_MintReturnsKey(t *testing.T) {
	var p Provider = StaticProvider{Key: "sk-static"}
	tok, err := p.Mint(15 * time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if tok.Value != "sk-static" {
		t.Errorf("Value = %q, want sk-static", tok.Value)
	}
	if err := p.Revoke(tok); err != nil {
		t.Errorf("Revoke: %v", err)
	}
}

func TestStaticProvider_EmptyKeyErrors(t *testing.T) {
	p := StaticProvider{Key: ""}
	if _, err := p.Mint(time.Minute); err == nil {
		t.Errorf("Mint with empty key: want error, got nil")
	}
}
