package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"drydock/internal/config"
	"drydock/internal/gwcreds"
	"drydock/internal/provider"
)

// keychainService is the service name Claude Code uses in the macOS Keychain.
const keychainService = "Claude Code-credentials"

// claudeKeychainBlob is the JSON shape stored by `claude login` in the macOS
// Keychain. Only `claudeAiOauth` is relevant; other top-level keys are ignored.
type claudeKeychainBlob struct {
	ClaudeAiOauth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"` // Unix epoch in milliseconds
	} `json:"claudeAiOauth"`
}

// parseClaudeCreds unmarshals the raw JSON blob from the macOS Keychain and
// returns a gwcreds.CredSnapshot. Returns an error if the blob contains no
// access token (i.e. the operator is not logged in).
func parseClaudeCreds(raw []byte) (gwcreds.CredSnapshot, error) {
	var blob claudeKeychainBlob
	if err := json.Unmarshal(raw, &blob); err != nil {
		return gwcreds.CredSnapshot{}, fmt.Errorf("auth: parse keychain blob: %w", err)
	}
	if blob.ClaudeAiOauth.AccessToken == "" {
		return gwcreds.CredSnapshot{}, fmt.Errorf("auth: no Claude credentials found — run `claude login` first")
	}
	return gwcreds.CredSnapshot{
		Access:  blob.ClaudeAiOauth.AccessToken,
		Refresh: blob.ClaudeAiOauth.RefreshToken,
		Expiry:  time.UnixMilli(blob.ClaudeAiOauth.ExpiresAt),
	}, nil
}

// authAgents returns the names of agents whose provider has an OAuthBackend,
// i.e. agents that `drydock auth` can actually bootstrap credentials for.
// Agents like opencode (ConfigBuilt, no OAuthBackend) are excluded so they
// are not advertised as valid auth subcommands.
func authAgents() []string {
	var out []string
	for _, p := range provider.Registry {
		if p.OAuthBackend != nil {
			out = append(out, p.Agent)
		}
	}
	return out
}

// runAuth dispatches `drydock auth <subcommand>`. It is registry-driven:
// adding a new OAuth provider to provider.Registry automatically makes it
// available as a subcommand — no switch needed here.
func runAuth(args []string) {
	consumeHelpFlag("auth", args)
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "drydock auth — usage: drydock auth %s [--status]\n", strings.Join(authAgents(), "|"))
		os.Exit(2)
	}
	p, ok := provider.ByAgent(args[0])
	if !ok || p.OAuthBackend == nil {
		fmt.Fprintf(os.Stderr, "drydock auth: unknown subcommand %q (want one of %v)\n", args[0], authAgents())
		os.Exit(2)
	}
	runAuthAgent(p, args[1:])
}

// bootstraps maps each agent name to its credential-bootstrap function.
// The bootstrap functions are defined in this package because they rely on
// platform-specific tooling (macOS Keychain, ~/.codex/auth.json) that belongs
// in the CLI layer, not in the provider registry (an internal package).
var bootstraps = map[string]func(cfgDir string) error{
	"claude": bootstrapClaudeCred,
	"codex":  bootstrapCodexCred,
}

// runAuthAgent is the shared implementation of `drydock auth <agent> [--status]`.
// It replaces the former structural twins runAuthClaude / runAuthCodex.
func runAuthAgent(p provider.Provider, args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Printf("drydock auth %s — %s\n", p.Agent, subHelp["auth"])
			os.Exit(0)
		}
	}
	statusOnly := len(args) > 0 && (args[0] == "--status" || args[0] == "-status")

	cfgDir := config.Dir()
	if statusOnly {
		snap, err := p.LoadOAuthSnap(cfgDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "auth: no stored credentials —", err)
			os.Exit(1)
		}
		printAgentValidity(p, snap)
		return
	}

	fn := bootstraps[p.Agent]
	if fn == nil {
		fmt.Fprintf(os.Stderr, "drydock auth: %q has no bootstrap implementation wired\n", p.Agent)
		os.Exit(2)
	}
	if err := fn(cfgDir); err != nil {
		fmt.Fprintln(os.Stderr, "auth:", err)
		os.Exit(1)
	}
	snap, _ := p.LoadOAuthSnap(cfgDir)
	printAgentValidity(p, snap)
}

