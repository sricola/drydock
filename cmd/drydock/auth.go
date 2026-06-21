package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"drydock/internal/config"
	"drydock/internal/gateway"
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
		fmt.Fprintln(os.Stderr, "drydock auth — usage: drydock auth claude [--status]")
		os.Exit(2)
	}
	switch args[0] {
	case "claude":
		runAuthClaude(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "drydock auth: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
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

	credPath := filepath.Join(config.Dir(), "claude-oauth.json")
	store := gateway.FileCredStore(credPath)

	if statusOnly {
		snap, err := store.Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, "auth: no stored credentials —", err)
			os.Exit(1)
		}
		printValidity(snap)
		return
	}

	// Read credentials from the macOS Keychain.
	out, err := exec.Command("security", "find-generic-password", "-s", keychainService, "-w").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "auth: could not read Claude credentials from Keychain — run `claude login` first")
		os.Exit(1)
	}

	snap, err := parseClaudeCreds(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "auth:", err)
		os.Exit(1)
	}

	if err := store.Save(snap); err != nil {
		fmt.Fprintln(os.Stderr, "auth: failed to save credentials:", err)
		os.Exit(1)
	}

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
