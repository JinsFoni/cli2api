package qoder

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// countStore counts LoadCredential / Get / SaveCredential so tests can assert
// the identity cache suppresses store round-trips and that refresh persists.
type countStore struct {
	fakeNativeStore

	getCalls  atomic.Int64
	loadCalls atomic.Int64
	saveCalls atomic.Int64
	chatBase  string
}

func (s *countStore) Get(ctx context.Context, id string) (accounts.Account, error) {
	s.getCalls.Add(1)
	return s.fakeNativeStore.Get(ctx, id)
}

func (s *countStore) LoadCredential(ctx context.Context, id string) (accounts.NativeCredential, error) {
	s.loadCalls.Add(1)
	return s.fakeNativeStore.LoadCredential(ctx, id)
}

func (s *countStore) SaveCredential(ctx context.Context, accountID, authType string, credential accounts.NativeCredential) error {
	s.saveCalls.Add(1)
	return s.fakeNativeStore.SaveCredential(ctx, accountID, authType, credential)
}

// globalAccount returns a global-region account hitting the test server.
func globalAccount(baseURL string) accounts.Account {
	return accounts.Account{ID: "acc-1", Provider: "qoder", ProviderRegion: "global", AuthType: "qoder-native-v1"}
}

// pointChatAt installs the test server as the chat host for both regions and
// as the openapi (refresh) host, so one fake upstream serves the whole flow.
func pointChatAt(t *testing.T, client *Client, upstreamURL string) {
	t.Helper()
	client.setEndpoints(map[string]nativeEndpoints{
		"global": {Base: upstreamURL, Origin: upstreamURL},
		"cn":     {Base: upstreamURL, Origin: upstreamURL},
	})
}

// sseEnvelope renders one upstream SSE frame: `data: {headers,body,statusCodeValue}`.
func sseEnvelope(statusCode int, body any) string {
	raw, _ := json.Marshal(map[string]any{"headers": map[string]string{}, "body": body, "statusCodeValue": statusCode})
	return "data: " + string(raw) + "\n\n"
}

// sseEnvelopeRaw renders a frame with a raw string body (already-encoded JSON).
func sseEnvelopeRaw(statusCode int, body string) string {
	payload := fmt.Sprintf(`{"headers":{},"body":%s,"statusCodeValue":%d}`, strconv.Quote(body), statusCode)
	return "data: " + payload + "\n\n"
}

// chunkJSON builds a nested OpenAI delta chunk as a string (the envelope body).
func chunkJSON(delta map[string]any, finish string) string {
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	raw, _ := json.Marshal(map[string]any{"choices": []any{choice}})
	return string(raw)
}

// readSSEChunks drains a translated SSE body into data-frame strings (in order).
func readSSEChunks(t *testing.T, body io.Reader) []string {
	t.Helper()
	var frames []string
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			frames = append(frames, strings.TrimSpace(strings.TrimPrefix(line, "data: ")))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read SSE body: %v", err)
	}
	return frames
}

// chatRequest is the minimal translate request for chat tests.
func chatRequest(model string, stream bool) translate.ChatRequest {
	return translate.ChatRequest{
		Model:    model,
		Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}},
		Stream:   stream,
	}
}

// nativeChatClient assembles the standard client for chat tests: credential
// store + endpoint override, no proxy. The chat HTTP client gets a 60s
// timeout like production; ChatStream itself must clear it per request.
func nativeChatClient(t *testing.T, store *countStore) *Client {
	t.Helper()
	client := NewClient(store)
	client.Bind(func(string) (string, bool) { return "", false }, func() string { return "" })
	pointChatAt(t, client, store.chatBase)
	return client
}

// ---------------------------------------------------------------------------
// 1. envelope stream → translated OpenAI chunk stream
// ---------------------------------------------------------------------------

