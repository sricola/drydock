package broker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"drydock/internal/audit"
	"drydock/internal/provider"
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
func (b *Broker) resumePush(id string, m gateMarker, st taskStage, diff string, logf *os.File) {
	// The resumed task owns this O_APPEND audit fd. Flush the terminal result and
	// close it, matching HandleTask's Sync-then-Close for the live path: without
	// this the fd leaks per resumed task and the terminal line is never fsynced.
	defer func() { _ = logf.Sync(); _ = logf.Close() }()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	b.registerTask(id, m.RepoRef, m.Instruction, cancel)
	defer b.unregisterTask(id)
	// registerTask always stamps StageRunning; this resume is, by construction,
	// a task that was sitting at the diff-approval gate when brokerd last died
	// (only gate-marked tasks reach resumePush), so correct it immediately:
	// otherwise healthz/`drydock status` counts it as "running" instead of
	// "pending_approval" until the gate resolves. Mirrors pushAndOpenPR's own
	// setStage(StagePending) before its live-path gate wait.
	b.setStage(id, StagePending)

	// Bind this resume's context to the reopened stage so a kill/shutdown aborts
	// its git push (the stage was reopened at boot, before this context existed).
	if wc, ok := st.(interface{ WithContext(context.Context) }); ok {
		wc.WithContext(ctx)
	}

	tr := &taskRun{
		b: b, ctx: ctx, sw: newDiscardStream(), id: id,
		repoRef: m.RepoRef, instruction: m.Instruction, platform: m.Platform,
		draft: m.Draft, agentName: m.Agent, st: st, logf: logf,
		auditPath: filepath.Join(b.AuditRoot, id+".jsonl"),
		taskStart: time.UnixMilli(m.TaskStartMs),
	}
	tr.subscription = audit.ReadMeta(tr.auditPath).Subscription
	if v, ok := provider.VendorForAgent(m.Agent); ok {
		tr.taskVendor = v
	}
	// Registered after the Sync/Close defer above, so it runs before them
	// (LIFO): the metrics row lands as the last line, matching the live path.
	defer tr.appendMetrics()
	if c, ok := st.(interface{ Cleanup() error }); ok {
		defer func() {
			if !tr.keepStage {
				_ = c.Cleanup()
			}
		}()
	}

	// Recorded unconditionally, unlike pushAndOpenPR's !tr.autoApprove guard:
	// auto-approved tasks return from gatePushMarked before its onReady ever
	// writes a gate marker, and resumePush only runs for marked tasks, so this
	// wait is always a real human-gate wait.
	gateStart := time.Now()
	ok, cause := b.gatePushMarked(ctx, tr, diff)
	tr.approvalGateWait = time.Since(gateStart)
	if cause == gateShutdown {
		tr.keepStage = true
		return // leave the marker; next boot resumes
	}
	files, insertions, deletions := diffStat(diff)
	tr.diffFiles = files
	tr.diffBytes = int64(len(diff))
	if !ok {
		subtype := "denied"
		tr.outcome = "denied"
		switch cause {
		case gateTimeout:
			// Resumed-and-timed-out maps to "interrupted" here, but the live
			// pushAndOpenPR path maps the same approval_timeout auto-deny to
			// "denied" (it has no equivalent on-disk "interrupted" subtype to
			// fall back to). Intentionally not reconciled in this change; see
			// the matching note in broker.go's pushAndOpenPR.
			subtype = "interrupted"
			tr.outcome = "" // already a distinct result-row subtype; no metrics override needed
		case gateKilled:
			// subtype stays "denied" (unchanged on-disk shape); the metrics row
			// carries the finer cancelled/denied split, same as the live path.
			tr.outcome = "cancelled"
		}
		// Broker-authored (src:broker) and carrying the metered cost, so a resumed
		// task's real spend still seeds the aggregate ledger.
		fmt.Fprintf(logf,
			`{"type":"result","subtype":%q,"is_error":false,"duration_ms":0,"total_cost_usd":%.6f,"num_turns":0,"src":"broker"}`+"\n",
			subtype, audit.TotalCost(tr.auditPath))
		return
	}
	tr.finishPush(files, insertions, deletions)
}
