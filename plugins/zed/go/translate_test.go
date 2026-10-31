package main

import (
	"encoding/json"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestPlanRouteMatrix(t *testing.T) {
	cases := []struct {
		source   sdktranslator.Format
		upstream sdktranslator.Format
		ok       bool
	}{
		{sdktranslator.FormatClaude, sdktranslator.FormatClaude, true},
		{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, true},
		{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude, true},
		{sdktranslator.FormatGemini, sdktranslator.FormatClaude, true},
		{sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, true},
		{sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse, true},
		{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAIResponse, true},
		{sdktranslator.FormatClaude, sdktranslator.FormatGemini, true},
		{sdktranslator.FormatOpenAI, sdktranslator.FormatGemini, true},
		{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatGemini, true},
	}
	for _, tc := range cases {
		plan, errPlan := planRoute(tc.source, tc.source, tc.upstream)
		if tc.ok && errPlan != nil {
			t.Errorf("%s -> %s: %v", tc.source, tc.upstream, errPlan)
			continue
		}
		if !tc.ok {
			continue
		}
		if tc.source == tc.upstream && (plan.RequestTo != "" || plan.ResponseFrom != "") {
			t.Errorf("%s passthrough planned translation: %+v", tc.source, plan)
		}
		if tc.upstream == sdktranslator.FormatOpenAIResponse && plan.RequestTo != "" && plan.RequestTo != sdktranslator.FormatCodex {
			t.Errorf("Responses upstream reached through %s, want codex", plan.RequestTo)
		}
	}
}

func TestNormalizeClaudeBody(t *testing.T) {
	model := zedModel{ID: "claude-sonnet-5-5", OutputTokens: 64000}
	body := mustJSON(t, map[string]any{
		"model":       "claude-sonnet-5-5",
		"max_tokens":  200000,
		"temperature": 0.7,
		"top_p":       0.9,
		"thinking":    map[string]any{"type": "enabled", "budget_tokens": 8192},
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	})
	out, errNormalize := normalizeClaudeBody(body, model)
	if errNormalize != nil {
		t.Fatal(errNormalize)
	}
	var parsed map[string]any
	if errUnmarshal := json.Unmarshal(out, &parsed); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	for _, field := range []string{"temperature", "top_p", "top_k"} {
		if _, exists := parsed[field]; exists {
			t.Errorf("%s survived normalization", field)
		}
	}
	thinking := parsed["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" {
		t.Errorf("thinking = %v", thinking)
	}
	if _, exists := thinking["budget_tokens"]; exists {
		t.Error("budget_tokens survived")
	}
	if got := parsed["output_config"].(map[string]any)["effort"]; got != "medium" {
		t.Errorf("effort = %v", got)
	}
	messages := parsed["messages"].([]any)
	content := messages[0].(map[string]any)["content"].([]any)
	if content[0].(map[string]any)["type"] != "text" {
		t.Errorf("content = %v", content)
	}
	if parsed["max_tokens"].(float64) != 64000 {
		t.Errorf("max_tokens = %v", parsed["max_tokens"])
	}
}

func TestNormalizeClaudeKeepsSamplingOnOlderModel(t *testing.T) {
	model := zedModel{ID: "claude-sonnet-4-5", OutputTokens: 8192}
	body := mustJSON(t, map[string]any{
		"model":       "claude-sonnet-4-5",
		"max_tokens":  100,
		"temperature": 0.2,
		"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
	})
	out, errNormalize := normalizeClaudeBody(body, model)
	if errNormalize != nil {
		t.Fatal(errNormalize)
	}
	var parsed map[string]any
	_ = json.Unmarshal(out, &parsed)
	if parsed["temperature"] != 0.2 {
		t.Fatalf("temperature dropped on an older model: %s", out)
	}
}

func TestBuildUpstreamBodyCarriesGeminiLimitsToGPT(t *testing.T) {
	plan, errPlan := planRoute(sdktranslator.FormatGemini, sdktranslator.FormatGemini, sdktranslator.FormatOpenAIResponse)
	if errPlan != nil {
		t.Fatal(errPlan)
	}
	model := zedModel{ID: "gpt-6-luna", Provider: "open_ai", OutputTokens: 128000}
	payload := mustJSON(t, map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}},
		"generationConfig": map[string]any{
			"maxOutputTokens": 32,
			"thinkingConfig":  map[string]any{"thinkingLevel": "high"},
		},
	})
	out, errBuild := buildUpstreamBody(t.Context(), plan, payload, model, true)
	if errBuild != nil {
		t.Fatal(errBuild)
	}
	if got := gjson.GetBytes(out, "max_output_tokens").Int(); got != 32 {
		t.Errorf("max_output_tokens = %d: %s", got, out)
	}
	if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "high" {
		t.Errorf("reasoning.effort = %q: %s", got, out)
	}
}

func TestParseZedLine(t *testing.T) {
	wrapped := []byte(`{"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}}`)
	line, errLine := parseZedLine(wrapped)
	if errLine != nil || line.Event == nil || !strings.Contains(string(line.Event), "text_delta") {
		t.Fatalf("wrapped event: %+v %v", line, errLine)
	}
	bare := []byte(`{"type":"message_stop"}`)
	line, errLine = parseZedLine(bare)
	if errLine != nil || line.Event == nil || !strings.Contains(string(line.Event), "message_stop") {
		t.Fatalf("bare event: %+v %v", line, errLine)
	}
	ended, errEnded := parseZedLine([]byte(`{"status":"stream_ended"}`))
	if errEnded != nil || !ended.Ended {
		t.Fatalf("stream_ended: %+v %v", ended, errEnded)
	}
	started, errStarted := parseZedLine([]byte(`{"status":"started"}`))
	if errStarted != nil || started.Event != nil || started.Ended {
		t.Fatalf("status line: %+v %v", started, errStarted)
	}
	failed, errFailed := parseZedLine([]byte(`{"status":{"failed":{"code":"upstream_http_429","message":"slow down"}}}`))
	if errFailed != nil || failed.Failure == nil || failed.Failure.Status != 429 {
		t.Fatalf("failure: %+v %v", failed, errFailed)
	}
}

func mustMarshalJSON(value any) []byte {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		panic(errMarshal)
	}
	return raw
}

func TestNormalizeResponsesStringInput(t *testing.T) {
	model := zedModel{ID: "gpt-6-luna", OutputTokens: 128000}
	body := mustJSON(t, map[string]any{"model": "gpt-6-luna", "input": "hello", "max_output_tokens": 8})
	out, errNormalize := normalizeResponsesBody(body, model)
	if errNormalize != nil {
		t.Fatal(errNormalize)
	}
	items := gjson.GetBytes(out, "input").Array()
	if len(items) != 1 || items[0].Get("type").String() != "message" {
		t.Fatalf("input: %s", out)
	}
	if items[0].Get("content.0.type").String() != "input_text" || items[0].Get("content.0.text").String() != "hello" {
		t.Fatalf("content: %s", out)
	}
	if gjson.GetBytes(out, "max_output_tokens").Int() != minResponsesOutputTokens {
		t.Fatalf("max_output_tokens: %s", out)
	}
}
