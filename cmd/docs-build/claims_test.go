package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecurityClaimsNoDrift pins high-risk operator-facing security claims that
// had drifted from the code (F-10). It fails if a corrected-away phrase
// reappears, so a doc or config edit cannot silently regress to a misleading
// claim about financial or containment posture. The blacklist below is the
// regression half; the generated site/docs/security-defaults.md (claims.go) is
// the affirmative single source of truth. Sits with the version-currency
// guards for the same reason: an operator reads these to decide whether
// unattended execution is acceptable.
func TestSecurityClaimsNoDrift(t *testing.T) {
	root := repoRoot(t)
	forbidden := []struct{ file, phrase, why string }{
		{"config/config.yaml", "hard USD ceiling", "task_budget_usd is a soft, post-hoc cap (F-02/F-10)"},
		{"internal/config/config.go", "hard USD ceiling", "the embedded config seed must match config.yaml"},
		{"cmd/drydock/daemon.go", "no aggregate spend cap yet", "the aggregate cap landed; point at aggregate_budget_usd"},
		{"README.md", "no-aggregate-cap", "the aggregate cap landed"},
		{"THREAT_MODEL.md", "gosu agent", "privilege drop uses setpriv via drop-agent.sh, not gosu"},
		{"THREAT_MODEL.md", "bounded by one call", "a hostile agent fires concurrent requests, not sequentially (F-02)"},
		{"README.md", "budget-capped token", "spend can overshoot by task_max_inflight requests (default 1); say budget-scoped and state the bound (F-02)"},
		{"site/docs/quickstart.md", "budget-capped token", "same as README (F-02)"},
		{"THREAT_MODEL.md", "budget-capped bearer", "same bound applies to the bearer description (F-02)"},
		{"docs/ROADMAP.md", "every external input is pinned", "apt and npm transitive graphs still float at image build (F-09)"},
	}
	for _, f := range forbidden {
		b, err := os.ReadFile(filepath.Join(root, f.file))
		if err != nil {
			t.Fatalf("read %s: %v", f.file, err)
		}
		if strings.Contains(string(b), f.phrase) {
			t.Errorf("%s reintroduced the stale claim %q; %s", f.file, f.phrase, f.why)
		}
	}
}

// The security-defaults page is generated from code (config.Defaults() and
// exported constants). A committed copy that no longer matches a fresh
// render means a default changed without regenerating docs: exactly the
// drift F-10 is about.
func TestSecurityDefaultsPageCurrent(t *testing.T) {
	root := repoRoot(t)
	want := renderClaims(securityClaims())
	got, err := os.ReadFile(filepath.Join(root, "site", "docs", "security-defaults.md"))
	if err != nil {
		t.Fatalf("read generated page: %v (run `go run ./cmd/docs-build` from the repo root)", err)
	}
	if string(got) != want {
		t.Error("site/docs/security-defaults.md is stale; run `go run ./cmd/docs-build` and commit the result")
	}
}

// Every claim row must cite a real enforcing test: an affirmative check, not
// just a forbidden-phrase blacklist. A renamed or deleted test surfaces here.
func TestSecurityDefaultsVerifiedByTestsExist(t *testing.T) {
	root := repoRoot(t)
	var testSrc strings.Builder
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "site" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			testSrc.Write(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	all := testSrc.String()
	for _, c := range securityClaims() {
		if c.Test == "" {
			t.Errorf("claim %q cites no enforcing test; every row must", c.Setting)
			continue
		}
		if !strings.Contains(all, "func "+c.Test+"(") {
			t.Errorf("claim %q cites test %q, which no longer exists", c.Setting, c.Test)
		}
	}
}
