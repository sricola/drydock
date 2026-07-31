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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"drydock/internal/audit"
	"drydock/internal/config"
	"drydock/internal/creds"
	"drydock/internal/depcache"
	"drydock/internal/egress"
	"drydock/internal/provider"
	"drydock/internal/remote"
	"drydock/internal/runner"
	"drydock/internal/stage"
	"drydock/internal/trustbrief"
)

// gitURLRef accepts any https://, git@, or ssh:// git URL. Local paths
// (no scheme, no `:` host) are still rejected because the staging clone
// would inherit a filesystem origin and adapters can't operate on it.
// The adapter (GitHub / GitLab / push-only) is selected separately by
// Task.Platform or hostname autodetect.
var gitURLRef = regexp.MustCompile(
	`^(?:https?://[A-Za-z0-9.-]+/|git@[A-Za-z0-9.-]+:|ssh://[A-Za-z0-9._-]+@[A-Za-z0-9.-]+/)[A-Za-z0-9._-]+/[A-Za-z0-9._-]+?(?:\.git)?/?$`,
)

// DefaultUncappedRequestCap bounds every task whose operator left
// task_max_requests unset, in every auth mode (F-02): api_key lanes with a USD
// budget, subscription lanes with none, and a priceless openai-compat lane
// alike. High enough for a real agentic task's many tool-use turns, low
// enough to stop a runaway loop from draining a subscription or, in api_key
// mode, running up spend between metered responses. Exported so cmd/brokerd
// (which mints the lease and decides the request cap) and writeBrief (which
// must report the cap actually enforced) share one source of truth instead of
// two constants that could drift apart.
const DefaultUncappedRequestCap = 1000

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
	// PlanOnly marks a plan-only run: the agent VM gets DRYDOCK_MODE=plan in
	// its env (the entrypoint keys off it) and is expected to produce a plan,
	// not a diff. Default false = normal implement-and-diff run.
	PlanOnly bool `json:"plan_only,omitempty"`
	// IssueURL records the issue the instruction was ingested from (CLI
	// --issue). Provenance only — the broker treats the instruction text as
	// the single source of truth either way.
	IssueURL string `json:"issue_url,omitempty"`
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
	// StageQuotaBytes, when > 0, hard-bounds each task's stage dir with an
	// APFS sparse image of this size (F-04). 0 = plain host dir.
	StageQuotaBytes int64
	AuditRoot       string
	Timeout         time.Duration
	// ApprovalTimeout, when > 0, auto-denies a task waiting at an approval gate
	// after this long and frees its concurrency slot. 0 = wait indefinitely.
	ApprovalTimeout time.Duration
	Network         string       // stable egress network name (e.g. drydock-egress)
	GatewayIP       string       // vmnet gateway IP the VM reaches (e.g. 192.168.64.1)
	ProxyPort       int          // squid port (e.g. 3128)
	Squid           SquidControl // per-task egress widening; nil = disabled
	TaskBudget      float64      // USD budget per task
	// MaxRequestCostUSD and TaskMaxRequests mirror the gateway's admission
	// policy (config max_request_cost_usd / task_max_requests). The broker
	// itself does not enforce them — the gateway does — but the Brief must
	// report the effective policy, in particular whether the USD budget was
	// a hard ceiling (reservation on) or the default post-hoc soft cap.
	MaxRequestCostUSD float64
	TaskMaxRequests   int
	DefaultModel      string // operator-level default; per-task Task.Model overrides
	// OpenAICompatModel is the model id for the openai-compat lane (from
	// config openai_compat.model). It's the per-task default for an opencode
	// task when --model isn't passed, since that vendor has no built-in model.
	OpenAICompatModel string
	Notify            bool   // fire macOS notifications on approval gates (config notifications)
	AnthropicAuth     string // "api_key" | "subscription"; recorded per task for `drydock tasks`
	OpenAIAuth        string // "api_key" | "subscription"; recorded per task for `drydock tasks`

	// Verify maps canonical "host/owner/repo" repo keys (repokey.Normalize)
	// to that repository's verification recipe (config verify.repos, mirrored
	// by cmd/brokerd). nil/empty = verifier off; a task whose repo has no
	// entry records verification status "not_configured" and pushes as before.
	Verify map[string]VerifyRepo

	// Setup maps canonical "host/owner/repo" repo keys (repokey.Normalize) to
	// that repository's execution profile (config profiles.repos, mirrored by
	// cmd/brokerd). nil/empty = setup off; a task whose repo has no entry
	// records setup status "not_configured" and runs as before. Unlike Verify
	// there is no advisory mode: a configured profile that does not pass
	// fails the task closed BEFORE the agent VM boots and before any bearer
	// is injected into any VM (fail-closed-before-spend; see runSetup).
	Setup map[string]SetupProfile

	// CacheRoot/CacheQuotaBytes wire the persistent dependency cache (config
	// cache_root/cache_quota_gb, mirrored by cmd/brokerd). The cache is
	// active only when BOTH are set (root non-empty, quota > 0) AND the
	// task's SetupProfile opts in with Cache: true. See cache.go for the
	// resolve/refcount/evict machinery and its serialization invariant.
	CacheRoot       string
	CacheQuotaBytes int64

	// cacheMu serializes EVERY cache-dir resolution (resolveCache) and every
	// eviction sweep (sweepCache) against each other — Evict can RemoveAll
	// any entry, so it must be exclusive against all cache use. cacheInUse
	// refcounts entry dirs currently mounted by live tasks (resolve
	// increments, task completion decrements); a sweep never removes an
	// in-use dir. cacheStore is built lazily under cacheMu from
	// CacheRoot/CacheQuotaBytes; nil means the cache is off.
	cacheMu    sync.Mutex
	cacheInUse map[string]int
	cacheStore *depcache.Store

	// DiffPolicy caps the size and shape of the diff a task may propose
	// (config diff_policy, wired from cfg.DiffPolicy by cmd/brokerd). The zero
	// value disables every cap. A violating diff fails closed as the terminal
	// outcome "policy_blocked" BEFORE the approval gate — auto_approve does
	// not bypass it. See taskRun.checkDiffCaps (diffpolicy.go).
	DiffPolicy config.DiffPolicy

	// UnmeteredVendors names vendors whose lane carries NO USD metering at all
	// (subscription auth lanes; a priceless openai-compat lane) — the same
	// conditions cmd/brokerd used to mint a math.MaxFloat64 lease budget for
	// that vendor. The Brief must say so honestly instead of reporting
	// TaskBudget/MaxRequestCostUSD as if they bounded the run.
	UnmeteredVendors map[string]bool

	PushMaxRetries       int
	PushRetryBackoff     time.Duration
	PushFreshBranchTries int

	// PolicyFields/PolicyHash are the daemon's effective policy as resolved by
	// config.Explain at boot, stashed here so GET /admin/policy can report what
	// brokerd actually loaded — read-only, no recomputation on request. Set
	// once by cmd/brokerd after cfg is resolved; nil/empty on a Broker built
	// directly by tests that don't care about the policy endpoint.
	// PolicyFields is the full provenance table; PolicyHash is computed over
	// config.PolicyComparisonFields(PolicyFields) — connection fields excluded
	// — so it lines up with the hash `drydock policy explain` computes locally.
	PolicyFields []config.Field
	PolicyHash   string

	// AggregateExceeded, when set, is consulted at task submission: if it
	// returns true for the task's vendor, the submission is rejected (402)
	// before the stream starts and before any lease is minted. nil disables
	// the pre-check. Wired to the gateway's AggregateExceeded by brokerd.
	AggregateExceeded func(vendor string) bool

	// Test seams. nil in production -> the real implementations
	// (defaultPrepareStage / runContainer). White-box tests inject fakes to
	// drive HandleTask without a git clone or a container run.
	prepareStage func(ctx context.Context, root, repoRef string) (taskStage, error)
	runAgent     func(ctx context.Context, args []string, stdout, stderr io.Writer) error
	// attachQuota mounts the hard per-task disk bound at a stage root. nil
	// in production falls back to stage.AttachQuota (a no-op off macOS).
	// Tests inject fakes.
	attachQuota func(root string, sizeBytes int64) error
	// watchStage seams the stage-size guard so a test can capture the host
	// root the free-floor is measured on. nil in production -> watchStageSize.
	watchStage func(root, hostRoot string, interval time.Duration, onExceed func()) *stageSizeGuard
	// newAdapter selects the remote PR/MR adapter. nil in production ->
	// remote.AdapterFor. White-box tests inject a fake to drive the
	// best-effort PR-open path without shelling out to gh/glab/tea.
	newAdapter func(repoRef, platform string) remote.Adapter
	// reopenStage reopens an existing stage by its root path. nil in production
	// falls back to defaultReopenStage (wraps stage.Reopen). Tests inject a fake
	// to drive ResumeAwaiting without a real git directory on disk.
	reopenStage func(root string) (taskStage, error)
	// deleteContainer seams the bounded best-effort force-delete of a task's
	// VM (task-<id> or verify-<id>). nil in production -> forceDeleteContainer.
	// Tests inject a fake to observe which containers get deleted.
	deleteContainer func(name string) error

	// MaxConcurrent caps how many tasks may be in any non-terminal state at
	// once. Excess POSTs to /tasks return 503. Default (when zero) is 2.
	MaxConcurrent int

	// slots is a bounded semaphore guarding MaxConcurrent. Initialized lazily
	// the first time HandleTask is called (so existing callers that build a
	// Broker by struct literal keep working).
	slotsOnce sync.Once
	slots     chan struct{}

	pendingMu sync.Mutex
	pending   map[string]chan gateReply // task_id -> approval channel
	// requiredAcks maps a task waiting at the diff-approval gate to the
	// second-look categories an approve must acknowledge. Registered by
	// awaitGate atomically with the b.pending entry (same lock hold) and
	// removed with it, so there is never a window where a task is approvable
	// but its ack requirement is invisible to signal. Guarded by pendingMu.
	requiredAcks map[string][]string
	tasks        map[string]*TaskState              // task_id -> live state (running + awaiting_approval)
	cancellers   map[string]context.CancelCauseFunc // task_id -> cancel hook for in-flight kill
}

