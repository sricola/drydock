package broker

import (
	"strings"
	"testing"

	"drydock/internal/egress"
)

// fakeSquid records AddTask/RemoveTask calls for lifecycle assertions.
type fakeSquid struct {
	added   []string
	removed []string
	domains map[string][]string
}

func (f *fakeSquid) AddTask(user, secret string, domains []string) error {
	f.added = append(f.added, user)
	if f.domains == nil {
		f.domains = map[string][]string{}
	}
	f.domains[user] = domains
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
	if got := fs.domains["task-abc123"]; len(got) != 1 || got[0] != "api.github.com" {
		t.Errorf("registered domains = %v", got)
	}
	// proxyAuth must be "user:secret@" with a non-empty secret.
	if !strings.HasPrefix(proxyAuth, "task-abc123:") || !strings.HasSuffix(proxyAuth, "@") || len(proxyAuth) <= len("task-abc123:@") {
		t.Errorf("proxyAuth = %q, want task-abc123:<secret>@", proxyAuth)
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
