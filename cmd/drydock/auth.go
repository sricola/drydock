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
	"drydock/internal/gateway"
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
// returns a gateway.CredSnapshot. Returns an error if the blob contains no
// access token (i.e. the operator is not logged in).
func parseClaudeCreds(raw []byte) (gateway.CredSnapshot, error) {
	var blob claudeKeychainBlob
	if err := json.Unmarshal(raw, &blob); err != nil {
		return gateway.CredSnapshot{}, fmt.Errorf("auth: parse keychain blob: %w", err)
	}
	if blob.ClaudeAiOauth.AccessToken == "" {
		return gateway.CredSnapshot{}, fmt.Errorf("auth: no Claude credentials found — run `claude login` first")
	}
	return gateway.CredSnapshot{
		Access:  blob.ClaudeAiOauth.AccessToken,
		Refresh: blob.ClaudeAiOauth.RefreshToken,
		Expiry:  time.UnixMilli(blob.ClaudeAiOauth.ExpiresAt),
	}, nil
}

// runAuth dispatches `drydock auth <subcommand>`.
func runAuth(args []string) {
	consumeHelpFlag("auth", args)
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "drydock auth — usage: drydock auth %s [--status]\n", strings.Join(provider.Agents(), "|"))
		os.Exit(2)
	}
	if _, ok := provider.ByAgent(args[0]); !ok {
		fmt.Fprintf(os.Stderr, "drydock auth: unknown subcommand %q (want one of %v)\n", args[0], provider.Agents())
		os.Exit(2)
	}
	switch args[0] {
	case "claude":
		runAuthClaude(args[1:])
	case "codex":
		runAuthCodex(args[1:])
	}
}

// bootstrapClaudeCred copies the Claude subscription credential from the macOS
// Keychain into drydock's store. Returns an error (never exits) so callers —
// the auth subcommand and the setup wizard — can react.
func bootstrapClaudeCred() error {
	out, err := exec.Command("security", "find-generic-password", "-s", keychainService, "-w").Output()
	if err != nil {
		return fmt.Errorf("could not read Claude credentials from Keychain — run `claude login` first")
	}
	snap, err := parseClaudeCreds(out)
	if err != nil {
		return err
	}
	return gateway.FileCredStore(filepath.Join(config.Dir(), "claude-oauth.json")).Save(snap)
}

// runAuthClaude implements `drydock auth claude [--status]`.
func runAuthClaude(args []string) {
	// Handle help flags before anything else.
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Printf("drydock auth claude — %s\n", subHelp["auth"])
			os.Exit(0)
		}
	}

	// --status: report current cred validity without re-copying.
	statusOnly := len(args) > 0 && (args[0] == "--status" || args[0] == "-status")

	if statusOnly {
		snap, err := gateway.FileCredStore(filepath.Join(config.Dir(), "claude-oauth.json")).Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, "auth: no stored credentials —", err)
			os.Exit(1)
		}
		printValidity(snap)
		return
	}

	if err := bootstrapClaudeCred(); err != nil {
		fmt.Fprintln(os.Stderr, "auth:", err)
		os.Exit(1)
	}
	snap, _ := gateway.FileCredStore(filepath.Join(config.Dir(), "claude-oauth.json")).Load()
	printValidity(snap)
}

// printValidity prints a token-free status line showing how long the token
// remains valid. The token value itself is never printed.
func printValidity(snap gateway.CredSnapshot) {
	remaining := time.Until(snap.Expiry)
	if remaining <= 0 {
		fmt.Println("authenticated as Claude subscription · token EXPIRED")
		return
	}
	mins := int(math.Round(remaining.Minutes()))
	fmt.Printf("authenticated as Claude subscription · token valid for %dm\n", mins)
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
func parseCodexCreds(raw []byte) (gateway.CredSnapshot, string, error) {
	var f codexAuthFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return gateway.CredSnapshot{}, "", fmt.Errorf("auth: parse codex auth.json: %w", err)
	}
	if f.Tokens.AccessToken == "" {
		return gateway.CredSnapshot{}, "", fmt.Errorf("auth: no Codex credentials found — run `codex login` first")
	}
	exp, err := jwtExpiry(f.Tokens.AccessToken)
	if err != nil {
		return gateway.CredSnapshot{}, "", err
	}
	return gateway.CredSnapshot{Access: f.Tokens.AccessToken, Refresh: f.Tokens.RefreshToken, Expiry: exp}, f.Tokens.AccountID, nil
}

// bootstrapCodexCred copies the ChatGPT/Codex credential from ~/.codex/auth.json
// into drydock's store. Returns an error (never exits).
func bootstrapCodexCred() error {
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
	store := gateway.NewCodexStore(filepath.Join(config.Dir(), "codex-oauth.json"))
	return store.Put(snap, account)
}

// runAuthCodex implements `drydock auth codex [--status]`.
func runAuthCodex(args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Printf("drydock auth codex — %s\n", subHelp["auth"])
			os.Exit(0)
		}
	}
	statusOnly := len(args) > 0 && (args[0] == "--status" || args[0] == "-status")

	if statusOnly {
		snap, err := gateway.NewCodexStore(filepath.Join(config.Dir(), "codex-oauth.json")).Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, "auth: no stored credentials —", err)
			os.Exit(1)
		}
		printCodexValidity(snap)
		return
	}

	if err := bootstrapCodexCred(); err != nil {
		fmt.Fprintln(os.Stderr, "auth:", err)
		os.Exit(1)
	}
	snap, _ := gateway.NewCodexStore(filepath.Join(config.Dir(), "codex-oauth.json")).Load()
	printCodexValidity(snap)
}

// printCodexValidity prints a token-free status line. The token and account id
// are never printed.
func printCodexValidity(snap gateway.CredSnapshot) {
	remaining := time.Until(snap.Expiry)
	if remaining <= 0 {
		fmt.Println("authenticated as Codex (ChatGPT) subscription · token EXPIRED (will refresh on next use)")
		return
	}
	fmt.Printf("authenticated as Codex (ChatGPT) subscription · token valid for %dm\n", int(math.Round(remaining.Minutes())))
}
