package gateway

import (
	"math"
	"testing"
)

// Anthropic bills prompt-cache writes at 1.25x and cache reads at 0.1x the
// input rate, and reports them in fields SEPARATE from input_tokens. A
// metering that reads only input_tokens under-counts every cached turn, which
// for a coding agent is most of them. The parser must surface both fields.
func TestParseAnthropicUsage_CacheTokens_JSON(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","usage":{"input_tokens":12,"output_tokens":34,` +
		`"cache_creation_input_tokens":1000,"cache_read_input_tokens":5000}}`)
	u, ok := parseAnthropicUsage(body, "application/json")
	if !ok {
		t.Fatal("ok=false")
	}
	if u.Model != "claude-sonnet-4-5" || u.Input != 12 || u.Output != 34 || u.CacheWrite != 1000 || u.CacheRead != 5000 {
		t.Fatalf("got %+v", u)
	}
}

func TestParseAnthropicUsage_CacheTokens_SSE(t *testing.T) {
	body := []byte(
		"event: message_start\n" +
			`data: {"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":10,"output_tokens":1,"cache_creation_input_tokens":200,"cache_read_input_tokens":3000}}}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","usage":{"output_tokens":42}}` + "\n\n")
	u, ok := parseAnthropicUsage(body, "text/event-stream")
	if !ok {
		t.Fatal("ok=false")
	}
	if u.Input != 10 || u.Output != 42 || u.CacheWrite != 200 || u.CacheRead != 3000 {
		t.Fatalf("got %+v", u)
	}
}

// A response whose only tokens are cache reads (input_tokens 0) is still a
// billable response and must still parse.
func TestParseAnthropicUsage_CacheOnlyStillParses(t *testing.T) {
	body := []byte(`{"model":"claude-x","usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":777}}`)
	u, ok := parseAnthropicUsage(body, "application/json")
	if !ok || u.CacheRead != 777 {
		t.Fatalf("got %+v ok=%v", u, ok)
	}
}

// cost prices cache tokens at the table's cache rates. Sonnet 4.5 is $3 per
// 1M input, so a cache write is $3.75 and a cache read is $0.30 per 1M.
func TestCost_CacheTokensAtCacheRates(t *testing.T) {
	got := cost(AnthropicPrices(), "claude-sonnet-4-5", Usage{Input: 1_000_000, Output: 0, CacheWrite: 1_000_000, CacheRead: 1_000_000})
	want := 3.0 + 3.75 + 0.30
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("cost = %v, want %v", got, want)
	}
}

// A price table with no cache rates (operator-supplied openai_compat prices)
// must never meter a cache token at zero: it falls back to the input rate,
// which is the conservative direction for a safety cap.
func TestCost_CacheTokensWithoutCacheRateFallBackToInputRate(t *testing.T) {
	prices := map[string]Price{"m": {InputPer1M: 2, OutputPer1M: 8}}
	got := cost(prices, "m", Usage{CacheWrite: 500_000, CacheRead: 500_000})
	if math.Abs(got-2.0) > 1e-9 {
		t.Errorf("cost = %v, want 2.0 (1M cache tokens at the $2 input rate)", got)
	}
}

func TestAnthropicPrices_EveryModelHasCacheRates(t *testing.T) {
	for model, p := range AnthropicPrices() {
		if math.Abs(p.CacheWritePer1M-p.InputPer1M*1.25) > 1e-9 || math.Abs(p.CacheReadPer1M-p.InputPer1M*0.10) > 1e-9 {
			t.Errorf("%s: cache rates %v/%v, want 1.25x/0.10x of input %v", model, p.CacheWritePer1M, p.CacheReadPer1M, p.InputPer1M)
		}
	}
}
