package broker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"drydock/internal/audit"
)

// interruptedResultLine is the synthetic terminal event appended to a task
// trace that a brokerd crash left without a result line. subtype "interrupted"
// (distinct from "error") tells `drydock tasks` the daemon died under the task
// rather than the task itself failing; duration_ms is 0 (death time unknown).
const interruptedResultLine = `{"type":"result","subtype":"interrupted","is_error":true,"duration_ms":0,"total_cost_usd":0,"num_turns":0}` + "\n"

// TerminateStuckAudits scans auditRoot for <id>.jsonl traces with no terminal
// result line — tasks that were running when a prior brokerd crashed — and
// appends a synthetic "interrupted" result so `drydock tasks` resolves them
// instead of showing "running?" forever. Idempotent: a trace that already has a
// result line is left untouched. SAFE ONLY AT BOOT, when no task is live.
// Returns the count terminated and the first error (per-file errors are
// non-fatal).
func TerminateStuckAudits(auditRoot string) (int, error) {
	entries, err := os.ReadDir(auditRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	var firstErr error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(auditRoot, e.Name())
		has, herr := audit.HasResultLine(path)
		if herr != nil {
			if firstErr == nil {
				firstErr = herr
			}
			continue
		}
		if has {
			continue
		}
		if aerr := appendLine(path, interruptedResultLine); aerr != nil {
			if firstErr == nil {
				firstErr = aerr
			}
			continue
		}
		n++
	}
	return n, firstErr
}

// appendLine appends s to the file at path, refusing symlinks (O_NOFOLLOW so a
// planted <id>.jsonl -> elsewhere can't redirect the boot-time write) and
// fsyncing so the interrupted marker survives an immediate re-crash.
func appendLine(path, s string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.WriteString(s); err != nil {
		return err
	}
	return f.Sync()
}

// ResumeAwaiting reconciles awaiting-approval push gates left by a prior
// brokerd life. For each gate marker: if the stage survived, re-register the
// task as pending and resume the gate+push headlessly; otherwise append an
// honest interrupted line and drop the marker (never leave a false ok).
// Each marker is handled in its own goroutine so one slow reopen cannot
// block the others, and the function itself returns immediately.
func (b *Broker) ResumeAwaiting(stageRoot string) {
	reopen := b.reopenStage
	if reopen == nil {
		reopen = defaultReopenStage
	}
	for id, m := range ListGateMarkers(b.AuditRoot) {
		id, m := id, m // capture loop vars for goroutine
		auditPath := filepath.Join(b.AuditRoot, id+".jsonl")
		st, err := reopen(filepath.Join(stageRoot, id))
		if err != nil {
			slog.Warn("resume: stage gone, marking interrupted", "task_id", id, "err", err)
			_ = appendLine(auditPath, interruptedResultLine)
			_ = removeGateMarker(b.AuditRoot, id)
			continue
		}
		diff, _ := os.ReadFile(filepath.Join(b.AuditRoot, id+".diff"))
		logf, err := os.OpenFile(auditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			slog.Warn("resume: cannot open audit for append", "task_id", id, "err", err)
			continue
		}
		slog.Info("resuming awaiting-approval task", "task_id", id)
		go b.resumePush(id, m, st, string(diff), logf)
	}
}

// resumePush re-registers a resumed task as pending and drives the gate+push
// tail headlessly (no live client). Approve pushes the surviving branch;
// deny/timeout write the honest terminal outcome. On shutdown the marker is
// left for the next boot (idempotent).
func (b *Broker) resumePush(id string, m gateMarker, st taskStage, diff string, logf io.Writer) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	b.registerTask(id, m.RepoRef, m.Instruction, cancel)
	defer b.unregisterTask(id)

	tr := &taskRun{
		b: b, ctx: ctx, sw: newDiscardStream(), id: id,
		repoRef: m.RepoRef, instruction: m.Instruction, platform: m.Platform,
		draft: m.Draft, agentName: m.Agent, st: st, logf: logf,
		auditPath: filepath.Join(b.AuditRoot, id+".jsonl"),
		taskStart: time.UnixMilli(m.TaskStartMs),
	}
	if c, ok := st.(interface{ Cleanup() error }); ok {
		defer func() {
			if !tr.keepStage {
				_ = c.Cleanup()
			}
		}()
	}

	ok, cause := b.gatePushMarked(ctx, tr, diff)
	if cause == gateShutdown {
		tr.keepStage = true
		return // leave the marker; next boot resumes
	}
	files, insertions, deletions := diffStat(diff)
	if !ok {
		subtype := "denied"
		if cause == gateTimeout {
			subtype = "interrupted"
		}
		// Broker-authored (src:broker) and carrying the metered cost, so a resumed
		// task's real spend still seeds the aggregate ledger.
		fmt.Fprintf(logf,
			`{"type":"result","subtype":%q,"is_error":false,"duration_ms":0,"total_cost_usd":%.6f,"num_turns":0,"src":"broker"}`+"\n",
			subtype, audit.TotalCost(tr.auditPath))
		return
	}
	tr.finishPush(diff, files, insertions, deletions)
}
