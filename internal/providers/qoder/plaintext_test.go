package qoder

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/translate"
)

func plainDecode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("body json: %v", err)
	}
	return out
}

func plainMessages(t *testing.T, body map[string]any) []any {
	t.Helper()
	messages, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("messages missing: %v", body["messages"])
	}
	return messages
}

func plainModelConfig(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	config, ok := body["model_config"].(map[string]any)
	if !ok {
		t.Fatalf("model_config missing: %v", body["model_config"])
	}
	return config
}

func plainParameters(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	parameters, ok := body["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters missing: %v", body["parameters"])
	}
	return parameters
}

// TestPlainChatBodyMultiTurnText: plain multi-turn text passes through with
// envelope defaults; business.name mirrors the first non-system message.
func TestPlainChatBodyMultiTurnText(t *testing.T) {
	req := translate.ChatRequest{
		Model: "qoder-agent",
		Messages: []translate.ChatMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi there"},
			{Role: "user", Content: "继续"},
		},
	}
	resolved := ResolvedModel{Key: "k2-pro", DisplayName: "K2 Pro", Source: "system", MaxInputTokens: 200000}
	body, err := buildPlainChatBody(req, resolved, "session-123")
	if err != nil {
		t.Fatal(err)
	}
	decoded := plainDecode(t, body)
	if decoded["session_id"] != "session-123" {
		t.Fatalf("session_id: %v", decoded["session_id"])
	}
	if decoded["chat_record_id"] != decoded["request_id"] {
		t.Fatal("chat_record_id must equal request_id")
	}
	for _, key := range []string{"request_id", "request_set_id"} {
		value, _ := decoded[key].(string)
		if len(value) != 36 || strings.Count(value, "-") != 4 {
			t.Fatalf("%s not UUID: %v", key, decoded[key])
		}
	}
	if decoded["stream"] != true || decoded["chat_task"] != "FREE_INPUT" || decoded["agent_id"] != "agent_common" {
		t.Fatalf("envelope fields: stream=%v chat_task=%v agent_id=%v", decoded["stream"], decoded["chat_task"], decoded["agent_id"])
	}
	if decoded["system"] != "" {
		t.Fatalf("system must be empty string: %v", decoded["system"])
	}
	messages := plainMessages(t, decoded)
	if len(messages) != 3 {
		t.Fatalf("message count: %d", len(messages))
	}
	first := messages[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "hello" {
		t.Fatalf("first message: %v", first)
	}
	config := plainModelConfig(t, decoded)
	if config["key"] != "k2-pro" || config["display_name"] != "K2 Pro" || config["format"] != "openai" {
		t.Fatalf("model_config: %v", config)
	}
	if config["max_input_tokens"] != float64(200000) {
		t.Fatalf("max_input_tokens: %v", config["max_input_tokens"])
	}
	if config["source"] != "system" {
		t.Fatalf("source: %v", config["source"])
	}
	parameters := plainParameters(t, decoded)
	if parameters["max_tokens"] != float64(32000) {
		t.Fatalf("default max_tokens: %v", parameters["max_tokens"])
	}
	business := decoded["business"].(map[string]any)
	if business["product"] != "cli" || business["type"] != "agent" || business["stage"] != "start" {
		t.Fatalf("business: %v", business)
	}
	if business["name"] != "hello" {
		t.Fatalf("business.name: %v", business["name"])
	}
	if business["version"] != qoderCLIVersion {
		t.Fatalf("business.version: %v", business["version"])
	}
}

