package webui

import (
	"strings"
	"testing"
)

// The web UI's CI vocabulary is expressed in two hand-maintained maps in
// app.js (the stage-badge labels and the history rail's outcome
// classification) plus their style rules. Nothing in Go can type-check them,
// so these tests read the embedded assets and assert the vocabulary is
// present and — the part that actually matters — classified on the correct
// side.

func asset(t *testing.T, name string) string {
	t.Helper()
	b, err := assetsFS.ReadFile("assets/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// TestAppJS_CIVocabulary: awaiting_ci has a stage-badge label, ci_failed is
// classified as an ERROR outcome, and — the load-bearing assertion — neither
// is in the set that earns the green checkmark. A CI verdict that was never
// observed reaches the operator as `dead_letter`, which is already red; the
// checkmark set must stay exactly {ok, completed}.
func TestAppJS_CIVocabulary(t *testing.T) {
	js := asset(t, "app.js")
	if !strings.Contains(js, "awaiting_ci:") {
		t.Error("app.js stage-badge label map is missing awaiting_ci")
	}
	if !strings.Contains(js, `it.outcome_key === "ci_failed"`) {
		t.Error("app.js does not classify the ci_failed outcome key")
	}
	// The isOk line is the whole risk surface: a green check must never be
	// the rendering for a CI failure or for an unobserved outcome.
	for _, line := range strings.Split(js, "\n") {
		if !strings.Contains(line, "const isOk") {
			continue
		}
		for _, forbidden := range []string{"ci_failed", "dead_letter", "timed_out", "unknown"} {
			if strings.Contains(line, forbidden) {
				t.Errorf("app.js grants the OK checkmark to %q: %s", forbidden, strings.TrimSpace(line))
			}
		}
		if !strings.Contains(line, `"ok"`) || !strings.Contains(line, `"completed"`) {
			t.Errorf("the isOk set changed unexpectedly: %s", strings.TrimSpace(line))
		}
	}
}

// TestStyleCSS_CIBadgeClasses: the badge classes app.js emits must have rules,
// or the new states render unstyled.
func TestStyleCSS_CIBadgeClasses(t *testing.T) {
	css := asset(t, "style.css")
	for _, cls := range []string{".stage-awaiting_ci", ".stage-ci_failed"} {
		if !strings.Contains(css, cls) {
			t.Errorf("style.css has no rule for %s", cls)
		}
	}
}
