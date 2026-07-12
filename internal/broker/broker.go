// Package broker wires staging, egress compilation, credential minting, the
// container run, diff capture, the approval gate, and the host-side push.
package broker

import (
	"context"
	"encoding/json"
	"errors"
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
	"syscall"
	"time"

	"drydock/internal/audit"
	"drydock/internal/creds"
	"drydock/internal/egress"
	"drydock/internal/provider"
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
	AddTask(user, secret string, domains []egress.Domain) error
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

	PushMaxRetries       int
	PushRetryBackoff     time.Duration
	PushFreshBranchTries int

	// AggregateExceeded, when set, is consulted at task submission: if it
	// returns true for the task's vendor, the submission is rejected (402)
	// before the stream starts and before any lease is minted. nil disables
	// the pre-check. Wired to the gateway's AggregateExceeded by brokerd.
	AggregateExceeded func(vendor string) bool

	// Test seams. nil in production -> the real implementations
	// (defaultPrepareStage / runContainer). White-box tests inject fakes to
	// drive HandleTask without a git clone or a container run.
	prepareStage func(root, repoRef string) (taskStage, error)
	runAgent     func(ctx context.Context, args []string, stdout, stderr io.Writer) error
	// newAdapter selects the remote PR/MR adapter. nil in production ->
	// remote.AdapterFor. White-box tests inject a fake to drive the
	// best-effort PR-open path without shelling out to gh/glab/tea.
	newAdapter func(repoRef, platform string) remote.Adapter
	// reopenStage reopens an existing stage by its root path. nil in production
	// falls back to defaultReopenStage (wraps stage.Reopen). Tests inject a fake
	// to drive ResumeAwaiting without a real git directory on disk.
	reopenStage func(root string) (taskStage, error)

	// MaxConcurrent caps how many tasks may be in any non-terminal state at
	// once. Excess POSTs to /tasks return 503. Default (when zero) is 2.
	MaxConcurrent int

	// slots is a bounded semaphore guarding MaxConcurrent. Initialized lazily
	// the first time HandleTask is called (so existing callers that build a
	// Broker by struct literal keep working).
	slotsOnce sync.Once
	slots     chan struct{}

	pendingMu  sync.Mutex
	pending    map[string]chan bool               // task_id -> approval channel
	tasks      map[string]*TaskState              // task_id -> live state (running + awaiting_approval)
	cancellers map[string]context.CancelCauseFunc // task_id -> cancel hook for in-flight kill
}

// taskStage is the subset of *stage.Stage that HandleTask uses. It exists so
// white-box tests can drive the handler without a real git clone; production
// uses realStage, a thin adapter over *stage.Stage.
type taskStage interface {
	WorkDir() string
	WriteTaskFiles(prompt string) error
	CaptureDiff() (string, error)
	Push(branch, msg string) error
	Commit(branch, message string) error
	PushBranch(localBranch, remoteBranch string) error
	PushEnv() []string
	Cleanup() error
}

type realStage struct{ s *stage.Stage }

func (r realStage) WorkDir() string { return r.s.WorkDir }
func (r realStage) WriteTaskFiles(prompt string) error {
	return r.s.WriteTaskFiles(prompt)
}
func (r realStage) CaptureDiff() (string, error)          { return r.s.CaptureDiff() }
func (r realStage) Push(branch, msg string) error         { return r.s.Push(branch, msg) }
func (r realStage) Commit(branch, msg string) error       { return r.s.Commit(branch, msg) }
func (r realStage) PushBranch(local, remote string) error { return r.s.PushBranch(local, remote) }
func (r realStage) PushEnv() []string                     { return r.s.PushEnv() }
func (r realStage) Cleanup() error                        { return r.s.Cleanup() }

// defaultPrepareStage is the production prepareStage: a real host clone with
// the .git dir moved out of the mounted work tree.
func defaultPrepareStage(root, repoRef string) (taskStage, error) {
	s, err := stage.Prepare(root, repoRef)
	if err != nil {
		return nil, err
	}
	return realStage{s}, nil
}

