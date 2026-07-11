package broker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGateMarker_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := gateMarker{RepoRef: "https://github.com/o/r", Instruction: "do x",
		Platform: "github", Agent: "claude", Draft: true, TaskStartMs: 42}
	if err := writeGateMarker(dir, "abc", m); err != nil {
		t.Fatal(err)
	}
	got, err := readGateMarker(dir, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if got != m {
		t.Errorf("round-trip = %+v, want %+v", got, m)
	}

	// Only the marker file, plus an unrelated file, in the dir.
	os.WriteFile(filepath.Join(dir, "abc.diff"), []byte("d"), 0o600)
	all := ListGateMarkers(dir)
	if len(all) != 1 {
		t.Fatalf("ListGateMarkers = %d, want 1", len(all))
	}
	if _, ok := all["abc"]; !ok {
		t.Errorf("marker id abc missing from %v", all)
	}

	if err := removeGateMarker(dir, "abc"); err != nil {
		t.Fatal(err)
	}
	if len(ListGateMarkers(dir)) != 0 {
		t.Error("marker not removed")
	}
}
