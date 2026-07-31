package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"drydock/internal/audit"
	"drydock/internal/broker"
	"drydock/internal/provider"
)

// BOOT RECONCILIATION for the global usage ceiling (docs/superpowers/plans/
// 2026-07-31-global-ceiling.md, G3 + Task 3).
//
// The durable ledger (internal/broker/globalledger.go) is AUTHORITATIVE; this
// sweep cross-checks it against the audit trail and ADDS what a crash lost.
// The one gap it exists to close is narrow and real: a task's terminal ledger
// write is the LAST thing its lifecycle does (globalrecord.go), so a task
// killed between its audit terminal and that write leaves full evidence in the
// audit and nothing in the ledger. Without this sweep that task's start — and
// its spend — silently stop counting against the ceiling forever.
//
// THE DIRECTION IS FIXED: on any ambiguity this over-counts, never under. That
// governs every decision below, and it is why the sweep only ever ADDS. It
// never removes a ledger entry that has no matching audit trace: a trace can be
// pruned (`drydock prune`), moved, or simply predate the retention an operator
// chose, while the ledger's own record of a task start is the thing the ceiling
// is measured on. Deleting a counted start because its evidence aged out would
// hand an operator a way to reset the ceiling by clearing the audit dir.
//
// ---------------------------------------------------------------------------
// WHAT IT DOES BETTER THAN seedAggregateFromAudit, the sibling boot sweep.
// ---------------------------------------------------------------------------
//
// seedAggregateFromAudit reseeds the per-vendor aggregate cap from the same
// directory, and it has three weaknesses this must not inherit:
//
//  1. IT KEYS ON FILE MTIME for both its window cutoff AND the timestamp it
//     seeds with. mtime is when the file was last TOUCHED, not when the task
//     ran — the CI work already had to paper over that with
//     appendPreservingMTime. Here the entry timestamp is the BROKER-AUTHORED
//     ended_at_ms on the metrics row (added for exactly this). mtime survives
//     only as (a) a cheap, SOUND pre-filter — a trace last written before the
//     floor cannot describe a task that ended after it — and (b) a last resort
//     for a trace written before ended_at_ms existed, where nothing better is
//     recoverable. It is never preferred over a broker-authored instant.
//
//  2. IT RETURNS SILENTLY ON A ReadDir ERROR, which fails OPEN: the cap is
//     seeded from nothing and admits freely. For a ceiling that is backwards
//     (G2). A read failure here DEGRADES the ledger instead, which globalcap.go
//     turns into a refusal on every enforced limb — "I don't know" means "no".
//
//  3. IT SKIPS ROWS WITH NO src, including TerminateStuckAudits' synthetic
//     `interrupted` line — so a task the daemon crashed under has its REAL
//     broker-metered spend (sitting in the broker row just above) rendered
//     invisible. This scans back to the last src=="broker" row
//     (audit.LastBrokerResult), so the crash case is exactly the case it reads
//     correctly.
//
// G4 is absolute here too: the recovered USD comes only from a broker-authored
// result row. audit.TotalCost is never consulted — it does not filter Src and
// would let an agent-printed total_cost_usd deflate the ceiling.
//
// ---------------------------------------------------------------------------
// THE CASES IT HAS TO HANDLE.
// ---------------------------------------------------------------------------
//
//	a task in the audit but not the ledger  -> ADDED (the whole point)
//	a ledger entry with no audit trace      -> LEFT ALONE (see above)
//	a task that was running at the crash    -> ADDED. pruneOrphanTasks ran
//	                                           TerminateStuckAudits first, so
//	                                           the trace has an honest terminal;
//	                                           its start counts, and its dollars
//	                                           are recorded as UNTRUSTED when no
//	                                           broker row survives, rather than
//	                                           invented.
//	a task resumed at the diff gate         -> ADDED here, and resumePush's own
//	                                           terminal write later is deduped by
//	                                           task id. This is what converges
//	                                           globalcap.go's documented residual
//	                                           (such a task was admitted in a
//	                                           previous process and holds no
//	                                           in-flight claim in this one).
//	clock movement                          -> the lookback anchor is the EARLIER
//	                                           of now and the ledger's own newest
//	                                           event, so a forward jump cannot
//	                                           push the whole window into the
//	                                           future and make the sweep add
//	                                           nothing. See reconcileFloorMs.
//	total-mode replay                       -> BOUNDED. See reconcileFloorMs.

