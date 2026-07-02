package broker

import (
	"encoding/json"
	"net/http"
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
// ignored on purpose — task cancellation is driven by /admin/kill or brokerd
// shutdown (CancelAll), never by the success of this write.
func (s *stream) emit(ev map[string]any) {
	_ = s.enc.Encode(ev)
	if s.f != nil {
		s.f.Flush()
	}
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
