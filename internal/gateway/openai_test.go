package gateway

import (
	"net/http"
	"testing"
)

func TestParseOpenAIUsage_NonStreaming(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","usage":{"input_tokens":120,"output_tokens":40}}`)
	m, in, out, ok := parseOpenAIUsage(body, "application/json")
	if !ok || m != "gpt-5-codex" || in != 120 || out != 40 {
		t.Fatalf("got (%q,%d,%d,%v)", m, in, out, ok)
	}
}

func TestParseOpenAIUsage_ChatCompletionsNaming(t *testing.T) {
	body := []byte(`{"model":"gpt-5","usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	m, in, out, ok := parseOpenAIUsage(body, "application/json")
	if !ok || m != "gpt-5" || in != 7 || out != 3 {
		t.Fatalf("got (%q,%d,%d,%v)", m, in, out, ok)
	}
}

func TestParseOpenAIUsage_StreamingResponsesEvent(t *testing.T) {
	body := []byte("event: response.completed\n" +
		`data: {"type":"response.completed","response":{"model":"gpt-5-codex","usage":{"input_tokens":500,"output_tokens":222}}}` + "\n\n" +
		"data: [DONE]\n")
	m, in, out, ok := parseOpenAIUsage(body, "text/event-stream; charset=utf-8")
	if !ok || m != "gpt-5-codex" || in != 500 || out != 222 {
		t.Fatalf("got (%q,%d,%d,%v)", m, in, out, ok)
	}
}

func TestParseOpenAIUsage_StreamingChatCompletionsNaming(t *testing.T) {
	body := []byte("data: {\"model\":\"gpt-5\",\"choices\":[]}\n\n" +
		"data: {\"model\":\"gpt-5\",\"usage\":{\"prompt_tokens\":80,\"completion_tokens\":12}}\n\n" +
		"data: [DONE]\n")
	m, in, out, ok := parseOpenAIUsage(body, "text/event-stream")
	if !ok || m != "gpt-5" || in != 80 || out != 12 {
		t.Fatalf("got (%q,%d,%d,%v)", m, in, out, ok)
	}
}

func TestOpenAIVendor_InjectsBearer(t *testing.T) {
	r, _ := http.NewRequest("POST", "http://gw/v1/responses", nil)
	r.Header.Set("Authorization", "Bearer tok_fake")
	OpenAIVendor().Inject(r, "sk-real")
	if got := r.Header.Get("Authorization"); got != "Bearer sk-real" {
		t.Fatalf("Authorization = %q", got)
	}
	if r.Header.Get("X-Api-Key") != "" {
		t.Fatalf("X-Api-Key should be unset for openai")
	}
}