const (
	// globalReconcileTotalLookback bounds a TOTAL-mode sweep's walk.
	//
	// Total mode (global_window == 0) has no decay, so a naive sweep would
	// replay the entire audit history on every boot. That is not merely slow:
	// total-mode compaction FOLDS all but the newest entries into a checkpoint,
	// which destroys their task ids, so GlobalLedger.Has answers false for them
	// and every boot would re-add tasks already inside the checkpoint's sums —
	// an over-count that repeats and grows without bound. reconcileFloorMs
	// clamps to the ledger's own oldest addressable entry for exactly that; this
	// constant is the second bound, for a ledger too young to have folded
	// anything (a fresh install, or the ceiling just switched on) where the
	// clamp is 0 and something still has to stop the walk at the top of a
	// multi-year audit dir. A week is far longer than any crash window and far
	// shorter than any plausible history.
	globalReconcileTotalLookback = 7 * 24 * time.Hour

	// globalReconcileMTimeSlack is the margin on the mtime pre-filter. The
	// filter is sound without it (a file last written before the floor cannot
	// hold a task that ended after it), but mtime comes from the filesystem's
	// clock rather than the broker's, and the cost of being generous is reading
	// a few extra traces while the cost of being tight is dropping a real one.
	globalReconcileMTimeSlack = time.Hour
)

// globalReconcileIDRE mirrors the broker's own task-id grammar. A file in the
// audit root whose name is not a task id is not a task trace, and a name is
// never trusted before it is matched — the queuestore idiom.
var globalReconcileIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// reconcileGlobalLedger cross-checks the durable global ledger against the
// audit trail and records everything the ledger is missing. nowMs is the
// caller's clock (the broker's own seam is unexported, and taking it as a
// parameter keeps the sweep deterministic under test).
//
// It is a no-op when the ceiling has no store, which is the stock install.
//
// MUST run before anything can start a task — before the dispatcher and before
// the HTTP listener — or the first admissions of the new process are decided
// against an under-counted ledger. See its call site in main.
func reconcileGlobalLedger(b *broker.Broker, nowMs int64) {
	l := b.GlobalLedger
	if l == nil {
		return
	}
	floor := reconcileFloorMs(l, nowMs)
	mtimeFloor := floor - globalReconcileMTimeSlack.Milliseconds()

	dir, err := os.ReadDir(b.AuditRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return // an install that has never run a task has nothing to reconcile
		}
		// FAIL CLOSED (G2). We cannot enumerate the evidence, so we do not know
		// how many starts or how many dollars the ledger is missing — not just
		// the dollars. Degrade marks BOTH limbs as lower bounds, which
		// globalcap.go turns into a refusal for whichever limbs are enforced.
		// An install with the ceiling off is untouched.
		l.Degrade(fmt.Sprintf(
			"the global ledger could not be reconciled at boot: the audit directory %s could not be read (%v), so recorded usage is a lower bound",
			b.AuditRoot, err))
		slog.Warn("global ledger: boot reconciliation could not read the audit directory; the ceiling will refuse enforced limbs",
			"audit_root", b.AuditRoot, "err", err)
		return
	}

	var (
		batch      []broker.GlobalEntry
		unreadable int
		untrusted  int
	)
	for _, de := range dir {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(de.Name(), ".jsonl")
		if !globalReconcileIDRE.MatchString(id) {
			continue
		}
		// The ledger is authoritative: a task it already knows about is already
		// counted, and Record would suppress the duplicate anyway. Checking here
		// keeps the sweep from opening thousands of traces it has no use for.
		if l.Has(id) {
			continue
		}
		var mtimeMs int64
		if info, ierr := de.Info(); ierr == nil {
			mtimeMs = info.ModTime().UnixMilli()
		}
		if mtimeMs > 0 && mtimeMs < mtimeFloor {
			continue // sound cheap filter; never used as the entry's timestamp
		}

		e, ok, readable := reconcileEntryFromAudit(b, filepath.Join(b.AuditRoot, de.Name()), id, mtimeMs, nowMs)
		if !readable {
			unreadable++
		}
		if !ok {
			continue
		}
		if e.EndedAtMs <= floor {
			continue // outside the lookback: it cannot affect either limb
		}
		if !e.USDTrusted && e.Metered {
			untrusted++
		}
		batch = append(batch, e)
	}

	// ONE fsync for the whole sweep, not one per entry: Record fsyncs per call
	// (~4ms), which is right for a single task terminal and ruinous for a boot
	// sweep that may recover thousands. RecordBatch has identical per-entry
	// semantics, including the id dedupe.
	if len(batch) > 0 {
		added, rerr := l.RecordBatch(nowMs, batch)
		if rerr != nil {
			slog.Warn("global ledger: boot reconciliation could not durably write every recovered entry; they are counted in memory",
				"added", added, "err", rerr)
		}
		slog.Info("global ledger: boot reconciliation recovered task terminals missing from the ledger",
			"added", added, "scanned_missing", len(batch), "usd_untrusted", untrusted, "path", l.Path())
	}
	if unreadable > 0 {
		// Each unreadable trace was still recorded as a START above (never
		// under-count the limb that bounds subscription mode), but its dollars
		// are unknowable, so say so out loud rather than letting the USD limb
		// quietly report a smaller number than the truth.
		l.Degrade(fmt.Sprintf(
			"the global ledger could not be fully reconciled at boot: %d audit trace(s) under %s could not be read, so their spend is unknown and recorded usage is a lower bound",
			unreadable, b.AuditRoot))
		slog.Warn("global ledger: boot reconciliation could not read some audit traces; the ceiling will refuse enforced limbs",
			"count", unreadable, "audit_root", b.AuditRoot)
	}
}

