package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestPlanRoute(t *testing.T) {
	const (
		chat      = sdktranslator.FormatOpenAI
		responses = sdktranslator.FormatOpenAIResponse
		messages  = sdktranslator.FormatClaude
		codex     = sdktranslator.FormatCodex
	)
	cases := []struct {
		client, upstream    sdktranslator.Format
		requestTo, response sdktranslator.Format
	}{
		{chat, chat, "", ""},
		{chat, messages, messages, messages},
		{chat, responses, codex, codex},
		{messages, chat, chat, chat},
		{messages, messages, "", ""},
		{messages, responses, codex, codex},
		{responses, chat, chat, chat},
		{responses, messages, messages, messages},
		{responses, responses, "", ""},
	}
	for _, tc := range cases {
		plan, errPlan := planRoute(tc.client, tc.client, tc.upstream)
		if errPlan != nil {
			t.Fatalf("%s->%s: %v", tc.client, tc.upstream, errPlan)
		}
		if plan.RequestTo != tc.requestTo || plan.ResponseFrom != tc.response {
			t.Errorf("%s->%s = request %q response %q", tc.client, tc.upstream, plan.RequestTo, plan.ResponseFrom)
		}
	}
	if _, errPlan := planRoute("bogus", "bogus", chat); errPlan == nil {
		t.Error("unknown source format planned")
	}
}

