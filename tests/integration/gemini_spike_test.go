//go:build geminispike

// Spike: does @google/gemini-cli route through a custom base URL in API-key
// mode with a strippable auth header, so the drydock gateway can broker it?
// Uses a fake httptest gateway and a SENTINEL key — no live endpoint, no real
// credential. See docs/superpowers/specs/2026-07-02-gemini-native-vendor-spike-design.md.
package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type capturedReq struct {
	Method string
	Path   string // URL.Path + "?" + RawQuery
	Header http.Header
	Body   string
}

func randHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

func geminiVersion(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("gemini", "--version").CombinedOutput()
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	return strings.TrimSpace(string(out))
}

func TestGeminiBrokering_Spike(t *testing.T) {
	if _, err := exec.LookPath("gemini"); err != nil {
		t.Skip("gemini CLI not installed; `npm i -g @google/gemini-cli` then rerun `make test-gemini-spike`")
	}

	// Minimal valid generateContent response with usageMetadata so the CLI
	// treats the turn as successful.
	resp := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": "ok"}}},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount": 3, "candidatesTokenCount": 1, "totalTokenCount": 4,
		},
		"modelVersion": "gemini-2.5-flash",
	}
	respBytes, _ := json.Marshal(resp)

	var (
		mu   sync.Mutex
		reqs []capturedReq
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, capturedReq{
			Method: r.Method,
			Path:   r.URL.Path + "?" + r.URL.RawQuery,
			Header: r.Header.Clone(),
			Body:   string(body),
		})
		mu.Unlock()
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: " + string(respBytes) + "\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(respBytes)
	}))
	defer srv.Close()

	sentinel := "SENTINEL-" + randHex(t)
	home := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Candidate headless invocation; Task 2 confirms/adjusts the flag.
	cmd := exec.CommandContext(ctx, "gemini", "-p", "say ok")
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"GOOGLE_GEMINI_BASE_URL="+srv.URL, // primary override
		"GEMINI_BASEURL="+srv.URL,         // belt-and-suspenders alternate name
		"GEMINI_API_KEY="+sentinel,
		"GEMINI_DIR="+filepath.Join(home, ".gemini"),
		"GOOGLE_GENAI_USE_GCA=",           // neutralize Cloud/Code-Assist auto-auth
		"GOOGLE_GENAI_USE_VERTEXAI=false", // force the Gemini API (not Vertex)
		"NO_COLOR=1",
		"TERM=dumb",
	)
	out, runErr := cmd.CombinedOutput()

	t.Logf("=== gemini version: %s", geminiVersion(t))
	t.Logf("=== invocation: gemini -p 'say ok' (exit err: %v)", runErr)
	t.Logf("=== stdout+stderr:\n%s", out)

	mu.Lock()
	defer mu.Unlock()
	t.Logf("=== captured %d request(s)", len(reqs))
	for i, rq := range reqs {
		t.Logf("[req %d] %s %s", i, rq.Method, rq.Path)
		t.Logf("[req %d] x-goog-api-key=%q authorization=%q key-in-query=%v",
			i, rq.Header.Get("x-goog-api-key"), rq.Header.Get("Authorization"),
			strings.Contains(rq.Path, "key="))
		t.Logf("[req %d] body: %s", i, rq.Body)
	}

	// --- GREEN gate (see spec §Go/No-Go) ---
	if len(reqs) == 0 {
		t.Fatalf("RED: gemini did not hit GOOGLE_GEMINI_BASE_URL — base-URL override not honored")
	}
	var brokered *capturedReq
	for i := range reqs {
		h := reqs[i].Header
		if h.Get("x-goog-api-key") == sentinel ||
			h.Get("Authorization") == "Bearer "+sentinel ||
			strings.Contains(reqs[i].Path, "key="+sentinel) {
			brokered = &reqs[i]
			break
		}
	}
	if brokered == nil {
		t.Fatalf("RED: sentinel key not found in any request header/query — gateway can't strip/replace it")
	}
	if runErr != nil {
		t.Fatalf("RED: gemini did not exit 0 in headless mode: %v", runErr)
	}
	t.Log("GREEN: base-URL honored, sentinel key isolated in a replaceable header, headless exit 0")
}
