//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Codex subscription spike (Task 1 / GO-NO-GO gate). Reproduces the go/no-go
// validation: a Codex (ChatGPT plan) OAuth access token, read from
// ~/.codex/auth.json, is accepted by chatgpt.com's Codex backend when forwarded
// with the Codex CLI header set — i.e. subscription access works through a
// gateway-shaped request. The refresh probe additionally guards the documented
// "refresh strips account_id" failure mode by repeating the request with the
// NEW access token but the ORIGINAL captured account_id.
//
//	DRYDOCK_CODEX_SPIKE=1 go test -tags=integration -run TestCodexSpike ./tests/integration/ -v
//
// Requires `codex login` (ChatGPT plan) on this host. Skipped by default — it
// makes a live request against the operator's subscription.
//
// Token safety: tokens are read into locals and never logged. No curl -v, no
// httputil.DumpRequest of auth headers. On a non-200 we print only the status
// and the response body (model output / structured error — safe).

// Local spike constants, mirroring internal/gateway's pinned values. Kept local
// so this opt-in spike test has no cross-package dependency (the unexported
// gateway constants are not visible from package integration).
const (
	codexBackendBaseURLSpike  = "https://chatgpt.com"
	codexBackendBasePathSpike = "/backend-api/codex"
	openaiOAuthClientIDSpike  = "app_EMoamEEZ73f0CkXaXp7hrann"
	openaiOAuthTokenURLSpike  = "https://auth.openai.com/oauth/token"

	codexOriginatorSpike = "codex_cli_rs"
	codexUserAgentSpike  = "codex_cli_rs/0.141.0 (Mac OS 27.0.0; arm64)"
	codexOpenAIBetaSpike = "responses=experimental"

	// codexModelSpike is the model that works for a ChatGPT-account Codex
	// session. Other names (gpt-5, gpt-5-codex) return HTTP 400 "model not
	// supported when using Codex with a ChatGPT account".
	codexModelSpike = "gpt-5.5"
)

// codexProbeBody is the minimal Responses-API request body (store:false,
// stream:true) used by both probes.
const codexProbeBody = `{"model":"` + codexModelSpike + `","instructions":"You are terse.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Reply with one word: hello"}]}],"store":false,"stream":true}`

// TestCodexSpike_SubscriptionAccessThroughBearer is the access probe: it POSTs
// the confirmed request through the confirmed headers and asserts 200 +
// response.completed in the SSE stream.
func TestCodexSpike_SubscriptionAccessThroughBearer(t *testing.T) {
	if os.Getenv("DRYDOCK_CODEX_SPIKE") != "1" {
		t.Skip("set DRYDOCK_CODEX_SPIKE=1 — live request against a real ChatGPT/codex login")
	}
	access, _, account := readCodexCreds(t)

	resp := postCodex(t, codexBackendBaseURLSpike+codexBackendBasePathSpike+"/responses", access, account, codexProbeBody)
	defer resp.Body.Close()
	body := readAllString(t, resp.Body) // SSE; model output, not the credential
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("KILL CRITERION: Codex subscription access denied to a gateway-shaped request: HTTP %d\nbody: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "response.completed") {
		t.Fatalf("KILL CRITERION: no completion event in stream\nbody: %s", body)
	}
}

// TestCodexSpike_RefreshPreservesAccountID is the refresh probe (the account_id
// guard): it exchanges the refresh token for a NEW access token, then repeats
// the access request with that new token AND the ORIGINAL captured account_id,
// asserting 200. This guards the documented "refresh strips account_id" failure
// mode.
func TestCodexSpike_RefreshPreservesAccountID(t *testing.T) {
	if os.Getenv("DRYDOCK_CODEX_SPIKE") != "1" {
		t.Skip("set DRYDOCK_CODEX_SPIKE=1 — live request against a real ChatGPT/codex login")
	}
	_, refresh, account := readCodexCreds(t)

	newAccess, status, body := refreshCodexAccess(t, refresh)
	if status == http.StatusTooManyRequests {
		// Rate-limited: the token exchange is validated-by-typed-error (the
		// endpoint/shape was understood, not a routing/credential error). We
		// cannot proceed to the post-refresh request assertion without a real
		// new token; record DEFERRED rather than faking a pass.
		t.Skipf("DEFERRED: refresh token endpoint rate-limited (HTTP 429); endpoint/shape confirmed but a clean new access token was not captured, so the post-refresh request assertion is not exercised. body: %s", body)
	}
	if status != http.StatusOK {
		t.Fatalf("KILL CRITERION: refresh grant failed: HTTP %d\nbody: %s", status, body)
	}
	if newAccess == "" {
		t.Fatalf("KILL CRITERION: refresh returned 200 but no access_token field")
	}

	// The key new assertion: NEW access token + ORIGINAL captured account_id.
	resp := postCodex(t, codexBackendBaseURLSpike+codexBackendBasePathSpike+"/responses", newAccess, account, codexProbeBody)
	defer resp.Body.Close()
	rbody := readAllString(t, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("KILL CRITERION: 200 pre-refresh but HTTP %d post-refresh with the captured account_id (refresh-strips-account_id failure mode)\nbody: %s", resp.StatusCode, rbody)
	}
	if !strings.Contains(rbody, "response.completed") {
		t.Fatalf("KILL CRITERION: post-refresh request returned 200 but no completion event\nbody: %s", rbody)
	}
}