func TestNormalizeFormat(t *testing.T) {
	for in, want := range map[string]sdktranslator.Format{
		"":                 sdktranslator.FormatOpenAI,
		"chat-completions": sdktranslator.FormatOpenAI,
		"Responses":        sdktranslator.FormatOpenAIResponse,
		"openai-response":  sdktranslator.FormatOpenAIResponse,
		"anthropic":        sdktranslator.FormatClaude,
		"claude":           sdktranslator.FormatClaude,
	} {
		if got := normalizeFormat(in); got != want {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}
}

func codexBody(t *testing.T, source sdktranslator.Format, payload string, stream bool) gjson.Result {
	t.Helper()
	plan, errPlan := planRoute(source, source, sdktranslator.FormatOpenAIResponse)
	if errPlan != nil {
		t.Fatal(errPlan)
	}
	body, errBuild := buildUpstreamBody(context.Background(), plan, []byte(payload), "gpt-6-luna", stream)
	if errBuild != nil {
		t.Fatal(errBuild)
	}
	return gjson.ParseBytes(body)
}

func TestCodexBodyFitsGoResponses(t *testing.T) {
	body := codexBody(t, sdktranslator.FormatOpenAI, `{"model":"x","messages":[{"role":"user","content":"hi"}],"max_tokens":32}`, false)
	if body.Get("max_output_tokens").Int() != 32 || body.Get("reasoning").Exists() || body.Get("parallel_tool_calls").Exists() {
		t.Errorf("chat body = %s", body.Raw)
	}
	if body.Get("stream").Bool() || body.Get("model").String() != "gpt-6-luna" || body.Get("store").Type != gjson.False || body.Get("include.0").String() != "reasoning.encrypted_content" {
		t.Errorf("chat body = %s", body.Raw)
	}

	body = codexBody(t, sdktranslator.FormatOpenAI, `{"model":"x","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":1,"reasoning_effort":"high","parallel_tool_calls":false,"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`, true)
	if body.Get("max_output_tokens").Int() != minResponsesOutputTokens || body.Get("reasoning.effort").String() != "high" || body.Get("parallel_tool_calls").Type != gjson.False || !body.Get("stream").Bool() {
		t.Errorf("chat body with options = %s", body.Raw)
	}

	// The Claude translator always asks for a stream; a buffered call must not.
	body = codexBody(t, sdktranslator.FormatClaude, `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, false)
	if body.Get("stream").Bool() || body.Get("max_output_tokens").Int() != minResponsesOutputTokens || body.Get("reasoning").Exists() {
		t.Errorf("claude body = %s", body.Raw)
	}
	body = codexBody(t, sdktranslator.FormatClaude, `{"model":"x","max_tokens":4096,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"hi"}]}`, true)
	if body.Get("reasoning.effort").String() == "" || body.Get("max_output_tokens").Int() != 4096 {
		t.Errorf("claude thinking body = %s", body.Raw)
	}
}

func TestBuildUpstreamBodyRejectsNonObject(t *testing.T) {
	plan, _ := planRoute(sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAI)
	for _, payload := range []string{``, `[]`, `not json`} {
		if _, errBuild := buildUpstreamBody(context.Background(), plan, []byte(payload), "m", false); errBuild == nil {
			t.Errorf("%q accepted", payload)
		}
	}
}

func TestCodexTerminalEvent(t *testing.T) {
	event, errEvent := codexTerminalEvent([]byte(`{"status":"incomplete","output":[]}`))
	if errEvent != nil || gjson.GetBytes(event, "type").String() != "response.incomplete" || gjson.GetBytes(event, "response.status").String() != "incomplete" {
		t.Fatalf("event = %s, %v", event, errEvent)
	}
	event, _ = codexTerminalEvent(readFixture(t, "responses_response.json"))
	if gjson.GetBytes(event, "type").String() != "response.completed" {
		t.Fatalf("event = %s", event)
	}
	if _, errEvent := codexTerminalEvent([]byte(`{"status":"failed","error":{"code":"server_error","message":"boom"}}`)); errEvent == nil || !strings.Contains(errEvent.Error(), "boom") {
		t.Fatalf("failed response: %v", errEvent)
	}
}

func TestSSESplitter(t *testing.T) {
	input := "event: a\r\ndata: {\"x\":1}\r\n\r\n: comment\n\ndata: line1\ndata: line2\n\nevent: tail\ndata: [DONE]"
	for size := 1; size <= len(input); size++ {
		var splitter sseSplitter
		var frames []sseFrame
		for start := 0; start < len(input); start += size {
			frames = append(frames, splitter.push([]byte(input[start:min(start+size, len(input))]))...)
		}
		frames = append(frames, splitter.flush()...)
		if len(frames) != 4 {
			t.Fatalf("size %d: %d frames", size, len(frames))
		}
		if frames[0].Event != "a" || string(frames[0].Data) != `{"x":1}` || string(frames[0].Raw) != "event: a\ndata: {\"x\":1}\n\n" {
			t.Fatalf("size %d: frame 0 = %+v", size, frames[0])
		}
		if frames[1].Data != nil || string(frames[1].Raw) != ": comment\n\n" {
			t.Fatalf("size %d: frame 1 = %+v", size, frames[1])
		}
		if string(frames[2].Data) != "line1\nline2" {
			t.Fatalf("size %d: frame 2 = %q", size, frames[2].Data)
		}
		if frames[3].Event != "tail" || string(frames[3].Data) != "[DONE]" {
			t.Fatalf("size %d: frame 3 = %+v", size, frames[3])
		}
	}
}

// Reasoning reaches every client format from every upstream protocol; the
// Chat and Messages fixtures are covered by TestExecutorRoutingMatrix.
func TestResponsesReasoningReachesOtherClients(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	reply := func(req hostHTTPRequest) gatewayReply {
		if req.Headers.Get("Accept") == "text/event-stream" {
			return gatewayReply{Status: 200, Body: readFixture(t, "responses_reasoning_stream.txt")}
		}
		return gatewayReply{Status: 200, Body: readFixture(t, "responses_reasoning_response.json")}
	}
	for _, client := range []string{"openai", "claude", "openai-response"} {
		for _, stream := range []bool{false, true} {
			gateway := installFakeGateway(t, reply)
			call := testCall{Source: client, Model: "opencode-go/gpt-6-luna", Stream: stream, Payload: clientPayload(client, "opencode-go/gpt-6-luna", stream)}
			var got clientResult
			if stream {
				executorExecuteStream(call.raw(t))
				chunks, errText := gateway.waitClosed("exec-1")
				if errText != "" {
					t.Fatalf("%s stream: %s", client, errText)
				}
				got = parseClientStream(t, client, chunks)
			} else {
				raw, _ := executorExecute(call.raw(t))
				got = parseClientResponse(t, client, decodeResult[pluginapi.ExecutorResponse](t, raw).Payload)
			}
			if got.Text != "OK" || strings.TrimSpace(got.Reasoning) != "Checking the instruction before answering." {
				t.Errorf("%s stream=%v: %+v", client, stream, got)
			}
		}
	}
}

// Encrypted Responses reasoning comes back to a Messages client as a signed
// thinking block, and that block goes back upstream as a reasoning item.
func TestResponsesReasoningRoundTripsThroughMessagesClient(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	gateway := installFakeGateway(t, func(hostHTTPRequest) gatewayReply {
		return gatewayReply{Status: 200, Body: readFixture(t, "responses_response.json")}
	})
	stream := string(readFixture(t, "responses_stream.txt"))
	start := strings.Index(stream, `"encrypted_content":"`) + len(`"encrypted_content":"`)
	signature := stream[start : start+strings.Index(stream[start:], `"`)]

	payload := `{"model":"opencode-go/gpt-6-luna","max_tokens":64,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[` +
		`{"role":"user","content":"Reply with OK"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"` + signature + `"},{"type":"text","text":"OK"}]},` +
		`{"role":"user","content":"Again"}]}`
	raw, _ := executorExecute(testCall{Source: "claude", Model: "opencode-go/gpt-6-luna", Payload: payload}.raw(t))
	decodeResult[pluginapi.ExecutorResponse](t, raw)
	body := gjson.ParseBytes(gateway.inferenceRequests()[0].Body)
	found := false
	for _, item := range body.Get("input").Array() {
		if item.Get("type").String() == "reasoning" && item.Get("encrypted_content").String() == signature {
			found = true
		}
	}
	if !found {
		t.Fatalf("thinking signature not replayed as a reasoning item: %s", body.Raw)
	}
}

