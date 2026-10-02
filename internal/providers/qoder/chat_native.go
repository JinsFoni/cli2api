package qoder

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// identityCache lazily creates the per-client identity cache.
func (c *Client) identityCache() *identityCache {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.identities == nil {
		c.identities = newIdentityCache()
	}
	return c.identities
}

// ---------------------------------------------------------------------------
// upstream SSE envelope parsing (worker/src/sse.mjs semantics)
// ---------------------------------------------------------------------------

// upstreamEnvelope is one `data: {headers, body, statusCodeValue}` frame.
type upstreamEnvelope struct {
	Body             json.RawMessage `json:"body"`
	StatusCodeValue  *int            `json:"statusCodeValue"`
	StatusCodeCompat *int            `json:"statusCode"`
}

// upstreamBody carries the decoded envelope body.
type upstreamBody struct {
	done bool
	err  error
	raw  []byte
}

// decodeUpstreamBody unwraps the envelope body: a JSON-encoded string is the
// nested OpenAI chunk, "[DONE]" closes the stream, objects pass through.
func decodeUpstreamBody(envelope upstreamEnvelope) upstreamBody {
	if len(envelope.Body) == 0 {
		return upstreamBody{}
	}
	trimmed := bytes.TrimSpace(envelope.Body)
	if bytes.Equal(trimmed, []byte(`"[DONE]"`)) || bytes.Equal(trimmed, []byte(`[DONE]`)) {
		return upstreamBody{done: true}
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return upstreamBody{err: err}
		}
		if text == "[DONE]" {
			return upstreamBody{done: true}
		}
		trimmed = []byte(text)
	}
	return upstreamBody{raw: trimmed}
}

// upstreamError is the best nested error candidate seen on the stream.
type upstreamError struct {
	status int
	body   map[string]any
}

const (
	upstreamErrorQuotaBonus   = 100
	upstreamErrorLimitBonus   = 90
	upstreamErrorRateBonus    = 40
	upstreamErrorGenericMalus = -50
	upstreamErrorBase         = 1
)

// scoreUpstreamError mirrors worker scoreUpstreamError: quota-shaped errors
// beat generic wrappers when several candidates were seen.
func scoreUpstreamError(err upstreamError) int {
	score := upstreamErrorBase
	code := strings.ToLower(stringField(err.body, "code"))
	typ := strings.ToLower(stringField(err.body, "type"))
	message := strings.ToLower(firstNonEmptyString(
		stringField(err.body, "message"),
		stringField(err.body, "localizedMessage"),
	))
	switch {
	case code == "insufficient_quota" || typ == "insufficient_quota":
		score += upstreamErrorQuotaBonus
	case strings.Contains(message, "exceeded your current quota") || strings.Contains(message, "token-limit"):
		score += upstreamErrorLimitBonus
	case strings.Contains(code, "429") || strings.Contains(message, "response code=429") || strings.Contains(message, "too many requests"):
		score += upstreamErrorRateBonus
	}
	if strings.Contains(message, "unknown sse issue") {
		score += upstreamErrorGenericMalus
	}
	return score
}

// rememberUpstreamError keeps the higher-scored candidate.
func rememberUpstreamError(prev, next *upstreamError) *upstreamError {
	if next == nil {
		return prev
	}
	if prev == nil || scoreUpstreamError(*next) >= scoreUpstreamError(*prev) {
		return next
	}
	return prev
}

// ---------------------------------------------------------------------------
// usage extraction (worker/src/usage.mjs semantics)
// ---------------------------------------------------------------------------

var (
	usagePromptKeys     = []string{"prompt_tokens", "input_tokens", "input_token_count", "total_input_tokens", "promptTokens", "inputTokens"}
	usageCompletionKeys = []string{"completion_tokens", "output_tokens", "output_token_count", "total_output_tokens", "completionTokens", "outputTokens"}
	usageTotalKeys      = []string{"total_tokens", "total_token_count", "totalTokens"}
	usageCacheReadKeys  = []string{"cache_read_tokens", "cache_read_input_tokens", "cached_tokens", "cached_content_token_count"}
	usageCacheWriteKeys = []string{"cache_write_tokens", "cache_creation_input_tokens", "cache_creation_tokens"}
	usageCreditKeys     = []string{"credits", "total_credits", "original_credits"}
)

// usageUsage is the flattened upstream usage plus its source marker.
type usageUsage struct {
	PromptTokens     *float64 `json:"prompt_tokens,omitempty"`
	CompletionTokens *float64 `json:"completion_tokens,omitempty"`
	TotalTokens      *float64 `json:"total_tokens,omitempty"`
	CacheReadTokens  *float64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *float64 `json:"cache_write_tokens,omitempty"`
	Credits          *float64 `json:"credits,omitempty"`
	Source           string   `json:"source"`
}