func TestNativeChatStreamEnvelopesTranslateToOpenAIChunks(t *testing.T) {
	var sawPath, sawAuth, sawContentType, sawAccept, sawModelKey, sawModelSource, sawUA string
	var sawBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawAuth = r.Header.Get("Authorization")
		sawContentType = r.Header.Get("Content-Type")
		sawAccept = r.Header.Get("Accept")
		sawModelKey = r.Header.Get("X-Model-Key")
		sawModelSource = r.Header.Get("X-Model-Source")
		sawUA = r.Header.Get("User-Agent")
		sawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w,
			sseEnvelopeRaw(200, chunkJSON(map[string]any{"role": "assistant"}, "")),
			sseEnvelopeRaw(200, chunkJSON(map[string]any{"content": "Hel"}, "")),
			sseEnvelopeRaw(200, chunkJSON(map[string]any{"reasoning": "hmm"}, "")),
			sseEnvelopeRaw(200, chunkJSON(map[string]any{"content": "lo"}, "stop")),
			sseEnvelope(200, map[string]any{"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7}}),
		)
	}))
	defer upstream.Close()

	store := &countStore{fakeNativeStore: fakeNativeStore{account: globalAccount(upstream.URL), credential: encryptedCredential(t, machineID, nil)}}
	store.chatBase = upstream.URL
	client := nativeChatClient(t, store)

	resp, resolved, err := client.ChatStream(context.Background(), "acc-1", chatRequest("glm-5.2", true))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}
	if resolved.ReasoningLevel != "" {
		t.Fatalf("unexpected reasoning level %q", resolved.ReasoningLevel)
	}

	frames := readSSEChunks(t, resp.Body)
	// role delta + content + reasoning + content/finish + usage + [DONE]
	if len(frames) != 6 {
		t.Fatalf("frames = %d: %v", len(frames), frames)
	}
	if frames[len(frames)-1] != "[DONE]" {
		t.Fatalf("missing terminal [DONE]: %v", frames)
	}
	var deltas []map[string]any
	for _, frame := range frames[:len(frames)-2] {
		var chunk struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Model   string `json:"model"`
			Choices []struct {
				Index        int            `json:"index"`
				Delta        map[string]any `json:"delta"`
				FinishReason string         `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(frame), &chunk); err != nil {
			t.Fatalf("frame %q: %v", frame, err)
		}
		if chunk.Object != "chat.completion.chunk" || chunk.Model != "glm-5.2" {
			t.Fatalf("chunk shape = %+v", chunk)
		}
		if !strings.HasPrefix(chunk.ID, "chatcmpl-") {
			t.Fatalf("chunk id = %q", chunk.ID)
		}
		deltas = append(deltas, chunk.Choices[0].Delta)
		if chunk.Choices[0].FinishReason != "" && chunk.Choices[0].FinishReason != "stop" {
			t.Fatalf("finish = %q", chunk.Choices[0].FinishReason)
		}
	}
	if deltas[0]["role"] != "assistant" {
		t.Fatalf("first delta must be the role delta: %v", deltas[0])
	}
	if deltas[1]["content"] != "Hel" {
		t.Fatalf("content delta = %v", deltas[1])
	}
	// worker sse.mjs: upstream `reasoning` is renamed `reasoning_content`.
	if deltas[2]["reasoning_content"] != "hmm" || hasKey(deltas[2], "reasoning") {
		t.Fatalf("reasoning delta = %v", deltas[2])
	}
	var finishChunk map[string]any
	if err := json.Unmarshal([]byte(frames[len(frames)-2]), &finishChunk); err != nil {
		t.Fatal(err)
	}
	usage, _ := finishChunk["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(11) || usage["completion_tokens"] != float64(7) || usage["source"] != "upstream" {
		t.Fatalf("usage frame = %v", finishChunk)
	}

	// Request-side invariants: native infer path, signed auth, no Encode on CN
	// style, plain JSON body, stream true.
	if sawPath != "/algo/api/v2/service/pro/sse/agent_chat_generation" {
		t.Fatalf("path = %s", sawPath)
	}
	if !strings.HasPrefix(sawAuth, "Bearer COSY.") {
		t.Fatalf("auth = %q", sawAuth)
	}
	if sawContentType != "application/json" || sawAccept != "text/event-stream" {
		t.Fatalf("content=%q accept=%q", sawContentType, sawAccept)
	}
	if sawModelKey != "glm-5.2" || sawModelSource != "system" {
		t.Fatalf("model key=%q source=%q", sawModelKey, sawModelSource)
	}
	if sawUA != "Bun/1.3.14" {
		t.Fatalf("user agent = %q", sawUA)
	}
	// Task 8 recording: upstream body arrives encoded (Encode=1); decode it
	// back before asserting the plain JSON fields.
	decoded, err := decodeNativeBody(sawBody)
	if err != nil {
		t.Fatalf("decode encoded chat body: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(decoded, &body); err != nil {
		t.Fatalf("request body not plain JSON after decode: %v", err)
	}
	if body["stream"] != true {
		t.Fatalf("upstream body must always stream: %v", body["stream"])
	}
	if _, ok := body["model_config"].(map[string]any); !ok {
		t.Fatalf("missing model_config")
	}
}

func hasKey(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}

// ---------------------------------------------------------------------------
// 2. statusCodeValue 401 envelope → refresh + persist + retry (fresh identity)
// ---------------------------------------------------------------------------

func TestNativeChat401EnvelopeRefreshesAndRetries(t *testing.T) {
	var chatAttempts atomic.Int64
	var refreshAttempts atomic.Int64
	var attemptCoses []string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/jobToken/refresh":
			refreshAttempts.Add(1)
			_, _ = w.Write([]byte(`{"token":"at-new","refresh_token":"jrt-new","expires_in":3600000}`))
		case r.Method == http.MethodPost && r.URL.Path == "/algo/api/v2/service/pro/sse/agent_chat_generation":
			n := chatAttempts.Add(1)
			mu.Lock()
			attemptCoses = append(attemptCoses, r.Header.Get("Cosy-Key"))
			mu.Unlock()
			if n == 1 {
				// The auth failure arrives as an in-stream envelope, so the
				// native peek must catch it before any bytes are forwarded.
				fmt.Fprint(w, sseEnvelopeRaw(401, `{"error":{"message":"token expired","type":"api_error"}}`))
				return
			}
			fmt.Fprint(w,
				sseEnvelopeRaw(200, chunkJSON(map[string]any{"content": "ok"}, "stop")),
				sseEnvelope(200, map[string]any{"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2}}),
			)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	store := &countStore{fakeNativeStore: fakeNativeStore{account: globalAccount(upstream.URL), credential: encryptedCredential(t, machineID, nil)}}
	store.chatBase = upstream.URL
	client := nativeChatClient(t, store)

	resp, _, err := client.ChatStream(context.Background(), "acc-1", chatRequest("glm-5.2", true))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if chatAttempts.Load() != 2 || refreshAttempts.Load() != 1 {
		t.Fatalf("chat=%d refresh=%d", chatAttempts.Load(), refreshAttempts.Load())
	}
	mu.Lock()
	first, second := attemptCoses[0], attemptCoses[1]
	mu.Unlock()
	// The retry must go out with a rebuilt identity (fresh runtime fields),
	// proving the credential-fingerprint cache was invalidated.
	if second == "" || first == "" || first == second {
		t.Fatalf("retry must rebuild identity: cosies %q vs %q", first, second)
	}
	frames := readSSEChunks(t, resp.Body)
	if len(frames) != 4 || frames[len(frames)-1] != "[DONE]" {
		t.Fatalf("retry stream frames = %v", frames)
	}
	// Refreshed tokens must be persisted.
	if store.saveCalls.Load() != 1 {
		t.Fatalf("SaveCredential calls = %d", store.saveCalls.Load())
	}
	saved, err := decodeUserBlob(store.credentials[0].UserBlob, machineID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.AccessToken != "at-new" || saved.RefreshToken != "jrt-new" {
		t.Fatalf("saved credential = %+v", saved)
	}
}

// ---------------------------------------------------------------------------
// 3. 403 duplicate request → surfaced pre-stream, no retry
// ---------------------------------------------------------------------------

func TestNativeChat403DuplicateDoesNotRetry(t *testing.T) {
	var chatAttempts atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/algo/api/v2/service/pro/sse/agent_chat_generation" {
			chatAttempts.Add(1)
			fmt.Fprint(w, sseEnvelopeRaw(403, `{"error":{"code":"FORBIDDEN","message":"Duplicate request","type":"api_error"}}`))
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	store := &countStore{fakeNativeStore: fakeNativeStore{account: globalAccount(upstream.URL), credential: encryptedCredential(t, machineID, nil)}}
	store.chatBase = upstream.URL
	client := nativeChatClient(t, store)

	_, _, err := client.ChatStream(context.Background(), "acc-1", chatRequest("glm-5.2", true))
	if err == nil {
		t.Fatal("403 envelope must surface pre-stream")
	}
	var providerErr *providers.Error
	if !errors.As(err, &providerErr) {
		t.Fatalf("error = %T (%v), want *providers.Error", err, err)
	}
	if providerErr.Status != 403 || providerErr.Kind != accounts.KindAuth {
		t.Fatalf("provider error = %+v", providerErr)
	}
	// executor ClassifyError maps the kind through its taxonomy: duplicate
	// request must land on KindAuth (matches worker AUTH_RE).
	if classified := classifyProviderErrorForTest(providerErr); classified.Kind != accounts.KindAuth {
		t.Fatalf("classified kind = %q, want auth", classified.Kind)
	}
	if !strings.Contains(providerErr.Error(), "Duplicate request") {
		t.Fatalf("upstream message lost: %v", providerErr)
	}
	if chatAttempts.Load() != 1 {
		t.Fatalf("403 must not retry, attempts = %d", chatAttempts.Load())
	}
}

// classifyProviderErrorForTest mirrors the executor's ClassifyError provider
// branch (Classify(status, message, "", kind, "")) so tests can assert the
// taxonomy mapping without importing executor (which imports qoder).
// accounts.Classified lives in executor (accounts.ClassifiedKind strings only),
// so a local struct keeps the assertion honest about kind + status.
type providerClassifiedForTest struct {
	Kind   string
	Status int
}

func classifyProviderErrorForTest(perr *providers.Error) providerClassifiedForTest {
	lower := strings.ToLower(perr.Message + " " + perr.Code + " " + perr.Type)
	kind := perr.Kind
	switch {
	case kind == "" && containsAny(lower, "forbidden", "duplicate request"):
		kind = accounts.KindAuth
	}
	return providerClassifiedForTest{Kind: kind, Status: perr.Status}
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 4. body [DONE] middle frames are ignored, stream continues
// ---------------------------------------------------------------------------

func TestNativeChatIgnoresMiddleDoneBodies(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w,
			sseEnvelopeRaw(200, chunkJSON(map[string]any{"content": "a"}, "")),
			sseEnvelope(200, "[DONE]"),
			sseEnvelopeRaw(200, `"[DONE]"`),
			sseEnvelopeRaw(200, chunkJSON(map[string]any{"content": "b"}, "stop")),
		)
	}))
	defer upstream.Close()

	store := &countStore{fakeNativeStore: fakeNativeStore{account: globalAccount(upstream.URL), credential: encryptedCredential(t, machineID, nil)}}
	store.chatBase = upstream.URL
	client := nativeChatClient(t, store)

	resp, _, err := client.ChatStream(context.Background(), "acc-1", chatRequest("glm-5.2", true))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var content strings.Builder
	for _, frame := range readSSEChunks(t, resp.Body) {
		if frame == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta        map[string]any `json:"delta"`
				FinishReason string         `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		_ = json.Unmarshal([]byte(frame), &chunk)
		if len(chunk.Choices) > 0 {
			if text, _ := chunk.Choices[0].Delta["content"].(string); text != "" {
				content.WriteString(text)
			}
		}
	}
	if content.String() != "ab" {
		t.Fatalf("content = %q, [DONE] bodies must not terminate the stream", content.String())
	}
}

