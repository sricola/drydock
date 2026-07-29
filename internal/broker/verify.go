package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"drydock/internal/audit"
	"drydock/internal/repokey"
	"drydock/internal/runner"
	"drydock/internal/trustbrief"
)

// VerifyRepo is one repository's verification recipe — the broker-local
// mirror of config.VerifyRepo (cmd/brokerd maps between them so the broker
// package never imports config). Timeout 0 uses DefaultVerifyTimeout.
// Required=true blocks the push unless the overall verification status is
// exactly "passed" — fail-closed, so inconclusive evidence blocks too.
type VerifyRepo struct {
	Commands [][]string
	Timeout  time.Duration
	Required bool
}

// DefaultVerifyTimeout bounds each verification command when the repo's
// config leaves timeout unset. Exported so the operator-facing docs and the
// Brief claims table cite the same constant the broker actually enforces.
const DefaultVerifyTimeout = 10 * time.Minute

// stagedExporter is the optional stage capability the verifier needs: a
// sealed export of the staged tree (ExportStaged returns the hash of the
// tree it actually archived, so recording the identity and materializing the
// copy is one atomic act), plus a re-hash for the push-time guard. Production
// realStage has it (via *stage.Stage); test fakes may not — absence makes
// verification inconclusive, never silently passed (same optional-capability
// idiom as BaseCommit/PushPreflight).
type stagedExporter interface {
	StagedTreeHash() (string, error)
	ExportStaged(string) (string, error)
}

// runVerify is the verifying stage: it runs the repo's host-approved
// verification commands against a sealed export of the agent's staged tree,
// each in a fresh, credential-free, network-denied VM, and records the
// broker-observed results on tr.verify for the Brief.
//
// The verdict for each command is the container process exit code observed
// through the b.runAgent seam — nothing printed by the verify VM can reach a
// status, exit code, or metric (F-07). The VM's output goes only to the
// display-only <id>.verify.log, capped, with its digest computed host-side.
//
// diff is the captured review diff, threaded through so a required-verify
// failure can still persist the trust brief before its terminal event.
//
// Returns false only when it already emitted the task's terminal event:
// required-and-not-passed (outcome "verify_failed") or task cancellation.
func (tr *taskRun) runVerify(diff string) bool {
	b := tr.b
	key := repokey.Normalize(tr.repoRef)
	cfg, ok := b.Verify[key]
	if !ok {
		// Matching is deliberately case-sensitive on the owner/repo path (the
		// host is lowercased by repokey.Normalize). A near-miss that differs
		// only in case would otherwise SILENTLY skip a required verification,
		// so warn loudly — but never auto-match: the operator wrote the key,
		// the operator fixes the key.
		for k := range b.Verify {
			if strings.EqualFold(k, key) {
				slog.Warn("verify: repo matches a verify.repos key only case-insensitively; "+
					"verification was SKIPPED (not_configured) — fix the config key casing",
					"task_repo", key, "config_key", k)
				break
			}
		}
		tr.verify = &trustbrief.Verification{Status: trustbrief.VerificationNotConfigured}
		return true
	}
	b.setStage(tr.id, StageVerifying)
	tr.sw.emit(map[string]any{"event": "stage", "stage": "verifying",
		"task_id": tr.id, "commands": len(cfg.Commands)})
	start := time.Now()
	v, cancelled := tr.execVerify(cfg)
	tr.verify = v
	tr.verifyDur = time.Since(start)
	if cancelled {
		// Mirror runSandbox's tr.ctx.Err() != nil path: broker-authored error
		// result, then the standard cancelled terminal.
		tr.appendBrokerResult(true)
		tr.outcome = "cancelled"
		tr.sw.emit(map[string]any{"event": "result", "outcome": "cancelled", "task_id": tr.id})
		return false
	}
	if cfg.Required && v.Status != trustbrief.VerificationPassed {
		// Fail closed: required verification that is anything but "passed"
		// (failed, inconclusive) blocks the push before any gate is entered.
		// The trust brief is persisted BEFORE the terminal event: the hint
		// below points the operator at `drydock inspect`, which reads the
		// brief, so a verify_failed task must leave the same evidence (brief
		// with the full Verification block; the .diff was already written at
		// capture time) a gated task would.
		b.writeBrief(tr, diff)
		// Synthetic audit result row mirrors finishPush's push_failed pattern
		// (last-wins over the agent's own success row, carrying metered cost).
		cost := audit.TotalCost(tr.auditPath)
		fmt.Fprintf(tr.logf,
			`{"type":"result","subtype":"verify_failed","is_error":false,"duration_ms":%d,"total_cost_usd":%.6f,"num_turns":0,"src":"broker"}`+"\n",
			time.Since(tr.taskStart).Milliseconds(), cost)
		tr.outcome = "verify_failed"
		// Point at the verification log only when one was actually created —
		// the inconclusive paths that never launched a VM (no export
		// capability, export failure, empty commands) have no log file, and a
		// hint at a nonexistent path would send the operator chasing nothing.
		hint := "drydock inspect " + tr.id
		if _, lerr := os.Lstat(tr.verifyLogPath()); lerr == nil {
			hint += " · verification log: " + tr.verifyLogPath()
		}
		tr.sw.emit(map[string]any{"event": "result", "outcome": "verify_failed",
			"task_id": tr.id, "verify_status": v.Status,
			"duration_ms": time.Since(tr.taskStart).Milliseconds(), "cost_usd": cost,
			"hint": hint})
		return false
	}
	return true
}

