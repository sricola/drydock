package gwcreds

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// CredSnapshot holds the OAuth token pair and its expiry.
type CredSnapshot struct {
	Access  string    `json:"access_token"`
	Refresh string    `json:"refresh_token"`
	Expiry  time.Time `json:"expiry"`
}

// CredStore persists and retrieves a CredSnapshot.
type CredStore interface {
	Load() (CredSnapshot, error)
	Save(CredSnapshot) error
}

// fileStore implements CredStore backed by a JSON file at mode 0600.
type fileStore struct{ path string }

// FileCredStore returns a CredStore that reads/writes JSON at path with mode 0600.
func FileCredStore(path string) CredStore { return &fileStore{path: path} }

func (f *fileStore) Load() (CredSnapshot, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return CredSnapshot{}, err
	}
	var s CredSnapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return CredSnapshot{}, err
	}
	return s, nil
}

func (f *fileStore) Save(s CredSnapshot) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.path)
}

// OAuthCred satisfies gateway.Credential (via structural typing) for a Claude
// subscription OAuth token.
// It refreshes the access token when within oauthRefreshMargin of expiry and
// persists the rotated snapshot via the store.
type OAuthCred struct {
	mu      sync.Mutex
	snap    CredSnapshot
	store   CredStore
	refresh func(refreshToken string) (CredSnapshot, error)
}

// NewOAuthCred creates an OAuthCred with refreshAnthropic as the default refresh func.
func NewOAuthCred(snap CredSnapshot, store CredStore) *OAuthCred {
	return &OAuthCred{
		snap:    snap,
		store:   store,
		refresh: refreshAnthropic,
	}
}

// Current returns the current access token, refreshing and persisting when near expiry.
func (c *OAuthCred) Current() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if time.Until(c.snap.Expiry) <= oauthRefreshMargin {
		newSnap, err := c.refresh(c.snap.Refresh)
		if err != nil {
			// Our in-memory refresh token may have been rotated out from under
			// us by another process sharing this credential file (drydock
			// doctor, drydock auth, a second broker). Reload from disk before
			// giving up: without this, one external rotation wedges a
			// long-running brokerd on 502 credential unavailable until restart.
			recovered, ok := c.recoverFromDisk()
			if !ok {
				return "", fmt.Errorf("oauth: refresh failed: %w", err)
			}
			newSnap = recovered
		}
		// If the new snapshot has no refresh token, keep the old one.
		if newSnap.Refresh == "" {
			newSnap.Refresh = c.snap.Refresh
		}
		c.snap = newSnap
		if err := c.store.Save(c.snap); err != nil {
			return "", fmt.Errorf("oauth: persist failed: %w", err)
		}
	}

	return c.snap.Access, nil
}

// recoverFromDisk handles a refresh failure caused by another process rotating
// the shared credential file out from under us. It reloads the on-disk
// snapshot; if that holds a *different* refresh token it either adopts it (when
// still valid) or re-refreshes with it. It returns ok=false — leaving the
// caller to surface the original error — when the disk holds nothing new (same
// or empty refresh token, or an unreadable file), so a genuinely dead token
// still fails instead of looping.
func (c *OAuthCred) recoverFromDisk() (CredSnapshot, bool) {
	disk, err := c.store.Load()
	if err != nil || disk.Refresh == "" || disk.Refresh == c.snap.Refresh {
		return CredSnapshot{}, false
	}
	if time.Until(disk.Expiry) > oauthRefreshMargin {
		return disk, true // disk token still valid — adopt it, no refresh needed
	}
	fresh, err := c.refresh(disk.Refresh)
	if err != nil {
		return CredSnapshot{}, false
	}
	return fresh, true
}

// refreshAnthropic exchanges a refresh token for a new CredSnapshot using the
// Anthropic OAuth token endpoint. The exact wire shape was validated in Task 1.
func refreshAnthropic(refreshToken string) (CredSnapshot, error) {
	return refreshOAuthToken(anthropicOAuthTokenURL, anthropicOAuthClientID, refreshToken)
}

// refreshOAuthToken performs the OAuth refresh-token grant shared by every
// subscription vendor: POST {grant_type, refresh_token, client_id} as JSON to
// tokenURL and parse {access_token, refresh_token, expires_in}. It never
// interpolates the token into an error.
func refreshOAuthToken(tokenURL, clientID, refreshToken string) (CredSnapshot, error) {
	body, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     clientID,
	})
	if err != nil {
		return CredSnapshot{}, err
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(tokenURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return CredSnapshot{}, fmt.Errorf("oauth: token request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return CredSnapshot{}, fmt.Errorf("oauth: token endpoint returned %d", resp.StatusCode)
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return CredSnapshot{}, fmt.Errorf("oauth: decode response: %w", err)
	}

	if result.AccessToken == "" {
		return CredSnapshot{}, errors.New("oauth: refresh response had no access_token")
	}

	return CredSnapshot{
		Access:  result.AccessToken,
		Refresh: result.RefreshToken,
		Expiry:  time.Now().Add(expiresInDuration(result.ExpiresIn)),
	}, nil
}

// maxTokenLifetime caps the trusted-but-unverified expires_in from the token
// endpoint. OAuth access tokens are short-lived (minutes to hours); 30 days is
// generous headroom for any real value.
const maxTokenLifetime = 30 * 24 * time.Hour

// expiresInDuration converts an expires_in (seconds) into a Duration, clamped on
// the raw seconds before the multiply. Clamping first is what makes it safe: a
// hostile or buggy endpoint returning a huge value would otherwise overflow the
// int64-nanosecond Duration (it wraps past ~292 years, which can flip the sign
// and yield a far-future or past expiry). A negative or zero value clamps to 0
// so the token is treated as already expired rather than valid forever.
func expiresInDuration(seconds int) time.Duration {
	const maxSeconds = int(maxTokenLifetime / time.Second)
	switch {
	case seconds <= 0:
		return 0
	case seconds > maxSeconds:
		return maxTokenLifetime
	default:
		return time.Duration(seconds) * time.Second
	}
}