// ---------------------------------------------------------------------------
// 5. usage arriving after finish is merged into the final frame
// ---------------------------------------------------------------------------

func TestNativeChatUsageAfterFinishLandsInFinalFrame(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w,
			sseEnvelopeRaw(200, chunkJSON(map[string]any{"content": "hey"}, "stop")),
			sseEnvelope(200, map[string]any{"usage": map[string]any{"prompt_tokens": 20, "completion_tokens": 5, "cache_read_tokens": 4, "credits": 0.25}}),
		)
	}))
	defer upstream.Close()

	store := &countStore{fakeNativeStore: fakeNativeStore{account: globalAccount(upstream.URL), credential: encryptedCredential(t, machineID, nil)}}
	store.chatBase = upstream.URL
	client := nativeChatClient(t, store)

	resp, _, err := client.ChatStream(context.Background(), "acc-1", chatRequest("glm-5.2", true))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	frames := readSSEChunks(t, resp.Body)
	if len(frames) < 3 {
		t.Fatalf("frames = %v", frames)
	}
	// The finish_reason chunk carries no usage; the terminal usage chunk does.
	var usage map[string]any
	for _, frame := range frames {
		if frame == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []json.RawMessage `json:"choices"`
			Usage   map[string]any    `json:"usage"`
		}
		_ = json.Unmarshal([]byte(frame), &chunk)
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}
	if usage == nil {
		t.Fatalf("no usage frame in %v", frames)
	}
	if usage["prompt_tokens"] != float64(20) || usage["cache_read_tokens"] != float64(4) || usage["credits"] != 0.25 {
		t.Fatalf("usage = %v", usage)
	}
	if _, ok := usage["prompt_tokens_details"]; !ok {
		t.Fatalf("cache_read_tokens must mirror into prompt_tokens_details: %v", usage)
	}
}

