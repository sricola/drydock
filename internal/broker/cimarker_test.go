package broker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCIID = "fedcba9876543210fedcba9876543210"

func sampleCIMarker() ciMarker {
	return ciMarker{
		TaskID:      testCIID,
		RepoRef:     "https://github.com/example/repo.git",
		Branch:      "agent/fedcba9876543210fedcba9876543210-2",
		PRNumber:    42,
		PRURL:       "https://github.com/example/repo/pull/42",
		Attempt:     2,
		RetryOf:     "0123456789abcdef0123456789abcdef",
		State:       CIPending,
		CreatedAtMs: 1700000000123,
		UpdatedAtMs: 1700000000456,
		DeadlineMs:  1700000900000,
	}
}

func TestCIMarkerRoundTrip(t *testing.T) {
	root := t.TempDir()
	want := sampleCIMarker()
	if err := writeCIMarker(root, want); err != nil {
		t.Fatalf("writeCIMarker: %v", err)
	}
	fi, err := os.Stat(ciMarkerPath(root, want.TaskID))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 0600", perm)
	}
	got, err := readCIMarker(root, want.TaskID)
	if err != nil {
		t.Fatalf("readCIMarker: %v", err)
	}
	if got != want {
		t.Errorf("round trip mangled the marker:\n got %+v\nwant %+v", got, want)
	}
	// The retry-chain fields must survive verbatim: the bound they encode has
	// to outlive a restart (D6).
	if got.Attempt != 2 || got.RetryOf != want.RetryOf {
		t.Errorf("retry chain not preserved: attempt=%d retry_of=%q", got.Attempt, got.RetryOf)
	}
}

func TestWriteCIMarkerRejectsBadID(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"", "short", "../../etc/passwd", strings.Repeat("g", 32), strings.Repeat("A", 32), strings.Repeat("a", 31)} {
		m := sampleCIMarker()
		m.TaskID = id
		if err := writeCIMarker(root, m); err == nil {
			t.Errorf("writeCIMarker accepted id %q", id)
		}
	}
}

func TestReadCIMarkerRejectsBadID(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	audit := filepath.Join(root, "audit")
	if err := os.MkdirAll(audit, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "evil.ci.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "short", "../outside/evil", strings.Repeat("A", 32), strings.Repeat("z", 31) + "!"} {
		if _, err := readCIMarker(audit, id); err == nil {
			t.Errorf("readCIMarker accepted id %q", id)
		}
	}
}

// A symlinked marker must be refused, not followed: it would otherwise feed the
// watcher substituted PR coordinates.
func TestReadCIMarkerRefusesSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, []byte(`{"task_id":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, ciMarkerPath(root, testCIID)); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := readCIMarker(root, testCIID); err == nil {
		t.Error("readCIMarker followed a symlink")
	}
}

func TestRemoveCIMarkerIdempotent(t *testing.T) {
	root := t.TempDir()
	m := sampleCIMarker()
	if err := writeCIMarker(root, m); err != nil {
		t.Fatal(err)
	}
	if err := removeCIMarker(root, m.TaskID); err != nil {
		t.Fatalf("first remove: %v", err)
	}
	if _, err := os.Stat(ciMarkerPath(root, m.TaskID)); !os.IsNotExist(err) {
		t.Fatalf("marker still present after remove: %v", err)
	}
	if err := removeCIMarker(root, m.TaskID); err != nil {
		t.Fatalf("second remove not idempotent: %v", err)
	}
	tmp := ciMarkerPath(root, m.TaskID) + ".tmp"
	if err := os.WriteFile(tmp, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeCIMarker(root, m.TaskID); err != nil {
		t.Fatalf("remove with stray tmp: %v", err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("stray .tmp survived remove: %v", err)
	}
	if err := removeCIMarker(root, "../evil"); err == nil {
		t.Error("removeCIMarker accepted a traversal id")
	}
}

func TestListCIMarkersSortsAndTolerates(t *testing.T) {
	root := t.TempDir()
	ids := []string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"cccccccccccccccccccccccccccccccc",
	}
	for i, created := range []int64{300, 100, 200} {
		m := sampleCIMarker()
		m.TaskID = ids[i]
		m.CreatedAtMs = created
		if err := writeCIMarker(root, m); err != nil {
			t.Fatal(err)
		}
	}
	// Garbage must be skipped, not fail the boot scan.
	if err := os.WriteFile(filepath.Join(root, "dddddddddddddddddddddddddddddddd.ci.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Other per-task artifacts in the same audit dir are ignored.
	if err := os.WriteFile(filepath.Join(root, ids[0]+".queue.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ids[0]+".brief.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(root, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee.ci.json")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	got, err := ListCIMarkers(root)
	if err != nil {
		t.Fatalf("ListCIMarkers: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (garbage and symlink skipped): %+v", len(got), got)
	}
	wantOrder := []string{ids[1], ids[2], ids[0]} // created 100, 200, 300
	for i, w := range wantOrder {
		if got[i].TaskID != w {
			t.Errorf("got[%d].TaskID = %s, want %s (CreatedAtMs order)", i, got[i].TaskID, w)
		}
	}
}

// A missing audit dir is an empty scan, not an error — brokerd boots before
// anything has ever been written.
func TestListCIMarkersMissingDir(t *testing.T) {
	got, err := ListCIMarkers(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(got) != 0 {
		t.Errorf("ListCIMarkers(missing) = %v, %v; want nil, nil", got, err)
	}
}
