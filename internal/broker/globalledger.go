package broker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// This file is the DURABLE GLOBAL LEDGER: the crash-safe, rolling-window record
// of task usage that the global ceiling (docs/superpowers/plans/
// 2026-07-31-global-ceiling.md) is enforced against. It is the store only —
// nothing here reads config, refuses a task, or writes an entry on a task
// terminal. Enforcement is globalcap.go (Task 2); recording and boot
// reconciliation are the task-terminal path and cmd/brokerd (Task 3).
//
// It is the third application of the queuestore.go idiom — id validated before
// it is trusted, temp + rename for whole-file writes, 0600 payload under a 0700
// dir, O_NOFOLLOW on every open, an idempotent sweep of the stray ".tmp" a
// crashed atomic write leaves, a tolerant scan that warns rather than failing
// boot, and NO WALL CLOCK anywhere: every timestamp is caller-supplied through
// the broker's b.now/nowMs clock seam, so tests are deterministic.
//
// The one clock this file reads for itself is MONOTONIC, and it is a guard
// rather than a source: it can only ever pull a caller-supplied instant
// BACKWARDS, never mint one. See "THE WRITE CLOCK" below for why a store whose
// writes delete things cannot take a caller's wall clock on trust.
//
// It diverges from the idiom in ONE place, deliberately: whole-file writes go
// through writeLedgerFileDurable rather than internal/atomicfile, because
// atomicfile fsyncs neither the temp file nor the directory and so does not
// actually provide the "whole old file or whole new one" guarantee that
// rewriteLocked's crash-safety argument rests on. For a credential file that
// costs a re-mint; here it would be a silent total under-count of a safety
// ceiling. See writeLedgerFileDurable.
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
//       (compaction, repair) still go through one durable temp+rename, so the only
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
	// inside its quarantine tombstone, for forensics. The WHOLE pre-repair file
	// is also copied aside before a repair rewrite (see preserveDamagedLocked),
	// so this bound costs no unrecoverable evidence.
	globalLedgerRawKeep = 512

	// globalLedgerMaxTaskLineBytes is the largest a single task line can
	// plausibly be. It is used only to classify DAMAGE: a damaged region longer
	// than this cannot be one entry, so the "one damaged line is exactly one
	// task start" identity does not hold for it. A task line is ~250 bytes with
	// every optional field populated; 2 KiB is generous headroom.
	globalLedgerMaxTaskLineBytes = 2048

	// globalLedgerCheckpointCopies is how many identical copies of a checkpoint
	// line a rewrite writes, consecutively, at the head of the file.
	//
	// A checkpoint is the ONLY line in this file whose loss is unbounded: in
	// total mode it carries the folded Starts and USD of the entire history, so
	// one corrupted byte in it would silently reset the start limb — the limb
	// that is the only bound on a subscription lane — by the whole folded count,
	// permanently, because the repair rewrite would then persist the reset. Task
	// lines have no such property (losing one loses one start, and the tombstone
	// stands in for it exactly), so they are not duplicated.
	//
	// Three copies means a single-byte corruption, a torn line, or any damage
	// that does not hit all three is recovered EXACTLY rather than degraded. See
	// recoverCheckpointPrefix.
	globalLedgerCheckpointCopies = 3

	// globalLedgerMinCompactEntries is the floor below which compaction never
	// runs: rewriting a small file on every append would turn an O(1) append
	// into O(n). Above the floor the threshold doubles off the surviving count
	// (see compactLocked), so compaction is amortized O(1) per entry.
	globalLedgerMinCompactEntries = 512

	// globalLedgerMaxCompactSlack bounds how many entries may accumulate ABOVE
	// the surviving count before the next compaction fires. Without a bound,
	// compactAt doubles off the survivors forever and a ledger that legitimately
	// grew large would go arbitrarily long between rewrites — and a rewrite is
	// what makes a repair, a fold and a failed durable append heal.
	//
	// It is deliberately SLACK rather than an absolute entry ceiling. An
	// absolute ceiling has a cliff: the moment the surviving set reaches it,
	// every append is over threshold and pays a full O(n) rewrite, so the
	// amortization disappears precisely where the file is biggest. Bounding the
	// headroom keeps a fixed number of cheap appends between rewrites at any
	// size, which is amortized O(1) everywhere.
	globalLedgerMaxCompactSlack = 1 << 16

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
	// StartsUnknown (shared with the damaged kind below) rides along when any
	// folded entry carried it — see foldTotalMode.
	Starts          int     `json:"starts,omitempty"`
	Damaged         int     `json:"damaged,omitempty"`
	UntrustedUSD    float64 `json:"untrusted_usd,omitempty"`
	FoldedThroughMs int64   `json:"folded_through_ms,omitempty"`
	// Sum is a checksum over the checkpoint's own counts (checkpointSum). It is
	// REQUIRED on a checkpoint and verified on read, because a checkpoint is the
	// one line whose numbers are not reconstructible from anything else: a
	// corruption that lands inside a digit still parses as JSON, and without the
	// checksum the ledger would believe the corrupted count. A checkpoint whose
	// checksum is missing or wrong is DAMAGE, not data. It also identifies the
	// copies of one fold: identical copies share a Sum, which is how
	// recoverCheckpointPrefix collapses them.
	Sum string `json:"sum,omitempty"`

	// --- damaged (GlobalEntryDamaged) and checkpoint ---
	// Raw is the head of the unreadable line, preserved for forensics. It
	// marshals as base64, so bytes that are not valid UTF-8 round-trip intact
	// and re-reading a tombstone can never itself produce damage. Damaged-only.
	Raw []byte `json:"raw,omitempty"`
	// StartsUnknown says this entry is NOT reliably worth the exact start count
	// it reports.
	//
	// On a TOMBSTONE it means the destroyed bytes did not positively identify
	// themselves as a single task line (see damagedIsOneTaskStart): they may
	// have been a checkpoint (worth the whole folded history) or several entries
	// whose separating newlines were destroyed (worth N starts).
	//
	// On a CHECKPOINT it means at least one such tombstone was folded into it,
	// so the checkpoint's own Starts is a lower bound too. This field is on the
	// checkpoint for a reason that cost a fail-open: a fold with nowhere to put
	// the flag SILENTLY CONVERTED a fail-closed "I don't know" into a definite,
	// wrong, and now PERSISTED count, which is exactly the reset the whole
	// tombstone mechanism exists to prevent. It is covered by checkpointSum.
	//
	// Either way the start limb treats it as an "I don't know", which under G2
	// is a refusal — the only honest answer, since the alternative is to
	// under-count the one limb that bounds a subscription lane.
	StartsUnknown bool `json:"starts_unknown,omitempty"`
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
	// Note the asymmetry, and it is deliberate: the Starts limb is USUALLY not
	// degraded by quarantine, because an ordinary damaged task line is still
	// worth exactly one start. Only the USD limb loses information. A deployment
	// with global_budget_usd off therefore keeps working through damage, and one
	// with it on refuses until the damage ages out of the window (or, in total
	// mode, until an operator repairs the file — the reason string names it).
	// The exception is StartsDegraded below, where the identity does not hold.
	Degraded       bool
	DegradedReason string
	// StartsDegraded says the START count is a lower bound too, so the task-start
	// limb must refuse as well. It is deliberately much rarer than Degraded: an
	// ordinary quarantined task line is worth exactly one start and does not set
	// it. It IS set when a destroyed region could not be identified as a single
	// task line — a lost checkpoint (which carries the whole folded history of a
	// total-mode ledger) or a region that took its separating newlines with it
	// (which hides N entries behind one tombstone). Without it, one bad byte in
	// a checkpoint silently resets the start limb by the entire folded count,
	// and the repair rewrite then makes that reset permanent.
	StartsDegraded       bool
	StartsDegradedReason string
	// LoadErr is LoadError()'s answer as of THIS snapshot. It is carried in the
	// usage rather than fetched by a second call so the ceiling can decide from
	// one consistent view taken under a single lock hold: two calls are two
	// critical sections, and a terminal write landing between them is a task
	// that counts zero times.
	LoadErr string
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
//     ONE TASK START, which is a LOWER BOUND rather than an equality, and the
//     difference is load-bearing enough to state exactly. It is exact only for
//     a damaged TASK line that is still recognisable as one line. It is NOT
//     exact in two cases, and both are handled rather than hand-waved:
//
//     (a) A damaged CHECKPOINT. In total mode a checkpoint carries the folded
//     Starts of the entire history, so scoring it as one start would erase
//     N-1 of them — and the forced repair rewrite would then persist that
//     erasure. Defended twice over: a checkpoint is written in
//     globalLedgerCheckpointCopies identical copies with a checksum, so
//     ordinary damage is RECOVERED EXACTLY from a surviving copy; and if
//     every copy is destroyed the tombstone is marked StartsUnknown and the
//     START limb degrades (refuses) instead of counting a smaller number.
//
//     (b) A damaged REGION that took its newlines with it. Lines are split on
//     "\n", so a run of destroyed bytes spanning several entries collapses
//     into ONE tombstone standing for N entries. Such a tombstone does not
//     look like a single task line (damagedIsOneTaskStart), so it too is
//     marked StartsUnknown and degrades the start limb.
//
//     The net guarantee is therefore: corruption never yields a start count
//     that is silently too small. It yields either the exact count or an
//     explicit refusal.
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
//  4. The repair is written back durably (temp+fsync+rename), which makes it IDEMPOTENT
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
//  6. A repair rewrite PRESERVES THE BYTES IT REPLACES, copying the pre-repair
//     file aside first (preserveDamagedLocked). A repair is the one operation
//     here that destroys evidence — it overwrites the damaged region with a
//     tombstone — and history that is gone is not recoverable by any later
//     decision, so the original is kept for an operator to inspect or salvage.
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

	// --- the WRITE clock's monotonic guard ---
	//
	// See writeNowLocked. mono is a seam ONLY so a test can drive the guard
	// deterministically; production always gets the real monotonic reading
	// installed by OpenGlobalLedger. A nil mono disables the guard, which is
	// what a hand-built zero-value GlobalLedger gets — never a real one.
	mono          func() int64
	clockAnchored bool
	clockWall     int64
	clockMono     int64
	clockSkew     int64
}

