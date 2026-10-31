package main

import (
	"encoding/json"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func claudeEvents() [][]byte {
	return [][]byte{
		[]byte(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}`),
		[]byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		[]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pong"}}`),
		[]byte(`{"type":"content_block_stop","index":0}`),
		[]byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`),
		[]byte(`{"type":"message_stop"}`),
	}
}

func TestAssembleClaude(t *testing.T) {
	out, errAssemble := assembleClaude(claudeEvents())
	if errAssemble != nil {
		t.Fatal(errAssemble)
	}
	var message map[string]any
	if errUnmarshal := json.Unmarshal(out, &message); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if message["type"] != "message" || message["stop_reason"] != "end_turn" {
		t.Fatalf("message: %s", out)
	}
	content := message["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "pong" {
		t.Fatalf("content: %v", content)
	}
}

func TestAssembleClaudeToolUse(t *testing.T) {
	events := [][]byte{
		[]byte(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`),
		[]byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}`),
		[]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`),
		[]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`),
		[]byte(`{"type":"content_block_stop","index":0}`),
		[]byte(`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`),
		[]byte(`{"type":"message_stop"}`),
	}
	out, errAssemble := assembleClaude(events)
	if errAssemble != nil {
		t.Fatal(errAssemble)
	}
	var message map[string]any
	_ = json.Unmarshal(out, &message)
	block := message["content"].([]any)[0].(map[string]any)
	if block["type"] != "tool_use" || block["input"].(map[string]any)["command"] != "ls" {
		t.Fatalf("tool block: %v", block)
	}
}

func TestAssembleResponses(t *testing.T) {
	events := [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}`),
	}
	out, errAssemble := assembleResponses(events)
	if errAssemble != nil {
		t.Fatal(errAssemble)
	}
	if !strings.Contains(string(out), `"status":"completed"`) {
		t.Fatalf("response: %s", out)
	}
}

func TestSSEFrame(t *testing.T) {
	frame := sseFrame([]byte(`{"type":"message_stop"}`))
	if string(frame) != "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n" {
		t.Fatalf("frame = %q", frame)
	}
	gemini := sseFrame([]byte(`{"candidates":[]}`))
	if string(gemini) != "data: {\"candidates\":[]}\n\n" {
		t.Fatalf("gemini frame = %q", gemini)
	}
}

func TestLineSplitter(t *testing.T) {
	var splitter lineSplitter
	lines := splitter.push([]byte("{\"a\":1}\n{\"b\":"))
	if len(lines) != 1 || string(lines[0]) != `{"a":1}` {
		t.Fatalf("lines = %q", lines)
	}
	lines = splitter.push([]byte("2}\n"))
	if len(lines) != 1 || string(lines[0]) != `{"b":2}` {
		t.Fatalf("lines = %q", lines)
	}
	if rest := splitter.flush(); len(rest) != 0 {
		t.Fatalf("flush = %q", rest)
	}
}

func TestStreamConverterPassthroughClaude(t *testing.T) {
	plan, errPlan := planRoute(sdktranslator.FormatClaude, sdktranslator.FormatClaude, sdktranslator.FormatClaude)
	if errPlan != nil {
		t.Fatal(errPlan)
	}
	converter := newStreamConverter(nil, plan, "claude-sonnet-5-5", nil, nil, nil)
	chunks, errConvert := converter.convert([]byte(`{"type":"message_start","message":{}}`))
	if errConvert != nil {
		t.Fatal(errConvert)
	}
	if len(chunks) != 1 || !strings.HasPrefix(string(chunks[0]), "event: message_start\n") {
		t.Fatalf("chunks = %q", chunks)
	}
}