// TestPlainChatBodySystemAndTools: system messages collapse into the top-level
// system field plus one canonical leading message; OpenAI and bare tools both
// normalize to function shape with default parameters.
func TestPlainChatBodySystemAndTools(t *testing.T) {
	tools := json.RawMessage(`[{"type":"function","function":{"name":"get_weather","description":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}},{"name":"bare_tool","description":"bare"}]`)
	req := translate.ChatRequest{
		Model: "m",
		Messages: []translate.ChatMessage{
			{Role: "system", Content: "you are terse"},
			{Role: "user", Content: "weather?"},
		},
		Tools: tools,
	}
	body, err := buildPlainChatBody(req, ResolvedModel{Key: "k"}, "s")
	if err != nil {
		t.Fatal(err)
	}
	decoded := plainDecode(t, body)
	if decoded["system"] != "you are terse" {
		t.Fatalf("system: %v", decoded["system"])
	}
	messages := plainMessages(t, decoded)
	if len(messages) != 2 {
		t.Fatalf("messages: %v", messages)
	}
	if messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("canonical system first: %v", messages[0])
	}
	toolsOut, ok := decoded["tools"].([]any)
	if !ok || len(toolsOut) != 2 {
		t.Fatalf("tools: %v", decoded["tools"])
	}
	first := toolsOut[0].(map[string]any)
	if first["type"] != "function" {
		t.Fatal("tool type")
	}
	fn := first["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["description"] != "weather" {
		t.Fatalf("tool function: %v", fn)
	}
	bare := toolsOut[1].(map[string]any)["function"].(map[string]any)
	if bare["name"] != "bare_tool" {
		t.Fatalf("bare tool: %v", bare)
	}
	params, ok := bare["parameters"].(map[string]any)
	if !ok || params["type"] != "object" {
		t.Fatalf("bare tool default parameters: %v", bare["parameters"])
	}
}

// TestPlainChatBodyToolCallHistoryFiltering: tool calls for undeclared tools
// drop along with their orphan results; declared-call results survive and
// arguments stay string-typed.
func TestPlainChatBodyToolCallHistoryFiltering(t *testing.T) {
	tools := json.RawMessage(`[{"type":"function","function":{"name":"known_tool","parameters":{}}}]`)
	req := translate.ChatRequest{
		Model: "m",
		Messages: []translate.ChatMessage{
			{Role: "user", Content: "run"},
			{Role: "assistant", Content: "", ToolCalls: json.RawMessage(`[
				{"id":"call_1","type":"function","function":{"name":"known_tool","arguments":"{\"x\":1}"}},
				{"id":"call_2","type":"function","function":{"name":"unknown_tool","arguments":"{}"}}
			]`)},
			{Role: "tool", ToolCallID: "call_1", Content: `{"ok":true}`},
			{Role: "tool", ToolCallID: "call_2", Content: "orphan"},
		},
		Tools: tools,
	}
	body, err := buildPlainChatBody(req, ResolvedModel{Key: "k"}, "s")
	if err != nil {
		t.Fatal(err)
	}
	decoded := plainDecode(t, body)
	messages := plainMessages(t, decoded)
	if len(messages) != 3 {
		for i, m := range messages {
			t.Logf("message %d: %v", i, m)
		}
		t.Fatalf("message count %d, want 3 (user, assistant, tool result)", len(messages))
	}
	assistant := messages[1].(map[string]any)
	calls, ok := assistant["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("retained tool calls: %v", assistant["tool_calls"])
	}
	retained := calls[0].(map[string]any)
	if retained["id"] != "call_1" {
		t.Fatalf("retained id: %v", retained)
	}
	fn := retained["function"].(map[string]any)
	if fn["arguments"] != `{"x":1}` {
		t.Fatalf("arguments must stay a string: %v", fn["arguments"])
	}
	result := messages[2].(map[string]any)
	if result["tool_call_id"] != "call_1" {
		t.Fatalf("result id: %v", result["tool_call_id"])
	}
	if result["content"] != `{"ok":true}` {
		t.Fatalf("result content: %v", result["content"])
	}
}

