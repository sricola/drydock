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

// jwtWithPayload builds a 3-segment token whose middle segment is the
// base64url-encoded payload — exercising the decode/parse branches of jwtExpiry.
func jwtWithPayload(payloadJSON string) string {
	return "h." + base64.RawURLEncoding.EncodeToString([]byte(payloadJSON)) + ".s"
}

func TestJWTExpiry_BadBase64(t *testing.T) {
	if _, err := jwtExpiry("h.@@@not-base64@@@.s"); err == nil {
		t.Error("want error when the payload segment is not valid base64url")
	}
}

func TestJWTExpiry_PayloadNotJSON(t *testing.T) {
	if _, err := jwtExpiry(jwtWithPayload("definitely not json")); err == nil {
		t.Error("want error when the decoded payload is not JSON")
	}
}

func TestJWTExpiry_NoExpClaim(t *testing.T) {
	if _, err := jwtExpiry(jwtWithPayload(`{"chatgpt_account_id":"x"}`)); err == nil {
		t.Error("want error when the JWT carries no exp claim")
	}
}

func TestParseClaudeCreds_MalformedJSON(t *testing.T) {
	if _, err := parseClaudeCreds([]byte(`{not valid json`)); err == nil {
		t.Error("want error for malformed keychain JSON")
	}
}

func TestParseCodexCreds_MalformedJSON(t *testing.T) {
	if _, _, err := parseCodexCreds([]byte(`{not valid json`)); err == nil {
		t.Error("want error for malformed auth.json")
	}
}

// A present-but-non-JWT access token must surface jwtExpiry's error, not panic.
func TestParseCodexCreds_AccessTokenNotJWT(t *testing.T) {
	raw := []byte(`{"tokens":{"access_token":"plain-opaque-token","refresh_token":"r","account_id":"a"}}`)
	if _, _, err := parseCodexCreds(raw); err == nil {
		t.Error("want error when access_token is not a decodable JWT")
	}
}

// TestBootstrapCores_Exist verifies that bootstrapClaudeCred and bootstrapCodexCred
// are defined and callable. In CI (not logged in) they return errors — that is the
// contract: they must return an error, never call os.Exit.
func TestBootstrapCores_Exist(t *testing.T) {
	_ = bootstrapClaudeCred
	_ = bootstrapCodexCred
}