// ---------------------------------------------------------------------------
// THE WRITE CLOCK, and why the ledger will not take one on trust.
// ---------------------------------------------------------------------------
//
// globalcap.go corrects the QUERY clock for a forward wall-clock jump, and
// documents why. That correction was necessary and not sufficient: every WRITE
// — Record, RecordBatch, Compact — also carries a caller-supplied `now`, and a
// write is where the ledger DESTROYS things. pruneLocked deletes every entry
// older than now-window from memory AND from the file, so a single Record made
// with a clock that has jumped forward by more than the window wipes the whole
// durable record. The start limb, the only bound on a subscription lane, resets
// to zero and the ceiling admits freely.
//
// pruneLocked's own defence — "anchor to the ledger's newest event" — cannot
// close this on its own, because the entry being recorded IS the newest event:
// it was just stamped with the jumped clock, so the anchor equals the jump and
// the anchor is a no-op exactly when it is needed.
//
// So the ledger measures its own writes the way globalcap measures its own
// reads: against MONOTONIC time, which no wall-clock change can move. It
// anchors a (caller-wall, monotonic) pair at open and accumulates however much
// the caller's clock has outrun monotonic time since.
//
// ---------------------------------------------------------------------------
// THE CORRECTION APPLIES TO THE PRUNE CUTOFF, AND TO NOTHING ELSE. This is the
// most important sentence in the file, and getting it wrong was a fail-open.
// ---------------------------------------------------------------------------
//
// An earlier version ALSO clamped every entry's EndedAtMs to this corrected
// instant. That looked like a strictly stronger invariant and was in fact a
// permanent, silent fail-open, because it created a SECOND skew accumulator
// alongside globalcap's and let the two drift apart without bound:
//
//   - The two accumulators share a tolerance (2000 ms of caller-vs-monotonic
//     movement between two CONSECUTIVE observations is drift, not a jump) but
//     they are SAMPLED AT DIFFERENT RATES. The query clock is sampled on every
//     admission check; the write clock only on a task terminal, which is orders
//     of magnitude rarer.
//   - Every write's input is ALREADY the query clock's output, so the write
//     clock only ever sees the residue the query clock discarded as
//     sub-tolerance drift. Aggregated over the much longer write interval, that
//     residue crosses the tolerance and is charged to l.clockSkew — while
//     globalcap's own skew stays at zero, because no single query interval ever
//     crossed it.
//   - Entries were then STAMPED at the write clock and QUERIED at the query
//     clock. The gap between the two is monotone (skew never decreases) and
//     never decays, so it grows until it exceeds global_window — at which point
//     Usage() sees NOTHING, both limbs read zero, and the ceiling admits
//     without bound, forever. Measured: 4000 terminals under nothing but
//     1900 ms sub-tolerance steps left 38,000,000 ms of write skew, zero
//     visible starts, and 18,131 admissions against global_max_tasks=5.
//
// So an entry's timestamp is clamped to the CALLER's now — the same instant the
// window is later queried against — and the corrected instant is used ONLY as
// the prune cutoff. That leaves ONE effective clock for the stamp-and-query
// pair, which is the only pair that has to agree, and the two properties that
// remain are the two C1 actually needed:
//
//   - The PRUNE clock is the corrected instant, so a forward jump can never
//     delete an entry that has not really aged out.
//   - An entry's EndedAtMs is CLAMPED to the caller's clock, so no entry can be
//     dated into the future by its own CONTENT — which is what keeps
//     newestEventMs, the anchor pruneLocked and boot reconciliation trust,
//     un-inflatable by a caller that took the timestamp from somewhere it
//     should not have.
//
// The residual correction can only ever move the prune cutoff BACKWARDS (skew
// is floored at zero and never decreases), so entries can only be kept LONGER
// than the window — over-counting, the direction a ceiling is allowed to take,
// and the direction that cannot blind the window.
//
// The other residual is the same one globalcap documents: a clock change while
// brokerd is DOWN is not detectable, because a monotonic reading does not
// survive a restart. It is logged at open, not refused.

// ledgerClockToleranceMs is the FLOOR on how much caller-wall-vs-monotonic
// movement BETWEEN TWO CONSECUTIVE WRITES is treated as drift rather than as a
// clock change. It mirrors ceilingClockToleranceMs and is deliberately generous:
// the cost of treating a jump as drift is a wiped ledger, while the cost of
// treating drift as a jump is keeping entries slightly too long.
const ledgerClockToleranceMs = ceilingClockToleranceMs