// TestPlainChatBodyImageMessage: OpenAI vision image_url parts survive
// normalization and flip model_config.is_vl even when the catalog row says
// non-VL.
func TestPlainChatBodyImageMessage(t *testing.T) {
	dataURL := "data:image/png;base64,aGVsbG8="
	req := translate.ChatRequest{
		Model: "m",
		Messages: []translate.ChatMessage{
			{Role: "user", Content: []any{
				map[string]any{"type": "text", "text": "what is this"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
			}},
		},
	}
	body, err := buildPlainChatBody(req, ResolvedModel{Key: "k", IsVL: false}, "s")
	if err != nil {
		t.Fatal(err)
	}
	decoded := plainDecode(t, body)
	messages := plainMessages(t, decoded)
	if len(messages) != 1 {
		t.Fatalf("message count: %d", len(messages))
	}
	parts, ok := messages[0].(map[string]any)["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content parts: %v", messages[0].(map[string]any)["content"])
	}
	if parts[0].(map[string]any)["type"] != "text" {
		t.Fatalf("text part: %v", parts[0])
	}
	image := parts[1].(map[string]any)
	if image["type"] != "image_url" {
		t.Fatalf("image part: %v", image)
	}
	url := image["image_url"].(map[string]any)["url"]
	if url != dataURL {
		t.Fatalf("image url: %v", url)
	}
	if plainModelConfig(t, decoded)["is_vl"] != true {
		t.Fatal("is_vl must flip on for image input")
	}
}

// TestPlainChatBodyReasoningEffortClamp: catalog allow list wins over the raw
// client effort; empty allow list passes the normalized level through.
func TestPlainChatBodyReasoningEffortClamp(t *testing.T) {
	base := translate.ChatRequest{Model: "m", Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}}}

	req := base
	req.ReasoningEffort = json.RawMessage(`"x-high"`)
	body, err := buildPlainChatBody(req, ResolvedModel{Key: "k", AllowedReasoningLevels: []string{"low", "high"}}, "s")
	if err != nil {
		t.Fatal(err)
	}
	parameters := plainParameters(t, plainDecode(t, body))
	// Shared clamp semantics (providers.ResolveReasoningLevel): requested level
	// not in the allow list and no catalog default → first allowed level.
	if parameters["reasoning_effort"] != "low" {
		t.Fatalf("clamped effort: %v", parameters["reasoning_effort"])
	}
	if parameters["enable_thinking"] != true {
		t.Fatalf("enable_thinking: %v", parameters["enable_thinking"])
	}

	req = base
	req.ReasoningEffort = json.RawMessage(`"none"`)
	body, err = buildPlainChatBody(req, ResolvedModel{Key: "k", AllowedReasoningLevels: []string{"none", "low", "high"}}, "s")
	if err != nil {
		t.Fatal(err)
	}
	decoded := plainDecode(t, body)
	parameters = plainParameters(t, decoded)
	// "none" in the allow list passes through unclamped and turns thinking off.
	if parameters["reasoning_effort"] != "none" {
		t.Fatalf("none effort: %v", parameters["reasoning_effort"])
	}
	if parameters["enable_thinking"] != false {
		t.Fatalf("enable_thinking for none: %v", parameters["enable_thinking"])
	}
	if plainModelConfig(t, decoded)["is_reasoning"] != false {
		t.Fatal("is_reasoning must follow effective thinking")
	}
}

// TestPlainChatBodyMaxTokensDefaults: max_completion_tokens wins over
// max_tokens; absent/invalid values fall back to 32000.
func TestPlainChatBodyMaxTokensDefaults(t *testing.T) {
	base := translate.ChatRequest{Model: "m", Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}}}

	body, err := buildPlainChatBody(base, ResolvedModel{Key: "k"}, "s")
	if err != nil {
		t.Fatal(err)
	}
	if plainParameters(t, plainDecode(t, body))["max_tokens"] != float64(32000) {
		t.Fatal("fallback max_tokens")
	}

	completion := base
	completion.MaxCompletionTokens = json.RawMessage(`2048`)
	completion.MaxTokens = json.RawMessage(`999`)
	body, err = buildPlainChatBody(completion, ResolvedModel{Key: "k"}, "s")
	if err != nil {
		t.Fatal(err)
	}
	if plainParameters(t, plainDecode(t, body))["max_tokens"] != float64(2048) {
		t.Fatal("max_completion_tokens must win")
	}

	sampled := base
	sampled.Temperature = json.RawMessage(`0.7`)
	sampled.TopP = json.RawMessage(`0.9`)
	sampled.Stop = json.RawMessage(`["END"]`)
	body, err = buildPlainChatBody(sampled, ResolvedModel{Key: "k"}, "s")
	if err != nil {
		t.Fatal(err)
	}
	parameters := plainParameters(t, plainDecode(t, body))
	if parameters["temperature"] != 0.7 || parameters["top_p"] != 0.9 {
		t.Fatalf("sampling params: %v", parameters)
	}
	stop, ok := parameters["stop"].([]any)
	if !ok || len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("stop: %v", parameters["stop"])
	}
}

