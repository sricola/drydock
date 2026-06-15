package gateway

import "testing"

func TestParseUsage_JSON(t *testing.T) {
	body := []byte(`{"model":"claude-x","usage":{"input_tokens":12,"output_tokens":34}}`)
	model, in, out, ok := parseUsage(body, "application/json")
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
	model, in, out, ok := parseUsage(body, "text/event-stream; charset=utf-8")
	if !ok || model != "claude-x" || in != 10 || out != 42 {
		t.Fatalf("got (%q,%d,%d,%v)", model, in, out, ok)
	}
}
