package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"drydock/internal/atomicfile"
)

// This file is the DURABLE GLOBAL LEDGER: the crash-safe, rolling-window record
// of task usage that the global ceiling (docs/superpowers/plans/
// 2026-07-31-global-ceiling.md) is enforced against. It is the store only —
// nothing here reads config, refuses a task, or writes an entry on a task
// terminal. Enforcement is globalcap.go (Task 2); recording and boot
// reconciliation are the task-terminal path and cmd/brokerd (Task 3).
//
// It is the third application of the queuestore.go idiom — id validated before
// it is trusted, atomicfile.Write (temp + rename) for whole-file writes, 0600
// payload under a 0700 dir, O_NOFOLLOW on every open, an idempotent sweep of
// the stray ".tmp" a crashed atomic write leaves, a tolerant scan that warns
// rather than failing boot, and NO time.Now anywhere: every timestamp is
// caller-supplied through the broker's b.now/nowMs clock seam, so tests are
// deterministic. It inherits queuestore's one acknowledged gap: the containing
// directory is not fsync'd, so a power loss in the same instant as a rename can
// still lose the rename. The rename's atomicity is the primary guarantee and it
// matches the rest of the audit dir.
//
// ---------------------------------------------------------------------------
// STORAGE LAYOUT, and why it is ONE append-only file rather than one file per
// task.
// ---------------------------------------------------------------------------
//
// queuestore and cimarker both use one file per task. This one deliberately
// does not, and the four criteria break the other way:
//
//   (a) Crash safety. One-file-per-task wins narrowly: every write is a rename.
//       A single append-only file has one crash signature instead — a TORN
//       TRAILING LINE — and that signature is bounded (at most the last entry),
//       detectable (the line does not parse), and handled conservatively rather
//       than dropped (see "the tolerant-scan tension" below). Whole-file writes
//       (compaction, repair) still go through atomicfile, so the only
//       non-atomic operation in the file is a single-line append.
//
//   (b) Boot scan cost. One file per task loses decisively. A ceiling with a
//       24h window over a busy unattended install is thousands of entries; that
//       is thousands of open/read/close syscalls in a directory that ALSO holds
//       every task's .jsonl/.diff/.brief.json/.queue.json/.ci.json, scanned at
//       boot by five other consumers. One sequential read of one file is a
//       single syscall pattern regardless of entry count.
//
//   (c) Compaction. The plan calls this out explicitly — the record must not
//       grow without bound, the accumulation mistake Increment A made with
//       terminal queue records. A single file compacts by rewriting itself
//       atomically: one rename replaces N unlinks, and a crash mid-compaction
//       leaves the WHOLE old file (over-counting, the safe direction) or the
//       whole new one, never a half-pruned set. With one file per task,
//       compaction is N independent unlinks and a crash leaves an arbitrary
//       prefix of them applied.
//
//   (d) Concurrent writers. The dispatcher, the synchronous POST /tasks path
//       and the CI watcher can all terminal a task. One file per task would be
//       lock-free across distinct ids, but the query surface has to hold an
//       in-memory index anyway (see (b)), and that index needs a mutex either
//       way. So the single file costs nothing here: one mutex guards both the
//       index and the append.
//
// The file lives in its OWN SUBDIRECTORY, <AuditRoot>/global/ledger.jsonl,
// not directly in AuditRoot. This is load-bearing, not tidiness: five
// independent consumers glob "*.jsonl" in the audit root and parse each hit as
// a per-task trace (cmd/brokerd's seedAggregateFromAudit, cmd/drydock's
// tasks/status/prune, internal/stats, internal/webui's audit routes). A ledger
// named <AuditRoot>/*.jsonl would be parsed as a task trace by every one of
// them. All five skip directories, so a subdirectory is collision-free — and it
// also keeps the compaction ".tmp" out of their listings.
//
// G4 holds by construction: the directory is 0700 and the file 0600, both
// host-only, and every value in an entry is supplied by the broker. Nothing an
// agent can write is read here — in particular the ledger never consults
// audit.TotalCost, which does not filter Src and therefore admits an
// agent-forged total_cost_usd.

