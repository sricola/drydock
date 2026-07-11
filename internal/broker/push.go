package broker

import (
	"context"
	"fmt"
	"time"
)

// pushStage is the subset of the stage used by push recovery.
type pushStage interface {
	Commit(branch, message string) error
	PushBranch(localBranch, remoteBranch string) error
}

// pushRetry bounds recovery. A zero field disables that path.
type pushRetry struct {
	MaxRetries       int           // transient-failure retries
	Backoff          time.Duration // base for exponential backoff (backoff << n)
	FreshBranchTries int           // alternate remote branch names on collision
}

// pushWithRecovery commits once, then pushes agent/<taskID> with bounded
// recovery: transient failures retry the same remote with exponential backoff;
// a branch-name collision retries to agent/<taskID>-2, -3, ...; auth, protected,
// and unknown failures stop immediately. It returns the remote branch that
// landed (nil err) or the classified reason and last error on failure. attempts
// counts PushBranch calls made.
func pushWithRecovery(ctx context.Context, st pushStage, taskID, message string, cfg pushRetry) (string, int, pushReason, error) {
	base := "agent/" + taskID
	if err := st.Commit(base, message); err != nil {
		return "", 0, reasonUnknown, err
	}
	remote := base
	attempts, transientTry, freshTry := 0, 0, 0
	for {
		attempts++
		err := st.PushBranch(base, remote)
		if err == nil {
			return remote, attempts, "", nil
		}
		reason := classifyPushError(err.Error())
		switch reason {
		case reasonTransient:
			if transientTry >= cfg.MaxRetries {
				return "", attempts, reason, err
			}
			if !sleepCtx(ctx, backoffFor(cfg.Backoff, transientTry)) {
				return "", attempts, reason, ctx.Err()
			}
			transientTry++
		case reasonNonFastForward:
			if freshTry >= cfg.FreshBranchTries {
				return "", attempts, reason, err
			}
			freshTry++
			remote = fmt.Sprintf("%s-%d", base, freshTry+1) // -2, -3, ...
		default: // reasonAuth, reasonProtected, reasonUnknown
			return "", attempts, reason, err
		}
	}
}

// backoffFor returns base doubled per retry (base<<try), with the shift capped
// so a large try count cannot overflow time.Duration (int64) into a
// non-positive value that would defeat the backoff.
func backoffFor(base time.Duration, try int) time.Duration {
	const maxShift = 30 // base<<30 is ~12 days for a 1s base; plenty
	if try > maxShift {
		try = maxShift
	}
	return base << try
}

// sleepCtx waits d, or returns false promptly if ctx is cancelled first. A
// non-positive d returns true immediately (used by tests with Backoff 0).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
