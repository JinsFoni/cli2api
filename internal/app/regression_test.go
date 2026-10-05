package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"bytes"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/app"
	"github.com/caigee-cmd/cli2api/internal/config"
	"github.com/caigee-cmd/cli2api/internal/executor"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
	"github.com/caigee-cmd/cli2api/internal/update"
)

func regressionApp(t *testing.T) *app.App {
	t.Helper()
	a := app.New(config.Config{ProxyAPIKey: "old-key", QoderHome: t.TempDir(), DataDir: t.TempDir(), RuntimeDir: t.TempDir()})
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func serveRegression(h http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestConsoleKeyRotationUpdatesExistingHandlerAndUpstreamRequests(t *testing.T) {
	a := regressionApp(t)
	// Capture the production handler once, exactly as http.Server does.
	h := a.Handler()
	named, err := a.Control.Keys.Create(context.Background(), accounts.CreateAPIKey{Name: "rotation-test", Providers: []string{"qoder"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	currentKey := "old-key"
	for rotation := 0; rotation < 2; rotation++ {
		w := serveRegression(h, "POST", "/api/system/console-key", currentKey, `{"rotate":true}`)
		if w.Code != 200 {
			t.Fatalf("rotation: %d %s", w.Code, w.Body.String())
		}
		var result struct {
			Secret string `json:"secret"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Secret == "" || result.Secret == currentKey {
			t.Fatal("rotation did not mint a new key")
		}
		if got := serveRegression(h, "GET", "/api/system/console-key", currentKey, ""); got.Code != 401 {
			t.Errorf("old key: got %d want 401", got.Code)
		}
		if got := serveRegression(h, "GET", "/api/system/console-key", result.Secret, ""); got.Code != 200 {
			t.Errorf("new key: got %d want 200", got.Code)
		}
		if got := serveRegression(h, "GET", "/api/system/console-key", named.Secret, ""); got.Code != 403 {
			t.Errorf("named key accessed console: %d", got.Code)
		}
		if got := serveRegression(h, "GET", "/v1/models", named.Secret, ""); got.Code != 200 {
			t.Errorf("named key invalidated: %d", got.Code)
		}
		saved, _, err := a.Manager.Store().GetSecret(context.Background(), "proxy_api_key")
		if err != nil || saved != result.Secret {
			t.Fatal("rotated key not persisted")
		}
		if a.Manager.ProxyAPIKey() != result.Secret {
			t.Fatal("runtime key not updated")
		}
		currentKey = result.Secret
	}
	// A fake ready qoder account answers through the executor's in-process
	// adapter slot, backed by an OpenAI-speaking httptest upstream.
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"id\":\"test\",\"model\":\"glm-5.2\",\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		io.WriteString(w, `{"model":"glm-5.2","choices":[{"message":{"content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	account, err := a.Manager.Store().Create(context.Background(), accounts.CreateAccount{Name: "fake-upstream", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	a.Pool.Upsert(executor.Item{ID: account.ID, Provider: "qoder", Runtime: "in_process", URL: upstream.URL, Models: []string{"glm-5.2"}})
	a.Executor.Providers = providers.NewRegistry()
	a.Executor.Providers.Register(providers.Adapter{ID: "qoder", Chat: upstreamChatBridge{pool: a.Pool, http: upstream.Client()}})
	// The gateway captured its executor copy when Handler() was built; push
	// the bridge-wired executor into the live handler.
	a.Gateway.Executor = a.Executor
	// Catalog probing is tested separately; keep the fake upstream deterministic.
	a.Gateway.Catalogs = nil
	for _, path := range []string{"/v1/chat/completions", "/api/chat", "/v1/messages", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", path, stream), func(t *testing.T) {
				body := fmt.Sprintf(`{"model":"qoder/glm-5.2","messages":[{"role":"user","content":"hello"}],"max_tokens":32,"stream":%v}`, stream)
				if path == "/v1/responses" {
					body = fmt.Sprintf(`{"model":"qoder/glm-5.2","input":"hello","stream":%v}`, stream)
				}
				key := currentKey
				if path != "/api/chat" {
					key = named.Secret
				}
				got := serveRegression(h, "POST", path, key, body)
				if got.Code != 200 || !strings.Contains(got.Body.String(), "OK") {
					t.Errorf("chat failed: %d %s", got.Code, got.Body.String())
				}
			})
		}
	}
	if calls.Load() != 8 {
		t.Errorf("upstream requests=%d want 8", calls.Load())
	}
}

// upstreamChatBridge implements providers.ProviderChat by posting the request
// to the pool item's URL, so regression tests can stand up fake upstreams
// without any worker transport.
type upstreamChatBridge struct {
	pool *executor.Pool
	http *http.Client
}

func (b upstreamChatBridge) urlFor(accountID string) string {
	if item, ok := b.pool.ByID(accountID); ok {
		return item.URL
	}
	return ""
}

func (b upstreamChatBridge) ChatNonStream(ctx context.Context, accountID string, req translate.ChatRequest) (providers.ChatOutcome, error) {
	payload, _ := json.Marshal(map[string]any{"model": req.Model, "messages": req.Messages, "stream": false})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(b.urlFor(accountID), "/")+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(httpReq)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return providers.ChatOutcome{}, &providers.Error{Status: resp.StatusCode, Message: string(raw)}
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.Choices) == 0 {
		return providers.ChatOutcome{}, fmt.Errorf("decode upstream reply: %v", err)
	}
	return providers.ChatOutcome{Content: decoded.Choices[0].Message.Content}, nil
}

func (b upstreamChatBridge) ChatStream(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	payload, _ := json.Marshal(map[string]any{"model": req.Model, "messages": req.Messages, "stream": true})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(b.urlFor(accountID), "/")+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(httpReq)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, providers.ResolvedChat{}, &providers.Error{Status: resp.StatusCode, Message: string(raw)}
	}
	return resp, providers.ResolvedChat{}, nil
}

func TestSaturatedPoolKeepsFiveSecondRetryAfter(t *testing.T) {
	a := regressionApp(t)
	h := a.Handler()
	var calls atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
		t.Error("saturated pool must not call upstream")
	}))
	defer worker.Close()
	account, err := a.Manager.Store().Create(context.Background(), accounts.CreateAccount{Name: "saturated", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	a.Pool.Upsert(executor.Item{
		ID: account.ID, Provider: "qoder", Runtime: "in_process", URL: worker.URL,
		Models: []string{"glm-5.2"}, MaxInFlight: 1,
	})
	a.Pool.MergeHealth(account.ID, true, true, 1, 0, "")
	a.Gateway.Catalogs = nil
	for _, path := range []string{"/v1/chat/completions", "/api/chat", "/v1/messages", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", path, stream), func(t *testing.T) {
				body := fmt.Sprintf(`{"model":"qoder/glm-5.2","messages":[{"role":"user","content":"hello"}],"max_tokens":32,"stream":%v}`, stream)
				if path == "/v1/messages" {
					body = fmt.Sprintf(`{"model":"qoder/glm-5.2","messages":[{"role":"user","content":"hello"}],"max_tokens":32,"stream":%v}`, stream)
				}
				if path == "/v1/responses" {
					body = fmt.Sprintf(`{"model":"qoder/glm-5.2","input":"hello","stream":%v}`, stream)
				}
				got := serveRegression(h, "POST", path, "old-key", body)
				if got.Code != http.StatusTooManyRequests {
					t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
				}
				if got.Header().Get("Retry-After") != "5" {
					t.Fatalf("Retry-After=%q want 5 body=%s", got.Header().Get("Retry-After"), got.Body.String())
				}
				item, _ := a.Pool.ByID(account.ID)
				if !item.DownUntil.IsZero() {
					t.Fatalf("capacity error cooled account until %v", item.DownUntil)
				}
			})
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls=%d", calls.Load())
	}
}

type concurrentChecker struct{ calls atomic.Int32 }

func (c *concurrentChecker) Check(context.Context, bool) (update.Info, error) {
	c.calls.Add(1)
	return update.Info{}, nil
}

type concurrentAgent struct{ calls atomic.Int32 }

func (a *concurrentAgent) Status(context.Context) (update.AgentStatus, error) {
	a.calls.Add(1)
	return update.AgentStatus{}, nil
}
func (a *concurrentAgent) Apply(context.Context, update.ApplyRequest) (update.ApplyResponse, error) {
	return update.ApplyResponse{}, nil
}

func TestConcurrentUpdateInfoUsesStableDependencies(t *testing.T) {
	a := regressionApp(t)
	checker := &concurrentChecker{}
	agent := &concurrentAgent{}
	a.Update.Checker = checker
	a.Update.Agent = agent
	coord := a.Update
	h := a.Handler()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if got := serveRegression(h, "GET", "/api/system/update", "old-key", ""); got.Code != 200 {
					t.Errorf("update GET: %d", got.Code)
				}
			}
		}()
	}
	wg.Wait()
	if a.Update != coord || a.Console.Update != coord || a.HTTP.Update != coord {
		t.Fatal("coordinator identity changed")
	}
	if checker.calls.Load() != 160 || agent.calls.Load() != 160 {
		t.Fatal("injected dependencies were not used")
	}
}

func TestConsoleKeyRotationConcurrentWithHTTPReaders(t *testing.T) {
	a := regressionApp(t)
	h := a.Handler()
	key := atomic.Pointer[string]{}
	initial := "old-key"
	key.Store(&initial)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				// Either answer is valid if the key rotates between snapshot and auth.
				secret := *key.Load()
				got := serveRegression(h, "GET", "/api/system/console-key", secret, "")
				if got.Code != 200 && got.Code != 401 {
					t.Errorf("concurrent key read: %d", got.Code)
				}
			}
		}()
	}
	for i := 0; i < 8; i++ {
		got := serveRegression(h, "POST", "/api/system/console-key", *key.Load(), `{"rotate":true}`)
		if got.Code != 200 {
			t.Errorf("rotation: %d", got.Code)
			break
		}
		var result struct {
			Secret string `json:"secret"`
		}
		if err := json.Unmarshal(got.Body.Bytes(), &result); err != nil {
			t.Error(err)
			break
		}
		key.Store(&result.Secret)
	}
	wg.Wait()
	if got := serveRegression(h, "GET", "/api/system/console-key", *key.Load(), ""); got.Code != 200 {
		t.Errorf("final key rejected: %d", got.Code)
	}
}
