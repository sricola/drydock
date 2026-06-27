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
	// Draft opens the PR/MR as a draft (gh/glab --draft; Gitea via a WIP:
	// title prefix). Default false.
	Draft bool `json:"draft"`
}

// SquidControl registers/deregisters per-task egress widening with squid.
// nil on a Broker disables widening enforcement (non-widened tasks and tests).
type SquidControl interface {
	AddTask(user, secret string, domains []string) error
	RemoveTask(user string) error
}

type Broker struct {
	Cfg          egress.Config
	Providers    map[string]creds.Provider // vendor -> provider
	DefaultAgent string                    // "" -> "claude"
	ImageRef     string
	StageRoot    string
	AuditRoot    string
	Timeout      time.Duration
	// ApprovalTimeout, when > 0, auto-denies a task waiting at an approval gate
	// after this long and frees its concurrency slot. 0 = wait indefinitely.
	ApprovalTimeout time.Duration
	Network         string       // stable egress network name (e.g. drydock-egress)
	GatewayIP       string       // vmnet gateway IP the VM reaches (e.g. 192.168.64.1)
	ProxyPort       int          // squid port (e.g. 3128)
	Squid           SquidControl // per-task egress widening; nil = disabled
	TaskBudget      float64      // USD budget per task
	DefaultModel    string       // operator-level default; per-task Task.Model overrides
	// OpenAICompatModel is the model id for the openai-compat lane (from
	// config openai_compat.model). It's the per-task default for an opencode
	// task when --model isn't passed, since that vendor has no built-in model.
	OpenAICompatModel string
	Notify            bool   // fire macOS notifications on approval gates (config notifications)
	AnthropicAuth     string // "api_key" | "subscription"; recorded per task for `drydock tasks`
	OpenAIAuth        string // "api_key" | "subscription"; recorded per task for `drydock tasks`

	// Test seams. nil in production -> the real implementations
	// (defaultPrepareStage / runContainer). White-box tests inject fakes to
	// drive HandleTask without a git clone or a container run.
	prepareStage func(root, repoRef string) (taskStage, error)
	runAgent     func(ctx context.Context, args []string, stdout, stderr io.Writer) error
	// newAdapter selects the remote PR/MR adapter. nil in production ->
	// remote.AdapterFor. White-box tests inject a fake to drive the
	// best-effort PR-open path without shelling out to gh/glab/tea.
	newAdapter func(repoRef, platform string) remote.Adapter

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
	Push(branch, msg string) error
	PushEnv() []string
	Cleanup() error
}

type realStage struct{ s *stage.Stage }