// defaultReopenStage is the production reopenStage: reopens an existing stage
// directory left on disk by a prior brokerd life (used by ResumeAwaiting).
func defaultReopenStage(root string) (taskStage, error) {
	s, err := stage.Reopen(root)
	if err != nil {
		return nil, err
	}
	return realStage{s: s}, nil
}

// runContainer is the production runAgent: it runs the Apple `container` CLI
// for the task and streams its output to the audit log.
func runContainer(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "container", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// MaxTaskBodyBytes caps the size of POST /tasks bodies. Generous enough for
// long instructions but small enough that local-DoS via 1GB instruction
// strings (or TCP-listener attacks when BROKER_ADDR is set) can't burn
// memory unbounded.
const MaxTaskBodyBytes = 64 << 10

// taskRun holds the per-task state that HandleTask threads through the task
// lifecycle. It exists so the stateful lifecycle steps (runEgressGate,
// runSandbox, pushAndOpenPR) can be methods with collapsed signatures instead
// of free functions carrying nine-to-twelve parameters each. HandleTask builds
// exactly one taskRun and fills its fields in as they become available; the
// deferred cleanups (slot release, cancel, unregister, widen cleanup,
// stage cleanup, log close, grant revoke) deliberately stay in HandleTask's
// scope so they fire at function return, not when a method returns early.
type taskRun struct {
	b   *Broker         // back-reference to the owning broker
	ctx context.Context // per-task context (rooted at Background, not the request)
	sw  *stream         // NDJSON event stream to the submit client
	id  string          // task ID

	// Request-derived, known when the taskRun is built.
	repoRef     string
	instruction string
	egressExtra []egress.Domain
	autoApprove bool
	draft       bool
	platform    string
	model       string

	// Filled in as HandleTask advances through the lifecycle.
	proxyAuth  string      // "<user>:<secret>@" widening userinfo (empty if none)
	st         taskStage   // prepared host stage
	grant      creds.Grant // minted ephemeral credential
	agentName  string      // resolved agent ("claude"|"codex"|...)
	taskVendor string      // vendor for the resolved agent
	logf       io.Writer   // audit log writer
	auditPath  string      // path to the audit .jsonl
	taskStart  time.Time   // set by runSandbox when the agent starts

	// keepStage, when true, suppresses the deferred stage Cleanup so the stage
	// directory survives a brokerd shutdown and can be resumed at next boot.
	// Set by pushAndOpenPR and resumePush when gatePushMarked returns gateShutdown.
	keepStage bool
}

// errTaskTerminated signals that a lifecycle method has already emitted the
// task's terminal event (cancelled / error / etc.) and HandleTask must return
// immediately without emitting anything further. It replaces the old
// (time.Time, bool) control-flow smuggling out of runSandbox: nil means
// "continue", a non-nil error means "stop, the terminal event is already out".
var errTaskTerminated = errors.New("task terminated; terminal event already emitted")

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

	// One context per task, deliberately rooted at Background (NOT r.Context()):
	// a submit client disconnecting — CLI ^C, or the web UI closing the
	// connection right after the `accepted` line — must NOT cancel the task.
	// Cancellation is driven only by /admin/kill (the stored cancel) and
	// brokerd shutdown (CancelAll iterates the stored cancels). Event writes to
	// the response become best-effort (emit already ignores write errors).
	taskCtx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
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

	// Aggregate budget pre-check: reject at submit time (before the stream
	// starts and before any lease is minted) when this task's vendor is
	// already at or over its cross-task cap. The task never starts. Skipped
	// when the cap is disabled (hook nil) or the vendor can't be resolved yet
	// (the real resolveAgent below emits the proper error).
	if b.AggregateExceeded != nil {
		if an, _, err := b.resolveAgent(t.Agent); err == nil {
			if v, _ := provider.VendorForAgent(an); v != "" && b.AggregateExceeded(v) {
				http.Error(w, "aggregate budget exhausted for "+v, http.StatusPaymentRequired)
				return
			}
		}
	}

	sw := newStream(w)
	sw.emit(map[string]any{"event": "accepted", "task_id": taskID, "repo": t.RepoRef})

	// All the per-task state below is threaded through the lifecycle steps as
	// taskRun fields rather than long parameter lists. Fields are filled in as
	// they become available; the defers above and below stay in this scope.
	tr := &taskRun{
		b:           b,
		ctx:         taskCtx,
		sw:          sw,
		id:          taskID,
		repoRef:     t.RepoRef,
		instruction: t.Instruction,
		egressExtra: t.EgressExtra,
		autoApprove: t.AutoApprove,
		draft:       t.Draft,
		platform:    t.Platform,
		model:       t.Model,
	}

	// Egress widening: block at the same kind of human-driven gate as the
	// diff push. Without this the requires_approval flag is a lie —
	// auto-approve would let any task ask for any host.
	if !tr.runEgressGate() {
		return
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
	tr.proxyAuth = proxyAuth

	stageDir := filepath.Join(b.StageRoot, taskID)

	prepare := b.prepareStage
	if prepare == nil {
		prepare = defaultPrepareStage
	}
	// Preflight: refuse to start a task onto an almost-full host disk (fail
	// closed rather than pile a fresh clone + run onto a disk about to fill).
	if free, ferr := freeBytes(b.StageRoot); ferr == nil && free < minFreeStageBytes {
		slog.Warn("refusing task: host low on disk", "task_id", taskID, "free_mib", free>>20)
		sw.emit(errorEvent(taskID, "host low on disk",
			fmt.Sprintf("only %d MiB free at the stage root; free space before submitting", free>>20)))
		return
	}
	sw.emit(map[string]any{"event": "stage", "stage": "preparing", "task_id": taskID})
	st, err := prepare(stageDir, t.RepoRef)
	if err != nil {
		slog.Warn("task clone failed", "task_id", taskID, "err", err)
		sw.emit(errorEvent(taskID, "clone failed", "check the repo URL and that brokerd can reach it"))
		return
	}
	defer func() {
		if !tr.keepStage {
			_ = st.Cleanup()
		}
	}()
	tr.st = st

	if err := st.WriteTaskFiles(t.Instruction); err != nil {
		slog.Warn("task stage failed", "task_id", taskID, "err", err)
		sw.emit(errorEvent(taskID, "stage failed", ""))
		return
	}

	agentName, prov, err := b.resolveAgent(t.Agent)
	if err != nil {
		sw.emit(errorEvent(taskID, err.Error(), ""))
		return
	}
	grant, err := prov.Mint(b.TaskBudget)
	if err != nil {
		slog.Warn("task credential mint failed", "task_id", taskID, "err", err)
		sw.emit(errorEvent(taskID, "credential mint failed", ""))
		return
	}
	defer grant.Revoke()
	tr.grant = grant
	tr.agentName = agentName

	// 0o700 keeps another local process from enumerating task IDs and
	// racing /admin/approve before the operator. The audit dir contains
	// the diff, the prompt, and the full stream-json trace — none of it
	// should be world-readable.
	if err := os.MkdirAll(b.AuditRoot, 0o700); err != nil {
		slog.Warn("task audit setup failed", "task_id", taskID, "err", err)
		sw.emit(errorEvent(taskID, "audit setup failed", ""))
		return
	}
	// 0o600 on the audit log: same reasoning. os.Create would create at
	// 0666 (umask-reduced); be explicit.
	logf, err := os.OpenFile(
		filepath.Join(b.AuditRoot, taskID+".jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		slog.Warn("task audit log open failed", "task_id", taskID, "err", err)
		sw.emit(errorEvent(taskID, "audit setup failed", ""))
		return
	}
	// fsync the trace on the way out so a hard crash can't lose the terminal
	// result line — the audit is the source of truth for outcome/cost, and a
	// lost last line reads as "running?" forever. Close is registered first so
	// it runs LAST (LIFO); Sync registered after runs first: flush, then close.
	defer logf.Close()
	defer func() { _ = logf.Sync() }()
	tr.logf = logf
	tr.auditPath = filepath.Join(b.AuditRoot, taskID+".jsonl")

	// Record this task's auth mode as the first audit line so `drydock tasks`
	// labels subscription runs accurately (instead of inferring from the
	// operator's current config at display time). It is not a `result` event,
	// so it never affects outcome/cost parsing.
	taskVendor, _ := provider.VendorForAgent(agentName)
	tr.taskVendor = taskVendor
	subscription := (taskVendor == "anthropic" && b.AnthropicAuth == "subscription") ||
		(taskVendor == "openai" && b.OpenAIAuth == "subscription")
	fmt.Fprintf(logf, `{"type":"drydock_meta","subscription":%t,"sensitive":%t}`+"\n", subscription, t.Sensitive)

	// Persist the invocation so `drydock retry <id>` can re-run this task
	// without the operator reconstructing repo+prompt+flags by hand. Marshaled
	// (not fmt'd) so the instruction can't break the JSON. auto_approve is
	// deliberately NOT recorded — a retry re-enters the approval gate unless the
	// operator opts back in. Not a `result`/`drydock_meta` line, so it doesn't
	// affect outcome/cost parsing.
	if inv, err := json.Marshal(map[string]any{
		"type": "drydock_task", "repo_ref": t.RepoRef, "instruction": t.Instruction,
		"agent": t.Agent, "model": t.Model, "platform": t.Platform,
		"egress_extra": t.EgressExtra, "draft": t.Draft, "sensitive": t.Sensitive,
	}); err == nil {
		fmt.Fprintf(logf, "%s\n", inv)
	}

	args := runner.BuildRunArgs(runner.Spec{
		TaskID:     taskID,
		Network:    b.Network,
		ImageRef:   b.ImageRef,
		Env:        tr.buildEnv(),
		StageDir:   st.WorkDir(),
		PromptFile: "/work/.task/prompt.txt",
		MemoryGB:   4,
		CPUs:       4,
	})

	if err := tr.runSandbox(args); err != nil {
		return // runSandbox already emitted the terminal event
	}

	diff, err := st.CaptureDiff()
	if err != nil {
		sw.emit(errorEvent(taskID, "diff capture failed", ""))
		return
	}
	if diff == "" {
		sw.emit(map[string]any{"event": "result", "outcome": "no_diff",
			"task_id": taskID, "duration_ms": time.Since(tr.taskStart).Milliseconds(),
			"cost_usd": audit.TotalCost(tr.auditPath)})
		return
	}

	tr.pushAndOpenPR(diff)
}

// runEgressGate handles the awaiting_egress stage when the task requests extra
// egress and requires_approval is set. Returns true to continue, false to abort
// (the appropriate terminal event has already been emitted).
func (tr *taskRun) runEgressGate() bool {
	b, extras := tr.b, tr.egressExtra
	if len(extras) == 0 || !b.Cfg.WideningRequiresApproval() {
		return true
	}
	b.setStage(tr.id, StageAwaitingEgress)
	tr.sw.emit(map[string]any{
		"event": "stage", "stage": "awaiting_egress", "task_id": tr.id,
		"extras":  summariseExtras(extras),
		"approve": "drydock approve " + tr.id,
		"deny":    "drydock deny " + tr.id,
	})
	b.setEgressExtra(tr.id, extras)
	ok := b.gateEgressWiden(tr.ctx, tr.id, extras)
	b.setEgressExtra(tr.id, nil)
	if !ok {
		if tr.ctx.Err() != nil {
			tr.sw.emit(map[string]any{"event": "result", "outcome": "cancelled", "task_id": tr.id})
			return false
		}
		tr.sw.emit(errorEvent(tr.id, "egress widening denied", ""))
		return false
	}
	b.setStage(tr.id, StageRunning)
	return true
}

// buildTaskEnv assembles the env slice passed to the container. It is pure
// (all inputs explicit) so it can be unit-tested without a Broker.
func buildTaskEnv(grantEnv []string, proxyAuth, gatewayIP string, proxyPort int,
	agentName, taskModel, openAICompatModel, operatorDefaultModel, taskVendor string) []string {
	env := append([]string{}, grantEnv...)
	env = append(env,
		fmt.Sprintf("HTTPS_PROXY=http://%s%s:%d", proxyAuth, gatewayIP, proxyPort),
		fmt.Sprintf("HTTP_PROXY=http://%s%s:%d", proxyAuth, gatewayIP, proxyPort),
		// Bypass squid for the credential gateway itself — squid's allowlist
		// is hostname-based and would deny a CONNECT to the gateway IP.
		"NO_PROXY=127.0.0.1,localhost,"+gatewayIP,
		"DRYDOCK_GW_IP="+gatewayIP,
	)
	defaultModel := effectiveDefaultModel(operatorDefaultModel, taskVendor)
	env = append(env, modelEnv(taskModelFor(taskModel, openAICompatModel, taskVendor), defaultModel)...)
	env = append(env, "DRYDOCK_AGENT="+agentName)
	return env
}

// buildEnv assembles the container env from the taskRun's fields. It exists only
// to collapse the call-site noise — the actual assembly stays in the pure,
// unit-tested buildTaskEnv free function.
func (tr *taskRun) buildEnv() []string {
	return buildTaskEnv(tr.grant.EnvVars(), tr.proxyAuth, tr.b.GatewayIP, tr.b.ProxyPort,
		tr.agentName, tr.model, tr.b.OpenAICompatModel, tr.b.DefaultModel, tr.taskVendor)
}

// runSandbox runs the agent container, writes to the audit log, and emits the
// "running" stage event. It records the task start time on the taskRun and
// returns nil on a successful run. On failure it emits the terminal event
// (cancelled / error) and returns errTaskTerminated, signalling HandleTask to
// return immediately without emitting anything further.
func (tr *taskRun) runSandbox(args []string) error {
	b := tr.b
	runCtx, runCancel := context.WithTimeout(tr.ctx, b.Timeout)
	defer runCancel()
	run := b.runAgent
	if run == nil {
		run = runContainer
	}
	b.setStage(tr.id, StageRunning)
	runningEv := map[string]any{"event": "stage", "stage": "running", "task_id": tr.id, "agent": tr.agentName}
	if tr.model != "" {
		runningEv["model"] = tr.model
	} else if b.DefaultModel != "" {
		runningEv["model"] = b.DefaultModel
	}
	tr.sw.emit(runningEv)

	tr.taskStart = time.Now()
	// Bound the bytes an untrusted task can emit to the host: a flood (yes, a
	// runaway build) would otherwise fill the audit log and the daemon's stdout
	// unbounded. When the shared stdout+stderr budget is crossed, the task is
	// cancelled and further output is dropped.
	outCap := newOutputCap(maxTaskOutputBytes, runCancel)
	// Bound the host disk a task can consume through its writable /work bind
	// mount: cancel it if the stage grows past the byte/file caps or host free
	// space drops below the floor (fill or inode-exhaust the host FS).
	stageRoot := ""
	if tr.st != nil {
		stageRoot = tr.st.WorkDir()
	}
	sizeGuard := watchStageSize(stageRoot, stageSizeInterval, runCancel)
	defer sizeGuard.stop()
	if err := run(runCtx, args, outCap.wrap(io.MultiWriter(tr.logf, os.Stdout)), outCap.wrap(tr.logf)); err != nil {
		// --rm covers a graceful exit; on timeout/kill the VM may survive,
		// so force-remove it (best effort) to honor the ephemeral-VM backstop.
		if derr := exec.Command("container", "delete", "--force", "task-"+tr.id).Run(); derr != nil {
			slog.Warn("force-delete of task VM failed; reaped at next brokerd boot",
				"task_id", tr.id, "err", derr)
		}
		if tr.ctx.Err() != nil {
			// Operator killed it, or the client went away. Be explicit.
			tr.sw.emit(map[string]any{"event": "result", "outcome": "cancelled", "task_id": tr.id})
			return errTaskTerminated
		}
		if outCap.exceeded() {
			// We cancelled the task ourselves: its output crossed the host cap.
			_, _ = fmt.Fprintf(tr.logf,
				`{"type":"result","subtype":"error","is_error":true,"duration_ms":%d,"total_cost_usd":0,"num_turns":0}`+"\n",
				time.Since(tr.taskStart).Milliseconds())
			tr.sw.emit(map[string]any{"event": "error", "task_id": tr.id,
				"audit":       tr.auditPath,
				"duration_ms": time.Since(tr.taskStart).Milliseconds(),
				"reason":      fmt.Sprintf("task terminated: output exceeded the %d MiB host cap", maxTaskOutputBytes>>20)})
			return errTaskTerminated
		}
		if sizeGuard.exceeded() {
			// We cancelled the task ourselves: its /work grew past the host disk
			// cap, or host free space dropped below the floor.
			_, _ = fmt.Fprintf(tr.logf,
				`{"type":"result","subtype":"error","is_error":true,"duration_ms":%d,"total_cost_usd":0,"num_turns":0}`+"\n",
				time.Since(tr.taskStart).Milliseconds())
			tr.sw.emit(map[string]any{"event": "error", "task_id": tr.id,
				"audit":       tr.auditPath,
				"duration_ms": time.Since(tr.taskStart).Milliseconds(),
				"reason":      fmt.Sprintf("task terminated: /work exceeded the host disk cap (%d GiB / %d files, or host low on free space)", maxStageBytes>>30, maxStageFiles)})
			return errTaskTerminated
		}
		// If claude never wrote a `result` event (e.g. the entrypoint died
		// before claude was even exec'd), `drydock tasks` would show this
		// task as `running?` forever. Append a synthetic terminal event so
		// the audit log is self-describing.
		_, _ = fmt.Fprintf(tr.logf,
			`{"type":"result","subtype":"error","is_error":true,"duration_ms":%d,"total_cost_usd":0,"num_turns":0}`+"\n",
			time.Since(tr.taskStart).Milliseconds())
		reason := "task failed: " + safeErr(err)
		ev := map[string]any{"event": "error", "task_id": tr.id,
			"audit": tr.auditPath, "duration_ms": time.Since(tr.taskStart).Milliseconds()}
		if line, ok := audit.Reason(tr.auditPath); ok {
			// The distilled line is the agent's own output — sanitize it like
			// any other operator-reflected, attacker-influenceable text.
			reason = safeStr(line)
			ev["hint"] = "run `drydock doctor` to check the sandbox image"
		}
		ev["reason"] = reason
		tr.sw.emit(ev)
		return errTaskTerminated
	}

	// codex exec doesn't emit Claude's stream-json `result` trailer, so a
	// completed codex task would read as `running?` in `drydock tasks`.
	// Synthesize the terminal event from the elapsed time and the metered
	// gateway spend. (Claude writes its own result line; don't double it.)
	if tr.agentName != "claude" {
		_, _ = fmt.Fprintf(tr.logf,
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":%d,"total_cost_usd":%.6f,"num_turns":0}`+"\n",
			time.Since(tr.taskStart).Milliseconds(), tr.grant.Spent())
	}
	return nil
}

// pushAndOpenPR handles the diff-approval gate, branch push, and PR creation.
// It always emits a terminal event (result/outcome=denied|cancelled|pushed|push_failed).
// HandleTask should return immediately after calling this; it is the last step
// in the task lifecycle.
func (tr *taskRun) pushAndOpenPR(diff string) {
	b := tr.b
	files, insertions, deletions := diffStat(diff)
	b.setStage(tr.id, StagePending)
	// Only announce the approval gate when there's actually a human gate to
	// wait on. Auto-approve pushes immediately, so an "awaiting_approval"
	// stage would be a misleading blip in the stream.
	if !tr.autoApprove {
		tr.sw.emit(map[string]any{"event": "stage", "stage": "awaiting_approval",
			"task_id": tr.id, "diff_bytes": len(diff), "files": files,
			"approve": "drydock approve " + tr.id,
			"deny":    "drydock deny " + tr.id,
			"review":  "drydock review " + tr.id})
	}
	approved := tr.autoApprove
	cause := gateApproved
	if !tr.autoApprove {
		approved, cause = b.gatePushMarked(tr.ctx, tr, diff)
	}
	if !approved {
		outcome := "denied"
		if cause == gateKilled || cause == gateShutdown {
			outcome = "cancelled"
		}
		if cause == gateShutdown {
			tr.keepStage = true
		}
		tr.sw.emit(map[string]any{"event": "result", "outcome": outcome,
			"task_id": tr.id, "diff_bytes": len(diff)})
		return
	}

	tr.finishPush(diff, files, insertions, deletions)
}

// finishPush performs the branch push (with recovery) and PR-open after the
// approval gate has passed, emitting the terminal pushed/push_failed event and,
// on failure, the synthetic audit line. Shared by the live path and resume.
func (tr *taskRun) finishPush(diff string, files, insertions, deletions int) {
	b := tr.b
	b.setStage(tr.id, StagePushing)
	base := "agent/" + tr.id
	adapterFor := b.newAdapter
	if adapterFor == nil {
		adapterFor = remote.AdapterFor
	}
	adapter := adapterFor(tr.repoRef, tr.platform)
	tr.sw.emit(map[string]any{"event": "stage", "stage": "pushing", "task_id": tr.id, "branch": base})

	branch, attempts, reason, err := pushWithRecovery(tr.ctx, tr.st, tr.id,
		"agent: "+firstLine(tr.instruction),
		pushRetry{MaxRetries: b.PushMaxRetries, Backoff: b.PushRetryBackoff, FreshBranchTries: b.PushFreshBranchTries})
	if err != nil {
		// Nothing landed on the remote (single-ref push is atomic). Record a
		// terminal push_failed result in the audit (carrying the metered cost so
		// cost + the aggregate-cap seed stay correct) and stream the reason.
		cost := audit.TotalCost(tr.auditPath)
		fmt.Fprintf(tr.logf,
			`{"type":"result","subtype":"push_failed","is_error":false,"duration_ms":%d,"total_cost_usd":%.6f,"num_turns":0}`+"\n",
			time.Since(tr.taskStart).Milliseconds(), cost)
		tr.sw.emit(map[string]any{"event": "result", "outcome": "push_failed",
			"task_id": tr.id, "reason": string(reason), "push_attempts": attempts,
			"branch": base, "error": safeErr(err),
			"files": files, "insertions": insertions, "deletions": deletions,
			"duration_ms": time.Since(tr.taskStart).Milliseconds(), "cost_usd": cost,
			"hint": "nothing was pushed to the remote; the diff is preserved; retry with `drydock retry " + tr.id + "`"})
		return
	}
	// Branch is saved. Opening the PR/MR is best-effort; never downgrade a
	// successful push to a failure.
	title, body := prContent(tr.instruction, tr.id)
	prErr := adapter.OpenRequest(remote.Request{
		WorkDir: tr.st.WorkDir(), Branch: branch, Env: tr.st.PushEnv(),
		Title: title, Body: body, Draft: tr.draft,
	})
	ev := map[string]any{"event": "result", "outcome": "pushed",
		"task_id": tr.id, "branch": branch, "platform": adapter.Name(),
		"pr_opened": prErr == nil, "push_attempts": attempts,
		"files": files, "insertions": insertions, "deletions": deletions,
		"duration_ms": time.Since(tr.taskStart).Milliseconds(), "cost_usd": audit.TotalCost(tr.auditPath)}
	if prErr != nil {
		ev["pr_error"] = safeErr(prErr)
		ev["pr_hint"] = "branch '" + branch + "' was pushed; open a PR manually (" + adapter.Name() + ")"
	}
	tr.sw.emit(ev)
}

// resolveAgent picks the agent (task value → operator default → "claude") and
// returns the credential provider for its vendor. Returns an error when the
// agent is unknown or has no configured key — fail-closed: the task never starts.
func (b *Broker) resolveAgent(taskAgent string) (name string, prov creds.Provider, err error) {
	name = taskAgent
	if name == "" {
		name = b.DefaultAgent
	}
	if name == "" {
		name = "claude"
	}
	vendor, known := provider.VendorForAgent(name)
	if !known {
		return name, nil, fmt.Errorf("unknown agent: %s (want %s)", name, strings.Join(provider.Agents(), "|"))
	}
	prov = b.Providers[vendor]
	if prov == nil {
		return name, nil, fmt.Errorf("agent unavailable — no API key configured for %s", name)
	}
	return name, prov, nil
}