// ---------------------------------------------------------------------------
// 6. non-stream collect + estimate fallback when usage is missing
// ---------------------------------------------------------------------------

func TestNativeChatNonStreamCollectsWithEstimateFallback(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w,
			sseEnvelopeRaw(200, chunkJSON(map[string]any{"content": "hel"}, "")),
			sseEnvelopeRaw(200, chunkJSON(map[string]any{"reasoning": "th"}, "")),
			sseEnvelopeRaw(200, chunkJSON(map[string]any{"content": "lo"}, "stop")),
		)
	}))
	defer upstream.Close()

	store := &countStore{fakeNativeStore: fakeNativeStore{account: globalAccount(upstream.URL), credential: encryptedCredential(t, machineID, nil)}}
	store.chatBase = upstream.URL
	client := nativeChatClient(t, store)

	outcome, err := client.ChatNonStream(context.Background(), "acc-1", chatRequest("glm-5.2", false))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Content != "hello" || outcome.Reasoning != "th" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if outcome.FinishReason != "stop" {
		t.Fatalf("finish = %q", outcome.FinishReason)
	}
	if outcome.UsageSource != "estimate" || outcome.PromptTokens != estimatePromptTokens([]translate.ChatMessage{{Role: "user", Content: "hi"}}) {
		t.Fatalf("usage = prompt %d source %q", outcome.PromptTokens, outcome.UsageSource)
	}
	if outcome.CompletionTokens != estimateTokens("helloth") {
		t.Fatalf("completion estimate = %d", outcome.CompletionTokens)
	}
}

