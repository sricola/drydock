package broker

import (
	"os"
	"path/filepath"
	"testing"
)

// A gate marker is what makes a task resumable after a restart, so its store
// gets the same defenses as the queue and CI marker stores: the write stages
// through a temp and renames (a planted symlink at <id>.gate.json is
// replaced, never written through) and the read refuses a symlink (parity
// with readQueueItem / readCIMarker).
func TestGateMarker_WriteReplacesPlantedSymlinkInsteadOfWritingThroughIt(t *testing.T) {
	dir := t.TempDir()
	elsewhere := filepath.Join(dir, "elsewhere.json")
	if err := os.WriteFile(elsewhere, []byte(`{"repo_ref":"untouched"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, gateMarkerPath(dir, abcID)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := writeGateMarker(dir, abcID, gateMarker{RepoRef: "r"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(elsewhere)
	if string(got) != `{"repo_ref":"untouched"}` {
		t.Errorf("the symlink target was written through: %s", got)
	}
	if fi, err := os.Lstat(gateMarkerPath(dir, abcID)); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("marker must be a regular file after the write, got %v (%v)", fi, err)
	}
	if m, err := readGateMarker(dir, abcID); err != nil || m.RepoRef != "r" {
		t.Errorf("readGateMarker = %+v, %v; want repo_ref r", m, err)
	}
}

func TestGateMarker_ReadRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "elsewhere.json")
	if err := os.WriteFile(real, []byte(`{"repo_ref":"evil"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, gateMarkerPath(dir, lnkID)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := readGateMarker(dir, lnkID); err == nil {
		t.Error("readGateMarker followed a symlinked marker; want an error")
	}
	if all := ListGateMarkers(dir); len(all) != 0 {
		t.Errorf("ListGateMarkers returned a symlinked marker: %v", all)
	}
}