// readCodexCreds reads access_token, refresh_token, and account_id from
// ~/.codex/auth.json. Never logs any value. Returns (access, refresh, account).
func readCodexCreds(t *testing.T) (access, refresh, account string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home dir: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		t.Skipf("no ~/.codex/auth.json (run `codex login`): %v", err)
	}
	var blob struct {
		AuthMode string `json:"auth_mode"`
		Tokens   struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &blob); err != nil {
		t.Fatalf("parse ~/.codex/auth.json: %v", err)
	}
	if blob.AuthMode != "chatgpt" {
		t.Skipf("~/.codex/auth.json auth_mode is %q, want \"chatgpt\" (ChatGPT-plan login required)", blob.AuthMode)
	}
	if blob.Tokens.AccessToken == "" || blob.Tokens.AccountID == "" {
		t.Skip("~/.codex/auth.json missing tokens.access_token or tokens.account_id — run `codex login`")
	}
	return blob.Tokens.AccessToken, blob.Tokens.RefreshToken, blob.Tokens.AccountID
}

// postCodex POSTs body to the Codex responses endpoint with the confirmed Codex
// CLI header set. Sets Authorization/account/originator/UA/beta. Never logs the
// token or request headers.
func postCodex(t *testing.T, endpoint, access, account, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("chatgpt-account-id", account)
	req.Header.Set("originator", codexOriginatorSpike)
	req.Header.Set("User-Agent", codexUserAgentSpike)
	req.Header.Set("OpenAI-Beta", codexOpenAIBetaSpike)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("request error: %v", err)
	}
	return resp
}

// refreshCodexAccess exchanges a refresh token for a new access token at the
// OpenAI OAuth token endpoint. Tries JSON encoding first; on HTTP 400 retries
// with form encoding (the body encoding was unconfirmed going in). Returns
// (newAccessToken, statusCode, responseBody). The response body is a structured
// OAuth response; it may contain token material, so callers must NOT log it on
// success — only on a non-200 typed error (which carries no token).
func refreshCodexAccess(t *testing.T, refresh string) (newAccess string, status int, body string) {
	t.Helper()

	parseAccess := func(b []byte) string {
		var r struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.Unmarshal(b, &r)
		return r.AccessToken
	}

	// Attempt 1: JSON body.
	jsonBody, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     openaiOAuthClientIDSpike,
		"refresh_token": refresh,
	})
	access, st, raw := doRefresh(t, "application/json", bytes.NewReader(jsonBody), parseAccess)
	if st != http.StatusBadRequest {
		t.Logf("refresh body encoding used: JSON (HTTP %d)", st)
		// Only surface the body when there is no token in it (non-200).
		if st != http.StatusOK {
			return access, st, string(raw)
		}
		return access, st, ""
	}

	// Attempt 2: form encoding.
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", openaiOAuthClientIDSpike)
	form.Set("refresh_token", refresh)
	access, st, raw = doRefresh(t, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), parseAccess)
	t.Logf("refresh body encoding used: form (HTTP %d; JSON attempt returned 400)", st)
	if st != http.StatusOK {
		return access, st, string(raw)
	}
	return access, st, ""
}

func doRefresh(t *testing.T, contentType string, body io.Reader, parseAccess func([]byte) string) (string, int, []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", openaiOAuthTokenURLSpike, body)
	if err != nil {
		t.Fatalf("build refresh request: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("refresh request error: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return parseAccess(raw), resp.StatusCode, raw
}

// readAllString reads a response body into a string. The Codex SSE body is model
// output / a structured error — safe to inspect, never the credential.
func readAllString(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return string(b)
}