// ledgerClockDriftDivisor makes the write clock's tolerance RATE-PROPORTIONAL on
// top of that floor: one part in this many of the monotonic interval since the
// previous write is also allowed as drift.
//
// It exists because the write clock is sampled far more rarely than the query
// clock — once per task terminal versus once per admission — and a FIXED
// tolerance calibrated for "two consecutive observations" is the wrong shape for
// an interval that may be an hour long. Real drift is a RATE (a slewing NTP
// client, a slow crystal), so an hour between two writes legitimately accrues
// far more of it than a millisecond does, and charging that to l.clockSkew walks
// the prune cutoff backwards without bound on a perfectly healthy host. The
// direction is safe — an over-wide cutoff keeps entries too long, it cannot
// blind the window — but an unbounded regression means a ledger that eventually
// never prunes at all.
//
// 1% is orders of magnitude above any real clock's error and orders of magnitude
// below any jump worth correcting: over a one-hour write interval it allows 36
// seconds of drift while still catching the 48-hour NTP correction C1 is about.
const ledgerClockDriftDivisor = 100

// ledgerClockDriftCapMs is the ABSOLUTE CEILING on the proportional term, and it
// is what keeps "1% of the interval" from eventually swallowing the jump the
// guard exists to catch.
//
// Proportional-without-a-cap is unbounded in the wrong direction: the tolerance
// grows with the gap between writes, so a long enough quiet period makes ANY
// jump look like drift. At 1%, two hundred days between two writes tolerates
// forty-eight — the exact NTP correction C1 names — and clockSkew stays zero
// through it. That is not reachable from production today (every production
// writer hands Record globalcap.ceilingNowMs, whose own fixed 2s tolerance
// catches the jump first, and cmd/brokerd's raw-clock call only ANCHORS), but
// this file's header states the monotonic guard holds "even for a caller that
// hands Record a raw wall clock", and a contract that is conditional on the
// write interval is not that contract.
//
// 60 seconds is chosen to keep BOTH halves true. It is still far above any real
// clock's error across any plausible write interval — an unsynchronised crystal
// at 100 ppm needs a week between writes to drift that far, and a host running
// NTP never gets near it — and it is far below the hours-to-days scale of the
// jumps and the windows this ceiling deals in. The residual is now bounded and
// statable: at most ~62 seconds of a forward jump is absorbed as drift, ever,
// regardless of how long the ledger sat idle.
const ledgerClockDriftCapMs = 60_000

// ledgerMonoMs is the default monotonic source: milliseconds since this
// process's own anchor, immune to every wall-clock change.
func ledgerMonoMs() int64 { return int64(time.Since(processStartMono) / time.Millisecond) }

// writeNowLocked is the PRUNE CUTOFF a mutating call measures against: the
// caller's clock, less however far the caller's clock has jumped forward
// relative to monotonic time since this handle was opened.
//
// Its output is NOT an entry timestamp and must never become one — see the
// dual-accumulator discussion above, which is a fail-open, not a nit.
//
// Callers must hold l.mu. A nil mono seam returns the caller's clock unchanged.
func (l *GlobalLedger) writeNowLocked(nowMs int64) int64 {
	if l.mono == nil {
		return nowMs
	}
	mono := l.mono()
	if !l.clockAnchored {
		l.clockAnchored, l.clockWall, l.clockMono = true, nowMs, mono
		return nowMs
	}
	// Measured against the PREVIOUS write, not a fixed anchor, and only a delta
	// over the tolerance is accumulated — the same jump-vs-drift distinction
	// globalcap.ceilingNowMs draws, for the same reason. A NEGATIVE delta is a
	// backward wall jump and is deliberately not corrected: it moves the cutoff
	// back and prunes less, which is the over-counting direction.
	delta := (nowMs - l.clockWall) - (mono - l.clockMono)
	// The tolerance is the fixed floor PLUS a rate-proportional term over the
	// monotonic interval this delta accrued across, CAPPED. See
	// ledgerClockDriftDivisor (writes are rare, so a fixed tolerance turns
	// ordinary drift into a permanent cutoff regression) and
	// ledgerClockDriftCapMs (uncapped, a long enough quiet period makes any jump
	// look like drift).
	tolerance := int64(ledgerClockToleranceMs)
	if elapsed := mono - l.clockMono; elapsed > 0 {
		drift := elapsed / ledgerClockDriftDivisor
		if drift > ledgerClockDriftCapMs {
			drift = ledgerClockDriftCapMs
		}
		tolerance += drift
	}
	l.clockWall, l.clockMono = nowMs, mono
	if delta >= tolerance {
		l.clockSkew += delta
		slog.Warn("globalledger: the write clock jumped forward relative to monotonic time",
			"path", l.path, "jump_ms", delta, "total_skew_ms", l.clockSkew,
			"note", "the ledger's PRUNE CUTOFF is corrected by this amount so the jump cannot delete durable entries; entry timestamps are NOT corrected — they are stamped against the caller's clock, which is the clock the window is queried against")
	}
	return nowMs - l.clockSkew
}

// clampEventMs holds the invariant "no entry is dated after the CALLER's clock".
// A missing timestamp becomes that clock; a future one is pulled back to it.
// Both directions are the ledger refusing to let an entry's own content set the
// anchor pruneLocked and reconcileFloorMs trust.
//
// callerNowMs, NOT writeNowLocked's corrected instant. The entry is later
// QUERIED against the caller's clock (globalcap's ceilingNowMs), and stamping it
// from a different accumulator is the dual-accumulator fail-open documented
// above: the two drift apart monotonically until the window sees nothing.
func clampEventMs(endedAtMs, callerNowMs int64) int64 {
	if endedAtMs <= 0 || endedAtMs > callerNowMs {
		return callerNowMs
	}
	return endedAtMs
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
	return OpenGlobalLedgerWithClock(auditRoot, window, nowMs, ledgerMonoMs)
}

