package gwcreds

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// The two constructors differ only in which vendor refresh grant they wire in.
// A copy-paste bug wiring Anthropic's refresh into the Codex credential (or vice
// versa) would silently send a refresh token to the wrong OAuth endpoint, so
// these tests pin the wired refresh func by identity and confirm a fresh token
// is served without any refresh (no network).

func funcPtr(fn any) uintptr { return reflect.ValueOf(fn).Pointer() }

func TestNewOAuthCred_WiresAnthropicRefreshAndServesFreshToken(t *testing.T) {
	snap := CredSnapshot{Access: "acc-anthropic", Refresh: "ref", Expiry: time.Now().Add(time.Hour)}
	c := NewOAuthCred(snap, FileCredStore(filepath.Join(t.TempDir(), "c.json")))

	if c == nil {
		t.Fatal("NewOAuthCred returned nil")
	}
	if funcPtr(c.refresh) != funcPtr(refreshAnthropic) {
		t.Error("NewOAuthCred did not wire refreshAnthropic")
	}
	// Fresh token (an hour out, well past oauthRefreshMargin): Current returns
	// the access token directly and must not attempt a refresh.
	got, err := c.Current()
	if err != nil {
		t.Fatalf("Current on a fresh token: %v", err)
	}
	if got != "acc-anthropic" {
		t.Errorf("Current = %q, want the snapshot access token", got)
	}
}

func TestNewOAuthCredCodex_WiresOpenAIRefresh(t *testing.T) {
	snap := CredSnapshot{Access: "acc-codex", Refresh: "ref", Expiry: time.Now().Add(time.Hour)}
	c := NewOAuthCredCodex(snap, NewCodexStore(filepath.Join(t.TempDir(), "codex.json")))

	if c == nil {
		t.Fatal("NewOAuthCredCodex returned nil")
	}
	if funcPtr(c.refresh) != funcPtr(refreshOpenAI) {
		t.Error("NewOAuthCredCodex did not wire refreshOpenAI")
	}
	if funcPtr(c.refresh) == funcPtr(refreshAnthropic) {
		t.Error("NewOAuthCredCodex wired the Anthropic refresh grant")
	}
	got, err := c.Current()
	if err != nil {
		t.Fatalf("Current on a fresh token: %v", err)
	}
	if got != "acc-codex" {
		t.Errorf("Current = %q, want the snapshot access token", got)
	}
}
