// Package broker wires staging, egress compilation, credential minting, the
// container run, diff capture, the approval gate, and the host-side push.
package broker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"drydock/internal/agent"
	"drydock/internal/creds"
	"drydock/internal/egress"
	"drydock/internal/remote"
	"drydock/internal/runner"
	"drydock/internal/stage"
)

// gitURLRef accepts any https://, git@, or ssh:// git URL. Local paths
// (no scheme, no `:` host) are still rejected because the staging clone
// would inherit a filesystem origin and adapters can't operate on it.
// The adapter (GitHub / GitLab / push-only) is selected separately by
// Task.Platform or hostname autodetect.
var gitURLRef = regexp.MustCompile(
	`^(?:https?://[A-Za-z0-9.-]+/|git@[A-Za-z0-9.-]+:|ssh://[A-Za-z0-9._-]+@[A-Za-z0-9.-]+/)[A-Za-z0-9._-]+/[A-Za-z0-9._-]+?(?:\.git)?/?$`,
)

type Task struct {
	RepoRef     string          `json:"repo_ref"`
	Instruction string          `json:"instruction"`
	EgressExtra []egress.Domain `json:"egress_extra"`
	Sensitive   bool            `json:"sensitive"`
	// AutoApprove skips the diff-push gate. Off by default — the central
	// security claim depends on a human (or trusted process) signing off on
	// the diff. Callers who really want a headless run must say so explicitly.
	AutoApprove bool `json:"auto_approve"`
	// Platform selects the remote adapter ("github" | "gitlab" | "none" |
	// ""). Empty falls back to hostname autodetect from RepoRef. Self-hosted
	// GitLab needs platform="gitlab" since the hostname won't say so.
	Platform string `json:"platform"`
	// Model passes through to `claude --model <Model>` in the sandbox. Empty
	// falls back to Broker.DefaultModel (operator config), then to claude's
	// own default. Value is unvalidated here — claude-code rejects unknown
	// IDs at start, fail-closed.
	Model string `json:"model"`
	// Agent selects the sandbox CLI: "claude" (default) or "codex". Empty
	// falls back to Broker.DefaultAgent, then "claude". Unknown agents are
	// rejected before any VM starts (fail-closed).
	Agent string `json:"agent"`
}

type Broker struct {
	Cfg          egress.Config
	Providers    map[string]creds.Provider // vendor -> provider
	DefaultAgent string                    // "" -> "claude"
	ImageRef     string
	StageRoot    string
	AuditRoot    string
	Timeout      time.Duration
	Network      string  // stable egress network name (e.g. drydock-egress)
	GatewayIP    string  // vmnet gateway IP the VM reaches (e.g. 192.168.64.1)
	ProxyPort    int     // squid port (e.g. 3128)
	TaskBudget   float64 // USD budget per task
	DefaultModel string  // operator-level default; per-task Task.Model overrides

	// Test seams. nil in production -> the real implementations
	// (defaultPrepareStage / runContainer). White-box tests inject fakes to
	// drive HandleTask without a git clone or a container run.
	prepareStage func(root, repoRef string) (taskStage, error)
	runAgent     func(ctx context.Context, args []string, stdout, stderr io.Writer) error

	// MaxConcurrent caps how many tasks may be in any non-terminal state at
	// once. Excess POSTs to /tasks return 503. Default (when zero) is 2.
	MaxConcurrent int

	// slots is a bounded semaphore guarding MaxConcurrent. Initialized lazily
	// the first time HandleTask is called (so existing callers that build a
	// Broker by struct literal keep working).
	slotsOnce sync.Once
	slots     chan struct{}

	pendingMu  sync.Mutex
	pending    map[string]chan bool          // task_id -> approval channel
	tasks      map[string]*TaskState         // task_id -> live state (running + awaiting_approval)
	cancellers map[string]context.CancelFunc // task_id -> cancel hook for in-flight kill
}

// taskStage is the subset of *stage.Stage that HandleTask uses. It exists so
// white-box tests can drive the handler without a real git clone; production
// uses realStage, a thin adapter over *stage.Stage.
type taskStage interface {
	WorkDir() string
	WriteTaskFiles(prompt, allowlist string) error
	CaptureDiff() (string, error)
	Push(adapter remote.Adapter, branch, msg string) error
	Cleanup() error
}

