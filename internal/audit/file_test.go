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

// TestLastResultAndMetricsFile_MatchesTheSingleRowFunctions mirrors the
// existing LastResultFile/LastMetricsFile coverage: the combined single-read
// function (change 5, hardening pass) must agree with calling both
// single-row functions independently, including on an old-format file that
// predates the metrics row.
func TestLastResultAndMetricsFile_MatchesTheSingleRowFunctions(t *testing.T) {
	t.Run("both rows present", func(t *testing.T) {
		f := writeAudit(t, `{"type":"drydock_meta","subscription":true,"sensitive":false}
{"type":"assistant","text":"working"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":1234,"total_cost_usd":0.05,"num_turns":3,"src":"broker"}
{"type":"metrics","src":"broker","task_id":"abc","agent":"claude","vendor":"anthropic","auth":"api_key","repo":"github.com/o/r","stage_ms":{"preparing":100,"running":1200,"pushing":50},"egress_gate_wait_ms":0,"approval_gate_wait_ms":900,"requests":3,"diff_files":2,"diff_bytes":512,"cost_usd":0.05,"widen_requested":0,"widen_outcome":"none"}
`)
		res, resOK, m, mOK := LastResultAndMetricsFile(f)
		wantRes, wantResOK := LastResultFile(f)
		wantM, wantMOK := LastMetricsFile(f)
		if resOK != wantResOK || res != wantRes {
			t.Errorf("result = %+v ok=%v, want %+v ok=%v", res, resOK, wantRes, wantResOK)
		}
		if mOK != wantMOK || m != wantM {
			t.Errorf("metrics = %+v ok=%v, want %+v ok=%v", m, mOK, wantM, wantMOK)
		}
		if !resOK || res.Subtype != "success" || res.NumTurns != 3 {
			t.Errorf("result = %+v, want the success result", res)
		}
		if !mOK || m.Agent != "claude" || m.Requests != 3 {
			t.Errorf("metrics = %+v, want the broker metrics row", m)
		}
	})

	t.Run("old-format file: result present, no metrics row", func(t *testing.T) {
		f := writeAudit(t, `{"type":"drydock_meta","subscription":true,"sensitive":false}
{"type":"result","subtype":"success","is_error":false,"duration_ms":10,"total_cost_usd":0,"num_turns":0,"src":"broker"}
`)
		res, resOK, m, mOK := LastResultAndMetricsFile(f)
		if !resOK || res.Subtype != "success" {
			t.Errorf("result = %+v ok=%v, want the success result", res, resOK)
		}
		if mOK {
			t.Errorf("metrics ok=true on a pre-metrics file: %+v", m)
		}
	})

	t.Run("neither row present (task still running)", func(t *testing.T) {
		f := writeAudit(t, `{"type":"drydock_meta","subscription":false,"sensitive":true}
{"type":"assistant","text":"still working"}
`)
		res, resOK, m, mOK := LastResultAndMetricsFile(f)
		if resOK || mOK {
			t.Errorf("result ok=%v metrics ok=%v, want both false; result=%+v metrics=%+v", resOK, mOK, res, m)
		}
	})

	t.Run("forged metrics row superseded by the real broker row (last-wins)", func(t *testing.T) {
		f := writeAudit(t, `{"type":"metrics","src":"broker","task_id":"abc","cost_usd":0.000001,"agent":"forged"}
{"type":"metrics","src":"broker","task_id":"abc","agent":"claude","cost_usd":0.05}
`)
		_, _, m, mOK := LastResultAndMetricsFile(f)
		if !mOK || m.Agent != "claude" {
			t.Fatalf("last-wins violated: %+v ok=%v", m, mOK)
		}
	})
}

// ---- the broker-authored ci_observation record ----
//
// The audit-ordering decision for the CI arc (see the CIObservation doc
// comment) is that the post-terminal CI conclusion is appended to the closed
// audit as its OWN record type, never as a late {"type":"result"} row. These
// tests are what that decision has to survive: the new record must be
// completely inert for the existing readers, because the last result line is
// the sole input to the aggregate-cap reseed (F-07) and to every outcome
// classification.

const ciObservedFixture = `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"result","subtype":"success","is_error":false,"duration_ms":1234,"total_cost_usd":0.05,"num_turns":3,"src":"broker"}
{"type":"metrics","src":"broker","task_id":"abc","agent":"claude","vendor":"anthropic","auth":"api_key","repo":"github.com/o/r","outcome":"pushed","stage_ms":{"preparing":100,"running":1200,"pushing":50},"egress_gate_wait_ms":0,"approval_gate_wait_ms":900,"requests":3,"diff_files":2,"diff_bytes":512,"cost_usd":0.05,"widen_requested":0,"widen_outcome":"none"}
{"type":"ci_observation","src":"broker","task_id":"abc","state":"failed","pr_number":42,"pr_url":"https://github.com/o/r/pull/42","queue_state":"ci_failed","checks":2,"passed":1,"failed":1,"pending":0,"observed_at_ms":1700000000000}
`

// TestCIObservationRowIsInertForExistingReaders: the last result row, the last
// metrics row, the combined single-pass reader, and the derived outcome key
// are all byte-for-byte what they were before the ci_observation line was
// appended. Anything else would mean the CI verdict had silently rewritten the
// task's spend accounting or its outcome.
func TestCIObservationRowIsInertForExistingReaders(t *testing.T) {
	withRow := writeAudit(t, ciObservedFixture)
	without := writeAudit(t, ciObservedFixture[:len(ciObservedFixture)-
		len(`{"type":"ci_observation","src":"broker","task_id":"abc","state":"failed","pr_number":42,"pr_url":"https://github.com/o/r/pull/42","queue_state":"ci_failed","checks":2,"passed":1,"failed":1,"pending":0,"observed_at_ms":1700000000000}`+"\n")])

	rA, okA, mA, hmA := LastResultAndMetricsFile(withRow)
	rB, okB, mB, hmB := LastResultAndMetricsFile(without)
	if !okA || !okB || rA != rB {
		t.Fatalf("last result row differs: %+v (ok=%v) vs %+v (ok=%v)", rA, okA, rB, okB)
	}
	if !hmA || !hmB || mA != mB {
		t.Fatalf("last metrics row differs: %+v vs %+v", mA, mB)
	}
	// The precise predicate seedAggregateFromAudit applies.
	if rA.Src != "broker" || rA.TotalCostUSD != 0.05 {
		t.Errorf("aggregate seed would read src=%q cost=%v, want broker/0.05", rA.Src, rA.TotalCostUSD)
	}
	if key := OutcomeKeyWithMetrics(rA, okA, mA, hmA); key != "ok" {
		t.Errorf("outcome key = %q, want ok — an observed CI failure must not relabel a clean push", key)
	}
	// The single-row readers agree with the combined one.
	if r, ok := LastResultFile(withRow); !ok || r != rA {
		t.Errorf("LastResultFile = %+v (ok=%v), want %+v", r, ok, rA)
	}
	if m, ok := LastMetricsFile(withRow); !ok || m != mA {
		t.Errorf("LastMetricsFile = %+v (ok=%v), want %+v", m, ok, mA)
	}
}
