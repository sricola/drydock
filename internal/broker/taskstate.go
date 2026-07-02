package broker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"time"

	"drydock/internal/egress"
)

// TaskStage tracks where a task currently is in its lifecycle. Only the
// non-terminal stages live in Broker.tasks — completed tasks fall out as
// HandleTask returns.
type TaskStage string

const (
	StageAwaitingEgress TaskStage = "awaiting_egress"
	StageRunning        TaskStage = "running"
	StagePending        TaskStage = "awaiting_approval"
	StagePushing        TaskStage = "pushing"
)

// TaskState is the operator-facing snapshot returned by GET /admin/tasks.
// EgressExtra is populated only when the task is at the egress gate so
// the operator can see what's being asked before approving.
type TaskState struct {
	ID          string          `json:"id"`
	Repo        string          `json:"repo"`
	Instruction string          `json:"instruction"` // truncated for display
	Stage       TaskStage       `json:"stage"`
	StartedAt   time.Time       `json:"started_at"`
	EgressExtra []egress.Domain `json:"egress_extra,omitempty"`
}

const instructionSnippetMax = 140

func proxyUser(taskID string) string { return "task-" + taskID }

// mintProxySecret returns a random hex secret for a task's proxy credential.
func mintProxySecret() (string, error) {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// setupWidening registers a per-task squid credential + ACL for the task's
// extra hosts and returns the "<user>:<secret>@" userinfo to splice into the
// VM's proxy URL, plus a cleanup that deregisters it. For a non-widened task
// (no extras) or when squid widening is disabled (b.Squid == nil) it is a
// no-op: empty proxyAuth and a no-op cleanup (always safe to defer). Fail-closed:
// a registration error is returned and the caller must abort before the run.
func (b *Broker) setupWidening(taskID string, extras []egress.Domain) (proxyAuth string, cleanup func(), err error) {
	cleanup = func() {}
	if len(extras) == 0 || b.Squid == nil {
		return "", cleanup, nil
	}
	secret, err := mintProxySecret()
	if err != nil {
		return "", cleanup, err
	}
	user := proxyUser(taskID)
	if err := b.Squid.AddTask(user, secret, extras); err != nil {
		return "", cleanup, err
	}
	cleanup = func() {
		if err := b.Squid.RemoveTask(user); err != nil {
			slog.Warn("egress widening cleanup failed", "user", user, "err", err)
		}
	}
	return user + ":" + secret + "@", cleanup, nil
}

// newID returns a hex token with 128 bits of entropy. /admin/approve is
// directly addressable by ID; with 48 bits a local attacker can race
// approvals if they can enumerate task IDs (e.g., readdir on an audit
// dir mode 0755 — fixed elsewhere). 128 bits removes online guessing
// from the attack tree entirely.
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// No entropy means we can't mint an unguessable task ID — and the
		// approval-race threat model leans on that. Fail closed, don't ship zeros.
		panic("drydock: crypto/rand failed — cannot mint task IDs: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// initSlots lazily builds the concurrency semaphore. Capacity comes from
// MaxConcurrent (or 2 if unset). Called from HandleTask via sync.Once so
// existing tests/callers that build a Broker by literal don't have to
// remember to do this.
func (b *Broker) initSlots() {
	n := b.MaxConcurrent
	if n <= 0 {
		n = 2
	}
	b.slots = make(chan struct{}, n)
}

// acquireSlot is a non-blocking semaphore-take. Returns false when the cap
// is hit — the handler returns 503 to the caller.
func (b *Broker) acquireSlot() bool {
	b.slotsOnce.Do(b.initSlots)
	select {
	case b.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (b *Broker) releaseSlot() {
	select {
	case <-b.slots:
	default:
	}
}

// registerTask records a task in the live-tasks map under StageRunning,
// and stashes its cancel hook so POST /admin/kill/{id} can abort it.
func (b *Broker) registerTask(id, repo, instruction string, cancel context.CancelFunc) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	if b.tasks == nil {
		b.tasks = make(map[string]*TaskState)
	}
	if b.cancellers == nil {
		b.cancellers = make(map[string]context.CancelFunc)
	}
	if r := []rune(instruction); len(r) > instructionSnippetMax {
		instruction = string(r[:instructionSnippetMax]) + "…"
	}
	b.tasks[id] = &TaskState{
		ID:          id,
		Repo:        repo,
		Instruction: instruction,
		Stage:       StageRunning,
		StartedAt:   time.Now(),
	}
	if cancel != nil {
		b.cancellers[id] = cancel
	}
}

func (b *Broker) setStage(id string, s TaskStage) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	if t, ok := b.tasks[id]; ok {
		t.Stage = s
	}
}

// setEgressExtra populates the requested-widening hosts on the task state so
// the operator can see exactly what's being asked at the egress gate. Cleared
// when the gate resolves.
func (b *Broker) setEgressExtra(id string, extras []egress.Domain) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	if t, ok := b.tasks[id]; ok {
		t.EgressExtra = extras
	}
}

func (b *Broker) unregisterTask(id string) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	delete(b.tasks, id)
	delete(b.cancellers, id)
}

// CancelAll cancels every in-flight task. Each task's own HandleTask then tears
// down its VM (force-delete) and returns a cancelled response — so a graceful
// brokerd shutdown doesn't orphan running VMs or drop clients at the gate. The
// cancels are collected under the lock and invoked outside it (the cancel paths
// reacquire pendingMu via the gates/unregister).
func (b *Broker) CancelAll() {
	b.pendingMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(b.cancellers))
	for _, c := range b.cancellers {
		cancels = append(cancels, c)
	}
	b.pendingMu.Unlock()
	for _, c := range cancels {
		c()
	}
}
