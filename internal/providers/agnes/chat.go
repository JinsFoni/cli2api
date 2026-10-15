package agnes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// ChatNonStream sends one non-streaming /chat/completions call and aggregates
// the OpenAI chat-completion JSON. The internal ChatOutcome is the same shape
// the other adapters produce; usage comes straight from the response.
func (c *Client) ChatNonStream(ctx context.Context, accountID string, req translate.ChatRequest) (providers.ChatOutcome, error) {
	credential, err := c.credential(ctx, accountID)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	model, err := resolveModel(req.Model)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	resp, err := c.doCompletion(ctx, client, credential, req, model, false)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return providers.ChatOutcome{}, newProviderError(resp.StatusCode, string(body))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return providers.ChatOutcome{}, fmt.Errorf("read agnes response: %w", err)
	}
	return outcomeFromResponse(body, model)
}

// ChatStream sends one streaming /chat/completions call and returns the
// upstream response untouched: Agnes already speaks the OpenAI
// chat-completions SSE dialect the gateway relays, so there is no rewrite.
func (c *Client) ChatStream(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	credential, err := c.credential(ctx, accountID)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	client.Timeout = 0
	model, err := resolveModel(req.Model)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	resp, err := c.doCompletion(ctx, client, credential, req, model, true)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, providers.ResolvedChat{}, newProviderError(resp.StatusCode, string(body))
	}
	return resp, providers.ResolvedChat{}, nil
}

func (c *Client) doCompletion(ctx context.Context, client *http.Client, credential Credential, req translate.ChatRequest, model string, stream bool) (*http.Response, error) {
	payload, err := buildChatRequest(req, model, stream)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpointBase(credential)+PathChatCompletions, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+credential.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	return client.Do(httpReq)
}

// resolveModel strips the gateway pin prefix. The executor has already mapped
// a public id to its native id when the account advertises it.
func resolveModel(model string) (string, error) {
	clean := strings.TrimSpace(model)
	clean = strings.TrimPrefix(clean, "agnes/")
	clean = strings.TrimSpace(clean)
	if clean == "" {
		return "", fmt.Errorf("agnes model required")
	}
	return clean, nil
}

// outcomeFromResponse maps a chat-completion JSON body onto ChatOutcome.
func outcomeFromResponse(body []byte, model string) (providers.ChatOutcome, error) {
	var completion struct {
		Model    string `json:"model"`
		Choices  []struct {
			Message struct {
				Role             string          `json:"role"`
				Content          any             `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				Reasoning        string          `json:"reasoning"`
				ToolCalls        json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &completion); err != nil {
		return providers.ChatOutcome{}, fmt.Errorf("decode agnes response: %w", err)
	}
	if completion.Error != nil && strings.TrimSpace(completion.Error.Message) != "" {
		return providers.ChatOutcome{}, newProviderError(502, string(body))
	}
	out := providers.ChatOutcome{Model: model, UsageSource: "upstream"}
	if len(completion.Choices) > 0 {
		choice := completion.Choices[0]
		out.Content = translate.ContentToString(choice.Message.Content)
		out.Reasoning = firstNonEmpty(choice.Message.ReasoningContent, choice.Message.Reasoning)
		if len(choice.Message.ToolCalls) > 0 {
			out.ToolCalls = choice.Message.ToolCalls
		}
		out.FinishReason = normalizeFinishReason(choice.FinishReason, len(choice.Message.ToolCalls) > 0)
	}
	out.PromptTokens = completion.Usage.PromptTokens
	out.CompletionTokens = completion.Usage.CompletionTokens
	if details := completion.Usage.PromptTokensDetails; details != nil {
		if cached := details.CachedTokens; cached > 0 {
			cacheRead := cached
			out.CacheReadTokens = &cacheRead
		}
	}
	return out, nil
}

// normalizeFinishReason maps Agnes finish reasons onto the OpenAI set the
// executor understands. Tool calls win over an ambiguous "stop".
func normalizeFinishReason(reason string, hasToolCalls bool) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length":
		return "length"
	case "tool_calls", "tool-calls", "function_call":
		return "tool_calls"
	}
	if hasToolCalls {
		return "tool_calls"
	}
	return "stop"
}
