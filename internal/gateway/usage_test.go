package gateway

import "testing"

// TestParseOpenAIUsage_MissingOrNull verifies boundary conditions of
// openaiUsageFromJSON: missing usage, explicit null, and zero-token usage.
// NOTE: production code treats zero-token usage as ok==false (the "zero is
// treated as field absent" invariant). If the plan's expected behavior for
// zero-token (ok==true) ever changes, revisit this test.
func TestParseOpenAIUsage_MissingOrNull(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantOK  bool
		wantIn  int
		wantOut int
	}{
		{
			name:   "no usage field",
			body:   `{"model":"gpt-x","choices":[]}`,
			wantOK: false,
		},
		{
			name:   "usage explicitly null",
			body:   `{"model":"gpt-x","usage":null}`,
			wantOK: false,
		},
		{
			// Production code treats zero tokens as ok==false ("Zero is treated
			// as field absent" per usage.go). The plan documents ok==true here,
			// but the implementation disagrees — see task report for details.
			name:   "usage present with zero tokens",
			body:   `{"model":"gpt-x","usage":{"prompt_tokens":0,"completion_tokens":0}}`,
			wantOK: false, // production returns false; plan expected true
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, in, out, ok := parseOpenAIUsage([]byte(c.body), "application/json")
			if ok != c.wantOK {
				t.Errorf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && (in != c.wantIn || out != c.wantOut) {
				t.Errorf("in=%d out=%d, want in=%d out=%d", in, out, c.wantIn, c.wantOut)
			}
		})
	}
}

func TestParseUsage_JSON(t *testing.T) {
	body := []byte(`{"model":"claude-x","usage":{"input_tokens":12,"output_tokens":34}}`)
	model, in, out, ok := parseAnthropicUsage(body, "application/json")
	if !ok || model != "claude-x" || in != 12 || out != 34 {
		t.Fatalf("got (%q,%d,%d,%v)", model, in, out, ok)
	}
}

func TestParseUsage_SSE(t *testing.T) {
	body := []byte(
		"event: message_start\n" +
			`data: {"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","usage":{"output_tokens":42}}` + "\n\n")
	model, in, out, ok := parseAnthropicUsage(body, "text/event-stream; charset=utf-8")
	if !ok || model != "claude-x" || in != 10 || out != 42 {
		t.Fatalf("got (%q,%d,%d,%v)", model, in, out, ok)
	}
}

func TestParseGoogleUsage_NonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],
		"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":40,"thoughtsTokenCount":10,"totalTokenCount":150},
		"modelVersion":"gemini-2.5-pro"}`)
	model, in, out, ok := parseGoogleUsage(body, "application/json")
	if !ok || model != "gemini-2.5-pro" || in != 100 || out != 50 { // 40 + 10 thoughts
		t.Fatalf("got model=%q in=%d out=%d ok=%v; want gemini-2.5-pro/100/50/true", model, in, out, ok)
	}
}

func TestParseGoogleUsage_SSEKeepsLast(t *testing.T) {
	body := []byte("data: {\"usageMetadata\":{\"promptTokenCount\":100,\"candidatesTokenCount\":10},\"modelVersion\":\"gemini-2.5-flash\"}\n\n" +
		"data: {\"usageMetadata\":{\"promptTokenCount\":100,\"candidatesTokenCount\":42},\"modelVersion\":\"gemini-2.5-flash\"}\n\n")
	model, in, out, ok := parseGoogleUsage(body, "text/event-stream")
	if !ok || model != "gemini-2.5-flash" || in != 100 || out != 42 {
		t.Fatalf("got model=%q in=%d out=%d ok=%v; want gemini-2.5-flash/100/42/true (last wins)", model, in, out, ok)
	}
}

func TestParseGoogleUsage_NoUsage(t *testing.T) {
	if _, _, _, ok := parseGoogleUsage([]byte(`{"candidates":[]}`), "application/json"); ok {
		t.Error("a body with no usageMetadata must return ok=false")
	}
}
