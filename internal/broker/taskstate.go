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
	StageSettingUp      TaskStage = "setting_up"
	StageRunning        TaskStage = "running"
	StageVerifying      TaskStage = "verifying"
	StagePending        TaskStage = "awaiting_approval"
	StagePushing        TaskStage = "pushing"
)

// TaskState is the operator-facing snapshot returned by GET /admin/tasks.
// EgressExtra is populated only when the task is at the egress gate so
// the operator can see what's being asked before approving.
type TaskState struct {
	ID          string    `json:"id"`
	Repo        string    `json:"repo"`
	Instruction string    `json:"instruction"` // truncated for display
	Stage       TaskStage `json:"stage"`
	// Agent is the RESOLVED sandbox agent (claude, codex, gemini, opencode),
	// stamped by the runner once credentials resolve (setAgent). Empty until
	// then, and for tasks resumed straight into a gate; omitempty keeps those
	// serialisations byte-identical to before the field existed. The web UI
	// used to hardcode a "claude" label here regardless of the actual lane.
	Agent       string          `json:"agent,omitempty"`
	StartedAt   time.Time       `json:"started_at"`
	EgressExtra []egress.Domain `json:"egress_extra,omitempty"`
	// SecondLook lists the diff-policy second-look categories the approver
	// must acknowledge, populated while the task waits at the approval gate.
	SecondLook []string `json:"second_look,omitempty"`
	// SpentUSD / SpentSrc are the task's BROKER-METERED spend, published when
	// it enters the diff-approval gate (setSpend). They exist for one reason:
	// the web UI used to render the spend at the push gate by scraping the live
	// audit jsonl for ANY total_cost_usd — including the agent's own, which is
	// untrusted text from inside the VM. That put a number a compromised agent
	// controls directly beside a human security decision (plan G4).
	//
	// SpentSrc says where the figure came from, and the UI renders it rather
	// than a bare number:
	//
	//	"broker"    — the gateway lease's own metering. Trustworthy.
	//	"unmetered" — a subscription lane, or an openai_compat lane with no
	//	              prices: there is no USD to meter, so SpentUSD is 0 by
	//	              construction rather than by measurement.
	//	""          — not known (the gate was reached without a lease, e.g. a
	//	              task resumed after a restart whose previous process left
	//	              no broker result row).
	//
	// Both omitempty, so a task that never reaches the gate serialises exactly
	// as it did before these fields existed.
	SpentUSD float64 `json:"spent_usd,omitempty"`
	SpentSrc string  `json:"spent_src,omitempty"`
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
// and stashes its cancel hook so POST /admin/kill/{id} can abort it. Callers
// that are not starting a task fresh (a resumed gate task, which by
// construction is already sitting at the diff-approval gate) should use
// registerTaskAt instead, so the stage lands correctly in the same critical
// section instead of a later, separately-locked correction.
func (b *Broker) registerTask(id, repo, instruction string, cancel context.CancelCauseFunc) {
	b.registerTaskAt(id, repo, instruction, cancel, StageRunning)
}

// registerTaskAt is registerTask with an explicit initial stage. It exists so
// resumePush can register a resumed task as StagePending atomically: the
// HTTP admin listener is already serving during boot reconciliation, so a
// register-as-running-then-correct-to-pending sequence (two lock
// acquisitions) would let a reader observe the wrong stage in between.
func (b *Broker) registerTaskAt(id, repo, instruction string, cancel context.CancelCauseFunc, stage TaskStage) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	if b.tasks == nil {
		b.tasks = make(map[string]*TaskState)
	}
	if b.cancellers == nil {
		b.cancellers = make(map[string]context.CancelCauseFunc)
	}
	if r := []rune(instruction); len(r) > instructionSnippetMax {
		instruction = string(r[:instructionSnippetMax]) + "…"
	}
	b.tasks[id] = &TaskState{
		ID:          id,
		Repo:        repo,
		Instruction: instruction,
		Stage:       stage,
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

// setAgent records the resolved agent on the live task row so operator
// surfaces (drydock tasks, the web UI board) label the run with the lane
// that is actually executing instead of guessing.
func (b *Broker) setAgent(id, agent string) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	if t, ok := b.tasks[id]; ok {
		t.Agent = agent
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

// setSecondLook populates the second-look acknowledgment categories on the
// task state so the operator sees what an approve must ack while the task
// waits at the diff-approval gate. Cleared (nil) when the gate resolves.
// Display only — enforcement lives in b.requiredAcks and signal (admin.go).
func (b *Broker) setSecondLook(id string, acks []string) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	if t, ok := b.tasks[id]; ok {
		t.SecondLook = acks
	}
}

// setSpend publishes a task's BROKER-METERED spend on its live state so the
// approval surfaces can show it without reading the agent's stdout. See
// TaskState.SpentUSD for why that distinction is load-bearing.
func (b *Broker) setSpend(id string, usd float64, src string) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	if t, ok := b.tasks[id]; ok {
		t.SpentUSD, t.SpentSrc = usd, src
	}
}

// publishGateSpend computes and publishes this task's spend for the diff gate.
// It reads the same broker-observed figure the ledger records — never
// audit.TotalCost, never the agent's result line — and marks a lane that
// carries no USD metering at all as "unmetered" rather than reporting its $0 as
// a measurement.
func (tr *taskRun) publishGateSpend() {
	b := tr.b
	if b == nil {
		return
	}
	// The same signal writeBrief and the ledger key on, so the gate, the brief
	// and the ceiling can never disagree about whether a lane meters.
	if tr.taskVendor == "" || b.UnmeteredVendors[tr.taskVendor] {
		b.setSpend(tr.id, 0, "unmetered")
		return
	}
	usd, trusted := tr.brokerMeteredSpendUSD()
	if !trusted || !usableUSD(usd) || usd < 0 {
		b.setSpend(tr.id, 0, "")
		return
	}
	b.setSpend(tr.id, usd, "broker")
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
	cancels := make([]context.CancelCauseFunc, 0, len(b.cancellers))
	for _, c := range b.cancellers {
		cancels = append(cancels, c)
	}
	b.pendingMu.Unlock()
	for _, c := range cancels {
		c(errShutdown)
	}
}
