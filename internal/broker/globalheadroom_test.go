package broker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// These tests drive the ceiling's OPERATOR SURFACE (plan G6). The lesson from
// increment B1 is explicit in the task: do not ship a surface that cannot fire,
// and do not document one that does not exist. So every assertion here is
// against real values produced by the real store, and the two that matter most
// are (a) the surface reports the SAME verdict the dispatcher would give, and
// (b) with the ceiling off it reports "off" without touching anything.

// With no limb configured the surface is inert: enabled false, every number
// zero, and — the part that would be easy to get wrong — NO ledger read, so a
// broker with a corrupt or absent store still answers instantly and truthfully.
func TestGlobalHeadroom_OffIsInert(t *testing.T) {
	b := capBroker(t)
	// A ledger that is present and would degrade if read. It must not be.
	b.GlobalLedger = plantLedgerLine(t, b.AuditRoot, time.Hour, "{ this is not json")

	s := b.GlobalCeilingStatus()
	if s.Enabled {
		t.Fatal("Enabled = true with both limbs 0")
	}
	if s.Blocked || s.Reason != "" || s.Degraded || s.LoadError != "" ||
		s.Starts != 0 || s.SpentUSD != 0 || s.Entries != 0 || s.WindowMs != 0 {
		t.Errorf("off ceiling reported state: %+v — it must read nothing", s)
	}
}

// The real numbers, from a real ledger: spend, starts, headroom, window, and
// the entry/damage counts, with the USD limb on and the task limb off.
func TestGlobalHeadroom_ReportsRealValues(t *testing.T) {
	b := capBroker(t)
	b.GlobalBudgetUSD = 50
	b.GlobalMaxTasks = 20
	b.GlobalLedger = capLedgerAt(t, b.AuditRoot, 24*time.Hour, 4, 2.5, capNow-1000)

	s := b.GlobalCeilingStatus()
	if !s.Enabled {
		t.Fatal("Enabled = false with both limbs set")
	}
	if s.BudgetUSD != 50 || s.MaxTasks != 20 {
		t.Errorf("limbs = $%v / %d, want 50/20", s.BudgetUSD, s.MaxTasks)
	}
	if s.SpentUSD != 10 {
		t.Errorf("SpentUSD = %v, want 10 (4 × $2.50)", s.SpentUSD)
	}
	if s.HeadroomUSD != 40 {
		t.Errorf("HeadroomUSD = %v, want 40", s.HeadroomUSD)
	}
	if s.RecordedStarts != 4 || s.InFlightStarts != 0 || s.Starts != 4 || s.HeadroomStarts != 16 {
		t.Errorf("starts = %d recorded / %d inflight / %d total / %d headroom, want 4/0/4/16",
			s.RecordedStarts, s.InFlightStarts, s.Starts, s.HeadroomStarts)
	}
	if s.Entries != 4 || s.Damaged != 0 {
		t.Errorf("entries/damaged = %d/%d, want 4/0", s.Entries, s.Damaged)
	}
	if s.WindowMs != (24*time.Hour).Milliseconds() || !strings.Contains(s.Window, "24h") {
		t.Errorf("window = %d ms / %q, want 24h", s.WindowMs, s.Window)
	}
	if s.Blocked {
		t.Errorf("Blocked = true well under both limbs: %s", s.Reason)
	}
	if s.Agent != "claude" {
		t.Errorf("Agent = %q, want the daemon's default_agent", s.Agent)
	}
}

// IN-FLIGHT STARTS ARE COUNTED. The ledger is written at task TERMINAL, so a
// headroom built from the ledger alone reports a smaller number than the
// ceiling enforces — an operator watching it would see "3 of 5" while the
// dispatcher parks. Starts must be recorded + in-flight, the sum admission
// compares.
func TestGlobalHeadroom_CountsInFlightStarts(t *testing.T) {
	b := capBroker(t)
	b.GlobalMaxTasks = 5
	b.GlobalLedger = capLedgerAt(t, b.AuditRoot, time.Hour, 2, 0, capNow-1000)

	if blocked, why := b.admitGlobalStart("live-1", "claude"); blocked {
		t.Fatalf("admission refused: %s", why)
	}
	s := b.GlobalCeilingStatus()
	if s.RecordedStarts != 2 || s.InFlightStarts != 1 || s.Starts != 3 || s.HeadroomStarts != 2 {
		t.Fatalf("starts = %d/%d/%d headroom %d, want 2 recorded + 1 in flight = 3, headroom 2",
			s.RecordedStarts, s.InFlightStarts, s.Starts, s.HeadroomStarts)
	}
	b.releaseGlobalStart("live-1")
	if s := b.GlobalCeilingStatus(); s.InFlightStarts != 0 || s.Starts != 2 {
		t.Errorf("after release: %d in flight / %d starts, want 0/2", s.InFlightStarts, s.Starts)
	}
}

