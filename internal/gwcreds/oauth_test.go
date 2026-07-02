package gwcreds

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// memStore is a test double for CredStore.
type memStore struct {
	snap      CredSnapshot
	saved     CredSnapshot
	saveCalls int
}

func (m *memStore) Load() (CredSnapshot, error) { return m.snap, nil }
func (m *memStore) Save(s CredSnapshot) error   { m.saved = s; m.saveCalls++; return nil }

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

// TestRefreshAnthropic_Success validates the full HTTP round-trip: correct
// request shape and correct CredSnapshot on a 200 response.
func TestRefreshAnthropic_Success(t *testing.T) {
	var (
		gotMethod      string
		gotContentType string
		gotBody        map[string]string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "A2",
			"refresh_token": "R2",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	orig := anthropicOAuthTokenURL
	anthropicOAuthTokenURL = srv.URL
	defer func() { anthropicOAuthTokenURL = orig }()

	before := time.Now()
	snap, err := refreshAnthropic("test-refresh-token")
	if err != nil {
		t.Fatalf("refreshAnthropic returned error: %v", err)
	}

	// Assert request shape.
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody["grant_type"] != "refresh_token" {
		t.Errorf("grant_type = %q, want refresh_token", gotBody["grant_type"])
	}
	if gotBody["refresh_token"] != "test-refresh-token" {
		t.Errorf("refresh_token field not forwarded correctly")
	}
	if gotBody["client_id"] != anthropicOAuthClientID {
		t.Errorf("client_id = %q, want %s", gotBody["client_id"], anthropicOAuthClientID)
	}

	// Assert returned snapshot.
	if snap.Access != "A2" {
		t.Errorf("Access = %q, want A2", snap.Access)
	}
	if snap.Refresh != "R2" {
		t.Errorf("Refresh = %q, want R2", snap.Refresh)
	}
	// Expiry should be approximately now+1h (within a 5s tolerance).
	expectedExpiry := before.Add(time.Hour)
	if snap.Expiry.Before(before) {
		t.Errorf("Expiry %v is before call time %v", snap.Expiry, before)
	}
	if snap.Expiry.After(expectedExpiry.Add(5 * time.Second)) {
		t.Errorf("Expiry %v is too far past expected %v", snap.Expiry, expectedExpiry)
	}
}

// TestRefreshAnthropic_NonOKError asserts that a non-200 response returns an
// error and that the error message does not contain the refresh token value.
func TestRefreshAnthropic_NonOKError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	orig := anthropicOAuthTokenURL
	anthropicOAuthTokenURL = srv.URL
	defer func() { anthropicOAuthTokenURL = orig }()

	const sensitiveToken = "super-secret-refresh"
	snap, err := refreshAnthropic(sensitiveToken)
	if err == nil {
		t.Fatal("expected error for 401 response, got nil")
	}
	// The error message must not leak the refresh token.
	if msg := err.Error(); contains(msg, sensitiveToken) {
		t.Errorf("error message leaks the refresh token value")
	}
	// Returned snapshot must be zero-value on error.
	if snap.Access != "" || snap.Refresh != "" {
		t.Errorf("non-empty snapshot returned on error: %+v", snap)
	}
}

// contains is a helper to check substring without importing strings at call site.
func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}()
}

// TestRefreshAnthropic_EmptyAccessToken asserts that a 200 response with an
// empty access_token is treated as an error.
func TestRefreshAnthropic_EmptyAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "",
			"refresh_token": "R2",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	orig := anthropicOAuthTokenURL
	anthropicOAuthTokenURL = srv.URL
	defer func() { anthropicOAuthTokenURL = orig }()

	snap, err := refreshAnthropic("any-token")
	if err == nil {
		t.Fatal("expected error for empty access_token, got nil")
	}
	if snap.Access != "" {
		t.Errorf("Access = %q on error, want empty", snap.Access)
	}
}