// ---------------------------------------------------------------------------
// 7. proxy: chat goes through the proxy transport cache
// ---------------------------------------------------------------------------

func TestNativeChatRoutesThroughProxyTransport(t *testing.T) {
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		fmt.Fprint(w, sseEnvelopeRaw(200, chunkJSON(map[string]any{"content": "x"}, "stop")))
	}))
	defer upstream.Close()

	deadProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	deadProxyURL := deadProxy.URL
	deadProxy.Close() // nothing listens anymore: proxy connect must fail

	store := &countStore{fakeNativeStore: fakeNativeStore{account: globalAccount(upstream.URL), credential: encryptedCredential(t, machineID, nil)}}
	store.chatBase = upstream.URL
	store.proxy = deadProxyURL
	client := nativeChatClient(t, store)

	_, _, err := client.ChatStream(context.Background(), "acc-1", chatRequest("glm-5.2", true))
	if err == nil {
		t.Fatal("dead proxy must fail the chat attempt")
	}
	if !isTransportError(err) {
		t.Fatalf("error = %T (%v), want TransportError", err, err)
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("upstream saw %d hits; request must not bypass the proxy", upstreamHits.Load())
	}
}

func isTransportError(err error) bool {
	_, ok := err.(TransportError)
	return ok
}

// ---------------------------------------------------------------------------
// 8. identity cache: hit within credential generation, rebuild on refresh
// ---------------------------------------------------------------------------

