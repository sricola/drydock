// Package audit parses drydock's on-disk per-task audit log (<id>.jsonl). The
// last {"type":"result"} line summarises the run; the first {"type":"drydock_meta"}
// line records auth mode + sensitivity. This is the single source of truth for
// outcome/cost so `drydock tasks` and the web UI agree.
package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"syscall"
)

// OpenRead opens an audit file read-only, refusing to traverse a final-component
// symlink (O_NOFOLLOW). The audit dir is the source-of-truth integrity artifact;
// a symlink planted there (by a same-uid process) must not redirect a read out
// of it. drydock runs on macOS/Linux only, both of which have O_NOFOLLOW.
// Callers that read one file more than once should OpenRead it once and use the
// *File variants (ReadMetaFile, LastResultFile) instead of re-opening per read.
func OpenRead(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

type Result struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	DurationMs   int64   `json:"duration_ms"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	NumTurns     int     `json:"num_turns"`
	// Src is "broker" on a broker-authored terminal result. The agent's stdout
	// is untrusted, so financial controls (the aggregate-cap seed) trust only
	// Src=="broker" cost, never a CLI-emitted total_cost_usd.
	Src string `json:"src"`
}

type Meta struct {
	Type         string `json:"type"`
	Subscription bool   `json:"subscription"`
	Sensitive    bool   `json:"sensitive"`
}

// StageMs is the per-stage wall-clock breakdown of a metrics row. Setup and
// Verifying are omitempty so rows written before those stages existed (and
// tasks that never run them) keep their exact prior shape; Queued (time from
// enqueue to dispatch, queued tasks only) is omitempty for the same reason —
// a synchronous POST /tasks task never sat on the queue and its row keeps
// its exact prior shape.
type StageMs struct {
	Queued    int64 `json:"queued,omitempty"`
	Preparing int64 `json:"preparing"`
	Setup     int64 `json:"setup,omitempty"`
	Running   int64 `json:"running"`
	Verifying int64 `json:"verifying,omitempty"`
	Pushing   int64 `json:"pushing"`
}

// Metrics is the broker-authored terminal {"type":"metrics"} row (one per
// task, last line of the file). Only Src=="broker" rows count, and the last
// one wins: the broker writes it after the agent's output ends, so a forged
// in-VM row is always superseded.
type Metrics struct {
	Type   string `json:"type"`
	Src    string `json:"src"`
	TaskID string `json:"task_id"`
	Agent  string `json:"agent"`
	Vendor string `json:"vendor"`
	Auth   string `json:"auth"`
	// Outcome is the terminal path the broker actually took: "pushed",
	// "denied", "cancelled", "push_failed", "error", "no_diff",
	// "setup_failed", "verify_failed", or
	// "policy_blocked" (auto-approved pushes fold into "pushed": the
	// auto/gated distinction isn't surfaced separately). Empty on pre-v0.6.7
	// rows; see OutcomeKeyWithMetrics.
	Outcome            string  `json:"outcome,omitempty"`
	Repo               string  `json:"repo"`
	Model              string  `json:"model,omitempty"`
	StageMs            StageMs `json:"stage_ms"`
	EgressGateWaitMs   int64   `json:"egress_gate_wait_ms"`
	ApprovalGateWaitMs int64   `json:"approval_gate_wait_ms"`
	Requests           int     `json:"requests"`
	DiffFiles          int     `json:"diff_files"`
	DiffBytes          int64   `json:"diff_bytes"`
	CostUSD            float64 `json:"cost_usd"`
	WidenRequested     int     `json:"widen_requested"`
	WidenOutcome       string  `json:"widen_outcome"`
}

// readFirstMeta parses the first line of r as a {"type":"drydock_meta"} record.
// Legacy/absent/malformed → zero value. Shared by ReadMeta and ReadMetaFile.
func readFirstMeta(r io.Reader) Meta {
	line, err := bufio.NewReader(r).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return Meta{}
	}
	var m Meta
	if json.Unmarshal(bytes.TrimSpace(line), &m) != nil || m.Type != "drydock_meta" {
		return Meta{}
	}
	return m
}

// tailLines reads the last ~16KB of f (from the given size) and returns it
// split into lines, for last-wins scans of terminal rows. It tolerates an
// unterminated trailing line (brokerd may be mid-write). Shared by the
// result-row and metrics-row tail scans so the window and seek handling
// cannot drift between them.
func tailLines(f *os.File, size int64) ([][]byte, error) {
	const tail = 16 * 1024
	off := int64(0)
	if size > tail {
		off = size - tail
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return bytes.Split(data, []byte("\n")), nil
}

// scanTailForResult returns the final {"type":"result",...} line in f's tail.
// found=false when none is present; err is a seek/read error. Shared by
// LastResult, LastResultFile, and HasResultLine.
func scanTailForResult(f *os.File, size int64) (Result, bool, error) {
	lines, err := tailLines(f, size)
	if err != nil {
		return Result{}, false, err
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var x Result
		if json.Unmarshal(lines[i], &x) == nil && x.Type == "result" {
			return x, true, nil
		}
	}
	return Result{}, false, nil
}

// ReadMeta returns the drydock_meta first line of path. Legacy/absent → zero value.
func ReadMeta(path string) Meta {
	f, err := OpenRead(path)
	if err != nil {
		return Meta{}
	}
	defer f.Close()
	return readFirstMeta(f)
}

// LastResult finds the final {"type":"result",...} line by reading only the
// file tail. ok=false when none is present (still running / killed early).
func LastResult(path string, size int64) (Result, bool) {
	f, err := OpenRead(path)
	if err != nil {
		return Result{}, false
	}
	defer f.Close()
	r, ok, _ := scanTailForResult(f, size)
	return r, ok
}

// ReadMetaFile reads the drydock_meta line from an already-opened file. The
// caller opens it with appropriate flags (e.g. O_NOFOLLOW) and closes it. The
// offset is reset to the start first, so callers may interleave ReadMetaFile
// with LastResultFile in any order.
func ReadMetaFile(f *os.File) Meta {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Meta{}
	}
	return readFirstMeta(f)
}

// LastResultFile is LastResult for a pre-opened *os.File, so the caller controls
// how it was opened (e.g. O_NOFOLLOW to refuse symlinks). ok=false when absent.
func LastResultFile(f *os.File) (Result, bool) {
	info, err := f.Stat()
	if err != nil {
		return Result{}, false
	}
	r, ok, _ := scanTailForResult(f, info.Size())
	return r, ok
}

// HasDuration reports whether a real duration is known. An interrupted task
// (brokerd died under it) has a synthetic 0ms we must not display as "0s".
func HasDuration(r Result, ok bool) bool { return ok && r.Subtype != "interrupted" }

// OutcomeKey classifies a result into a stable machine key: "running" (no
// result row), "interrupted", "push_failed", "error", "ok", or the raw
// subtype for anything else (e.g. a broker-authored "denied"). Outcome
// renders its display string from this key and `drydock stats` aggregates
// on it, so the two views classify a task identically by construction.
func OutcomeKey(r Result, ok bool) string {
	switch {
	case !ok:
		return "running"
	case r.Subtype == "interrupted":
		return "interrupted"
	case r.Subtype == "push_failed":
		return "push_failed"
	case r.IsError:
		return "error"
	case r.Subtype == "success":
		return "ok"
	default:
		return r.Subtype
	}
}

// Outcome derives the human outcome string from OutcomeKey.
func Outcome(r Result, ok bool, m Meta) string {
	return outcomeString(OutcomeKey(r, ok), r, m)
}

// OutcomeKeyWithMetrics is OutcomeKey, folding in the broker's terminal-path
// outcome from the metrics row for the cases the result row cannot tell
// apart on its own: a diff denied at the approval gate (the result row is
// still the agent's own pre-gate "success" line: pushAndOpenPR streams
// outcome=denied to the live client but never rewrites the audit log), a
// mid-run kill (runSandbox's ctx-cancelled branch reuses appendBrokerResult's
// generic subtype:"error"), and a fail-closed diff-capture failure (V-01:
// HandleTask's CaptureDiff error branch sets tr.outcome = "error" on the
// metrics row but, like the denied case, appends no broker result row, so
// the result row is still the agent's own pre-failure "success" line).
//
// The one rule, applied uniformly regardless of which metrics outcome fired:
// a non-empty broker metrics outcome (denied, cancelled, error) REFINES any
// coarse result-row key. "ok" is coarse because it's the agent's own
// pre-gate/pre-failure success line and the broker's real terminal path
// (denied/cancelled/error) never rewrote it. "error" is coarse because
// appendBrokerResult's mid-run-kill branch reuses the same generic subtype
// for every abort reason, so the metrics row is the only place that says
// WHICH abort it was (the live-kill path relies on an "error" key becoming
// "cancelled" here). "denied" is likewise coarse for a RESUMED task: gateOutcome
// gives a killed resume the on-disk subtype "denied" (the resumed path's
// vocabulary has no "killed" subtype of its own) while the metrics row
// carries the finer "cancelled" (resume-kill relies on that refinement
// landing here too). The one key that is NOT coarse is "push_failed": it
// carries strictly more specific push-time information than any gate-cause
// outcome could, so it is the sole key the override never touches (a
// metrics-row "error" refining it would be a regression, not a refinement).
// interrupted and any raw agent subtype are refined the same as the others;
// in practice no code path pairs a non-empty metrics outcome with either
// today (the "interrupted" result rows TerminateStuckAudits and the resumed
// gateTimeout branch write both go with hasMetrics == false / m.Outcome ==
// ""), so this is future-proofing, not a currently-observed override. A
// pre-outcome-field metrics row (m.Outcome == "", true for every audit file
// written before this field existed, and for a shutdown-parked gate: see
// gateOutcome) matches no case below and leaves key unchanged, so the
// fallback IS the unmodified key, not a separate code path.
func OutcomeKeyWithMetrics(r Result, ok bool, m Metrics, hasMetrics bool) string {
	key := OutcomeKey(r, ok)
	if !hasMetrics || key == "push_failed" {
		return key
	}
	switch m.Outcome {
	case "denied", "cancelled", "error":
		return m.Outcome
	}
	return key
}

// OutcomeWithMetrics is Outcome, classified through OutcomeKeyWithMetrics
// instead of OutcomeKey: the display-string counterpart to
// OutcomeKeyWithMetrics for readers (`drydock tasks`, the web UI) that need
// the human string, not just the machine key.
func OutcomeWithMetrics(r Result, ok bool, meta Meta, m Metrics, hasMetrics bool) string {
	return outcomeString(OutcomeKeyWithMetrics(r, ok, m, hasMetrics), r, meta)
}

// outcomeString renders the human display string for an already-classified
// key, shared by Outcome and OutcomeWithMetrics so the two can never drift.
func outcomeString(key string, r Result, m Meta) string {
	if key == "running" {
		return "running?"
	}
	var s string
	switch key {
	case "push_failed":
		s = "push failed"
	case "setup_failed":
		s = "setup failed"
	case "policy_blocked":
		s = "policy blocked"
	case "planned":
		// Plan-mode terminal: the broker captured the agent's plan and
		// stopped — nothing was verified or pushed. Displayed as-is (matches
		// the raw-subtype default; the explicit case documents the vocabulary).
		s = "planned"
	case "dead_letter":
		// Queue terminal: the queued task exhausted its run without a clean
		// finish and was parked as undeliverable (Increment B adds retry).
		s = "dead-letter"
	// NOTE what has no case here: "ci_failed". It is a QUEUE terminal, and no
	// writer anywhere emits it as a result subtype — deliberately, so an
	// observed CI failure can never relabel a push that landed exactly as asked
	// (see CIObservation). A display case for a key this function cannot
	// receive would be a claim that `drydock tasks`/`stats` surface CI verdicts.
	// They do not; `drydock queue list` and GET /queue do.
	case "completed":
		// Queue terminal: the queued task finished cleanly (pushed, no_diff,
		// or planned). Displayed as-is; the explicit case documents the
		// broker-observed queue vocabulary alongside dead_letter.
		s = "completed"
	case "ok":
		if r.NumTurns > 0 {
			unit := "turns"
			if r.NumTurns == 1 {
				unit = "turn"
			}
			s = fmt.Sprintf("ok (%d %s)", r.NumTurns, unit)
		} else {
			s = "ok"
		}
	default:
		s = key
	}
	if m.Sensitive {
		s += " · sensitive"
	}
	return s
}

// Cost formats the cost column. Subscription runs show the literal word; a
// task with no result line shows "-".
func Cost(m Meta, r Result, ok bool) string {
	if !ok {
		return "-"
	}
	if m.Subscription {
		return "subscription"
	}
	return fmt.Sprintf("$%.4f", r.TotalCostUSD)
}

// TotalCost returns total_cost_usd from the last result line in path.
// Returns 0 when no result line is present or the file cannot be read.
func TotalCost(path string) float64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	r, ok := LastResult(path, fi.Size())
	if !ok {
		return 0
	}
	return r.TotalCostUSD
}

// HasResultLine reports whether path's tail contains a parsed
// {"type":"result",...} line. Returns (false, nil) when no result is
// present; returns (false, err) when the file cannot be read.
func HasResultLine(path string) (bool, error) {
	f, err := OpenRead(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	_, ok, err := scanTailForResult(f, info.Size())
	return ok, err
}

// taskLine is the {"type":"drydock_task",...} invocation record.
type taskLine struct {
	Type  string `json:"type"`
	Agent string `json:"agent"`
}

// TaskAgent returns the agent recorded in path's drydock_task line, or "" if
// absent (a pre-v0.6.0 trace) or unreadable. Opened O_NOFOLLOW like the other
// audit reads.
func TaskAgent(path string) string {
	f, err := OpenRead(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		if !bytes.Contains(sc.Bytes(), []byte(`"drydock_task"`)) {
			continue
		}
		var tl taskLine
		if json.Unmarshal(sc.Bytes(), &tl) == nil && tl.Type == "drydock_task" {
			return tl.Agent
		}
	}
	return ""
}

// TaskAgentFile is TaskAgent for an already-open file. The invocation record
// is written immediately after drydock_meta, before any agent output, so
// only the first few lines are scanned instead of the whole (potentially
// MB-sized) trace; "" for pre-v0.6.0 traces or when absent from the head.
func TaskAgentFile(f *os.File) string {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for i := 0; i < 4 && sc.Scan(); i++ {
		var tl taskLine
		if json.Unmarshal(sc.Bytes(), &tl) == nil && tl.Type == "drydock_task" {
			return tl.Agent
		}
	}
	return ""
}

var progressLine = regexp.MustCompile(`^\[\d+/\d+\]`)

// looksLikeError reports whether a line reads as a failure message, so
// Reason can prefer it over an incidental trailing line.
func looksLikeError(ln string) bool {
	l := strings.ToLower(ln)
	return strings.Contains(l, "error") || strings.Contains(l, "fatal") ||
		strings.Contains(l, "panic") || strings.Contains(l, "failed")
}

// Reason returns the last human-meaningful line of an audit log — the line
// that explains a boot failure (e.g. an entrypoint error). It skips empty
// lines, container progress lines ("[6/6] …"), and JSON event lines.
// ok is false when nothing meaningful is found, so the caller falls back to
// a generic error message.
func Reason(path string) (line string, ok bool) {
	f, err := OpenRead(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return "", false
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	lastMeaningful := ""
	for i := len(lines) - 1; i >= 0; i-- {
		ln := strings.TrimSpace(lines[i])
		if ln == "" || strings.HasPrefix(ln, "{") || strings.HasPrefix(ln, "[") || progressLine.MatchString(ln) {
			continue
		}
		// Prefer the most recent line that actually reads as an error: some
		// agents (e.g. codex) print incidental trailing output after the real
		// failure — `ERROR: exceeded retry limit …` followed by a bare token
		// count — and the bare count is useless as an operator-facing reason.
		if looksLikeError(ln) {
			return ln, true
		}
		if lastMeaningful == "" {
			lastMeaningful = ln // fallback when no line reads as an error
		}
	}
	if lastMeaningful != "" {
		return lastMeaningful, true
	}
	return "", false
}

// CIObservation is the broker-authored {"type":"ci_observation"} record: ONE
// terminal, host-observed CI conclusion for the pull request a task's push
// opened, appended to that task's audit trace.
//
// It is USUALLY written long after the task terminated and its audit fd was
// closed — but not always, and the code must not assume it: the marker-write
// unwind records an observation synchronously inside finishPush, and the
// watcher can conclude a marker the instant it lands, both while the task's own
// audit fd is still open. Every writer of a .jsonl trace therefore opens it
// O_APPEND (see broker.runLifecycle and broker.appendLine) so two fds can
// interleave lines but can never overwrite each other.
//
// TRUST: this row is a LOG, never an input to a control decision. The trace it
// lands in is agent-writable — the VM's stdout is copied into it verbatim — so
// an agent can print a line that decodes as a broker-authored ci_observation
// (Type and Src are both attacker-supplied text). Readers may render it; no
// reader may branch on it. The AUTHORITATIVE, machine-readable form of the same
// fact is the durable queue item (broker.QueueItem.CIState + its terminal
// state), which nothing inside the VM can write.
//
// WHY THIS IS NOT A {"type":"result"} ROW — the ordering decision, recorded so
// nobody "simplifies" it into one:
//
// The CI observation is the only fact drydock records about a task after the
// task is over. It happens minutes to hours later, on a file the broker has
// already flushed and closed, about work that happened on GitHub rather than
// on this host. Writing it as a late `result` line would collide with three
// invariants that the last-result-line is the sole carrier of:
//
//  1. SPEND (F-07). seedAggregateFromAudit reseeds the rolling aggregate cap
//     from the LAST result line's broker-authored total_cost_usd. A late
//     result row would have to restate a cost it did not measure — copying a
//     number out of a closed file to avoid deleting that task's spend from
//     the cap. A row whose only job is to not break accounting is a row that
//     will eventually break accounting.
//
//  2. OUTCOME. OutcomeKey classifies a task from that same last row. A push
//     that landed exactly as asked would start rendering as "ci_failed" in
//     `drydock tasks`, the web-UI history strip, and `drydock stats` — i.e.
//     the task's own success rate would silently become a CI success rate.
//     The task did not fail; its branch's CI did.
//
//  3. F-07's ACTUAL RULE, which is that the broker authors the last result
//     line on every LIFECYCLE EXIT PATH. Every such path already does. The CI
//     watch is not a lifecycle exit path; it runs after all of them.
//
// So the record gets its own type. It is still broker-authored (Src is always
// "broker"), still lives in the per-task audit (so `drydock logs <id>` and the
// web UI log view show it), and is provably inert for every existing reader:
// scanTailForResult requires type=="result" and LastMetricsFile requires
// type=="metrics", so both backward scans skip it and select exactly the rows
// they selected before. The AUTHORITATIVE, machine-readable form of the same
// fact is the durable queue item's terminal state (completed / ci_failed /
// dead_letter) — see broker.QueueAwaitingCI.
//
// It carries NO CI log text (D3). State and Detail are broker-authored
// vocabulary; the counts are broker-computed. There is no field a repository's
// own workflow output could reach.
type CIObservation struct {
	Type   string `json:"type"`
	Src    string `json:"src"`
	TaskID string `json:"task_id"`
	// State is the terminal ciwatch state: passed, failed, no_checks,
	// timed_out, or unknown. It is never empty on a written record, and
	// "passed" is the ONLY value that means CI succeeded.
	State    string `json:"state"`
	PRNumber int    `json:"pr_number,omitempty"`
	PRURL    string `json:"pr_url,omitempty"`
	// QueueState is the durable queue terminal this observation drove, or ""
	// for a synchronous (non-queued) task, which has no queue item at all.
	QueueState string `json:"queue_state,omitempty"`
	Checks     int    `json:"checks"`
	Passed     int    `json:"passed"`
	Failed     int    `json:"failed"`
	Pending    int    `json:"pending"`
	// Detail is short broker-authored English explaining a non-conclusive end
	// (a timeout, a give-up). Empty for a conclusive observation.
	Detail string `json:"detail,omitempty"`
	// Attempt/RetryOf place this observation in its bounded-retry chain
	// (broker.Task's same-named fields): Attempt counts RETRIES, so an
	// operator-submitted task is 0 and the Nth automatic retry is N.
	Attempt int    `json:"attempt,omitempty"`
	RetryOf string `json:"retry_of,omitempty"`
	// RetryTaskID is the bounded retry this observation enqueued, and
	// RetryDetail is the broker's one-line reason when it enqueued none (the
	// bound was reached, the spend cap was exhausted, the retry was refused).
	// Both are broker-authored EVIDENCE for an operator following a chain. The
	// authoritative link is the durable broker.QueueItem.RetryTaskID and the
	// child's own Task.RetryOf, neither of which anything in a VM can write.
	RetryTaskID  string `json:"retry_task_id,omitempty"`
	RetryDetail  string `json:"retry_detail,omitempty"`
	ObservedAtMs int64  `json:"observed_at_ms"`
}

// THERE IS DELIBERATELY NO LastCIObservation READER HERE, and adding one is a
// decision, not a convenience. This package's tail readers exist to feed
// CONTROL DECISIONS — the aggregate-cap reseed, the outcome classification —
// and both fields such a scan could filter on (type, src) are attacker-supplied
// text in an agent-writable file, so anything it returned would be unsafe for
// exactly the callers who would reach for it. The authoritative, machine-
// readable form of this fact is the durable queue item
// (broker.QueueItem.CIState plus its terminal state), which nothing inside the
// VM can write. Render the row from the raw trace if you want to show it; do
// not build a typed reader that invites branching on it.

// LastMetricsFile finds the final broker-authored {"type":"metrics"} line by
// reading only the file tail (same 16KB window as LastResultFile). ok=false
// when absent (pre-metrics trace, interrupted task, or still running).
func LastMetricsFile(f *os.File) (Metrics, bool) {
	info, err := f.Stat()
	if err != nil {
		return Metrics{}, false
	}
	lines, err := tailLines(f, info.Size())
	if err != nil {
		return Metrics{}, false
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var m Metrics
		if json.Unmarshal(lines[i], &m) == nil && m.Type == "metrics" && m.Src == "broker" {
			return m, true
		}
	}
	return Metrics{}, false
}

// LastResultAndMetricsFile finds the final {"type":"result",...} row AND the
// final broker-authored {"type":"metrics",...} row in a single tail read of
// f, instead of the two independent Stat+Seek+16KB reads LastResultFile and
// LastMetricsFile each perform. Same tail window and last-wins semantics as
// each of those; callers that want both rows (handleHistory, tasks.go
// summarize) should prefer this over calling both single-row functions. The
// existing single-row functions stay for callers that only need one.
func LastResultAndMetricsFile(f *os.File) (Result, bool, Metrics, bool) {
	info, err := f.Stat()
	if err != nil {
		return Result{}, false, Metrics{}, false
	}
	lines, err := tailLines(f, info.Size())
	if err != nil {
		return Result{}, false, Metrics{}, false
	}
	var (
		res   Result
		resOK bool
		m     Metrics
		mOK   bool
	)
	for i := len(lines) - 1; i >= 0 && (!resOK || !mOK); i-- {
		line := lines[i]
		if !resOK {
			var x Result
			if json.Unmarshal(line, &x) == nil && x.Type == "result" {
				res, resOK = x, true
				continue
			}
		}
		if !mOK {
			var x Metrics
			if json.Unmarshal(line, &x) == nil && x.Type == "metrics" && x.Src == "broker" {
				m, mOK = x, true
			}
		}
	}
	return res, resOK, m, mOK
}
