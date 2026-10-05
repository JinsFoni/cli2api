package qoder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/proxy"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// Client wraps S08 worker transport behind providers.Adapter slots.
// It still talks to a child-process worker URL; it is not an in-process
// cloud client. Callers must keep using the child-process path for
// probe/quota/login/chat until those slices are accepted.
type Client struct {
	mu sync.RWMutex

	locate      func(accountID string) (string, bool)
	proxyAPIKey func() string

	// nativeStore binds the in-process control plane (check-in, native quota,
	// credential probing) to the account store. Nil keeps the pure worker
	// behavior for callers that only need chat routing.
	nativeStore NativeStore

	// transports caches proxy transports for direct control-plane calls.
	transports proxy.TransportCache

	modelsHTTP *http.Client
	healthHTTP *http.Client
	quotaHTTP  *http.Client
	adminHTTP  *http.Client
	chatHTTP   *http.Client
	nativeBase *http.Client

	// endpointsOverride replaces the openapi hosts for native control-plane
	// calls; set only by tests.
	endpointsOverride map[string]nativeEndpoints

	// identities caches built chat identities (runtime fields) per account;
	// nil until the first native chat.
	identities *identityCache

	loginTimeout  time.Duration
	loginInterval time.Duration
}

func NewClient(store NativeStore) *Client {
	return &Client{
		nativeStore:   store,
		modelsHTTP:    &http.Client{Timeout: 15 * time.Second},
		healthHTTP:    &http.Client{Timeout: 2 * time.Second},
		quotaHTTP:     &http.Client{Timeout: 5 * time.Second},
		adminHTTP:     &http.Client{Timeout: 120 * time.Second},
		chatHTTP:      &http.Client{Timeout: 120 * time.Second},
		nativeBase:    &http.Client{Timeout: 15 * time.Second},
		loginTimeout:  90 * time.Second,
		loginInterval: 200 * time.Millisecond,
	}
}

func (c *Client) Bind(locate func(string) (string, bool), proxyAPIKey func() string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.locate = locate
	c.proxyAPIKey = proxyAPIKey
	c.mu.Unlock()
}

func (c *Client) SetHTTP(httpClient *http.Client) {
	if c == nil || httpClient == nil {
		return
	}
	c.mu.Lock()
	c.modelsHTTP = httpClient
	c.healthHTTP = httpClient
	c.quotaHTTP = httpClient
	c.adminHTTP = httpClient
	c.chatHTTP = httpClient
	c.mu.Unlock()
}

func (c *Client) SetLoginWait(timeout, interval time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.loginTimeout = timeout
	c.loginInterval = interval
	c.mu.Unlock()
}

func (c *Client) Adapter() providers.Adapter {
	// Check-in, quota, models, and readiness are all native now (direct
	// Bearer/COSY HTTP + stored credential). Probe for qoder never fabricates
	// model availability: routeBaseMatches still requires a catalog or proven
	// model hit for the concrete request.
	return providers.Adapter{
		ID:      "qoder",
		Login:   c,
		Chat:    c,
		Models:  c,
		Prober:  c,
		Checkin: c,
	}
}

func (c *Client) key() string {
	c.mu.RLock()
	fn := c.proxyAPIKey
	c.mu.RUnlock()
	if fn == nil {
		return ""
	}
	return fn()
}

func (c *Client) Models(ctx context.Context, accountID string) ([]providers.ModelInfo, error) {
	return c.FetchModelsNative(ctx, accountID)
}

func numberField(entry map[string]any, key string) int {
	if value, ok := numberFieldValue(entry, key); ok {
		return value
	}
	return 0
}

func numberFieldValue(entry map[string]any, key string) (int, bool) {
	switch value := entry[key].(type) {
	case float64:
		return int(value), true
	case int:
		return value, true
	case json.Number:
		n, err := value.Int64()
		return int(n), err == nil
	default:
		return 0, false
	}
}

func intSliceField(entry map[string]any, key string) ([]int, bool) {
	raw, ok := entry[key].([]any)
	if !ok {
		if typed, ok := entry[key].([]int); ok {
			return typed, len(typed) > 0
		}
		return nil, false
	}
	out := make([]int, 0, len(raw))
	for _, item := range raw {
		switch value := item.(type) {
		case float64:
			out = append(out, int(value))
		case int:
			out = append(out, value)
		case json.Number:
			if n, err := value.Int64(); err == nil {
				out = append(out, int(n))
			}
		}
	}
	return out, len(out) > 0
}

// largestContextWindow is the largest selectable window, when Qoder
// advertises more than the default (`available_context_windows`).
func largestContextWindow(entry map[string]any) (int, bool) {
	windows, ok := intSliceField(entry, "available_context_windows")
	if !ok {
		return 0, false
	}
	largest := 0
	for _, window := range windows {
		if window > largest {
			largest = window
		}
	}
	if largest <= 0 {
		return 0, false
	}
	return largest, true
}