// usageCount coerces one usage value; negatives are ignored like the worker.
func usageCount(value any) *float64 {
	switch typed := value.(type) {
	case float64:
		if typed >= 0 {
			return &typed
		}
	case json.Number:
		if parsed, err := typed.Float64(); err == nil && parsed >= 0 {
			return &parsed
		}
	case string:
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64); err == nil && parsed >= 0 {
			return &parsed
		}
	}
	return nil
}

func pickUsageKey(object map[string]any, keys []string) *float64 {
	for _, key := range keys {
		if value, ok := object[key]; ok {
			if count := usageCount(value); count != nil {
				return count
			}
		}
	}
	return nil
}

// accumulateUsage walks one decoded body for usage-shaped keys (depth-bounded,
// worker usage.mjs extractUsage semantics).
func accumulateUsage(best *usageUsage, value any, depth int) {
	if value == nil || depth > 8 {
		return
	}
	switch typed := value.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
			return
		}
		var parsed any
		if json.Unmarshal([]byte(trimmed), &parsed) == nil {
			accumulateUsage(best, parsed, depth+1)
		}
	case []any:
		for _, item := range typed {
			accumulateUsage(best, item, depth+1)
		}
	case map[string]any:
		if found := pickUsageKey(typed, usagePromptKeys); found != nil {
			best.PromptTokens = found
		}
		if found := pickUsageKey(typed, usageCompletionKeys); found != nil {
			best.CompletionTokens = found
		}
		if found := pickUsageKey(typed, usageTotalKeys); found != nil {
			best.TotalTokens = found
		}
		if found := pickUsageKey(typed, usageCacheReadKeys); found != nil {
			best.CacheReadTokens = found
		}
		if found := pickUsageKey(typed, usageCacheWriteKeys); found != nil {
			best.CacheWriteTokens = found
		}
		if found := pickUsageKey(typed, usageCreditKeys); found != nil {
			best.Credits = found
		}
		for _, key := range []string{"prompt_tokens_details", "usage", "llm_model_result", "body", "data"} {
			if nested, ok := typed[key]; ok {
				accumulateUsage(best, nested, depth+1)
			}
		}
	}
}

// usageLooksUseful reports whether the body carries any usage signal.
func usageLooksUseful(value any) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if _, ok := object["usage"]; ok {
		return true
	}
	if _, ok := object["llm_model_result"]; ok {
		return true
	}
	for _, keys := range [][]string{usagePromptKeys, usageCompletionKeys, usageCacheReadKeys, usageCacheWriteKeys, usageCreditKeys} {
		if pickUsageKey(object, keys) != nil {
			return true
		}
	}
	return false
}

// resolveUsage merges the accumulated upstream usage over the fallback frame.
func resolveUsage(best usageUsage, fallback usageUsage) usageUsage {
	merged := usageUsage{
		PromptTokens:     best.PromptTokens,
		CompletionTokens: best.CompletionTokens,
		TotalTokens:      best.TotalTokens,
		CacheReadTokens:  best.CacheReadTokens,
		CacheWriteTokens: best.CacheWriteTokens,
		Credits:          best.Credits,
	}
	if merged.PromptTokens == nil {
		merged.PromptTokens = fallback.PromptTokens
	}
	if merged.CompletionTokens == nil {
		merged.CompletionTokens = fallback.CompletionTokens
	}
	if merged.TotalTokens == nil && merged.PromptTokens != nil && merged.CompletionTokens != nil {
		total := *merged.PromptTokens + *merged.CompletionTokens
		merged.TotalTokens = &total
	}
	merged.Source = "estimate"
	if best.PromptTokens != nil || best.CompletionTokens != nil || best.CacheReadTokens != nil || best.CacheWriteTokens != nil {
		merged.Source = "upstream"
	} else if fallback.Source != "" {
		merged.Source = fallback.Source
	}
	return merged
}

// usageMap renders the usage for the outbound JSON frame, mirroring worker
// key mapping: cache_read mirrors into prompt_tokens_details.cached_tokens.
func (u usageUsage) usageMap() map[string]any {
	out := map[string]any{}
	if u.PromptTokens != nil {
		out["prompt_tokens"] = *u.PromptTokens
	}
	if u.CompletionTokens != nil {
		out["completion_tokens"] = *u.CompletionTokens
	}
	if u.TotalTokens != nil {
		out["total_tokens"] = *u.TotalTokens
	}
	out["source"] = u.Source
	if u.CacheReadTokens != nil {
		out["cache_read_tokens"] = *u.CacheReadTokens
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": *u.CacheReadTokens}
	}
	if u.CacheWriteTokens != nil {
		out["cache_write_tokens"] = *u.CacheWriteTokens
	}
	if u.Credits != nil {
		out["credits"] = *u.Credits
	}
	return out
}

