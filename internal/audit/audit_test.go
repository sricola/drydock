package audit

import (
	"os"
	"path/filepath"
	"testing"
)

func writeJSONL(t *testing.T, lines ...string) (path string, size int64) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "id.jsonl")
	data := ""
	for _, l := range lines {
		data += l + "\n"
	}
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	return p, fi.Size()
}

func TestOutcomeAndCost(t *testing.T) {
	meta := `{"type":"drydock_meta","subscription":false,"sensitive":false}`
	subMeta := `{"type":"drydock_meta","subscription":true,"sensitive":false}`
	sens := `{"type":"drydock_meta","subscription":false,"sensitive":true}`

	cases := []struct {
		name        string
		lines       []string
		wantOutcome string
		wantCost    string
		wantDur     bool
	}{
		{"success with turns", []string{meta, `{"type":"result","subtype":"success","is_error":false,"duration_ms":12,"total_cost_usd":0.0731,"num_turns":2}`}, "ok (2 turn)", "$0.0731", true},
		{"success no turns", []string{meta, `{"type":"result","subtype":"success","is_error":false,"duration_ms":12,"total_cost_usd":0,"num_turns":0}`}, "ok", "$0.0000", true},
		{"error", []string{meta, `{"type":"result","subtype":"error","is_error":true,"duration_ms":5,"total_cost_usd":0.01,"num_turns":1}`}, "error", "$0.0100", true},
		{"interrupted", []string{meta, `{"type":"result","subtype":"interrupted","is_error":false,"duration_ms":0,"total_cost_usd":0,"num_turns":0}`}, "interrupted", "$0.0000", false},
		{"subscription", []string{subMeta, `{"type":"result","subtype":"success","is_error":false,"duration_ms":3,"total_cost_usd":0,"num_turns":1}`}, "ok (1 turn)", "subscription", true},
		{"sensitive suffix", []string{sens, `{"type":"result","subtype":"success","is_error":false,"duration_ms":3,"total_cost_usd":0,"num_turns":1}`}, "ok (1 turn) · sensitive", "$0.0000", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, sz := writeJSONL(t, tc.lines...)
			last, ok := LastResult(p, sz)
			if !ok {
				t.Fatal("expected a result line")
			}
			m := ReadMeta(p)
			if got := Outcome(last, ok, m); got != tc.wantOutcome {
				t.Errorf("Outcome = %q, want %q", got, tc.wantOutcome)
			}
			if got := Cost(m, last, ok); got != tc.wantCost {
				t.Errorf("Cost = %q, want %q", got, tc.wantCost)
			}
			if got := HasDuration(last, ok); got != tc.wantDur {
				t.Errorf("HasDuration = %v, want %v", got, tc.wantDur)
			}
		})
	}
}

func TestNoResultLine(t *testing.T) {
	p, sz := writeJSONL(t, `{"type":"drydock_meta","subscription":false,"sensitive":false}`)
	last, ok := LastResult(p, sz)
	if ok {
		t.Fatal("expected ok=false with no result line")
	}
	if Outcome(last, ok, ReadMeta(p)) != "running?" {
		t.Errorf("Outcome should be running? when no result")
	}
	if Cost(ReadMeta(p), last, ok) != "-" {
		t.Errorf("Cost should be - when no result")
	}
}
