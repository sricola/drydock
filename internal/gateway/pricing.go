package gateway

// Price is the USD cost per 1M tokens.
type Price struct {
	InputPer1M  float64
	OutputPer1M float64
}

// DefaultPrices seeds the per-task budget gate. Keys match the model string
// that parseUsage extracts from `message.model` (the base ID, not the modelUsage
// "[1m]" suffix). Numbers are approximate — Anthropic publishes the live rates
// and the 1M-context tier carries a premium not modeled here. The gate is a
// safety cap, not a billing source of truth; tune for your workload.
//
// "default" catches any model not in the table (e.g. when a new release lands
// before the operator updates this file). It's set to the family's high end
// so new models can't accidentally spend past the budget while the table
// catches up.
func DefaultPrices() map[string]Price {
	return map[string]Price{
		// Opus family — frontier tier.
		"claude-opus-4-5": {InputPer1M: 15, OutputPer1M: 75},
		"claude-opus-4-6": {InputPer1M: 15, OutputPer1M: 75},
		"claude-opus-4-7": {InputPer1M: 15, OutputPer1M: 75},
		"claude-opus-4-8": {InputPer1M: 15, OutputPer1M: 75},
		// Sonnet family — balanced tier.
		"claude-sonnet-4-5": {InputPer1M: 3, OutputPer1M: 15},
		"claude-sonnet-4-6": {InputPer1M: 3, OutputPer1M: 15},
		// Haiku family — speed tier.
		"claude-haiku-4-5": {InputPer1M: 1, OutputPer1M: 5},
		// Fallback — conservatively keyed to Opus pricing so unknown models
		// can't overrun budgets while the table catches up.
		"default": {InputPer1M: 15, OutputPer1M: 75},
	}
}

func cost(prices map[string]Price, model string, in, out int) float64 {
	p, ok := prices[model]
	if !ok {
		p = prices["default"]
	}
	return float64(in)/1e6*p.InputPer1M + float64(out)/1e6*p.OutputPer1M
}
