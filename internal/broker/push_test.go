package broker

import (
	"context"
	"errors"
	"testing"
	"time"
)

// scriptStage returns a queued error per PushBranch call; "" means success.
type scriptStage struct {
	commitErr error
	pushErrs  []error // consumed per PushBranch call
	branches  []string
	commits   int
}

func (s *scriptStage) Commit(branch, message string) error { s.commits++; return s.commitErr }
func (s *scriptStage) PushBranch(local, remote string) error {
	s.branches = append(s.branches, remote)
	if len(s.branches)-1 < len(s.pushErrs) {
		return s.pushErrs[len(s.branches)-1]
	}
	return nil
}

var errTransient = errors.New("fatal: Could not resolve host: github.com")
var errNonFF = errors.New("! [rejected] (non-fast-forward)")
var errAuth = errors.New("fatal: Authentication failed")

func cfg0() pushRetry { return pushRetry{MaxRetries: 3, Backoff: 0, FreshBranchTries: 2} }

func TestPushWithRecovery_TransientThenSuccess(t *testing.T) {
	st := &scriptStage{pushErrs: []error{errTransient, errTransient, nil}}
	branch, attempts, _, err := pushWithRecovery(context.Background(), st, "abc", "m", cfg0())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if attempts != 3 || branch != "agent/abc" {
		t.Errorf("attempts=%d branch=%q, want 3 / agent/abc", attempts, branch)
	}
}

func TestPushWithRecovery_FreshBranchOnCollision(t *testing.T) {
	st := &scriptStage{pushErrs: []error{errNonFF, nil}} // base rejected, -2 ok
	branch, attempts, _, err := pushWithRecovery(context.Background(), st, "abc", "m", cfg0())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if branch != "agent/abc-2" || attempts != 2 {
		t.Errorf("branch=%q attempts=%d, want agent/abc-2 / 2", branch, attempts)
	}
}

func TestPushWithRecovery_AuthStopsImmediately(t *testing.T) {
	st := &scriptStage{pushErrs: []error{errAuth, nil}} // would succeed on retry, but auth is terminal
	_, attempts, reason, err := pushWithRecovery(context.Background(), st, "abc", "m", cfg0())
	if err == nil {
		t.Fatal("want error for auth failure")
	}
	if reason != reasonAuth || attempts != 1 {
		t.Errorf("reason=%q attempts=%d, want auth / 1", reason, attempts)
	}
}

func TestPushWithRecovery_TransientExhausted(t *testing.T) {
	st := &scriptStage{pushErrs: []error{errTransient, errTransient, errTransient, errTransient}}
	_, attempts, reason, err := pushWithRecovery(context.Background(), st, "abc", "m", cfg0())
	if err == nil || reason != reasonTransient {
		t.Fatalf("want transient failure, got reason=%q err=%v", reason, err)
	}
	if attempts != 4 { // 1 initial + 3 retries
		t.Errorf("attempts=%d, want 4", attempts)
	}
}

func TestPushWithRecovery_CommitFailureIsTerminal(t *testing.T) {
	st := &scriptStage{commitErr: errors.New("nothing to commit")}
	_, attempts, _, err := pushWithRecovery(context.Background(), st, "abc", "m", cfg0())
	if err == nil || attempts != 0 || len(st.branches) != 0 {
		t.Errorf("commit failure should be terminal with no push attempt; attempts=%d branches=%v err=%v", attempts, st.branches, err)
	}
}

func TestPushWithRecovery_CtxCancelDuringBackoff(t *testing.T) {
	st := &scriptStage{pushErrs: []error{errTransient, nil}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled up front: the backoff after the first failure returns immediately
	_, _, _, err := pushWithRecovery(ctx, st, "abc", "m", pushRetry{MaxRetries: 3, Backoff: time.Hour, FreshBranchTries: 2})
	if err == nil {
		t.Fatal("want error when ctx is cancelled during backoff")
	}
}

func TestBackoffFor_CapsShiftNoOverflow(t *testing.T) {
	base := time.Second
	if got := backoffFor(base, 0); got != base {
		t.Errorf("backoffFor try0 = %v, want %v", got, base)
	}
	if got := backoffFor(base, 2); got != 4*base {
		t.Errorf("backoffFor try2 = %v, want %v", got, 4*base)
	}
	// A large try must not overflow to a non-positive duration.
	if got := backoffFor(base, 100); got <= 0 {
		t.Errorf("backoffFor try100 = %v, want a large positive duration (no overflow)", got)
	}
}
