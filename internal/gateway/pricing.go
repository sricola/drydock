package gateway

// Price is the USD cost per 1M tokens.
type Price struct {
	InputPer1M  float64
	OutputPer1M float64
}

// DefaultPrices is a coarse table; "default" is the fallback for unknown models.
// Tune to your actual model mix — this only gates the per-task budget.
func DefaultPrices() map[string]Price {
	return map[string]Price{
		"default": {InputPer1M: 3, OutputPer1M: 15},
	}
}

func cost(prices map[string]Price, model string, in, out int) float64 {
	p, ok := prices[model]
	if !ok {
		p = prices["default"]
	}
	return float64(in)/1e6*p.InputPer1M + float64(out)/1e6*p.OutputPer1M
}