// reconcileFloorMs is the oldest instant the sweep will look back to.
//
// THE ANCHOR IS THE EARLIER OF the caller's now AND the ledger's own newest
// real event. That mirrors globalledger's pruning anchor, deliberately, because
// the two operations have opposite risks and the same defence: pruning is
// destructive and must not trust a clock that jumped FORWARD, while this sweep
// is purely additive and its failure mode is adding NOTHING. Without the anchor,
// a forward clock jump (an NTP correction, a misconfigured host) would put the
// entire lookback window in the future, the sweep would find nothing to
// reconcile, and a crash-lost terminal would be lost for good. Anchoring to the
// ledger's own history means the sweep always covers at least the span
// compaction has not yet deleted. A BACKWARDS jump needs no defence: it moves
// the floor earlier, which reconciles more — the over-counting direction.
//
// WINDOWED MODE stops at the window: an entry outside it cannot affect either
// limb, so recovering it would be pure cost.
//
// TOTAL MODE has no window, so it takes the LATER of a fixed lookback and the
// ledger's OLDEST ADDRESSABLE ENTRY. The second clamp is the important one and
// it is Task 1's fold horizon showing through: total-mode compaction folds older
// entries into a checkpoint that preserves their sums but destroys their ids,
// so Has answers false for them and replaying anything at or before the fold
// boundary would re-add tasks already counted — on every boot, forever. Bounded
// under-counting of history older than the ledger's own memory is the correct
// trade against an unbounded, repeating over-count; a task that ran before the
// oldest entry the ledger can still name either predates the ledger or is
// already inside the checkpoint's totals.
func reconcileFloorMs(l *broker.GlobalLedger, nowMs int64) int64 {
	anchor := nowMs
	if ev := l.NewestEventMs(); ev > 0 && ev < anchor {
		anchor = ev
	}
	if w := l.WindowMs(); w > 0 {
		return anchor - w
	}
	floor := anchor - globalReconcileTotalLookback.Milliseconds()
	if oldest := l.Usage(nowMs).OldestMs; oldest > floor {
		floor = oldest
	}
	return floor
}