// printAgentValidity prints a token-free status line showing how long the
// stored token remains valid. It replaces the former pair printValidity /
// printCodexValidity, using Provider.AuthLabel for the entity name and
// Provider.RefreshOnExpiry for the refresh note.
// The token value itself is never printed.
func printAgentValidity(p provider.Provider, snap gwcreds.CredSnapshot) {
	remaining := time.Until(snap.Expiry)
	label := p.AuthLabel
	if remaining <= 0 {
		msg := "authenticated as " + label + " · token EXPIRED"
		if p.RefreshOnExpiry {
			msg += " (will refresh on next use)"
		}
		fmt.Println(msg)
		return
	}
	mins := int(math.Round(remaining.Minutes()))
	fmt.Printf("authenticated as %s · token valid for %dm\n", label, mins)
}

// bootstrapClaudeCred copies the Claude subscription credential from the macOS
// Keychain into drydock's store. Returns an error (never exits) so callers —
// the auth subcommand and the setup wizard — can react.
func bootstrapClaudeCred(cfgDir string) error {
	out, err := exec.Command("security", "find-generic-password", "-s", keychainService, "-w").Output()
	if err != nil {
		return fmt.Errorf("could not read Claude credentials from Keychain — run `claude login` first")
	}
	snap, err := parseClaudeCreds(out)
	if err != nil {
		return err
	}
	p, ok := provider.ByAgent("claude")
	if !ok {
		return fmt.Errorf("auth: claude provider not registered")
	}
	return gwcreds.FileCredStore(filepath.Join(cfgDir, p.OAuthFile)).Save(snap)
}

// codexAuthFile is the relevant shape of ~/.codex/auth.json (auth_mode
// "chatgpt"). Only these fields are read; the id_token is ignored.
type codexAuthFile struct {
	Tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
}

// jwtExpiry decodes ONLY the exp claim from a JWT. It never returns or logs any
// other claim (the Codex access token's payload carries account_id/plan/org).
func jwtExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("auth: access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("auth: decode JWT payload: %w", err)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("auth: parse JWT claims: %w", err)
	}
	if claims.Exp == 0 {
		return time.Time{}, fmt.Errorf("auth: JWT has no exp claim")
	}
	return time.Unix(claims.Exp, 0), nil
}

// parseCodexCreds maps ~/.codex/auth.json to a CredSnapshot + the ChatGPT
// account id. Expiry comes from the access-token JWT exp claim.
func parseCodexCreds(raw []byte) (gwcreds.CredSnapshot, string, error) {
	var f codexAuthFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return gwcreds.CredSnapshot{}, "", fmt.Errorf("auth: parse codex auth.json: %w", err)
	}
	if f.Tokens.AccessToken == "" {
		return gwcreds.CredSnapshot{}, "", fmt.Errorf("auth: no Codex credentials found — run `codex login` first")
	}
	exp, err := jwtExpiry(f.Tokens.AccessToken)
	if err != nil {
		return gwcreds.CredSnapshot{}, "", err
	}
	return gwcreds.CredSnapshot{Access: f.Tokens.AccessToken, Refresh: f.Tokens.RefreshToken, Expiry: exp}, f.Tokens.AccountID, nil
}

// bootstrapCodexCred copies the ChatGPT/Codex credential from ~/.codex/auth.json
// into drydock's store. Returns an error (never exits).
func bootstrapCodexCred(cfgDir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("could not determine home directory: %w", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		return fmt.Errorf("could not read ~/.codex/auth.json — run `codex login` first")
	}
	snap, account, err := parseCodexCreds(raw)
	if err != nil {
		return err
	}
	p, ok := provider.ByAgent("codex")
	if !ok {
		return fmt.Errorf("auth: codex provider not registered")
	}
	store := gwcreds.NewCodexStore(filepath.Join(cfgDir, p.OAuthFile))
	return store.Put(snap, account)
}
