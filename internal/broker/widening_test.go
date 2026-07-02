package broker

import (
	"errors"
	"testing"

	"drydock/internal/egress"
)

// fakeSquid records AddTask/RemoveTask calls for lifecycle assertions.
type fakeSquid struct {
	added   []string
	removed []string
	domains map[string][]egress.Domain
	secrets map[string]string // user -> secret handed to AddTask
	addErr  error             // when set, AddTask fails (registration error)
}

func (f *fakeSquid) AddTask(user, secret string, domains []egress.Domain) error {
	if f.addErr != nil {
		return f.addErr
	}
	f.added = append(f.added, user)
	if f.domains == nil {
		f.domains = map[string][]egress.Domain{}
		f.secrets = map[string]string{}
	}
	f.domains[user] = domains
	f.secrets[user] = secret
	return nil
}
func (f *fakeSquid) RemoveTask(user string) error {
	f.removed = append(f.removed, user)
	return nil
}

func TestSetupWidening_RegistersAndReturnsAuth(t *testing.T) {
	fs := &fakeSquid{}
	b := &Broker{Squid: fs}
	extras := []egress.Domain{{Host: "api.github.com", Ports: []int{443}}}

	proxyAuth, cleanup, err := b.setupWidening("abc123", extras)
	if err != nil {
		t.Fatalf("setupWidening: %v", err)
	}
	if len(fs.added) != 1 || fs.added[0] != "task-abc123" {
		t.Fatalf("AddTask users = %v, want [task-abc123]", fs.added)
	}
	if got := fs.domains["task-abc123"]; len(got) != 1 || got[0].Host != "api.github.com" ||
		len(got[0].Ports) != 1 || got[0].Ports[0] != 443 {
		t.Errorf("registered domains = %v", got)
	}
	// proxyAuth must be "user:secret@" with a non-empty secret, and the secret
	// must be exactly the one handed to AddTask (no scramble/reuse).
	wantAuth := "task-abc123:" + fs.secrets["task-abc123"] + "@"
	if fs.secrets["task-abc123"] == "" {
		t.Errorf("AddTask received an empty secret")
	}
	if proxyAuth != wantAuth {
		t.Errorf("proxyAuth = %q, want %q (secret must match the one given to AddTask)", proxyAuth, wantAuth)
	}
	// cleanup deregisters.
	cleanup()
	if len(fs.removed) != 1 || fs.removed[0] != "task-abc123" {
		t.Errorf("cleanup removed = %v, want [task-abc123]", fs.removed)
	}
}

func TestSetupWidening_NoExtrasIsNoOp(t *testing.T) {
	fs := &fakeSquid{}
	b := &Broker{Squid: fs}
	proxyAuth, cleanup, err := b.setupWidening("abc123", nil)
	if err != nil {
		t.Fatal(err)
	}
	if proxyAuth != "" {
		t.Errorf("proxyAuth = %q, want empty for non-widened task", proxyAuth)
	}
	cleanup() // must be safe to call
	if len(fs.added) != 0 || len(fs.removed) != 0 {
		t.Errorf("non-widened touched squid: added=%v removed=%v", fs.added, fs.removed)
	}
}

func TestSetupWidening_AddTaskErrorFailsClosed(t *testing.T) {
	fs := &fakeSquid{addErr: errors.New("reconfigure boom")}
	b := &Broker{Squid: fs}
	extras := []egress.Domain{{Host: "api.github.com", Ports: []int{443}}}

	proxyAuth, cleanup, err := b.setupWidening("abc123", extras)
	if err == nil {
		t.Fatal("setupWidening must return the AddTask error (fail-closed)")
	}
	if proxyAuth != "" {
		t.Errorf("proxyAuth = %q, want empty on registration failure", proxyAuth)
	}
	if cleanup == nil {
		t.Fatal("cleanup must be non-nil even on error (caller defers it)")
	}
	cleanup() // must be safe; nothing was registered, so nothing to remove
	if len(fs.removed) != 0 {
		t.Errorf("cleanup deregistered %v after a failed AddTask", fs.removed)
	}
}

func TestSetupWidening_NilSquidIsNoOp(t *testing.T) {
	b := &Broker{} // Squid nil
	proxyAuth, cleanup, err := b.setupWidening("abc123", []egress.Domain{{Host: "x.com", Ports: []int{443}}})
	if err != nil {
		t.Fatal(err)
	}
	if proxyAuth != "" {
		t.Errorf("proxyAuth = %q, want empty when Squid is nil", proxyAuth)
	}
	cleanup() // must not panic
}