// reconcileEntryFromAudit builds the ledger entry for one audit trace.
//
// ok is false when there is nothing to record. readable is false when the trace
// could not be read at all — in which case ok is still TRUE and a start-only
// entry is returned, because the file's existence under a valid task id is
// itself evidence that a task ran, and dropping it would under-count the limb
// that bounds subscription mode. The caller degrades the USD side separately.
func reconcileEntryFromAudit(b *broker.Broker, path, id string, mtimeMs, nowMs int64) (e broker.GlobalEntry, ok, readable bool) {
	e = broker.GlobalEntry{
		Kind:   broker.GlobalEntryTask,
		TaskID: id,
		Src:    broker.GlobalSrcReconcile,
	}
	// O_NOFOLLOW: a planted <id>.jsonl -> elsewhere must not feed the ceiling
	// substituted numbers, the same rule every other audit read follows.
	f, oerr := audit.OpenRead(path)
	if oerr != nil {
		if os.IsNotExist(oerr) {
			return e, false, true // raced a prune; there is nothing to read
		}
		e.EndedAtMs = mtimeMs
		if e.EndedAtMs <= 0 {
			e.EndedAtMs = nowMs
		}
		e.Outcome = "unknown"
		// Metered is left false and USDTrusted false: we know a task started,
		// and we know nothing at all about its dollars.
		return e, true, false
	}
	defer f.Close()

	meta := audit.ReadMetaFile(f)
	agent := audit.TaskAgentFile(f)
	if agent == "" {
		agent = b.DefaultAgent
	}
	m, hasMetrics := audit.LastMetricsFile(f)
	res, hasBrokerResult := audit.LastBrokerResultFile(f)

	// The vendor on the metrics row is broker-authored and records what the
	// task ACTUALLY resolved to; re-resolving the agent name is the fallback for
	// a trace with no metrics row, and it can disagree with history if the
	// operator has since changed agents.
	vendor := m.Vendor
	if vendor == "" {
		vendor, _ = provider.VendorForAgent(agent)
	}

	// THE TIMESTAMP, broker-authored first. mtime is the last resort only —
	// see this file's header on seedAggregateFromAudit's weakness.
	switch {
	case hasMetrics && m.EndedAtMs > 0:
		e.EndedAtMs = m.EndedAtMs
	case mtimeMs > 0:
		e.EndedAtMs = mtimeMs
	default:
		e.EndedAtMs = nowMs
	}

	e.Agent = agent
	e.Vendor = vendor
	e.Auth = "api_key"
	if meta.Subscription {
		e.Auth = "subscription"
	}
	// Same signal the live terminal path and writeBrief use, plus the trace's
	// own recorded auth mode: a subscription run meters at $0 by construction
	// however the CURRENT config is written, and this trace may predate a
	// config change.
	e.Metered = vendor != "" && !b.UnmeteredVendors[vendor] && !meta.Subscription
	// G4: BROKER-AUTHORED COST ONLY. Never audit.TotalCost, which returns the
	// last result row of any authorship. A trace with no broker row (a genuine
	// mid-run kill) yields an untrusted $0: the start still counts, and the
	// dollars are declared unknown rather than invented. G7 names the
	// task-start limb as the backstop for precisely this.
	if hasBrokerResult && res.TotalCostUSD > 0 {
		e.USD = res.TotalCostUSD
		e.USDTrusted = e.Metered
	} else if hasBrokerResult {
		e.USDTrusted = e.Metered // a broker-authored, believable zero
	}
	switch {
	case hasMetrics && m.Outcome != "":
		e.Outcome = m.Outcome
	case hasBrokerResult && res.Subtype != "":
		e.Outcome = res.Subtype
	default:
		e.Outcome = "unknown"
	}
	return e, true, true
}
