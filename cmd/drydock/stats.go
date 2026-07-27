package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"drydock/internal/stats"
)

// runStats aggregates the audit dir into run metrics. Like `tasks`, it reads
// AUDIT_ROOT directly: brokerd does not need to be running.
func runStats(args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	since := fs.String("since", "30d", "window (e.g. 7d, 2w, 720h); 0 = everything")
	by := fs.String("by", "", "group by: agent | vendor | repo | day | week")
	asJSON := fs.Bool("json", false, "emit the report as JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: drydock stats [--since 30d] [--by agent|vendor|repo|day|week] [--json]

Aggregates recent runs from the audit dir: outcome rates, duration and
gate-wait percentiles, spend, and egress-widen frequency.`)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	d, err := parseRetention(*since)
	if err != nil {
		die("--since: %v", err)
	}
	if err := writeStats(os.Stdout, auditDir(), d, *by, *asJSON); err != nil {
		die("stats: %v", err)
	}
}

// writeStats is the testable core: collect, aggregate, render to w.
func writeStats(w io.Writer, dir string, since time.Duration, by string, asJSON bool) error {
	cutoff := time.Time{}
	if since > 0 {
		cutoff = time.Now().Add(-since)
	}
	samples, orphans, skipped := stats.Collect(dir, cutoff)
	rep := stats.Report{
		Since: cutoff, Overall: stats.Summarize(samples),
		OrphanWidens: orphans, SkippedFiles: skipped,
	}
	if by != "" {
		groups, err := stats.GroupBy(samples, by)
		if err != nil {
			return err
		}
		rep.Groups, rep.GroupBy = groups, by
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	renderStats(w, rep)
	return nil
}

// renderStats writes rep as a compact plain-text report: an overall block
// (outcomes, durations, gate waits, spend, widens, footnotes) and, when the
// report was grouped, a fixed-width table beneath it (matching runTasks's
// Fprintf style).
func renderStats(w io.Writer, rep stats.Report) {
	if rep.Since.IsZero() {
		fmt.Fprintf(w, "tasks: %d (all time)\n", rep.Overall.Tasks)
	} else {
		fmt.Fprintf(w, "tasks: %d (since %s)\n", rep.Overall.Tasks, rep.Since.Format("2006-01-02"))
	}

	renderOutcomes(w, rep.Overall)

	fmt.Fprintf(w, "dur p50/p95: %s\n", durPair(rep.Overall.DurP50Ms, rep.Overall.DurP95Ms, rep.Overall.Tasks-rep.Overall.PreMetricsTasks))
	fmt.Fprintf(w, "approval wait p50/p95: %s\n", gateWaitPair(rep.Overall.ApprovalWaitP50Ms, rep.Overall.ApprovalWaitP95Ms))
	fmt.Fprintf(w, "egress wait p50/p95: %s\n", gateWaitPair(rep.Overall.EgressWaitP50Ms, rep.Overall.EgressWaitP95Ms))

	renderSpend(w, rep.Overall)
	renderWidens(w, rep.Overall, rep.OrphanWidens)
	renderFootnotes(w, rep.Overall, rep.SkippedFiles)

	if len(rep.Groups) > 0 {
		renderGroups(w, rep)
	}
}

// renderOutcomes prints one line per outcome present, newest/most-common
// ordering doesn't matter here so we just walk the map; rates are rounded to
// the nearest percent.
func renderOutcomes(w io.Writer, s stats.Summary) {
	for _, outcome := range []string{"ok", "error", "push_failed", "interrupted", "running"} {
		n, ok := s.Outcomes[outcome]
		if !ok || n == 0 {
			continue
		}
		rate := 0
		if s.Tasks > 0 {
			rate = (n*100 + s.Tasks/2) / s.Tasks
		}
		fmt.Fprintf(w, "  %s: %d (%d%%)\n", outcome, n, rate)
	}
}

// durPair renders "p50/p95" for a duration pair, or "-" when there were no
// samples with a recorded duration (n counts samples that have metrics/duration).
func durPair(p50, p95 int64, n int) string {
	if n <= 0 {
		return "-"
	}
	return shortDur(p50) + "/" + shortDur(p95)
}

// gateWaitPair renders "p50/p95" for a gate-wait pair, or "-" when neither
// percentile has a nonzero value (i.e. no engaged-gate samples).
func gateWaitPair(p50, p95 int64) string {
	if p50 == 0 && p95 == 0 {
		return "-"
	}
	return shortDur(p50) + "/" + shortDur(p95)
}

// renderSpend prints the spend line. Unmetered (subscription) tasks are never
// folded into the dollar total; their count is appended as a parenthetical.
func renderSpend(w io.Writer, s stats.Summary) {
	fmt.Fprintf(w, "spend: $%.2f total, $%.2f/day", s.SpendUSD, s.SpendPerDayUSD)
	if s.UnmeteredTasks > 0 {
		fmt.Fprintf(w, " (+%d unmetered subscription task(s))", s.UnmeteredTasks)
	}
	fmt.Fprintln(w)
}

// renderWidens prints the egress-widen line, appending the orphan
// (requested-but-never-ran) count when nonzero.
func renderWidens(w io.Writer, s stats.Summary, orphanWidens int) {
	fmt.Fprintf(w, "egress widens: %d requested, %d approved", s.WidenRequested, s.WidenApproved)
	if orphanWidens > 0 {
		fmt.Fprintf(w, ", %d never ran (denied/cancelled at gate)", orphanWidens)
	}
	fmt.Fprintln(w)
}

// renderFootnotes prints the pre-metrics and skipped-file footnotes, each
// only when nonzero. The singular form must read "1 task predates metrics".
func renderFootnotes(w io.Writer, s stats.Summary, skippedFiles int) {
	if s.PreMetricsTasks > 0 {
		verb := "predate"
		if s.PreMetricsTasks == 1 {
			verb = "predates"
		}
		plural := "s"
		if s.PreMetricsTasks == 1 {
			plural = ""
		}
		fmt.Fprintf(w, "%d task%s %s metrics (timing columns partial)\n", s.PreMetricsTasks, plural, verb)
	}
	if skippedFiles > 0 {
		plural := "s"
		if skippedFiles == 1 {
			plural = ""
		}
		fmt.Fprintf(w, "%d unreadable file%s skipped\n", skippedFiles, plural)
	}
}

// renderGroups prints the fixed-width per-dimension table, mirroring
// runTasks's fmt.Printf column style.
func renderGroups(w io.Writer, rep stats.Report) {
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%-16s  %5s  %6s  %8s  %8s  %10s\n", strings.ToUpper(rep.GroupBy), "TASKS", "OK%", "DUR p50", "APPR p50", "SPEND")
	for _, g := range rep.Groups {
		okRate := 0
		if g.Tasks > 0 {
			okRate = (g.Outcomes["ok"]*100 + g.Tasks/2) / g.Tasks
		}
		dur := "-"
		if g.Tasks-g.PreMetricsTasks > 0 {
			dur = shortDur(g.DurP50Ms)
		}
		wait := "-"
		if g.ApprovalWaitP50Ms > 0 {
			wait = shortDur(g.ApprovalWaitP50Ms)
		}
		spend := "-"
		if g.Tasks-g.UnmeteredTasks > 0 {
			spend = fmt.Sprintf("$%.2f", g.SpendUSD)
		}
		fmt.Fprintf(w, "%-16s  %5d  %5d%%  %8s  %8s  %10s\n",
			g.Key, g.Tasks, okRate, dur, wait, spend)
	}
}