// A Responses client's reasoning history reaches a Chat model as
// reasoning_content.
func TestResponsesReasoningHistoryReachesChatModel(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	gateway := installFakeGateway(t, fixtureGateway(t))
	payload := `{"model":"opencode-go/kimi-k3","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"Reply with OK"}]},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"Earlier reasoning."}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"Again"}]}]}`
	raw, _ := executorExecute(testCall{Source: "openai-response", Model: "opencode-go/kimi-k3", Payload: payload}.raw(t))
	decodeResult[pluginapi.ExecutorResponse](t, raw)
	body := gjson.ParseBytes(gateway.inferenceRequests()[0].Body)
	if got := body.Get(`messages.#(role=="assistant").reasoning_content`).String(); got != "Earlier reasoning." {
		t.Fatalf("assistant reasoning_content = %q in %s", got, body.Raw)
	}
}

func TestStreamConverterEndings(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	chatNoDone := strings.Replace(string(readFixture(t, "chat_stream.txt")), "data: [DONE]", "", 1)
	cases := []struct {
		name, client, model, body string
		wantErr                   string
		wantTerminal              bool
	}{
		{name: "chat without DONE to chat", client: "openai", model: "kimi-k3", body: chatNoDone, wantTerminal: true},
		{name: "chat without DONE to responses", client: "openai-response", model: "kimi-k3", body: chatNoDone, wantTerminal: true},
		{name: "truncated chat", client: "openai", model: "kimi-k3", body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"O\"}}]}\n\n", wantErr: "before the response completed"},
		{name: "truncated messages", client: "claude", model: "qwen3.8-max", body: strings.Split(string(readFixture(t, "messages_stream.txt")), "event: message_delta")[0], wantErr: "before the response completed"},
		{name: "chat error payload", client: "claude", model: "kimi-k3", body: "data: {\"error\":{\"type\":\"server_error\",\"message\":\"upstream exploded\"}}\n\n", wantErr: "upstream exploded"},
		{name: "messages error to chat", client: "openai", model: "qwen3.8-max", body: "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n", wantErr: "overloaded_error: Overloaded"},
		{name: "messages error to messages", client: "claude", model: "qwen3.8-max", body: "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"},
		{name: "responses failed to messages", client: "claude", model: "gpt-6-luna", body: "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"model crashed\"}}}\n\n", wantErr: "server_error: model crashed"},
		{name: "responses failed to responses", client: "openai-response", model: "gpt-6-luna", body: "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"model crashed\"}}}\n\n"},
		{name: "bare JSON error body", client: "openai", model: "kimi-k3", body: "{\"error\":{\"type\":\"server_error\",\"message\":\"not a stream\"}}", wantErr: "not a stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gateway := installFakeGateway(t, func(hostHTTPRequest) gatewayReply { return gatewayReply{Status: 200, Body: []byte(tc.body)} })
			model := "opencode-go/" + tc.model
			raw, _ := executorExecuteStream(testCall{Source: tc.client, Model: model, Stream: true, Payload: clientPayload(tc.client, model, true)}.raw(t))
			decodeResult[rpcExecutorStreamResponse](t, raw)
			chunks, errText := gateway.waitClosed("exec-1")
			if tc.wantErr == "" && errText != "" {
				t.Fatalf("closed with %q", errText)
			}
			if tc.wantErr != "" && !strings.Contains(errText, tc.wantErr) {
				t.Fatalf("closed with %q, want %q", errText, tc.wantErr)
			}
			if tc.wantTerminal && !parseClientStream(t, tc.client, chunks).Terminal {
				t.Fatalf("no terminal event in %q", chunks)
			}
		})
	}
}

