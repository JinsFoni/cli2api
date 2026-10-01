package qoder

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// qoderCLIVersion mirrors worker business.version (QODER_CLI_VERSION || "1.1.32").
const qoderCLIVersion = "1.1.32"

// ResolvedModel is the catalog-routed model the caller resolved for this chat.
// Task 7 wires it from the runtime catalog; tests construct it directly.
type ResolvedModel struct {
	Key, DisplayName, Source string
	MaxInputTokens           int
	IsReasoning, IsVL        bool
	AllowedReasoningLevels   []string // catalog-driven allow list
}

// newUUIDv4 returns a random RFC 4122 v4 UUID (JS crypto.randomUUID
// equivalent). rand.Read failures are practically impossible on supported
// platforms; on the defensive path a fixed v4-shaped UUID is returned rather
// than failing the chat body.
func newUUIDv4() string {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return formatCosyUUID(u)
}

// plainContentToString is the port of worker contentToString: string
// passthrough, array parts joined with "\n" (text/content keys), nil → "",
// else fmt.Sprint.
func plainContentToString(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			switch part := item.(type) {
			case string:
				if part != "" {
					parts = append(parts, part)
				}
			case map[string]any:
				text, _ := part["text"].(string)
				if text == "" {
					text, _ = part["content"].(string)
				}
				if text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// estimateTokens is the port of worker estimateTokens: CJK chars ~1 token,
// other chars ~1 token/4, minimum 1 for non-empty input.
func estimateTokens(text string) int {
	cjk, other := 0, 0
	for _, ch := range text {
		if isCJKOrHangulRune(ch) {
			cjk++
		} else {
			other++
		}
	}
	if cjk+other == 0 {
		return 0
	}
	value := cjk + other/4
	if value < 1 {
		value = 1
	}
	return value
}

func isCJKOrHangulRune(ch rune) bool {
	switch {
	case ch >= 0x4e00 && ch <= 0x9fff, ch >= 0x3400 && ch <= 0x4dbf,
		ch >= 0xf900 && ch <= 0xfaff, ch >= 0x3040 && ch <= 0x30ff,
		ch >= 0xac00 && ch <= 0xd7af:
		return true
	}
	return false
}

// estimatePromptTokens is the port of worker estimatePromptTokens: per-message
// overhead of 4 tokens plus content estimate.
func estimatePromptTokens(messages []translate.ChatMessage) int {
	sum := 0
	for _, m := range messages {
		sum += estimateTokens(plainContentToString(m.Content)) + 4
	}
	return sum
}

// wantsReasoningChat is the port of worker wantsReasoning over
// translate.ChatRequest.
func wantsReasoningChat(req translate.ChatRequest) bool {
	if req.IsReasoning != nil && *req.IsReasoning {
		return true
	}
	if req.EnableThinking != nil && *req.EnableThinking {
		return true
	}
	if req.EnableReasoning != nil && *req.EnableReasoning {
		return true
	}
	if len(req.Thinking) > 0 {
		var b bool
		if err := json.Unmarshal(req.Thinking, &b); err == nil {
			return b
		}
		var s string
		if err := json.Unmarshal(req.Thinking, &s); err == nil {
			t := strings.ToLower(s)
			return t != "" && t != "disabled" && t != "none" && t != "off" && t != "false"
		}
		var obj map[string]any
		if err := json.Unmarshal(req.Thinking, &obj); err == nil {
			typ := strings.ToLower(plainFirstString(obj["type"], obj["mode"]))
			if typ != "" && typ != "disabled" && typ != "none" && typ != "off" {
				return true
			}
			if enabled, ok := obj["enabled"].(bool); ok && enabled {
				return true
			}
		}
	}
	if len(req.ReasoningEffort) > 0 {
		var s string
		if err := json.Unmarshal(req.ReasoningEffort, &s); err == nil {
			e := strings.ToLower(s)
			return e != "" && e != "none" && e != "off" && e != "disabled"
		}
	}
	return false
}

func plainFirstString(values ...any) string {
	for _, value := range values {
		if s, ok := value.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// incomingToolCall mirrors the OpenAI tool_calls entries on the wire.
type incomingToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Function *struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

func parseToolCalls(raw json.RawMessage) []incomingToolCall {
	if len(raw) == 0 {
		return nil
	}
	var calls []incomingToolCall
	if err := json.Unmarshal(raw, &calls); err != nil {
		return nil
	}
	return calls
}

// normalizePlainTools is the port of worker normalizeTools: accept OpenAI
// function-tool shape or bare function definitions, drop the rest.
func normalizePlainTools(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 {
		return []map[string]any{}
	}
	var items []any
	if err := json.Unmarshal(raw, &items); err != nil {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		var name, description string
		var parameters any
		if fn, ok := tool["function"].(map[string]any); ok && tool["type"] == "function" {
			name, _ = fn["name"].(string)
			description, _ = fn["description"].(string)
			parameters = fn["parameters"]
		} else if bareName, _ := tool["name"].(string); bareName != "" {
			_, hasParams := tool["parameters"]
			_, hasDescription := tool["description"]
			if !hasParams && !hasDescription {
				continue
			}
			name = bareName
			description, _ = tool["description"].(string)
			parameters = tool["parameters"]
		} else {
			continue
		}
		if name == "" {
			continue
		}
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": description,
				"parameters":  parameters,
			},
		})
	}
	return out
}

// filterUnknownToolHistory is the port of worker filterUnknownToolHistory:
// drop assistant tool_calls (and their tool results) whose function name is
// not in the declared tool set. No declared tools → history passes through.
func filterUnknownToolHistory(messages []translate.ChatMessage, toolsRaw json.RawMessage) ([]translate.ChatMessage, []string, []string) {
	definedNames := map[string]bool{}
	for _, tool := range normalizePlainTools(toolsRaw) {
		fn := tool["function"].(map[string]any)
		definedNames[fn["name"].(string)] = true
	}
	if len(definedNames) == 0 {
		return messages, nil, nil
	}
	droppedToolCallIDs := map[string]bool{}
	droppedToolNames := map[string]bool{}
	filtered := make([]translate.ChatMessage, 0, len(messages))
	for _, message := range messages {
		if message.Role == "assistant" && len(message.ToolCalls) > 0 {
			calls := parseToolCalls(message.ToolCalls)
			retained := make([]incomingToolCall, 0, len(calls))
			for _, call := range calls {
				name := strings.TrimSpace(callFunctionName(call))
				if name != "" && definedNames[name] {
					retained = append(retained, call)
					continue
				}
				if id := strings.TrimSpace(call.ID); id != "" {
					droppedToolCallIDs[id] = true
				}
				if name != "" {
					droppedToolNames[name] = true
				}
			}
			if len(retained) == 0 {
				if plainContentToString(message.Content) != "" {
					stripped := message
					stripped.ToolCalls = nil
					filtered = append(filtered, stripped)
				}
				continue
			}
			kept := message
			kept.ToolCalls, _ = json.Marshal(retained)
			filtered = append(filtered, kept)
			continue
		}
		if message.Role == "tool" && droppedToolCallIDs[strings.TrimSpace(message.ToolCallID)] {
			continue
		}
		filtered = append(filtered, message)
	}
	return filtered, mapKeys(droppedToolCallIDs), mapKeys(droppedToolNames)
}

func callFunctionName(call incomingToolCall) string {
	if call.Function != nil && call.Function.Name != "" {
		return call.Function.Name
	}
	return call.Name
}

func mapKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// normalizePlainToolResultContent is the port of worker
// normalizeToolResultContent.
func normalizePlainToolResultContent(content any, isError bool) string {
	var normalized string
	switch v := content.(type) {
	case string:
		normalized = v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			switch part := item.(type) {
			case string:
				if part != "" {
					parts = append(parts, part)
				}
			case map[string]any:
				kind, _ := part["type"].(string)
				switch kind {
				case "text":
					if text, ok := part["text"].(string); ok {
						parts = append(parts, text)
					} else {
						parts = append(parts, plainJSON(part))
					}
				case "image", "image_url":
					mime := plainFirstString(part["mime_type"], part["mimeType"])
					if mime == "" {
						if image, ok := part["image_url"].(map[string]any); ok {
							if detail, ok := image["detail"].(string); ok {
								mime = detail
							}
						}
					}
					if mime == "" {
						mime = "unknown"
					}
					parts = append(parts, "[Image: "+mime+"]")
				case "resource_link":
					title := plainFirstString(part["title"], part["name"], part["uri"])
					if title == "" {
						title = "resource"
					}
					parts = append(parts, "[Link to "+title+"]")
				case "resource":
					if resource, ok := part["resource"].(map[string]any); ok {
						if text, ok := resource["text"].(string); ok {
							parts = append(parts, text)
							continue
						}
						mime := plainFirstString(resource["mime_type"], resource["mimeType"])
						if mime == "" {
							mime = "unknown"
						}
						parts = append(parts, "[Embedded Resource: "+mime+"]")
					} else {
						parts = append(parts, plainJSON(part))
					}
				default:
					parts = append(parts, plainJSON(part))
				}
			}
		}
		normalized = strings.Join(parts, "\n")
	case nil:
		normalized = "(no content)"
	default:
		normalized = plainJSON(content)
	}
	if normalized == "" {
		normalized = "(no content)"
	}
	if isError && !plainStartsWithError(normalized) {
		normalized = "Error: " + normalized
	}
	return normalized
}

func plainJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

// plainStartsWithError mirrors JS /^Error:\s/u: "Error:" followed by
// whitespace (or end of input).
func plainStartsWithError(value string) bool {
	if !strings.HasPrefix(value, "Error:") {
		return false
	}
	rest := value[len("Error:"):]
	if rest == "" {
		return true
	}
	r, size := utf8.DecodeRuneInString(rest)
	return size > 0 && unicode.IsSpace(r)
}

// normalizePlainToolCall is the port of worker normalizeToolCall: stable IDs,
// arguments coerced to string.
func normalizePlainToolCall(call incomingToolCall, messageIndex, callIndex int, usedIDs map[string]bool, sourceIDs map[string]string) map[string]any {
	sourceID := strings.TrimSpace(call.ID)
	id := sourceID
	if id == "" || usedIDs[id] {
		id = fmt.Sprintf("qoder_call_%d_%d", messageIndex, callIndex)
		for usedIDs[id] {
			id += "_retry"
		}
	}
	usedIDs[id] = true
	if sourceID != "" {
		if _, seen := sourceIDs[sourceID]; !seen {
			sourceIDs[sourceID] = id
		}
	}
	name := callFunctionName(call)
	arguments := "{}"
	if call.Function != nil && len(call.Function.Arguments) > 0 {
		raw := call.Function.Arguments
		trimmed := strings.TrimSpace(string(raw))
		if strings.HasPrefix(trimmed, "\"") {
			var s string
			if err := json.Unmarshal(raw, &s); err == nil {
				arguments = s
			} else {
				arguments = trimmed
			}
		} else {
			arguments = trimmed
		}
	}
	return map[string]any{
		"id":   id,
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	}
}

