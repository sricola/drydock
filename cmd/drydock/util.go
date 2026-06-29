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
