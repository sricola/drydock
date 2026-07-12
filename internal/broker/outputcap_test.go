package broker

import (
	"bytes"
	"testing"
)

func TestOutputCap_StopsForwardingAndFiresOnce(t *testing.T) {
	var buf bytes.Buffer
	fires := 0
	oc := newOutputCap(10, func() { fires++ })
	w := oc.wrap(&buf)

	mustWrite(t, w, "12345")  // under the cap: forwarded
	mustWrite(t, w, "678901") // 5+6=11 > 10: crosses, swallowed, fires
	mustWrite(t, w, "more")   // already fired: swallowed

	if got := buf.String(); got != "12345" {
		t.Errorf("forwarded %q, want only the pre-cap bytes %q", got, "12345")
	}
	if !oc.exceeded() {
		t.Error("exceeded() = false, want true after crossing the cap")
	}
	if fires != 1 {
		t.Errorf("onExceed fired %d times, want exactly 1", fires)
	}
}

func TestOutputCap_SharedBudgetAcrossStdoutAndStderr(t *testing.T) {
	var out, errb bytes.Buffer
	fires := 0
	oc := newOutputCap(10, func() { fires++ })
	stdout := oc.wrap(&out)
	stderr := oc.wrap(&errb)

	mustWrite(t, stdout, "aaaaaa") // 6
	mustWrite(t, stderr, "bbbbbb") // +6 = 12 > 10: the combined total trips it
	mustWrite(t, stderr, "cccc")   // swallowed

	if fires != 1 {
		t.Errorf("onExceed fired %d times, want 1 (shared budget)", fires)
	}
	if !oc.exceeded() {
		t.Error("want exceeded after the combined overflow")
	}
	if errb.String() != "" {
		t.Errorf("stderr forwarded %q after the cap, want empty", errb.String())
	}
}

func mustWrite(t *testing.T, w interface{ Write([]byte) (int, error) }, s string) {
	t.Helper()
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatalf("write %q: %v", s, err)
	}
}