// normalizePlainContentForUpstream is the port of worker
// normalizeContentForUpstream: array content → typed parts; non-array
// passthrough (nil → "").
func normalizePlainContentForUpstream(content any) any {
	parts, ok := content.([]any)
	if !ok {
		if content == nil {
			return ""
		}
		return content
	}
	out := make([]any, 0, len(parts))
	for _, item := range parts {
		switch part := item.(type) {
		case string:
			out = append(out, map[string]any{"type": "text", "text": part})
		case map[string]any:
			kind, _ := part["type"].(string)
			switch kind {
			case "text", "input_text", "output_text":
				text := plainFirstString(part["text"], part["content"])
				out = append(out, map[string]any{"type": "text", "text": text})
			case "image_url":
				var image map[string]any
				switch value := part["image_url"].(type) {
				case string:
					image = map[string]any{"url": value}
				case map[string]any:
					image = value
				}
				if url, _ := image["url"].(string); url != "" {
					out = append(out, map[string]any{"type": "image_url", "image_url": image})
				}
			}
		}
	}
	return out
}

type pendingToolResult struct {
	message    map[string]any
	toolCallID string
	images     []map[string]any
}

// normalizePlainMessages is the port of worker normalizeMessagesForUpstream:
// tool results are buffered, ordered to their call batch, orphan results
// dropped, tool-result images hoisted into a trailing user message.
func normalizePlainMessages(messages []translate.ChatMessage) []map[string]any {
	usedIDs := map[string]bool{}
	sourceIDs := map[string]string{}
	callsByID := map[string]bool{}
	var normalized []map[string]any
	var currentBatch []string
	consumedBatchIDs := map[string]bool{}
	var pending []pendingToolResult

	flush := func() {
		if len(pending) == 0 {
			return
		}
		order := map[string]int{}
		for index, id := range currentBatch {
			order[id] = index
		}
		sort.SliceStable(pending, func(left, right int) bool {
			leftOrder := len(currentBatch) + 1
			if value, ok := order[pending[left].toolCallID]; ok {
				leftOrder = value
			}
			rightOrder := len(currentBatch) + 1
			if value, ok := order[pending[right].toolCallID]; ok {
				rightOrder = value
			}
			return leftOrder < rightOrder
		})
		var images []map[string]any
		for _, result := range pending {
			belongs := false
			if len(currentBatch) > 0 {
				_, belongs = order[result.toolCallID]
			} else {
				belongs = callsByID[result.toolCallID]
			}
			if belongs {
				normalized = append(normalized, result.message)
				images = append(images, result.images...)
			}
		}
		if len(images) > 0 {
			content := make([]any, 0, len(images))
			for _, image := range images {
				content = append(content, image)
			}
			normalized = append(normalized, map[string]any{"role": "user", "content": content})
		}
		pending = nil
	}

	for index, message := range messages {
		switch message.Role {
		case "assistant":
			flush()
			consumedBatchIDs = map[string]bool{}
			content := normalizePlainContentForUpstream(message.Content)
			out := map[string]any{"role": message.Role, "content": content}
			if calls := parseToolCalls(message.ToolCalls); len(calls) > 0 {
				normalizedCalls := make([]map[string]any, 0, len(calls))
				for callIndex, call := range calls {
					normalizedCall := normalizePlainToolCall(call, index, callIndex, usedIDs, sourceIDs)
					callsByID[normalizedCall["id"].(string)] = true
					normalizedCalls = append(normalizedCalls, normalizedCall)
				}
				out["tool_calls"] = normalizedCalls
				currentBatch = currentBatch[:0]
				for _, call := range normalizedCalls {
					currentBatch = append(currentBatch, call["id"].(string))
				}
				if plainContentIsEmpty(content) {
					out["content"] = ""
				}
			} else {
				currentBatch = currentBatch[:0]
			}
			normalized = append(normalized, out)
		case "tool":
			requestedID := strings.TrimSpace(message.ToolCallID)
			toolCallID := requestedID
			if mapped, ok := sourceIDs[requestedID]; ok && mapped != "" {
				toolCallID = mapped
			}
			if toolCallID == "" {
				for _, id := range currentBatch {
					if !consumedBatchIDs[id] {
						toolCallID = id
						break
					}
				}
			}
			if toolCallID == "" || !callsByID[toolCallID] {
				continue
			}
			consumedBatchIDs[toolCallID] = true
			out := map[string]any{
				"role":         message.Role,
				"content":      normalizePlainToolResultContent(message.Content, false),
				"tool_call_id": toolCallID,
			}
			if message.Name != "" {
				out["name"] = message.Name
			}
			var images []map[string]any
			if parts, ok := message.Content.([]any); ok {
				for _, item := range parts {
					part, ok := item.(map[string]any)
					if !ok {
						continue
					}
					kind, _ := part["type"].(string)
					switch kind {
					case "image_url":
						if normalizedPart, ok := normalizePlainContentForUpstream([]any{part}).([]any); ok && len(normalizedPart) == 1 {
							if image, ok := normalizedPart[0].(map[string]any); ok {
								images = append(images, image)
							}
						}
					case "image":
						data, _ := part["data"].(string)
						mime := plainFirstString(part["mimeType"], part["mime_type"])
						if data != "" && mime != "" {
							images = append(images, map[string]any{
								"type":      "image_url",
								"image_url": map[string]any{"url": "data:" + mime + ";base64," + data},
							})
						}
					}
				}
			}
			pending = append(pending, pendingToolResult{message: out, toolCallID: toolCallID, images: images})
		default:
			flush()
			currentBatch = currentBatch[:0]
			consumedBatchIDs = map[string]bool{}
			var content any
			if message.Role == "user" {
				content = normalizePlainContentForUpstream(message.Content)
			} else {
				content = plainContentToString(message.Content)
			}
			normalized = append(normalized, map[string]any{"role": message.Role, "content": content})
		}
	}
	flush()
	return normalized
}