func TestStreamConverterPassthroughGeminiIsBareJSON(t *testing.T) {
	plan, errPlan := planRoute(sdktranslator.FormatGemini, sdktranslator.FormatGemini, sdktranslator.FormatGemini)
	if errPlan != nil {
		t.Fatal(errPlan)
	}
	converter := newStreamConverter(nil, plan, "gemini-3-flash", nil, nil, nil)
	chunks, errConvert := converter.convert([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`))
	if errConvert != nil {
		t.Fatal(errConvert)
	}
	if len(chunks) != 1 || strings.HasPrefix(string(chunks[0]), "data:") {
		t.Fatalf("gemini chunk must be bare JSON: %q", chunks)
	}
}

func TestGeminiTranslatedStreamFlushesTerminal(t *testing.T) {
	plan, errPlan := planRoute(sdktranslator.FormatClaude, sdktranslator.FormatClaude, sdktranslator.FormatGemini)
	if errPlan != nil {
		t.Fatal(errPlan)
	}
	converter := newStreamConverter(nil, plan, "gemini-3-flash", nil, nil, nil)
	event := []byte(`{"candidates":[{"content":{"parts":[{"text":"pong"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6}}`)
	chunks, errConvert := converter.convert(event)
	if errConvert != nil {
		t.Fatal(errConvert)
	}
	var combined strings.Builder
	for _, chunk := range chunks {
		combined.Write(chunk)
	}
	terminal, errFinish := converter.finish()
	if errFinish != nil {
		t.Fatal(errFinish)
	}
	for _, chunk := range terminal {
		combined.Write(chunk)
	}
	out := combined.String()
	if !strings.Contains(out, "pong") {
		t.Fatalf("translated stream lost the text:\n%s", out)
	}
	if !strings.Contains(out, "message_stop") {
		t.Fatalf("translated stream has no terminal event:\n%s", out)
	}
}

func TestAssembleClaudeRejectsErrorAndMissingStop(t *testing.T) {
	events := [][]byte{
		[]byte(`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[]}}`),
		[]byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`),
	}
	if _, errAssemble := assembleClaude(events); errAssemble == nil {
		t.Fatal("an error event must fail the folded response")
	}
	partial := [][]byte{
		[]byte(`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[]}}`),
		[]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`),
	}
	if _, errAssemble := assembleClaude(partial); errAssemble == nil {
		t.Fatal("a message without message_stop must fail")
	}
}

func TestAssembleClaudeRejectsIncompleteToolInput(t *testing.T) {
	events := [][]byte{
		[]byte(`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[]}}`),
		[]byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"Bash","input":{}}}`),
		[]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`),
		[]byte(`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":5}}`),
		[]byte(`{"type":"message_stop"}`),
	}
	if out, errAssemble := assembleClaude(events); errAssemble == nil {
		t.Fatalf("cut-off tool input must fail instead of becoming {}: %s", out)
	}
}

func TestAssembleGeminiKeepsResponseIdentity(t *testing.T) {
	events := [][]byte{
		[]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}],"responseId":"gemini_1","modelVersion":"gemini-3-flash"}`),
		[]byte(`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"responseId":"gemini_1","modelVersion":"gemini-3-flash"}`),
	}
	out, errAssemble := assembleGemini(events)
	if errAssemble != nil {
		t.Fatal(errAssemble)
	}
	if gjson.GetBytes(out, "responseId").String() != "gemini_1" || gjson.GetBytes(out, "modelVersion").String() != "gemini-3-flash" {
		t.Fatalf("response identity dropped: %s", out)
	}
}

func TestChatRequiresFinishReason(t *testing.T) {
	events := [][]byte{
		[]byte(`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`),
		[]byte(`[DONE]`),
	}
	if out, errAssemble := assembleChat(events); errAssemble == nil {
		t.Fatalf("a Chat response without finish_reason must fail: %s", out)
	}
	converter := newStreamConverter(t.Context(), routePlan{Source: sdktranslator.FormatOpenAI, Response: sdktranslator.FormatOpenAI, Upstream: sdktranslator.FormatOpenAI}, "grok", nil, nil, nil)
	if _, errConvert := converter.convert(events[0]); errConvert != nil {
		t.Fatal(errConvert)
	}
	if _, errFinish := converter.finish(); errFinish == nil {
		t.Fatal("a Chat stream that ends without finish_reason must fail")
	}
}

func TestAssembleChatToolCalls(t *testing.T) {
	events := [][]byte{
		[]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":"}}]}}]}`),
		[]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]},"finish_reason":"tool_calls"}]}`),
		[]byte(`[DONE]`),
	}
	out, errAssemble := assembleChat(events)
	if errAssemble != nil {
		t.Fatal(errAssemble)
	}
	var response map[string]any
	_ = json.Unmarshal(out, &response)
	choice := response["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	calls := message["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	if call["id"] != "call_1" || call["function"].(map[string]any)["name"] != "Bash" {
		t.Fatalf("tool call: %v", call)
	}
	if call["function"].(map[string]any)["arguments"] != `{"command":"ls"}` {
		t.Fatalf("arguments: %v", call["function"])
	}
}

func TestStreamConverterRestoresToolNamespaces(t *testing.T) {
	plan := routePlan{Source: sdktranslator.FormatOpenAIResponse, Response: sdktranslator.FormatOpenAIResponse, Upstream: sdktranslator.FormatOpenAIResponse}
	converter := newStreamConverter(t.Context(), plan, "gpt-6.1-sol", nil, nil, map[string]namespacedTool{"sleep": {Namespace: "clock", Name: "sleep"}})
	chunks, errConvert := converter.convert([]byte(`{"type":"response.output_item.done","item":{"type":"function_call","name":"sleep","call_id":"c1","arguments":"{}"}}`))
	if errConvert != nil {
		t.Fatal(errConvert)
	}
	if len(chunks) != 1 || !strings.Contains(string(chunks[0]), `"namespace":"clock"`) {
		t.Fatalf("chunks: %q", chunks)
	}
}