// TestPlainChatBodyEmptyConversation: worker emits a ping user message when no
// non-system content survives normalization.
func TestPlainChatBodyEmptyConversation(t *testing.T) {
	req := translate.ChatRequest{Model: "m", Messages: []translate.ChatMessage{{Role: "system", Content: "only system"}}}
	body, err := buildPlainChatBody(req, ResolvedModel{Key: "k"}, "s")
	if err != nil {
		t.Fatal(err)
	}
	decoded := plainDecode(t, body)
	if decoded["system"] != "only system" {
		t.Fatalf("system: %v", decoded["system"])
	}
	messages := plainMessages(t, decoded)
	// Worker semantics: system stays, and the ping fallback joins it when no
	// non-system content survives → [system, ping].
	if len(messages) != 2 {
		t.Fatalf("messages: %v", messages)
	}
	if messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("system first: %v", messages[0])
	}
	fallback := messages[1].(map[string]any)
	if fallback["role"] != "user" || fallback["content"] != "ping" {
		t.Fatalf("ping fallback: %v", fallback)
	}
}

// TestEstimateTokensAndPrompt: CJK-heavy text estimates ~1 token per char,
// ASCII ~1 per 4, with the per-message overhead of 4.
func TestEstimateTokensAndPrompt(t *testing.T) {
	if got := estimateTokens("four"); got != 1 {
		t.Fatalf("ascii estimate: %d", got)
	}
	if got := estimateTokens("四个汉字"); got != 4 {
		t.Fatalf("cjk estimate: %d", got)
	}
	if got := estimateTokens(""); got != 0 {
		t.Fatalf("empty estimate: %d", got)
	}
	messages := []translate.ChatMessage{
		{Role: "user", Content: "abcd"},
		{Role: "user", Content: "字"},
	}
	if got := estimatePromptTokens(messages); got != (1+4)+(1+4) {
		t.Fatalf("prompt estimate: %d", got)
	}
}

// TestSummarizeAndDiagnoseToolHistory: summary/diagnostic helpers keep their
// worker shape (counts, batch sizes, control-char and binary flags).
func TestSummarizeAndDiagnoseToolHistory(t *testing.T) {
	normalized := []map[string]any{
		{"role": "assistant", "content": "", "tool_calls": []map[string]any{
			{"id": "call_1", "type": "function", "function": map[string]any{"name": "known_tool", "arguments": `{"x":1}`}},
			{"id": "call_2", "type": "function", "function": map[string]any{"name": "known_tool", "arguments": `{}`}},
		}},
		{"role": "tool", "content": `{"ok":true}`, "tool_call_id": "call_1"},
		{"role": "tool", "content": "bin\x00ary", "tool_call_id": "call_2"},
	}
	summary := summarizeNormalizedToolHistory(normalized)
	if len(summary) != 3 {
		t.Fatalf("summary length: %d", len(summary))
	}
	calls, ok := summary[0]["toolCalls"].([]map[string]any)
	if !ok || len(calls) != 2 {
		t.Fatalf("summary toolCalls: %v", summary[0]["toolCalls"])
	}
	if calls[0]["argumentLength"] != len(`{"x":1}`) {
		t.Fatalf("argumentLength: %v", calls[0]["argumentLength"])
	}

	messages := []translate.ChatMessage{
		{Role: "assistant", ToolCalls: json.RawMessage(`[
			{"id":"call_1","type":"function","function":{"name":"known_tool","arguments":"{}"}},
			{"id":"call_2","type":"function","function":{"name":"known_tool","arguments":"{}"}}
		]`)},
		{Role: "tool", ToolCallID: "call_1", Content: "{\"ok\":true}"},
		{Role: "tool", ToolCallID: "call_2", Content: "bin\x00ary"},
	}
	diagnostics := diagnosePlainToolResults(messages)
	if len(diagnostics) != 2 {
		t.Fatalf("diagnostics: %v", diagnostics)
	}
	if diagnostics[0]["parallelBatchSize"] != 2 {
		t.Fatalf("batch size: %v", diagnostics[0]["parallelBatchSize"])
	}
	if diagnostics[0]["jsonValid"] != true {
		t.Fatalf("jsonValid: %v", diagnostics[0]["jsonValid"])
	}
	if diagnostics[1]["looksBinary"] != true || diagnostics[1]["hasControlChars"] != true {
		t.Fatalf("binary flags: %v", diagnostics[1])
	}
}