const (
	globalLedgerDirName  = "global"
	globalLedgerFileName = "ledger.jsonl"

	// globalLedgerMaxBytes bounds what a single boot scan will read. The file
	// is kept far below this by compaction; the cap exists so a corrupted or
	// hand-planted file cannot make boot allocate without limit. Hitting it is
	// itself a degradation (we did not see the whole ledger), never a silent
	// truncation.
	globalLedgerMaxBytes = 64 << 20

	// globalLedgerRawKeep is how many bytes of an unreadable line are preserved
	// inside its quarantine tombstone, for forensics.
	globalLedgerRawKeep = 512

	// globalLedgerMinCompactEntries is the floor below which compaction never
	// runs: rewriting a small file on every append would turn an O(1) append
	// into O(n). Above the floor the threshold doubles off the surviving count
	// (see compactLocked), so compaction is amortized O(1) per entry.
	globalLedgerMinCompactEntries = 512

	// globalLedgerTotalKeep is how many entries total mode (window == 0) keeps
	// individually addressable before folding older ones into a checkpoint.
	//
	// It exists because total mode has no decay: nothing ever ages out, so the
	// file would grow forever. Folding preserves the sums exactly, but it
	// destroys the folded ids — and boot reconciliation (Task 3) uses id
	// presence to decide whether an audited task is already counted. Keeping
	// the most recent 2000 terminals verbatim leaves that dedupe horizon orders
	// of magnitude larger than any crash window, while hard-bounding the file
	// at 2001 lines regardless of how long the broker has run.
	globalLedgerTotalKeep = 2000
)

// GlobalEntryKind distinguishes the three things a ledger line can be. The kind
// is explicit rather than inferred so a line whose shape is ambiguous is
// damage, not a guess.
type GlobalEntryKind string

const (
	// GlobalEntryTask is one task terminal: one task start, and (when metered)
	// one broker-metered USD figure.
	GlobalEntryTask GlobalEntryKind = "task"
	// GlobalEntryDamaged is a QUARANTINE TOMBSTONE standing in for a line that
	// could not be read. See "the tolerant-scan tension" on GlobalLedger.
	GlobalEntryDamaged GlobalEntryKind = "damaged"
	// GlobalEntryCheckpoint is a fold of many older entries into their exact
	// sums. Written only in total mode (window == 0).
	GlobalEntryCheckpoint GlobalEntryKind = "checkpoint"
)

// Src values record WHO authored an entry, so reconciliation can tell its own
// work from the task-terminal path's and an operator can see why a number is
// what it is.
const (
	// GlobalSrcTerminal: written by the task-terminal path as the task ended.
	GlobalSrcTerminal = "terminal"
	// GlobalSrcReconcile: written by boot reconciliation for a task found in
	// the audit but missing from the ledger — killed between its terminal and
	// its ledger write. G3's over-count-on-ambiguity direction.
	GlobalSrcReconcile = "reconcile"
	// GlobalSrcRepair: a quarantine tombstone minted by the boot scan.
	GlobalSrcRepair = "repair"
	// GlobalSrcCompact: a checkpoint minted by compaction.
	GlobalSrcCompact = "compact"
)

