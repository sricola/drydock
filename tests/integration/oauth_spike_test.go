//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"
)

// OAuth subscription spike (plan Task 1 / Deliverable 0). Reproduces the
// go/no-go validation: a Claude Code OAuth access token, read from the macOS
// Keychain, is accepted by api.anthropic.com when forwarded with the OAuth beta
// header — i.e. subscription access works through a gateway-shaped request.
//
//	DRYDOCK_OAUTH_SPIKE=1 go test -tags=integration -run TestOAuthSpike ./tests/integration/ -v
//
// Requires `claude login` (Pro/Max) on macOS. Skipped by default — it makes a
// live request against the operator's subscription.
//
// Result 2026-06-20: HTTP 200 with a completion (access confirmed). The refresh
// grant against console.anthropic.com/v1/oauth/token returned a typed
// rate_limit_error (endpoint/shape correct; clean 200 deferred to normal use).
func TestOAuthSpike_SubscriptionAccessThroughBearer(t *testing.T) {
	if os.Getenv("DRYDOCK_OAUTH_SPIKE") != "1" {
		t.Skip("set DRYDOCK_OAUTH_SPIKE=1 — live request against a real Pro/Max login")
	}
	access := claudeAccessTokenFromKeychain(t)

	reqBody, _ := json.Marshal(map[string]any{
		"model":      "claude-haiku-4-5-20251001",
		"max_tokens": 16,
		// Subscription/OAuth access requires the Claude Code identity. The real
		// VM runs Claude Code, so its requests carry this naturally; the gateway
		// only forwards.
		"system":   "You are Claude Code, Anthropic's official CLI for Claude.",
		"messages": []map[string]string{{"role": "user", "content": "Reply with the single word: OK"}},
	})
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", anthropicOAuthBetaSpike)
	req.Header.Set("content-type", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("request error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("KILL CRITERION: subscription access denied to a gateway-shaped request: HTTP %d", resp.StatusCode)
	}
}

// anthropicOAuthBetaSpike mirrors internal/gateway's pinned beta header; kept
// local so this throwaway spike test has no cross-package dependency.
const anthropicOAuthBetaSpike = "oauth-2025-04-20"

// claudeAccessTokenFromKeychain reads the Claude Code OAuth access token from
// the macOS Keychain. Never logs the value.
func claudeAccessTokenFromKeychain(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("security", "find-generic-password", "-s", "Claude Code-credentials", "-w").Output()
	if err != nil {
		t.Skipf("no Claude Code Keychain credentials (run `claude login`): %v", err)
	}
	var blob struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &blob); err != nil {
		t.Fatalf("parse keychain blob: %v", err)
	}
	if blob.ClaudeAiOauth.AccessToken == "" {
		t.Skip("Keychain has no claudeAiOauth.accessToken — run `claude login`")
	}
	return blob.ClaudeAiOauth.AccessToken
}
