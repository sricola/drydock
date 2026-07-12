package audit

import (
	"os"
	"path/filepath"
	"testing"
)

// ReadMetaFile and LastResultFile operate on a pre-opened *os.File (the caller
// controls the open flags, e.g. O_NOFOLLOW). Both reset the offset before
// reading, so a caller may interleave them in any order on the same handle.
// These tests lock that order-independence and the parsed values.

func writeAudit(t *testing.T, lines string) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "task.jsonl")
	if err := os.WriteFile(p, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestReadMetaFile_And_LastResultFile_OrderIndependent(t *testing.T) {
	f := writeAudit(t, `{"type":"drydock_meta","subscription":true,"sensitive":false}
{"type":"assistant","text":"working"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":1234,"total_cost_usd":0.05,"num_turns":3}
`)

	// Read result first, then meta, then result again: each call must seek, so
	// the interleaving does not corrupt the other's read.
	r1, ok1 := LastResultFile(f)
	m := ReadMetaFile(f)
	r2, ok2 := LastResultFile(f)

	if !m.Subscription || m.Type != "drydock_meta" {
		t.Errorf("ReadMetaFile = %+v, want subscription meta", m)
	}
	for i, got := range []struct {
		r  Result
		ok bool
	}{{r1, ok1}, {r2, ok2}} {
		if !got.ok {
			t.Fatalf("LastResultFile call %d: ok=false, want a result", i)
		}
		if got.r.Subtype != "success" || got.r.TotalCostUSD != 0.05 || got.r.NumTurns != 3 || got.r.DurationMs != 1234 {
			t.Errorf("LastResultFile call %d = %+v, want the success result", i, got.r)
		}
	}
}

func TestLastResultFile_AbsentResultReturnsFalse(t *testing.T) {
	// Meta present but no result line (task still running / killed early).
	f := writeAudit(t, `{"type":"drydock_meta","subscription":false,"sensitive":true}
{"type":"assistant","text":"still working"}
`)

	if _, ok := LastResultFile(f); ok {
		t.Error("LastResultFile ok=true, want false when no result line is present")
	}
	// Meta is still readable from the same handle after the tail scan.
	if m := ReadMetaFile(f); !m.Sensitive {
		t.Errorf("ReadMetaFile = %+v, want sensitive meta", m)
	}
}
