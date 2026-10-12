package gateway

import (
	"encoding/json"
	"strings"
)

// Usage is what a vendor parser reads out of one response: the model that
// served it and the token counts the vendor bills. CacheWrite and CacheRead
// are the prompt-cache counts vendors report SEPARATELY from Input (Anthropic:
// cache_creation_input_tokens / cache_read_input_tokens); vendors that fold
// cached tokens into their input count (OpenAI, Google) leave them zero.
type Usage struct {
	Model      string
	Input      int
	Output     int
	CacheWrite int
	CacheRead  int
}

func (u Usage) any() bool { return u.Input > 0 || u.Output > 0 || u.CacheWrite > 0 || u.CacheRead > 0 }

// anthropicUsage is the wire shape of an Anthropic `usage` object.
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// parseAnthropicUsage extracts the billed usage from an Anthropic response
// body, handling both a single JSON message and an SSE stream.
func parseAnthropicUsage(body []byte, contentType string) (Usage, bool) {
	if strings.Contains(contentType, "text/event-stream") {
		return parseSSEUsage(body)
	}
	var m struct {
		Model string         `json:"model"`
		Usage anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(body, &m) != nil {
		return Usage{}, false
	}
	u := Usage{Model: m.Model, Input: m.Usage.InputTokens, Output: m.Usage.OutputTokens,
		CacheWrite: m.Usage.CacheCreationInputTokens, CacheRead: m.Usage.CacheReadInputTokens}
	return u, u.any()
}

// parseSSEUsage reads an Anthropic stream: message_start carries the model,
// the input-side counts (including the cache counts) and a provisional output
// count; message_delta carries the final output count.
func parseSSEUsage(body []byte) (u Usage, ok bool) {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Model string         `json:"model"`
				Usage anthropicUsage `json:"usage"`
			} `json:"message"`
			Usage anthropicUsage `json:"usage"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			u = Usage{Model: ev.Message.Model, Input: ev.Message.Usage.InputTokens, Output: ev.Message.Usage.OutputTokens,
				CacheWrite: ev.Message.Usage.CacheCreationInputTokens, CacheRead: ev.Message.Usage.CacheReadInputTokens}
			ok = true
		case "message_delta":
			if ev.Usage.OutputTokens > 0 {
				u.Output, ok = ev.Usage.OutputTokens, true
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
func parseOpenAIUsage(body []byte, contentType string) (u Usage, ok bool) {
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
			if last, k := openaiUsageFromJSON([]byte(data)); k {
				u, ok = last, k
			}
		}
		return
	}
	return openaiUsageFromJSON(body)
}

// openaiUsageFromJSON reads one OpenAI object. Cached prompt tokens
// (input_tokens_details.cached_tokens) are INCLUDED in input_tokens on this
// API, so metering them at the input rate is already conservative; they are
// not split out.
func openaiUsageFromJSON(b []byte) (Usage, bool) {
	var m struct {
		Model    string       `json:"model"`
		Usage    *openaiUsage `json:"usage"`
		Response *struct {
			Model string       `json:"model"`
			Usage *openaiUsage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(b, &m) != nil {
		return Usage{}, false
	}
	ou, model := m.Usage, m.Model
	if ou == nil && m.Response != nil {
		ou, model = m.Response.Usage, m.Response.Model
	}
	if ou == nil {
		return Usage{}, false
	}
	// Zero is treated as "field absent": OpenAI omits the Responses-style
	// fields when a response uses Chat-Completions naming (and vice versa),
	// and a real call never reports zero input+output tokens.
	in, out := ou.InputTokens, ou.OutputTokens
	if in == 0 {
		in = ou.PromptTokens
	}
	if out == 0 {
		out = ou.CompletionTokens
	}
	if in == 0 && out == 0 {
		return Usage{}, false
	}
	return Usage{Model: model, Input: in, Output: out}, true
}

// parseGoogleUsage extracts (model, input, output) from a Gemini response.
// Input = promptTokenCount; output = candidatesTokenCount + thoughtsTokenCount
// (Gemini 2.5 bills thinking tokens as output). Handles non-streaming JSON and
// SSE (alt=sse), keeping the last usageMetadata seen (the final chunk carries
// the cumulative totals). model comes from modelVersion.
// Cached content (cachedContentTokenCount) is included in promptTokenCount on
// this API, so it is metered at the input rate (conservative) and not split out.
func parseGoogleUsage(body []byte, contentType string) (u Usage, ok bool) {
	if strings.Contains(contentType, "text/event-stream") {
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			if last, k := googleUsageFromJSON([]byte(data)); k {
				u, ok = last, k
			}
		}
		return
	}
	return googleUsageFromJSON(body)
}

func googleUsageFromJSON(b []byte) (Usage, bool) {
	var m struct {
		ModelVersion  string `json:"modelVersion"`
		UsageMetadata *struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
		} `json:"usageMetadata"`
	}
	if json.Unmarshal(b, &m) != nil || m.UsageMetadata == nil {
		return Usage{}, false
	}
	in := m.UsageMetadata.PromptTokenCount
	out := m.UsageMetadata.CandidatesTokenCount + m.UsageMetadata.ThoughtsTokenCount
	if in == 0 && out == 0 {
		return Usage{}, false
	}
	return Usage{Model: m.ModelVersion, Input: in, Output: out}, true
}
