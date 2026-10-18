package broker

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"drydock/internal/egress"
)

// <id>.widen.json is a reviewer artifact in the audit dir. It must be written
// like every other audit artifact: atomically and without following a planted
// symlink, so a link left at that name can neither redirect the write nor leave
// a torn file for the reviewer.
func TestGateEgressWiden_MarkerWriteNeverFollowsASymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	const id = "te-widen"
	if err := os.Symlink(victim, filepath.Join(dir, id+".widen.json")); err != nil {
		t.Skip("symlinks unavailable")
	}
	b := &Broker{AuditRoot: dir}
	extras := []egress.Domain{{Host: "internal.example.com", Ports: []int{443}}}
	done := make(chan bool, 1)
	go func() { done <- b.gateEgressWiden(context.Background(), id, extras) }()
	if !waitFor(5*time.Second, func() bool {
		b.pendingMu.Lock()
		_, ok := b.pending[id]
		b.pendingMu.Unlock()
		return ok
	}) {
		t.Fatal("egress gate never registered")
	}
	req := httptest.NewRequest("POST", "/admin/deny/"+id, nil)
	req.SetPathValue("id", id)
	b.HandleDeny(httptest.NewRecorder(), req)
	<-done

	if got, _ := os.ReadFile(victim); string(got) != "untouched" {
		t.Fatalf("the symlink target was overwritten: %q", got)
	}
	fi, err := os.Lstat(filepath.Join(dir, id+".widen.json"))
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("marker is %v, want a regular file replacing the symlink", fi.Mode())
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("marker mode %o, want 0600", fi.Mode().Perm())
	}
}
