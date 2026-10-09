package gwcreds

import (
	"errors"
	"testing"
	"time"
)

// The re-import hook models the Claude Code Keychain: the shared grant the
// operator copied at `drydock auth claude`, which Claude Code itself keeps
// refreshing (rotating the single-use refresh token). These tests pin the
// order of precedence: Keychain first, own refresh second, disk recovery last.

func TestOAuthCred_ReimportAdoptsRotatedKeychainTokenWithoutRefreshing(t *testing.T) {
	store := &memStore{}
	c := &OAuthCred{
		snap:  CredSnapshot{Access: "mem-old", Refresh: "r1", Expiry: time.Now().Add(30 * time.Second)},
		store: store,
		refresh: func(string) (CredSnapshot, error) {
			t.Fatal("must not call the token endpoint when the Keychain already holds a rotated, valid token")
			return CredSnapshot{}, nil
		},
	}
	c.SetReimport(func() (CredSnapshot, bool) {
		return CredSnapshot{Access: "kc-new", Refresh: "r2", Expiry: time.Now().Add(time.Hour)}, true
	})
	got, err := c.Current()
	if err != nil || got != "kc-new" {
		t.Fatalf("Current=%q,%v want kc-new", got, err)
	}
	if c.snap.Refresh != "r2" || store.saved.Refresh != "r2" {
		t.Errorf("rotated Keychain token not adopted/persisted: mem=%q disk=%q", c.snap.Refresh, store.saved.Refresh)
	}
}

func TestOAuthCred_ReimportRefreshesWithKeychainTokenWhenBothStale(t *testing.T) {
	store := &memStore{}
	c := &OAuthCred{
		snap:  CredSnapshot{Access: "mem-old", Refresh: "r1", Expiry: time.Now().Add(-time.Minute)},
		store: store,
		refresh: func(r string) (CredSnapshot, error) {
			if r == "r1" {
				return CredSnapshot{}, errors.New("token endpoint returned 400")
			}
			return CredSnapshot{Access: "fresh", Refresh: "r3", Expiry: time.Now().Add(time.Hour)}, nil
		},
	}
	// Keychain holds a DIFFERENT refresh token (Claude Code rotated it) that is
	// itself past the margin: refresh with that one, never with the dead r1.
	c.SetReimport(func() (CredSnapshot, bool) {
		return CredSnapshot{Access: "kc-old", Refresh: "r2", Expiry: time.Now().Add(-time.Minute)}, true
	})
	got, err := c.Current()
	if err != nil || got != "fresh" {
		t.Fatalf("Current=%q,%v want fresh", got, err)
	}
	if c.snap.Refresh != "r3" || store.saved.Access != "fresh" {
		t.Errorf("refreshed snapshot not adopted/persisted: %+v / %+v", c.snap, store.saved)
	}
}

func TestOAuthCred_ReimportIdenticalTokenFallsThroughToOwnRefresh(t *testing.T) {
	store := &memStore{}
	refreshed := false
	c := &OAuthCred{
		snap:  CredSnapshot{Access: "old", Refresh: "r1", Expiry: time.Now().Add(30 * time.Second)},
		store: store,
		refresh: func(r string) (CredSnapshot, error) {
			refreshed = true
			if r != "r1" {
				t.Fatalf("refresh used %q, want r1", r)
			}
			return CredSnapshot{Access: "new", Refresh: "r2", Expiry: time.Now().Add(time.Hour)}, nil
		},
	}
	// Keychain has exactly our grant: nobody rotated it, so we are first.
	c.SetReimport(func() (CredSnapshot, bool) {
		return CredSnapshot{Access: "old", Refresh: "r1", Expiry: time.Now().Add(30 * time.Second)}, true
	})
	if got, err := c.Current(); err != nil || got != "new" {
		t.Fatalf("Current=%q,%v want new", got, err)
	}
	if !refreshed {
		t.Error("own refresh must run when the Keychain holds the same grant")
	}
}

func TestOAuthCred_ReimportNotOKFallsThroughToOwnRefresh(t *testing.T) {
	c := &OAuthCred{
		snap:  CredSnapshot{Access: "old", Refresh: "r1", Expiry: time.Now().Add(30 * time.Second)},
		store: &memStore{},
		refresh: func(string) (CredSnapshot, error) {
			return CredSnapshot{Access: "new", Refresh: "r2", Expiry: time.Now().Add(time.Hour)}, nil
		},
	}
	c.SetReimport(func() (CredSnapshot, bool) { return CredSnapshot{}, false })
	if got, err := c.Current(); err != nil || got != "new" {
		t.Fatalf("Current=%q,%v want new", got, err)
	}
}

func TestOAuthCred_ReimportNotConsultedWhenFresh(t *testing.T) {
	c := &OAuthCred{
		snap:  CredSnapshot{Access: "tok", Refresh: "r1", Expiry: time.Now().Add(time.Hour)},
		store: &memStore{},
		refresh: func(string) (CredSnapshot, error) {
			t.Fatal("should not refresh")
			return CredSnapshot{}, nil
		},
	}
	c.SetReimport(func() (CredSnapshot, bool) {
		t.Fatal("should not consult the Keychain while the token is fresh")
		return CredSnapshot{}, false
	})
	if got, _ := c.Current(); got != "tok" {
		t.Errorf("Current=%q want tok", got)
	}
}