// GlobalEntry is one line of the ledger.
//
// The schema is deliberately complete for Tasks 2 and 3 so neither needs to
// change an on-disk format that will already have live entries written by an
// earlier daemon — the same discipline ciMarker used to carry Attempt/RetryOf
// before B2 needed them.
//
// A RETRY needs nothing special: it is a NEW task with a NEW id (queuestore's
// state machine is forward-only and acyclic, so a bounded retry is always a new
// item), so it is simply another GlobalEntryTask. Attempt/RetryOf are carried
// only so an operator can see WHICH chain consumed the ceiling; the counting
// itself treats a retry and its parent identically, per G1.
//
// RECONCILIATION needs three things, all present: TaskID as the dedupe key
// (Has), Src to distinguish a reconciled entry from a terminal-path one, and
// EndedAtMs as a broker-authored event time so the window is computed from when
// the task RAN — not from a file's mtime, the flaw seedAggregateFromAudit has
// and that appendPreservingMTime had to paper over.
type GlobalEntry struct {
	Kind GlobalEntryKind `json:"kind"`
	// TaskID is the broker's own 32-hex task id (newID). It is the dedupe key:
	// ids are 128-bit random, so two distinct starts can never collide, which
	// makes "already present" a safe suppression rather than an under-count.
	TaskID string `json:"task_id,omitempty"`
	// StartedAtMs/EndedAtMs are broker-authored unix-millis from the b.now
	// seam. EndedAtMs positions the entry in the rolling window and is
	// REQUIRED for a task entry; StartedAtMs is informational (0 = unknown).
	StartedAtMs int64 `json:"started_at_ms,omitempty"`
	EndedAtMs   int64 `json:"ended_at_ms"`
	// Vendor and Agent are the resolved lane. The USD limb is cross-vendor by
	// design (G1) — with N vendors, today's per-vendor aggregate_budget_usd is
	// really an N x ceiling — but the vendor is kept so an operator can see
	// where the money went and so Task 4's headroom surface can break it down.
	Vendor string `json:"vendor,omitempty"`
	Agent  string `json:"agent,omitempty"`
	// Auth mirrors audit.Metrics.Auth ("api_key" | "subscription") for parity
	// with the audit trail. Metered is the SEMANTIC the ceiling uses: false
	// means the lane carries no USD metering at all (a subscription lane, or a
	// priceless openai-compat lane — the b.UnmeteredVendors set), so its USD is
	// $0 by construction and not by measurement.
	Auth    string `json:"auth,omitempty"`
	Metered bool   `json:"metered,omitempty"`
	// USD is BROKER-METERED spend only: the gateway lease's SpentUSD, parsed by
	// the broker from the proxied response body. Never audit.TotalCost, which
	// does not filter Src and so admits an agent-forged figure (G4).
	//
	// USDTrusted says whether THIS figure is believable. It is false for an
	// unmetered lane, for a task whose metering was never established, and for
	// a reconciled entry whose cost could not be recovered from a broker-
	// authored result row. An untrusted figure is NEVER summed into the USD
	// limb — it is reported separately (GlobalUsage.UntrustedUSD) so an
	// operator sees it without the ceiling pretending to have measured it.
	USD        float64 `json:"usd,omitempty"`
	USDTrusted bool    `json:"usd_trusted,omitempty"`
	// Outcome mirrors audit.Metrics.Outcome. Informational: the ceiling counts
	// every start regardless of how it ended.
	Outcome string `json:"outcome,omitempty"`
	// Attempt and RetryOf mirror the same-named Task/ciMarker fields: Attempt
	// counts RETRIES (an operator-submitted task is 0), RetryOf is the
	// immediate parent id.
	Attempt int    `json:"attempt,omitempty"`
	RetryOf string `json:"retry_of,omitempty"`
	Src     string `json:"src,omitempty"`

	// --- checkpoint-only (GlobalEntryCheckpoint) ---
	// Starts/Damaged/UntrustedUSD carry the folded counts; USD carries the
	// folded trustworthy sum. FoldedThroughMs is the newest EndedAtMs folded in.
	Starts          int     `json:"starts,omitempty"`
	Damaged         int     `json:"damaged,omitempty"`
	UntrustedUSD    float64 `json:"untrusted_usd,omitempty"`
	FoldedThroughMs int64   `json:"folded_through_ms,omitempty"`

	// --- damaged-only (GlobalEntryDamaged) ---
	// Raw is the head of the unreadable line, preserved for forensics. It
	// marshals as base64, so bytes that are not valid UTF-8 round-trip intact
	// and re-reading a tombstone can never itself produce damage.
	Raw []byte `json:"raw,omitempty"`
}

// GlobalUsage is the ceiling's query answer: both G1 limbs computed over a
// caller-supplied `now`, plus everything Task 2 needs to be honest about what
// it does and does not know.
type GlobalUsage struct {
	// USD is the sum of TRUSTWORTHY broker-metered spend in window — the
	// global_budget_usd limb.
	USD float64
	// UntrustedUSD is in-window spend the ledger declines to count: unmetered
	// lanes and unrecoverable figures. Display and diagnostics only.
	UntrustedUSD float64
	// Starts is the count of task starts in window — the global_max_tasks limb.
	// Retries and their parents count alike. Quarantined entries count here:
	// a line exists because a task ran.
	Starts int
	// Entries is how many ledger records the window covers (a checkpoint is
	// one record covering many starts), for operator display.
	Entries int
	// Damaged is how many in-window entries are quarantined — starts whose
	// dollars are unknown.
	Damaged int
	// Degraded says the ledger could not be fully read, so USD is a LOWER
	// BOUND. Under G2 the global check must refuse a start when a limb it is
	// enforcing is degraded: for a ceiling, "I don't know" means "no".
	//
	// Note the asymmetry, and it is deliberate: the Starts limb is NOT degraded
	// by quarantine, because a damaged line is still counted as a start. Only
	// the USD limb loses information. A deployment with global_budget_usd off
	// therefore keeps working through damage, and one with it on refuses until
	// the damage ages out of the window (or, in total mode, until an operator
	// repairs the file — the reason string names it).
	Degraded       bool
	DegradedReason string
	// WindowMs is the configured window; 0 is total mode (no decay).
	WindowMs int64
	// OldestMs/NewestMs bracket the in-window entries (0 when empty).
	OldestMs int64
	NewestMs int64
}