func TestNativeChatIdentityCacheHitsAndInvalidates(t *testing.T) {
	var refreshAttempts atomic.Int64
	var chatAttempts atomic.Int64
	var cosies []string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/jobToken/refresh":
			refreshAttempts.Add(1)
			_, _ = w.Write([]byte(`{"token":"at-new","refresh_token":"jrt-new","expires_in":3600000}`))
		case r.Method == http.MethodPost && r.URL.Path == "/algo/api/v2/service/pro/sse/agent_chat_generation":
			chatAttempts.Add(1)
			mu.Lock()
			cosies = append(cosies, r.Header.Get("Cosy-Key"))
			mu.Unlock()
			if chatAttempts.Load() == 1 {
				fmt.Fprint(w, sseEnvelopeRaw(401, `{"error":{"message":"expired"}}`))
				return
			}
			fmt.Fprint(w, sseEnvelopeRaw(200, chunkJSON(map[string]any{"content": "ok"}, "stop")))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	store := &countStore{fakeNativeStore: fakeNativeStore{account: globalAccount(upstream.URL), credential: encryptedCredential(t, machineID, nil)}}
	store.chatBase = upstream.URL
	client := nativeChatClient(t, store)

	// First chat builds and caches the identity.
	resp1, _, err := client.ChatStream(context.Background(), "acc-1", chatRequest("glm-5.2", true))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp1.Body.Close()
	// Second chat within the same credential generation must not touch the
	// credential store again.
	beforeLoads := store.loadCalls.Load()
	resp2, _, err := client.ChatStream(context.Background(), "acc-1", chatRequest("glm-5.2", true))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if store.loadCalls.Load() != beforeLoads {
		t.Fatalf("identity cache missed: loads %d → %d", beforeLoads, store.loadCalls.Load())
	}
	mu.Lock()
	cachedCosy := cosies[len(cosies)-1]
	mu.Unlock()

	// After the 401-refresh roundtrip the cached identity must be gone: the
	// next chat rebuilds runtime fields (new Cosy-Key).
	resp3, _, err := client.ChatStream(context.Background(), "acc-1", chatRequest("glm-5.2", true))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp3.Body.Close()
	mu.Lock()
	afterCosy := cosies[len(cosies)-1]
	mu.Unlock()
	if cachedCosy == "" || afterCosy == "" || cachedCosy == afterCosy {
		t.Fatalf("refresh must invalidate the identity cache: %q vs %q", cachedCosy, afterCosy)
	}
}

// ---------------------------------------------------------------------------
// 9. headers: Cosy-User, Cosy-Version, CN header deletions
// ---------------------------------------------------------------------------

