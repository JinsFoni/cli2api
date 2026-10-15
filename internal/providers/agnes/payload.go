package agnes

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/translate"
)

// chatCompletionRequest is the OpenAI-compatible body Agnes accepts. Fields
// the client did not send stay unset (omitempty) rather than defaulted: the
// platform applies its own model defaults.
type chatCompletionRequest struct {
	Model           string          `json:"model"`
	Messages        []chatMessage   `json:"messages"`
	Stream          bool            `json:"stream"`
	MaxTokens       *int            `json:"max_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Stop            json.RawMessage `json:"stop,omitempty"`
	Tools           json.RawMessage `json:"tools,omitempty"`
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
	ResponseFormat  json.RawMessage `json:"response_format,omitempty"`
	ParallelToolCalls *bool         `json:"parallel_tool_calls,omitempty"`
	StreamOptions   *streamOptions  `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role             string          `json:"role"`
	Content          any             `json:"content,omitempty"`
	Name             string          `json:"name,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ToolCalls        json.RawMessage `json:"tool_calls,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
}

// buildChatRequest converts an internal chat request into the OpenAI
// chat-completions body. Agnes speaks the OpenAI dialect natively, so the
// mapping is thin: messages pass through, tools are normalized to the flat
// OpenAI form, and only known-safe sampling knobs are forwarded.
func buildChatRequest(req translate.ChatRequest, model string, stream bool) ([]byte, error) {
	body := chatCompletionRequest{
		Model:    model,
		Messages: buildMessages(req.Messages),
		Stream:   stream,
	}
	if maxTokens := parseMaxTokens(req); maxTokens > 0 {
		body.MaxTokens = &maxTokens
	}
	body.Temperature = parseNumber(req.Temperature)
	body.TopP = parseNumber(req.TopP)
	if len(req.Stop) > 0 && string(req.Stop) != "null" {
		body.Stop = req.Stop
	}
	if len(req.Tools) > 0 {
		normalized, err := translate.NormalizeOpenAITools(req.Tools)
		if err == nil && len(normalized) > 0 {
			body.Tools = normalized
		}
		if err == nil && len(normalized) > 0 && len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
			body.ToolChoice = req.ToolChoice
		}
	}
	if len(req.ResponseFormat) > 0 && string(req.ResponseFormat) != "null" {
		body.ResponseFormat = req.ResponseFormat
	}
	body.ParallelToolCalls = req.ParallelToolCalls
	if stream {
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	return json.Marshal(body)
}

func buildMessages(messages []translate.ChatMessage) []chatMessage {
	if len(messages) == 0 {
		return []chatMessage{}
	}
	out := make([]chatMessage, 0, len(messages))
	for _, msg := range messages {
		entry := chatMessage{
			Role:             strings.ToLower(strings.TrimSpace(msg.Role)),
			Content:          msg.Content,
			Name:             msg.Name,
			ToolCallID:       msg.ToolCallID,
			ToolCalls:        msg.ToolCalls,
			ReasoningContent: msg.ReasoningContent,
		}
		switch entry.Role {
		case "system", "user", "assistant", "tool", "developer":
		default:
			entry.Role = "user"
		}
		out = append(out, entry)
	}
	return out
}

func parseMaxTokens(req translate.ChatRequest) int {
	for _, raw := range []json.RawMessage{req.MaxCompletionTokens, req.MaxTokens} {
		if len(raw) == 0 {
			continue
		}
		var n int
		if json.Unmarshal(raw, &n) == nil && n > 0 {
			return n
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			var parsed int
			if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &parsed); err == nil && parsed > 0 {
				return parsed
			}
		}
	}
	return 0
}

func parseNumber(raw json.RawMessage) *float64 {
	if len(raw) == 0 {
		return nil
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return &f
	}
	return nil
}