// taskStage is the subset of *stage.Stage that HandleTask uses. It exists so
// white-box tests can drive the handler without a real git clone; production
// uses realStage, a thin adapter over *stage.Stage.
type taskStage interface {
	WorkDir() string
	WriteTaskFiles(prompt string) error
	CaptureDiff() (string, error)
	ReadPlan() (string, bool)
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
func (r realStage) ReadPlan() (string, bool)              { return r.s.ReadPlan() }
func (r realStage) Commit(branch, msg string) error       { return r.s.Commit(branch, msg) }
func (r realStage) PushBranch(local, remote string) error { return r.s.PushBranch(local, remote) }
func (r realStage) PushEnv() []string                     { return r.s.PushEnv() }
func (r realStage) Cleanup() error                        { return r.s.Cleanup() }
func (r realStage) BaseCommit() (string, error)           { return r.s.BaseCommit() }

// StagedTreeHash/ExportStaged surface the stage's sealed-export capability
// (the verifier's stagedExporter optional interface) on the production stage.
func (r realStage) StagedTreeHash() (string, error)         { return r.s.StagedTreeHash() }
func (r realStage) ExportStaged(dst string) (string, error) { return r.s.ExportStaged(dst) }

// WithContext binds the task context to the stage's git subprocesses. Used on
// the resume path (the stage is reopened before the per-task context exists).
func (r realStage) WithContext(ctx context.Context) { r.s.WithContext(ctx) }

// defaultPrepareStage is the production prepareStage: a real host clone with
// the .git dir moved out of the mounted work tree. ctx cancels the clone.
func defaultPrepareStage(ctx context.Context, root, repoRef string) (taskStage, error) {
	s, err := stage.Prepare(ctx, root, repoRef)
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
	// On ctx cancel (operator kill / task timeout) CommandContext kills the
	// container process, but cmd.Run would otherwise block until the stdout and
	// stderr pipes close. If the CLI leaves a helper holding them, WaitDelay
	// force-closes the pipes so Run returns shortly after the kill instead of
	// pinning the task goroutine (and its concurrency slot) indefinitely.
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

// vmDeleteTimeout bounds the best-effort force-delete of a task's VM on
// kill/timeout. It uses a FRESH context (not the task's, which is already
// cancelled by the kill) so the delete still runs, but bounded so a wedged
// container daemon can't pin the task goroutine and its concurrency slot here.
// A VM that outlives this is reaped at the next brokerd boot anyway.
const vmDeleteTimeout = 30 * time.Second

func forceDeleteContainer(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), vmDeleteTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "container", "delete", "--force", name)
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

// forceDelete routes through the deleteContainer seam (tests) or the real
// bounded force-delete. Shared by the agent-run (task-<id>) and verifier
// (verify-<id>) teardown paths.
func (b *Broker) forceDelete(name string) error {
	if b.deleteContainer != nil {
		return b.deleteContainer(name)
	}
	return forceDeleteContainer(name)
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
	sensitive   bool
	draft       bool
	platform    string
	model       string
	planOnly    bool
	issueURL    string

	// Filled in as HandleTask advances through the lifecycle.
	proxyAuth  string      // "<user>:<secret>@" widening userinfo (empty if none)
	st         taskStage   // prepared host stage
	grant      creds.Grant // minted ephemeral credential
	agentName  string      // resolved agent ("claude"|"codex"|...)
	taskVendor string      // vendor for the resolved agent
	logf       io.Writer   // audit log writer
	auditPath  string      // path to the audit .jsonl
	taskStart  time.Time   // set by runSandbox when the agent starts

	// verify is the broker-observed verification evidence, set by runVerify on
	// every live path that produced a diff (status "not_configured" when the
	// repo has no verify config). nil before runVerify and on the resume path,
	// where verification did not run in this process.
	verify    *trustbrief.Verification
	verifyDur time.Duration // wall-clock of the verifying stage (metrics)

	// setup is the broker-observed setup evidence, set by runSetup on every
	// live path (status "not_configured" when the repo has no execution
	// profile). nil before runSetup and on the resume path, where setup did
	// not run in this process.
	setup    *trustbrief.SetupEvidence
	setupDur time.Duration // wall-clock of the setting_up stage (metrics)
	// Dependency-cache participation, set by resolveCache (cache.go) before
	// the first setup VM boots. cacheDir == "" means uncached; when set, the
	// SAME dir is mounted rw into the setup VMs and READ-ONLY into the agent
	// VM, and its in-use refcount is held until releaseCache (deferred in
	// HandleTask) drops it at task completion.
	cacheDir    string
	cacheKey    string // full 64-hex cache key (Brief records a 12-hex prefix)
	cacheHit    bool
	cacheStatus string // "" (off) | trustbrief.CacheHit/CacheMiss/CacheDisabledNoLockfile

	// setupStart is when runSetup began its first command — the moment the
	// preparing stage ended for a task with an execution profile. Zero when no
	// setup ran. appendMetrics uses it to end StageMs.Preparing at setup
	// start; without it preparing would span prep+setup (runSandbox only
	// overwrites taskStart when the agent starts), double-counting setup.
	setupStart time.Time

	// Metrics capture (observability 4.7): filled as the lifecycle advances,
	// written once by the deferred appendMetrics.
	prepStart        time.Time     // set at the "preparing" stage emit
	runEnd           time.Time     // set when the agent run returns
	egressGateWait   time.Duration // wall-clock at the egress-widen gate
	approvalGateWait time.Duration // wall-clock at the diff-approval gate
	pushDur          time.Duration // finishPush wall-clock
	subscription     bool          // this task's lane is unmetered
	widenOutcome     string        // "" (none) | "approved"; denied dies pre-audit
	diffFiles        int
	diffBytes        int64
	// outcome is the terminal path taken, written into the metrics row's
	// Outcome field: "pushed"|"denied"|"cancelled"|"push_failed"|"error"|
	// "no_diff"|"setup_failed"|"verify_failed"|"policy_blocked"|"planned". Set at each terminal return; "" (never reached a terminal
	// return with the audit log open, e.g. resumePush's shutdown re-defer)
	// leaves the metrics row's Outcome empty, same as a pre-outcome-field row.
	outcome string

	// requiredAcks is the second-look acknowledgment categories the approver
	// must ack for this diff (diff_policy.second_look_paths x the diff's
	// trustbrief flags). Computed at gate entry — pushAndOpenPR on the live
	// path, resumePush (from the persisted diff) on the resume path — and
	// enforced by signal (admin.go) before any approve reaches the gate.
	requiredAcks []string

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
		sensitive:   t.Sensitive,
		draft:       t.Draft,
		platform:    t.Platform,
		model:       t.Model,
		planOnly:    t.PlanOnly,
		issueURL:    t.IssueURL,
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
	tr.prepStart = time.Now()
	sw.emit(map[string]any{"event": "stage", "stage": "preparing", "task_id": taskID})

	if b.StageQuotaBytes > 0 {
		attach := b.attachQuota
		if attach == nil {
			attach = stage.AttachQuota
		}
		qerr := os.MkdirAll(stageDir, 0o700)
		if qerr == nil {
			qerr = attach(stageDir, b.StageQuotaBytes)
		}
		if qerr != nil {
			// Fail closed: never fall back to an unbounded plain dir when
			// the operator configured a hard bound (F-04).
			slog.Warn("task stage quota failed", "task_id", taskID, "err", qerr)
			sw.emit(errorEvent(taskID, "stage quota setup failed",
				"hdiutil could not create or mount the stage image; see the broker log"))
			_ = (&stage.Stage{Root: stageDir}).Cleanup()
			return
		}
		// Until tr.st exists, no defer tears the mount down; cover the
		// clone/prompt-write failure exits in between.
		defer func() {
			if tr.st == nil {
				_ = (&stage.Stage{Root: stageDir}).Cleanup()
			}
		}()
	}

	st, err := prepare(tr.ctx, stageDir, t.RepoRef)
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

	// Write-auth preflight (see the 2026-07-27 push-preflight spec): prove
	// push credentials against the actual remote NOW, before task files,
	// credential mint, or any VM spend. Fail-closed with the same
	// classification the real push uses. Optional capability so test fakes
	// without it are unaffected (the BaseCommit idiom).
	if pf, ok := tr.st.(interface{ PushPreflight(string) error }); ok {
		if err := pf.PushPreflight("agent/" + taskID); err != nil {
			class := classifyPushError(err.Error())
			slog.Warn("task push preflight failed", "task_id", taskID, "class", string(class), "err", err)
			hint := ""
			if class == reasonAuth {
				hint = "no working push credential for this remote; run `gh auth setup-git` (https) or check your SSH key/agent"
			}
			sw.emit(errorEvent(taskID,
				"push preflight failed ("+string(class)+"): "+gitOutputFirstLine(err), hint))
			return
		}
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
	// Terminal metrics row (observability): registered after the Sync/Close
	// defers so it runs before them, on every exit path from here on.
	defer tr.appendMetrics()
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
	tr.subscription = subscription
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
		"plan_only": t.PlanOnly, "issue_url": t.IssueURL,
	}); err == nil {
		fmt.Fprintf(logf, "%s\n", inv)
	}

	// Host-config setup phase (execution profiles): run the repo's
	// host-approved setup/readiness commands in bearer-free VMs against the
	// live stage BEFORE the agent VM boots. Fail-closed-before-spend: on any
	// setup failure runSetup emits the terminal event and we return HERE —
	// the grant minted above was never injected into any VM (buildSetupEnv
	// carries no credential; runner.BuildRunArgs below is never reached), so
	// a broken workspace costs $0 in API spend. The deferred grant.Revoke
	// registered at mint time still fires on this return.
	//
	// The cache release is deferred HERE — not inside runSetup — because the
	// entry runSetup's resolveCache refcounts stays bind-mounted through the
	// agent run below; only at task completion may its in-use count drop
	// (and the post-completion eviction sweep run). No-op for uncached tasks.
	defer tr.releaseCache()
	if !tr.runSetup() {
		return
	}

	// Write the operator's prompt AFTER setup, immediately before the agent VM
	// boots. Setup VMs run untrusted host-config-invoked code (npm postinstall,
	// pip setup.py, build hooks) with the live /work mounted rw — writing the
	// prompt earlier would let a malicious dependency rewrite
	// /work/.task/prompt.txt during setup while the trust brief's
	// InstructionSHA256 (computed host-side from the original instruction)
	// still attested the untampered text, masking the swap. Writing last means
	// the operator's prompt always wins: WriteTaskFiles O_TRUNC-overwrites any
	// regular prompt.txt a setup command pre-created, and refuses (fail-closed
	// here, as stage failed) if setup replaced .task or prompt.txt with a
	// symlink.
	if err := tr.st.WriteTaskFiles(t.Instruction); err != nil {
		slog.Warn("task stage failed", "task_id", taskID, "err", err)
		tr.outcome = "error"
		sw.emit(errorEvent(taskID, "stage failed", ""))
		return
	}

	args := runner.BuildRunArgs(runner.Spec{
		TaskID:     taskID,
		Network:    b.Network,
		ImageRef:   b.ImageRef,
		Env:        tr.buildEnv(),
		StageDir:   st.WorkDir(),
		PromptFile: "/work/.task/prompt.txt",
		// The SAME cache entry setup just populated, mounted READ-ONLY at
		// /deps (BuildRunArgs adds the readonly flag): the agent reads the
		// warm cache but can never write it — agent-run code must not be
		// able to poison dependencies consumed by other tasks. Empty (no
		// mount) when this task runs uncached.
		CacheDir: tr.cacheDir,
		MemoryGB: 4,
		CPUs:     4,
	})

	if err := tr.runSandbox(args); err != nil {
		return // runSandbox already emitted the terminal event
	}

	diff, err := st.CaptureDiff()
	if err != nil {
		reason := "diff capture failed"
		if errors.Is(err, stage.ErrDiffTooLarge) {
			reason = fmt.Sprintf("task failed closed: staged diff exceeds the %d MiB review cap, so it cannot be fully reviewed (V-01)", stage.MaxDiffBytes>>20)
		}
		tr.outcome = "error"
		sw.emit(errorEvent(taskID, reason, ""))
		return
	}
	// Plan-mode scope gate: a PlanOnly task terminates HERE, before the
	// no_diff/verify/push logic even runs. The never-push guarantee is
	// structural — this early return is the only continuation for a planOnly
	// task, so runVerify and pushAndOpenPR are unreachable regardless of what
	// the agent left in the work tree (empty diff, non-empty diff, hostile
	// diff — all terminate "planned").
	if tr.planOnly {
		tr.finishPlanned(diff)
		return
	}
	if diff == "" {
		tr.outcome = "no_diff"
		sw.emit(map[string]any{"event": "result", "outcome": "no_diff",
			"task_id": taskID, "duration_ms": time.Since(tr.taskStart).Milliseconds(),
			"cost_usd": audit.TotalCost(tr.auditPath)})
		return
	}

	// Persist the review diff as soon as it exists — before verification and
	// the approval gate — so every outcome from here on (verify_failed and a
	// mid-verify cancel included) leaves the `.diff` evidence beside the audit
	// log, even when the task never reaches the gate.
	b.persistDiff(taskID, diff)

	if !tr.runVerify(diff) {
		return // runVerify emitted the terminal event
	}
	tr.pushAndOpenPR(diff)
}

