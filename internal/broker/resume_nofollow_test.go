package broker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The audit trail a resumed gate appends to must be opened like every other
// audit artifact: never through a symlink. A link planted at <id>.jsonl would
// otherwise turn the resume into a write to an arbitrary file.
func TestResumeAwaiting_RefusesASymlinkedAuditTrail(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("untouched\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, lnkID+".jsonl")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := os.WriteFile(filepath.Join(dir, lnkID+".diff"), []byte("diff --git a/x b/x\n+y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeGateMarker(dir, lnkID, gateMarker{RepoRef: "https://github.com/o/r.git", Agent: "claude"})
	fs := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	b := &Broker{AuditRoot: dir, reopenStage: func(string) (taskStage, error) { return fs, nil }}

	b.ResumeAwaiting(t.TempDir())
	// Let anything the call started finish (a resumed gate would be cancelled
	// here and write its terminal), so the assertions below are not racing it.
	b.BeginQueueDrain()
	b.CancelAll()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b.WaitQueueDrain(ctx)

	if got, _ := os.ReadFile(victim); string(got) != "untouched\n" {
		t.Fatalf("resume wrote through the symlink: %q", got)
	}
	b.pendingMu.Lock()
	_, pending := b.pending[lnkID]
	b.pendingMu.Unlock()
	if pending {
		t.Error("a task whose audit trail is a symlink was resumed as pending")
	}
}
