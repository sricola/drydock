package gateway

// Price is the USD cost per 1M tokens.
type Price struct {
	InputPer1M  float64
	OutputPer1M float64
}

// AnthropicPrices seeds the per-task budget gate. Keys match the model string
// that parseAnthropicUsage extracts from `message.model` (the base ID, not the
// modelUsage "[1m]" suffix). Numbers are approximate — Anthropic publishes the
// live rates and the 1M-context tier carries a premium not modeled here. The
// gate is a safety cap, not a billing source of truth; tune for your workload.
//
// "default" catches any model not in the table (e.g. when a new release lands
// before the operator updates this file). It's set to the family's high end
// so new models can't accidentally spend past the budget while the table
// catches up.
func AnthropicPrices() map[string]Price {
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

// OpenAIPrices seeds the per-task budget gate for Codex tasks. USD per 1M
// tokens, approximate (OpenAI publishes live rates). "default" is the family
// high end so a new model can't overrun the budget before this table catches
// up. Tune for your workload — the gate is a safety cap, not billing truth.
func OpenAIPrices() map[string]Price {
	return map[string]Price{
		"gpt-5":       {InputPer1M: 1.25, OutputPer1M: 10},
		"gpt-5-codex": {InputPer1M: 1.25, OutputPer1M: 10},
		"gpt-5-mini":  {InputPer1M: 0.25, OutputPer1M: 2},
		"o4-mini":     {InputPer1M: 1.1, OutputPer1M: 4.4},
		"default":     {InputPer1M: 1.25, OutputPer1M: 10},
	}
}

// GooglePrices seeds the per-task budget gate for Gemini tasks. USD per 1M
// tokens, approximate (≤200k-context tier; the >200k tier carries a premium not
// modeled here). "default" is keyed to Pro (the family high end) so a new model
// can't overrun the budget before this table catches up. Keys match the
// modelVersion string parseGoogleUsage extracts.
func GooglePrices() map[string]Price {
	return map[string]Price{
		"gemini-2.5-pro":        {InputPer1M: 1.25, OutputPer1M: 10},
		"gemini-2.5-flash":      {InputPer1M: 0.30, OutputPer1M: 2.50},
		"gemini-2.5-flash-lite": {InputPer1M: 0.10, OutputPer1M: 0.40},
		"default":               {InputPer1M: 1.25, OutputPer1M: 10},
	}
}

func cost(prices map[string]Price, model string, in, out int) float64 {
	p, ok := prices[model]
	if !ok {
		p = prices["default"]
	}
	return float64(in)/1e6*p.InputPer1M + float64(out)/1e6*p.OutputPer1M
}