// finishPlanned is the plan-mode terminal: it captures the agent's plan
// (best-effort), persists the run's evidence, and ends the task with outcome
// "planned". It deliberately mirrors the verify_failed terminal idiom — brief
// before terminal event, broker-authored result row last — and it never
// verifies and never pushes: HandleTask returns immediately after this call,
// so for a planOnly task pushAndOpenPR/runVerify are structurally
// unreachable (THE plan-mode invariant), whatever the diff contains.
func (tr *taskRun) finishPlanned(diff string) {
	b := tr.b
	plan, ok := tr.st.ReadPlan()
	if ok {
		b.persistPlan(tr.id, plan)
	}
	if diff != "" {
		// A planning agent shouldn't touch the tree, but if it did, leave the
		// diff beside the audit log like every other evidence-bearing outcome
		// — the operator reviewing the plan should see what else changed.
		b.persistDiff(tr.id, diff)
	}
	// The brief is written BEFORE the terminal event (the hint below points at
	// `drydock inspect`, which reads it), recording PlanOnly/IssueURL so the
	// plan run's provenance survives next to the plan artifact.
	b.writeBrief(tr, diff)
	// Synthetic audit result row mirrors runVerify's verify_failed pattern
	// (last-wins over the agent's own success row, carrying metered cost).
	cost := audit.TotalCost(tr.auditPath)
	fmt.Fprintf(tr.logf,
		`{"type":"result","subtype":"planned","is_error":false,"duration_ms":%d,"total_cost_usd":%.6f,"num_turns":0,"src":"broker"}`+"\n",
		time.Since(tr.taskStart).Milliseconds(), cost)
	tr.outcome = "planned"
	tr.sw.emit(map[string]any{"event": "result", "outcome": "planned",
		"task_id": tr.id, "plan_bytes": len(plan), "has_plan": ok,
		"duration_ms": time.Since(tr.taskStart).Milliseconds(), "cost_usd": cost,
		"hint": "drydock inspect " + tr.id + " — review the plan, then run without --plan to implement"})
}