func TestNativeChatHeadersGlobalAndCN(t *testing.T) {
	// Task 8 真机录制(testdata/native/prepare_cn.json,2026-10-05):双
	// region 头矩阵一致——无 Appcode/Clientip/Machineos,Data-Policy 存在,
	// Machinetoken == MachineId,URL 均带 Encode=1。本测试按录制基线回归:
	// 各起一个假上游,分别服务 global 与 cn 账号。
	newCaptureServer := func(headersOut *http.Header, queryOut *url.Values) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*headersOut = r.Header.Clone()
			*queryOut = r.URL.Query()
			fmt.Fprint(w, sseEnvelopeRaw(200, chunkJSON(map[string]any{"content": "ok"}, "stop")))
		}))
	}

	var globalHeaders, cnHeaders http.Header
	var globalQuery, cnQuery url.Values
	globalUpstream := newCaptureServer(&globalHeaders, &globalQuery)
	defer globalUpstream.Close()
	cnUpstream := newCaptureServer(&cnHeaders, &cnQuery)
	defer cnUpstream.Close()

	store := &countStore{fakeNativeStore: fakeNativeStore{account: globalAccount(globalUpstream.URL), credential: encryptedCredential(t, machineID, nil)}}
	store.chatBase = globalUpstream.URL
	client := nativeChatClient(t, store)
	resp, _, err := client.ChatStream(context.Background(), "acc-1", chatRequest("glm-5.2", true))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if globalHeaders.Get("Cosy-User") != "u-1" || globalHeaders.Get("Cosy-Version") != qoderCLIVersion {
		t.Fatalf("global headers user=%q version=%q", globalHeaders.Get("Cosy-User"), globalHeaders.Get("Cosy-Version"))
	}
	if globalHeaders.Get("Cosy-Machineid") != machineID || globalHeaders.Get("X-Model-Key") != "glm-5.2" {
		t.Fatalf("global machine=%q modelkey=%q", globalHeaders.Get("Cosy-Machineid"), globalHeaders.Get("X-Model-Key"))
	}
	if globalHeaders.Get("Appcode") != "" || globalHeaders.Get("Cosy-Clientip") != "" || globalHeaders.Get("Cosy-Machineos") != "" {
		t.Fatalf("global must not carry Appcode/Clientip/Machineos: appcode=%q clientip=%q machineos=%q",
			globalHeaders.Get("Appcode"), globalHeaders.Get("Cosy-Clientip"), globalHeaders.Get("Cosy-Machineos"))
	}
	if globalQuery.Get("Encode") != "1" {
		t.Fatalf("global query must carry Encode=1: %v", globalQuery)
	}

	cnStore := &countStore{fakeNativeStore: fakeNativeStore{account: cnAccount(cnUpstream.URL), credential: encryptedCredential(t, machineID, nil)}}
	cnStore.chatBase = cnUpstream.URL
	cnClient := nativeChatClient(t, cnStore)
	cnResp, _, err := cnClient.ChatStream(context.Background(), "acc-1", chatRequest("glm-5.2", true))
	if err != nil {
		t.Fatal(err)
	}
	_ = cnResp.Body.Close()
	if cnHeaders.Get("Appcode") != "" || cnHeaders.Get("Cosy-Clientip") != "" {
		t.Fatalf("cn must not carry Appcode/Clientip (per recording): appcode=%q clientip=%q", cnHeaders.Get("Appcode"), cnHeaders.Get("Cosy-Clientip"))
	}
	if cnHeaders.Get("Cosy-Data-Policy") != "agree" {
		t.Fatalf("cn Cosy-Data-Policy = %q, want agree (per recording)", cnHeaders.Get("Cosy-Data-Policy"))
	}
	if cnHeaders.Get("Cosy-Machinetoken") != cnHeaders.Get("Cosy-Machineid") || cnHeaders.Get("Cosy-Machineid") == "" {
		t.Fatalf("cn machinetoken must equal machineid (per recording): %q vs %q", cnHeaders.Get("Cosy-Machinetoken"), cnHeaders.Get("Cosy-Machineid"))
	}
	if cnHeaders.Get("Cosy-User") != "u-1" {
		t.Fatalf("cn cosy user = %q", cnHeaders.Get("Cosy-User"))
	}
	if cnQuery.Get("Encode") != "1" {
		t.Fatalf("cn query must carry Encode=1 (per recording): %v", cnQuery)
	}
}

// ---------------------------------------------------------------------------
// 10. 401 envelope with a failing refresh surfaces the auth error
// ---------------------------------------------------------------------------

