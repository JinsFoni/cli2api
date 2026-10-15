package agnes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

func TestChatNonStreamAggregates(t *testing.T) {
	rec := newRecorder()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-1","object":"chat.completion","model":"agnes-2.5-flash",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello world"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":3}}
		}`))
	}))
	defer srv.Close()
	client, _ := newTestClient(t, srv)

	out, err := client.ChatNonStream(context.Background(), "acc-1", translate.ChatRequest{
		Model:    "agnes/agnes-2.5-flash",
		Messages: []translate.ChatMessage{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("ChatNonStream: %v", err)
	}
	if out.Content != "hello world" || out.Model != "agnes-2.5-flash" {
		t.Fatalf("unexpected outcome: %+v", out)
	}
	if out.PromptTokens != 11 || out.CompletionTokens != 7 {
		t.Fatalf("usage not mapped: %+v", out)
	}
	if out.CacheReadTokens == nil || *out.CacheReadTokens != 3 {
		t.Fatalf("cache read not mapped: %+v", out.CacheReadTokens)
	}
	if out.FinishReason != "stop" || out.UsageSource != "upstream" {
		t.Fatalf("finish/usage source wrong: %+v", out)
	}
	if got := rec.header("Authorization"); got != "Bearer sk-test_key_1234567890" {
		t.Fatalf("Authorization header = %q", got)
	}
	if got := rec.header("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type header = %q", got)
	}
	if !strings.Contains(rec.body, `"model":"agnes-2.5-flash"`) {
		t.Fatalf("request body lost the resolved model: %s", rec.body)
	}
	if strings.Contains(rec.body, `agnes/`) {
		t.Fatalf("pin prefix leaked upstream: %s", rec.body)
	}
}

func TestChatNonStreamToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"model":"agnes-2.5-flash",
			"choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}
			]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":5,"completion_tokens":9}
		}`))
	}))
	defer srv.Close()
	client, _ := newTestClient(t, srv)

	out, err := client.ChatNonStream(context.Background(), "acc-1", translate.ChatRequest{
		Model:    "agnes-2.5-flash",
		Messages: []translate.ChatMessage{{Role: "user", Content: "weather?"}},
	})
	if err != nil {
		t.Fatalf("ChatNonStream: %v", err)
	}
	if out.FinishReason != "tool_calls" || len(out.ToolCalls) == 0 {
		t.Fatalf("tool calls not mapped: %+v", out)
	}
	if !strings.Contains(string(out.ToolCalls), `"get_weather"`) {
		t.Fatalf("tool call payload wrong: %s", out.ToolCalls)
	}
}

func TestChatStreamRelaysUpstreamSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	client, _ := newTestClient(t, srv)

	resp, _, err := client.ChatStream(context.Background(), "acc-1", translate.ChatRequest{
		Model:    "agnes-2.5-flash",
		Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("stream content type = %q", ct)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), `data: {"choices"`) {
		t.Fatalf("stream body not relayed verbatim: %q", string(buf[:n]))
	}
}

func TestChatStreamForwardsStreamFlag(t *testing.T) {
	rec := newRecorder()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	client, _ := newTestClient(t, srv)

	resp, _, err := client.ChatStream(context.Background(), "acc-1", translate.ChatRequest{
		Model:    "agnes-2.5-flash",
		Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	resp.Body.Close()
	if !strings.Contains(rec.body, `"stream":true`) {
		t.Fatalf("stream flag not forced: %s", rec.body)
	}
	if !strings.Contains(rec.body, `"stream_options"`) {
		t.Fatalf("stream_options (usage) missing: %s", rec.body)
	}
}

func TestChatMapsUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"Insufficient balance","type":"agnes_error"}}`))
	}))
	defer srv.Close()
	client, _ := newTestClient(t, srv)

	_, err := client.ChatNonStream(context.Background(), "acc-1", translate.ChatRequest{
		Model:    "agnes-2.5-flash",
		Messages: []translate.ChatMessage{{Role: "user", Content: "hello"}},
	})
	var providerErr *providers.Error
	if !errors.As(err, &providerErr) {
		t.Fatalf("expected *providers.Error, got %v", err)
	}
	if providerErr.Kind != accounts.KindQuota {
		t.Fatalf("kind = %q, want quota", providerErr.Kind)
	}
}

func TestChatRequiresCredentialAndModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	client, store := newTestClient(t, srv)
	store.payload = []byte(`{"format":"agnes-key-v1"}`)

	if _, err := client.ChatNonStream(context.Background(), "acc-1", translate.ChatRequest{
		Model:    "agnes-2.5-flash",
		Messages: []translate.ChatMessage{{Role: "user", Content: "hello"}},
	}); err == nil || !strings.Contains(err.Error(), "requires an api key") {
		t.Fatalf("expected incomplete credential error, got %v", err)
	}

	cred := Credential{APIKey: "sk-test_key_1234567890"}
	payload, err := cred.Encode()
	if err != nil {
		t.Fatalf("encode credential: %v", err)
	}
	store.payload = payload
	if _, err := client.ChatNonStream(context.Background(), "acc-1", translate.ChatRequest{
		Messages: []translate.ChatMessage{{Role: "user", Content: "hello"}},
	}); err == nil || !strings.Contains(err.Error(), "model required") {
		t.Fatalf("expected model required error, got %v", err)
	}
}