// GlobalLedger is the durable ledger's in-process handle: the whole file held
// in memory behind one mutex, with appends and compactions written through to
// disk.
//
// ---------------------------------------------------------------------------
// THE TOLERANT-SCAN vs NEVER-UNDER-COUNT TENSION, and how it is resolved.
// ---------------------------------------------------------------------------
//
// queuestore and cimarker both skip an unreadable file with a warning, because
// one corrupt item must not strand every other queued task. That reflex is
// exactly WRONG for a ceiling: a skipped entry is spend and a start that
// silently stop counting, which loosens the cap — and G2/G3 fix the direction
// on ambiguity as over-counting, never under.
//
// The resolution is that tolerance and never-under-count are only in conflict
// if "tolerate" means "discard". Here it does not. The scan never discards a
// datum:
//
//  1. A line that cannot be read becomes a QUARANTINE TOMBSTONE
//     (GlobalEntryDamaged) rather than being skipped. The tombstone counts as
//     ONE TASK START — which is not a guess: the line exists because something
//     wrote an entry, and an entry is written per task terminal. So the Starts
//     limb, the one that bounds subscription mode and cannot be under-reported
//     by a metering gap (G7), NEVER under-counts from corruption.
//
//  2. Its dollars are genuinely unknown, and the honest response to an unknown
//     dollar figure is not to invent a conservative one — any number picked
//     would be fiction. So the USD limb reports itself DEGRADED, and G2 turns
//     that into a refusal at admission. That is the same answer over-counting
//     would give (the cap is treated as reached) without pretending to a
//     measurement.
//
//  3. The tombstone is stamped with the caller's `now` at the moment of
//     repair, which is the LATEST the damaged entry could possibly have been
//     written and therefore the most conservative position in the window: it
//     ages out no sooner than the real entry would have.
//
//  4. The repair is written back through atomicfile, which makes it IDEMPOTENT
//     — a tombstone is a well-formed entry, so the next boot re-reads it as
//     data instead of re-damaging it and minting a second one. Without the
//     write-back, each restart would add another tombstone and the over-count
//     would grow without bound, which is its own kind of dishonesty.
//
//  5. Damage that prevents reading the file AT ALL (a symlinked path, an I/O
//     error, a file larger than globalLedgerMaxBytes) cannot be quarantined
//     line-by-line, so the whole store is marked degraded and — critically —
//     is NEVER rewritten. Compaction and repair both refuse on a degraded
//     store, because a rewrite would replace bytes we could not read and turn
//     "we cannot see the ledger" into "the ledger is empty".
//
// The net rule: information is only ever ADDED by the scan, never removed, and
// where information is irrecoverable the ledger says so out loud instead of
// returning a smaller number.
type GlobalLedger struct {
	mu       sync.Mutex
	path     string
	windowMs int64 // 0 = total mode

	entries []GlobalEntry
	byID    map[string]struct{}

	// loadErr is non-empty when the durable file could not be fully read. It is
	// sticky for the life of the handle: the missing bytes do not come back,
	// and pretending otherwise after one successful append would be worse than
	// staying loud.
	loadErr string
	// compactAt is the entry count at which the next compaction fires. It
	// doubles off the surviving count after each run, so compaction is
	// amortized O(1) per recorded entry.
	compactAt int
}

// GlobalLedgerPath is the ledger's location under an audit root. Exported so
// operator surfaces can name the file an operator may need to inspect.
func GlobalLedgerPath(auditRoot string) string {
	return filepath.Join(auditRoot, globalLedgerDirName, globalLedgerFileName)
}

// OpenGlobalLedger loads the durable ledger under auditRoot and returns a
// handle over it. window == 0 is total mode (no decay); nowMs is the caller's
// clock, used to stamp any quarantine tombstone the scan mints and to drive the
// boot compaction.
//
// It ALWAYS returns a usable handle when auditRoot is non-empty, even on error,
// because the caller's job on error is to fail closed — and a nil store gives a
// caller nothing to ask.
//
// The error and GlobalUsage.Degraded are deliberately NOT the same signal. The
// error is everything the operator should be told, including a boot compaction
// that could not be written to a full or read-only disk. Degraded is the
// narrower claim that the NUMBERS are untrustworthy, which is what G2 turns
// into a refusal — a failed compaction does not make them untrustworthy, since
// the in-memory state is complete either way.
func OpenGlobalLedger(auditRoot string, window time.Duration, nowMs int64) (*GlobalLedger, error) {
	if auditRoot == "" {
		return nil, errors.New("globalledger: empty audit root")
	}
	l := &GlobalLedger{
		path:      GlobalLedgerPath(auditRoot),
		byID:      map[string]struct{}{},
		compactAt: globalLedgerMinCompactEntries,
	}
	if window > 0 {
		l.windowMs = window.Milliseconds()
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		l.loadErr = "global ledger directory is unavailable: " + err.Error()
		return l, fmt.Errorf("globalledger: %w", err)
	}
	// Sweep the stray ".tmp" a crashed atomicfile.Write leaves, exactly as
	// removeQueueItem does. Idempotent; a missing file is success.
	if err := os.Remove(l.path + ".tmp"); err != nil && !os.IsNotExist(err) {
		slog.Warn("globalledger: could not remove a stale temp file", "path", l.path+".tmp", "err", err)
	}

	lines, truncated, err := readGlobalLedgerLines(l.path)
	if err != nil {
		// We cannot see the durable record. Do NOT rewrite anything.
		l.loadErr = fmt.Sprintf("global ledger at %s could not be read (%v); usage is unknown", l.path, err)
		return l, fmt.Errorf("globalledger: read %s: %w", l.path, err)
	}
	damaged := l.parseLocked(lines, nowMs)
	if truncated {
		l.loadErr = fmt.Sprintf("global ledger at %s exceeds %d bytes and was not read in full; usage is a lower bound",
			l.path, globalLedgerMaxBytes)
	}
	// A repair is needed iff the scan quarantined something; compaction may be
	// needed independently. Both are the same atomic rewrite.
	if damaged > 0 {
		slog.Warn("globalledger: quarantined unreadable ledger lines",
			"path", l.path, "count", damaged,
			"note", "counted as task starts; the USD limb is a lower bound until they age out")
	}
	if err := l.compactLocked(nowMs, damaged > 0); err != nil {
		// The in-memory state is correct and complete; only the rewrite failed.
		// Report it, but the handle stays usable and honest.
		slog.Warn("globalledger: could not rewrite the ledger at boot", "path", l.path, "err", err)
		return l, fmt.Errorf("globalledger: rewrite %s: %w", l.path, err)
	}
	if l.loadErr != "" {
		return l, errors.New(l.loadErr)
	}
	return l, nil
}

