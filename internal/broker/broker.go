// Package broker wires staging, egress compilation, credential minting, the
// container run, diff capture, the approval gate, and the host-side push.
package broker

import (
	"context"
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
	WriteTaskFiles(prompt string) error
	CaptureDiff() (string, error)
	Push(branch, msg string) error
	PushEnv() []string
	Cleanup() error
}

type realStage struct{ s *stage.Stage }

func (r realStage) WorkDir() string { return r.s.WorkDir }
func (r realStage) WriteTaskFiles(prompt string) error {
	return r.s.WriteTaskFiles(prompt)
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

	// One context per task, deliberately rooted at Background (NOT r.Context()):
	// a submit client disconnecting — CLI ^C, or the web UI closing the
	// connection right after the `accepted` line — must NOT cancel the task.
	// Cancellation is driven only by /admin/kill (the stored cancel) and
	// brokerd shutdown (CancelAll iterates the stored cancels). Event writes to
	// the response become best-effort (emit already ignores write errors).
	taskCtx, cancel := context.WithCancel(context.Background())
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
	if !b.runEgressGate(taskCtx, taskID, t.EgressExtra, sw) {
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

	stageDir := filepath.Join(b.StageRoot, taskID)

	prepare := b.prepareStage
	if prepare == nil {
		prepare = defaultPrepareStage
	}
	sw.emit(map[string]any{"event": "stage", "stage": "preparing", "task_id": taskID})
	st, err := prepare(stageDir, t.RepoRef)
	if err != nil {
		slog.Warn("task clone failed", "task_id", taskID, "err", err)
		sw.emit(errorEvent(taskID, "clone failed", "check the repo URL and that brokerd can reach it"))
		return
	}
	defer st.Cleanup() // wipe the host scratch (work tree + host-only git dir)

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
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		slog.Warn("task audit log open failed", "task_id", taskID, "err", err)
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

	env := buildTaskEnv(grant.EnvVars(), proxyAuth, b.GatewayIP, b.ProxyPort,
		agentName, t.Model, b.OpenAICompatModel, b.DefaultModel, taskVendor)

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

	auditPath := filepath.Join(b.AuditRoot, taskID+".jsonl")
	taskStart, ok := b.runSandbox(taskCtx, taskID, agentName, t.Model, args, logf, auditPath, grant, sw)
	if !ok {
		return
	}

	diff, err := st.CaptureDiff()
	if err != nil {
		sw.emit(errorEvent(taskID, "diff capture failed", ""))
		return
	}
	if diff == "" {
		sw.emit(map[string]any{"event": "result", "outcome": "no_diff",
			"task_id": taskID, "duration_ms": time.Since(taskStart).Milliseconds(),
			"cost_usd": audit.TotalCost(auditPath)})
		return
	}

	b.pushAndOpenPR(taskCtx, taskID, diff, t.Instruction, t.AutoApprove, t.Draft,
		sw, st, t.RepoRef, t.Platform, taskStart, auditPath)
}

// runEgressGate handles the awaiting_egress stage when the task requests extra
// egress and requires_approval is set. Returns true to continue, false to abort
// (the appropriate terminal event has already been emitted).
func (b *Broker) runEgressGate(ctx context.Context, taskID string, extras []egress.Domain, sw *stream) bool {
	if len(extras) == 0 || !b.Cfg.PerTaskWidening.RequiresApproval {
		return true
	}
	b.setStage(taskID, StageAwaitingEgress)
	sw.emit(map[string]any{
		"event": "stage", "stage": "awaiting_egress", "task_id": taskID,
		"extras":  summariseExtras(extras),
		"approve": "drydock approve " + taskID,
		"deny":    "drydock deny " + taskID,
	})
	b.setEgressExtra(taskID, extras)
	ok := b.gateEgressWiden(ctx, taskID, extras)
	b.setEgressExtra(taskID, nil)
	if !ok {
		if ctx.Err() != nil {
			sw.emit(map[string]any{"event": "result", "outcome": "cancelled", "task_id": taskID})
			return false
		}
		sw.emit(errorEvent(taskID, "egress widening denied", ""))
		return false
	}
	b.setStage(taskID, StageRunning)
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

// runSandbox runs the agent container, writes to the audit log, and emits the
// "running" stage event. It returns (taskStart, true) on a successful run.
// On failure it emits the terminal event and returns (taskStart, false),
// signalling the caller to return immediately.
func (b *Broker) runSandbox(taskCtx context.Context, taskID, agentName, taskModel string,
	args []string, logf io.Writer, auditPath string, grant creds.Grant, sw *stream) (time.Time, bool) {
	runCtx, runCancel := context.WithTimeout(taskCtx, b.Timeout)
	defer runCancel()
	run := b.runAgent
	if run == nil {
		run = runContainer
	}
	b.setStage(taskID, StageRunning)
	runningEv := map[string]any{"event": "stage", "stage": "running", "task_id": taskID, "agent": agentName}
	if taskModel != "" {
		runningEv["model"] = taskModel
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
			return taskStart, false
		}
		// If claude never wrote a `result` event (e.g. the entrypoint died
		// before claude was even exec'd), `drydock tasks` would show this
		// task as `running?` forever. Append a synthetic terminal event so
		// the audit log is self-describing.
		_, _ = fmt.Fprintf(logf,
			`{"type":"result","subtype":"error","is_error":true,"duration_ms":%d,"total_cost_usd":0,"num_turns":0}`+"\n",
			time.Since(taskStart).Milliseconds())
		reason := "task failed: " + safeErr(err)
		ev := map[string]any{"event": "error", "task_id": taskID,
			"audit": auditPath, "duration_ms": time.Since(taskStart).Milliseconds()}
		if line, ok := audit.Reason(auditPath); ok {
			// The distilled line is the agent's own output — sanitize it like
			// any other operator-reflected, attacker-influenceable text.
			reason = safeStr(line)
			ev["hint"] = "run `drydock doctor` to check the sandbox image"
		}
		ev["reason"] = reason
		sw.emit(ev)
		return taskStart, false
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
	return taskStart, true
}

// pushAndOpenPR handles the diff-approval gate, branch push, and PR creation.
// It always emits a terminal event (result/outcome=denied|cancelled|pushed, or
// an error event on push failure). HandleTask should return immediately after
// calling this — it is the last step in the task lifecycle.
func (b *Broker) pushAndOpenPR(taskCtx context.Context, taskID, diff, instruction string,
	autoApprove, draft bool, sw *stream, st taskStage, repoRef, platform string,
	taskStart time.Time, auditPath string) {
	files, insertions, deletions := diffStat(diff)
	b.setStage(taskID, StagePending)
	// Only announce the approval gate when there's actually a human gate to
	// wait on. Auto-approve pushes immediately, so an "awaiting_approval"
	// stage would be a misleading blip in the stream.
	if !autoApprove {
		sw.emit(map[string]any{"event": "stage", "stage": "awaiting_approval",
			"task_id": taskID, "diff_bytes": len(diff), "files": files,
			"approve": "drydock approve " + taskID,
			"deny":    "drydock deny " + taskID,
			"review":  "drydock review " + taskID})
	}
	if !b.gatePush(taskCtx, taskID, diff, autoApprove) {
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
	adapter := adapterFor(repoRef, platform)
	sw.emit(map[string]any{"event": "stage", "stage": "pushing", "task_id": taskID, "branch": branch})
	if err := st.Push(branch, "agent: "+firstLine(instruction)); err != nil {
		sw.emit(errorEvent(taskID, "push failed: "+safeErr(err), "check the remote and push credentials"))
		return
	}
	// Branch is saved. Opening the PR/MR is best-effort — never downgrade a
	// successful push to a failure.
	title, body := prContent(instruction, taskID)
	prErr := adapter.OpenRequest(remote.Request{
		WorkDir: st.WorkDir(), Branch: branch, Env: st.PushEnv(),
		Title: title, Body: body, Draft: draft,
	})
	ev := map[string]any{"event": "result", "outcome": "pushed",
		"task_id": taskID, "branch": branch, "platform": adapter.Name(),
		"pr_opened": prErr == nil,
		"files":     files, "insertions": insertions, "deletions": deletions,
		"duration_ms": time.Since(taskStart).Milliseconds(), "cost_usd": audit.TotalCost(auditPath)}
	if prErr != nil {
		ev["pr_error"] = safeErr(prErr)
		ev["pr_hint"] = "branch '" + branch + "' was pushed; open a PR manually (" + adapter.Name() + ")"
	}
	sw.emit(ev)
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
	vendor, known := agent.Vendor(name)
	if !known {
		return name, nil, fmt.Errorf("unknown agent: %s (want %s)", name, strings.Join(provider.Agents(), "|"))
	}
	prov = b.Providers[vendor]
	if prov == nil {
		return name, nil, fmt.Errorf("agent unavailable — no API key configured for %s", name)
	}
	return name, prov, nil
}
