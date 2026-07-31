package webui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/audit"
	"drydock/internal/trustbrief"
)

func auditServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	os.WriteFile(filepath.Join(dir, id+".diff"), []byte("diff --git a b\n+line\n"), 0o600)
	os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(
		`{"type":"drydock_meta","subscription":false,"sensitive":false}`+"\n"+
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":1200,"total_cost_usd":0.05,"num_turns":3,"src":"broker"}`+"\n"), 0o600)
	os.WriteFile(filepath.Join(dir, id+".widen.json"), []byte(`[{"host":"x.test","ports":[443]}]`), 0o600)
	trustbrief.Write(dir, id, trustbrief.Brief{SchemaVersion: 1, TaskID: id})
	return &Server{AuditRoot: dir, Token: "secret"}
}

func TestDiffAndLogsAndWiden(t *testing.T) {
	s := auditServer(t)
	id := "0123456789abcdef0123456789abcdef"
	logsWant := `{"type":"drydock_meta","subscription":false,"sensitive":false}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":1200,"total_cost_usd":0.05,"num_turns":3,"src":"broker"}` + "\n"
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
	if len(items) != 1 || items[0].Outcome != "ok (3 turns)" || items[0].OutcomeKey != "ok" ||
		items[0].Cost != "$0.0500" || !items[0].HasDuration || items[0].DurationMs != 1200 {
		t.Fatalf("history item wrong: %+v", items)
	}
}

// TestHistory_DeniedOutcomeFromMetricsRow is the regression test for the
// history rail/table bug: a denied task's result row is the agent's own
// pre-gate success line, so before the metrics row's outcome field existed
// this showed "ok (3 turns)" with a ✓ icon instead of "denied" with a
// neutral glyph.
func TestHistory_DeniedOutcomeFromMetricsRow(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(
		`{"type":"drydock_meta","subscription":false,"sensitive":false}`+"\n"+
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":1200,"total_cost_usd":0.05,"num_turns":3}`+"\n"+
			`{"type":"metrics","src":"broker","task_id":"`+id+`","outcome":"denied"}`+"\n"), 0o600)
	s := &Server{AuditRoot: dir, Token: "secret"}

	rec := do(t, s, "GET", "/api/history", "127.0.0.1:7878", "Bearer secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("history = %d", rec.Code)
	}
	var items []HistoryItem
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Outcome != "denied" || items[0].OutcomeKey != "denied" {
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

func TestBriefRoute(t *testing.T) {
	s := auditServer(t)
	id := "0123456789abcdef0123456789abcdef"
	// 200 + exact body + json content-type
	rec := do(t, s, "GET", "/api/brief/"+id, "127.0.0.1:7878", "Bearer secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("brief GET = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	if !strings.Contains(rec.Body.String(), `"schema_version"`) {
		t.Errorf("brief body missing schema_version:\n%s", rec.Body.String())
	}
	// 404 missing
	if rec := do(t, s, "GET", "/api/brief/ffffffffffffffffffffffffffffffff", "127.0.0.1:7878", "Bearer secret"); rec.Code != http.StatusNotFound {
		t.Errorf("missing brief = %d, want 404", rec.Code)
	}
	// 400 bad id
	if rec := do(t, s, "GET", "/api/brief/NOTHEX", "127.0.0.1:7878", "Bearer secret"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad id = %d, want 400", rec.Code)
	}
}

func TestBriefSymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	s := &Server{AuditRoot: dir, Token: "secret"}
	id := "0123456789abcdef0123456789abcdef"
	if err := os.Symlink("/etc/hosts", filepath.Join(dir, id+trustbrief.Suffix)); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, s, "GET", "/api/brief/"+id, "127.0.0.1:7878", "Bearer secret"); rec.Code != http.StatusNotFound {
		t.Errorf("symlinked brief = %d, want 404", rec.Code)
	}
}

// G4: the history table's cost column must be BROKER-OBSERVED. A trace whose
// only result row is the AGENT's — the row an in-VM CLI writes to its own
// stdout — must not be presented as a measured figure: it is still shown (the
// number exists) but marked, and CostAgentReported says so to the UI.
func TestHistory_AgentReportedCostIsMarked(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(
		`{"type":"drydock_meta","subscription":false,"sensitive":false}`+"\n"+
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":1200,"total_cost_usd":9999.99,"num_turns":3}`+"\n"), 0o600)
	s := &Server{AuditRoot: dir, Token: "secret"}
	rec := do(t, s, "GET", "/api/history", "127.0.0.1:7878", "Bearer secret")
	var items []HistoryItem
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %+v", items)
	}
	if !items[0].CostAgentReported {
		t.Error("CostAgentReported = false for a trace with no broker result row")
	}
	if !strings.HasSuffix(items[0].Cost, audit.AgentReportedCostMark) {
		t.Errorf("Cost = %q, want the agent-reported mark %q appended", items[0].Cost, audit.AgentReportedCostMark)
	}
}

// And the converse, which is the actual defense: a broker row present alongside
// a forged agent row renders the BROKER's number, whichever came last.
func TestHistory_ForgedAgentCostCannotDisplaceTheBrokerFigure(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(
		`{"type":"drydock_meta","subscription":false,"sensitive":false}`+"\n"+
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":1200,"total_cost_usd":2.5,"num_turns":3,"src":"broker"}`+"\n"+
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":1200,"total_cost_usd":0.0001,"num_turns":3}`+"\n"), 0o600)
	s := &Server{AuditRoot: dir, Token: "secret"}
	rec := do(t, s, "GET", "/api/history", "127.0.0.1:7878", "Bearer secret")
	var items []HistoryItem
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Cost != "$2.5000" || items[0].CostAgentReported {
		t.Fatalf("history item = %+v, want the broker-metered $2.5000 unmarked", items)
	}
}
