package broker

import (
	"context"
	"errors"
	"fmt"
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

// SetupProfile is one repository's execution profile — the broker-local
// mirror of config.SetupProfile (cmd/brokerd maps between them so the broker
// package never imports config for it). Setup commands prepare the live
// stage; Readiness commands then gate the run. Timeout 0 uses
// DefaultSetupTimeout per command.
//
// Unlike VerifyRepo there is no Required knob: setup is ALWAYS enforced. A
// profile that does not pass means the workspace isn't ready, and a
// not-ready workspace fails the task closed BEFORE the agent VM boots and
// before any bearer is injected into any VM (fail-closed-before-spend).
type SetupProfile struct {
	Setup     [][]string
	Readiness [][]string
	Timeout   time.Duration
}

// DefaultSetupTimeout bounds each setup/readiness command when the repo's
// profile leaves timeout unset. Exported so the operator-facing docs and the
// Brief claims table cite the same constant the broker actually enforces.
const DefaultSetupTimeout = 10 * time.Minute

// runSetup is the setting_up stage: it runs the repo's host-approved setup
// and readiness commands against the LIVE stage work tree, each in a fresh
// bearer-free VM whose egress is squid-only (runner.BuildSetupArgs), and
// records the broker-observed results on tr.setup for the Brief.
//
// The security invariant is fail-closed-before-spend: runSetup is called
// after the credential grant is minted but BEFORE the agent VM boots, and
// the setup env (buildSetupEnv) never carries the grant — so on any setup
// failure the task terminates with the bearer never injected into any VM
// and zero API budget spent. The verdict for each command is the container
// process exit code observed through the b.runAgent seam — nothing printed
// by the setup VM can flip a status (mirroring runVerify / F-07).
//
// Returns false only when it already emitted the task's terminal event:
// setup-not-passed (outcome "setup_failed") or task cancellation.
func (tr *taskRun) runSetup() bool {
	b := tr.b
	key := repokey.Normalize(tr.repoRef)
	cfg, ok := b.Setup[key]
	if !ok {
		// Matching is deliberately case-sensitive on the owner/repo path (the
		// host is lowercased by repokey.Normalize). A near-miss that differs
		// only in case would otherwise SILENTLY skip the profile, so warn
		// loudly — but never auto-match: the operator wrote the key, the
		// operator fixes the key. (Same rule as runVerify.)
		for k := range b.Setup {
			if strings.EqualFold(k, key) {
				slog.Warn("setup: repo matches a profiles.repos key only case-insensitively; "+
					"setup was SKIPPED (not_configured) — fix the config key casing",
					"task_repo", key, "config_key", k)
				break
			}
		}
		tr.setup = &trustbrief.SetupEvidence{Status: trustbrief.SetupNotConfigured}
		return true
	}
	b.setStage(tr.id, StageSettingUp)
	tr.sw.emit(map[string]any{"event": "stage", "stage": "setting_up",
		"task_id": tr.id, "commands": len(cfg.Setup) + len(cfg.Readiness)})
	start := time.Now()
	s, cancelled := tr.execSetup(cfg)
	tr.setup = s
	tr.setupDur = time.Since(start)
	if tr.taskStart.IsZero() {
		// The agent hasn't started yet (runSandbox sets taskStart), but the
		// duration accounting downstream (appendBrokerResult, the result rows,
		// writeBrief's SpendFacts) is anchored at tr.taskStart. Anchor it at
		// the setup phase so a setup-terminated task reports its real
		// wall-clock instead of a since-the-epoch absurdity; runSandbox
		// overwrites it when the agent actually starts.
		tr.taskStart = start
	}
	if cancelled {
		// Mirror runSandbox's tr.ctx.Err() != nil path: broker-authored error
		// result, then the standard cancelled terminal.
		tr.appendBrokerResult(true)
		tr.outcome = "cancelled"
		tr.sw.emit(map[string]any{"event": "result", "outcome": "cancelled", "task_id": tr.id})
		return false
	}
	if s.Status != trustbrief.SetupPassed {
		// Fail closed before spend: anything but "passed" (failed,
		// inconclusive) terminates the task HERE — the agent VM never boots
		// and the minted grant, injected into no VM, is revoked by the defer
		// registered at mint time. The trust brief is persisted BEFORE the
		// terminal event (diff is empty — no agent ran, so there is none) so
		// `drydock inspect` shows the full Setup evidence block.
		b.writeBrief(tr, "")
		// Synthetic audit result row mirrors runVerify's verify_failed pattern
		// (last-wins, src:"broker", carrying metered cost — zero here, since
		// no bearer ever existed inside a VM).
		cost := audit.TotalCost(tr.auditPath)
		fmt.Fprintf(tr.logf,
			`{"type":"result","subtype":"setup_failed","is_error":false,"duration_ms":%d,"total_cost_usd":%.6f,"num_turns":0,"src":"broker"}`+"\n",
			time.Since(tr.taskStart).Milliseconds(), cost)
		tr.outcome = "setup_failed"
		// Point at the setup log only when one was actually created — the
		// inconclusive paths that never launched a VM have no log file.
		hint := "drydock inspect " + tr.id + " — setup failed before the agent ran; no API budget spent"
		if _, lerr := os.Lstat(tr.setupLogPath()); lerr == nil {
			hint += " · setup log: " + tr.setupLogPath()
		}
		tr.sw.emit(map[string]any{"event": "result", "outcome": "setup_failed",
			"task_id": tr.id, "setup_status": s.Status,
			"reason":      setupFailReason(s),
			"duration_ms": time.Since(tr.taskStart).Milliseconds(), "cost_usd": cost,
			"hint": hint})
		return false
	}
	return true
}

// execSetup runs the profile's setup then readiness commands sequentially
// (fail-fast) and returns the evidence block. cancelled=true means tr.ctx was
// cancelled mid-run (kill or shutdown) — the caller emits the standard
// cancelled terminal.
func (tr *taskRun) execSetup(cfg SetupProfile) (s *trustbrief.SetupEvidence, cancelled bool) {
	b := tr.b
	// The egress posture is asserted only once a setup VM is actually about
	// to launch (below, after the log-open check succeeds): the inconclusive
	// paths that never ran a VM must not carry a posture claim for VMs that
	// never existed.
	s = &trustbrief.SetupEvidence{}

	all := make([][]string, 0, len(cfg.Setup)+len(cfg.Readiness))
	all = append(all, cfg.Setup...)
	all = append(all, cfg.Readiness...)
	if len(all) == 0 {
		// config.Validate forbids an empty profile, but Broker.Setup can be
		// populated programmatically too. Zero commands is zero evidence, and
		// zero evidence never reads as "passed" — and since setup has no
		// advisory mode, inconclusive fails the task closed.
		slog.Warn("setup: no commands configured; setup inconclusive (fails closed)", "task_id", tr.id)
		s.Status = trustbrief.SetupInconclusive
		return s, false
	}

	// Display-only log of the setup VMs' output: same defenses as every other
	// audit artifact (0600, O_NOFOLLOW). Nothing in it is ever parsed.
	logPath := tr.setupLogPath()
	logf, err := os.OpenFile(logPath,
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		slog.Warn("setup: could not open setup log; setup inconclusive (fails closed)",
			"task_id", tr.id, "err", err)
		s.Status = trustbrief.SetupInconclusive
		return s, false
	}
	// Double Close (explicit close before hashing below, then this defer on
	// early returns) is harmless on *os.File.
	defer logf.Close()

	// Every never-ran path is behind us: from here at least one setup VM
	// launches, so record the egress posture those VMs actually run with.
	// (Setup does have network access — squid-only, no gateway — unlike the
	// verifier's "denied".)
	s.Network = "egress-allowlisted"

	// The setup env: proxy/gateway vars only, NEVER the grant bearer. This is
	// half of fail-closed-before-spend — the other half is runSetup's
	// placement before the agent VM boots.
	setupEnv := buildSetupEnv(tr.proxyAuth, b.GatewayIP, b.ProxyPort)

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
		timeout = DefaultSetupTimeout
	}
	run := b.runAgent
	if run == nil {
		run = runContainer
	}

	cmds := make([]trustbrief.SetupCommand, 0, len(all))
	stop := false
	for _, argv := range all {
		if stop {
			// Fail fast: once a command failed (or timed out / errored), the
			// rest are recorded as skipped, never run.
			cmds = append(cmds, trustbrief.SetupCommand{Argv: argv, Status: trustbrief.VerifyCmdSkipped})
			continue
		}
		args := runner.BuildSetupArgs(runner.SetupSpec{
			TaskID:   tr.id,
			Network:  b.Network,
			ImageRef: b.ImageRef,
			StageDir: tr.st.WorkDir(),
			Env:      setupEnv,
			Argv:     argv,
			MemoryGB: 4,
			CPUs:     4,
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
		sc := trustbrief.SetupCommand{Argv: argv, DurationMs: dur.Milliseconds()}
		var ee *exec.ExitError
		switch {
		case rerr == nil:
			sc.Status = trustbrief.VerifyCmdPassed
		case tr.ctx.Err() != nil:
			// Operator kill or brokerd shutdown mid-setup: force-delete the
			// possibly-surviving VM and hand the cancelled terminal to runSetup.
			tr.deleteSetupVM()
			s.Status = trustbrief.SetupInconclusive
			return s, true
		case outCap.exceeded():
			// We cancelled the command ourselves: its output crossed the host
			// cap. Evidence is absent, not failing — VerifyCmdError (which
			// still fails the task closed; setup has no advisory mode).
			tr.deleteSetupVM()
			slog.Warn("setup: command output exceeded the host cap; terminated",
				"task_id", tr.id, "cap_mib", maxTaskOutputBytes>>20)
			fmt.Fprintf(logf, "\n[drydock] setup output exceeded the %d MiB host cap; command terminated\n",
				maxTaskOutputBytes>>20)
			sc.Status = trustbrief.VerifyCmdError
			stop = true
		case timedOut:
			// --rm covers a graceful exit; on timeout the VM may survive, so
			// force-remove it (best effort), same as the agent-run path.
			tr.deleteSetupVM()
			sc.Status = trustbrief.VerifyCmdTimedOut
			stop = true
		case errors.As(rerr, &ee):
			sc.Status = trustbrief.VerifyCmdFailed
			sc.ExitCode = ee.ExitCode()
			stop = true
		default:
			// Infra failure (container CLI missing, image missing): evidence
			// absent, not a repo failure — but it still fails closed.
			slog.Warn("setup: command run error", "task_id", tr.id, "err", rerr)
			sc.Status = trustbrief.VerifyCmdError
			stop = true
		}
		cmds = append(cmds, sc)
	}
	s.Commands = cmds

	// Overall verdict from the per-command statuses alone: all passed →
	// passed; ANYTHING else (failed, timed_out, error, and their skipped
	// tails) → failed. Setup has no advisory tier and no inconclusive middle
	// ground once VMs have run: a workspace that didn't finish preparing is a
	// workspace that isn't ready.
	s.Status = trustbrief.SetupPassed
	for _, c := range cmds {
		if c.Status != trustbrief.VerifyCmdPassed {
			s.Status = trustbrief.SetupFailed
			break
		}
	}

	// Digest the log host-side by re-reading the closed file, so LogSHA256
	// attests to the bytes on disk, not to what we think we wrote.
	if cerr := logf.Close(); cerr != nil {
		slog.Warn("setup: close setup log", "task_id", tr.id, "err", cerr)
	}
	if sum, herr := fileSHA256NoFollow(logPath); herr == nil {
		s.LogSHA256 = sum
	} else {
		slog.Warn("setup: could not hash setup log", "task_id", tr.id, "err", herr)
	}
	return s, false
}

// setupFailReason names the first command that stopped the setup phase, for
// the terminal event's reason field.
func setupFailReason(s *trustbrief.SetupEvidence) string {
	for _, c := range s.Commands {
		switch c.Status {
		case trustbrief.VerifyCmdPassed, trustbrief.VerifyCmdSkipped:
			continue
		case trustbrief.VerifyCmdFailed:
			return fmt.Sprintf("setup command failed (exit %d): %s", c.ExitCode, strings.Join(c.Argv, " "))
		case trustbrief.VerifyCmdTimedOut:
			return "setup command timed out: " + strings.Join(c.Argv, " ")
		default:
			return "setup command error: " + strings.Join(c.Argv, " ")
		}
	}
	return "setup produced no usable evidence (status " + s.Status + ")"
}

// deleteSetupVM force-deletes this task's setup VM through the same bounded
// container-delete path runSandbox uses for task-<id>. Best effort: a
// survivor is reaped at the next brokerd boot by the orphan reaper.
func (tr *taskRun) deleteSetupVM() {
	if err := tr.b.forceDelete(runner.SetupContainerName(tr.id)); err != nil {
		slog.Warn("force-delete of setup VM failed; reaped at next brokerd boot",
			"task_id", tr.id, "err", err)
	}
}

// setupLogPath is the display-only setup-VM output log for this task.
func (tr *taskRun) setupLogPath() string {
	return filepath.Join(tr.b.AuditRoot, tr.id+".setup.log")
}