// ---------------------------------------------------------------------------
// chunk translation (worker/src/sse.mjs pipeNestedSseToOpenAI)
// ---------------------------------------------------------------------------

// toolCallBuffer accumulates one streamed tool call.
type toolCallBuffer struct {
	ID        string
	Type      string
	Name      string
	Arguments string
}

// chatStreamState is the per-stream translation state shared by the head
// probe and the piped body.
type chatStreamState struct {
	Model   string
	ChatID  string
	Created int64

	content           strings.Builder
	reasoning         strings.Builder
	roleSent          bool
	finishEmitted     bool
	toolCalls         map[int]*toolCallBuffer
	nextToolCallIndex int
	anonymousIndexes  map[string]int
	finishReason      string
	sawDone           bool
	sawError          *upstreamError
	usage             usageUsage
	promptTokens      int
}

func newChatStreamState(model string, promptTokens int) *chatStreamState {
	return &chatStreamState{
		Model:            model,
		ChatID:           "chatcmpl-" + newUUIDv4(),
		Created:          time.Now().Unix(),
		promptTokens:     promptTokens,
		toolCalls:        map[int]*toolCallBuffer{},
		anonymousIndexes: map[string]int{},
	}
}

// finalizeUsage resolves the usage at stream end: upstream numbers win, the
// estimate fallback covers prompt tokens and the actual content+reasoning
// size (worker resolveUsage semantics).
func (s *chatStreamState) finalizeUsage() usageUsage {
	return resolveUsage(s.usage, usageUsage{
		PromptTokens:     float64Ptr(float64(s.promptTokens)),
		CompletionTokens: float64Ptr(float64(estimateTokens(s.content.String() + s.reasoning.String()))),
		Source:           "estimate",
	})
}

func float64Ptr(value float64) *float64 { return &value }

