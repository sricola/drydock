package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// The status breakdown must surface the setting_up stage: a task mid-setup
// is neither "running" (no agent yet) nor invisible — the operator sees it.
func TestRunStatus_ShowsSettingUpCount(t *testing.T) {
	useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w,
			`{"ok":true,"setting_up":2,"running":1,"verifying":0,"awaiting_egress":0,"pending_approval":3,"pushing":0}`)
	}))
	t.Setenv("AUDIT_ROOT", t.TempDir())

	out := captureStdout(t, runStatus)
	if !strings.Contains(out, "2 setting up") {
		t.Errorf("status breakdown missing the setting-up count:\n%s", out)
	}
	if !strings.Contains(out, "1 running") || !strings.Contains(out, "3 awaiting diff") {
		t.Errorf("status breakdown lost an existing segment:\n%s", out)
	}
}

func TestRunStatus_DegradedLedgerWarningFollowsMaxDenials(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want, not  string
	}{
		{"guards on", `{"ok":true,"rejection_ledger_error":"line 3 is bad","queue_max_denials":2}`,
			"queue adds are refused and dispatch is parked", "nothing is refused"},
		{"guards off", `{"ok":true,"rejection_ledger_error":"line 3 is bad","queue_max_denials":0}`,
			"queue.max_denials is 0, so nothing is refused; the ledger is still recorded", "queue adds are refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Setenv("AUDIT_ROOT", t.TempDir())
			out := captureStdout(t, runStatus)
			if !strings.Contains(out, "WARNING") || !strings.Contains(out, tc.want) || strings.Contains(out, tc.not) {
				t.Errorf("status output wrong:\n%s", out)
			}
		})
	}
}