// THE VERDICT COMES FROM THE ENFORCEMENT PATH. A surface that recomputed
// "blocked" from the same numbers could drift; this asserts the two agree in
// both directions, including the exact reason string an admission would give.
func TestGlobalHeadroom_VerdictMatchesEnforcement(t *testing.T) {
	b := capBroker(t)
	b.GlobalMaxTasks = 3
	b.GlobalLedger = capLedgerAt(t, b.AuditRoot, time.Hour, 3, 0, capNow-1000)

	s := b.GlobalCeilingStatus()
	blocked, reason := b.globalCeilingExceeded("claude")
	if !blocked {
		t.Fatal("the ceiling admits at 3 of 3; the fixture is wrong")
	}
	if !s.Blocked || s.Reason != reason {
		t.Errorf("surface says blocked=%v %q; enforcement says blocked=%v %q",
			s.Blocked, s.Reason, blocked, reason)
	}
	if s.HeadroomStarts != 0 {
		t.Errorf("HeadroomStarts = %d at the cap, want 0 (floored, never negative)", s.HeadroomStarts)
	}
}

// The honesty flags reach the surface: a quarantined ledger line degrades the
// USD number, and an operator must be able to see that the figure beside the
// refusal is a LOWER BOUND rather than a measurement.
func TestGlobalHeadroom_SurfacesDegraded(t *testing.T) {
	b := capBroker(t)
	b.GlobalBudgetUSD = 10
	b.GlobalLedger = plantLedgerLine(t, b.AuditRoot, time.Hour, "{ this is not json")

	s := b.GlobalCeilingStatus()
	if !s.Degraded || s.DegradedReason == "" {
		t.Fatalf("Degraded = %v %q, want the quarantine reason surfaced", s.Degraded, s.DegradedReason)
	}
	if s.Damaged != 1 || s.Starts != 1 {
		t.Errorf("damaged/starts = %d/%d, want 1/1 (a quarantined line is still one start)", s.Damaged, s.Starts)
	}
	if !s.Blocked {
		t.Error("Blocked = false with the enforced USD limb degraded — the surface must not read healthier than the ceiling")
	}
}

// Total mode renders as total, not as "the last 0s".
func TestGlobalHeadroom_TotalModeWindowLabel(t *testing.T) {
	b := capBroker(t)
	b.GlobalMaxTasks = 5
	b.GlobalLedger = capLedgerAt(t, b.AuditRoot, 0, 1, 0, capNow-1000)
	s := b.GlobalCeilingStatus()
	if s.WindowMs != 0 || !strings.Contains(s.Window, "total") {
		t.Errorf("window = %d / %q, want total mode", s.WindowMs, s.Window)
	}
}

// A configured limb with NO store answers fail-closed here too: it is the same
// "I don't know" the ceiling turns into a refusal (G2), and the surface must
// say so rather than reporting a comfortable zero.
func TestGlobalHeadroom_NilLedgerReadsBlocked(t *testing.T) {
	b := capBroker(t)
	b.GlobalMaxTasks = 5
	s := b.GlobalCeilingStatus()
	if !s.Enabled {
		t.Fatal("Enabled = false with a limb set")
	}
	if !s.Blocked {
		t.Error("Blocked = false with a configured limb and no ledger; the ceiling refuses in that state")
	}
}

// The HTTP surface: GET /admin/ceiling serves the same struct as JSON.
func TestHandleCeiling_ServesTheHeadroom(t *testing.T) {
	b := capBroker(t)
	b.GlobalBudgetUSD = 20
	b.GlobalLedger = capLedgerAt(t, b.AuditRoot, time.Hour, 2, 3, capNow-1000)

	rec := httptest.NewRecorder()
	b.HandleCeiling(rec, httptest.NewRequest(http.MethodGet, "/admin/ceiling", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got GlobalCeilingStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
	}
	if !got.Enabled || got.SpentUSD != 6 || got.BudgetUSD != 20 || got.HeadroomUSD != 14 {
		t.Errorf("body = %+v, want enabled with $6 of $20 spent and $14 left", got)
	}
	// The off case must serialise as an unambiguous "off" rather than as a
	// zeroed reading someone could mistake for an empty ledger.
	off := capBroker(t)
	rec = httptest.NewRecorder()
	off.HandleCeiling(rec, httptest.NewRequest(http.MethodGet, "/admin/ceiling", nil))
	if !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Errorf("off body = %s, want enabled:false", rec.Body.String())
	}
}
