package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"drydock/internal/broker"
)

// END TO END OVER THE WIRE, against the REAL handler.
//
// ceilingStatus is a hand-written mirror of broker.GlobalCeilingStatus (the CLI
// does not import the broker package, exactly like taskState), so the JSON
// contract is the only thing holding the two together — and a silently renamed
// key would make `drydock stats` print zeroes for a ceiling that is doing its
// job. This drives fetchCeiling against a live GET /admin/ceiling and asserts
// every field an operator reads survives the round trip.
func TestFetchCeiling_MirrorsTheBrokerContract(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	b := &broker.Broker{
		AuditRoot:       t.TempDir(),
		DefaultAgent:    "claude",
		MaxConcurrent:   2,
		GlobalBudgetUSD: 50,
		GlobalMaxTasks:  20,
	}
	now := time.Now().UnixMilli()
	lg, err := broker.OpenGlobalLedger(b.AuditRoot, 24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	b.GlobalLedger = lg
	if err := lg.Record(now, broker.GlobalEntry{
		Kind: "task", TaskID: strings.Repeat("a", 32), EndedAtMs: now,
		Vendor: "anthropic", Agent: "claude", Auth: "api_key",
		Metered: true, USD: 12.5, USDTrusted: true, Outcome: "pushed",
	}); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/ceiling", b.HandleCeiling)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("BROKER_ADDR", strings.TrimPrefix(srv.URL, "http://"))

	got, err := fetchCeiling()
	if err != nil {
		t.Fatalf("fetchCeiling: %v", err)
	}
	want := b.GlobalCeilingStatus()
	if !got.Enabled || got.BudgetUSD != want.BudgetUSD || got.SpentUSD != want.SpentUSD ||
		got.HeadroomUSD != want.HeadroomUSD || got.MaxTasks != want.MaxTasks ||
		got.Starts != want.Starts || got.RecordedStarts != want.RecordedStarts ||
		got.InFlightStarts != want.InFlightStarts || got.HeadroomStarts != want.HeadroomStarts ||
		got.WindowMs != want.WindowMs || got.Window != want.Window ||
		got.Entries != want.Entries || got.Degraded != want.Degraded {
		t.Fatalf("wire mismatch — a renamed json key would look exactly like this:\n got %+v\nwant %+v", *got, want)
	}
	if got.SpentUSD != 12.5 || got.HeadroomUSD != 37.5 || got.Starts != 1 || got.HeadroomStarts != 19 {
		t.Errorf("decoded values = %+v, want $12.50 spent / $37.50 left / 1 start / 19 left", *got)
	}

	// And the section renders from LIVE fetched data, not just from a literal.
	var sb strings.Builder
	renderCeiling(&sb, got)
	out := sb.String()
	if !strings.Contains(out, "$12.50 of $50.00 broker-metered — $37.50 left") ||
		!strings.Contains(out, "starts: 1 of 20 — 19 left") {
		t.Errorf("renderCeiling on live data:\n%s", out)
	}
}

// A daemon with the ceiling OFF answers enabled:false, which must decode
// cleanly and render nothing — the stock-install path.
func TestFetchCeiling_OffDecodesAndRendersNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	b := &broker.Broker{AuditRoot: t.TempDir(), DefaultAgent: "claude"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/ceiling", b.HandleCeiling)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("BROKER_ADDR", strings.TrimPrefix(srv.URL, "http://"))

	got, err := fetchCeiling()
	if err != nil {
		t.Fatalf("fetchCeiling: %v", err)
	}
	if got.Enabled {
		t.Fatal("enabled with no limb configured")
	}
	var sb strings.Builder
	renderCeiling(&sb, got)
	if sb.Len() != 0 {
		t.Errorf("ceiling off but stats printed %q", sb.String())
	}
}

// An older daemon with no /admin/ceiling route (404) yields an error, so the
// section is omitted entirely — never rendered as a zeroed reading an operator
// could mistake for "plenty of headroom".
func TestFetchCeiling_UnknownRouteIsAnError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	t.Setenv("BROKER_ADDR", strings.TrimPrefix(srv.URL, "http://"))
	if _, err := fetchCeiling(); err == nil {
		t.Error("fetchCeiling returned no error against a daemon with no ceiling route")
	}
}