// contextWindowDefault is the model's default context window: Qoder's
// `default_context_window` when present, else the legacy `context_length`.
func contextWindowDefault(entry map[string]any) int {
	if window, ok := numberFieldValue(entry, "default_context_window"); ok && window > 0 {
		return window
	}
	if window, ok := numberFieldValue(entry, "context_length"); ok && window > 0 {
		return window
	}
	return 0
}

func ModelInfos(entries []map[string]any) []providers.ModelInfo {
	out := make([]providers.ModelInfo, 0, len(entries))
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		publicID := stringField(entry, "id")
		native := stringField(entry, "mapped_key", "native_model")
		if native == "" {
			native = publicID
		}
		info := providers.ModelInfo{
			PublicModel: publicID,
			NativeModel: native,
			DisplayName: stringField(entry, "display_name"),
			Credits:     qoderEntryCredits(entry),
			Free:        qoderEntryFree(entry),
			Capabilities: providers.ModelCapabilities{
				ContextWindow: contextWindowDefault(entry),
				Reasoning:     boolField(entry, "is_reasoning"),
				Tools:         true,
				Images:        true,
			},
		}
		if maxWindow, ok := largestContextWindow(entry); ok {
			if maxWindow > info.Capabilities.ContextWindow {
				info.Capabilities.ContextWindowMax = maxWindow
			}
		}
		if output, ok := numberFieldValue(entry, "max_output_tokens"); ok && output > 0 {
			info.Capabilities.MaxOutput = output
		}
		if info.PublicModel == "" && info.NativeModel == "" {
			continue
		}
		out = append(out, info)
	}
	return out
}

// qoderEntryCredits renders a catalog entry's price as the console credits text,
// mirroring the Qoder client label: a model tagged `limited_time_free` reads as
// "0" (the console then shows its free badge); otherwise the numeric
// `price_factor` renders as `<factor>x`. An explicit upstream `credits` string
// wins. Returns "" when Qoder reported neither.
func qoderEntryCredits(entry map[string]any) string {
	if explicit := stringField(entry, "credits"); explicit != "" {
		return explicit
	}
	if qoderEntryLimitedTimeFree(entry) {
		return "0"
	}
	factor, ok := floatField(entry, "price_factor")
	if !ok {
		return ""
	}
	if factor <= 0 {
		return "0"
	}
	return "x" + strconv.FormatFloat(factor, 'f', -1, 64)
}

// qoderEntryLimitedTimeFree is the Qoder client's own free signal: the
// `limited_time_free` tag.
func qoderEntryLimitedTimeFree(entry map[string]any) bool {
	for _, tag := range stringSliceField(entry, "tags") {
		if strings.EqualFold(strings.TrimSpace(tag), "limited_time_free") {
			return true
		}
	}
	return false
}

// qoderEntryFree reports whether the model should carry the console's free
// badge. It is derived from the same credits it renders, so the badge can never
// contradict a positive multiplier: a `limited_time_free` tag or a zero factor
// is free, and a positive factor is not (Qwen3.8-Max reports is_free=true with
// a 0.5 factor and the Qoder client still shows "0.50x Credit").
func qoderEntryFree(entry map[string]any) bool {
	if qoderEntryLimitedTimeFree(entry) {
		return true
	}
	factor, ok := floatField(entry, "price_factor")
	return ok && factor <= 0
}

