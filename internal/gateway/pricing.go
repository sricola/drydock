package gateway

// Price is the USD cost per 1M tokens. CacheWritePer1M and CacheReadPer1M
// price the prompt-cache tokens a vendor reports separately from input
// (Usage.CacheWrite / CacheRead); zero means "no separate rate", and cost then
// charges those tokens at the input rate rather than at nothing.
type Price struct {
	InputPer1M      float64
	OutputPer1M     float64
	CacheWritePer1M float64
	CacheReadPer1M  float64
}

// anthropic builds an Anthropic price: cache writes bill at 1.25x and cache
// reads at 0.1x the input rate across the 4.x families.
func anthropic(in, out float64) Price {
	return Price{InputPer1M: in, OutputPer1M: out, CacheWritePer1M: in * 1.25, CacheReadPer1M: in * 0.10}
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
		"claude-opus-4-5": anthropic(15, 75),
		"claude-opus-4-6": anthropic(15, 75),
		"claude-opus-4-7": anthropic(15, 75),
		"claude-opus-4-8": anthropic(15, 75),
		// Sonnet family — balanced tier.
		"claude-sonnet-4-5": anthropic(3, 15),
		"claude-sonnet-4-6": anthropic(3, 15),
		// Haiku family — speed tier.
		"claude-haiku-4-5": anthropic(1, 5),
		// Fallback — conservatively keyed to Opus pricing so unknown models
		// can't overrun budgets while the table catches up.
		"default": anthropic(15, 75),
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
		"default":     anthropic(1.25, 10),
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
		"default":               anthropic(1.25, 10),
	}
}

// cost prices one response. A cache token is never free: a table without a
// cache rate (operator-supplied openai_compat prices) charges it at the input
// rate, the conservative direction for a safety cap.
func cost(prices map[string]Price, model string, u Usage) float64 {
	p, ok := prices[model]
	if !ok {
		p = prices["default"]
	}
	cw, cr := p.CacheWritePer1M, p.CacheReadPer1M
	if cw == 0 {
		cw = p.InputPer1M
	}
	if cr == 0 {
		cr = p.InputPer1M
	}
	return float64(u.Input)/1e6*p.InputPer1M + float64(u.Output)/1e6*p.OutputPer1M +
		float64(u.CacheWrite)/1e6*cw + float64(u.CacheRead)/1e6*cr
}