// writeChunk emits one OpenAI-style SSE data frame.
func (s *chatStreamState) writeChunk(w io.Writer, delta map[string]any, finish string) error {
	if delta == nil {
		delta = map[string]any{}
	}
	payload, err := json.Marshal(map[string]any{
		"id":      s.ChatID,
		"object":  "chat.completion.chunk",
		"created": s.Created,
		"model":   s.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	})
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: "+string(payload)+"\n\n"); err != nil {
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// ensureRole emits the leading role delta once.
func (s *chatStreamState) ensureRole(w io.Writer) error {
	if s.roleSent {
		return nil
	}
	if err := s.writeChunk(w, map[string]any{"role": "assistant"}, ""); err != nil {
		return err
	}
	s.roleSent = true
	return nil
}

// finishChunk writes the finish_reason chunk unless one already went out
// with the last delta (upstream frames can carry content and finish_reason
// together; the marker must appear exactly once).
func (s *chatStreamState) finishChunk(w io.Writer) error {
	if s.finishEmitted {
		return nil
	}
	if err := s.ensureRole(w); err != nil {
		return err
	}
	if err := s.writeChunk(w, map[string]any{}, s.finishReason); err != nil {
		return err
	}
	s.finishEmitted = true
	return nil
}

// handleBody folds one decoded upstream body into the outbound stream.
func (s *chatStreamState) handleBody(w io.Writer, body map[string]any, statusCode *int) error {
	if usageLooksUseful(body) {
		accumulateUsage(&s.usage, body, 0)
	}
	if rawError, ok := body["error"]; ok {
		if _, hasChoices := body["choices"]; !hasChoices {
			s.noteError(rawError, 0)
			return nil
		}
	}
	if _, hasChoices := body["choices"]; !hasChoices {
		if stringField(body, "code") != "" || stringField(body, "msgCode") != "" {
			s.noteError(body, 0)
			return nil
		}
	}
	if statusCode != nil && *statusCode >= 400 {
		if _, hasChoices := body["choices"]; !hasChoices {
			s.noteError(body["error"], *statusCode)
			return nil
		}
	}

	choices, _ := body["choices"].([]any)
	var choice map[string]any
	if len(choices) > 0 {
		choice, _ = choices[0].(map[string]any)
	}
	if choice == nil {
		choice = map[string]any{}
	}
	if finish := stringField(choice, "finish_reason"); finish != "" {
		s.finishReason = finish
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		if message, ok := choice["message"].(map[string]any); ok {
			delta = message
		} else {
			delta = map[string]any{}
		}
	}

	outDelta := map[string]any{}
	if text := stringField(delta, "content"); text != "" {
		s.content.WriteString(text)
		outDelta["content"] = text
	}
	if reasoning := stringField(delta, "reasoning_content"); reasoning != "" {
		s.reasoning.WriteString(reasoning)
		outDelta["reasoning_content"] = reasoning
	} else if reasoning := stringField(delta, "reasoning"); reasoning != "" {
		s.reasoning.WriteString(reasoning)
		outDelta["reasoning_content"] = reasoning
	}

	if rawToolCalls, ok := delta["tool_calls"].([]any); ok && len(rawToolCalls) > 0 {
		outCalls := make([]map[string]any, 0, len(rawToolCalls))
		assignedThisDelta := map[int]bool{}
		for _, rawCall := range rawToolCalls {
			call, _ := rawCall.(map[string]any)
			if call == nil {
				continue
			}
			idx, out := s.assignToolCall(call, assignedThisDelta)
			assignedThisDelta[idx] = true
			outCalls = append(outCalls, out)
		}
		if len(outCalls) > 0 {
			outDelta["tool_calls"] = outCalls
		}
	}

	if len(outDelta) == 0 {
		return nil
	}
	if err := s.ensureRole(w); err != nil {
		return err
	}
	if err := s.writeChunk(w, outDelta, s.finishReason); err != nil {
		return err
	}
	if s.finishReason != "" {
		s.finishEmitted = true
	}
	return nil
}

// assignToolCall mirrors the worker's anonymous-index backfill: explicit index
// wins, then a known anonymous key (id or name), then the single-open-call
// rule, then the next free slot.
func (s *chatStreamState) assignToolCall(call map[string]any, assignedThisDelta map[int]bool) (int, map[string]any) {
	fn, _ := call["function"].(map[string]any)
	if fn == nil {
		fn = map[string]any{}
	}
	name := stringField(fn, "name")
	arguments := stringField(fn, "arguments")
	id := stringField(call, "id")

	lookupKey := ""
	switch {
	case id != "":
		lookupKey = id
	case name != "":
		lookupKey = "name:" + name
	}

	idx := -1
	if raw, ok := call["index"].(float64); ok && raw == float64(int(raw)) {
		idx = int(raw)
	}
	if idx < 0 && lookupKey != "" {
		if cached, ok := s.anonymousIndexes[lookupKey]; ok {
			idx = cached
		}
	}
	if idx < 0 && len(assignedThisDelta) == 0 && len(s.toolCalls) == 1 {
		for only := range s.toolCalls {
			idx = only
		}
	}
	if idx < 0 {
		idx = s.nextToolCallIndex
		for {
			if _, taken := s.toolCalls[idx]; !taken {
				break
			}
			idx++
		}
	}
	if idx >= s.nextToolCallIndex {
		s.nextToolCallIndex = idx + 1
	}
	if lookupKey != "" {
		s.anonymousIndexes[lookupKey] = idx
	}

	buffer := s.toolCalls[idx]
	isNewCall := buffer == nil
	if isNewCall {
		buffer = &toolCallBuffer{
			ID:   "call_" + s.ChatID + "_" + strconv.Itoa(idx),
			Type: "function",
		}
		s.toolCalls[idx] = buffer
	}
	if id != "" {
		buffer.ID = id
	}
	if typ := stringField(call, "type"); typ != "" {
		buffer.Type = typ
	}
	if name != "" {
		buffer.Name += name
	}
	if arguments != "" {
		buffer.Arguments += arguments
	}

	out := map[string]any{
		"index": idx,
		"type":  buffer.Type,
		"function": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	}
	// The id goes out only on the fragment that opens the call (a newly
	// created buffer or an explicit upstream id), mirroring OpenAI streaming.
	if isNewCall || id != "" {
		out["id"] = buffer.ID
	}
	return idx, out
}

// noteError records the better of the current and previous error candidates.
func (s *chatStreamState) noteError(raw any, status int) {
	if raw == nil {
		return
	}
	if object, ok := raw.(map[string]any); ok {
		s.sawError = rememberUpstreamError(s.sawError, &upstreamError{status: status, body: object})
		return
	}
	if text, ok := raw.(string); ok && text != "" {
		s.sawError = rememberUpstreamError(s.sawError, &upstreamError{
			status: status,
			body:   map[string]any{"message": text},
		})
	}
}

// handleRawLine parses one upstream line and folds it into the outbound
// writer. It reports whether the frame was a [DONE] marker.
func (s *chatStreamState) handleRawLine(w io.Writer, line string) (bool, error) {
	if !strings.HasPrefix(line, "data:") {
		return false, nil
	}
	raw := strings.TrimSpace(line[len("data:"):])
	if raw == "" {
		return false, nil
	}
	if raw == "[DONE]" {
		s.sawDone = true
		return true, nil
	}
	var envelope upstreamEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return false, nil // malformed frame: ignored like the worker
	}
	decoded := decodeUpstreamBody(envelope)
	if decoded.done {
		s.sawDone = true
		return true, nil
	}
	if decoded.err != nil || len(decoded.raw) == 0 {
		return false, nil
	}
	var body map[string]any
	if err := json.Unmarshal(decoded.raw, &body); err != nil {
		return false, nil
	}
	if err := s.handleBody(w, body, envelope.StatusCodeValue); err != nil {
		return false, err
	}
	return false, nil
}