// runEgressGate handles the awaiting_egress stage when the task requests extra
// egress and requires_approval is set. Returns true to continue, false to abort
// (the appropriate terminal event has already been emitted).
func (tr *taskRun) runEgressGate() bool {
	b, extras := tr.b, tr.egressExtra
	if len(extras) == 0 || !b.Cfg.WideningRequiresApproval() {
		if len(extras) > 0 {
			// Widening required but no gate configured: extras are applied
			// without a human gate, so the outcome is still "approved".
			tr.widenOutcome = "approved"
		}
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
	gateStart := time.Now()
	ok := b.gateEgressWiden(tr.ctx, tr.id, extras)
	tr.egressGateWait = time.Since(gateStart)
	b.setEgressExtra(tr.id, nil)
	if !ok {
		if tr.ctx.Err() != nil {
			tr.sw.emit(map[string]any{"event": "result", "outcome": "cancelled", "task_id": tr.id})
			return false
		}
		tr.sw.emit(errorEvent(tr.id, "egress widening denied", ""))
		return false
	}
	tr.widenOutcome = "approved"
	b.setStage(tr.id, StageRunning)
	return true
}

// buildSetupEnv assembles the env for a setup VM: the egress-proxy vars and
// the gateway IP, and NOTHING else — never a grant bearer or any credential
// material. The setup VM runs pre-review repo code (npm postinstall, pip
// setup.py, arbitrary build hooks) with network egress, so it must have no
// credential to leak or spend; this is one half of fail-closed-before-spend
// (the other is runSetup's placement before the agent VM boots).
// buildTaskEnv builds on it so the agent and setup VMs share one proxy
// config. Pure (all inputs explicit) so it unit-tests without a Broker.
func buildSetupEnv(proxyAuth, gatewayIP string, proxyPort int) []string {
	return []string{
		fmt.Sprintf("HTTPS_PROXY=http://%s%s:%d", proxyAuth, gatewayIP, proxyPort),
		fmt.Sprintf("HTTP_PROXY=http://%s%s:%d", proxyAuth, gatewayIP, proxyPort),
		// Bypass squid for the credential gateway itself — squid's allowlist
		// is hostname-based and would deny a CONNECT to the gateway IP. (The
		// setup VM's firewall pin drops :8088 anyway; the agent VM needs it.)
		"NO_PROXY=127.0.0.1,localhost," + gatewayIP,
		"DRYDOCK_GW_IP=" + gatewayIP,
	}
}

// buildTaskEnv assembles the env slice passed to the agent container. It is
// pure (all inputs explicit) so it can be unit-tested without a Broker.
func buildTaskEnv(grantEnv []string, proxyAuth, gatewayIP string, proxyPort int,
	agentName, taskModel, openAICompatModel, operatorDefaultModel, taskVendor string,
	planOnly, cacheActive bool) []string {
	env := append([]string{}, grantEnv...)
	env = append(env, buildSetupEnv(proxyAuth, gatewayIP, proxyPort)...)
	defaultModel := effectiveDefaultModel(operatorDefaultModel, taskVendor)
	env = append(env, modelEnv(taskModelFor(taskModel, openAICompatModel, taskVendor), defaultModel)...)
	env = append(env, "DRYDOCK_AGENT="+agentName)
	if planOnly {
		// The entrypoint switches the agent into plan mode off this var
		// (consumed there; absent entirely on a normal run).
		env = append(env, "DRYDOCK_MODE=plan")
	}
	if cacheActive {
		// Point the package managers at the read-only /deps mount so the
		// agent's installs reuse the warm cache setup populated. Absent
		// entirely on an uncached run (no mount to point at).
		env = append(env, runner.CacheEnv()...)
	}
	return env
}

// buildEnv assembles the container env from the taskRun's fields. It exists only
// to collapse the call-site noise — the actual assembly stays in the pure,
// unit-tested buildTaskEnv free function.
func (tr *taskRun) buildEnv() []string {
	return buildTaskEnv(tr.grant.EnvVars(), tr.proxyAuth, tr.b.GatewayIP, tr.b.ProxyPort,
		tr.agentName, tr.model, tr.b.OpenAICompatModel, tr.b.DefaultModel, tr.taskVendor,
		tr.planOnly, tr.cacheDir != "")
}

// appendBrokerResult writes the broker-authored terminal result for this task.
// It must be the LAST result line in the audit stream on EVERY exit path:
// last-wins parsing plus the src:"broker" seed filter make it the only record
// the cost display and the restart-seeded aggregate ledger trust. A path that
// skips it (or writes total_cost_usd:0) erases that task's gateway-metered
// spend from the rolling cap after a brokerd restart (F-07). The agent's own
// stdout is an untrusted source (a compromised CLI can forge a cheap
// total_cost_usd), so this record is written from the broker's own metering,
// after the agent's output ends, making it the last result line.
func (tr *taskRun) appendBrokerResult(isError bool) {
	subtype := "success"
	if isError {
		subtype = "error"
	}
	turns := 0
	if rc, ok := tr.grant.(interface{ Requests() int }); ok {
		turns = rc.Requests()
	}
	_, _ = fmt.Fprintf(tr.logf,
		`{"type":"result","subtype":"%s","is_error":%t,"duration_ms":%d,"total_cost_usd":%.6f,"num_turns":%d,"src":"broker"}`+"\n",
		subtype, isError, time.Since(tr.taskStart).Milliseconds(), tr.grant.Spent(), turns)
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
	// The free floor must be measured on the HOST filesystem. stageRoot is
	// the work dir inside the (possibly quota-image-backed) stage, and its
	// parent is the image mountpoint, so neither works: b.StageRoot is the
	// one path that stays on the host no matter the stage layout (F-04).
	watch := b.watchStage
	if watch == nil {
		watch = watchStageSize
	}
	sizeGuard := watch(stageRoot, b.StageRoot, stageSizeInterval, runCancel)
	defer sizeGuard.stop()
	err := run(runCtx, args, outCap.wrap(io.MultiWriter(tr.logf, os.Stdout)), outCap.wrap(tr.logf))
	tr.runEnd = time.Now()
	if err != nil {
		// --rm covers a graceful exit; on timeout/kill the VM may survive,
		// so force-remove it (best effort) to honor the ephemeral-VM backstop.
		if derr := b.forceDelete("task-" + tr.id); derr != nil {
			slog.Warn("force-delete of task VM failed; reaped at next brokerd boot",
				"task_id", tr.id, "err", derr)
		}
		if tr.ctx.Err() != nil {
			// Operator killed it, or the client went away. Be explicit.
			tr.appendBrokerResult(true)
			tr.outcome = "cancelled"
			tr.sw.emit(map[string]any{"event": "result", "outcome": "cancelled", "task_id": tr.id})
			return errTaskTerminated
		}
		if outCap.exceeded() {
			// We cancelled the task ourselves: its output crossed the host cap.
			tr.appendBrokerResult(true)
			tr.outcome = "error"
			tr.sw.emit(map[string]any{"event": "error", "task_id": tr.id,
				"audit":       tr.auditPath,
				"duration_ms": time.Since(tr.taskStart).Milliseconds(),
				"reason":      fmt.Sprintf("task terminated: output exceeded the %d MiB host cap", maxTaskOutputBytes>>20)})
			return errTaskTerminated
		}
		if sizeGuard.exceeded() {
			// We cancelled the task ourselves: its /work grew past the host disk
			// cap, or host free space dropped below the floor.
			tr.appendBrokerResult(true)
			tr.outcome = "error"
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
		tr.appendBrokerResult(true)
		tr.outcome = "error"
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

	// Append a broker-authored terminal result for EVERY agent (Claude also
	// emits its own result line for its turn count; ours wins via last-wins
	// parsing). See appendBrokerResult for why this record, not the agent's
	// own, is the one the cost display and the aggregate ledger trust.
	tr.appendBrokerResult(false)
	return nil
}

// writeBrief assembles and persists the broker-observed evidence report at
// the diff-approval gate, for every task that produced a diff — including
// auto-approved ones, where the Brief is the only pre-push record a human
// can later audit. Best-effort: the Brief is advisory evidence in v1, so a
// write failure warns and the gate proceeds rather than failing the task.
// It returns the DiffFacts it computed so the diff-policy caps check (and
// later consumers) reuse the one Analyze pass instead of re-parsing the diff.
func (b *Broker) writeBrief(tr *taskRun, diff string) trustbrief.DiffFacts {
	policy := trustbrief.PolicyFacts{
		BudgetUSD:      b.TaskBudget,
		BudgetHard:     b.MaxRequestCostUSD > 0,
		MaxRequests:    b.TaskMaxRequests,
		TimeoutSeconds: int(b.Timeout.Seconds()),
		EgressDefault:  domainStrings(b.Cfg.Default.Domains),
		EgressWidened:  domainStrings(tr.egressExtra),
	}
	if policy.MaxRequests <= 0 {
		policy.MaxRequests = DefaultUncappedRequestCap
	}
	if b.UnmeteredVendors[tr.taskVendor] {
		// This lane mints leases at math.MaxFloat64: TaskBudget/BudgetHard
		// describe a cap that was never actually applied. Report the honest
		// state: no USD metering, and the request-count backstop that is the
		// real enforced bound in every mode (F-02).
		policy.BudgetUnbounded = true
		policy.BudgetUSD = 0
		policy.BudgetHard = false
	}
	policy.SnapshotSHA256 = policy.Fingerprint()

	// Verification evidence comes straight from runVerify's broker-observed
	// block. tr.verify is nil only on paths that never ran the verifier in
	// this process (direct writeBrief unit calls; the resume path) — those
	// honestly read as not_configured.
	verification := trustbrief.Verification{Status: trustbrief.VerificationNotConfigured}
	if tr.verify != nil {
		verification = *tr.verify
	}
	// Setup evidence likewise comes straight from runSetup's broker-observed
	// block; nil (direct writeBrief unit calls; the resume path) honestly
	// reads as not_configured — which needs no MissingEvidence noise, since
	// a repo without an execution profile is the normal state.
	setupEv := trustbrief.SetupEvidence{Status: trustbrief.SetupNotConfigured}
	if tr.setup != nil {
		setupEv = *tr.setup
	}
	var missing []string
	switch verification.Status {
	case trustbrief.VerificationNotConfigured:
		missing = append(missing, "verification not configured for this repository (no verify.repos entry)")
	case trustbrief.VerificationInconclusive:
		missing = append(missing, "verification inconclusive — treat as unverified")
	}
	if setupEv.Status == trustbrief.SetupInconclusive {
		missing = append(missing, "setup inconclusive — the workspace may not have been prepared")
	}
	missing = append(missing, "agent summary not captured (broker records no agent claims in v1)")

	brief := trustbrief.Brief{
		SchemaVersion: 1,
		TaskID:        tr.id,
		GeneratedAt:   time.Now().UTC(),
		Task: trustbrief.TaskFacts{
			InstructionSHA256: trustbrief.HashInstruction(tr.instruction),
			RepoRef:           trustbrief.RedactRepoRef(tr.repoRef),
			Sensitive:         tr.sensitive,
			AutoApprove:       tr.autoApprove,
			PlanOnly:          tr.planOnly,
			IssueURL:          tr.issueURL,
		},
		Runtime: trustbrief.RuntimeFacts{
			ImageRef: b.ImageRef, Agent: tr.agentName, Vendor: tr.taskVendor, Model: tr.model,
		},
		Policy: policy,
		Spend: trustbrief.SpendFacts{
			USDBrokerMetered: tr.grant.Spent(),
			DurationMs:       time.Since(tr.taskStart).Milliseconds(),
		},
		Diff:            trustbrief.Analyze(diff),
		Verification:    verification,
		Setup:           setupEv,
		MissingEvidence: missing,
	}
	// Optional capability: the production stage knows its clone commit; test
	// fakes may not. Absence is recorded, never silently dropped.
	if bc, ok := tr.st.(interface{ BaseCommit() (string, error) }); ok {
		if c, err := bc.BaseCommit(); err == nil {
			brief.Task.BaseCommit = c
		}
	}
	if brief.Task.BaseCommit == "" {
		brief.MissingEvidence = append(brief.MissingEvidence, "base_commit unavailable")
	}
	if brief.Diff.Truncated {
		brief.MissingEvidence = append(brief.MissingEvidence,
			"review diff truncated at its cap; file counts cover the truncated portion only")
	}
	if err := trustbrief.Write(b.AuditRoot, tr.id, brief); err != nil {
		slog.Warn("could not persist trust brief", "task_id", tr.id, "err", err)
	}
	return brief.Diff
}

// domainStrings renders egress domains as "host:p1,p2" strings for the Brief.
func domainStrings(ds []egress.Domain) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		ports := make([]string, 0, len(d.Ports))
		for _, p := range d.Ports {
			ports = append(ports, strconv.Itoa(p))
		}
		out = append(out, d.Host+":"+strings.Join(ports, ","))
	}
	return out
}

// pushAndOpenPR handles the diff-approval gate, branch push, and PR creation.
// It always emits a terminal event
// (result/outcome=policy_blocked|denied|cancelled|pushed|push_failed).
// HandleTask should return immediately after calling this; it is the last step
// in the task lifecycle.
func (tr *taskRun) pushAndOpenPR(diff string) {
	b := tr.b
	files, insertions, deletions := diffStat(diff)
	tr.diffFiles = files
	tr.diffBytes = int64(len(diff))
	facts := b.writeBrief(tr, diff)
	// Diff-policy caps are ENFORCEMENT, applied before any gate — including
	// the auto-approve branch below, which must never bypass them. A blocked
	// task fails closed: nothing is pushed and it never registers as pending.
	// The Brief and the .diff are already persisted, so `drydock inspect`
	// shows the operator exactly what was blocked. The synthetic audit result
	// row mirrors runVerify's verify_failed pattern (last-wins over the
	// agent's own success row, carrying metered cost).
	if blocked, reason := tr.checkDiffCaps(facts, diff); blocked {
		cost := audit.TotalCost(tr.auditPath)
		fmt.Fprintf(tr.logf,
			`{"type":"result","subtype":"policy_blocked","is_error":false,"duration_ms":%d,"total_cost_usd":%.6f,"num_turns":0,"src":"broker"}`+"\n",
			time.Since(tr.taskStart).Milliseconds(), cost)
		tr.outcome = "policy_blocked"
		tr.sw.emit(map[string]any{"event": "result", "outcome": "policy_blocked",
			"task_id": tr.id, "reason": reason,
			"duration_ms": time.Since(tr.taskStart).Milliseconds(), "cost_usd": cost,
			"hint": "drydock inspect " + tr.id + " — a diff-policy cap blocked this task before review"})
		return
	}
	// Second-look acknowledgments (diff_policy.second_look_paths): computed
	// here at gate entry from the same DiffFacts the caps check used, so what
	// the approver must acknowledge is exactly what the Brief reports. The
	// requirement is registered with the pending channel inside awaitGate and
	// enforced by signal — an approve missing any of these categories is
	// refused with 422 and the task stays pending. Auto-approve is unaffected:
	// second-look is a human-gate aid, and the hard enforcement that even
	// auto-approve cannot bypass is checkDiffCaps above.
	tr.requiredAcks = requiredAcks(facts, b.DiffPolicy)
	b.setStage(tr.id, StagePending)
	b.setSecondLook(tr.id, tr.requiredAcks)
	// Only announce the approval gate when there's actually a human gate to
	// wait on. Auto-approve pushes immediately, so an "awaiting_approval"
	// stage would be a misleading blip in the stream.
	if !tr.autoApprove {
		gateEv := map[string]any{"event": "stage", "stage": "awaiting_approval",
			"task_id": tr.id, "diff_bytes": len(diff), "files": files,
			"approve": "drydock approve " + tr.id,
			"deny":    "drydock deny " + tr.id,
			"review":  "drydock review " + tr.id}
		if len(tr.requiredAcks) > 0 {
			gateEv["second_look"] = tr.requiredAcks
		}
		// Surface the verifier's broker-observed verdict to the reviewer at
		// the gate (advisory verification's whole value lives here).
		if tr.verify != nil && tr.verify.Status != trustbrief.VerificationNotConfigured {
			gateEv["verify"] = tr.verify.Status
		}
		tr.sw.emit(gateEv)
	}
	gateStart := time.Now()
	approved, cause := b.gatePushMarked(tr.ctx, tr, diff)
	if !tr.autoApprove {
		tr.approvalGateWait = time.Since(gateStart)
	}
	// Gate resolved (either way): the second-look ask is no longer pending,
	// mirroring how setEgressExtra clears after the egress gate.
	b.setSecondLook(tr.id, nil)
	if !approved {
		// See gateOutcome's doc comment for how this maps and where the live
		// and resumed (reconcile.go's resumePush) paths intentionally diverge.
		outcome, _ := gateOutcome(cause, false)
		if cause == gateShutdown {
			tr.keepStage = true
		}
		tr.outcome = outcome
		tr.sw.emit(map[string]any{"event": "result", "outcome": outcome,
			"task_id": tr.id, "diff_bytes": len(diff)})
		return
	}

	tr.finishPush(files, insertions, deletions)
}

// finishPush performs the branch push (with recovery) and PR-open after the
// approval gate has passed, emitting the terminal pushed/push_failed event and,
// on failure, the synthetic audit line. Shared by the live path and resume.
func (tr *taskRun) finishPush(files, insertions, deletions int) {
	pushStart := time.Now()
	defer func() { tr.pushDur = time.Since(pushStart) }()
	// Pushed tree == verified tree is ENFORCED, not asserted: re-hash the
	// staged tree and fail the push closed if it drifted since verification.
	if !tr.verifiedTreeGuard() {
		return
	}
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
			`{"type":"result","subtype":"push_failed","is_error":false,"duration_ms":%d,"total_cost_usd":%.6f,"num_turns":0,"src":"broker"}`+"\n",
			time.Since(tr.taskStart).Milliseconds(), cost)
		tr.outcome = "push_failed"
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
	tr.outcome = "pushed"
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