// OpenGlobalLedgerWithClock is OpenGlobalLedger with the write clock's
// monotonic source injected. Production always wants OpenGlobalLedger, which
// supplies the real one.
//
// A NIL mono DISABLES THE GUARD and makes the ledger trust the nowMs it is
// handed. That is for tests that drive time synthetically — a test that records
// at T and then compacts at T+1e12 is deliberately advancing the clock, and a
// guard that called it a jump would make the rolling window untestable while
// telling nobody anything. It is the same seam, and the same reasoning,
// globalcap.ceilingNowMs already carries for the query clock. The guard itself
// is covered by tests that inject a monotonic source and move the two apart.
func OpenGlobalLedgerWithClock(auditRoot string, window time.Duration, nowMs int64, mono func() int64) (*GlobalLedger, error) {
	if auditRoot == "" {
		return nil, errors.New("globalledger: empty audit root")
	}
	l := &GlobalLedger{
		path:      GlobalLedgerPath(auditRoot),
		byID:      map[string]struct{}{},
		compactAt: globalLedgerMinCompactEntries,
		// The write clock's monotonic guard, anchored on the first mutating
		// call. See writeNowLocked: without it a forward wall-clock jump deletes
		// the whole durable record on the next append.
		mono: mono,
	}
	if window > 0 {
		l.windowMs = window.Milliseconds()
	}
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		// PATH-FREE: this string is rendered into a 402 body for whoever
		// submitted the task, and an *os.PathError carries the host's absolute
		// audit-root layout. The wrapped error below, which goes to the
		// operator's log, keeps the path.
		l.loadErr = "the global ledger directory is unavailable: " + pathFreeErr(err)
		return l, fmt.Errorf("globalledger: %w", err)
	}
	// MkdirAll only applies the mode to directories it CREATES, so a directory
	// left behind by an older build (or by a hand-run mkdir) can be group- or
	// world-readable and stay that way. The ledger is host-only by G4, so tighten
	// it rather than inherit whatever is there.
	if fi, err := os.Stat(dir); err == nil && fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			slog.Warn("globalledger: could not tighten the ledger directory to 0700", "path", dir, "err", err)
		}
	}
	// Sweep the stray ".tmp" a crashed rewrite leaves, exactly as removeQueueItem
	// does. Idempotent; a missing file is success.
	if err := os.Remove(l.path + ".tmp"); err != nil && !os.IsNotExist(err) {
		slog.Warn("globalledger: could not remove a stale temp file", "path", l.path+".tmp", "err", err)
	}

	// The lock is held for the whole load. Nothing else can reach this handle
	// yet, so it is uncontended — it is taken because parseLocked/compactLocked
	// say "Locked", and a name that lies about its precondition is how the next
	// caller gets it wrong.
	l.mu.Lock()
	defer l.mu.Unlock()
	// Anchor the write clock's monotonic guard before anything can mutate. At
	// open the skew is zero by construction, so this changes nothing here; it
	// establishes the reference every later Record/RecordBatch/Compact is
	// measured against.
	nowMs = l.writeNowLocked(nowMs)

	lines, truncated, err := readGlobalLedgerLines(l.path)
	if err != nil {
		// We cannot see the durable record. Do NOT rewrite anything. The reason
		// is deliberately PATH-FREE: it is rendered into a 402 body for the
		// submitting client, and the host's audit-root layout is not that
		// client's business. The returned error, which goes to the operator's
		// log, names the file.
		l.loadErr = fmt.Sprintf("the global ledger could not be read (%s); usage is unknown", pathFreeErr(err))
		return l, fmt.Errorf("globalledger: read %s: %w", l.path, err)
	}
	damaged := l.parseLocked(lines, nowMs)
	if truncated {
		l.loadErr = fmt.Sprintf("the global ledger exceeds %d bytes and was not read in full; usage is a lower bound",
			globalLedgerMaxBytes)
	}
	// A repair is needed iff the scan quarantined something; compaction may be
	// needed independently. Both are the same atomic rewrite.
	if damaged > 0 {
		slog.Warn("globalledger: quarantined unreadable ledger lines",
			"path", l.path, "count", damaged,
			"note", "counted as task starts; the USD limb is a lower bound until they age out")
		// The repair is about to overwrite the damaged bytes. Keep them: a
		// tombstone records THAT a line was lost, never what it said, and an
		// operator who wants the history back has no other copy.
		//
		// Skipped when the store is already degraded, because then the rewrite
		// below refuses and there is nothing to preserve the file FROM — writing
		// a copy per boot against a file nobody is going to overwrite would just
		// litter the audit root.
		if l.loadErr == "" {
			l.preserveDamagedLocked(nowMs)
		}
	}
	if err := l.compactLocked(nowMs, damaged > 0); err != nil {
		// The in-memory state is correct and complete; only the rewrite failed.
		// Report it, but the handle stays usable and honest.
		slog.Warn("globalledger: could not rewrite the ledger at boot", "path", l.path, "err", err)
		return l, fmt.Errorf("globalledger: rewrite %s: %w", l.path, err)
	}
	// A BOOT-TIME clock check, and the honest limit of it. If the caller's clock
	// is more than a whole window past the newest recorded event, every entry the
	// ledger holds is out of window and the limbs read zero. Two different worlds
	// produce that picture — a daemon that was simply idle (or down) for longer
	// than the window, which is legitimate and must not be refused, and a host
	// clock that moved FORWARD across the restart, which silently resets the
	// ceiling. Nothing in the file distinguishes them: a monotonic reference does
	// not survive a reboot, and every timestamp on disk comes from the same wall
	// clock that moved. The IN-PROCESS case is fully handled (globalcap.go's
	// clock-skew correction); this one is logged so an operator can see it, and
	// the residual is documented rather than papered over.
	if l.windowMs > 0 {
		if ev := newestEventMs(l.entries); ev > 0 && nowMs-ev > l.windowMs {
			slog.Warn("globalledger: every recorded entry is older than the rolling window at boot",
				"path", l.path, "newest_event_ms", ev, "now_ms", nowMs, "window_ms", l.windowMs,
				"note", "the global ceiling reads zero usage; this is either a genuine idle period or a forward host-clock change across the restart, which cannot be told apart from the ledger alone")
		}
	}
	if l.loadErr != "" {
		return l, fmt.Errorf("globalledger: %s (%s)", l.loadErr, l.path)
	}
	return l, nil
}

// preserveDamagedLocked copies the pre-repair file aside before a repair
// rewrite overwrites it, as <ledger>.damaged-<nowMs>.
//
// The repair is the only operation in this file that destroys information: a
// tombstone records that a line was unreadable, never what it contained, so
// once the rewrite lands the original bytes are gone for good. For a safety
// control whose whole argument is "never silently lose a start", overwriting
// unrecoverable history to tidy the file is the wrong trade — the copy is one
// read and one write, on a boot path that only runs when damage was found.
//
// Best effort by design: failing to preserve must not stop the repair, since a
// file left un-repaired re-quarantines (and re-warns) on every boot. Errors are
// logged and swallowed.
func (l *GlobalLedger) preserveDamagedLocked(nowMs int64) {
	data, err := os.ReadFile(l.path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("globalledger: could not preserve the pre-repair ledger", "path", l.path, "err", err)
		}
		return
	}
	backup := fmt.Sprintf("%s.damaged-%d", l.path, nowMs)
	// O_EXCL: never clobber an earlier preservation, and never follow a planted
	// symlink out of the audit root.
	f, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		if !os.IsExist(err) {
			slog.Warn("globalledger: could not preserve the pre-repair ledger", "path", backup, "err", err)
		}
		return
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		slog.Warn("globalledger: could not preserve the pre-repair ledger", "path", backup, "err", err)
		return
	}
	if err := f.Sync(); err != nil {
		slog.Warn("globalledger: could not preserve the pre-repair ledger", "path", backup, "err", err)
		return
	}
	slog.Warn("globalledger: preserved the damaged ledger before repairing it",
		"path", backup, "note", "the repaired file replaces unreadable lines with tombstones; this copy is the only record of what they contained")
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
	if e.Src == "" {
		e.Src = GlobalSrcTerminal
	}
	e.Raw = nil             // never set on a task entry
	e.StartsUnknown = false // damaged-only
	e.Sum = ""              // checkpoint-only
	usdErr := sanitizeEntryUSD(&e)

	l.mu.Lock()
	defer l.mu.Unlock()
	// TWO DIFFERENT INSTANTS, and keeping them apart is load-bearing:
	//
	//   - the ENTRY is stamped against the CALLER's clock, which is the clock the
	//     window is later queried against. Clamping it to the corrected instant
	//     instead makes the ledger's skew accumulator and globalcap's drift apart
	//     until the window sees nothing — see writeNowLocked's discussion.
	//   - the PRUNE CUTOFF is the corrected instant, so a forward wall-clock jump
	//     cannot delete an entry that has not really aged out.
	//
	// The clamp still holds "no entry is dated after the clock that recorded it",
	// which is what keeps newestEventMs — the anchor pruneLocked trusts —
	// un-inflatable by entry CONTENT.
	e.EndedAtMs = clampEventMs(e.EndedAtMs, nowMs)
	nowMs = l.writeNowLocked(nowMs)
	if _, dup := l.byID[e.TaskID]; dup {
		return nil
	}
	l.entries = append(l.entries, e)
	l.byID[e.TaskID] = struct{}{}

	appendErr := l.appendLocked(e)
	if err := l.compactLocked(nowMs, false); err != nil && appendErr == nil {
		appendErr = err
	}
	if appendErr == nil {
		appendErr = usdErr
	}
	return appendErr
}

