package gwcreds

import (
	"encoding/json"
	"os"
	"time"

	"drydock/internal/atomicfile"
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

// NewCodexStore opens the account-id-aware Codex credential store at path.
// Call Put (bootstrap) or Load before the first refresh-driven Save, so the
// captured account id is set; otherwise Save would persist a blank id.
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
	return atomicfile.Write(s.path, data, 0o600)
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
// OAuth token endpoint. Shape confirmed in Task 1.
func refreshOpenAI(refreshToken string) (CredSnapshot, error) {
	return refreshOAuthToken(openaiOAuthTokenURL, openaiOAuthClientID, refreshToken)
}
