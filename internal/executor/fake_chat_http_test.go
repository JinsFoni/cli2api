package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// fakeChatViaHTTP bridges the executor's in-process paths to test upstream
// servers that speak the OpenAI chat protocol: it posts the request to the
// pool item's URL and decodes the OpenAI-style response. This preserves the
// translation/routing assertions that previously exercised the worker
// transport without reintroducing that transport in production code.
type fakeChatViaHTTP struct {
	client *http.Client
	calls  atomic.Int32
	pool   *Pool
}

func newFakeChatViaHTTP(client *http.Client) *fakeChatViaHTTP {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &fakeChatViaHTTP{client: client}
}

func (f *fakeChatViaHTTP) ChatNonStream(ctx context.Context, accountID string, req translate.ChatRequest) (providers.ChatOutcome, error) {
	f.calls.Add(1)
	item, ok := f.itemByID(accountID)
	if !ok {
		return providers.ChatOutcome{}, &providers.Error{Status: 503, Kind: "unavailable", Message: "no item url"}
	}
	payload := map[string]any{"model": req.Model, "messages": req.Messages, "stream": false}
	body, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, item.URL, bytes.NewReader(body))
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
			Status:     resp.StatusCode,
			Kind:       resp.Header.Get("X-Qoder-Error-Kind"),
			Message:    string(raw),
			RetryAfter: retryAfterSeconds(resp.Header.Get("Retry-After")),
		}
	}
	var decoded struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   string          `json:"content"`
				ToolCalls json.RawMessage `json:"tool_calls"`
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
	f.calls.Add(1)
	item, ok := f.itemByID(accountID)
	if !ok {
		return nil, providers.ResolvedChat{}, &providers.Error{Status: 503, Kind: "unavailable", Message: "no item url"}
	}
	payload := map[string]any{"model": req.Model, "messages": req.Messages, "stream": true}
	body, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, item.URL, bytes.NewReader(body))
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
			Status:     resp.StatusCode,
			Kind:       resp.Header.Get("X-Qoder-Error-Kind"),
			Message:    string(raw),
			RetryAfter: retryAfterSeconds(resp.Header.Get("Retry-After")),
		}
	}
	return resp, providers.ResolvedChat{}, nil
}

// itemByID looks up the pool URL for the account. Registered per test via
// bindPool; tests construct the executor first, then attach the adapter.
func (f *fakeChatViaHTTP) itemByID(accountID string) (Item, bool) {
	if f.pool == nil {
		return Item{}, false
	}
	return f.pool.ByID(accountID)
}

func retryAfterSeconds(v string) time.Duration {
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}
