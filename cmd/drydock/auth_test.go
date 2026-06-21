package main

import (
	"encoding/base64"
	"fmt"
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

func makeJWTExp(exp int64) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"chatgpt_account_id":"SHOULD_NOT_BE_LOGGED"}`, exp)))
	return "h." + payload + ".s"
}

func TestParseCodexCreds(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	raw := []byte(fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"access_token":%q,"refresh_token":"r1","account_id":"acc-uuid"}}`, makeJWTExp(exp)))
	snap, account, err := parseCodexCreds(raw)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Refresh != "r1" || account != "acc-uuid" {
		t.Fatalf("snap=%+v account=%q", snap, account)
	}
	if d := snap.Expiry.Unix() - exp; d < -2 || d > 2 {
		t.Errorf("expiry from JWT exp wrong: %v vs %v", snap.Expiry.Unix(), exp)
	}
}

func TestParseCodexCreds_NotLoggedIn(t *testing.T) {
	if _, _, err := parseCodexCreds([]byte(`{"tokens":{}}`)); err == nil {
		t.Error("want error when no access token")
	}
}

func TestJWTExpiry_Malformed(t *testing.T) {
	if _, err := jwtExpiry("not-a-jwt"); err == nil {
		t.Error("want error for non-JWT")
	}
}
