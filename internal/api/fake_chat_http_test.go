package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/endpoint"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// fakeChatViaHTTP bridges the executor's in-process paths to the compat
// tests' fake upstream servers (which speak the OpenAI chat protocol): it
// posts the request to the pool item's URL and returns/decodes the OpenAI
// response. This preserves the translation assertions that previously
// exercised the worker transport without reintroducing that transport.
type fakeChatViaHTTP struct {
	client *http.Client
	urlFor func(accountID string) string
}

func newFakeChatViaHTTP(client *http.Client, urlFor func(accountID string) string) *fakeChatViaHTTP {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &fakeChatViaHTTP{client: client, urlFor: urlFor}
}

func (f *fakeChatViaHTTP) ChatNonStream(ctx context.Context, accountID string, req translate.ChatRequest) (providers.ChatOutcome, error) {
	payload := chatPayloadFor(req, false)
	body, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(f.urlFor(accountID), "/")+endpoint.ChatCompletionsPath, bytes.NewReader(body))
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Qoder-Account", accountID)
	resp, err := f.client.Do(httpReq)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return providers.ChatOutcome{}, &providers.Error{
			Status:  resp.StatusCode,
			Kind:    resp.Header.Get("X-Qoder-Error-Kind"),
			Message: string(raw),
		}
	}
	var decoded struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string          `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				ToolCalls        json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int    `json:"prompt_tokens"`
			CompletionTokens int    `json:"completion_tokens"`
			Source           string `json:"source"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return providers.ChatOutcome{}, err
	}
	out := providers.ChatOutcome{Model: decoded.Model}
	if len(decoded.Choices) > 0 {
		out.Content = decoded.Choices[0].Message.Content
		out.Reasoning = decoded.Choices[0].Message.ReasoningContent
		out.ToolCalls = decoded.Choices[0].Message.ToolCalls
		out.FinishReason = decoded.Choices[0].FinishReason
	}
	out.PromptTokens = decoded.Usage.PromptTokens
	out.CompletionTokens = decoded.Usage.CompletionTokens
	out.UsageSource = decoded.Usage.Source
	if out.Model == "" {
		out.Model = req.Model
	}
	return out, nil
}

func (f *fakeChatViaHTTP) ChatStream(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	payload := chatPayloadFor(req, true)
	body, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(f.urlFor(accountID), "/")+endpoint.ChatCompletionsPath, bytes.NewReader(body))
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Qoder-Account", accountID)
	resp, err := f.client.Do(httpReq)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, providers.ResolvedChat{}, &providers.Error{
			Status:  resp.StatusCode,
			Kind:    resp.Header.Get("X-Qoder-Error-Kind"),
			Message: string(raw),
		}
	}
	return resp, providers.ResolvedChat{}, nil
}

// chatPayloadFor mirrors the qoder adapter's OpenAI-style payload assembly.
func chatPayloadFor(req translate.ChatRequest, stream bool) map[string]any {
	payload := map[string]any{"model": req.Model, "messages": req.Messages, "stream": stream}
	if len(req.MaxCompletionTokens) > 0 {
		payload["max_tokens"] = req.MaxCompletionTokens
	} else if len(req.MaxTokens) > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	if len(req.Temperature) > 0 {
		payload["temperature"] = req.Temperature
	}
	if len(req.Tools) > 0 {
		payload["tools"] = req.Tools
	}
	if len(req.ToolChoice) > 0 {
		payload["tool_choice"] = req.ToolChoice
	}
	return payload
}