// sanitizeEntryUSD forces an entry's dollar figures to be REAL, FINITE and
// NON-NEGATIVE, downgrading anything else to "unknown" and reporting what it
// did. It runs before an entry can reach memory or disk.
//
// It exists because a NaN spends the ceiling in the one direction it must never
// fail. The USD limb compares `usage.USD >= budget`, and every comparison
// against NaN is FALSE, so a single NaN in the running total makes the USD limb
// admit forever — a control that fails OPEN, permanently, from one bad float.
// The figure is not hypothetical: it is parsed by the broker out of a proxied
// vendor response body, so it is exactly the kind of value a vendor bug or a
// hostile upstream can make strange. A +Inf is the same failure with the
// opposite sign (it makes the limb refuse forever), and a NEGATIVE figure
// SUBTRACTS from the total, which is an under-count that a hand-edited or
// replayed entry could use to buy back headroom.
//
// It also protects the FILE: json.Marshal refuses a non-finite float, so a NaN
// that reached l.entries would make every subsequent rewrite fail — compaction
// would never advance, the ledger would grow without bound, and the entry
// would never become durable.
//
// The response is to SANITIZE, never to reject the entry. The task really ran;
// dropping its record because its dollar figure is garbage would under-count
// the start limb, which is the one direction G2/G3 forbid. So the start is kept
// and only the money is disclaimed: USD 0, USDTrusted false — the same shape a
// task with no metering at all gets, which the ceiling already knows how to
// report honestly (GlobalUsage.UntrustedUSD) and which G7 names the start limb
// as the backstop for.
func sanitizeEntryUSD(e *GlobalEntry) error {
	bad := ""
	switch {
	case math.IsNaN(e.USD):
		bad = "not a number"
	case math.IsInf(e.USD, 0):
		bad = "infinite"
	case e.USD < 0:
		bad = "negative"
	}
	if bad == "" {
		return nil
	}
	was := e.USD
	e.USD, e.USDTrusted = 0, false
	return fmt.Errorf(
		"globalledger: task %s reported a %s spend figure (%v); the task start is still counted, but its spend is recorded as unknown rather than measured",
		e.TaskID, bad, was)
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
	u, _ := l.UsageWithClaims(nowMs, nil, "")
	return u
}

// UsageWithClaims is Usage PLUS the answer to "which of these in-flight claims
// has the ledger not counted yet", computed under a SINGLE hold of the ledger's
// mutex.
//
// The single hold is the whole point, and it is a correctness requirement rather
// than an optimisation. The ceiling's decision is
//
//	starts = usage.Starts + (claims the ledger does not already have)
//
// and if that is assembled from two separate ledger calls, a task-terminal write
// landing BETWEEN them counts ZERO times: the usage snapshot was taken before
// the write, so it excludes the task, and by the time the claim set is walked
// the ledger DOES have the id, so the claim is skipped as "already counted".
// The task is admitted, spends, ends — and the ceiling never sees it. Holding
// capMu does not help: the race is against the LEDGER's mutex, which the
// two-call form drops and re-takes. Taken together under one hold, every
// interleaving lands on one side or the other and the task counts exactly once.
//
// selfID is excluded from the tally: it is the id of the start being decided,
// and a re-admission of an already-claimed task must not count itself.
//
// A nil store answers "degraded" AND counts every claim as uncounted — the
// over-counting direction, which is the one a ceiling is allowed to take.
func (l *GlobalLedger) UsageWithClaims(nowMs int64, claims map[string]struct{}, selfID string) (GlobalUsage, int) {
	if l == nil {
		uncounted := 0
		for id := range claims {
			if id != selfID {
				uncounted++
			}
		}
		return GlobalUsage{Degraded: true,
			DegradedReason:       "the global ledger is unavailable; usage cannot be determined",
			StartsDegraded:       true,
			StartsDegradedReason: "the global ledger is unavailable; the task-start count cannot be determined",
			LoadErr:              "the global ledger is unavailable; usage cannot be determined",
		}, uncounted
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	uncounted := 0
	for id := range claims {
		if id == selfID {
			continue
		}
		if _, ok := l.byID[id]; ok {
			continue // already counted by the pass below; counting it again would tighten the ceiling
		}
		uncounted++
	}

	u := GlobalUsage{WindowMs: l.windowMs, LoadErr: l.loadErr}
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
	damagedUnknown := 0
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
			// A line existed, so at least one task started. Its dollars are
			// unknown, which makes the USD limb a lower bound — see Degraded
			// below. Whether it is worth EXACTLY one start is a separate
			// question, and StartsUnknown is the scan's answer to it.
			u.Starts++
			u.Damaged++
			if e.StartsUnknown {
				damagedUnknown++
			}
		case GlobalEntryCheckpoint:
			u.Starts += e.Starts
			u.Damaged += e.Damaged
			u.USD += e.USD
			u.UntrustedUSD += e.UntrustedUSD
			// A fold that swallowed an unidentifiable damaged region carries the
			// flag; the folded Starts is then a lower bound and the start limb
			// must keep refusing. Erasing it here would be the same fail-open
			// the fold itself used to have, one layer down.
			if e.StartsUnknown {
				damagedUnknown++
			}
		}
	}
	switch {
	case l.loadErr != "":
		u.Degraded, u.DegradedReason = true, l.loadErr
	case u.Damaged > 0:
		u.Degraded = true
		u.DegradedReason = fmt.Sprintf(
			"%d unreadable entr%s in the global ledger are counted as task starts but their spend is unknown; the USD total is a lower bound",
			u.Damaged, plural(u.Damaged, "y", "ies"))
	}
	// The START limb degrades only where the "one damaged line is one task
	// start" identity actually breaks: bytes we never read at all, or a
	// destroyed region the scan could not identify as a single task line (a
	// checkpoint carrying a whole folded history, or several entries whose
	// separating newlines went with them). An ordinary quarantined task line
	// does NOT get here, which is what keeps one bad byte from bricking a
	// deployment that only enforces the start limb.
	switch {
	case l.loadErr != "":
		u.StartsDegraded, u.StartsDegradedReason = true, l.loadErr
	case damagedUnknown > 0:
		u.StartsDegraded = true
		u.StartsDegradedReason = fmt.Sprintf(
			"%d unreadable region%s in the global ledger could not be identified as a single task entry, so %s may stand for a folded checkpoint or several entries at once; the task-start count is a lower bound",
			damagedUnknown, plural(damagedUnknown, "", "s"), plural(damagedUnknown, "it", "they"))
	}
	return u, uncounted
}

