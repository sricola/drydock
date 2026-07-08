package runner

import (
	"errors"
	"strings"
	"testing"
)

// fakeRun returns a run func that replays scripted results and records calls.
func fakeRun(t *testing.T, results map[string]struct {
	out string
	err error
}, calls *[]string) func(args ...string) (string, error) {
	t.Helper()
	return func(args ...string) (string, error) {
		key := strings.Join(args, " ")
		*calls = append(*calls, key)
		r, ok := results[key]
		if !ok {
			t.Fatalf("unexpected container call: %q", key)
		}
		return r.out, r.err
	}
}

func TestEnsureContainerSystem_AlreadyRunning(t *testing.T) {
	var calls []string
	run := fakeRun(t, map[string]struct {
		out string
		err error
	}{
		"network ls": {"NETWORK  SUBNET\ndefault  192.168.64.0/24\n", nil},
	}, &calls)
	started, err := EnsureContainerSystem(run, func(string) { t.Error("notify must not fire when already running") })
	if err != nil || started {
		t.Fatalf("got (started=%v, err=%v), want (false, nil)", started, err)
	}
	if len(calls) != 1 {
		t.Errorf("probe only, got calls %v", calls)
	}
}

func TestEnsureContainerSystem_DownThenStarted(t *testing.T) {
	var calls []string
	notified := false
	run := fakeRun(t, map[string]struct {
		out string
		err error
	}{
		"network ls":                           {"error: XPC connection error\n", errors.New("exit 1")},
		"system start --enable-kernel-install": {"", nil},
	}, &calls)
	started, err := EnsureContainerSystem(run, func(string) { notified = true })
	if err != nil || !started {
		t.Fatalf("got (started=%v, err=%v), want (true, nil)", started, err)
	}
	if !notified {
		t.Error("notify must fire before starting")
	}
	if len(calls) != 2 {
		t.Errorf("probe + start, got calls %v", calls)
	}
}

func TestEnsureContainerSystem_ProbeFailsForOtherReason(t *testing.T) {
	var calls []string
	run := fakeRun(t, map[string]struct {
		out string
		err error
	}{
		"network ls": {"permission denied\n", errors.New("exit 1")},
	}, &calls)
	started, err := EnsureContainerSystem(run, func(string) {})
	if err == nil || started {
		t.Fatalf("got (started=%v, err=%v), want error and no start", started, err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error should carry probe output: %v", err)
	}
	if len(calls) != 1 {
		t.Errorf("must NOT attempt start on a non-service failure, got %v", calls)
	}
}

func TestEnsureContainerSystem_StartFails(t *testing.T) {
	var calls []string
	run := fakeRun(t, map[string]struct {
		out string
		err error
	}{
		"network ls":                           {"cannot reach system service\n", errors.New("exit 1")},
		"system start --enable-kernel-install": {"kernel install refused\n", errors.New("exit 1")},
	}, &calls)
	_, err := EnsureContainerSystem(run, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "kernel install refused") {
		t.Fatalf("want start failure surfaced, got %v", err)
	}
}