func TestNativeChat401RefreshFailureSurfacesAuthError(t *testing.T) {
	var chatAttempts atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/jobToken/refresh":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"TOKEN_EXPIRE"}`))
		case "/algo/api/v2/service/pro/sse/agent_chat_generation":
			chatAttempts.Add(1)
			fmt.Fprint(w, sseEnvelopeRaw(401, `{"error":{"message":"expired"}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	store := &countStore{fakeNativeStore: fakeNativeStore{account: globalAccount(upstream.URL), credential: encryptedCredential(t, machineID, nil)}}
	store.chatBase = upstream.URL
	client := nativeChatClient(t, store)

	_, _, err := client.ChatStream(context.Background(), "acc-1", chatRequest("glm-5.2", true))
	if err == nil {
		t.Fatal("refresh failure must surface")
	}
	var providerErr *providers.Error
	if !errors.As(err, &providerErr) {
		t.Fatalf("error = %T (%v), want *providers.Error", err, err)
	}
	if chatAttempts.Load() != 1 {
		t.Fatalf("attempts = %d, no blind retry after failed refresh", chatAttempts.Load())
	}
}

// ---------------------------------------------------------------------------
// 11. runtime fields on the request body feed ResolvedChat.ReasoningLevel
// ---------------------------------------------------------------------------

func TestNativeChatReasoningLevelEchoesClampedEffort(t *testing.T) {
	var sawParameters map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		decoded, err := decodeNativeBody(body)
		if err != nil {
			t.Errorf("decode encoded chat body: %v", err)
		}
		var parsed map[string]any
		_ = json.Unmarshal(decoded, &parsed)
		sawParameters, _ = parsed["parameters"].(map[string]any)
		fmt.Fprint(w, sseEnvelopeRaw(200, chunkJSON(map[string]any{"content": "x"}, "stop")))
	}))
	defer upstream.Close()

	store := &countStore{fakeNativeStore: fakeNativeStore{account: globalAccount(upstream.URL), credential: encryptedCredential(t, machineID, nil)}}
	store.chatBase = upstream.URL
	client := nativeChatClient(t, store)

	req := chatRequest("glm-5.2", true)
	req.ReasoningEffort = []byte(`"high"`)
	resp, resolved, err := client.ChatStream(context.Background(), "acc-1", req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resolved.ReasoningLevel != "high" {
		t.Fatalf("resolved level = %q", resolved.ReasoningLevel)
	}
	if sawParameters["reasoning_effort"] != "high" {
		t.Fatalf("body effort = %v", sawParameters["reasoning_effort"])
	}

	// Without reasoning params the level is the worker's "none" echo only when
	// enable_thinking exists; plain chat reports "none" via missing effort.
	reqNone := chatRequest("glm-5.2", true)
	respNone, resolvedNone, err := client.ChatStream(context.Background(), "acc-1", reqNone)
	if err != nil {
		t.Fatal(err)
	}
	_ = respNone.Body.Close()
	if resolvedNone.ReasoningLevel != "" {
		t.Fatalf("plain chat level = %q, want empty", resolvedNone.ReasoningLevel)
	}
	if _, present := sawParameters["reasoning_effort"]; present {
		t.Fatalf("plain chat must not carry reasoning_effort: %v", sawParameters)
	}
}

// ---------------------------------------------------------------------------
// 12. tool-call deltas stream with anonymous index backfill
// ---------------------------------------------------------------------------

func TestNativeChatStreamToolCallsBackfillIndexAndID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// No explicit index / id on either tool-call delta: the translator must
		// assign sequential indexes and stable placeholder ids.
		fmt.Fprint(w,
			sseEnvelopeRaw(200, chunkJSON(map[string]any{
				"tool_calls": []any{map[string]any{
					"type":     "function",
					"function": map[string]any{"name": "get", "arguments": "{\"q\""},
				}},
			}, "")),
			sseEnvelopeRaw(200, chunkJSON(map[string]any{
				"tool_calls": []any{map[string]any{
					"type":     "function",
					"function": map[string]any{"name": "", "arguments": ":1}"},
				}},
			}, "")),
			sseEnvelopeRaw(200, chunkJSON(map[string]any{}, "tool_calls")),
		)
	}))
	defer upstream.Close()

	store := &countStore{fakeNativeStore: fakeNativeStore{account: globalAccount(upstream.URL), credential: encryptedCredential(t, machineID, nil)}}
	store.chatBase = upstream.URL
	client := nativeChatClient(t, store)

	resp, _, err := client.ChatStream(context.Background(), "acc-1", chatRequest("glm-5.2", true))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var name, args string
	var indexes []float64
	var ids []string
	for _, frame := range readSSEChunks(t, resp.Body) {
		if frame == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    float64 `json:"index"`
						ID       string  `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(frame), &chunk); err != nil {
			t.Fatalf("frame %q: %v", frame, err)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		for _, tc := range chunk.Choices[0].Delta.ToolCalls {
			indexes = append(indexes, tc.Index)
			if tc.ID != "" {
				ids = append(ids, tc.ID)
			}
			name += tc.Function.Name
			args += tc.Function.Arguments
		}
	}
	if name != "get" || args != `{"q":1}` {
		t.Fatalf("tool call name=%q args=%q", name, args)
	}
	if len(indexes) != 2 || indexes[0] != 0 || indexes[1] != 0 {
		t.Fatalf("indexes = %v, both deltas must map to call 0", indexes)
	}
	if len(ids) != 1 || !strings.HasPrefix(ids[0], "call_") {
		t.Fatalf("ids = %v, placeholder id expected on the first delta", ids)
	}
}

var _ = time.Now