func plainContentIsEmpty(content any) bool {
	switch v := content.(type) {
	case string:
		return v == ""
	case nil:
		return true
	default:
		return false
	}
}

// summarizeNormalizedToolHistory is the port of worker
// summarizeNormalizedToolHistory (first 128 messages).
func summarizeNormalizedToolHistory(messages []map[string]any) []map[string]any {
	limit := len(messages)
	if limit > 128 {
		limit = 128
	}
	summary := make([]map[string]any, 0, limit)
	for index, message := range messages[:limit] {
		role, _ := message["role"].(string)
		if role == "" {
			role = "<missing>"
		}
		entry := map[string]any{"index": index, "role": role}
		if role == "assistant" {
			if calls, ok := message["tool_calls"].([]map[string]any); ok && len(calls) > 0 {
				callLimit := len(calls)
				if callLimit > 32 {
					callLimit = 32
				}
				callSummaries := make([]map[string]any, 0, callLimit)
				for _, call := range calls[:callLimit] {
					id, _ := call["id"].(string)
					if id == "" {
						id = "<missing>"
					}
					name := "<missing>"
					argumentLength := 0
					if fn, ok := call["function"].(map[string]any); ok {
						if value, _ := fn["name"].(string); value != "" {
							name = value
						}
						if value, ok := fn["arguments"].(string); ok {
							argumentLength = len(value)
						}
					}
					callSummaries = append(callSummaries, map[string]any{
						"id": id, "name": name, "argumentLength": argumentLength,
					})
				}
				entry["toolCalls"] = callSummaries
			}
		}
		if role == "tool" {
			toolCallID, _ := message["tool_call_id"].(string)
			if toolCallID == "" {
				toolCallID = "<missing>"
			}
			entry["toolCallId"] = toolCallID
			content := message["content"]
			entry["contentType"] = plainContentType(content)
			entry["contentLength"] = plainContentLength(content)
		} else if _, hasCalls := entry["toolCalls"]; !hasCalls {
			entry["contentLength"] = plainContentLength(message["content"])
		}
		summary = append(summary, entry)
	}
	return summary
}

