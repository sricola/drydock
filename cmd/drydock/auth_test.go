package main

import (
	"testing"
	"time"
)

func TestParseClaudeCreds(t *testing.T) {
	raw := []byte(`{"claudeAiOauth":{"accessToken":"a1","refreshToken":"r1","expiresAt":1750000000000}}`)
	snap, err := parseClaudeCreds(raw)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Access != "a1" || snap.Refresh != "r1" {
		t.Fatalf("snap=%+v", snap)
	}
	// expiresAt 1750000000000 ms → time.UnixMilli(1750000000000)
	want := time.UnixMilli(1750000000000)
	if !snap.Expiry.Equal(want) {
		t.Fatalf("snap.Expiry = %v, want %v", snap.Expiry, want)
	}
}

func TestParseClaudeCreds_NotLoggedIn(t *testing.T) {
	if _, err := parseClaudeCreds([]byte(`{}`)); err == nil {
		t.Error("want error for empty creds")
	}
}

func TestParseClaudeCreds_EmptyAccessToken(t *testing.T) {
	raw := []byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"r1","expiresAt":1750000000000}}`)
	if _, err := parseClaudeCreds(raw); err == nil {
		t.Error("want error for empty accessToken")
	}
}
