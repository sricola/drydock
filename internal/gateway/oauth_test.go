package gateway

import (
	"testing"
	"time"
)

// memStore is a test double for CredStore.
type memStore struct {
	snap  CredSnapshot
	saved CredSnapshot
}

func (m *memStore) Load() (CredSnapshot, error) { return m.snap, nil }
func (m *memStore) Save(s CredSnapshot) error   { m.saved = s; return nil }

func TestOAuthCred_RefreshesWhenExpiring(t *testing.T) {
	store := &memStore{}
	c := &OAuthCred{
		snap:  CredSnapshot{Access: "old", Refresh: "r1", Expiry: time.Now().Add(30 * time.Second)},
		store: store,
		refresh: func(r string) (CredSnapshot, error) {
			if r != "r1" {
				t.Fatalf("refresh used %q", r)
			}
			return CredSnapshot{Access: "new", Refresh: "r2", Expiry: time.Now().Add(time.Hour)}, nil
		},
	}
	got, err := c.Current()
	if err != nil || got != "new" {
		t.Fatalf("Current=%q,%v want new", got, err)
	}
	if store.saved.Refresh != "r2" {
		t.Errorf("rotated refresh not persisted: %q", store.saved.Refresh)
	}
}

func TestOAuthCred_NoRefreshWhenFresh(t *testing.T) {
	c := &OAuthCred{
		snap:  CredSnapshot{Access: "tok", Expiry: time.Now().Add(time.Hour)},
		store: &memStore{},
		refresh: func(string) (CredSnapshot, error) {
			t.Fatal("should not refresh")
			return CredSnapshot{}, nil
		},
	}
	got, _ := c.Current()
	if got != "tok" {
		t.Errorf("Current=%q want tok", got)
	}
}
