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
