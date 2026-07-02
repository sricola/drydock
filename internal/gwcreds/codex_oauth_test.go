package gwcreds

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexStore_RoundTripPreservesAccountID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex-oauth.json")
	s := NewCodexStore(path)
	if err := s.Put(CredSnapshot{Access: "a1", Refresh: "r1", Expiry: time.Now().Add(time.Hour)}, "acct-uuid"); err != nil {
		t.Fatal(err)
	}
	// A fresh store Loads the snapshot AND learns the account id.
	s2 := NewCodexStore(path)
	snap, err := s2.Load()
	if err != nil || snap.Access != "a1" {
		t.Fatalf("Load=%+v,%v", snap, err)
	}
	if s2.AccountID() != "acct-uuid" {
		t.Errorf("AccountID=%q want acct-uuid", s2.AccountID())
	}
	// A refresh-driven Save (no account id passed) must NOT drop it.
	if err := s2.Save(CredSnapshot{Access: "a2", Refresh: "r2", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	s3 := NewCodexStore(path)
	_, _ = s3.Load()
	if s3.AccountID() != "acct-uuid" {
		t.Errorf("account id lost on refresh Save: %q", s3.AccountID())
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode=%v want 0600", fi.Mode().Perm())
	}
}

func TestRefreshOpenAI_ParsesAndDoesNotLeak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["grant_type"] != "refresh_token" || body["client_id"] != openaiOAuthClientID || body["refresh_token"] != "r1" {
			t.Errorf("bad refresh body: %+v", body)
		}
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"r2","expires_in":3600}`))
	}))
	defer srv.Close()
	old := openaiOAuthTokenURL
	openaiOAuthTokenURL = srv.URL
	defer func() { openaiOAuthTokenURL = old }()

	snap, err := refreshOpenAI("r1")
	if err != nil || snap.Access != "new-access" || snap.Refresh != "r2" {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
	if time.Until(snap.Expiry) < 50*time.Minute {
		t.Errorf("expiry too soon: %v", snap.Expiry)
	}
}

func TestRefreshOpenAI_NonOKErrorHasNoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"rate_limited"}`))
	}))
	defer srv.Close()
	old := openaiOAuthTokenURL
	openaiOAuthTokenURL = srv.URL
	defer func() { openaiOAuthTokenURL = old }()

	_, err := refreshOpenAI("super-secret-refresh")
	if err == nil {
		t.Fatal("want error on 429")
	}
	if strings.Contains(err.Error(), "super-secret-refresh") {
		t.Error("refresh token leaked into error")
	}
}