type realStage struct{ s *stage.Stage }

func (r realStage) WorkDir() string { return r.s.WorkDir }
func (r realStage) WriteTaskFiles(prompt, allowlist string) error {
	return r.s.WriteTaskFiles(prompt, allowlist)
}
func (r realStage) CaptureDiff() (string, error) { return r.s.CaptureDiff() }
func (r realStage) Push(adapter remote.Adapter, branch, msg string) error {
	return r.s.Push(adapter, branch, msg)
}
func (r realStage) Cleanup() error { return r.s.Cleanup() }

// defaultPrepareStage is the production prepareStage: a real host clone with
// the .git dir moved out of the mounted work tree.
func defaultPrepareStage(root, repoRef string) (taskStage, error) {
	s, err := stage.Prepare(root, repoRef)
	if err != nil {
		return nil, err
	}
	return realStage{s}, nil
}

// runContainer is the production runAgent: it runs the Apple `container` CLI
// for the task and streams its output to the audit log.
func runContainer(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "container", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

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
	cap := b.MaxConcurrent
	if cap <= 0 {
		cap = 2
	}
	b.slots = make(chan struct{}, cap)
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
	if len(instruction) > instructionSnippetMax {
		instruction = instruction[:instructionSnippetMax] + "…"
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

// MaxTaskBodyBytes caps the size of POST /tasks bodies. Generous enough for
// long instructions but small enough that local-DoS via 1GB instruction
// strings (or TCP-listener attacks when BROKER_ADDR is set) can't burn
// memory unbounded.
const MaxTaskBodyBytes = 64 << 10

func (b *Broker) HandleTask(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxTaskBodyBytes)
	var t Task
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !gitURLRef.MatchString(t.RepoRef) {
		http.Error(w, "repo_ref must be an https/git/ssh URL (no local paths)", http.StatusBadRequest)
		return
	}
	if !b.acquireSlot() {
		http.Error(w,
			"too many concurrent tasks; raise DRYDOCK_MAX_CONCURRENT_TASKS or wait",
			http.StatusServiceUnavailable)
		return
	}
	defer b.releaseSlot()

	taskID := newID()

	// One context per task. Cancelling it propagates to the container run
	// (via exec.CommandContext below) AND to gatePush's select. /admin/kill
	// invokes the stored cancel; client disconnect also propagates here.
	taskCtx, cancel := context.WithCancel(r.Context())
	defer cancel()
	b.registerTask(taskID, t.RepoRef, t.Instruction, cancel)
	defer b.unregisterTask(taskID)

	// Validate widening request before anyone can approve it. Without this
	// a wildcard or otherwise-malformed host could compile into squid's
	// dstdomain file and silently widen the allowlist past what the
	// reviewer thought they were approving.
	if len(t.EgressExtra) > 0 {
		if err := egress.ValidateDomains(t.EgressExtra); err != nil {
			http.Error(w, "egress_extra invalid: "+safeErr(err), http.StatusBadRequest)
			return
		}
	}
	// Egress widening: block at the same kind of human-driven gate as the
	// diff push. Without this the requires_approval flag is a lie —
	// auto-approve would let any task ask for any host.
	if len(t.EgressExtra) > 0 && b.Cfg.PerTaskWidening.RequiresApproval {
		b.setStage(taskID, StageAwaitingEgress)
		b.setEgressExtra(taskID, t.EgressExtra)
		ok := b.gateEgressWiden(taskCtx, taskID, t.EgressExtra)
		b.setEgressExtra(taskID, nil)
		if !ok {
			if taskCtx.Err() != nil {
				writeJSON(w, map[string]any{"task_id": taskID, "cancelled": true, "pushed": false})
				return
			}
			http.Error(w, "egress widening denied", http.StatusForbidden)
			return
		}
		b.setStage(taskID, StageRunning)
	}

	stageDir := filepath.Join(b.StageRoot, taskID)

	prepare := b.prepareStage
	if prepare == nil {
		prepare = defaultPrepareStage
	}
	st, err := prepare(stageDir, t.RepoRef)
	if err != nil {
		http.Error(w, "clone failed", http.StatusBadGateway)
		return
	}
	defer st.Cleanup() // wipe the host scratch (work tree + host-only git dir)

	allowlist := egress.CompileAllowlist(b.Cfg, t.EgressExtra)
	if err := st.WriteTaskFiles(t.Instruction, allowlist); err != nil {
		http.Error(w, "stage failed", http.StatusInternalServerError)
		return
	}

	agentName, prov, status, msg := b.resolveAgent(t.Agent)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	grant, err := prov.Mint(b.TaskBudget)
	if err != nil {
		http.Error(w, "credential mint failed", http.StatusInternalServerError)
		return
	}
	defer grant.Revoke()

	// 0o700 keeps another local process from enumerating task IDs and
	// racing /admin/approve before the operator. The audit dir contains
	// the diff, the prompt, and the full stream-json trace — none of it
	// should be world-readable.
	if err := os.MkdirAll(b.AuditRoot, 0o700); err != nil {
		http.Error(w, "audit dir failed", http.StatusInternalServerError)
		return
	}
	// 0o600 on the audit log: same reasoning. os.Create would create at
	// 0666 (umask-reduced); be explicit.
	logf, err := os.OpenFile(
		filepath.Join(b.AuditRoot, taskID+".jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		http.Error(w, "audit file failed", http.StatusInternalServerError)
		return
	}
	defer logf.Close()

	env := append([]string{}, grant.EnvVars()...)
	env = append(env,
		fmt.Sprintf("HTTPS_PROXY=http://%s:%d", b.GatewayIP, b.ProxyPort),
		fmt.Sprintf("HTTP_PROXY=http://%s:%d", b.GatewayIP, b.ProxyPort),
		// Bypass squid for the credential gateway itself — squid's allowlist
		// is hostname-based and would deny a CONNECT to the gateway IP.
		"NO_PROXY=127.0.0.1,localhost,"+b.GatewayIP,
		"DRYDOCK_GW_IP="+b.GatewayIP,
	)
	env = append(env, modelEnv(t.Model, b.DefaultModel)...)
	env = append(env, "DRYDOCK_AGENT="+agentName)

	args := runner.BuildRunArgs(runner.Spec{
		TaskID:     taskID,
		Network:    b.Network,
		ImageRef:   b.ImageRef,
		Env:        env,
		StageDir:   st.WorkDir(),
		PromptFile: "/work/.task/prompt.txt",
		MemoryGB:   4,
		CPUs:       4,
	})

	runCtx, runCancel := context.WithTimeout(taskCtx, b.Timeout)
	defer runCancel()
	run := b.runAgent
	if run == nil {
		run = runContainer
	}
	taskStart := time.Now()
	if err := run(runCtx, args, io.MultiWriter(logf, os.Stdout), logf); err != nil {
		// --rm covers a graceful exit; on timeout/kill the VM may survive,
		// so force-remove it (best effort) to honor the ephemeral-VM backstop.
		if derr := exec.Command("container", "delete", "--force", "task-"+taskID).Run(); derr != nil {
			slog.Warn("force-delete of task VM failed; reaped at next brokerd boot",
				"task_id", taskID, "err", derr)
		}
		if taskCtx.Err() != nil {
			// Operator killed it, or the client went away. Be explicit.
			writeJSON(w, map[string]any{"task_id": taskID, "cancelled": true, "pushed": false})
			return
		}
		// If claude never wrote a `result` event (e.g. the entrypoint died
		// before claude was even exec'd), `drydock tasks` would show this
		// task as `running?` forever. Append a synthetic terminal event so
		// the audit log is self-describing.
		_, _ = fmt.Fprintf(logf,
			`{"type":"result","subtype":"error","is_error":true,"duration_ms":%d,"total_cost_usd":0,"num_turns":0}`+"\n",
			time.Since(taskStart).Milliseconds())
		http.Error(w, "task failed: "+safeErr(err), http.StatusInternalServerError)
		return
	}

	// codex exec doesn't emit Claude's stream-json `result` trailer, so a
	// completed codex task would read as `running?` in `drydock tasks`.
	// Synthesize the terminal event from the elapsed time and the metered
	// gateway spend. (Claude writes its own result line; don't double it.)
	if agentName != "claude" {
		_, _ = fmt.Fprintf(logf,
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":%d,"total_cost_usd":%.6f,"num_turns":0}`+"\n",
			time.Since(taskStart).Milliseconds(), grant.Spent())
	}

	diff, err := st.CaptureDiff()
	if err != nil {
		http.Error(w, "diff capture failed", http.StatusInternalServerError)
		return
	}
	if diff == "" {
		writeJSON(w, map[string]any{"task_id": taskID, "diff": "", "pushed": false})
		return
	}
	b.setStage(taskID, StagePending)
	if !b.gatePush(taskCtx, taskID, diff, t.AutoApprove) {
		// Distinguish "killed" from "denied" in the response so the operator
		// (and audit consumers) know what happened.
		if taskCtx.Err() != nil {
			writeJSON(w, map[string]any{"task_id": taskID, "diff": diff, "cancelled": true, "pushed": false})
			return
		}
		writeJSON(w, map[string]any{"task_id": taskID, "diff": diff, "pushed": false})
		return
	}

	b.setStage(taskID, StagePushing)
	branch := "agent/" + taskID
	adapter := remote.AdapterFor(t.RepoRef, t.Platform)
	if err := st.Push(adapter, branch, "agent: "+firstLine(t.Instruction)); err != nil {
		http.Error(w, "push failed: "+safeErr(err), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"task_id": taskID, "branch": branch, "platform": adapter.Name(), "pushed": true})
}

// gateEgressWiden blocks until POST /admin/approve/{id} or /admin/deny/{id}
// (or the HTTP client disconnects / the task is killed). Returning false
// aborts the task before any allowlist compilation — the requested hosts
// never reach squid. Mirrors gatePush so the operator only has to learn one
// approval flow.
func (b *Broker) gateEgressWiden(ctx context.Context, taskID string, extras []egress.Domain) bool {
	ch := make(chan bool, 1)
	b.pendingMu.Lock()
	if b.pending == nil {
		b.pending = make(map[string]chan bool)
	}
	b.pending[taskID] = ch
	b.pendingMu.Unlock()
	defer func() {
		b.pendingMu.Lock()
		delete(b.pending, taskID)
		b.pendingMu.Unlock()
	}()

	// Persist the request next to the audit so reviewers have a stable
	// artifact (the in-flight TaskState would disappear on a brokerd crash).
	widenPath := filepath.Join(b.AuditRoot, taskID+".widen.json")
	if err := os.MkdirAll(b.AuditRoot, 0o700); err == nil {
		if payload, jerr := json.MarshalIndent(extras, "", "  "); jerr == nil {
			_ = os.WriteFile(widenPath, payload, 0o600)
		}
	}
	summary := summariseExtras(extras)
	slog.Info("task awaiting egress widening",
		"task_id", taskID, "extras", summary,
		"hint", "drydock approve "+taskID+" | drydock deny "+taskID)
	notifyMac("drydock — task wants more egress",
		fmt.Sprintf("task %s · %s · drydock approve %s", taskID, summary, taskID))

	select {
	case ok := <-ch:
		return ok
	case <-ctx.Done():
		slog.Info("task cancelled at egress gate", "task_id", taskID)
		return false
	}
}

func summariseExtras(extras []egress.Domain) string {
	if len(extras) == 0 {
		return "no hosts"
	}
	parts := make([]string, 0, len(extras))
	for _, d := range extras {
		ports := ""
		for i, p := range d.Ports {
			if i > 0 {
				ports += ","
			}
			ports += fmt.Sprintf("%d", p)
		}
		parts = append(parts, fmt.Sprintf("%s:%s", d.Host, ports))
	}
	return strings.Join(parts, " ")
}

// gatePush blocks until POST /admin/approve/{id} or /admin/deny/{id} (or the
// HTTP client disconnects). Returning false aborts the push and the diff is
// returned to the caller without ever touching origin. When auto is true the
// gate is bypassed — callers must opt in explicitly via Task.AutoApprove.
func (b *Broker) gatePush(ctx context.Context, taskID, diff string, auto bool) bool {
	if auto {
		slog.Info("task auto-approve push", "task_id", taskID, "reason", "caller opted in")
		return true
	}
	ch := make(chan bool, 1)
	b.pendingMu.Lock()
	if b.pending == nil {
		b.pending = make(map[string]chan bool)
	}
	b.pending[taskID] = ch
	b.pendingMu.Unlock()
	defer func() {
		b.pendingMu.Lock()
		delete(b.pending, taskID)
		b.pendingMu.Unlock()
	}()

	// Persist the diff for the human reviewing it.
	diffPath := filepath.Join(b.AuditRoot, taskID+".diff")
	_ = os.WriteFile(diffPath, []byte(diff), 0o600)
	slog.Info("task awaiting approval",
		"task_id", taskID, "diff_bytes", len(diff), "diff_path", diffPath,
		"hint", "drydock approve "+taskID+" | drydock deny "+taskID)
	notifyMac("drydock — task awaiting approval",
		fmt.Sprintf("task %s · %d byte diff · drydock approve %s", taskID, len(diff), taskID))

	select {
	case ok := <-ch:
		return ok
	case <-ctx.Done():
		slog.Info("task client disconnected before approval; aborting push", "task_id", taskID)
		return false
	}
}

// HandleApprove signals the pending task's channel with true. Wire as
// POST /admin/approve/{id}.
func (b *Broker) HandleApprove(w http.ResponseWriter, r *http.Request) { b.signal(w, r, true) }

// HandleDeny signals false. Wire as POST /admin/deny/{id}.
func (b *Broker) HandleDeny(w http.ResponseWriter, r *http.Request) { b.signal(w, r, false) }

// HandlePending returns the set of task IDs currently awaiting approval.
// Kept as IDs-only for the existing approve/deny CLI path; richer output
// lives at /admin/tasks.
func (b *Broker) HandlePending(w http.ResponseWriter, r *http.Request) {
	b.pendingMu.Lock()
	ids := make([]string, 0, len(b.pending))
	for k := range b.pending {
		ids = append(ids, k)
	}
	b.pendingMu.Unlock()
	writeJSON(w, ids)
}

// HandleTasks returns rich state for every task currently in flight
// (running, awaiting approval, or pushing). The result is sorted oldest-
// first so the CLI table is deterministic.
func (b *Broker) HandleTasks(w http.ResponseWriter, r *http.Request) {
	b.pendingMu.Lock()
	out := make([]*TaskState, 0, len(b.tasks))
	for _, t := range b.tasks {
		// Copy so the caller can't mutate the live state and we don't hold
		// the lock during JSON encoding.
		cp := *t
		out = append(out, &cp)
	}
	b.pendingMu.Unlock()
	// Stable order: oldest first.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].StartedAt.After(out[j].StartedAt); j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	writeJSON(w, out)
}

// notifyMac fires a macOS notification via osascript. Silent no-op when
// osascript isn't on PATH (i.e. running on Linux for tests/CI) or when the
// operator opts out with DRYDOCK_NO_NOTIFY=1. We swallow errors: a missing
// notification must never block the approval gate.
func notifyMac(title, body string) {
	if os.Getenv("DRYDOCK_NO_NOTIFY") == "1" {
		return
	}
	if _, err := exec.LookPath("osascript"); err != nil {
		return
	}
	// AppleScript string-escape: backslashes and double quotes both need it.
	escape := func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		return strings.ReplaceAll(s, `"`, `\"`)
	}
	script := fmt.Sprintf(`display notification "%s" with title "%s"`, escape(body), escape(title))
	_ = exec.Command("osascript", "-e", script).Run()
}

// HandleHealth is a liveness/readiness probe. Returns ok plus a coarse
// breakdown so launchd KeepAlive, `drydock status`, and `drydock init`'s
// eventual smoke probe can all use the same endpoint.
func (b *Broker) HandleHealth(w http.ResponseWriter, r *http.Request) {
	b.pendingMu.Lock()
	pending := len(b.pending)
	var awaitingEgress, running, pendingApproval, pushing int
	for _, t := range b.tasks {
		switch t.Stage {
		case StageAwaitingEgress:
			awaitingEgress++
		case StageRunning:
			running++
		case StagePending:
			pendingApproval++
		case StagePushing:
			pushing++
		}
	}
	b.pendingMu.Unlock()
	writeJSON(w, map[string]any{
		"ok":               true,
		"pending":          pending, // legacy field; matches old shape
		"awaiting_egress":  awaitingEgress,
		"running":          running,
		"pending_approval": pendingApproval,
		"pushing":          pushing,
	})
}

// HandleKill cancels the per-task context, which aborts the container run
// (if still in flight) and the gatePush wait (if at the approval gate).
// Returns 204 on success, 404 if no such live task. The corresponding
// `POST /tasks` request will return a body with "cancelled": true.
func (b *Broker) HandleKill(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b.pendingMu.Lock()
	cancel, ok := b.cancellers[id]
	b.pendingMu.Unlock()
	if !ok {
		http.Error(w, "no such task", http.StatusNotFound)
		return
	}
	cancel()
	w.WriteHeader(http.StatusNoContent)
}

func (b *Broker) signal(w http.ResponseWriter, r *http.Request, ok bool) {
	id := r.PathValue("id")
	b.pendingMu.Lock()
	ch, exists := b.pending[id]
	b.pendingMu.Unlock()
	if !exists {
		http.Error(w, "no such pending task", http.StatusNotFound)
		return
	}
	select {
	case ch <- ok:
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "already signaled", http.StatusConflict)
	}
}