// TestPlainChatBodyToolResultImageHoisting: image parts inside tool results
// are stripped to text placeholders and re-hoisted into a trailing user
// message (OpenAI image_url and data/mime image shapes).
func TestPlainChatBodyToolResultImageHoisting(t *testing.T) {
	tools := json.RawMessage(`[{"type":"function","function":{"name":"shot","parameters":{}}}]`)
	req := translate.ChatRequest{
		Model: "m",
		Messages: []translate.ChatMessage{
			{Role: "user", Content: "screenshot"},
			{Role: "assistant", ToolCalls: json.RawMessage(`[{"id":"call_1","type":"function","function":{"name":"shot","arguments":"{}"}}]`)},
			{Role: "tool", ToolCallID: "call_1", Content: []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,QUJD"}},
			}},
			{Role: "user", Content: "describe it"},
		},
		Tools: tools,
	}
	body, err := buildPlainChatBody(req, ResolvedModel{Key: "k"}, "s")
	if err != nil {
		t.Fatal(err)
	}
	decoded := plainDecode(t, body)
	messages := plainMessages(t, decoded)
	// user, assistant(tool_calls), tool(text placeholder), user(images), user(describe)
	if len(messages) != 5 {
		for i, m := range messages {
			t.Logf("message %d: %v", i, m)
		}
		t.Fatalf("message count %d, want 5", len(messages))
	}
	toolMsg := messages[2].(map[string]any)
	if toolMsg["content"] != "[Image: unknown]" {
		t.Fatalf("tool content placeholder: %v", toolMsg["content"])
	}
	hoisted := messages[3].(map[string]any)
	if hoisted["role"] != "user" {
		t.Fatalf("hoisted role: %v", hoisted)
	}
	parts, ok := hoisted["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("hoisted content: %v", hoisted["content"])
	}
	image := parts[0].(map[string]any)
	if image["type"] != "image_url" {
		t.Fatalf("hoisted part: %v", image)
	}
	if url := image["image_url"].(map[string]any)["url"]; url != "data:image/png;base64,QUJD" {
		t.Fatalf("hoisted url: %v", url)
	}
	// Anthropic-style image part with raw data + mime type becomes a data:URL
	// (fresh request so message indices stay stable).
	req2 := translate.ChatRequest{
		Model: "m",
		Messages: []translate.ChatMessage{
			{Role: "user", Content: "screenshot"},
			{Role: "assistant", ToolCalls: json.RawMessage(`[{"id":"call_1","type":"function","function":{"name":"shot","arguments":"{}"}}]`)},
			{Role: "tool", ToolCallID: "call_1", Content: []any{
				map[string]any{"type": "image", "data": "QUJD", "mimeType": "image/jpeg"},
			}},
		},
		Tools: tools,
	}
	body, err = buildPlainChatBody(req2, ResolvedModel{Key: "k"}, "s")
	if err != nil {
		t.Fatal(err)
	}
	messages = plainMessages(t, plainDecode(t, body))
	// [user, assistant(tool_calls), tool placeholder, hoisted user]
	if len(messages) != 4 {
		t.Fatalf("req2 message count %d", len(messages))
	}
	if messages[2].(map[string]any)["content"] != "[Image: image/jpeg]" {
		t.Fatalf("req2 placeholder: %v", messages[2].(map[string]any)["content"])
	}
	hoisted = messages[3].(map[string]any)
	part := hoisted["content"].([]any)[0].(map[string]any)
	if url := part["image_url"].(map[string]any)["url"]; url != "data:image/jpeg;base64,QUJD" {
		t.Fatalf("data url: %v", url)
	}
}
