package gateway

import (
	"testing"
	"time"
)

func TestSpendLedger_RollingWindow(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	l := newSpendLedger(24 * time.Hour)
	l.add("anthropic", 1.0, now.Add(-48*time.Hour)) // outside window
	l.add("anthropic", 2.0, now.Add(-1*time.Hour))  // inside
	l.add("anthropic", 3.0, now.Add(-2*time.Hour))  // inside
	if got := l.windowed("anthropic", now); got != 5.0 {
		t.Errorf("windowed = %v, want 5.0 (old entry aged out)", got)
	}
}

func TestSpendLedger_TotalMode(t *testing.T) {
	now := time.Now()
	l := newSpendLedger(0) // total mode: no decay
	l.add("openai", 1.0, now.Add(-1000*time.Hour))
	l.add("openai", 2.5, now)
	if got := l.windowed("openai", now); got != 3.5 {
		t.Errorf("total-mode windowed = %v, want 3.5 (no decay)", got)
	}
}

func TestSpendLedger_PerVendorIsolation(t *testing.T) {
	now := time.Now()
	l := newSpendLedger(time.Hour)
	l.add("anthropic", 10.0, now)
	l.add("openai", 1.0, now)
	if got := l.windowed("openai", now); got != 1.0 {
		t.Errorf("openai windowed = %v, want 1.0 (anthropic must not bleed in)", got)
	}
	if got := l.windowed("google", now); got != 0 {
		t.Errorf("unknown vendor = %v, want 0", got)
	}
}
