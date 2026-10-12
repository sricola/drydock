package gwcreds

import (
	"errors"
	"testing"
	"time"
)

// TestOAuthCred_RecoverFromDisk_KeepsDiskRefreshWhenEndpointOmitsIt pins the
// refresh-token bookkeeping on the disk-recovery path. When the in-memory
// token (r1) is dead because another process rotated the shared file to r2,
// recovery re-refreshes with r2. If the token endpoint answers WITHOUT a
// refresh_token (grants that do not rotate), the credential must keep r2,
// the live token it just used. Keeping r1, the token the endpoint already
// rejected, would persist a dead refresh token and wedge every later refresh
// on "token endpoint returned 400" until restart.
func TestOAuthCred_RecoverFromDisk_KeepsDiskRefreshWhenEndpointOmitsIt(t *testing.T) {
	store := &memStore{snap: CredSnapshot{Access: "disk-old", Refresh: "r2", Expiry: time.Now().Add(-time.Minute)}}
	c := &OAuthCred{
		snap:  CredSnapshot{Access: "mem-old", Refresh: "r1", Expiry: time.Now().Add(-time.Minute)},
		store: store,
		refresh: func(r string) (CredSnapshot, error) {
			if r != "r2" {
				return CredSnapshot{}, errors.New("stale token rejected")
			}
			// Endpoint returns a new access token but no refresh token.
			return CredSnapshot{Access: "fresh", Expiry: time.Now().Add(time.Hour)}, nil
		},
	}
	got, err := c.Current()
	if err != nil || got != "fresh" {
		t.Fatalf("Current()=%q,%v; want \"fresh\"", got, err)
	}
	if c.snap.Refresh != "r2" {
		t.Errorf("in-memory refresh = %q, want r2 (the disk token that just worked, not the rejected r1)", c.snap.Refresh)
	}
	if store.saved.Refresh != "r2" {
		t.Errorf("persisted refresh = %q, want r2", store.saved.Refresh)
	}
}