func plainContentLength(content any) int {
	if value, ok := content.(string); ok {
		return len(value)
	}
	if content == nil {
		return 0
	}
	return len(plainJSON(content))
}

func plainContentType(content any) string {
	switch content.(type) {
	case nil:
		return "null"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64, int:
		return "number"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}

// diagnosePlainToolResults is the port of worker diagnoseToolResults (first
// 64 tool results).
func diagnosePlainToolResults(messages []translate.ChatMessage) []map[string]any {
	callNames := map[string]string{}
	resultBatchSize := map[string]int{}
	for _, message := range messages {
		if message.Role == "assistant" && len(message.ToolCalls) > 0 {
			calls := parseToolCalls(message.ToolCalls)
			if len(calls) == 0 {
				continue
			}
			var batch []string
			for _, call := range calls {
				id := strings.TrimSpace(call.ID)
				if id == "" {
					continue
				}
				name := strings.TrimSpace(callFunctionName(call))
				if name == "" {
					name = "<missing>"
				}
				callNames[id] = name
				batch = append(batch, id)
			}
			for _, id := range batch {
				resultBatchSize[id] = len(batch)
			}
		} else if message.Role != "tool" {
			resultBatchSize = map[string]int{}
		}
	}
	diagnostics := make([]map[string]any, 0, 8)
	for index, message := range messages {
		if message.Role != "tool" {
			continue
		}
		trimmedID := strings.TrimSpace(message.ToolCallID)
		toolCallID := trimmedID
		if toolCallID == "" {
			toolCallID = "<missing>"
		}
		content := message.Content
		serialized := ""
		if value, ok := content.(string); ok {
			serialized = value
		} else if content != nil {
			serialized = plainJSON(content)
		}
		name, known := callNames[trimmedID]
		if !known {
			if message.Name != "" {
				name = message.Name
			} else {
				name = "<unknown>"
			}
		}
		entry := map[string]any{
			"resultIndex":       index,
			"toolCallId":        toolCallID,
			"toolName":          name,
			"contentType":       plainContentType(content),
			"contentLength":     len(serialized),
			"hasControlChars":   plainHasControlChars(serialized),
			"looksBinary":       plainLooksBinary(serialized),
			"parallelBatchSize": resultBatchSize[trimmedID],
		}
		if _, ok := content.(string); ok {
			var probe any
			entry["jsonValid"] = json.Unmarshal([]byte(serialized), &probe) == nil
		} else {
			entry["jsonValid"] = nil
		}
		diagnostics = append(diagnostics, entry)
		if len(diagnostics) >= 64 {
			break
		}
	}
	return diagnostics
}

func plainHasControlChars(value string) bool {
	for _, ch := range value {
		switch {
		case ch >= 0x00 && ch <= 0x08, ch == 0x0b, ch == 0x0c,
			ch >= 0x0e && ch <= 0x1f, ch == 0x7f:
			return true
		}
	}
	return false
}

func plainLooksBinary(value string) bool {
	if strings.ContainsRune(value, 0x00) || strings.ContainsRune(value, 0xfffd) {
		return true
	}
	runes := []rune(value)
	if len(runes) == 0 {
		return false
	}
	count := 0
	for _, ch := range runes {
		switch {
		case ch >= 0x00 && ch <= 0x08, ch >= 0x0e && ch <= 0x1f:
			count++
		}
	}
	return float64(count)/float64(len(runes)) > 0.01
}

// clampReasoningLevel maps the requested effort through the shared level
// table and clamps to the catalog allow list (AGENTS.md: catalog wins;
// console value is a default only).
func clampReasoningLevel(rawEffort string, allowed []string) string {
	normalized := providers.NormalizeReasoningLevel(rawEffort)
	if len(allowed) == 0 {
		return normalized
	}
	return providers.ResolveReasoningLevel(normalized, providers.ModelCapabilities{
		ReasoningOptions: allowed,
	})
}

// buildPlainChatBody builds the upstream RemoteChatAsk plain JSON body from a
// translated chat request. Semantic port of worker/src/plaintext.mjs
// buildPlainChatBody (own code, MIT). Deviations from the worker, all
// controller-ruled:
//   - no capture-template merge in the native path (base map stays empty;
//     buildPlainChatBodyWithTemplate keeps the hook for Task 8 findings);
//   - top-level `system` request param does not exist on translate.ChatRequest,
//     so system text comes from messages only;
//   - is_vl also turns on when the conversation carries images;
//   - parameters.reasoning_effort is the catalog-clamped level, not the raw
//     client value; enable_thinking follows worker effort semantics.
func buildPlainChatBody(req translate.ChatRequest, resolved ResolvedModel, sessionID string) ([]byte, error) {
	return buildPlainChatBodyWithTemplate(req, resolved, sessionID, nil)
}

func buildPlainChatBodyWithTemplate(req translate.ChatRequest, resolved ResolvedModel, sessionID string, template map[string]any) ([]byte, error) {
	base := map[string]any{}
	for key, value := range template {
		base[key] = value
	}
	requestID := newUUIDv4()
	requestedModel := strings.TrimSpace(req.Model)
	if requestedModel == "" {
		requestedModel = "auto"
	}
	mappedModel := resolved.Key
	if mappedModel == "" {
		mappedModel = requestedModel
	}

	filtered, _, _ := filterUnknownToolHistory(req.Messages, req.Tools)
	openaiMessages := normalizePlainMessages(filtered)

	systemText := ""
	var systemMessages []map[string]any
	var nonSystem []map[string]any
	for _, message := range openaiMessages {
		role, _ := message["role"].(string)
		if role == "system" || role == "developer" {
			if text := plainContentToString(message["content"]); text != "" {
				if systemText != "" {
					systemText += "\n\n"
				}
				systemText += text
			}
			continue
		}
		nonSystem = append(nonSystem, message)
	}
	if systemText != "" {
		systemMessages = append(systemMessages, map[string]any{"role": "system", "content": systemText})
	}

	normalizedEffort := ""
	if len(req.ReasoningEffort) > 0 {
		var s string
		if err := json.Unmarshal(req.ReasoningEffort, &s); err == nil && strings.TrimSpace(s) != "" {
			normalizedEffort = strings.TrimSpace(s)
		}
	}
	var effectiveThinking *bool
	switch {
	case req.EnableThinking != nil:
		value := *req.EnableThinking
		effectiveThinking = &value
	case normalizedEffort != "":
		value := plainEffortEnablesThinking(normalizedEffort)
		effectiveThinking = &value
	case req.EnableReasoning != nil:
		value := *req.EnableReasoning
		effectiveThinking = &value
	}

	hasImages := plainHasImages(nonSystem)

	modelConfig := map[string]any{
		"key":              mappedModel,
		"display_name":     firstNonEmptyString(resolved.DisplayName, mappedModel),
		"model":            "",
		"format":           "openai",
		"is_vl":            resolved.IsVL || hasImages,
		"is_reasoning":     resolved.IsReasoning,
		"api_key":          "",
		"url":              "",
		"source":           firstNonEmptyString(resolved.Source, "system"),
		"max_input_tokens": resolved.MaxInputTokens,
	}
	if resolved.MaxInputTokens <= 0 {
		modelConfig["max_input_tokens"] = 180000
	}
	if effectiveThinking != nil {
		modelConfig["is_reasoning"] = *effectiveThinking
	}

	parameters := map[string]any{"max_tokens": plainMaxTokens(req, 32000)}
	if normalizedEffort != "" {
		parameters["reasoning_effort"] = clampReasoningLevel(normalizedEffort, resolved.AllowedReasoningLevels)
	}
	if effectiveThinking != nil {
		parameters["enable_thinking"] = *effectiveThinking
	}
	if len(req.ReasoningBudgetTokens) > 0 {
		parameters["reasoning_budget_tokens"] = json.RawMessage(req.ReasoningBudgetTokens)
	}
	if len(req.ContextLength) > 0 {
		parameters["context_length"] = json.RawMessage(req.ContextLength)
	}
	if len(req.ToolChoice) > 0 {
		parameters["tool_choice"] = json.RawMessage(req.ToolChoice)
	}
	if len(req.Temperature) > 0 {
		parameters["temperature"] = json.RawMessage(req.Temperature)
	}
	if len(req.TopP) > 0 {
		parameters["top_p"] = json.RawMessage(req.TopP)
	}
	if len(req.Stop) > 0 {
		parameters["stop"] = json.RawMessage(req.Stop)
	}
	if req.ParallelToolCalls != nil {
		parameters["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if len(req.ResponseFormat) > 0 {
		parameters["response_format"] = json.RawMessage(req.ResponseFormat)
	}

	messages := make([]map[string]any, 0, len(systemMessages)+len(nonSystem))
	messages = append(messages, systemMessages...)
	if len(nonSystem) > 0 {
		messages = append(messages, nonSystem...)
	} else {
		messages = append(messages, map[string]any{"role": "user", "content": "ping"})
	}

	businessName := "chat"
	if len(nonSystem) > 0 {
		if value := plainContentToString(nonSystem[0]["content"]); value != "" {
			runes := []rune(value)
			if len(runes) > 40 {
				runes = runes[:40]
			}
			businessName = string(runes)
		}
	}
	businessVersion := os.Getenv("QODER_CLI_VERSION")
	if businessVersion == "" {
		businessVersion = qoderCLIVersion
	}

	base["request_id"] = requestID
	base["request_set_id"] = newUUIDv4()
	base["chat_record_id"] = requestID
	base["session_id"] = sessionID
	base["stream"] = true
	if _, ok := base["chat_task"]; !ok {
		base["chat_task"] = "FREE_INPUT"
	}
	base["chat_context"] = map[string]any{
		"text": "", "features": map[string]any{}, "extra": map[string]any{},
		"chatPrompt": "", "imageUrls": []any{},
	}
	base["is_reply"] = false
	base["is_retry"] = false
	if _, ok := base["source"]; !ok {
		base["source"] = "cli"
	}
	if _, ok := base["version"]; !ok {
		base["version"] = "1.0"
	}
	if _, ok := base["agent_id"]; !ok {
		base["agent_id"] = "agent_common"
	}
	base["task_id"] = "common"
	if _, ok := base["session_type"]; !ok {
		base["session_type"] = "assistant"
	}
	if _, ok := base["aliyun_user_type"]; !ok {
		base["aliyun_user_type"] = ""
	}
	base["model_config"] = modelConfig
	base["custom_model"] = nil
	base["system"] = systemText
	base["messages"] = messages
	base["tools"] = normalizePlainTools(req.Tools)
	base["parameters"] = parameters
	base["business"] = map[string]any{
		"product":  "cli",
		"version":  businessVersion,
		"type":     "agent",
		"id":       newUUIDv4(),
		"name":     businessName,
		"begin_at": time.Now().UnixMilli(),
		"stage":    "start",
	}
	return json.Marshal(base)
}

func plainEffortEnablesThinking(effort string) bool {
	switch strings.ToLower(effort) {
	case "none", "off", "disabled":
		return false
	}
	return true
}

func plainMaxTokens(req translate.ChatRequest, fallback int) int {
	raw := req.MaxTokens
	if len(req.MaxCompletionTokens) > 0 {
		raw = req.MaxCompletionTokens
	}
	if len(raw) == 0 {
		return fallback
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil || value <= 0 {
		return fallback
	}
	return int(value)
}

func plainHasImages(messages []map[string]any) bool {
	for _, message := range messages {
		if parts, ok := message["content"].([]any); ok {
			for _, item := range parts {
				if part, ok := item.(map[string]any); ok {
					if kind, _ := part["type"].(string); kind == "image_url" {
						return true
					}
				}
			}
		}
	}
	return false
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
