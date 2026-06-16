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
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"drydock/internal/creds"
	"drydock/internal/egress"
	"drydock/internal/runner"
	"drydock/internal/stage"
)

// githubRepoRef matches the three RepoRef forms whose `origin` lets the
// host-side `gh pr create` find a GitHub host. Local paths are rejected
// because the staging clone would inherit a filesystem origin and the push
// flow can't open a PR against it.
var githubRepoRef = regexp.MustCompile(
	`^(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)[A-Za-z0-9._-]+/[A-Za-z0-9._-]+?(?:\.git)?/?$`,
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
}

// ApprovalFn gates the egress-widening step. The diff-push step now has
// its own explicit gate driven by Task.AutoApprove + the admin endpoints.
type ApprovalFn func(kind string, payload any) bool

type Broker struct {
	Cfg        egress.Config
	Creds      creds.Provider
	Approve    ApprovalFn
	ImageRef   string
	StageRoot  string
	AuditRoot  string
	Timeout    time.Duration
	Network    string  // stable egress network name (e.g. drydock-egress)
	GatewayIP  string  // vmnet gateway IP the VM reaches (e.g. 192.168.64.1)
	ProxyPort  int     // squid port (e.g. 3128)
	TaskBudget float64 // USD budget per task

	// MaxConcurrent caps how many tasks may be in any non-terminal state at
	// once. Excess POSTs to /tasks return 503. Default (when zero) is 2.
	MaxConcurrent int

	// slots is a bounded semaphore guarding MaxConcurrent. Initialized lazily
	// the first time HandleTask is called (so existing callers that build a
	// Broker by struct literal keep working).
	slotsOnce sync.Once
	slots     chan struct{}

	pendingMu sync.Mutex
	pending   map[string]chan bool       // task_id -> approval channel
	tasks     map[string]*TaskState      // task_id -> live state (running + awaiting_approval)
}

// TaskStage tracks where a task currently is in its lifecycle. Only the
// non-terminal stages live in Broker.tasks — completed tasks fall out as
// HandleTask returns.
type TaskStage string

const (
	StageRunning  TaskStage = "running"
	StagePending  TaskStage = "awaiting_approval"
	StagePushing  TaskStage = "pushing"
)

// TaskState is the operator-facing snapshot returned by GET /admin/tasks.
type TaskState struct {
	ID          string    `json:"id"`
	Repo        string    `json:"repo"`
	Instruction string    `json:"instruction"` // truncated for display
	Stage       TaskStage `json:"stage"`
	StartedAt   time.Time `json:"started_at"`
}

const instructionSnippetMax = 140

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
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

// registerTask records a task in the live-tasks map under StageRunning.
func (b *Broker) registerTask(id, repo, instruction string) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	if b.tasks == nil {
		b.tasks = make(map[string]*TaskState)
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
}

func (b *Broker) setStage(id string, s TaskStage) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	if t, ok := b.tasks[id]; ok {
		t.Stage = s
	}
}

func (b *Broker) unregisterTask(id string) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	delete(b.tasks, id)
}

func (b *Broker) HandleTask(w http.ResponseWriter, r *http.Request) {
	var t Task
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !githubRepoRef.MatchString(t.RepoRef) {
		http.Error(w, "repo_ref must be a github.com URL (https://, git@, or ssh://)", http.StatusBadRequest)
		return
	}
	if !b.acquireSlot() {
		http.Error(w,
			"too many concurrent tasks; raise DRYDOCK_MAX_CONCURRENT_TASKS or wait",
			http.StatusServiceUnavailable)
		return
	}
	defer b.releaseSlot()

	if len(t.EgressExtra) > 0 && b.Cfg.PerTaskWidening.RequiresApproval {
		if !b.Approve("widen egress", t.EgressExtra) {
			http.Error(w, "egress widening denied", http.StatusForbidden)
			return
		}
	}

	taskID := newID()
	b.registerTask(taskID, t.RepoRef, t.Instruction)
	defer b.unregisterTask(taskID)
	stageDir := filepath.Join(b.StageRoot, taskID)

	st, err := stage.Prepare(stageDir, t.RepoRef)
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

	grant, err := b.Creds.Mint(b.TaskBudget)
	if err != nil {
		http.Error(w, "credential mint failed", http.StatusInternalServerError)
		return
	}
	defer grant.Revoke()

	if err := os.MkdirAll(b.AuditRoot, 0o755); err != nil {
		http.Error(w, "audit dir failed", http.StatusInternalServerError)
		return
	}
	logf, err := os.Create(filepath.Join(b.AuditRoot, taskID+".jsonl"))
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

	args := runner.BuildRunArgs(runner.Spec{
		TaskID:     taskID,
		Network:    b.Network,
		ImageRef:   b.ImageRef,
		Env:        env,
		StageDir:   st.WorkDir,
		PromptFile: "/work/.task/prompt.txt",
		MemoryGB:   4,
		CPUs:       4,
	})

	ctx, cancel := context.WithTimeout(r.Context(), b.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "container", args...)
	cmd.Stdout = io.MultiWriter(logf, os.Stdout)
	cmd.Stderr = logf
	if err := cmd.Run(); err != nil {
		// --rm covers a graceful exit; on timeout/kill the VM may survive, so
		// force-remove it (best effort) to honor the ephemeral-VM backstop.
		_ = exec.Command("container", "delete", "--force", "task-"+taskID).Run()
		http.Error(w, "task failed: "+err.Error(), http.StatusInternalServerError)
		return
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
	if !b.gatePush(r.Context(), taskID, diff, t.AutoApprove) {
		writeJSON(w, map[string]any{"task_id": taskID, "diff": diff, "pushed": false})
		return
	}

	b.setStage(taskID, StagePushing)
	branch := "agent/" + taskID
	if err := st.Push(branch, "agent: "+firstLine(t.Instruction)); err != nil {
		http.Error(w, "push failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"task_id": taskID, "branch": branch, "pushed": true})
}

// gatePush blocks until POST /admin/approve/{id} or /admin/deny/{id} (or the
// HTTP client disconnects). Returning false aborts the push and the diff is
// returned to the caller without ever touching origin. When auto is true the
// gate is bypassed — callers must opt in explicitly via Task.AutoApprove.
func (b *Broker) gatePush(ctx context.Context, taskID, diff string, auto bool) bool {
	if auto {
		log.Printf("task %s: auto-approve push (caller opted in)", taskID)
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
	log.Printf("task %s awaiting approval (%d bytes, diff at %s) — run: drydock approve %s | drydock deny %s",
		taskID, len(diff), diffPath, taskID, taskID)
	notifyMac("drydock — task awaiting approval",
		fmt.Sprintf("task %s · %d byte diff · drydock approve %s", taskID, len(diff), taskID))

	select {
	case ok := <-ch:
		return ok
	case <-ctx.Done():
		log.Printf("task %s: client disconnected before approval; aborting push", taskID)
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
	var running, pendingApproval, pushing int
	for _, t := range b.tasks {
		switch t.Stage {
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
		"running":          running,
		"pending_approval": pendingApproval,
		"pushing":          pushing,
	})
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

// firstLine returns the first line of s, capped, for a sane commit subject from
// an attacker-influenced instruction.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 72 {
		s = s[:72]
	}
	return s
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
