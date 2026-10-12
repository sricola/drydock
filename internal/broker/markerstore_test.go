package broker

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type probeMarker struct {
	Name string `json:"name"`
}

var probeStore = markerStore[probeMarker]{suffix: ".probe.json", what: "probe"}

// Every marker type gets the same guarantees from one implementation. These
// pin the contract once; the per-type files only add ordering and body checks.
func TestMarkerStore_RejectsIDsThatAreNotTaskIDs(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{"", "abc", "../escape", strings.Repeat("A", 32), strings.Repeat("1", 31)} {
		if err := probeStore.write(root, bad, probeMarker{Name: "x"}); err == nil {
			t.Errorf("write accepted id %q", bad)
		}
		if _, err := probeStore.read(root, bad); err == nil {
			t.Errorf("read accepted id %q", bad)
		}
		if err := probeStore.remove(root, bad); err == nil {
			t.Errorf("remove accepted id %q", bad)
		}
	}
	if entries, _ := os.ReadDir(filepath.Dir(root)); len(entries) != 1 {
		t.Errorf("a rejected id created something outside root: %v", entries)
	}
}

func TestMarkerStore_RoundTripIsAtomicAnd0600(t *testing.T) {
	root := t.TempDir()
	id := queueTestID('1')
	if err := probeStore.write(root, id, probeMarker{Name: "one"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(root, id+".probe.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", fi.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(root, "*.tmp")); len(left) != 0 {
		t.Errorf("temp left behind: %v", left)
	}
	got, err := probeStore.read(root, id)
	if err != nil || got.Name != "one" {
		t.Fatalf("read = %+v, %v", got, err)
	}
}

func TestMarkerStore_ReadRefusesSymlinkAndListSkipsItWithAWarning(t *testing.T) {
	root := t.TempDir()
	good, lnk, bad := queueTestID('1'), queueTestID('2'), queueTestID('3')
	if err := probeStore.write(root, good, probeMarker{Name: "good"}); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "elsewhere.json")
	os.WriteFile(real, []byte(`{"name":"evil"}`), 0o600)
	if err := os.Symlink(real, filepath.Join(root, lnk+".probe.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	os.WriteFile(filepath.Join(root, bad+".probe.json"), []byte("{not json"), 0o600)
	os.WriteFile(filepath.Join(root, good+".other.json"), []byte("{}"), 0o600) // wrong suffix: ignored
	os.Mkdir(filepath.Join(root, queueTestID('4')+".probe.json"), 0o700)       // a directory: ignored

	if _, err := probeStore.read(root, lnk); err == nil {
		t.Error("read followed a symlinked marker")
	}

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prev)

	all, err := probeStore.list(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[good].Name != "good" {
		t.Errorf("list = %v, want only %s", all, good)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, lnk+".probe.json") || !strings.Contains(logs, bad+".probe.json") {
		t.Errorf("skipped markers must be named in the log; got:\n%s", logs)
	}
}

func TestMarkerStore_RemoveIsIdempotentAndSweepsTemps(t *testing.T) {
	root := t.TempDir()
	id := queueTestID('1')
	if err := probeStore.write(root, id, probeMarker{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, id+".probe.json")
	os.WriteFile(p+".tmp", []byte("legacy"), 0o600)
	os.WriteFile(p+".4242.tmp", []byte("crashed"), 0o600)
	for i := 0; i < 2; i++ {
		if err := probeStore.remove(root, id); err != nil {
			t.Fatalf("remove #%d: %v", i+1, err)
		}
	}
	if left, _ := filepath.Glob(p + "*"); len(left) != 0 {
		t.Errorf("remove left %v", left)
	}
}

func TestMarkerStore_ListOfMissingRootIsEmpty(t *testing.T) {
	all, err := probeStore.list(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(all) != 0 {
		t.Errorf("list(missing) = %v, %v; want empty, nil", all, err)
	}
}