func (r realStage) WorkDir() string { return r.s.WorkDir }
func (r realStage) WriteTaskFiles(prompt, allowlist string) error {
	return r.s.WriteTaskFiles(prompt, allowlist)
}
func (r realStage) CaptureDiff() (string, error)  { return r.s.CaptureDiff() }
func (r realStage) Push(branch, msg string) error { return r.s.Push(branch, msg) }
func (r realStage) PushEnv() []string             { return r.s.PushEnv() }
func (r realStage) Cleanup() error                { return r.s.Cleanup() }

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
	hosts := make([]string, 0, len(extras))
	for _, d := range extras {
		hosts = append(hosts, d.Host)
	}
	if err := b.Squid.AddTask(user, secret, hosts); err != nil {
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

	sw := newStream(w)
	sw.emit(map[string]any{"event": "accepted", "task_id": taskID, "repo": t.RepoRef})

	// Egress widening: block at the same kind of human-driven gate as the
	// diff push. Without this the requires_approval flag is a lie —
	// auto-approve would let any task ask for any host.
	if len(t.EgressExtra) > 0 && b.Cfg.PerTaskWidening.RequiresApproval {
		b.setStage(taskID, StageAwaitingEgress)
		sw.emit(map[string]any{
			"event": "stage", "stage": "awaiting_egress", "task_id": taskID,
			"extras":  summariseExtras(t.EgressExtra),
			"approve": "drydock approve " + taskID,
			"deny":    "drydock deny " + taskID,
		})
		b.setEgressExtra(taskID, t.EgressExtra)
		ok := b.gateEgressWiden(taskCtx, taskID, t.EgressExtra)
		b.setEgressExtra(taskID, nil)
		if !ok {
			if taskCtx.Err() != nil {
				sw.emit(map[string]any{"event": "result", "outcome": "cancelled", "task_id": taskID})
				return
			}
			sw.emit(errorEvent(taskID, "egress widening denied", ""))
			return
		}
		b.setStage(taskID, StageRunning)
	}

	// Register per-task egress widening (no-op for non-widened tasks). The
	// returned userinfo scopes the extra hosts to THIS task's proxy credential;
	// cleanup deregisters on every exit path. Fail-closed.
	proxyAuth, widenCleanup, err := b.setupWidening(taskID, t.EgressExtra)
	if err != nil {
		sw.emit(errorEvent(taskID, "egress widening setup failed", ""))
		return
	}
	defer widenCleanup()

	stageDir := filepath.Join(b.StageRoot, taskID)

	prepare := b.prepareStage
	if prepare == nil {
		prepare = defaultPrepareStage
	}
	sw.emit(map[string]any{"event": "stage", "stage": "preparing", "task_id": taskID})
	st, err := prepare(stageDir, t.RepoRef)
	if err != nil {
		sw.emit(errorEvent(taskID, "clone failed", "check the repo URL and that brokerd can reach it"))
		return
	}
	defer st.Cleanup() // wipe the host scratch (work tree + host-only git dir)

	allowlist := egress.CompileAllowlist(b.Cfg, t.EgressExtra)
	if err := st.WriteTaskFiles(t.Instruction, allowlist); err != nil {
		sw.emit(errorEvent(taskID, "stage failed", ""))
		return
	}

	agentName, prov, status, msg := b.resolveAgent(t.Agent)
	if status != 0 {
		sw.emit(errorEvent(taskID, msg, ""))
		return
	}
	grant, err := prov.Mint(b.TaskBudget)
	if err != nil {
		sw.emit(errorEvent(taskID, "credential mint failed", ""))
		return
	}
	defer grant.Revoke()

	// 0o700 keeps another local process from enumerating task IDs and
	// racing /admin/approve before the operator. The audit dir contains
	// the diff, the prompt, and the full stream-json trace — none of it
	// should be world-readable.
	if err := os.MkdirAll(b.AuditRoot, 0o700); err != nil {
		sw.emit(errorEvent(taskID, "audit setup failed", ""))
		return
	}
	// 0o600 on the audit log: same reasoning. os.Create would create at
	// 0666 (umask-reduced); be explicit.
	logf, err := os.OpenFile(
		filepath.Join(b.AuditRoot, taskID+".jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		sw.emit(errorEvent(taskID, "audit setup failed", ""))
		return
	}
	defer logf.Close()

	// Record this task's auth mode as the first audit line so `drydock tasks`
	// labels subscription runs accurately (instead of inferring from the
	// operator's current config at display time). It is not a `result` event,
	// so it never affects outcome/cost parsing.
	taskVendor, _ := agent.Vendor(agentName)
	subscription := (taskVendor == "anthropic" && b.AnthropicAuth == "subscription") ||
		(taskVendor == "openai" && b.OpenAIAuth == "subscription")
	fmt.Fprintf(logf, `{"type":"drydock_meta","subscription":%t,"sensitive":%t}`+"\n", subscription, t.Sensitive)

	env := append([]string{}, grant.EnvVars()...)
	env = append(env,
		fmt.Sprintf("HTTPS_PROXY=http://%s%s:%d", proxyAuth, b.GatewayIP, b.ProxyPort),
		fmt.Sprintf("HTTP_PROXY=http://%s%s:%d", proxyAuth, b.GatewayIP, b.ProxyPort),
		// Bypass squid for the credential gateway itself — squid's allowlist
		// is hostname-based and would deny a CONNECT to the gateway IP.
		"NO_PROXY=127.0.0.1,localhost,"+b.GatewayIP,
		"DRYDOCK_GW_IP="+b.GatewayIP,
	)
	defaultModel := effectiveDefaultModel(b.DefaultModel, taskVendor)
	env = append(env, modelEnv(taskModelFor(t.Model, b.OpenAICompatModel, taskVendor), defaultModel)...)
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
	b.setStage(taskID, StageRunning)
	runningEv := map[string]any{"event": "stage", "stage": "running", "task_id": taskID, "agent": agentName}
	if m := t.Model; m != "" {
		runningEv["model"] = m
	} else if b.DefaultModel != "" {
		runningEv["model"] = b.DefaultModel
	}
	sw.emit(runningEv)
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
			sw.emit(map[string]any{"event": "result", "outcome": "cancelled", "task_id": taskID})
			return
		}
		// If claude never wrote a `result` event (e.g. the entrypoint died
		// before claude was even exec'd), `drydock tasks` would show this
		// task as `running?` forever. Append a synthetic terminal event so
		// the audit log is self-describing.
		_, _ = fmt.Fprintf(logf,
			`{"type":"result","subtype":"error","is_error":true,"duration_ms":%d,"total_cost_usd":0,"num_turns":0}`+"\n",
			time.Since(taskStart).Milliseconds())
		auditPath := filepath.Join(b.AuditRoot, taskID+".jsonl")
		reason := "task failed: " + safeErr(err)
		ev := map[string]any{"event": "error", "task_id": taskID,
			"audit": auditPath, "duration_ms": time.Since(taskStart).Milliseconds()}
		if line, ok := reasonFromAudit(auditPath); ok {
			// The distilled line is the agent's own output — sanitize it like
			// any other operator-reflected, attacker-influenceable text.
			reason = safeStr(line)
			ev["hint"] = "run `drydock doctor` to check the sandbox image"
		}
		ev["reason"] = reason
		sw.emit(ev)
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
		sw.emit(errorEvent(taskID, "diff capture failed", ""))
		return
	}
	auditPath := filepath.Join(b.AuditRoot, taskID+".jsonl")
	if diff == "" {
		sw.emit(map[string]any{"event": "result", "outcome": "no_diff",
			"task_id": taskID, "duration_ms": time.Since(taskStart).Milliseconds(),
			"cost_usd": auditCost(auditPath)})
		return
	}

	files, insertions, deletions := diffStat(diff)
	b.setStage(taskID, StagePending)
	// Only announce the approval gate when there's actually a human gate to
	// wait on. Auto-approve pushes immediately, so an "awaiting_approval"
	// stage would be a misleading blip in the stream.
	if !t.AutoApprove {
		sw.emit(map[string]any{"event": "stage", "stage": "awaiting_approval",
			"task_id": taskID, "diff_bytes": len(diff), "files": files,
			"approve": "drydock approve " + taskID,
			"deny":    "drydock deny " + taskID,
			"review":  "drydock review " + taskID})
	}
	if !b.gatePush(taskCtx, taskID, diff, t.AutoApprove) {
		outcome := "denied"
		if taskCtx.Err() != nil {
			outcome = "cancelled"
		}
		sw.emit(map[string]any{"event": "result", "outcome": outcome,
			"task_id": taskID, "diff_bytes": len(diff)})
		return
	}

	b.setStage(taskID, StagePushing)
	branch := "agent/" + taskID
	adapterFor := b.newAdapter
	if adapterFor == nil {
		adapterFor = remote.AdapterFor
	}
	adapter := adapterFor(t.RepoRef, t.Platform)
	sw.emit(map[string]any{"event": "stage", "stage": "pushing", "task_id": taskID, "branch": branch})
	if err := st.Push(branch, "agent: "+firstLine(t.Instruction)); err != nil {
		sw.emit(errorEvent(taskID, "push failed: "+safeErr(err), "check the remote and push credentials"))
		return
	}
	// Branch is saved. Opening the PR/MR is best-effort — never downgrade a
	// successful push to a failure.
	title, body := prContent(t.Instruction, taskID)
	prErr := adapter.OpenRequest(remote.Request{
		WorkDir: st.WorkDir(), Branch: branch, Env: st.PushEnv(),
		Title: title, Body: body, Draft: t.Draft,
	})
	ev := map[string]any{"event": "result", "outcome": "pushed",
		"task_id": taskID, "branch": branch, "platform": adapter.Name(),
		"pr_opened": prErr == nil,
		"files":     files, "insertions": insertions, "deletions": deletions,
		"duration_ms": time.Since(taskStart).Milliseconds(), "cost_usd": auditCost(auditPath)}
	if prErr != nil {
		ev["pr_error"] = safeErr(prErr)
		ev["pr_hint"] = "branch '" + branch + "' was pushed; open a PR manually (" + adapter.Name() + ")"
	}
	sw.emit(ev)
}

