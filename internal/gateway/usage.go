package gateway

import (
	"encoding/json"
	"strings"
)

// parseAnthropicUsage extracts (model, input tokens, output tokens) from an Anthropic
// response body, handling both a single JSON message and an SSE stream.
func parseAnthropicUsage(body []byte, contentType string) (model string, in, out int, ok bool) {
	if strings.Contains(contentType, "text/event-stream") {
		return parseSSEUsage(body)
	}
	var m struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &m) == nil && (m.Usage.InputTokens > 0 || m.Usage.OutputTokens > 0) {
		return m.Model, m.Usage.InputTokens, m.Usage.OutputTokens, true
	}
	return "", 0, 0, false
}

func parseSSEUsage(body []byte) (model string, in, out int, ok bool) {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Model string `json:"model"`
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			model, in, out, ok = ev.Message.Model, ev.Message.Usage.InputTokens, ev.Message.Usage.OutputTokens, true
		case "message_delta":
			if ev.Usage.OutputTokens > 0 {
				out, ok = ev.Usage.OutputTokens, true
			}
		}
	}
	return
}

type openaiUsage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// parseOpenAIUsage extracts (model, input, output) from an OpenAI response.
// Handles non-streaming JSON and SSE streams, and both Responses naming
// (input_tokens/output_tokens) and Chat Completions naming
// (prompt_tokens/completion_tokens). For streams it keeps the last usage seen.
func parseOpenAIUsage(body []byte, contentType string) (model string, in, out int, ok bool) {
	if strings.Contains(contentType, "text/event-stream") {
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}
			if m, i, o, k := openaiUsageFromJSON([]byte(data)); k {
				model, in, out, ok = m, i, o, k
			}
		}
		return
	}
	return openaiUsageFromJSON(body)
}

func openaiUsageFromJSON(b []byte) (model string, in, out int, ok bool) {
	var m struct {
		Model    string       `json:"model"`
		Usage    *openaiUsage `json:"usage"`
		Response *struct {
			Model string       `json:"model"`
			Usage *openaiUsage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(b, &m) != nil {
		return "", 0, 0, false
	}
	u, model := m.Usage, m.Model
	if u == nil && m.Response != nil {
		u, model = m.Response.Usage, m.Response.Model
	}
	if u == nil {
		return "", 0, 0, false
	}
	// Zero is treated as "field absent": OpenAI omits the Responses-style
	// fields when a response uses Chat-Completions naming (and vice versa),
	// and a real call never reports zero input+output tokens.
	in, out = u.InputTokens, u.OutputTokens
	if in == 0 {
		in = u.PromptTokens
	}
	if out == 0 {
		out = u.CompletionTokens
	}
	if in == 0 && out == 0 {
		return "", 0, 0, false
	}
	return model, in, out, true
}