// ---------------------------------------------------------------------------
// Task 3 additions: the batched write, the reconcile anchor, and the external
// degrade. All three are ADDITIVE — nothing above changes shape — and each
// exists for one specific need of recording + boot reconciliation.
// ---------------------------------------------------------------------------

// RecordBatch durably records many task entries with ONE fsync, for boot
// reconciliation.
//
// It exists because Record fsyncs per call (~4ms): correct for one call per
// task terminal, ruinous for a boot sweep that may add thousands of entries
// recovered from the audit. This takes the lock once, marshals every accepted
// entry into a single buffer, and issues exactly one open/write/fsync — the
// same durability guarantee as N Records, at the cost of one.
//
// Semantics are Record's, per entry: the Kind defaults to task and only task
// entries are accepted, an invalid task id is rejected, a missing EndedAtMs is
// filled from nowMs, a duplicate TaskID is silently skipped (idempotent
// replay), and an entry whose durable write fails STAYS IN MEMORY rather than
// being dropped — losing a real task because the disk was full is the one
// direction G2/G3 forbid.
//
// It returns how many entries were newly recorded and the first error. A
// per-entry validation error does NOT abort the batch: one malformed record
// must not stop the rest of a reconciliation from being counted.
func (l *GlobalLedger) RecordBatch(nowMs int64, es []GlobalEntry) (int, error) {
	if l == nil {
		return 0, errors.New("globalledger: nil ledger")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Same split as Record: entries are clamped to the CALLER's clock (the one
	// the window is queried against) and only the PRUNE CUTOFF takes the
	// jump-corrected instant. Boot reconciliation is the OTHER writer, and a
	// forward wall-clock jump reaching pruneLocked through it wipes the durable
	// record just as thoroughly. See writeNowLocked.
	callerNowMs := nowMs
	nowMs = l.writeNowLocked(nowMs)

	var (
		buf      bytes.Buffer
		added    int
		firstErr error
	)
	for _, e := range es {
		if e.Kind == "" {
			e.Kind = GlobalEntryTask
		}
		if e.Kind != GlobalEntryTask {
			if firstErr == nil {
				firstErr = fmt.Errorf("globalledger: RecordBatch only accepts task entries, got %q", e.Kind)
			}
			continue
		}
		if !queueIDRE.MatchString(e.TaskID) {
			if firstErr == nil {
				firstErr = fmt.Errorf("globalledger: invalid task id %q", e.TaskID)
			}
			continue
		}
		e.EndedAtMs = clampEventMs(e.EndedAtMs, callerNowMs)
		if e.Src == "" {
			e.Src = GlobalSrcReconcile
		}
		e.Raw = nil
		e.StartsUnknown = false
		e.Sum = ""
		// Same rule as Record: a garbage dollar figure disclaims the money and
		// keeps the start, and it must never reach l.entries — a non-finite
		// float there would make every later rewrite fail and wedge compaction.
		if err := sanitizeEntryUSD(&e); err != nil {
			slog.Warn("globalledger: a reconciled entry carried an unusable spend figure", "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
		if _, dup := l.byID[e.TaskID]; dup {
			continue
		}
		payload, err := json.Marshal(e)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		buf.Write(payload)
		buf.WriteByte('\n')
		l.entries = append(l.entries, e)
		l.byID[e.TaskID] = struct{}{}
		added++
	}
	if added == 0 {
		return 0, firstErr
	}
	if err := l.appendBytesLocked(buf.Bytes()); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := l.compactLocked(nowMs, false); err != nil && firstErr == nil {
		firstErr = err
	}
	return added, firstErr
}

// NewestEventMs is the newest REAL event time the ledger holds, ignoring the
// window and ignoring quarantine tombstones (whose timestamps are repair-time
// stamps, not events).
//
// Boot reconciliation uses it as its lookback ANCHOR, for the mirror image of
// the reason pruneLocked uses it as a pruning anchor. Pruning is destructive,
// so it must not trust a clock that jumped FORWARD; reconciliation is purely
// additive, so its risk is the opposite — a forward-jumped clock would put the
// whole lookback window in the future and the sweep would silently ADD NOTHING,
// which is the under-count G3 forbids. Anchoring both to the ledger's own
// newest event keeps the two consistent: the reconcile never skips a task that
// compaction has not already deleted.
func (l *GlobalLedger) NewestEventMs() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return newestEventMs(l.entries)
}

// Degrade marks the ledger's numbers as a lower bound because a caller OUTSIDE
// this file could not read something the totals depend on — specifically, boot
// reconciliation failing to read the audit trail it cross-checks against (G3).
//
// It sets the same signal a failed load sets (LoadError), so globalcap.go
// refuses on BOTH limbs: a reconcile that could not enumerate the audit does
// not know how many starts it is missing, not just how many dollars. Under G2
// that "I don't know" must mean "no" — but only for a limb actually being
// enforced, which globalcap.go already gates, so an install with the ceiling
// off is unaffected.
//
// It is STICKY and first-wins, exactly like a load error: the bytes do not come
// back, and the first reason is the root cause rather than a later symptom. Its
// side effect on compaction is deliberate and inherited, not incidental — a
// degraded store never rewrites itself, because rewriting from an incomplete
// picture would turn "we could not see everything" into "this is everything".
//
// An empty reason is ignored: degrading with nothing to tell the operator would
// be a refusal with no explanation.
func (l *GlobalLedger) Degrade(reason string) {
	if l == nil || reason == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.loadErr == "" {
		l.loadErr = reason
	}
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
	return l.rewriteLocked(l.writeNowLocked(nowMs))
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
	// A rewrite writes the checkpoint's copies consecutively at the HEAD of the
	// file, so a damaged line inside that head region may be a lost copy rather
	// than a lost task entry. Resolve the region first: if any copy survived, the
	// checkpoint is recovered exactly and the destroyed copies are dropped
	// without a tombstone (they carried nothing the survivor does not).
	cp, cpLines, cpLost := recoverCheckpointPrefix(lines)
	if cp != nil {
		l.entries = append(l.entries, *cp)
		if cpLost > 0 {
			slog.Warn("globalledger: recovered the checkpoint from a surviving copy",
				"path", l.path, "lost_copies", cpLost, "copies", globalLedgerCheckpointCopies,
				"note", "the folded start and spend totals are exact; the rewrite below restores the lost copies")
		}
	}
	for _, raw := range lines[cpLines:] {
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
		if e.Kind == GlobalEntryCheckpoint {
			// A checkpoint outside the head region: only possible in a
			// hand-edited or concatenated file. Keep it (its counts are
			// checksummed, so it is real) unless it duplicates the recovered one.
			if cp != nil && e.Sum == cp.Sum {
				continue
			}
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
		// A dollar figure that is not a real, non-negative number is not data.
		// It cannot have come from this broker (Record sanitizes), so it is a
		// hand-edit, a replay or a corruption — and believing it would either
		// poison the total with a NaN (which makes the USD limb admit forever,
		// since every comparison against NaN is false) or SUBTRACT from the
		// limb. Quarantining keeps the start and disclaims the money.
		if !usableUSD(e.USD) {
			return e, false
		}
	case GlobalEntryCheckpoint:
		if e.EndedAtMs <= 0 || e.Starts < 0 || e.Damaged < 0 {
			return e, false
		}
		// Same rule, and it matters more here: a checkpoint's USD is the folded
		// sum of an entire history, so a negative one buys back arbitrary
		// headroom.
		if !usableUSD(e.USD) || !usableUSD(e.UntrustedUSD) {
			return e, false
		}
		// The checksum is REQUIRED. A checkpoint is the only line whose numbers
		// nothing else can reconstruct, and a corruption that lands inside a
		// digit still parses as valid JSON — without this, the ledger would
		// silently believe a smaller start count forever. A checkpoint that does
		// not verify is damage, and the copies exist so that damage is
		// recoverable rather than fatal.
		if e.Sum == "" || e.Sum != checkpointSum(e) {
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

// usableUSD is the ledger's definition of a believable dollar figure: real,
// finite, and not negative. Money that fails it is recorded as unknown, never
// as a number.
func usableUSD(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0
}

// pathFreeErr renders err WITHOUT the filesystem path it may carry.
//
// Every degrade reason in the ceiling ends up in a 402 body for whoever
// submitted the task — globalcap.go says so in three places and then this file
// handed it an *os.PathError, whose Error() is "open /Users/.../audit/global/
// ledger.jsonl: permission denied". A refusal is exactly the moment a prober
// goes looking, and the host's audit-root layout is not the submitting client's
// business. The operator still gets the path: every caller logs it separately
// and the wrapped error returned alongside keeps it.
// PathFreeError is pathFreeErr for callers outside this package. Boot
// reconciliation (cmd/brokerd) builds a Degrade reason out of a ReadDir error,
// and that reason travels the same route into a 402 body.
func PathFreeError(err error) string { return pathFreeErr(err) }

func pathFreeErr(err error) string {
	if err == nil {
		return ""
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Op + ": " + pe.Err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Op + ": " + le.Err.Error()
	}
	var se *os.SyscallError
	if errors.As(err, &se) {
		return se.Syscall + ": " + se.Err.Error()
	}
	return err.Error()
}

// checkpointSum is the checkpoint's self-checksum: a digest over exactly the
// fields whose loss cannot be reconstructed from anything else in the file. It
// deliberately excludes Sum itself and excludes nothing that carries a count.
//
// It is truncated to 128 bits, which is not a security boundary — the file is
// 0600 under a 0700 host-only directory and an attacker who can write it can
// write anything — but a corruption-detection one, and 128 bits makes an
// accidental collision impossible in practice.
func checkpointSum(e GlobalEntry) string {
	h := sha256.New()
	fmt.Fprintf(h, "drydock-global-checkpoint-v2\x00%d\x00%d\x00%d\x00%d\x00%s\x00%s\x00%s\x00%t",
		e.EndedAtMs, e.FoldedThroughMs, e.Starts, e.Damaged,
		strconv.FormatFloat(e.USD, 'x', -1, 64),
		strconv.FormatFloat(e.UntrustedUSD, 'x', -1, 64),
		e.Src, e.StartsUnknown)
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// recoverCheckpointPrefix resolves the head of the file, where a rewrite writes
// the checkpoint's identical copies consecutively.
//
// It returns the recovered checkpoint (nil if none), how many leading lines it
// consumed, and how many copies were unreadable. When it recovers nothing it
// consumes nothing, so a file with no checkpoint — every windowed ledger, and
// every total-mode ledger below the fold threshold — is parsed exactly as
// before and a damaged first line is quarantined normally.
//
// The point of the copies is that the folded Starts of a whole history must not
// hinge on one byte. Damage that misses at least one copy is recovered EXACTLY:
// the destroyed copies are dropped without a tombstone, because a tombstone for
// them would be a phantom start, and the rewrite that follows restores the full
// set of copies. Damage that hits EVERY copy falls through to the ordinary
// quarantine path, where the tombstones are marked StartsUnknown and the start
// limb refuses rather than counting a reset total.
func recoverCheckpointPrefix(lines [][]byte) (cp *GlobalEntry, consumed, lost int) {
	var found *GlobalEntry
	n, missing := 0, 0
	for i := 0; i < len(lines) && i < globalLedgerCheckpointCopies; i++ {
		line := bytes.TrimRight(lines[i], "\r")
		if len(bytes.TrimSpace(line)) == 0 {
			break
		}
		e, ok := decodeGlobalEntry(line)
		if !ok {
			// Provisionally a lost copy. It only counts as one if a sibling copy
			// turns up, which is what makes this safe: with no surviving copy
			// nothing is consumed and the line is quarantined as usual.
			n, missing = i+1, missing+1
			continue
		}
		if e.Kind != GlobalEntryCheckpoint {
			break // the head region is over
		}
		if found == nil {
			cpy := e
			found = &cpy
		} else if e.Sum != found.Sum {
			break // a different fold: not a copy, so stop consuming
		}
		n = i + 1
	}
	if found == nil {
		return nil, 0, 0
	}
	return found, n, missing
}

func quarantine(line []byte, nowMs int64) GlobalEntry {
	raw := line
	if len(raw) > globalLedgerRawKeep {
		raw = raw[:globalLedgerRawKeep]
	}
	return GlobalEntry{
		Kind:          GlobalEntryDamaged,
		EndedAtMs:     nowMs,
		Src:           GlobalSrcRepair,
		Raw:           append([]byte(nil), raw...),
		StartsUnknown: !damagedIsOneTaskStart(line),
	}
}

// damagedIsOneTaskStart asks whether a destroyed line can still be shown to be
// exactly ONE task entry — the claim the tombstone's "counts as one start"
// rests on.
//
// It requires POSITIVE evidence rather than absence of contrary evidence,
// because the failure mode of guessing wrong is a silent under-count of the one
// limb that bounds a subscription lane:
//
//   - exactly one `"kind":` in the line. Lines are split on "\n", so a damaged
//     region that took its newlines with it arrives here as several entries
//     concatenated — and would otherwise be scored as one start instead of N.
//   - that kind is `task`. A checkpoint carries the folded Starts of the whole
//     history; scoring it as one start is the entire I1 failure.
//   - a `task_id` field, the shape only a task entry has.
//   - a length a single task entry can actually have.
//
// The common crash signature passes: an append torn mid-line leaves a PREFIX,
// and kind and task_id are the first two fields json.Marshal emits, so a torn
// task line still identifies itself. A random byte flip in the middle of a task
// line passes too. Damage that lands inside `"kind":"task"` itself does not,
// and refusing there is the conservative answer for a control.
func damagedIsOneTaskStart(line []byte) bool {
	if len(line) == 0 || len(line) > globalLedgerMaxTaskLineBytes {
		return false
	}
	if bytes.Count(line, []byte(`"kind":`)) != 1 {
		return false
	}
	if !bytes.Contains(line, []byte(`"kind":"`+string(GlobalEntryTask)+`"`)) {
		return false
	}
	return bytes.Contains(line, []byte(`"task_id":"`))
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

// appendBytesLocked appends already-marshalled, newline-terminated lines in ONE
// open/write/fsync. It is appendLocked's bulk twin (same flags, same 0600, same
// O_NOFOLLOW, same fsync) and exists for RecordBatch, whose whole point is that
// a boot sweep of thousands of recovered entries costs one fsync rather than
// thousands. Deliberately left as a separate function rather than folded into
// appendLocked: the single-entry path is the hot, crash-critical one and is not
// worth disturbing for the sake of four shared lines.
//
// The crash signature is identical to appendLocked's — a torn TRAILING line,
// which the boot scan quarantines rather than drops — because a partial write
// can only truncate the buffer's tail. Interior lines that made it to disk are
// whole JSON objects.
func (l *GlobalLedger) appendBytesLocked(lines []byte) error {
	if len(lines) == 0 {
		return nil
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(lines); err != nil {
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
// durably (writeLedgerFileDurable: temp + fsync + rename + directory fsync).
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
	for i, e := range kept {
		payload, err := json.Marshal(e)
		if err != nil {
			// ONE unmarshalable entry must not be able to wedge compaction
			// forever. Returning here would mean the rewrite never lands, so
			// compactAt never advances, every later Record retries the same
			// failing O(n) rewrite, and the file grows without bound — a single
			// bad float would permanently disable the mechanism that keeps this
			// ledger bounded and that makes a repair stick. So the entry is
			// replaced, IN PLACE and in memory, by a tombstone that preserves
			// its start and disclaims everything else.
			slog.Warn("globalledger: an in-memory entry could not be marshalled; replacing it with a tombstone",
				"path", l.path, "task_id", e.TaskID, "kind", e.Kind, "err", err)
			t := GlobalEntry{
				Kind: GlobalEntryDamaged, EndedAtMs: e.EndedAtMs, Src: GlobalSrcRepair,
				Raw: []byte("unmarshalable in-memory entry: " + err.Error()),
				// A task entry is worth exactly one start; anything else may be
				// worth more, and the start limb must say so.
				StartsUnknown: e.Kind != GlobalEntryTask,
			}
			if t.EndedAtMs <= 0 {
				t.EndedAtMs = nowMs
			}
			kept[i] = t
			payload, err = json.Marshal(t)
			if err != nil {
				return err // unreachable: the tombstone has no float fields
			}
		}
		buf.Write(payload)
		buf.WriteByte('\n')
		// A checkpoint is written in identical copies so that the folded history
		// of the whole ledger does not hinge on a single byte. See
		// globalLedgerCheckpointCopies and recoverCheckpointPrefix.
		if e.Kind == GlobalEntryCheckpoint {
			for c := 1; c < globalLedgerCheckpointCopies; c++ {
				buf.Write(payload)
				buf.WriteByte('\n')
			}
		}
	}
	if err := writeLedgerFileDurable(l.path, buf.Bytes(), 0o600); err != nil {
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
	// ...but not without bound. Doubling forever would let a ledger that grew
	// large go arbitrarily long between rewrites, and a rewrite is what makes a
	// repair, a fold and a failed durable append heal.
	//
	// The bound is SLACK ABOVE THE SURVIVORS, not an absolute count, and that
	// distinction is the difference between amortized O(1) and a cliff. An
	// absolute ceiling means that once the surviving set reaches it, every
	// single append is over threshold and triggers a full O(n) rewrite —
	// compaction degrades to quadratic exactly where the file is largest.
	// Bounding the HEADROOM instead keeps a fixed number of appends between
	// rewrites however large the ledger grows, so the amortized cost stays
	// constant and the file stays bounded by survivors+slack.
	if cap := len(kept) + globalLedgerMaxCompactSlack; l.compactAt > cap {
		l.compactAt = cap
	}
	return nil
}

// writeLedgerFileDurable is the ledger's whole-file write: temp + fsync +
// rename + directory fsync.
//
// It deliberately does NOT use internal/atomicfile, which this file used
// before. atomicfile does os.WriteFile followed by os.Rename with no fsync of
// either the temp file or the directory, so the guarantee rewriteLocked
// documents — a crash leaves the whole old file or the whole new one — is not
// actually provided: a power loss can land the rename while the new file's data
// is still in page cache, and the ledger comes back EMPTY or short. For a
// credential file (atomicfile's other callers) that costs a re-mint; for a
// safety ledger it is a total under-count of the ceiling, which is the one
// direction G2/G3 forbid. atomicfile's other callers are left exactly as they
// are; this is a local divergence, paid for by one fsync on a path that runs on
// an amortized schedule rather than per append.
//
// The temp open also carries O_EXCL and O_NOFOLLOW, matching every other open in
// this file: a pre-planted "ledger.jsonl.tmp" symlink must not be followed and
// must not be written through.
func writeLedgerFileDurable(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	// A stale temp from a crashed rewrite is swept at open, but a rewrite that
	// failed after creating it within THIS process would otherwise wedge every
	// later one on O_EXCL.
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	// The fsync that makes the rename mean something.
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	// And the directory fsync that makes the RENAME durable. Best effort: the
	// rename has already happened and the data is already on disk, so failing
	// here would report a write that in fact succeeded. Some filesystems refuse
	// a directory fsync outright.
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		if err := d.Sync(); err != nil {
			slog.Debug("globalledger: could not fsync the ledger directory", "path", filepath.Dir(path), "err", err)
		}
		d.Close()
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
	// Pruning is DESTRUCTIVE, so it gets TWO defences and needs both.
	//
	// The first is upstream and is the real one: nowMs here has already been
	// through writeNowLocked, so a caller's forward wall-clock jump has been
	// subtracted out against monotonic time, and every entry's EndedAtMs has
	// been clamped to that same corrected instant. Without it this anchor is a
	// no-op exactly when it matters — the entry being recorded IS the newest
	// event, so a jumped clock sets the anchor to the jump.
	//
	// The second is the anchor below: the cutoff is measured from the EARLIER of
	// the corrected now and the ledger's own newest event, so a stale entry set
	// loaded from a file written by an earlier process (whose monotonic anchor
	// is gone) can still only be aged out by its own history. A clock that jumps
	// backwards needs no defence: it moves the cutoff back and prunes less,
	// which is the over-counting direction.
	//
	// Queries still use the caller's raw now: reading a smaller in-window number
	// is recoverable, deleting the record is not.
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
			// The fold must CARRY the "I don't know", not erase it. A tombstone
			// marked StartsUnknown stands for an unread region that may have
			// been a whole checkpoint or several entries; scoring it as exactly
			// one start and dropping the flag turns a fail-closed refusal into a
			// definite, wrong count — and the fold is written to disk, so the
			// error is permanent. The flag propagates instead, and the start
			// limb keeps refusing until an operator repairs the file.
			cp.StartsUnknown = cp.StartsUnknown || e.StartsUnknown
		case GlobalEntryCheckpoint:
			cp.Starts += e.Starts
			cp.Damaged += e.Damaged
			cp.USD += e.USD
			cp.UntrustedUSD += e.UntrustedUSD
			// Folding a checkpoint into a checkpoint carries the flag onward for
			// the same reason: total mode folds repeatedly, forever.
			cp.StartsUnknown = cp.StartsUnknown || e.StartsUnknown
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
	// Stamped LAST, over the final values: the checksum is what lets a later
	// read tell a real checkpoint from a corrupted one, and it must cover the
	// EndedAtMs adjustment above.
	cp.Sum = checkpointSum(cp)
	return append([]GlobalEntry{cp}, ordered[split:]...)
}