// sortedToolCalls renders the accumulated tool calls by index.
func (s *chatStreamState) sortedToolCalls() []map[string]any {
	indexes := make([]int, 0, len(s.toolCalls))
	for idx := range s.toolCalls {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	out := make([]map[string]any, 0, len(indexes))
	for _, idx := range indexes {
		buffer := s.toolCalls[idx]
		if buffer.Name == "" {
			continue
		}
		out = append(out, map[string]any{
			"id":   buffer.ID,
			"type": buffer.Type,
			"function": map[string]any{
				"name":      buffer.Name,
				"arguments": buffer.Arguments,
			},
		})
	}
	return out
}

// peekKind classifies the outcome of the upstream head probe.
type peekKind int

const (
	peekOK peekKind = iota
	peekAuth
	peekFatal
)

// peekResult carries the probe outcome: OK hands back a response preloaded
// with the drained bytes; Auth asks the caller to refresh and retry once;
// Fatal carries the pre-stream providers error.
type peekResult struct {
	kind peekKind
	err  error
	resp *http.Response
}

// peekUpstreamStatus reads the stream head until the first decodable
// envelope decides the outcome: a choices body means OK; a 401 error envelope
// means refresh + one retry; anything else becomes a providers.Error before
// any byte reaches the client. Only the consumed prefix is replayed — the
// live body stays open so streaming continues unbuffered.
func (c *Client) peekUpstreamStatus(resp *http.Response, state *chatStreamState) peekResult {
	reader := bufio.NewReader(resp.Body)
	var consumed []byte
	for {
		line, readErr := reader.ReadString('\n')
		consumed = append(consumed, line...)
		if kind, err, decided := classifyPeekLine(state, line); decided {
			if kind != peekOK {
				_ = resp.Body.Close()
				return peekResult{kind: kind, err: err}
			}
			return peekResult{kind: kind, resp: preloadResponse(resp, consumed, reader)}
		}
		if readErr != nil {
			break
		}
	}
	if state.sawError != nil {
		_ = resp.Body.Close()
		if state.sawError.status == http.StatusUnauthorized {
			return peekResult{kind: peekAuth}
		}
		return peekResult{kind: peekFatal, err: stateError(state)}
	}
	return peekResult{kind: peekOK, resp: preloadResponse(resp, consumed, reader)}
}

// classifyPeekLine folds one upstream line into the peek decision. It reports
// whether the line settled the outcome (ok / auth-retry / fatal).
func classifyPeekLine(state *chatStreamState, line string) (peekKind, error, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return 0, nil, false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if raw == "" || raw == "[DONE]" {
		return 0, nil, false
	}
	var envelope upstreamEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return 0, nil, false
	}
	decoded := decodeUpstreamBody(envelope)
	if decoded.done || len(decoded.raw) == 0 || decoded.err != nil {
		return 0, nil, false
	}
	var body map[string]any
	if err := json.Unmarshal(decoded.raw, &body); err != nil {
		return 0, nil, false
	}
	if usageLooksUseful(body) {
		accumulateUsage(&state.usage, body, 0)
	}
	if rawError, ok := body["error"]; ok {
		if _, hasChoices := body["choices"]; hasChoices {
			return peekOK, nil, true
		}
		state.noteError(rawError, statusOf(envelope))
		return peekDecisionFromState(state)
	}
	if _, hasChoices := body["choices"]; hasChoices {
		return peekOK, nil, true
	}
	if stringField(body, "code") != "" || stringField(body, "msgCode") != "" {
		state.noteError(body, statusOf(envelope))
		return peekDecisionFromState(state)
	}
	return 0, nil, false
}

// peekDecisionFromState turns a recorded upstream error into the peek
// outcome: 401 means refresh + one retry, everything else is fatal.
func peekDecisionFromState(state *chatStreamState) (peekKind, error, bool) {
	if state.sawError != nil && state.sawError.status == http.StatusUnauthorized {
		return peekAuth, nil, true
	}
	return peekFatal, stateError(state), true
}

// stateError formats the recorded stream error as a providers error.
func stateError(state *chatStreamState) error {
	if state.sawError == nil {
		return errors.New("qoder chat upstream error")
	}
	return streamErrorToProviderError(*state.sawError)
}

func statusOf(envelope upstreamEnvelope) int {
	if envelope.StatusCodeValue != nil {
		return *envelope.StatusCodeValue
	}
	if envelope.StatusCodeCompat != nil {
		return *envelope.StatusCodeCompat
	}
	return 0
}

// preloadResponse wraps the consumed head and the peek reader so the piper
// resumes exactly where the peek stopped (buffered prefix first, then the
// reader that owns the live body). Closing the replay body closes the
// original so the piper owns exactly one Close.
func preloadResponse(resp *http.Response, consumed []byte, reader *bufio.Reader) *http.Response {
	cloned := *resp
	cloned.Body = &replayBody{
		Reader:   io.MultiReader(bytes.NewReader(consumed), reader),
		original: resp.Body,
	}
	return &cloned
}

