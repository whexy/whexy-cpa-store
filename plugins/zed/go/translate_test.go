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
	model := zedModel{ID: "claude-sonnet-5-5", OutputTokens: 64000, SupportsThinking: true, EffortLevels: []string{"low", "medium", "high", "xhigh", "max"}}
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

func TestNormalizeClaudeKeepsOlderModelParameters(t *testing.T) {
	// Zed lists claude-sonnet-4-5 with thinking but no effort levels.
	model := zedModel{ID: "claude-sonnet-4-5", OutputTokens: 8192, SupportsThinking: true}
	body := mustJSON(t, map[string]any{
		"model":       "claude-sonnet-4-5",
		"max_tokens":  4096,
		"temperature": 0.2,
		"thinking":    map[string]any{"type": "enabled", "budget_tokens": 2048},
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
	if gjson.GetBytes(out, "thinking.type").String() != "enabled" || gjson.GetBytes(out, "thinking.budget_tokens").Int() != 2048 {
		t.Fatalf("budget thinking rewritten on a model without effort levels: %s", out)
	}
	if gjson.GetBytes(out, "output_config.effort").Exists() {
		t.Fatalf("effort set on a model without effort levels: %s", out)
	}
}

func TestNormalizeClaudeFitsBudgetUnderOutputLimit(t *testing.T) {
	// Claude Code's thinking request, sent to a model whose output limit is
	// below its max_tokens.
	model := zedModel{ID: "claude-sonnet-4-5", OutputTokens: 8192, SupportsThinking: true}
	body := []byte(`{"max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":31999},"messages":[{"role":"user","content":"hi"}]}`)
	out, errNormalize := normalizeClaudeBody(body, model)
	if errNormalize != nil {
		t.Fatal(errNormalize)
	}
	if gjson.GetBytes(out, "max_tokens").Int() != 8192 || gjson.GetBytes(out, "thinking.budget_tokens").Int() != 8191 {
		t.Fatalf("budget not fitted under max_tokens: %s", out)
	}
}

func TestNormalizeClaudeSkipsUnknownEffortLevels(t *testing.T) {
	model := zedModel{ID: "claude-future", OutputTokens: 8192, SupportsThinking: true, EffortLevels: []string{"turbo"}}
	body := []byte(`{"max_tokens":4096,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"hi"}]}`)
	out, errNormalize := normalizeClaudeBody(body, model)
	if errNormalize != nil {
		t.Fatal(errNormalize)
	}
	if gjson.GetBytes(out, "output_config.effort").Exists() {
		t.Fatalf("an effort no level matches was written: %s", out)
	}
}

func TestApplyRequestedEffort(t *testing.T) {
	sonnet46 := zedModel{ID: "claude-sonnet-4-6", SupportsThinking: true, EffortLevels: []string{"low", "medium", "high", "max"}}
	sonnet45 := zedModel{ID: "claude-sonnet-4-5", SupportsThinking: true}
	sol61 := zedModel{ID: "gpt-6.1-sol", SupportsThinking: true, EffortLevels: []string{"low", "medium", "high", "xhigh", "max"}}
	sol6 := zedModel{ID: "gpt-6-sol", SupportsThinking: true, EffortLevels: []string{"low", "medium", "high", "xhigh"}}
	gpt52 := zedModel{ID: "gpt-5.2"}
	argon := zedModel{ID: "gemini-4-argon", SupportsThinking: true, EffortLevels: []string{"LOW", "MEDIUM", "HIGH"}}
	claudeBody := []byte(`{"max_tokens":8192,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`)
	cases := []struct {
		name     string
		upstream sdktranslator.Format
		model    zedModel
		suffix   string
		body     []byte
		want     map[string]string
		absent   []string
	}{
		{"level turns on adaptive thinking", sdktranslator.FormatClaude, sonnet46, "medium", []byte(`{}`), map[string]string{"thinking.type": "adaptive", "output_config.effort": "medium"}, nil},
		{"unlisted level rounds down", sdktranslator.FormatClaude, sonnet46, "xhigh", []byte(`{}`), map[string]string{"output_config.effort": "high"}, nil},
		{"weaker than every level", sdktranslator.FormatClaude, sonnet46, "minimal", []byte(`{}`), map[string]string{"output_config.effort": "low"}, nil},
		{"budget maps to a level", sdktranslator.FormatClaude, sonnet46, "16384", []byte(`{}`), map[string]string{"output_config.effort": "medium"}, nil},
		{"zero turns thinking off", sdktranslator.FormatClaude, sonnet46, "0", claudeBody, nil, []string{"thinking", "output_config.effort"}},
		{"none turns thinking off", sdktranslator.FormatClaude, sonnet46, "none", claudeBody, nil, []string{"thinking", "output_config.effort"}},
		{"auto keeps the request", sdktranslator.FormatClaude, sonnet46, "auto", claudeBody, map[string]string{"thinking.type": "adaptive", "output_config.effort": "high"}, nil},
		{"budget model takes a number", sdktranslator.FormatClaude, sonnet45, "4096", []byte(`{}`), map[string]string{"thinking.type": "enabled", "thinking.budget_tokens": "4096"}, []string{"output_config"}},
		{"budget model ignores a level", sdktranslator.FormatClaude, sonnet45, "high", []byte(`{}`), nil, []string{"thinking", "output_config"}},
		{"budget stays below max_tokens", sdktranslator.FormatClaude, sonnet45, "16384", []byte(`{"max_tokens":8192}`), map[string]string{"thinking.budget_tokens": "8191"}, nil},
		{"budget rises to the minimum", sdktranslator.FormatClaude, sonnet45, "500", []byte(`{"max_tokens":8192}`), map[string]string{"thinking.budget_tokens": "1024"}, nil},
		{"no room for a budget", sdktranslator.FormatClaude, sonnet45, "4096", []byte(`{"max_tokens":1024}`), nil, []string{"thinking"}},
		{"none on a model without thinking", sdktranslator.FormatOpenAIResponse, gpt52, "none", []byte(`{"reasoning":{"effort":"high"}}`), nil, []string{"reasoning"}},
		{"max where listed", sdktranslator.FormatOpenAIResponse, sol61, "max", []byte(`{}`), map[string]string{"reasoning.effort": "max"}, nil},
		{"max rounds down", sdktranslator.FormatOpenAIResponse, sol6, "ultra", []byte(`{}`), map[string]string{"reasoning.effort": "xhigh"}, nil},
		{"model without thinking", sdktranslator.FormatOpenAIResponse, gpt52, "high", []byte(`{}`), nil, []string{"reasoning"}},
		{"gemini level", sdktranslator.FormatGemini, argon, "max", []byte(`{}`), map[string]string{"generationConfig.thinkingConfig.thinkingLevel": "HIGH", "generationConfig.thinkingConfig.includeThoughts": "true"}, nil},
	}
	for _, tc := range cases {
		out := applyRequestedEffort(tc.upstream, tc.body, tc.suffix, tc.model)
		for path, want := range tc.want {
			if got := gjson.GetBytes(out, path).String(); got != want {
				t.Errorf("%s: %s = %q, want %q: %s", tc.name, path, got, want, out)
			}
		}
		for _, path := range tc.absent {
			if gjson.GetBytes(out, path).Exists() {
				t.Errorf("%s: %s survived: %s", tc.name, path, out)
			}
		}
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

func TestNormalizeClaudeBodyFitsZedParser(t *testing.T) {
	// The shapes Claude Code sends that Zed's Anthropic parser rejects.
	model := zedModel{ID: "claude-sonnet-5-5", OutputTokens: 64000, SupportsThinking: true, EffortLevels: []string{"low", "medium", "high"}}
	body := mustJSON(t, map[string]any{
		"model":      "claude-sonnet-5-5",
		"max_tokens": 32000,
		"thinking":   map[string]any{"type": "between_tools"},
		"context_management": map[string]any{"edits": []any{
			map[string]any{"type": "clear_thinking_20251015", "keep": "all"},
			map[string]any{"type": "clear_tool_uses_20250919"},
		}},
		"tools": []any{
			map[string]any{"name": "Bash", "description": "Run a command", "input_schema": map[string]any{"type": "object"}},
			map[string]any{"name": "mcp__docs__search", "input_schema": map[string]any{"type": "object"}},
			map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 8},
		},
		"tool_choice": map[string]any{"type": "tool", "name": "web_search"},
		"messages": []any{
			map[string]any{"role": "user", "content": "list files"},
			map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "<system-reminder>a</system-reminder>"}}},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ls"}}}},
			map[string]any{"role": "system", "content": "<system-reminder>b</system-reminder>"},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "a.txt"}}},
		},
	})
	out, errNormalize := normalizeClaudeBody(body, model)
	if errNormalize != nil {
		t.Fatal(errNormalize)
	}
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 3 {
		t.Fatalf("messages: %s", out)
	}
	for index, role := range []string{"user", "assistant", "user"} {
		if got := messages[index].Get("role").String(); got != role {
			t.Fatalf("message %d role = %q: %s", index, got, out)
		}
	}
	if texts := messages[0].Get("content.#.text").Array(); len(texts) != 2 || texts[1].String() != "<system-reminder>a</system-reminder>" {
		t.Fatalf("first turn: %s", messages[0].Raw)
	}
	last := messages[2].Get("content").Array()
	if len(last) != 2 || last[0].Get("type").String() != "tool_result" || last[0].Get("is_error").Type != gjson.False || last[1].Get("text").String() != "<system-reminder>b</system-reminder>" {
		t.Fatalf("tool_result turn: %s", messages[2].Raw)
	}
	if gjson.GetBytes(out, "context_management").Exists() {
		t.Fatalf("clearing edits survived: %s", out)
	}
	if gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("between_tools survived: %s", out)
	}
	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 2 || tools[1].Get("description").Type != gjson.String {
		t.Fatalf("tools: %s", out)
	}
	if gjson.GetBytes(out, "tool_choice").Exists() {
		t.Fatalf("tool_choice naming a dropped tool survived: %s", out)
	}
}

func TestNormalizeClaudeKeepsCompaction(t *testing.T) {
	model := zedModel{ID: "claude-sonnet-5-5"}
	body := mustJSON(t, map[string]any{
		"max_tokens": 1024,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"context_management": map[string]any{"edits": []any{
			map[string]any{"type": "clear_thinking_20251015", "keep": "all"},
			map[string]any{"type": "compact_20260112", "trigger": map[string]any{"type": "input_tokens", "value": 150000}},
		}},
	})
	out, errNormalize := normalizeClaudeBody(body, model)
	if errNormalize != nil {
		t.Fatal(errNormalize)
	}
	edits := gjson.GetBytes(out, "context_management.edits").Array()
	if len(edits) != 1 || edits[0].Get("type").String() != "compact_20260112" || edits[0].Get("trigger.value").Int() != 150000 {
		t.Fatalf("edits: %s", out)
	}
}