// gateEgressWiden blocks until POST /admin/approve/{id} or /admin/deny/{id}
// (or the HTTP client disconnects / the task is killed). Returning false
// aborts the task before any allowlist compilation — the requested hosts
// never reach squid. Mirrors gatePush so the operator only has to learn one
// approval flow.
func (b *Broker) gateEgressWiden(ctx context.Context, taskID string, extras []egress.Domain) bool {
	if b.ApprovalTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, b.ApprovalTimeout)
		defer cancel()
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

	// Persist the request next to the audit so reviewers have a stable
	// artifact (the in-flight TaskState would disappear on a brokerd crash).
	widenPath := filepath.Join(b.AuditRoot, taskID+".widen.json")
	if err := os.MkdirAll(b.AuditRoot, 0o700); err == nil {
		if payload, jerr := json.MarshalIndent(extras, "", "  "); jerr == nil {
			if werr := os.WriteFile(widenPath, payload, 0o600); werr != nil {
				slog.Warn("could not persist egress-widen request", "task_id", taskID, "err", werr)
			}
		}
	}
	summary := summariseExtras(extras)
	slog.Info("task awaiting egress widening",
		"task_id", taskID, "extras", summary,
		"hint", "drydock approve "+taskID+" | drydock deny "+taskID)
	b.notifyMac("drydock — task wants more egress",
		fmt.Sprintf("task %s · %s · drydock approve %s", taskID, summary, taskID))

	select {
	case ok := <-ch:
		return ok
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			slog.Warn("task auto-denied at egress gate (approval_timeout reached)", "task_id", taskID, "timeout", b.ApprovalTimeout)
		} else {
			slog.Info("task cancelled at egress gate", "task_id", taskID)
		}
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
	if b.ApprovalTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, b.ApprovalTimeout)
		defer cancel()
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
	if werr := os.WriteFile(diffPath, []byte(diff), 0o600); werr != nil {
		slog.Warn("could not persist diff for review", "task_id", taskID, "path", diffPath, "err", werr)
	}
	slog.Info("task awaiting approval",
		"task_id", taskID, "diff_bytes", len(diff), "diff_path", diffPath,
		"hint", "drydock approve "+taskID+" | drydock deny "+taskID)
	b.notifyMac("drydock — task awaiting approval",
		fmt.Sprintf("task %s · %d byte diff · drydock approve %s", taskID, len(diff), taskID))

	select {
	case ok := <-ch:
		return ok
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			slog.Warn("task auto-denied at approval gate (approval_timeout reached)", "task_id", taskID, "timeout", b.ApprovalTimeout)
		} else {
			slog.Info("task client disconnected before approval; aborting push", "task_id", taskID)
		}
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