// replayBody replays the peeked prefix and then the live remainder.
type replayBody struct {
	io.Reader
	original io.Closer
}

func (b *replayBody) Close() error { return b.original.Close() }

// ---------------------------------------------------------------------------
// error surfacing (worker/src/errors.mjs kind mapping, executor taxonomy)
// ---------------------------------------------------------------------------

var (
	quotaRE    = regexp.MustCompile(`insufficient_quota|exceeded your current quota|额度已用尽|额度用尽|购买加量包`)
	rateRE     = regexp.MustCompile(`too many requests|rate.?limit|response code=429|resource_exhausted|rate_limit_exceeded|account busy|in-flight`)
	promptRE   = regexp.MustCompile(`#?token-limit|oversized prompt|prompt too (large|long)|context length|local precheck rejected`)
	authTextRE = regexp.MustCompile(`null pointer|forbidden|duplicate request|401|403|unauthorized|auth|credential|refresh.?token|access.?token`)
	notReadyRE = regexp.MustCompile(`hot context not ready|auth manager not captured|not ready|loginwithdeviceflow unavailable|loginwithpat unavailable|worker may not be warm`)
	modelRE    = regexp.MustCompile(`model_not_available|model_catalog_unavailable|is not available for this qoder account`)
)

// streamErrorToProviderError maps the upstream nested error onto the provider
// error taxonomy the executor understands. Status keeps the envelope value
// when present (403 duplicate request stays 403/KindAuth — no retry).
func streamErrorToProviderError(err upstreamError) *providers.Error {
	body := err.body
	message := firstNonEmptyString(
		stringField(body, "message"),
		stringField(body, "localizedMessage"),
		stringField(body, "msg"),
	)
	code := stringField(body, "code")
	if code == "" {
		code = stringField(body, "msgCode")
	}
	typ := stringField(body, "type")
	status := err.status

	// Token-limit quota errors are client-fixable input problems.
	if promptRE.MatchString(strings.ToLower(message)) {
		return &providers.Error{
			Kind:    accounts.KindInvalidRequest,
			Status:  http.StatusBadRequest,
			Message: "Upstream model input token limit hit. Reduce system prompt / history / tools. Detail: " + message,
			Code:    "prompt_too_large",
			Type:    "invalid_request_error",
		}
	}
	if code == "insufficient_quota" || typ == "insufficient_quota" || quotaRE.MatchString(strings.ToLower(message)) {
		return &providers.Error{
			Kind:    accounts.KindQuota,
			Status:  http.StatusTooManyRequests,
			Message: "Upstream model quota exhausted. Detail: " + message,
			Code:    "insufficient_quota",
			Type:    "insufficient_quota",
		}
	}

	searchable := strings.ToLower(message + " " + code + " " + typ)
	kind := classifyUpstreamText(searchable, status)

	outStatus := status
	if outStatus < 400 {
		switch kind {
		case accounts.KindQuota, accounts.KindRateLimit:
			outStatus = http.StatusTooManyRequests
		case accounts.KindAuth:
			outStatus = http.StatusForbidden
		default:
			outStatus = http.StatusBadGateway
		}
	}
	if kind == accounts.KindRateLimit {
		outStatus = http.StatusTooManyRequests
	}
	return &providers.Error{
		Kind:    kind,
		Status:  outStatus,
		Message: firstNonEmptyString(message, code, "upstream_error"),
		Code:    firstNonEmptyString(code, "upstream_error"),
		Type:    firstNonEmptyString(typ, "api_error"),
	}
}

// classifyUpstreamText is the worker errors.mjs decision tree reduced to the
// kinds the executor taxonomy knows.
func classifyUpstreamText(searchable string, status int) string {
	switch {
	case promptRE.MatchString(searchable):
		return accounts.KindInvalidRequest
	case quotaRE.MatchString(searchable):
		return accounts.KindQuota
	case modelRE.MatchString(searchable):
		return accounts.KindModelNotAvailable
	case notReadyRE.MatchString(searchable):
		return accounts.KindNotReady
	case authTextRE.MatchString(searchable) && !quotaRE.MatchString(searchable) && !rateRE.MatchString(searchable):
		return accounts.KindAuth
	case rateRE.MatchString(searchable) || status == http.StatusTooManyRequests:
		return accounts.KindRateLimit
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return accounts.KindAuth
	default:
		return accounts.KindUnavailable
	}
}

// ---------------------------------------------------------------------------
// request assembly
// ---------------------------------------------------------------------------

