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

// The seeded table must (a) cover the current 4.x families, (b) have a
// fallback that's at least as expensive as the priciest known model, so an
// unknown release can't accidentally undercount and overrun the budget.
func TestDefaultPrices_CoversFamiliesAndFailsConservative(t *testing.T) {
	p := DefaultPrices()
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