// notifyMac fires a macOS notification via osascript. Silent no-op when the
// operator opts out (config notifications: false / DRYDOCK_NO_NOTIFY=1) or
// when osascript isn't on PATH (i.e. running on Linux for tests/CI). We
// swallow errors: a missing notification must never block the approval gate.
func (b *Broker) notifyMac(title, body string) {
	if !b.Notify {
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
	if r := []rune(out); len(r) > 72 {
		out = string(r[:72])
	}
	return out
}

// prContent derives a PR title and body from the task instruction. Title is the
// first line, clipped to 72 chars (PR titles must stay short). Body is the
// instruction plus a drydock provenance footer, capped at ~4 KB so it never
// blows argv limits (the full instruction is preserved in the task audit). An
// empty instruction yields ("",""), so adapters fall back to the CLI's --fill.
func prContent(instruction, taskID string) (title, body string) {
	if strings.TrimSpace(instruction) == "" {
		return "", ""
	}
	title = firstLine(instruction)
	if r := []rune(title); len(r) > 72 {
		title = string(r[:71]) + "…"
	}
	const bodyCap = 4096
	body = instruction
	if len(body) > bodyCap {
		body = body[:bodyCap] + "\n\n[truncated — full instruction in the drydock task audit]"
	}
	body += "\n\n---\nGenerated by drydock (task " + taskID + ")."
	return title, body
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
	return safeStr(err.Error())
}

// safeStr strips non-printable bytes from operator-reflected text. Any output
// that traces back to the agent (its stdout/stderr, a distilled audit line) can
// carry attacker-influenced bytes; reflecting them raw lets a clever agent
// inject ANSI escapes into operator terminals. Strip non-printables and cap.
func safeStr(s string) string {
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

// errorEvent builds a terminal error event. hint may be empty.
func errorEvent(taskID, reason, hint string) map[string]any {
	ev := map[string]any{"event": "error", "task_id": taskID, "reason": reason}
	if hint != "" {
		ev["hint"] = hint
	}
	return ev
}

// taskModelFor picks the per-task model before the operator default is applied.
// An explicit --model always wins. Otherwise the openai-compat vendor
// (opencode) falls back to the configured openai_compat.model, since that lane
// has no built-in model the way claude/codex do.
func taskModelFor(taskModel, openAICompatModel, vendor string) string {
	if taskModel == "" && vendor == "openai-compat" {
		return openAICompatModel
	}
	return taskModel
}

// effectiveDefaultModel applies the operator DefaultModel only where it makes
// sense. The operator default is claude/codex-oriented; it must not leak into
// the opencode lane (it'd become `-m drydock/<claude-model>` and not resolve).
// For openai-compat the model comes only from --model or openai_compat.model.
func effectiveDefaultModel(operatorDefault, vendor string) string {
	if vendor == "openai-compat" {
		return ""
	}
	return operatorDefault
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
