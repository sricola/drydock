package gateway

import (
	"math"
	"testing"
)

func TestCost_KnownModel(t *testing.T) {
	prices := map[string]Price{"claude-x": {InputPer1M: 3, OutputPer1M: 15}}
	got := cost(prices, "claude-x", 1_000_000, 2_000_000)
	want := 3.0 + 30.0
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("cost = %v, want %v", got, want)
	}
}

func TestCost_FallsBackToDefault(t *testing.T) {
	prices := map[string]Price{"default": {InputPer1M: 10, OutputPer1M: 10}}
	got := cost(prices, "unknown-model", 500_000, 0)
	if math.Abs(got-5.0) > 1e-9 {
		t.Errorf("cost = %v, want 5.0", got)
	}
}

func TestGooglePrices_MetersKnownAndDefault(t *testing.T) {
	p := GooglePrices()
	if _, ok := p["gemini-2.5-pro"]; !ok {
		t.Fatal("missing gemini-2.5-pro")
	}
	if _, ok := p["default"]; !ok {
		t.Fatal("missing default fallback")
	}
	// 1M in + 1M out on Pro = 1.25 + 10 = 11.25
	if got := cost(p, "gemini-2.5-pro", 1_000_000, 1_000_000); got != 11.25 {
		t.Errorf("pro cost = %v, want 11.25", got)
	}
	// Unknown model falls back to default (Pro high end), not $0.
	if got := cost(p, "gemini-9-ultra", 1_000_000, 0); got != 1.25 {
		t.Errorf("unknown-model input cost = %v, want 1.25 (default)", got)
	}
}

// The seeded table must (a) cover the current 4.x families, (b) have a
// fallback that's at least as expensive as the priciest known model, so an
// unknown release can't accidentally undercount and overrun the budget.
func TestAnthropicPrices_CoversFamiliesAndFailsConservative(t *testing.T) {
	p := AnthropicPrices()
	mustHave := []string{
		"claude-opus-4-7", "claude-opus-4-8",
		"claude-sonnet-4-6",
		"claude-haiku-4-5",
		"default",
	}
	for _, k := range mustHave {
		if _, ok := p[k]; !ok {
			t.Errorf("DefaultPrices missing key %q", k)
		}
	}
	def := p["default"]
	for k, v := range p {
		if k == "default" {
			continue
		}
		if v.InputPer1M > def.InputPer1M || v.OutputPer1M > def.OutputPer1M {
			t.Errorf("default ($%.0f/$%.0f) is cheaper than known %q ($%.0f/$%.0f) — unknown models could overrun budget",
				def.InputPer1M, def.OutputPer1M, k, v.InputPer1M, v.OutputPer1M)
		}
	}
}
