package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// THE GLOBAL CEILING'S CLI HEADROOM SURFACE (plan G6).
//
// `drydock stats` is the natural home for it: it is already the "how much has
// this cost me" command, and the ceiling is the answer to the next question an
// operator asks there — "how much have I got left".
//
// It is fetched from the DAEMON, not recomputed from the audit dir, and that is
// deliberate even though the rest of `stats` reads the audit dir directly and
// works with brokerd stopped. Three of the numbers exist only in the running
// process: the in-flight starts that have been admitted but not recorded, the
// live degraded flags, and the verdict itself. A CLI-side re-derivation would
// disagree with the daemon exactly when it matters (mid-dispatch, or with a
// damaged ledger), and a headroom figure that disagrees with the enforcement is
// worse than no headroom figure at all.
//
// So: brokerd down -> the section is OMITTED and stats renders exactly as it
// did before (no fabricated zeroes, no scary banner on a command that is
// legitimately usable offline). Ceiling off -> also omitted, because a feature
// that is off has no headroom to report.

// ceilingStatus mirrors GET /admin/ceiling's body (broker.GlobalCeilingStatus).
// Redeclared rather than imported for the same reason taskState is: the CLI
// does not import the broker package, and the JSON contract is the interface.
type ceilingStatus struct {
	Enabled bool `json:"enabled"`

	BudgetUSD float64 `json:"budget_usd"`
	MaxTasks  int     `json:"max_tasks"`
	WindowMs  int64   `json:"window_ms"`
	Window    string  `json:"window"`

	SpentUSD     float64 `json:"spent_usd"`
	HeadroomUSD  float64 `json:"headroom_usd"`
	UntrustedUSD float64 `json:"untrusted_usd"`

	RecordedStarts int `json:"recorded_starts"`
	InFlightStarts int `json:"in_flight_starts"`
	Starts         int `json:"starts"`
	HeadroomStarts int `json:"headroom_starts"`

	Entries int `json:"entries"`
	Damaged int `json:"damaged"`

	Degraded             bool   `json:"degraded"`
	DegradedReason       string `json:"degraded_reason,omitempty"`
	StartsDegraded       bool   `json:"starts_degraded"`
	StartsDegradedReason string `json:"starts_degraded_reason,omitempty"`
	LoadError            string `json:"load_error,omitempty"`

	Blocked bool   `json:"blocked"`
	Reason  string `json:"reason,omitempty"`
	Agent   string `json:"agent,omitempty"`
}

// fetchCeiling asks the running brokerd for the ceiling's live headroom. Short
// timeout for the same reason fetchLivePolicy has one: the audit-derived report
// is the primary output of `stats` and must not hang behind a sick daemon.
//
// Every failure — daemon down, 404 on an older daemon that has no such route,
// an unparseable body — returns an error and the caller simply omits the
// section. There is no partial rendering: a headroom line built from a failed
// read would be a number an operator might act on.
func fetchCeiling() (*ceilingStatus, error) {
	c, base := brokerClient()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/admin/ceiling", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /admin/ceiling: brokerd returned %s", resp.Status)
	}
	var out ceilingStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return nil, fmt.Errorf("parse /admin/ceiling: %w", err)
	}
	return &out, nil
}

// renderCeiling writes the headroom block. Nothing is written when the status
// is absent (daemon unreachable) or the ceiling is off — `drydock stats` on a
// stock install is byte-identical to what it printed before this existed.
//
// Every rendered string that came off the wire goes through safeCell: the
// reasons are broker-authored, but they are reflected into a terminal and this
// column follows the same rule as every other one.
func renderCeiling(w io.Writer, s *ceilingStatus) {
	if s == nil || !s.Enabled {
		return
	}
	fmt.Fprintf(w, "\nglobal ceiling (%s):\n", safeCell(s.Window))
	if s.BudgetUSD > 0 {
		fmt.Fprintf(w, "  spend:  $%.2f of $%.2f broker-metered — $%.2f left\n",
			s.SpentUSD, s.BudgetUSD, s.HeadroomUSD)
		if s.UntrustedUSD > 0 {
			fmt.Fprintf(w, "          (+$%.2f on unmetered lanes, not counted against the limb)\n", s.UntrustedUSD)
		}
	}
	if s.MaxTasks > 0 {
		fmt.Fprintf(w, "  starts: %d of %d — %d left (%d recorded, %d in flight)\n",
			s.Starts, s.MaxTasks, s.HeadroomStarts, s.RecordedStarts, s.InFlightStarts)
	}
	// The honesty flags. Each says a number above is a LOWER BOUND, which is
	// also why the ceiling refuses on it — an operator who sees "$3 of $50" and
	// a refusal needs the two facts side by side or the daemon looks broken.
	if s.Degraded {
		fmt.Fprintf(w, "  DEGRADED: the spend total is a lower bound — %s\n", safeCell(s.DegradedReason))
	}
	if s.StartsDegraded {
		fmt.Fprintf(w, "  DEGRADED: the start count is a lower bound — %s\n", safeCell(s.StartsDegradedReason))
	}
	if s.LoadError != "" {
		fmt.Fprintf(w, "  DEGRADED: the ledger could not be read in full — %s\n", safeCell(s.LoadError))
	}
	if s.Damaged > 0 {
		fmt.Fprintf(w, "  %d quarantined ledger entr(ies) in window (counted as starts; their dollars are unknown)\n", s.Damaged)
	}
	if s.Blocked {
		fmt.Fprintf(w, "  BLOCKED: %s\n", safeCell(s.Reason))
	}
}