// TestExpiresInDuration_Clamps guards the expiry math against a hostile or
// buggy token endpoint: a huge expires_in must not overflow the int64-nanosecond
// Duration (which would wrap the sign and hand back a far-future or past expiry),
// and a non-positive value must clamp to 0 (already-expired) rather than valid
// forever.
func TestExpiresInDuration_Clamps(t *testing.T) {
	const maxSeconds = int(maxTokenLifetime / time.Second)
	cases := []struct {
		name    string
		seconds int
		want    time.Duration
	}{
		{"normal hour", 3600, time.Hour},
		{"zero clamps to expired", 0, 0},
		{"negative clamps to expired", -5, 0},
		{"at cap", maxSeconds, maxTokenLifetime},
		{"over cap clamps down", maxSeconds + 1, maxTokenLifetime},
		{"overflow-scale value clamps, never wraps negative", 1 << 62, maxTokenLifetime},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := expiresInDuration(tc.seconds)
			if got != tc.want {
				t.Errorf("expiresInDuration(%d) = %v, want %v", tc.seconds, got, tc.want)
			}
			if got < 0 {
				t.Errorf("expiresInDuration(%d) = %v is negative (overflow wrap)", tc.seconds, got)
			}
		})
	}
}

// TestRefreshAnthropic_AbsurdExpiresIn confirms the clamp is wired into the real
// refresh path: a wildly large expires_in yields a bounded, future expiry rather
// than an overflowed (possibly past) one.
func TestRefreshAnthropic_AbsurdExpiresIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "A2",
			"refresh_token": "R2",
			"expires_in":    int64(1) << 62,
		})
	}))
	defer srv.Close()

	orig := anthropicOAuthTokenURL
	anthropicOAuthTokenURL = srv.URL
	defer func() { anthropicOAuthTokenURL = orig }()

	before := time.Now()
	snap, err := refreshAnthropic("any-token")
	if err != nil {
		t.Fatalf("refreshAnthropic returned error: %v", err)
	}
	if !snap.Expiry.After(before) {
		t.Errorf("Expiry %v is not in the future (overflow wrapped it)", snap.Expiry)
	}
	if snap.Expiry.After(before.Add(maxTokenLifetime + 5*time.Second)) {
		t.Errorf("Expiry %v exceeds the clamp ceiling", snap.Expiry)
	}
}

// TestFileCredStore_RoundTripAndPerms writes a snapshot via FileCredStore,
// loads it back, asserts field equality, and checks the file mode is 0600.
func TestFileCredStore_RoundTripAndPerms(t *testing.T) {
	path := t.TempDir() + "/creds.json"
	store := FileCredStore(path)

	expiry := time.Now().Add(time.Hour).Truncate(time.Second) // truncate for JSON precision
	original := CredSnapshot{
		Access:  "acc-token-123",
		Refresh: "ref-token-456",
		Expiry:  expiry,
	}

	if err := store.Save(original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.Access != original.Access {
		t.Errorf("Access: got %q, want %q", loaded.Access, original.Access)
	}
	if loaded.Refresh != original.Refresh {
		t.Errorf("Refresh: got %q, want %q", loaded.Refresh, original.Refresh)
	}
	if !loaded.Expiry.Equal(original.Expiry) {
		t.Errorf("Expiry: got %v, want %v", loaded.Expiry, original.Expiry)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %04o, want 0600", mode)
	}
}

// TestOAuthCred_RefreshFailure asserts that when the refresh func returns an
// error, Current() propagates that error, the in-memory access token is not
// corrupted, and the store's Save is not called.
func TestOAuthCred_RefreshFailure(t *testing.T) {
	store := &memStore{}
	originalAccess := "still-valid-access"
	c := &OAuthCred{
		// Expiry in the past forces a refresh attempt.
		snap:  CredSnapshot{Access: originalAccess, Refresh: "r-old", Expiry: time.Now().Add(-time.Minute)},
		store: store,
		refresh: func(string) (CredSnapshot, error) {
			return CredSnapshot{}, errors.New("upstream refresh failed")
		},
	}

	tok, err := c.Current()
	if err == nil {
		t.Fatal("expected error from Current() when refresh fails, got nil")
	}
	if tok != "" {
		t.Errorf("Current() returned token %q on refresh error, want empty string", tok)
	}
	// In-memory snap must not have been corrupted.
	if c.snap.Access != originalAccess {
		t.Errorf("in-memory access token changed to %q after refresh failure", c.snap.Access)
	}
	// Store must not have been written.
	if store.saveCalls != 0 {
		t.Errorf("Save was called %d time(s) after a refresh failure, want 0", store.saveCalls)
	}
}