// resolvedModelForChat maps the requested model through the native catalog.
// The request value wins when present; the fallback key mirrors the worker's
// hot-model default and "auto" behavior.
func resolvedModelForChat(model string) ResolvedModel {
	requested := strings.TrimSpace(model)
	if requested == "" {
		requested = "auto"
	}
	return ResolvedModel{
		Key:         requested,
		DisplayName: requested,
		Source:      "system",
	}
}

// buildNativeChatRequest assembles the signed upstream request for one chat
// attempt: freshly generated runtime fields, plain body, cosy UA.
func (c *Client) buildNativeChatRequest(ctx context.Context, endpoint nativeEndpoints, identity inferIdentity, req translate.ChatRequest) (*http.Request, providers.ResolvedChat, error) {
	resolved := resolvedModelForChat(req.Model)
	normalizedEffort := ""
	if len(req.ReasoningEffort) > 0 {
		var raw string
		if err := json.Unmarshal(req.ReasoningEffort, &raw); err == nil {
			normalizedEffort = strings.TrimSpace(raw)
		}
	}
	reasoningLevel := ""
	if normalizedEffort != "" {
		reasoningLevel = clampReasoningLevel(normalizedEffort, resolved.AllowedReasoningLevels)
	}
	body, err := buildPlainChatBody(req, resolved, newUUIDv4())
	if err != nil {
		return nil, providers.ResolvedChat{}, fmt.Errorf("build chat body: %w", err)
	}
	prepared, err := identity.prepareInferRequest(endpoint.base, string(body), resolved.Key, resolved.Source, nil, nil)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, prepared.URL, strings.NewReader(prepared.Body))
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	for key, values := range prepared.Header {
		for _, value := range values {
			httpReq.Header.Add(key, value)
		}
	}
	httpReq.Header.Set("User-Agent", cosyUserAgent)
	return httpReq, providers.ResolvedChat{ReasoningLevel: reasoningLevel}, nil
}

// refreshChatCredential rotates the stored credential and invalidates the
// chat cache. It returns the refreshed blob so the immediate retry can sign
// with it even when the store round-trips through a stale read.
func (c *Client) refreshChatCredential(ctx context.Context, httpClient *http.Client, accountID string, account accounts.Account, cred userBlob) (userBlob, error) {
	refreshed, err := c.refreshAccessToken(ctx, httpClient, account, cred)
	if err != nil {
		return userBlob{}, err
	}
	if err := c.persistRefreshedCredential(ctx, account, refreshed); err != nil {
		return userBlob{}, fmt.Errorf("refresh succeeded but save failed: %w", err)
	}
	c.identityCache().put(accountID, identityEntry{account: account, cred: refreshed})
	return refreshed, nil
}

// ---------------------------------------------------------------------------
// public chat entry points
// ---------------------------------------------------------------------------

// ChatNonStream collects the translated stream into one outcome. Usage falls
// back to estimates when the upstream carried none.
func (c *Client) ChatNonStream(ctx context.Context, accountID string, req translate.ChatRequest) (providers.ChatOutcome, error) {
	resp, resolved, err := c.openChatStream(ctx, accountID, req)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	defer resp.Body.Close()
	state := newChatStreamState(req.Model, estimatePromptTokens(req.Messages))
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if _, err := state.handleRawLine(io.Discard, line); err != nil {
			return providers.ChatOutcome{}, err
		}
	}
	if err := scanner.Err(); err != nil {
		return providers.ChatOutcome{}, fmt.Errorf("qoder chat stream read: %w", err)
	}
	return state.outcome(resolved)
}

// ChatStream returns the translated OpenAI SSE response for the gateway to
// relay. The upstream head is probed first so auth failures surface as
// provider errors before any byte reaches the client.
func (c *Client) ChatStream(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	resp, resolved, err := c.openChatStream(ctx, accountID, req)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	state := newChatStreamState(req.Model, estimatePromptTokens(req.Messages))
	reader, writer := io.Pipe()
	go func() {
		err := state.pipe(resp.Body, writer)
		_ = resp.Body.Close()
		_ = writer.CloseWithError(err)
	}()
	translated := &http.Response{
		Status:     resp.Status,
		StatusCode: resp.StatusCode,
		Proto:      resp.Proto,
		ProtoMajor: resp.ProtoMajor,
		ProtoMinor: resp.ProtoMinor,
		Header:     resp.Header.Clone(),
		Body:       reader,
		Request:    resp.Request,
	}
	translated.Header.Set("Content-Type", "text/event-stream")
	translated.Header.Del("Content-Length")
	return translated, resolved, nil
}

