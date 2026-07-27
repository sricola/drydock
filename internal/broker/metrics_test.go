package broker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"drydock/internal/creds"
)

type countingGrant struct {
	fakeGrant
	requests int
}

func (g *countingGrant) Requests() int { return g.requests }

type staticProvider struct{ g creds.Grant }

func (p *staticProvider) Mint(float64) (creds.Grant, error) { return p.g, nil }

// lastMetricsLine returns the parsed last {"type":"metrics"} line and asserts
// it is the FINAL line of the audit file (guaranteed-last is the trust rule).
func lastMetricsLine(t *testing.T, auditData string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimRight(auditData, "\n"), "\n")
	var m map[string]any
	if json.Unmarshal([]byte(lines[len(lines)-1]), &m) != nil || m["type"] != "metrics" {
		t.Fatalf("last audit line is not a metrics row:\n%s", auditData)
	}
	return m
}

func TestHandleTask_Success_WritesMetricsRowLast(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{spent: 0.02}
	b := testBroker(t, "anthropic", st, grant, writesResult(`{"type":"result","subtype":"success"}`))
	_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"do x","agent":"claude","auto_approve":true}`)
	id, _ := events[0]["task_id"].(string)

	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	if m["src"] != "broker" || m["task_id"] != id {
		t.Fatalf("metrics row identity wrong: %v", m)
	}
	if m["agent"] != "claude" || m["vendor"] != "anthropic" || m["auth"] != "api_key" {
		t.Errorf("dimensions wrong: agent=%v vendor=%v auth=%v", m["agent"], m["vendor"], m["auth"])
	}
	if repo, _ := m["repo"].(string); repo == "" || strings.Contains(repo, "do x") {
		t.Errorf("repo=%q, want redacted non-empty repo ref", repo)
	}
	if _, ok := m["stage_ms"].(map[string]any); !ok {
		t.Errorf("stage_ms missing: %v", m)
	}
	if m["widen_outcome"] != "none" {
		t.Errorf("widen_outcome=%v, want none (no extras requested)", m["widen_outcome"])
	}
}

func TestHandleTask_AgentError_StillWritesMetricsRow(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir()}
	grant := &fakeGrant{spent: 0.01}
	b := testBroker(t, "anthropic", st, grant,
		func(ctx context.Context, args []string, stdout, stderr io.Writer) error {
			return errors.New("container exploded")
		})
	_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`)
	id, _ := events[0]["task_id"].(string)
	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	if m["src"] != "broker" {
		t.Fatalf("metrics row missing on the error path: %v", m)
	}
}

func TestAppendBrokerResult_NumTurnsFromGrantRequests(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &countingGrant{fakeGrant: fakeGrant{spent: 0.02}, requests: 7}
	b := testBroker(t, "anthropic", st, &grant.fakeGrant, writesResult(`{"type":"result","subtype":"success"}`))
	// testBroker takes *fakeGrant; swap the provider to mint the counting grant instead.
	b.Providers["anthropic"] = &staticProvider{g: grant}
	_, events, _ := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	id, _ := events[0]["task_id"].(string)
	audit := readAudit(t, b.AuditRoot, id)
	if !strings.Contains(audit, `"num_turns":7`) {
		t.Errorf("result row does not carry the lease request count:\n%s", audit)
	}
}
