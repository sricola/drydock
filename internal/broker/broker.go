// Package broker wires staging, egress compilation, credential minting, the
// container run, diff capture, the approval gate, and the host-side push.
package broker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"macagent/internal/creds"
	"macagent/internal/egress"
	"macagent/internal/runner"
	"macagent/internal/stage"
)

type Task struct {
	RepoRef     string          `json:"repo_ref"`
	Instruction string          `json:"instruction"`
	EgressExtra []egress.Domain `json:"egress_extra"`
	Sensitive   bool            `json:"sensitive"`
}

// ApprovalFn returns true to allow the action. MVP default may auto-approve.
type ApprovalFn func(kind string, payload any) bool

type Broker struct {
	Cfg       egress.Config
	Creds     creds.Provider
	Approve   ApprovalFn
	ImageRef  string
	StageRoot string
	AuditRoot string
	Timeout   time.Duration
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (b *Broker) HandleTask(w http.ResponseWriter, r *http.Request) {
	var t Task
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(t.EgressExtra) > 0 && b.Cfg.PerTaskWidening.RequiresApproval {
		if !b.Approve("widen egress", t.EgressExtra) {
			http.Error(w, "egress widening denied", http.StatusForbidden)
			return
		}
	}

	taskID := newID()
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

	tok, err := b.Creds.Mint(15 * time.Minute)
	if err != nil {
		http.Error(w, "credential mint failed", http.StatusInternalServerError)
		return
	}
	defer b.Creds.Revoke(tok)

	if err := os.MkdirAll(b.AuditRoot, 0o755); err != nil {
		http.Error(w, "audit dir failed", http.StatusInternalServerError)
		return
	}
	auditPath := filepath.Join(b.AuditRoot, taskID+".jsonl")
	logf, err := os.Create(auditPath)
	if err != nil {
		http.Error(w, "audit file failed", http.StatusInternalServerError)
		return
	}
	defer logf.Close()

	args := runner.BuildRunArgs(runner.Spec{
		TaskID:     taskID,
		Network:    "", // MVP: default per-VM network; v2 sets a named egress net
		ImageRef:   b.ImageRef,
		APIKey:     tok.Value,
		StageDir:   st.WorkDir, // mount ONLY the work tree, never the host-only .git
		PromptFile: "/work/.task/prompt.txt",
		MemoryGB:   4,
		CPUs:       4,
	})
	// MVP runs on the default network; drop the empty --network pair.
	args = dropEmptyNetwork(args)

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
	if !b.Approve("push diff", diff) {
		writeJSON(w, map[string]any{"task_id": taskID, "diff": diff, "pushed": false})
		return
	}

	branch := "agent/" + taskID
	if err := st.Push(branch, "agent: "+firstLine(t.Instruction)); err != nil {
		http.Error(w, "push failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"task_id": taskID, "branch": branch, "pushed": true})
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

func dropEmptyNetwork(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--network" && i+1 < len(args) && args[i+1] == "" {
			i++ // skip flag and its empty value
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
