package broker

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// stream writes newline-delimited JSON events to the POST /tasks response and
// flushes after each, so the submit client renders progress live instead of
// blocking on a single response at the end.
type stream struct {
	enc *json.Encoder
	f   http.Flusher
}

// newStream commits the response to a 200 NDJSON stream. Call it only after all
// pre-accept validation passes: once the header is written the status can no
// longer change, so every later exit must emit a terminal event, not http.Error.
func newStream(w http.ResponseWriter) *stream {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	return &stream{enc: json.NewEncoder(w), f: f}
}

// emit writes one event line and flushes. Encode errors (client gone) are
// ignored on purpose — task cancellation is driven by the request context, not
// by the success of this write.
func (s *stream) emit(ev map[string]any) {
	_ = s.enc.Encode(ev)
	if s.f != nil {
		s.f.Flush()
	}
}

var progressLine = regexp.MustCompile(`^\[\d+/\d+\]`)

// reasonFromAudit returns the last human-meaningful line of the audit log — the
// line that explains a boot failure (e.g. an entrypoint error). It skips empty
// lines, container progress lines ("[6/6] …"), and JSON event lines. ok is
// false when nothing meaningful is found, so the caller falls back to safeErr.
func reasonFromAudit(path string) (line string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		ln := strings.TrimSpace(lines[i])
		if ln == "" || strings.HasPrefix(ln, "{") || progressLine.MatchString(ln) {
			continue
		}
		return ln, true
	}
	return "", false
}

// auditCost returns total_cost_usd of the last result line in the audit log —
// the same value `drydock tasks` reports (see cmd/drydock/tasks.go) — so the
// submit summary agrees with it. Returns 0 when no result line is present.
func auditCost(path string) float64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var cost float64
	for _, ln := range strings.Split(string(data), "\n") {
		if !strings.Contains(ln, `"result"`) {
			continue
		}
		var r struct {
			Type         string  `json:"type"`
			TotalCostUSD float64 `json:"total_cost_usd"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(ln)), &r) == nil && r.Type == "result" {
			cost = r.TotalCostUSD
		}
	}
	return cost
}

// diffStat counts files changed and lines added/removed in a unified diff.
// Approximate — binary files and pure renames have no +/- content lines — which
// is fine for a one-line summary.
func diffStat(diff string) (files, insertions, deletions int) {
	for _, ln := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(ln, "diff --git "):
			files++
		case strings.HasPrefix(ln, "+++ "), strings.HasPrefix(ln, "--- "):
			// file headers, not content
		case strings.HasPrefix(ln, "+"):
			insertions++
		case strings.HasPrefix(ln, "-"):
			deletions++
		}
	}
	return
}
