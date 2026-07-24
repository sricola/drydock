package broker

import (
	"bytes"
	"sync"
	"testing"
)

// recordingWriter records each Write's bytes as a separate entry. It is NOT
// goroutine-safe on purpose: if two capWriters sharing one outputCap were to
// forward concurrently (the stdout+stderr-into-one-audit-file case), the race
// detector flags the concurrent append here — which is exactly the mid-line
// interleave the lock-across-forward prevents.
type recordingWriter struct{ writes [][]byte }

func (r *recordingWriter) Write(p []byte) (int, error) {
	cp := append([]byte(nil), p...)
	r.writes = append(r.writes, cp)
	return len(p), nil
}

// TestOutputCap_SerializesConcurrentWriters drives the shared stdout+stderr
// writers concurrently into one sink; under `-race` this fails if capWriter
// forwards without holding the lock.
func TestOutputCap_SerializesConcurrentWriters(t *testing.T) {
	sink := &recordingWriter{}
	oc := newOutputCap(1<<30, func() {})
	wOut, wErr := oc.wrap(sink), oc.wrap(sink)

	var wg sync.WaitGroup
	for _, w := range []interface{ Write([]byte) (int, error) }{wOut, wErr} {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				_, _ = w.Write([]byte("a stream-json line\n"))
			}
		}()
	}
	wg.Wait()
	if len(sink.writes) != 1000 {
		t.Fatalf("got %d recorded writes, want 1000 (all forwarded, serialized)", len(sink.writes))
	}
}

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