func stringSliceField(entry map[string]any, key string) []string {
	if typed, ok := entry[key].([]string); ok {
		return typed
	}
	raw, ok := entry[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func floatField(entry map[string]any, key string) (float64, bool) {
	switch value := entry[key].(type) {
	case float64:
		return value, true
	case int:
		return float64(value), true
	case json.Number:
		f, err := value.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func CatalogIDsFromInfos(models []providers.ModelInfo) []string {
	ids := make([]string, 0, len(models)*3)
	for _, model := range models {
		ids = append(ids, model.PublicModel, model.NativeModel, model.DisplayName)
	}
	return ids
}

func (c *Client) Probe(ctx context.Context, accountID string) (providers.AccountHealth, error) {
	// Native readiness: the credential must decode and the chat identity
	// (runtime fields, COSY pair) must assemble. Model routing still requires
	// a catalog hit, so this never fabricates model availability.
	account, cred, err := c.resolvedCredential(ctx, accountID)
	if err != nil {
		return providers.AccountHealth{LastError: err.Error()}, nil
	}
	if _, err := buildChatIdentity(account, cred); err != nil {
		return providers.AccountHealth{LastError: err.Error()}, nil
	}
	return providers.AccountHealth{Ready: true, Hot: true, UID: cred.UID}, nil
}

func (c *Client) Quota(ctx context.Context, accountID string) (*providers.QuotaInfo, error) {
	snapshot, err := c.QuotaSnapshot(ctx, accountID, false)
	if err != nil || snapshot == nil {
		return nil, err
	}
	return &providers.QuotaInfo{
		Used:       snapshot.Used,
		Total:      snapshot.Total,
		Remaining:  snapshot.Remaining,
		Percentage: snapshot.Percentage,
		Unit:       snapshot.Unit,
		Exceeded:   snapshot.Exceeded,
		FetchedAt:  snapshot.FetchedAt,
	}, nil
}

func (c *Client) QuotaSnapshot(ctx context.Context, accountID string, force bool) (*accounts.QuotaSnapshot, error) {
	return c.nativeQuotaSnapshot(ctx, accountID)
}

// errNativeUnavailable marks structural reasons the in-process quota path
// cannot even be attempted; callers may fall back to the worker for these.
var errNativeUnavailable = errors.New("qoder native quota unavailable")

func (c *Client) nativeQuotaSnapshot(ctx context.Context, accountID string) (*accounts.QuotaSnapshot, error) {
	if c.nativeStore == nil {
		return nil, fmt.Errorf("%w: store not bound", errNativeUnavailable)
	}
	account, cred, err := c.resolvedCredential(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errNativeUnavailable, err)
	}
	if _, ok := c.endpoint(account.ProviderRegion); !ok {
		return nil, fmt.Errorf("%w: region %q has no openapi endpoint", errNativeUnavailable, account.ProviderRegion)
	}
	httpClient, err := c.nativeHTTP(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errNativeUnavailable, err)
	}
	quota, err := c.fetchNativeQuota(ctx, httpClient, account, cred)
	if err != nil {
		return nil, err
	}
	return quota.snapshot(), nil
}

// StartLogin starts the native device-code login flow for an account. The
// device flow (worker-era) is retired; PAT login is the supported path, but
// StartLogin/PollLogin remain for providers.Login interface completeness.
func (c *Client) StartLogin(ctx context.Context, accountID string) (providers.LoginSession, error) {
	return providers.LoginSession{}, fmt.Errorf("qoder device login is retired; use PAT login")
}

func (c *Client) PollLogin(ctx context.Context, accountID string) (bool, string, error) {
	return false, "", fmt.Errorf("qoder device login is retired; use PAT login")
}

func BuildChatPayload(req translate.ChatRequest, stream bool) map[string]any {
	payload := map[string]any{
		"model":    req.Model,
		"messages": req.Messages,
		"stream":   stream,
	}
	if len(req.MaxCompletionTokens) > 0 {
		payload["max_tokens"] = req.MaxCompletionTokens
	} else if len(req.MaxTokens) > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	if len(req.Temperature) > 0 {
		payload["temperature"] = json.RawMessage(req.Temperature)
	}
	if len(req.TopP) > 0 {
		payload["top_p"] = json.RawMessage(req.TopP)
	}
	if len(req.Stop) > 0 {
		payload["stop"] = json.RawMessage(req.Stop)
	}
	if req.ParallelToolCalls != nil {
		payload["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if len(req.ResponseFormat) > 0 {
		payload["response_format"] = json.RawMessage(req.ResponseFormat)
	}
	if req.IsReasoning != nil {
		payload["is_reasoning"] = *req.IsReasoning
	}
	if req.EnableThinking != nil {
		payload["enable_thinking"] = *req.EnableThinking
	}
	if req.EnableReasoning != nil {
		payload["enable_reasoning"] = *req.EnableReasoning
	}
	if len(req.Thinking) > 0 {
		payload["thinking"] = json.RawMessage(req.Thinking)
	}
	if len(req.ReasoningEffort) > 0 {
		payload["reasoning_effort"] = json.RawMessage(req.ReasoningEffort)
	}
	if len(req.ReasoningBudgetTokens) > 0 {
		payload["reasoning_budget_tokens"] = json.RawMessage(req.ReasoningBudgetTokens)
	}
	if len(req.ContextLength) > 0 {
		payload["context_length"] = json.RawMessage(req.ContextLength)
	}
	if len(req.MaxInputTokens) > 0 {
		payload["max_input_tokens"] = json.RawMessage(req.MaxInputTokens)
	}
	if len(req.Tools) > 0 {
		payload["tools"] = json.RawMessage(req.Tools)
	}
	if len(req.ToolChoice) > 0 {
		payload["tool_choice"] = json.RawMessage(req.ToolChoice)
	}
	return payload
}

func stringField(entry map[string]any, keys ...string) string {
	for _, key := range keys {
		value, _ := entry[key].(string)
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func boolField(entry map[string]any, key string) bool {
	value, _ := entry[key].(bool)
	return value
}
