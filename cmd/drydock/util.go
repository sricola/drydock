package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"drydock/internal/config"
)

// auditDir returns the audit root brokerd writes to. Mirrors brokerd's default
// so the CLI can find the same files without a config dance. Resolution order:
//   1. AUDIT_ROOT env override
//   2. ~/.drydock/audit (current default; matches config.Defaults())
//   3. /tmp/broker/audit (legacy fallback — see below)
//
// The legacy fallback exists so an operator who upgraded from drydock < v0.1.4
// can still run `drydock tasks` / `drydock logs` against their old history
// without setting AUDIT_ROOT by hand. New tasks land in the new default; the
// fallback only triggers if the new path is empty/missing.
func auditDir() string {
	if v := os.Getenv("AUDIT_ROOT"); v != "" {
		return v
	}
	if d := config.Dir(); d != "" {
		preferred := filepath.Join(d, "audit")
		if hasAudit(preferred) {
			return preferred
		}
		// Fall back to the legacy /tmp path if it has files and the new
		// one is empty — surfaces old history during the transition.
		if !hasAudit(preferred) && hasAudit("/tmp/broker/audit") {
			return "/tmp/broker/audit"
		}
		return preferred
	}
	return "/tmp/broker/audit"
}

// hasAudit returns true if dir exists and contains at least one .jsonl file.
// Used by auditDir's legacy-fallback path so we only divert to /tmp when the
// operator actually has history there.
func hasAudit(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".jsonl" {
			return true
		}
	}
	return false
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
