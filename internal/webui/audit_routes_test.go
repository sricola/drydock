package webui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func auditServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	os.WriteFile(filepath.Join(dir, id+".diff"), []byte("diff --git a b\n+line\n"), 0o600)
	os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(
		`{"type":"drydock_meta","subscription":false,"sensitive":false}`+"\n"+
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":1200,"total_cost_usd":0.05,"num_turns":3}`+"\n"), 0o600)
	os.WriteFile(filepath.Join(dir, id+".widen.json"), []byte(`[{"host":"x.test","ports":[443]}]`), 0o600)
	return &Server{AuditRoot: dir, Token: "secret"}
}

func TestDiffAndLogsAndWiden(t *testing.T) {
	s := auditServer(t)
	id := "0123456789abcdef0123456789abcdef"
	logsWant := `{"type":"drydock_meta","subscription":false,"sensitive":false}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":1200,"total_cost_usd":0.05,"num_turns":3}` + "\n"
	for _, tc := range []struct{ path, want string }{
		{"/api/diff/" + id, "diff --git a b\n+line\n"},
		{"/api/widen/" + id, `[{"host":"x.test","ports":[443]}]`},
		{"/api/logs/" + id, logsWant},
	} {
		rec := do(t, s, "GET", tc.path, "127.0.0.1:7878", "Bearer secret")
		if rec.Code != http.StatusOK || rec.Body.String() != tc.want {
			t.Errorf("%s = %d %q", tc.path, rec.Code, rec.Body.String())
		}
	}
	// Missing → 404.
	rec := do(t, s, "GET", "/api/diff/ffffffffffffffffffffffffffffffff", "127.0.0.1:7878", "Bearer secret")
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing diff = %d, want 404", rec.Code)
	}
	// Bad id → 400.
	if rec := do(t, s, "GET", "/api/diff/NOPE", "127.0.0.1:7878", "Bearer secret"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad id = %d, want 400", rec.Code)
	}
}

func TestHistory(t *testing.T) {
	s := auditServer(t)
	rec := do(t, s, "GET", "/api/history", "127.0.0.1:7878", "Bearer secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("history = %d", rec.Code)
	}
	var items []HistoryItem
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Outcome != "ok (3 turn)" || items[0].Cost != "$0.0500" || !items[0].HasDuration || items[0].DurationMs != 1200 {
		t.Fatalf("history item wrong: %+v", items)
	}
}

func TestSymlinkRejected(t *testing.T) {
	s := auditServer(t)
	id := "ffffffffffffffffffffffffffffffff"
	os.Symlink("/etc/hosts", filepath.Join(s.AuditRoot, id+".diff"))
	rec := do(t, s, "GET", "/api/diff/"+id, "127.0.0.1:7878", "Bearer secret")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("symlinked diff: got status %d, want 404 — must not follow symlinks", rec.Code)
	}
}

// TestHistorySymlinkRejected mirrors TestSymlinkRejected for handleHistory:
// a .jsonl that is a symlink must NOT be included in the history list — we
// must not follow the symlink and expose the target's content as an audit record.
func TestHistorySymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0"
	// Plant a .jsonl symlink pointing at a file outside the audit dir.
	if err := os.Symlink("/etc/hosts", filepath.Join(dir, id+".jsonl")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	s := &Server{AuditRoot: dir, Token: "secret"}
	rec := do(t, s, "GET", "/api/history", "127.0.0.1:7878", "Bearer secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("handleHistory status = %d, want 200", rec.Code)
	}
	var items []HistoryItem
	if err := json.NewDecoder(rec.Body).Decode(&items); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("symlinked .jsonl appeared in history (len=%d) — handleHistory must not follow symlinks", len(items))
	}
}
