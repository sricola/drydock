package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// These tests exist because of a bug that a string-grep over the assets could
// not see, and in fact HID.
//
// The web UI once carried an `outcome_key === "ci_failed"` branch and an
// `awaiting_ci` stage-badge label. Both were unreachable. `outcome_key` is
// produced by audit.OutcomeKeyWithMetrics from a task's LAST {"type":"result"}
// row, and NOTHING writes a result row for a CI observation — deliberately, so
// an observed CI failure can never relabel a push that landed exactly as asked
// (see audit.CIObservation's "WHY THIS IS NOT A result ROW"). `stage` comes
// from the LIVE-task stream, and an awaiting_ci item has no live task; the web
// UI has no queue view at all. So a task whose CI the broker OBSERVED FAIL
// rendered as a green check, while the CHANGELOG and the docs claimed the UI
// surfaced it.
//
// The old test asserted those strings were PRESENT in the asset bytes, which
// passed with no producer anywhere. These assert the real property instead:
// what the pipeline actually produces (first test), and that the assets claim
// no vocabulary beyond it (second test).

func asset(t *testing.T, name string) string {
	t.Helper()
	b, err := assetsFS.ReadFile("assets/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// TestHistory_ObservedCIFailureStillClassifiesAsACleanPush is the producer-side
// half: it builds exactly what the broker writes for a task whose push landed
// and whose CI was then OBSERVED to fail — a `success` result row, a metrics
// row, and the broker-authored ci_observation record — and asserts the history
// API classifies it "ok".
//
// That is CORRECT and intended: the task did what it was asked to. It is also
// the proof that "ci_failed" can never reach the history strip, so any UI code
// branching on it is dead. If a future change DOES make this API carry a CI
// verdict, this test fails and the UI vocabulary can be added back alongside it
// — in that order.
func TestHistory_ObservedCIFailureStillClassifiesAsACleanPush(t *testing.T) {
	dir := t.TempDir()
	id := strings.Repeat("a", 32)
	trace := `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"result","subtype":"success","is_error":false,"duration_ms":1234,"total_cost_usd":0.05,"num_turns":3,"src":"broker"}
{"type":"metrics","src":"broker","task_id":"` + id + `","agent":"claude","vendor":"anthropic","outcome":"pushed","cost_usd":0.05}
{"type":"ci_observation","src":"broker","task_id":"` + id + `","state":"failed","pr_number":42,"queue_state":"ci_failed","checks":2,"passed":1,"failed":1,"observed_at_ms":1700000000000}
`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(trace), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{AuditRoot: dir}
	rr := httptest.NewRecorder()
	s.handleHistory(rr, httptest.NewRequest(http.MethodGet, "/api/history", nil))
	var items []HistoryItem
	if err := json.Unmarshal(rr.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode history: %v (%s)", err, rr.Body.String())
	}
	if len(items) != 1 {
		t.Fatalf("history items = %d, want 1", len(items))
	}
	if items[0].OutcomeKey != "ok" {
		t.Fatalf("outcome_key = %q, want \"ok\" — a CI verdict must not relabel the task's own terminal", items[0].OutcomeKey)
	}
}

// outcomeKeyRE finds every outcome_key literal app.js branches on.
var outcomeKeyRE = regexp.MustCompile(`outcome_key === "([a-z_]+)"`)

// stripJSComments drops whole-line // comments. app.js uses no block comments,
// and a trailing-comment false positive would only ever make a test stricter.
func stripJSComments(js string) string {
	var kept []string
	for _, line := range strings.Split(js, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// TestAppJS_ClaimsNoVocabularyThePipelineCannotProduce is the asset-side half.
// Every outcome_key the UI branches on must be a key
// audit.OutcomeKeyWithMetrics can actually return, and the assets must name no
// CI vocabulary at all. A label for a state that cannot arrive is not harmless
// decoration: it is a claim to the operator — and, until this commit, to the
// CHANGELOG — that a surface works.
func TestAppJS_ClaimsNoVocabularyThePipelineCannotProduce(t *testing.T) {
	// Comment lines are stripped: the file explains at length why the CI
	// vocabulary is absent, and that prose must not read as the vocabulary
	// being present. Only executable text is checked.
	js := stripJSComments(asset(t, "app.js"))

	// The keys audit can emit: OutcomeKey's own returns, plus the broker-written
	// result subtypes and the metrics-row refinements it passes through.
	// Notably NOT "ci_failed", which is a QUEUE terminal and never a result
	// subtype.
	producible := map[string]bool{
		"running": true, "interrupted": true, "push_failed": true,
		"error": true, "ok": true, "denied": true, "cancelled": true,
		"planned": true, "setup_failed": true, "verify_failed": true,
		"policy_blocked": true, "dead_letter": true, "completed": true,
	}
	for _, m := range outcomeKeyRE.FindAllStringSubmatch(js, -1) {
		if !producible[m[1]] {
			t.Errorf("app.js branches on outcome_key %q, which audit.OutcomeKeyWithMetrics never produces; the branch is dead and the surface it implies does not exist", m[1])
		}
	}
	// CI vocabulary specifically: it belongs on the queue surfaces
	// (`drydock queue list`, GET /queue), which the web UI does not render.
	css := asset(t, "style.css")
	for _, dead := range []string{"ci_failed", "awaiting_ci"} {
		if strings.Contains(js, dead) {
			t.Errorf("app.js mentions %q; the web UI has no queue view, so nothing can ever deliver it", dead)
		}
		if strings.Contains(css, dead) {
			t.Errorf("style.css styles %q, a badge class app.js can never emit", dead)
		}
	}
}

// TestAppJS_OKGlyphSetIsExactlyOkAndCompleted is unchanged in spirit from the
// original, and is the one assertion that must never relax: a green checkmark
// must never be the rendering for a failure or for an outcome nobody observed.
func TestAppJS_OKGlyphSetIsExactlyOkAndCompleted(t *testing.T) {
	js := asset(t, "app.js")
	found := false
	for _, line := range strings.Split(js, "\n") {
		if !strings.Contains(line, "const isOk") {
			continue
		}
		found = true
		for _, forbidden := range []string{"ci_failed", "dead_letter", "timed_out", "unknown"} {
			if strings.Contains(line, forbidden) {
				t.Errorf("app.js grants the OK checkmark to %q: %s", forbidden, strings.TrimSpace(line))
			}
		}
		if !strings.Contains(line, `"ok"`) || !strings.Contains(line, `"completed"`) {
			t.Errorf("the isOk set changed unexpectedly: %s", strings.TrimSpace(line))
		}
	}
	if !found {
		t.Error("app.js has no isOk classification line; the history glyph logic moved without this test moving with it")
	}
}
