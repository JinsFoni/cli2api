package qoder

import (
	"encoding/json"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

func TestAdapterRegistersNativeControlPlane(t *testing.T) {
	adapter := NewClient(nil).Adapter()
	if adapter.ID != "qoder" {
		t.Fatalf("id = %q", adapter.ID)
	}
	if adapter.Prober == nil {
		t.Fatal("Prober must register once probe/quota are native (pool-routed only)")
	}
	if adapter.Models == nil || adapter.Login == nil || adapter.Chat == nil {
		t.Fatal("expected Models, Login, and Chat wrappers")
	}
	if adapter.Checkin == nil {
		t.Fatal("native check-in must stay registered")
	}
}

func TestBuildChatPayloadForwardsReasoningAndContextParameters(t *testing.T) {
	enableThinking := true
	payload := BuildChatPayload(translate.ChatRequest{
		Model:                 "minimax-m3",
		Messages:              []translate.ChatMessage{{Role: "user", Content: "hi"}},
		EnableThinking:        &enableThinking,
		ReasoningEffort:       json.RawMessage(`"high"`),
		ReasoningBudgetTokens: json.RawMessage(`16384`),
		ContextLength:         json.RawMessage(`500000`),
		MaxInputTokens:        json.RawMessage(`1000000`),
	}, true)

	if payload["enable_thinking"] != true {
		t.Fatalf("enable_thinking = %#v", payload["enable_thinking"])
	}
	for key, want := range map[string]string{
		"reasoning_effort":        `"high"`,
		"reasoning_budget_tokens": "16384",
		"context_length":          "500000",
		"max_input_tokens":        "1000000",
	} {
		got, ok := payload[key].(json.RawMessage)
		if !ok || string(got) != want {
			t.Fatalf("%s = %#v, want %s", key, payload[key], want)
		}
	}
}

func TestBuildChatPayloadTokenPrecedenceAndOmission(t *testing.T) {
	for _, stream := range []bool{false, true} {
		req := translate.ChatRequest{Model: "m", Messages: []translate.ChatMessage{{Role: "user", Content: "hi"}}}
		payload := BuildChatPayload(req, stream)
		if len(payload) != 3 || payload["stream"] != stream || payload["model"] != "m" {
			t.Fatalf("minimal payload=%+v", payload)
		}
		req.MaxTokens = json.RawMessage(`12`)
		payload = BuildChatPayload(req, stream)
		if string(payload["max_tokens"].(json.RawMessage)) != "12" {
			t.Fatalf("max_tokens=%v", payload["max_tokens"])
		}
		req.MaxCompletionTokens = json.RawMessage(`34`)
		payload = BuildChatPayload(req, stream)
		if string(payload["max_tokens"].(json.RawMessage)) != "34" {
			t.Fatalf("completion token precedence=%v", payload["max_tokens"])
		}
		no := false
		req.EnableThinking = &no
		req.EnableReasoning = &no
		req.IsReasoning = &no
		req.ParallelToolCalls = &no
		payload = BuildChatPayload(req, stream)
		for _, key := range []string{"enable_thinking", "enable_reasoning", "is_reasoning", "parallel_tool_calls"} {
			if value, ok := payload[key]; !ok || value != false {
				t.Fatalf("explicit false %s=%v present=%v", key, value, ok)
			}
		}
	}
}

func TestModelInfosMapsQoderPriceFields(t *testing.T) {
	models := ModelInfos([]map[string]any{
		{"id": "kimi-k3", "mapped_key": "kmodel_latest", "display_name": "Kimi-K3", "price_factor": 1.4},
		{"id": "qwen3.7-plus", "mapped_key": "qmodel", "display_name": "Qwen3.7-Plus", "price_factor": 0.1},
		{"id": "qwen3.8-flash", "mapped_key": "qfmodel", "display_name": "Qwen3.8-Flash", "price_factor": 0.0, "is_free": true},
		{"id": "qwen3.8-max", "mapped_key": "qmodel_38max", "display_name": "Qwen3.8-Max", "price_factor": 0.5, "is_free": true},
		{"id": "auto", "mapped_key": "auto", "display_name": "Auto", "price_factor": 0.5},
		// No price data at all: neither credits nor free may be invented.
		{"id": "no-price", "mapped_key": "npmodel", "display_name": "NoPrice"},
	})

	byID := map[string]providers.ModelInfo{}
	for _, m := range models {
		byID[m.PublicModel] = m
	}

	if got := byID["kimi-k3"].Credits; got != "x1.4" {
		t.Errorf("kimi-k3 credits = %q, want x1.4", got)
	}
	if got := byID["qwen3.7-plus"].Credits; got != "x0.1" {
		t.Errorf("qwen3.7-plus credits = %q, want x0.1", got)
	}
	if got := byID["auto"].Credits; got != "x0.5" {
		t.Errorf("auto credits = %q, want x0.5", got)
	}

	free := byID["qwen3.8-flash"]
	if !free.Free || free.Credits != "0" {
		t.Errorf("qwen3.8-flash free=%v credits=%q, want free + \"0\"", free.Free, free.Credits)
	}
	// A dual is_free + positive factor model (Qwen3.8-Max) is NOT free: the
	// Qoder client still labels it with its multiplier.
	dual := byID["qwen3.8-max"]
	if dual.Free || dual.Credits != "x0.5" {
		t.Errorf("qwen3.8-max free=%v credits=%q, want not-free + x0.5", dual.Free, dual.Credits)
	}
	for id, m := range byID {
		if id == "qwen3.8-flash" {
			continue
		}
		if m.Free {
			t.Errorf("%s must not be flagged free", id)
		}
	}
	if priced := byID["no-price"]; priced.Credits != "" || priced.Free {
		t.Errorf("no-price must stay unpriced: credits=%q free=%v", priced.Credits, priced.Free)
	}
}

func TestModelInfosPrefersExplicitCreditsText(t *testing.T) {
	models := ModelInfos([]map[string]any{
		{"id": "x", "mapped_key": "xk", "display_name": "X", "credits": "2x credits", "price_factor": 0.5},
	})
	if len(models) != 1 || models[0].Credits != "2x credits" {
		t.Fatalf("explicit credits text must win: %+v", models)
	}
}

func TestApplyModelPricingMapsWorkerFields(t *testing.T) {
	entry := map[string]any{"id": "kimi-k3", "price_factor": 1.4}
	ApplyModelPricing(entry)
	if entry["credits"] != "x1.4" {
		t.Errorf("credits = %v, want x1.4", entry["credits"])
	}
	if _, ok := entry["free"]; ok {
		t.Errorf("paid model must not be flagged free: %v", entry["free"])
	}

	freeEntry := map[string]any{"id": "qwen3.8-flash", "price_factor": 0.0, "is_free": true}
	ApplyModelPricing(freeEntry)
	if freeEntry["credits"] != "0" || freeEntry["free"] != true {
		t.Errorf("free model = %+v, want credits 0 + free", freeEntry)
	}

	// is_free=true with a positive factor (Qwen3.8-Max) must NOT be free and
	// must show the multiplier, matching the Qoder client label.
	dual := map[string]any{"id": "qwen3.8-max", "price_factor": 0.5, "is_free": true}
	ApplyModelPricing(dual)
	if dual["credits"] != "x0.5" {
		t.Errorf("dual credits = %v, want x0.5", dual["credits"])
	}
	if _, ok := dual["free"]; ok {
		t.Errorf("dual model must not be flagged free: %v", dual["free"])
	}

	// limited_time_free tag is the Qoder free signal.
	tagged := map[string]any{"id": "tagged", "price_factor": 0.5, "tags": []any{"limited_time_free"}}
	ApplyModelPricing(tagged)
	if tagged["credits"] != "0" || tagged["free"] != true {
		t.Errorf("limited_time_free = %+v, want credits 0 + free", tagged)
	}

	unpriced := map[string]any{"id": "unknown"}
	ApplyModelPricing(unpriced)
	if _, ok := unpriced["credits"]; ok {
		t.Errorf("unpriced model must not gain credits: %+v", unpriced)
	}
	if _, ok := unpriced["free"]; ok {
		t.Errorf("unpriced model must not be flagged free: %+v", unpriced)
	}
}

func TestApplyModelContextUsesUpstreamWindows(t *testing.T) {
	// qwen3.8-max: default window 200000, selectable up to 1M.
	entry := map[string]any{
		"id":                        "qwen3.8-max",
		"context_length":            180000,
		"default_context_window":    200000,
		"available_context_windows": []any{200000.0, 400000.0, 1000000.0},
		"max_output_tokens":         32000.0,
	}
	ApplyModelContext(entry)
	if entry["catalog_context_length"] != 200000 {
		t.Errorf("catalog_context_length = %v, want 200000", entry["catalog_context_length"])
	}
	if entry["catalog_context_length_max"] != 1000000 {
		t.Errorf("catalog_context_length_max = %v, want 1000000", entry["catalog_context_length_max"])
	}
	if got, _ := numberFieldValue(entry, "max_output_tokens"); got != 32000 {
		t.Errorf("max_output_tokens = %v, want 32000", entry["max_output_tokens"])
	}

	// glm-5.3-flash: max_input_tokens 1M, no higher selectable window.
	flash := map[string]any{"id": "glm-5.3-flash", "context_length": 1000000.0}
	ApplyModelContext(flash)
	if flash["catalog_context_length"] != 1000000 {
		t.Errorf("glm-5.3-flash window = %v, want 1000000", flash["catalog_context_length"])
	}
	if _, ok := flash["catalog_context_length_max"]; ok {
		t.Errorf("no available_context_windows -> no max tier: %v", flash["catalog_context_length_max"])
	}

	// No context metadata: nothing invented.
	bare := map[string]any{"id": "unknown"}
	ApplyModelContext(bare)
	if _, ok := bare["catalog_context_length"]; ok {
		t.Errorf("bare entry must not gain a window: %+v", bare)
	}
}

func TestModelInfosCarriesContextWindows(t *testing.T) {
	models := ModelInfos([]map[string]any{
		{
			"id": "qwen3.8-max", "mapped_key": "qmodel_38max", "display_name": "Qwen3.8-Max",
			"default_context_window": 200000.0, "available_context_windows": []any{200000.0, 1000000.0},
			"max_output_tokens": 32000.0,
		},
	})
	if len(models) != 1 {
		t.Fatalf("models = %+v", models)
	}
	caps := models[0].Capabilities
	if caps.ContextWindow != 200000 {
		t.Errorf("ContextWindow = %d, want 200000", caps.ContextWindow)
	}
	if caps.ContextWindowMax != 1000000 {
		t.Errorf("ContextWindowMax = %d, want 1000000", caps.ContextWindowMax)
	}
	if caps.MaxOutput != 32000 {
		t.Errorf("MaxOutput = %d, want 32000", caps.MaxOutput)
	}
}