// openChatStream resolves the identity, builds the signed request, sends it,
// and handles the 401-refresh-retry ladder. The returned response carries the
// drained head buffered ahead of the live body.
func (c *Client) openChatStream(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	account, _, identity, err := c.chatCredentialFor(ctx, accountID)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	endpoint, ok := c.endpoint(account.ProviderRegion)
	if !ok {
		return nil, providers.ResolvedChat{}, fmt.Errorf("qoder region %q has no chat endpoint", account.ProviderRegion)
	}
	httpClient, err := c.nativeHTTP(ctx, account)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	if httpClient.Timeout > 0 {
		cloned := *httpClient
		cloned.Timeout = 0
		httpClient = &cloned
	}

	attempt, resolved, err := c.buildNativeChatRequest(ctx, endpoint, identity, req)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	resp, err := httpClient.Do(attempt)
	if err != nil {
		return nil, providers.ResolvedChat{}, TransportError{Err: err}
	}

	peek := c.peekUpstreamStatus(resp, newChatStreamState(req.Model, estimatePromptTokens(req.Messages)))
	switch peek.kind {
	case peekOK:
		return peek.resp, resolved, nil
	case peekAuth:
		_, cred, credErr := c.resolvedCredential(ctx, accountID)
		if credErr != nil {
			return nil, providers.ResolvedChat{}, credErr
		}
		refreshed, refreshErr := c.refreshChatCredential(ctx, httpClient, accountID, account, cred)
		if refreshErr != nil {
			return nil, providers.ResolvedChat{}, authRefreshFailure(refreshErr)
		}
		retryIdentity, err := buildChatIdentity(account, refreshed)
		if err != nil {
			return nil, providers.ResolvedChat{}, err
		}
		retryReq, retryResolved, err := c.buildNativeChatRequest(ctx, endpoint, retryIdentity, req)
		if err != nil {
			return nil, providers.ResolvedChat{}, err
		}
		retryResp, err := httpClient.Do(retryReq)
		if err != nil {
			return nil, providers.ResolvedChat{}, TransportError{Err: err}
		}
		return retryResp, retryResolved, nil
	default:
		return nil, providers.ResolvedChat{}, peek.err
	}
}

// authRefreshFailure surfaces a failed token refresh as an auth-class
// provider error: the executor must cool the account down instead of
// retrying the chat against the same dead credential.
func authRefreshFailure(err error) error {
	message := err.Error()
	return &providers.Error{
		Kind:    accounts.KindAuth,
		Status:  http.StatusUnauthorized,
		Message: message,
		Code:    "upstream_refresh_failed",
		Type:    "api_error",
	}
}

// pipe drains the upstream SSE into the outbound writer, then appends the
// finish chunk, the usage chunk, and the terminal [DONE].
func (s *chatStreamState) pipe(body io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		if _, err := s.handleRawLine(w, scanner.Text()); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if s.sawError != nil {
		return streamErrorToProviderError(*s.sawError)
	}
	if !s.sawDone && s.finishReason == "" {
		return errors.New("upstream_stream_incomplete: upstream stream ended before [DONE]")
	}
	merged := s.finalizeUsage()
	if err := s.finishChunk(w); err != nil {
		return err
	}
	final := map[string]any{
		"id":      s.ChatID,
		"object":  "chat.completion.chunk",
		"created": s.Created,
		"model":   s.Model,
		"choices": []any{},
		"usage":   merged.usageMap(),
	}
	payload, err := json.Marshal(final)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: "+string(payload)+"\n\n"); err != nil {
		return err
	}
	_, err = io.WriteString(w, "data: [DONE]\n\n")
	return err
}

// outcome renders the accumulated stream state as a non-stream chat outcome.
func (s *chatStreamState) outcome(resolved providers.ResolvedChat) (providers.ChatOutcome, error) {
	if s.sawError != nil {
		return providers.ChatOutcome{}, streamErrorToProviderError(*s.sawError)
	}
	content := s.content.String()
	reasoning := s.reasoning.String()
	toolCalls := s.sortedToolCalls()
	var toolRaw json.RawMessage
	if len(toolCalls) > 0 {
		raw, err := json.Marshal(toolCalls)
		if err != nil {
			return providers.ChatOutcome{}, err
		}
		toolRaw = raw
	}
	finish := s.finishReason
	if finish == "" {
		if len(toolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	merged := s.finalizeUsage()
	outcome := providers.ChatOutcome{
		Model:            s.Model,
		Content:          content,
		Reasoning:        reasoning,
		ToolCalls:        toolRaw,
		FinishReason:     finish,
		PromptTokens:     int(*merged.PromptTokens),
		CompletionTokens: int(*merged.CompletionTokens),
		UsageSource:      merged.Source,
		ReasoningLevel:   resolved.ReasoningLevel,
	}
	if merged.CacheReadTokens != nil {
		value := int(*merged.CacheReadTokens)
		outcome.CacheReadTokens = &value
	}
	if merged.CacheWriteTokens != nil {
		value := int(*merged.CacheWriteTokens)
		outcome.CacheWriteTokens = &value
	}
	if merged.Credits != nil {
		value := *merged.Credits
		outcome.Credits = &value
	}
	return outcome, nil
}