// Path is the ledger file this handle writes.
func (l *GlobalLedger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// LoadError reports why the durable file could not be read IN FULL, or "" when
// it was. It exists because GlobalUsage.Degraded deliberately conflates two
// different losses, and the ceiling (globalcap.go) has to tell them apart to
// gate its refusal PER LIMB:
//
//   - A QUARANTINED LINE sets Degraded but not this: the tombstone still counts
//     as one task start, so the Starts limb has lost nothing and must keep
//     working. Only the USD figure became a lower bound.
//   - BYTES WE NEVER READ (a symlinked path, an I/O error, a file over the
//     read cap) set this: the starts inside them are missing from the count
//     too, so BOTH limbs are lower bounds and both must refuse.
//
// Without the distinction the ceiling would either brick a task-limb-only
// deployment on one corrupt byte, or admit freely against a ledger it could not
// open at all. A nil store answers with a non-empty reason: unavailable is the
// strongest form of unreadable.
func (l *GlobalLedger) LoadError() string {
	if l == nil {
		return "the global ledger is unavailable; usage cannot be determined"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.loadErr
}

// WindowMs is the configured rolling window in milliseconds; 0 is total mode.
// Boot reconciliation reads it to bound how far back it has to look: outside
// the window an audited task cannot affect either limb.
func (l *GlobalLedger) WindowMs() int64 {
	if l == nil {
		return 0
	}
	return l.windowMs
}

// Record durably appends e. nowMs is the caller's clock: it fills a missing
// EndedAtMs (so an entry is never dropped for want of a timestamp, while
// nothing here reads a wall clock) and drives the amortized compaction check.
//
// Idempotent on TaskID. A task id is 128-bit random, so two distinct starts can
// never share one; suppressing a duplicate therefore cannot under-count, and it
// does stop a crash-replayed terminal from double-counting.
//
// If the durable append fails, the entry STAYS in memory and the error is
// returned. The task really did run — losing it from the running total because
// the disk was full would under-count the ceiling, which is the one direction
// G2/G3 forbid. Over-counting for the life of this process is the safe answer,
// and the caller learns durability failed. It also self-heals: compaction
// rewrites the whole in-memory set, so the next successful rewrite makes the
// entry durable after all.
func (l *GlobalLedger) Record(nowMs int64, e GlobalEntry) error {
	if l == nil {
		return errors.New("globalledger: nil ledger")
	}
	if e.Kind == "" {
		e.Kind = GlobalEntryTask
	}
	if e.Kind != GlobalEntryTask {
		return fmt.Errorf("globalledger: Record only accepts task entries, got %q", e.Kind)
	}
	// The id shape is validated because it is an IDENTITY and a dedupe key, not
	// because a path is built from it (this store builds no per-id paths). An
	// unvalidated id would let a malformed record poison the dedupe set and
	// would land unchecked in a durable artifact.
	if !queueIDRE.MatchString(e.TaskID) {
		return fmt.Errorf("globalledger: invalid task id %q", e.TaskID)
	}
	if e.EndedAtMs <= 0 {
		e.EndedAtMs = nowMs
	}
	if e.Src == "" {
		e.Src = GlobalSrcTerminal
	}
	e.Raw = nil // never set on a task entry

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, dup := l.byID[e.TaskID]; dup {
		return nil
	}
	l.entries = append(l.entries, e)
	l.byID[e.TaskID] = struct{}{}

	appendErr := l.appendLocked(e)
	if err := l.compactLocked(nowMs, false); err != nil && appendErr == nil {
		appendErr = err
	}
	return appendErr
}

// Has reports whether a task id is already recorded. Boot reconciliation (Task
// 3) uses it to add only the tasks the ledger is missing, so an audit trail
// replayed against a healthy ledger cannot double-count.
//
// A nil store answers false, which makes reconciliation ADD rather than skip —
// the over-counting direction, matching G3.
func (l *GlobalLedger) Has(taskID string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.byID[taskID]
	return ok
}

// Usage computes both G1 limbs over the caller-supplied now. It is on the
// admission path, so it is a single pass over the in-memory entries with no
// allocation and no I/O; compaction keeps that slice at the order of the
// in-window entry count.
//
// It is deliberately NON-DESTRUCTIVE, unlike gateway.spendLedger.windowed which
// prunes in place. An admission check must never be able to lose data: pruning
// belongs to compaction, where it is written through atomically and can be
// reasoned about on its own.
//
// A nil store answers "degraded", which under G2 is a refusal.
func (l *GlobalLedger) Usage(nowMs int64) GlobalUsage {
	if l == nil {
		return GlobalUsage{Degraded: true, DegradedReason: "the global ledger is unavailable; usage cannot be determined"}
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	u := GlobalUsage{WindowMs: l.windowMs}
	// The cutoff is STRICTLY BEFORE the window: an entry counts iff
	// EndedAtMs > now-window. This matches gateway.spendLedger.windowed's
	// e.ts.After(cutoff) exactly, so the two ledgers cannot disagree about
	// whether a given instant is inside a window of the same length. In total
	// mode there is no cutoff at all.
	var cutoff int64
	windowed := l.windowMs > 0
	if windowed {
		cutoff = nowMs - l.windowMs
	}
	for i := range l.entries {
		e := &l.entries[i]
		if windowed && e.EndedAtMs <= cutoff {
			continue
		}
		u.Entries++
		if u.OldestMs == 0 || e.EndedAtMs < u.OldestMs {
			u.OldestMs = e.EndedAtMs
		}
		if e.EndedAtMs > u.NewestMs {
			u.NewestMs = e.EndedAtMs
		}
		switch e.Kind {
		case GlobalEntryTask:
			u.Starts++
			if e.USDTrusted {
				u.USD += e.USD
			} else {
				u.UntrustedUSD += e.USD
			}
		case GlobalEntryDamaged:
			// A line existed, so a task started. Its dollars are unknown, which
			// makes the USD limb a lower bound — see Degraded below.
			u.Starts++
			u.Damaged++
		case GlobalEntryCheckpoint:
			u.Starts += e.Starts
			u.Damaged += e.Damaged
			u.USD += e.USD
			u.UntrustedUSD += e.UntrustedUSD
		}
	}
	switch {
	case l.loadErr != "":
		u.Degraded, u.DegradedReason = true, l.loadErr
	case u.Damaged > 0:
		u.Degraded = true
		u.DegradedReason = fmt.Sprintf(
			"%d unreadable entr%s in the global ledger (%s) are counted as task starts but their spend is unknown; the USD total is a lower bound",
			u.Damaged, plural(u.Damaged, "y", "ies"), l.path)
	}
	return u
}

// Compact prunes and rewrites the durable file now. Callers do not need it —
// Record compacts on an amortized schedule and OpenGlobalLedger compacts at
// boot — but boot reconciliation and tests want it explicit.
func (l *GlobalLedger) Compact(nowMs int64) error {
	if l == nil {
		return errors.New("globalledger: nil ledger")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rewriteLocked(nowMs)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ---------------------------------------------------------------------------
// reading
// ---------------------------------------------------------------------------

// readGlobalLedgerLines reads the ledger file's raw lines. The open refuses
// symlinks (O_NOFOLLOW, parity with readQueueItem/readCIMarker) so a planted
// ledger -> elsewhere can neither feed the ceiling substituted numbers nor be
// clobbered by a compaction rewriting through it. A missing file is not an
// error: an install that has never run a task has no ledger.
//
// truncated is true when the file is larger than globalLedgerMaxBytes, which is
// a degradation (we did not see all of it), never a silent short read.
func readGlobalLedgerLines(path string) (lines [][]byte, truncated bool, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, globalLedgerMaxBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > globalLedgerMaxBytes {
		data, truncated = data[:globalLedgerMaxBytes], true
	}
	return bytes.Split(data, []byte("\n")), truncated, nil
}

// parseLocked turns raw lines into entries, quarantining anything it cannot
// read, and returns the number of quarantine tombstones minted. nowMs stamps
// each tombstone: the latest instant the damaged entry could have been written,
// and therefore the most conservative position in the rolling window.
func (l *GlobalLedger) parseLocked(lines [][]byte, nowMs int64) int {
	damaged := 0
	for _, raw := range lines {
		line := bytes.TrimRight(raw, "\r")
		if len(bytes.TrimSpace(line)) == 0 {
			continue // the trailing newline, or blank padding
		}
		e, ok := decodeGlobalEntry(line)
		if !ok {
			damaged++
			l.entries = append(l.entries, quarantine(line, nowMs))
			continue
		}
		if e.Kind == GlobalEntryTask {
			if _, dup := l.byID[e.TaskID]; dup {
				// Two lines for one 128-bit id can only be a replayed write, so
				// keeping the first cannot lose a distinct start.
				slog.Warn("globalledger: dropping a duplicate ledger entry", "task_id", e.TaskID)
				continue
			}
			l.byID[e.TaskID] = struct{}{}
		}
		l.entries = append(l.entries, e)
	}
	return damaged
}

// decodeGlobalEntry parses one line and checks that the decoded body is
// SELF-CONSISTENT. The cimarker lesson generalised: a body that fails its own
// invariants is not data, and here failing them is DAMAGE (quarantined, still
// counted as a start), never a silent skip.
func decodeGlobalEntry(line []byte) (GlobalEntry, bool) {
	var e GlobalEntry
	if err := json.Unmarshal(line, &e); err != nil {
		return e, false
	}
	switch e.Kind {
	case GlobalEntryTask:
		// The id must be a real task id: it is this entry's identity and the
		// reconciliation dedupe key, and it lands in a durable artifact.
		if !queueIDRE.MatchString(e.TaskID) {
			return e, false
		}
		// No timestamp means no position in the window. Admitting it would
		// create an entry that either never ages out or ages out immediately;
		// quarantining it gets the conservative one (stamped at repair time)
		// without inventing a history.
		if e.EndedAtMs <= 0 {
			return e, false
		}
	case GlobalEntryCheckpoint:
		if e.EndedAtMs <= 0 || e.Starts < 0 || e.Damaged < 0 {
			return e, false
		}
	case GlobalEntryDamaged:
		if e.EndedAtMs <= 0 {
			return e, false
		}
	default:
		// An unknown kind is not something to guess at.
		return e, false
	}
	return e, true
}

func quarantine(line []byte, nowMs int64) GlobalEntry {
	raw := line
	if len(raw) > globalLedgerRawKeep {
		raw = raw[:globalLedgerRawKeep]
	}
	return GlobalEntry{
		Kind:      GlobalEntryDamaged,
		EndedAtMs: nowMs,
		Src:       GlobalSrcRepair,
		Raw:       append([]byte(nil), raw...),
	}
}

// ---------------------------------------------------------------------------
// writing
// ---------------------------------------------------------------------------

// appendLocked appends one entry as a line. O_APPEND makes the write
// positionally atomic against other appenders; O_NOFOLLOW keeps parity with the
// read side; the fsync is what makes "recorded" mean "survives a power loss".
//
// A crash mid-write leaves a TORN TRAILING LINE — the single non-atomic
// operation in this file, and the reason the scan quarantines rather than
// skips.
func (l *GlobalLedger) appendLocked(e GlobalEntry) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(payload, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// compactLocked runs a rewrite when one is due: forced (a repair the scan owes
// the file) or because the entry count crossed the amortized threshold.
func (l *GlobalLedger) compactLocked(nowMs int64, force bool) error {
	if !force && len(l.entries) < l.compactAt {
		return nil
	}
	return l.rewriteLocked(nowMs)
}

// rewriteLocked prunes/folds the in-memory entries and writes the whole file
// through atomicfile (temp + rename).
//
// Crash safety: the rename is atomic, so a crash mid-compaction leaves either
// the WHOLE old file — which still holds every entry the new one would have
// kept, so it over-counts rather than losing anything — or the whole new one.
// A half-pruned state is unrepresentable. Nothing is double-counted either,
// because the new file is a strict subset-plus-fold of the old and the two
// never coexist.
//
// It REFUSES on a degraded store: rewriting bytes we could not read would turn
// "the ledger is unreadable" into "the ledger is empty", which is precisely the
// under-count G2/G3 forbid.
func (l *GlobalLedger) rewriteLocked(nowMs int64) error {
	if l.loadErr != "" {
		return fmt.Errorf("globalledger: refusing to rewrite a ledger that could not be read: %s", l.loadErr)
	}
	kept := l.pruneLocked(nowMs)
	var buf bytes.Buffer
	for _, e := range kept {
		payload, err := json.Marshal(e)
		if err != nil {
			return err
		}
		buf.Write(payload)
		buf.WriteByte('\n')
	}
	if err := atomicfile.Write(l.path, buf.Bytes(), 0o600); err != nil {
		return err
	}
	l.entries = kept
	l.byID = make(map[string]struct{}, len(kept))
	for _, e := range kept {
		if e.Kind == GlobalEntryTask {
			l.byID[e.TaskID] = struct{}{}
		}
	}
	// Double off the survivors so compaction is amortized O(1) per entry and a
	// ledger that is legitimately large does not rewrite itself on every append.
	l.compactAt = 2 * len(kept)
	if l.compactAt < globalLedgerMinCompactEntries {
		l.compactAt = globalLedgerMinCompactEntries
	}
	return nil
}

// pruneLocked returns the entries that survive compaction.
//
// Windowed mode drops what has aged out. Total mode has no decay, so instead it
// FOLDS all but the newest globalLedgerTotalKeep entries into one checkpoint
// carrying their exact sums — bounding the file at 2001 lines however long the
// broker runs, while leaving a dedupe horizon far larger than any crash window.
func (l *GlobalLedger) pruneLocked(nowMs int64) []GlobalEntry {
	if l.windowMs <= 0 {
		return foldTotalMode(l.entries, nowMs)
	}
	// Pruning is DESTRUCTIVE, so its clock is the EARLIER of the caller's now
	// and the ledger's own newest event. A clock that jumps backwards already
	// moves the cutoff back and prunes less (over-counting, the safe
	// direction); a clock that jumps FORWARD — an NTP correction, a
	// misconfigured host — would otherwise age out the entire durable record in
	// one pass and silently reset the ceiling to zero. Anchoring to the newest
	// recorded event means a forward jump can only ever delete entries that the
	// ledger's own history already says are old. Queries still use the caller's
	// raw now: reading a smaller in-window number is recoverable, deleting the
	// record is not.
	pruneNow := nowMs
	if ev := newestEventMs(l.entries); ev > 0 && ev < pruneNow {
		pruneNow = ev
	}
	cutoff := pruneNow - l.windowMs
	kept := make([]GlobalEntry, 0, len(l.entries))
	for _, e := range l.entries {
		if e.EndedAtMs > cutoff {
			kept = append(kept, e)
		}
	}
	return kept
}

// newestEventMs is the newest REAL event time in the ledger. Quarantine
// tombstones are excluded on purpose: they are stamped with a repair-time
// clock, so letting one set the pruning anchor would hand a bad clock the very
// influence pruneLocked exists to deny it.
func newestEventMs(entries []GlobalEntry) int64 {
	var newest int64
	for _, e := range entries {
		if e.Kind == GlobalEntryDamaged {
			continue
		}
		if e.EndedAtMs > newest {
			newest = e.EndedAtMs
		}
	}
	return newest
}

// foldTotalMode collapses all but the newest globalLedgerTotalKeep entries into
// a single checkpoint. Sums are preserved EXACTLY — a fold changes how the
// ledger is stored, never what it says.
func foldTotalMode(entries []GlobalEntry, nowMs int64) []GlobalEntry {
	if len(entries) <= globalLedgerTotalKeep {
		return entries
	}
	ordered := make([]GlobalEntry, len(entries))
	copy(ordered, entries)
	// Stable by event time so "newest" means newest, even if a reconciled entry
	// for an older task arrived after a newer one.
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].EndedAtMs < ordered[j].EndedAtMs })
	split := len(ordered) - globalLedgerTotalKeep
	cp := GlobalEntry{Kind: GlobalEntryCheckpoint, Src: GlobalSrcCompact, EndedAtMs: nowMs}
	for _, e := range ordered[:split] {
		switch e.Kind {
		case GlobalEntryTask:
			cp.Starts++
			if e.USDTrusted {
				cp.USD += e.USD
			} else {
				cp.UntrustedUSD += e.USD
			}
		case GlobalEntryDamaged:
			cp.Starts++
			cp.Damaged++
		case GlobalEntryCheckpoint:
			cp.Starts += e.Starts
			cp.Damaged += e.Damaged
			cp.USD += e.USD
			cp.UntrustedUSD += e.UntrustedUSD
		}
		if e.EndedAtMs > cp.FoldedThroughMs {
			cp.FoldedThroughMs = e.EndedAtMs
		}
	}
	// The checkpoint is positioned at the newest instant it folds, not at the
	// fold time: if this file is later read under a WINDOW (an operator turning
	// global_window from 0 to 24h), the fold must not claim to be newer than
	// the events in it. It stays in window until its own newest event ages out,
	// which keeps the whole historical total counted for a full window — an
	// over-count, the required direction.
	if cp.FoldedThroughMs > 0 {
		cp.EndedAtMs = cp.FoldedThroughMs
	}
	return append([]GlobalEntry{cp}, ordered[split:]...)
}
