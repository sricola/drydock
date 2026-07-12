package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecurityClaimsNoDrift pins high-risk operator-facing security claims that
// had drifted from the code (F-10). It fails if a corrected-away phrase
// reappears, so a doc or config edit cannot silently regress to a misleading
// claim about financial or containment posture. This is the lightweight half of
// "one source of truth for capability claims"; a fully generated table remains a
// follow-up. Sits with the version-currency guards for the same reason: an
// operator reads these to decide whether unattended execution is acceptable.
func TestSecurityClaimsNoDrift(t *testing.T) {
	root := repoRoot(t)
	forbidden := []struct{ file, phrase, why string }{
		{"config/config.yaml", "hard USD ceiling", "task_budget_usd is a soft, post-hoc cap (F-02/F-10)"},
		{"internal/config/config.go", "hard USD ceiling", "the embedded config seed must match config.yaml"},
		{"cmd/drydock/daemon.go", "no aggregate spend cap yet", "the aggregate cap landed; point at aggregate_budget_usd"},
		{"README.md", "no-aggregate-cap", "the aggregate cap landed"},
		{"THREAT_MODEL.md", "gosu agent", "privilege drop uses setpriv via drop-agent.sh, not gosu"},
		{"THREAT_MODEL.md", "bounded by one call", "a hostile agent fires concurrent requests, not sequentially (F-02)"},
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
