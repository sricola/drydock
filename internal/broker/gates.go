package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"drydock/internal/egress"
)

var (
	errTaskKilled = errors.New("task killed")
	errShutdown   = errors.New("brokerd shutting down")
)

// gateCause records why awaitGate returned false (or true).
type gateCause int

const (
	gateApproved gateCause = iota
	gateDenied
	gateKilled
	gateTimeout
	gateShutdown
)

// awaitGate is the shared skeleton for gatePushMarked and gateEgressWiden. It:
//   - optionally wraps ctx with the operator ApprovalTimeout;
//   - registers a buffered channel in b.pending under taskID (deregisters on return);
//   - calls onReady, which must persist the review artifact, log, and fire any
//     macOS notification specific to this gate;
//   - blocks until the channel receives a signal or ctx is cancelled.
//
// Returns (true, gateApproved) on approval, (false, cause) on deny/kill/timeout/shutdown.
// timeoutMsg is logged at Warn on DeadlineExceeded; cancelMsg is logged at Info
// on any context cancellation that is not a shutdown.
func (b *Broker) awaitGate(ctx context.Context, taskID, timeoutMsg, cancelMsg string, onReady func()) (bool, gateCause) {
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

	onReady()

	select {
	case ok := <-ch:
		if ok {
			return true, gateApproved
		}
		return false, gateDenied
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			slog.Warn(timeoutMsg, "task_id", taskID, "timeout", b.ApprovalTimeout)
			return false, gateTimeout
		}
		switch context.Cause(ctx) {
		case errShutdown:
			slog.Info("task gate interrupted by shutdown", "task_id", taskID)
			return false, gateShutdown
		case errTaskKilled:
			slog.Info(cancelMsg, "task_id", taskID)
			return false, gateKilled
		default:
			slog.Info(cancelMsg, "task_id", taskID)
			return false, gateKilled
		}
	}
}

// persistDiff writes the captured review diff to <AuditRoot>/<id>.diff with
// the same defenses as every other audit artifact (0600, O_NOFOLLOW — a
// planted symlink fails instead of redirecting the write). Best effort: a
// failure warns rather than failing the task, matching writeBrief. Returns
// the path for logging. Called at diff-capture time in HandleTask (so
// verify_failed and mid-verify cancels leave the evidence too) and again from
// gatePushMarked's onReady (harmless rewrite of the same bytes; the resume
// path reads the file this leaves behind).
func (b *Broker) persistDiff(id, diff string) string {
	path := filepath.Join(b.AuditRoot, id+".diff")
	f, err := os.OpenFile(path,
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		slog.Warn("could not persist diff for review", "task_id", id, "err", err)
		return path
	}
	defer f.Close()
	if _, err := f.WriteString(diff); err != nil {
		slog.Warn("could not persist diff for review", "task_id", id, "err", err)
	}
	return path
}

// gatePushMarked blocks at the diff-approval gate with a durable resume marker.
// It persists the diff and a gate marker, blocks for approval, then removes the
// marker UNLESS the gate was interrupted by shutdown (left for boot resume).
// When the task opted into auto-approve the gate is bypassed entirely (no wait,
// no review artifact) — callers opt in explicitly via Task.AutoApprove.
func (b *Broker) gatePushMarked(ctx context.Context, tr *taskRun, diff string) (bool, gateCause) {
	if tr.autoApprove {
		slog.Info("task auto-approve push", "task_id", tr.id, "reason", "caller opted in")
		return true, gateApproved
	}
	ok, cause := b.awaitGate(ctx, tr.id,
		"task auto-denied at approval gate (approval_timeout reached)",
		"task killed or broker shutting down before approval; aborting",
		func() {
			diffPath := b.persistDiff(tr.id, diff)
			if werr := writeGateMarker(b.AuditRoot, tr.id, gateMarker{
				RepoRef: tr.repoRef, Instruction: tr.instruction, Platform: tr.platform,
				Agent: tr.agentName, Draft: tr.draft,
				TaskStartMs: tr.taskStart.UnixMilli(),
			}); werr != nil {
				slog.Warn("could not persist gate marker", "task_id", tr.id, "err", werr)
			}
			slog.Info("task awaiting approval",
				"task_id", tr.id, "diff_bytes", len(diff), "diff_path", diffPath,
				"hint", "drydock approve "+tr.id+" | drydock deny "+tr.id)
			b.notifyMac("drydock: task awaiting approval",
				fmt.Sprintf("task %s (%d byte diff): drydock approve %s", tr.id, len(diff), tr.id))
		})
	if cause != gateShutdown {
		if err := removeGateMarker(b.AuditRoot, tr.id); err != nil {
			slog.Warn("gate marker not removed; may cause a spurious interrupted on the next boot",
				"task_id", tr.id, "err", err)
		}
	}
	return ok, cause
}

// gateOutcome maps a gate cause to the outcome recorded on the metrics row
// and, for a headless resume, the on-disk result subtype the caller appends
// directly to the audit log. It is the single place encoding how the live
// path (pushAndOpenPR, resumed=false) and the resume path (resumePush,
// resumed=true) agree, and the one place they intentionally disagree.
//
// Every cause maps identically on both paths except gateTimeout: the live
// path has no on-disk "interrupted" result row to fall back on (the agent's
// own "success" row is still the last result line, so the metrics-row
// outcome IS the only record of the auto-deny), so it reads as "denied".
// A resumed task, however, writes its own result row directly, so it uses
// the more precise "interrupted" subtype and leaves the metrics outcome
// empty (OutcomeKeyWithMetrics then classifies purely from that subtype,
// exactly as a pre-outcome-field metrics row would).
//
// gateShutdown always yields an empty outcome on both paths: a shutdown-
// parked task is only paused, not cancelled or denied, and its resume is
// (by construction) still alive. Recording any terminal outcome for it
// would make `drydock tasks` / the web UI History show a false terminal
// state for the whole time it sits parked awaiting the next boot; leaving
// it empty means readers fall back to the (not-yet-written) result row, the
// same rule the resumed gateTimeout case already relies on.
func gateOutcome(cause gateCause, resumed bool) (outcome, subtype string) {
	switch cause {
	case gateDenied:
		return "denied", "denied"
	case gateKilled:
		// subtype stays "denied" (unchanged on-disk shape); the metrics row
		// carries the finer cancelled/denied split.
		return "cancelled", "denied"
	case gateTimeout:
		if resumed {
			return "", "interrupted"
		}
		return "denied", "denied"
	case gateShutdown:
		return "", ""
	default:
		return "denied", "denied"
	}
}

// gateEgressWiden blocks until POST /admin/approve/{id} or /admin/deny/{id}
// (or the HTTP client disconnects / the task is killed). Returning false
// aborts the task before any allowlist compilation — the requested hosts
// never reach squid. Mirrors gatePushMarked so the operator only has to learn one
// approval flow.
func (b *Broker) gateEgressWiden(ctx context.Context, taskID string, extras []egress.Domain) bool {
	ok, _ := b.awaitGate(ctx, taskID,
		"task auto-denied at egress gate (approval_timeout reached)",
		"task cancelled at egress gate",
		func() {
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
		})
	return ok
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
