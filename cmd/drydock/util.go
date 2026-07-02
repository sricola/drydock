package main

import (
	"fmt"
	"os"
	"time"

	"drydock/internal/config"
)

// auditDir returns the audit root brokerd writes to, read from the same place
// brokerd reads it so the CLI (ui, tasks, status, prune) always looks where the
// diffs/logs/history actually are. Resolution order, all via config.Load:
//  1. AUDIT_ROOT env override
//  2. ~/.drydock/config.yaml -> audit_root (the operator's configured value)
//  3. config.Defaults() (~/.drydock/audit)
//
// Reading config.audit_root — rather than guessing from directory state — is
// what keeps the UI in sync with the broker: a configured audit_root that
// differs from the default used to make diffs/logs/history silently 404.
func auditDir() string {
	if v := os.Getenv("AUDIT_ROOT"); v != "" {
		return v
	}
	if cfg, err := config.Load(config.DefaultPath()); err == nil {
		return cfg.AuditRoot
	}
	return "/tmp/broker/audit" // config unreadable — last-resort legacy default
}

// diffPath returns AUDIT_ROOT/<id>.diff. Used by `review` and `kill`.
func diffPath(id string) string { return auditDir() + "/" + id + ".diff" }

// auditPath returns AUDIT_ROOT/<id>.jsonl. Used by `logs` and `tasks`.
func auditPath(id string) string { return auditDir() + "/" + id + ".jsonl" }

// relAge renders a timestamp as a short human age ("3m", "2h", "5d"). Used by
// `tasks` and `status` so the columns fit.
func relAge(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}

// shortDur renders a millisecond duration as "1.2s", "37s", "2m13s".
func shortDur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", ms)
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	default:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}

// die prints an error and exits 1.
func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "drydock: "+format+"\n", a...)
	os.Exit(1)
}

// tty is true if stdout looks like an interactive terminal. We emit ANSI
// colors only when true; piping to a log file otherwise produces "[32m" garbage.
var tty = func() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}()

// humanBytes formats a byte count as a human-readable string (e.g. "1.2KB",
// "37MB"). Used by prune and submit_render for display.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	const units = "KMGTPE"
	if exp >= len(units) {
		exp = len(units) - 1
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), units[exp])
}