func TestStreamStopsWhenClientLeaves(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	gateway := installFakeGateway(t, fixtureGateway(t))
	gateway.readSize = 1 << 20
	gateway.fake.handle(pluginabi.MethodHostStreamEmit, func(json.RawMessage) (any, error) {
		return nil, &hostCallError{Message: "stream exec-1 is not open"}
	})
	upstreamClosed := make(chan struct{}, 1)
	gateway.fake.handle(pluginabi.MethodHostHTTPStreamClose, func(json.RawMessage) (any, error) {
		upstreamClosed <- struct{}{}
		return struct{}{}, nil
	})
	executorExecuteStream(testCall{Source: "openai", Model: "opencode-go/kimi-k3", Stream: true, Payload: clientPayload("openai", "opencode-go/kimi-k3", true)}.raw(t))
	gateway.waitClosed("exec-1")
	<-upstreamClosed
	if reads := len(gateway.fake.callsTo(pluginabi.MethodHostHTTPStreamRead)); reads != 1 {
		t.Fatalf("kept reading after the client left: %d reads", reads)
	}
}

func TestStreamReadErrorEndsStream(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	gateway := installFakeGateway(t, fixtureGateway(t))
	gateway.fake.handle(pluginabi.MethodHostHTTPStreamRead, func(json.RawMessage) (any, error) {
		return hostStreamReadResponse{Payload: []byte("data: {\"choices\":[]}\n\n"), Error: "connection reset", Done: true}, nil
	})
	executorExecuteStream(testCall{Source: "openai", Model: "opencode-go/kimi-k3", Stream: true, Payload: clientPayload("openai", "opencode-go/kimi-k3", true)}.raw(t))
	if _, errText := gateway.waitClosed("exec-1"); !strings.Contains(errText, "connection reset") {
		t.Fatalf("closed with %q", errText)
	}
}
