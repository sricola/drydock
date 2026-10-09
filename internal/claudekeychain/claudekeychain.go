// Package claudekeychain reads the Claude Code login that `claude login`
// stores in the macOS Keychain, as a gwcreds.CredSnapshot. It is the single
// parser for that blob, shared by `drydock auth claude` (the bootstrap copy
// into ~/.drydock), `drydock doctor` (the staleness check), and brokerd (the
// re-import hook on the subscription credential, see gwcreds.OAuthCred).
//
// Why brokerd needs it: the grant drydock copies is the SAME grant Claude Code
// keeps using. OAuth refresh tokens are single-use, so whichever client
// refreshes first rotates the pair and invalidates the other's copy. Claude
// Code refreshes whenever it is used, so a copy taken at `drydock auth claude`
// is dead within one access-token lifetime (about ninety minutes) unless
// brokerd re-reads the Keychain before trying to refresh on its own.
package claudekeychain

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"drydock/internal/gwcreds"
)

// Service is the Keychain service name `claude login` writes under.
const Service = "Claude Code-credentials"

// blob is the JSON shape stored in the Keychain item. Only `claudeAiOauth` is
// relevant; other top-level keys are ignored.
type blob struct {
	ClaudeAiOauth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"` // Unix epoch in milliseconds
	} `json:"claudeAiOauth"`
}

// Parse unmarshals the raw Keychain blob into a CredSnapshot. It errors when
// the blob carries no access token (the operator is not logged in).
func Parse(raw []byte) (gwcreds.CredSnapshot, error) {
	var b blob
	if err := json.Unmarshal(raw, &b); err != nil {
		return gwcreds.CredSnapshot{}, fmt.Errorf("auth: parse keychain blob: %w", err)
	}
	if b.ClaudeAiOauth.AccessToken == "" {
		return gwcreds.CredSnapshot{}, fmt.Errorf("auth: no Claude credentials found, run `claude login` first")
	}
	return gwcreds.CredSnapshot{
		Access:  b.ClaudeAiOauth.AccessToken,
		Refresh: b.ClaudeAiOauth.RefreshToken,
		Expiry:  time.UnixMilli(b.ClaudeAiOauth.ExpiresAt),
	}, nil
}

// readBlob is the `security` shell-out, a package var so tests can swap it.
var readBlob = func() ([]byte, error) {
	return exec.Command("security", "find-generic-password", "-s", Service, "-w").Output()
}

// Read returns the current Keychain login. The error is operator-facing and
// never carries token material.
func Read() (gwcreds.CredSnapshot, error) {
	out, err := readBlob()
	if err != nil {
		return gwcreds.CredSnapshot{}, fmt.Errorf("could not read Claude credentials from Keychain, run `claude login` first")
	}
	return Parse(out)
}

// Snapshot is Read in the shape gwcreds.OAuthCred's re-import hook wants: a
// snapshot and ok, with every failure (no Keychain, not logged in, unparseable
// blob) collapsed to ok=false so the credential falls back to its own refresh.
func Snapshot() (gwcreds.CredSnapshot, bool) {
	snap, err := Read()
	if err != nil {
		return gwcreds.CredSnapshot{}, false
	}
	return snap, true
}
