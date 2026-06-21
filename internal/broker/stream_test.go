package broker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiffStat(t *testing.T) {
	diff := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1,2 @@\n-old\n+new\n+more\n"
	files, ins, del := diffStat(diff)
	if files != 1 || ins != 2 || del != 1 {
		t.Errorf("diffStat = (%d,%d,%d), want (1,2,1)", files, ins, del)
	}
}

func TestReasonFromAudit_LastInfraLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	content := `{"type":"drydock_meta","subscription":true}` + "\n" +
		"[6/6] Starting container [0s]\n" +
		"/usr/local/bin/entrypoint.sh: line 5: DRYDOCK_GW_IP: missing gateway ip\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := reasonFromAudit(path)
	want := "/usr/local/bin/entrypoint.sh: line 5: DRYDOCK_GW_IP: missing gateway ip"
	if !ok || got != want {
		t.Errorf("reasonFromAudit = %q,%v; want %q,true", got, ok, want)
	}
}

func TestReasonFromAudit_NoMeaningfulLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"result"}`+"\n[1/6] x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := reasonFromAudit(path); ok {
		t.Error("want ok=false when only JSON/progress lines are present")
	}
}

func TestAuditCost_LastResultLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	body := `{"type":"result","total_cost_usd":0.05}` + "\n" +
		`{"type":"result","total_cost_usd":0.11}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := auditCost(path); c != 0.11 {
		t.Errorf("auditCost = %v, want 0.11", c)
	}
}
