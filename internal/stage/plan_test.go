package stage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ReadPlan reads agent-authored output (.task/plan.md) back OUT of the
// untrusted work tree onto the trusted host. The same F-01 posture as
// WriteTaskFiles applies from the read side: a hostile repo (or the agent VM
// itself) can plant .task or plan.md as a symlink to an operator file, and a
// followed link would exfiltrate that file's contents into the plan artifact.

func writePlan(t *testing.T, s *Stage, text string) {
	t.Helper()
	dir := filepath.Join(s.WorkDir, ".task")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStage_ReadPlan_ReadsPlanFile(t *testing.T) {
	s := prepare(t, makeOriginRepo(t))
	writePlan(t, s, "## Plan\n1. do x\n")
	got, ok := s.ReadPlan()
	if !ok || got != "## Plan\n1. do x\n" {
		t.Fatalf("ReadPlan = (%q, %v), want the plan text and true", got, ok)
	}
}

func TestStage_ReadPlan_AbsentReturnsFalse(t *testing.T) {
	s := prepare(t, makeOriginRepo(t))
	if got, ok := s.ReadPlan(); ok || got != "" {
		t.Fatalf("ReadPlan on missing plan = (%q, %v), want (\"\", false)", got, ok)
	}
}

func TestStage_ReadPlan_RefusesSymlink(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("OPERATOR-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := prepare(t, makeHostileOriginRepo(t, ".task/plan.md", secret))
	got, ok := s.ReadPlan()
	if ok || got != "" {
		t.Errorf("ReadPlan followed a symlinked plan.md: (%q, %v), want (\"\", false)", got, ok)
	}
	if strings.Contains(got, "OPERATOR-SECRET") {
		t.Error("ReadPlan exfiltrated a host file through a symlinked plan.md")
	}
}

func TestStage_ReadPlan_RefusesSymlinkedTaskDir(t *testing.T) {
	victimDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(victimDir, "plan.md"), []byte("OPERATOR-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := prepare(t, makeHostileOriginRepo(t, ".task", victimDir))
	got, ok := s.ReadPlan()
	if ok || got != "" {
		t.Errorf("ReadPlan read through a symlinked .task: (%q, %v), want (\"\", false)", got, ok)
	}
}

func TestStage_ReadPlan_CapsOversizedPlan(t *testing.T) {
	s := prepare(t, makeOriginRepo(t))
	writePlan(t, s, strings.Repeat("x", MaxPlanBytes+4096))
	got, ok := s.ReadPlan()
	if !ok {
		t.Fatal("ReadPlan = false on an oversized plan, want a capped read")
	}
	if len(got) != MaxPlanBytes {
		t.Errorf("len = %d, want capped at MaxPlanBytes (%d)", len(got), MaxPlanBytes)
	}
}
