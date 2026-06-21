package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// codexFile is the on-disk shape of ~/.drydock/codex-oauth.json: the OAuth
// token pair + the captured ChatGPT account id (constant across refreshes).
type codexFile struct {
	Access    string    `json:"access_token"`
	Refresh   string    `json:"refresh_token"`
	Expiry    time.Time `json:"expiry"`
	AccountID string    `json:"account_id"`
}

// CodexStore is a CredStore that also retains the ChatGPT account id. Save
// (called by OAuthCred on refresh rotation) preserves the account id captured
// at bootstrap, guarding the documented "refresh strips account id" failure.
type CodexStore struct {
	path      string
	accountID string
}

func NewCodexStore(path string) *CodexStore { return &CodexStore{path: path} }

func (s *CodexStore) Load() (CredSnapshot, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return CredSnapshot{}, err
	}
	var f codexFile
	if err := json.Unmarshal(data, &f); err != nil {
		return CredSnapshot{}, err
	}
	s.accountID = f.AccountID
	return CredSnapshot{Access: f.Access, Refresh: f.Refresh, Expiry: f.Expiry}, nil
}

func (s *CodexStore) Save(snap CredSnapshot) error {
	data, err := json.Marshal(codexFile{Access: snap.Access, Refresh: snap.Refresh, Expiry: snap.Expiry, AccountID: s.accountID})
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Put writes the initial credential file including the account id. Used by
// `drydock auth codex`; later refreshes persist via Save, preserving it.
func (s *CodexStore) Put(snap CredSnapshot, accountID string) error {
	s.accountID = accountID
	return s.Save(snap)
}

func (s *CodexStore) AccountID() string { return s.accountID }

// NewOAuthCredCodex is OAuthCred wired to the OpenAI refresh grant.
func NewOAuthCredCodex(snap CredSnapshot, store CredStore) *OAuthCred {
	return &OAuthCred{snap: snap, store: store, refresh: refreshOpenAI}
}

// refreshOpenAI exchanges a refresh token for a new CredSnapshot via the OpenAI
// OAuth token endpoint. Shape confirmed in Task 1. Never interpolates the token
// into errors.
func refreshOpenAI(refreshToken string) (CredSnapshot, error) {
	body, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     openaiOAuthClientID,
	})
	if err != nil {
		return CredSnapshot{}, err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(openaiOAuthTokenURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return CredSnapshot{}, fmt.Errorf("oauth: codex token request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return CredSnapshot{}, fmt.Errorf("oauth: codex token endpoint returned %d", resp.StatusCode)
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return CredSnapshot{}, fmt.Errorf("oauth: decode codex token response: %w", err)
	}
	if result.AccessToken == "" {
		return CredSnapshot{}, fmt.Errorf("oauth: codex refresh response had no access_token")
	}
	return CredSnapshot{
		Access:  result.AccessToken,
		Refresh: result.RefreshToken,
		Expiry:  time.Now().Add(time.Duration(result.ExpiresIn) * time.Second),
	}, nil
}
