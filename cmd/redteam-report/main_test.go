package main

import (
	"strings"
	"testing"
)

// cannedJSON holds a minimal set of go test -json events:
//   - A1 test passes
//   - A6 test fails
//   - non-A test (TestCaptureDiff_ExcludesTaskDir) passes
//   - a package-level event (no Test field) that must be ignored
//   - a non-JSON line (build noise) that must not crash the parser
const cannedJSON = `
not-json-at-all
{"Action":"run","Package":"drydock/cmd/drydock","Test":"TestRedteam_A1_RealKeyNeverInVM"}
{"Action":"pass","Package":"drydock/cmd/drydock","Test":"TestRedteam_A1_RealKeyNeverInVM","Elapsed":0.01}
{"Action":"run","Package":"drydock/cmd/drydock","Test":"TestRedteam_A6_EgressWidenDenied"}
{"Action":"fail","Package":"drydock/cmd/drydock","Test":"TestRedteam_A6_EgressWidenDenied","Elapsed":0.02}
{"Action":"run","Package":"drydock/tests/integration","Test":"TestRedteam_V1_VerifierVMHasNoNetworkAndNoCredentials"}
{"Action":"pass","Package":"drydock/tests/integration","Test":"TestRedteam_V1_VerifierVMHasNoNetworkAndNoCredentials","Elapsed":3.5}
{"Action":"run","Package":"drydock/internal/egress","Test":"TestIPV4Literal_Rejected"}
{"Action":"pass","Package":"drydock/internal/egress","Test":"TestIPV4Literal_Rejected","Elapsed":0.01}
{"Action":"run","Package":"drydock/cmd/drydock","Test":"TestCaptureDiff_ExcludesTaskDir"}
{"Action":"pass","Package":"drydock/cmd/drydock","Test":"TestCaptureDiff_ExcludesTaskDir","Elapsed":0.01}
{"Action":"fail","Package":"drydock/cmd/drydock","Elapsed":0.10}
`

func TestSummarize_PerClaimVerdicts(t *testing.T) {
	rep, allGreen := summarize(strings.NewReader(cannedJSON))

	// A1 must be green
	a1, ok := rep["A1"]
	if !ok {
		t.Fatal("claim A1 missing from report")
	}
	if a1.failed > 0 {
		t.Errorf("A1: expected no failures, got %d", a1.failed)
	}
	if a1.passed == 0 {
		t.Error("A1: expected at least one passing test")
	}

	// A6 must be red
	a6, ok := rep["A6"]
	if !ok {
		t.Fatal("claim A6 missing from report")
	}
	if a6.failed == 0 {
		t.Error("A6: expected at least one failure")
	}

	// V1 (the verifier-VM claim) is a claim code too, grouped like the A-series.
	v1, ok := rep["V1"]
	if !ok {
		t.Fatal("claim V1 missing from report (verifier-VM tests must group under their claim code, not their raw name)")
	}
	if v1.passed != 1 || v1.failed != 0 {
		t.Errorf("V1: passed=%d failed=%d, want 1/0", v1.passed, v1.failed)
	}

	// A non-red-team name containing V<digit> must not be grouped as a claim.
	if _, ok := rep["V4"]; ok {
		t.Error("TestIPV4Literal_Rejected was grouped under claim V4")
	}
	if _, ok := rep["TestIPV4Literal_Rejected"]; !ok {
		t.Error("TestIPV4Literal_Rejected missing under its own name")
	}

	// non-A test must appear under its own key
	key := "TestCaptureDiff_ExcludesTaskDir"
	tc, ok := rep[key]
	if !ok {
		t.Fatalf("non-A claim %q missing from report", key)
	}
	if tc.failed > 0 {
		t.Errorf("%s: expected no failures, got %d", key, tc.failed)
	}
	if tc.passed == 0 {
		t.Errorf("%s: expected at least one passing test", key)
	}

	// package-level event must not add a spurious claim
	if _, ok := rep[""]; ok {
		t.Error("empty-key claim present: package-level event was not filtered")
	}

	// overall verdict must be false (A6 failed)
	if allGreen {
		t.Error("allGreen should be false when A6 failed")
	}
}

func TestSummarize_AllPass(t *testing.T) {
	input := `
{"Action":"pass","Package":"drydock/cmd/drydock","Test":"TestRedteam_A3_FirewallImmutable","Elapsed":0.01}
{"Action":"pass","Package":"drydock/cmd/drydock","Test":"TestRedteam_A4_HostFSReadOnly","Elapsed":0.01}
`
	rep, allGreen := summarize(strings.NewReader(input))

	if !allGreen {
		t.Error("allGreen should be true when all tests pass")
	}
	for _, k := range []string{"A3", "A4"} {
		cl, ok := rep[k]
		if !ok {
			t.Errorf("claim %s missing", k)
			continue
		}
		if cl.failed > 0 {
			t.Errorf("claim %s: unexpected failures %d", k, cl.failed)
		}
	}
}

func TestSummarize_SkipOnly(t *testing.T) {
	input := `{"Action":"skip","Package":"drydock/cmd/drydock","Test":"TestRedteam_A5_GateBlocksUnapprovedPush","Elapsed":0.00}`
	rep, allGreen := summarize(strings.NewReader(input))

	// a claim with only skips is not a failure
	if !allGreen {
		t.Error("allGreen should be true when only skips, no failures")
	}
	a5, ok := rep["A5"]
	if !ok {
		t.Fatal("claim A5 missing from report")
	}
	if a5.failed > 0 {
		t.Errorf("A5: expected no failures, got %d", a5.failed)
	}
	if a5.skipped == 0 {
		t.Error("A5: expected at least one skipped test")
	}
}

func TestSummarize_EmptyInput(t *testing.T) {
	_, allGreen := summarize(strings.NewReader(""))
	if !allGreen {
		t.Error("allGreen should be true when input is empty (no failures)")
	}
}
