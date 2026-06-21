package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type renderMode int

const (
	modeTTY renderMode = iota
	modePiped
	modeQuiet
)

// renderer turns a stream of broker events into terminal output. It holds the
// task id needed for summary and error lines; piped/quiet modes are stateless
// line printers and are what the tests exercise.
type renderer struct {
	w    io.Writer
	mode renderMode
	id   string
}

func newRenderer(w io.Writer, mode renderMode) *renderer {
	return &renderer{w: w, mode: mode}
}

// progress prints a transient status line (suppressed in quiet mode).
func (r *renderer) progress(s string) {
	if r.mode == modeQuiet {
		return
	}
	if r.mode == modeTTY {
		fmt.Fprintf(r.w, "\r\033[2K  %s", s)
		return
	}
	fmt.Fprintf(r.w, "  %s\n", s)
}

// persist prints a line that stays on screen (approval block, summary, error).
// On a TTY it first clears the transient progress line.
func (r *renderer) persist(s string) {
	if r.mode == modeTTY {
		fmt.Fprintf(r.w, "\r\033[2K%s\n", s)
		return
	}
	fmt.Fprintf(r.w, "%s\n", s)
}

// handle renders one event and reports the exit code + whether the stream is
// terminal.
func (r *renderer) handle(ev map[string]any) (exit int, done bool) {
	switch ev["event"] {
	case "accepted":
		r.id, _ = ev["task_id"].(string)
		r.progress("task " + short(r.id) + " accepted")
	case "stage":
		r.stage(ev)
	case "result":
		r.summary(ev)
		return 0, true
	case "error":
		r.errorOut(ev)
		return 1, true
	}
	return 0, false
}

func (r *renderer) stage(ev map[string]any) {
	switch ev["stage"] {
	case "awaiting_egress":
		r.persist("⏸ awaiting egress approval · " + str(ev["extras"]))
		r.persist("   approve: " + str(ev["approve"]) + "     deny: " + str(ev["deny"]))
	case "preparing":
		r.progress("preparing · cloning repo")
	case "running":
		r.progress("running · " + str(ev["agent"]) + " working")
	case "awaiting_approval":
		r.persist(fmt.Sprintf("⏸ awaiting approval · %s diff (%d files)", humanBytes(int64(num(ev["diff_bytes"]))), num(ev["files"])))
		if rv := str(ev["review"]); rv != "" {
			r.persist("   review:  " + rv)
		}
		r.persist("   approve: " + str(ev["approve"]) + "     deny: " + str(ev["deny"]) + "     (^C aborts)")
	case "pushing":
		r.progress("pushing → " + str(ev["branch"]))
	}
}

func (r *renderer) summary(ev map[string]any) {
	id := short(r.id)
	switch ev["outcome"] {
	case "pushed":
		stat := fmt.Sprintf("%d files +%d/-%d", num(ev["files"]), num(ev["insertions"]), num(ev["deletions"]))
		r.persist(fmt.Sprintf("✓ pushed %s (%s) · %s · %s%s",
			str(ev["branch"]), str(ev["platform"]), stat, durStr(ev["duration_ms"]), costStr(ev["cost_usd"])))
	case "no_diff":
		r.persist(fmt.Sprintf("✓ task %s finished · no changes%s", id, costStr(ev["cost_usd"])))
	case "denied":
		r.persist(fmt.Sprintf("task %s: diff denied — not pushed", id))
	case "cancelled":
		r.persist(fmt.Sprintf("task %s: cancelled", id))
	default:
		r.persist(fmt.Sprintf("task %s: %v", id, ev["outcome"]))
	}
}

func (r *renderer) errorOut(ev map[string]any) {
	r.persist("✗ task " + short(r.id) + " failed: " + str(ev["reason"]))
	if h := str(ev["hint"]); h != "" {
		r.persist("   → " + h)
	}
	if a := str(ev["audit"]); a != "" {
		r.persist("   audit: " + a)
	}
}

// consume reads the NDJSON event stream (or a legacy single object) from r,
// renders it to w, and returns the process exit code.
func consume(r io.Reader, w io.Writer, mode renderMode) int {
	rnd := newRenderer(w, mode)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			fmt.Fprintln(w, line) // not JSON — pass through defensively
			continue
		}
		if _, ok := ev["event"]; !ok {
			printPretty(w, ev) // legacy single-object response from an old brokerd
			return 0
		}
		if exit, done := rnd.handle(ev); done {
			return exit
		}
	}
	if err := sc.Err(); err != nil {
		rnd.persist("✗ stream error: " + err.Error())
		return 1
	}
	// Stream ended with no terminal event — brokerd likely died mid-task.
	rnd.persist("✗ connection closed before the task finished")
	return 1
}

// consumeJSON streams the raw NDJSON body to w (for --json: one JSON object per
// line, untouched) while tracking the terminal event so the exit code still
// reflects task success/failure. A streamed failure is HTTP 200 + an `error`
// event, so the caller cannot key the exit code on the HTTP status alone.
// Returns 0 on a `result` (or a legacy single-object response), 1 on an `error`
// event or if the stream ends with no terminal event (brokerd died mid-task).
func consumeJSON(r io.Reader, w io.Writer) int {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	exit := 1 // no terminal event seen ⇒ treat as failure
	for sc.Scan() {
		line := sc.Text()
		fmt.Fprintln(w, line) // raw passthrough — preserve the exact NDJSON
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(trimmed), &ev) != nil {
			continue
		}
		if _, ok := ev["event"]; !ok {
			return 0 // legacy single-object response from an old brokerd (200 = success)
		}
		switch ev["event"] {
		case "result":
			exit = 0
		case "error":
			exit = 1
		}
	}
	if sc.Err() != nil {
		return 1
	}
	return exit
}

// --- small formatting helpers ---

func short(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) int {
	if f, ok := v.(float64); ok { // JSON numbers decode to float64
		return int(f)
	}
	return 0
}

func durStr(v any) string {
	ms := num(v)
	if ms <= 0 {
		return "0s"
	}
	return shortDur(int64(ms))
}

func costStr(v any) string {
	f, ok := v.(float64)
	if !ok || f <= 0 {
		return ""
	}
	return fmt.Sprintf(" · $%.2f", f)
}
