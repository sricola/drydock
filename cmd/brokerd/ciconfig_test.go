package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"drydock/internal/broker"
	"drydock/internal/config"
)

// The feature ships DARK: with the stock config, applyCIConfig must leave
// b.CIWatch false — and b.CIWatch is the one gate brokerd consults before
// calling StartCIWatch, so false here means the watcher never starts, no
// <id>.ci.json marker is ever written, and the host's gh credential is never
// exercised on a timer.
func TestApplyCIConfig_OffByDefaultSoTheWatcherNeverStarts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := config.Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b := &broker.Broker{}
	applyCIConfig(b, cfg.CI)
	if b.CIWatch {
		t.Fatal("stock config enabled the CI watch; brokerd would start the watcher on a plain install")
	}
	// Durations still map (they are inert while the watch is off, but must not
	// silently become something else if the operator flips watch: true later).
	if b.CIPollInterval != config.DefaultCIPollInterval {
		t.Errorf("CIPollInterval = %v, want %v", b.CIPollInterval, config.DefaultCIPollInterval)
	}
	if b.CIWatchTimeout != config.DefaultCIWatchTimeout {
		t.Errorf("CIWatchTimeout = %v, want %v", b.CIWatchTimeout, config.DefaultCIWatchTimeout)
	}
	// The BOUNDED RETRY ships off too, and this is the assertion that keeps it
	// off: broker.CIMaxAttempts <= 0 is the single gate the retry decision
	// consults, so 0 here means an observed CI failure can never enqueue a
	// child and can never spend a fresh task_budget_usd unattended.
	if b.CIMaxAttempts != 0 {
		t.Errorf("CIMaxAttempts = %d, want 0 (bounded retry ships off)", b.CIMaxAttempts)
	}
}

// The shipped sample config must also leave the watch off — it is the file an
// operator gets from `drydock init` / `make install`.
func TestApplyCIConfig_ShippedSampleLeavesWatchOff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := config.Load("../../config/config.yaml")
	if err != nil {
		t.Fatalf("Load shipped config: %v", err)
	}
	b := &broker.Broker{}
	applyCIConfig(b, cfg.CI)
	if b.CIWatch {
		t.Error("the shipped config/config.yaml enables the CI watch; it must ship off")
	}
	if cfg.CI.MaxAttempts != 0 || b.CIMaxAttempts != 0 {
		t.Errorf("the shipped config sets ci.max_attempts = %d (broker: %d), want 0",
			cfg.CI.MaxAttempts, b.CIMaxAttempts)
	}
}

func TestApplyCIConfig_MapsEveryField(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := filepath.Join(t.TempDir(), "c.yaml")
	body := "network: x\ngateway_ip: 1.2.3.4\n" +
		"ci:\n  watch: true\n  poll_interval: 30s\n  watch_timeout: 2h\n  max_attempts: 3\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b := &broker.Broker{}
	applyCIConfig(b, cfg.CI)
	if !b.CIWatch {
		t.Error("ci.watch: true did not reach broker.CIWatch")
	}
	if b.CIPollInterval != 30*time.Second {
		t.Errorf("CIPollInterval = %v, want 30s", b.CIPollInterval)
	}
	if b.CIWatchTimeout != 2*time.Hour {
		t.Errorf("CIWatchTimeout = %v, want 2h", b.CIWatchTimeout)
	}
	// B1 declared ci.max_attempts INERT — it never reached the broker. B2 wires
	// it, and this is the assertion that says so: without it the bound would
	// read 0 at runtime whatever the operator configured, i.e. the retry would
	// be permanently off and the config key a lie.
	if b.CIMaxAttempts != 3 {
		t.Errorf("CIMaxAttempts = %d, want 3 — ci.max_attempts did not reach the broker", b.CIMaxAttempts)
	}
}

// A zero duration must stay zero rather than being pre-resolved here: the
// broker's own fallback owns "unset", so mapping a 0 to a concrete value in
// two places is how the two layers drift.
func TestApplyCIConfig_ZeroDurationsPassThroughUntouched(t *testing.T) {
	b := &broker.Broker{CIPollInterval: 99 * time.Second, CIWatchTimeout: 99 * time.Hour,
		CIMaxAttempts: 99}
	applyCIConfig(b, config.CIConfig{Watch: true})
	if b.CIPollInterval != 0 || b.CIWatchTimeout != 0 {
		t.Errorf("zero durations not passed through: poll=%v timeout=%v", b.CIPollInterval, b.CIWatchTimeout)
	}
	// The retry bound is OVERWRITTEN, not merged: an unset ci.max_attempts must
	// clear a previously-set field rather than leave a stale bound behind.
	if b.CIMaxAttempts != 0 {
		t.Errorf("CIMaxAttempts = %d, want 0 — an unset max_attempts must clear it", b.CIMaxAttempts)
	}
}