// execVerify exports the sealed tree and runs the commands, returning the
// evidence block. cancelled=true means tr.ctx was cancelled mid-run (kill or
// shutdown) — the caller emits the standard cancelled terminal.
func (tr *taskRun) execVerify(cfg VerifyRepo) (v *trustbrief.Verification, cancelled bool) {
	b := tr.b
	// The Network/Credentials posture is asserted only once a verifier VM is
	// actually about to launch (below, after the capability/export/log checks
	// all succeed): the inconclusive paths that never ran a VM must not carry
	// a posture claim for VMs that never existed.
	v = &trustbrief.Verification{}

	if len(cfg.Commands) == 0 {
		// config.Validate forbids an empty command list, but Broker.Verify can
		// be populated programmatically too. Zero commands is zero evidence,
		// and zero evidence must never read as "passed" (F-07).
		slog.Warn("verify: no commands configured; verification inconclusive", "task_id", tr.id)
		v.Status = trustbrief.VerificationInconclusive
		return v, false
	}

	es, ok := tr.st.(stagedExporter)
	if !ok {
		slog.Warn("verify: stage lacks staged-tree export; verification inconclusive", "task_id", tr.id)
		v.Status = trustbrief.VerificationInconclusive
		return v, false
	}
	// The export must sit inside the stage (same quota image, reaped by the
	// same Cleanup), but taskStage only exposes WorkDir(). stage.Prepare
	// lays out <root>/work and <root>/git, so the stage root is WorkDir's
	// parent and the sealed export goes at <root>/verify beside them.
	// ExportStaged returns the hash of the tree it actually archived — the
	// recorded TreeSHA and the sealed copy come from ONE write-tree run, so
	// there is no window in which a work-tree edit could make the recorded
	// identity differ from what the verifier ran against.
	verifyDir := filepath.Join(filepath.Dir(tr.st.WorkDir()), "verify")
	tree, err := es.ExportStaged(verifyDir)
	if err != nil {
		slog.Warn("verify: staged-tree export failed; verification inconclusive",
			"task_id", tr.id, "err", err)
		v.Status = trustbrief.VerificationInconclusive
		return v, false
	}
	v.TreeSHA = tree

	// Display-only log of the verify VMs' output: same defenses as every other
	// audit artifact (0600, O_NOFOLLOW). Nothing in it is ever parsed.
	logPath := tr.verifyLogPath()
	logf, err := os.OpenFile(logPath,
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		slog.Warn("verify: could not open verification log; verification inconclusive",
			"task_id", tr.id, "err", err)
		v.Status = trustbrief.VerificationInconclusive
		return v, false
	}
	// Double Close (explicit close before hashing below, then this defer on
	// early returns) is harmless on *os.File.
	defer logf.Close()

	// Every never-ran path is behind us: from here at least one verifier VM
	// launches, so record the capability posture those VMs actually run with.
	v.Network, v.Credentials = "denied", "none"

	// One shared output budget across all commands (same bound as the agent
	// run). onExceed cancels whichever command is in flight so a log flood
	// terminates the VM instead of filling the host disk.
	var curMu sync.Mutex
	var cancelCur context.CancelFunc
	outCap := newOutputCap(maxTaskOutputBytes, func() {
		curMu.Lock()
		defer curMu.Unlock()
		if cancelCur != nil {
			cancelCur()
		}
	})
	capped := outCap.wrap(logf)

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultVerifyTimeout
	}
	run := b.runAgent
	if run == nil {
		run = runContainer
	}

	cmds := make([]trustbrief.VerifyCommand, 0, len(cfg.Commands))
	stop := false
	for _, argv := range cfg.Commands {
		if stop {
			// Fail fast: once a command failed (or timed out / errored), the
			// rest are recorded as skipped, never run.
			cmds = append(cmds, trustbrief.VerifyCommand{Argv: argv, Status: trustbrief.VerifyCmdSkipped})
			continue
		}
		args := runner.BuildVerifyArgs(runner.VerifySpec{
			TaskID:    tr.id,
			Network:   b.Network,
			ImageRef:  b.ImageRef,
			VerifyDir: verifyDir,
			Argv:      argv,
			MemoryGB:  4,
			CPUs:      4,
		})
		cmdCtx, cancel := context.WithTimeout(tr.ctx, timeout)
		curMu.Lock()
		cancelCur = cancel
		curMu.Unlock()
		cstart := time.Now()
		rerr := run(cmdCtx, args, capped, capped)
		dur := time.Since(cstart)
		curMu.Lock()
		cancelCur = nil
		curMu.Unlock()
		timedOut := cmdCtx.Err() == context.DeadlineExceeded
		cancel()

		// Classify from what the BROKER observed — the seam's error — never
		// from VM output. Context states are checked before exit codes because
		// a killed container also surfaces as an ExitError.
		vc := trustbrief.VerifyCommand{Argv: argv, DurationMs: dur.Milliseconds()}
		var ee *exec.ExitError
		switch {
		case rerr == nil:
			vc.Status = trustbrief.VerifyCmdPassed
		case tr.ctx.Err() != nil:
			// Operator kill or brokerd shutdown mid-verify: force-delete the
			// possibly-surviving VM and hand the cancelled terminal to runVerify.
			tr.deleteVerifyVM()
			v.Status = trustbrief.VerificationInconclusive
			return v, true
		case outCap.exceeded():
			// We cancelled the command ourselves: its output crossed the host
			// cap. Evidence is absent, not failing — VerifyCmdError.
			tr.deleteVerifyVM()
			slog.Warn("verify: command output exceeded the host cap; terminated",
				"task_id", tr.id, "cap_mib", maxTaskOutputBytes>>20)
			fmt.Fprintf(logf, "\n[drydock] verification output exceeded the %d MiB host cap; command terminated\n",
				maxTaskOutputBytes>>20)
			vc.Status = trustbrief.VerifyCmdError
			stop = true
		case timedOut:
			// --rm covers a graceful exit; on timeout the VM may survive, so
			// force-remove it (best effort), same as the agent-run path.
			tr.deleteVerifyVM()
			vc.Status = trustbrief.VerifyCmdTimedOut
			stop = true
		case errors.As(rerr, &ee):
			vc.Status = trustbrief.VerifyCmdFailed
			vc.ExitCode = ee.ExitCode()
			stop = true
		default:
			// Infra failure (container CLI missing, image missing): evidence
			// absent, not a pass and not a repo failure.
			slog.Warn("verify: command run error", "task_id", tr.id, "err", rerr)
			vc.Status = trustbrief.VerifyCmdError
			stop = true
		}
		cmds = append(cmds, vc)
	}
	v.Commands = cmds

	// Overall verdict from the per-command statuses alone: all passed →
	// passed; any failed → failed; anything else present (timed_out, error,
	// skipped-after-non-failure) → inconclusive. Absence of evidence must
	// never read as passing.
	status := trustbrief.VerificationPassed
	for _, c := range cmds {
		switch c.Status {
		case trustbrief.VerifyCmdPassed:
		case trustbrief.VerifyCmdFailed:
			status = trustbrief.VerificationFailed
		default:
			if status != trustbrief.VerificationFailed {
				status = trustbrief.VerificationInconclusive
			}
		}
	}
	v.Status = status

	// Digest the log host-side by re-reading the closed file, so LogSHA256
	// attests to the bytes on disk, not to what we think we wrote.
	if cerr := logf.Close(); cerr != nil {
		slog.Warn("verify: close verification log", "task_id", tr.id, "err", cerr)
	}
	if sum, herr := fileSHA256NoFollow(logPath); herr == nil {
		v.LogSHA256 = sum
	} else {
		slog.Warn("verify: could not hash verification log", "task_id", tr.id, "err", herr)
	}
	return v, false
}

