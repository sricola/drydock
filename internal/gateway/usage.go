package gateway

import (
	"encoding/json"
	"strings"
)

// parseUsage extracts (model, input tokens, output tokens) from an Anthropic
// response body, handling both a single JSON message and an SSE stream.
func parseUsage(body []byte, contentType string) (model string, in, out int, ok bool) {
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