// firstLine returns the first line of s, sanitized, for a sane commit subject
// from an attacker-influenced instruction. Strips control characters and
// ANSI escapes (they'd visually corrupt `git log` and terminal output), and
// drops a leading '-' so the subject can't be confused for a git option
// when re-used in some future tool. Capped at 72 chars.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		// Keep printable ASCII + a tolerant unicode range (no controls).
		if r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	for len(out) > 0 && out[0] == '-' {
		out = strings.TrimSpace(out[1:])
	}
	if out == "" {
		out = "agent task"
	}
	if len(out) > 72 {
		out = out[:72]
	}
	return out
}

// safeErr renders an error for reflection in an HTTP response body. err.Error()
// can carry attacker-influenced bytes (agent stderr, container-CLI output);
// reflecting those raw makes upstream SIEM ingestion brittle and lets a clever
// agent inject ANSI escapes into operator terminals. Strip non-printables and
// cap.
func safeErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 200 {
		out = out[:200] + "…"
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// modelEnv resolves the model passthrough for a task: the per-task value wins,
// then the operator default. When both are empty the env stays unset so
// entrypoint.sh skips `--model` and claude-code picks its own default.
func modelEnv(taskModel, defaultModel string) []string {
	switch {
	case taskModel != "":
		return []string{"DRYDOCK_MODEL=" + taskModel}
	case defaultModel != "":
		return []string{"DRYDOCK_MODEL=" + defaultModel}
	}
	return nil
}

// resolveAgent picks the agent (task value → operator default → "claude") and
// returns the credential provider for its vendor. status is 0 when usable;
// otherwise status is the HTTP code and msg the client-facing reason. It is
// fail-closed: unknown agents and vendors with no configured key are rejected.
func (b *Broker) resolveAgent(taskAgent string) (name string, prov creds.Provider, status int, msg string) {
	name = taskAgent
	if name == "" {
		name = b.DefaultAgent
	}
	if name == "" {
		name = "claude"
	}
	vendor, known := agent.Vendor(name)
	if !known {
		return name, nil, http.StatusBadRequest, "unknown agent: " + name + " (want claude|codex)"
	}
	prov = b.Providers[vendor]
	if prov == nil {
		return name, nil, http.StatusBadRequest, "agent unavailable — no API key configured for " + name
	}
	return name, prov, 0, ""
}