// verifiedTreeGuard enforces "pushed tree == verified tree" at push time.
// When verification recorded a TreeSHA, the staged tree is re-hashed here; on
// any error or mismatch the push fails closed exactly like a push error
// (synthetic push_failed audit row + terminal event) and nothing is pushed.
// Returns true to continue with the push.
func (tr *taskRun) verifiedTreeGuard() bool {
	if tr.verify == nil || tr.verify.TreeSHA == "" {
		return true
	}
	var err error
	if es, ok := tr.st.(stagedExporter); !ok {
		err = errors.New("stage lost its staged-tree capability before push")
	} else {
		var h string
		h, err = es.StagedTreeHash()
		if err == nil {
			if h == tr.verify.TreeSHA {
				return true
			}
			err = fmt.Errorf("staged tree %s != verified tree %s", h, tr.verify.TreeSHA)
		}
	}
	cost := audit.TotalCost(tr.auditPath)
	fmt.Fprintf(tr.logf,
		`{"type":"result","subtype":"push_failed","is_error":false,"duration_ms":%d,"total_cost_usd":%.6f,"num_turns":0,"src":"broker"}`+"\n",
		time.Since(tr.taskStart).Milliseconds(), cost)
	tr.outcome = "push_failed"
	tr.sw.emit(map[string]any{"event": "result", "outcome": "push_failed",
		"task_id": tr.id,
		"reason":  "verified-tree mismatch: work tree changed after verification",
		"branch":  "agent/" + tr.id, "error": safeErr(err),
		"duration_ms": time.Since(tr.taskStart).Milliseconds(), "cost_usd": cost,
		"hint": "nothing was pushed; the work tree no longer matches the verified tree — retry with `drydock retry " + tr.id + "`"})
	return false
}

// deleteVerifyVM force-deletes this task's verifier VM through the same
// bounded container-delete path runSandbox uses for task-<id>. Best effort:
// a survivor is reaped at the next brokerd boot by the orphan reaper.
func (tr *taskRun) deleteVerifyVM() {
	if err := tr.b.forceDelete(runner.VerifyContainerName(tr.id)); err != nil {
		slog.Warn("force-delete of verify VM failed; reaped at next brokerd boot",
			"task_id", tr.id, "err", err)
	}
}

// verifyLogPath is the display-only verifier output log for this task.
func (tr *taskRun) verifyLogPath() string {
	return filepath.Join(tr.b.AuditRoot, tr.id+".verify.log")
}

// fileSHA256NoFollow hashes a file's bytes, refusing symlinks like every
// other audit-artifact open in this package.
func fileSHA256NoFollow(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
