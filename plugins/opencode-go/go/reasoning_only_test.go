package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

// reasoningOnlyFixtures are upstream responses that spent the whole token
// limit on reasoning, so they carry no text: the Chat stream is recorded
// from Go (deepseek-v4.1-flash, max_tokens 32).
var reasoningOnlyFixtures = map[string][2]string{
	"openai":          {"chat_reasoning_only_response.json", "chat_reasoning_only_stream.txt"},
	"claude":          {"messages_response.json", "messages_stream.txt"},
	"openai-response": {"responses_reasoning_only_response.json", "responses_reasoning_only_stream.txt"},
}

// sseEvents returns the data payloads of an SSE byte stream.
func sseEvents(t *testing.T, chunks [][]byte) []gjson.Result {
	t.Helper()
	var events []gjson.Result
	for _, line := range strings.Split(string(bytes.Join(chunks, nil)), "\n") {
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			events = append(events, gjson.Parse(strings.TrimSpace(data)))
		}
	}
	return events
}

// A response cut off by the token limit while still reasoning ends every
// client format with its "stopped at the limit" terminal state.
func TestReasoningOnlyOutputEndsWithTokenLimit(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	for upstream, names := range reasoningOnlyFixtures {
		for _, client := range []string{"openai", "claude", "openai-response"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s->%s/stream=%v", upstream, client, stream), func(t *testing.T) {
					body := readFixture(t, names[0])
					if stream {
						body = readFixture(t, names[1])
					}
					gateway := installFakeGateway(t, func(hostHTTPRequest) gatewayReply { return gatewayReply{Status: 200, Body: body} })
					model := "opencode-go/" + protocolModels[upstream]
					call := testCall{Source: client, Model: model, Stream: stream, Payload: clientPayload(client, model, stream)}
					if !stream {
						raw, _ := executorExecute(call.raw(t))
						checkReasoningOnlyResponse(t, client, decodeResult[pluginapi.ExecutorResponse](t, raw).Payload)
						return
					}
					raw, _ := executorExecuteStream(call.raw(t))
					decodeResult[rpcExecutorStreamResponse](t, raw)
					chunks, errText := gateway.waitClosed("exec-1")
					if errText != "" {
						t.Fatalf("stream closed with %q", errText)
					}
					checkReasoningOnlyStream(t, client, chunks)
				})
			}
		}
	}
}

func checkReasoningOnlyResponse(t *testing.T, client string, payload []byte) {
	t.Helper()
	root := gjson.ParseBytes(payload)
	got := parseClientResponse(t, client, payload)
	if got.Text != "" || got.Reasoning == "" {
		t.Errorf("content = %+v", got)
	}
	switch client {
	case "openai":
		if reason := root.Get("choices.0.finish_reason").String(); reason != "length" {
			t.Errorf("finish_reason = %q", reason)
		}
	case "claude":
		if reason := root.Get("stop_reason").String(); reason != "max_tokens" {
			t.Errorf("stop_reason = %q", reason)
		}
	case "openai-response":
		if root.Get("status").String() != "incomplete" || root.Get("incomplete_details.reason").String() != "max_output_tokens" {
			t.Errorf("status = %s, incomplete_details = %s", root.Get("status").Raw, root.Get("incomplete_details").Raw)
		}
	}
}

func checkReasoningOnlyStream(t *testing.T, client string, chunks [][]byte) {
	t.Helper()
	got := parseClientStream(t, client, chunks)
	if !got.Terminal || got.Text != "" || got.Reasoning == "" {
		t.Fatalf("stream = %+v", got)
	}
	switch client {
	case "openai":
		last := gjson.ParseBytes(chunks[len(chunks)-1])
		reasons := ""
		for _, chunk := range chunks {
			reasons += gjson.GetBytes(chunk, "choices.0.finish_reason").String()
		}
		if reasons != "length" {
			t.Errorf("finish reasons = %q (last chunk %s)", reasons, last.Raw)
		}
	case "claude":
		stop := ""
		for _, event := range sseEvents(t, chunks) {
			if event.Get("type").String() == "message_delta" {
				stop = event.Get("delta.stop_reason").String()
			}
		}
		if stop != "max_tokens" {
			t.Errorf("stop_reason = %q", stop)
		}
	case "openai-response":
		events := sseEvents(t, chunks)
		last := events[len(events)-1]
		if last.Get("type").String() != "response.incomplete" || last.Get("response.status").String() != "incomplete" || last.Get("response.incomplete_details.reason").String() != "max_output_tokens" {
			t.Fatalf("terminal event = %s", last.Raw)
		}
		response := last.Get("response")
		if response.Get("id").String() != events[0].Get("response.id").String() || response.Get("id").String() == "" || response.Get("created_at").Int() == 0 || response.Get("model").String() == "" {
			t.Errorf("terminal response identity = %s, created = %s", response.Raw, events[0].Raw)
		}
		if response.Get(`output.#(type=="reasoning")`).Raw == "" || response.Get("usage.output_tokens").Int() == 0 || response.Get("usage.input_tokens").Int() == 0 {
			t.Errorf("terminal response = %s", response.Raw)
		}
		previous := int64(-1)
		for _, event := range events {
			sequence := event.Get("sequence_number").Int()
			if sequence <= previous {
				t.Fatalf("sequence_number %d after %d in %s", sequence, previous, event.Raw)
			}
			previous = sequence
		}
	}
}

// The supplied terminal event follows the upstream stop reason, and a
// terminal event the translator emitted itself is never doubled.
func TestResponsesTerminalEventIsSuppliedOnce(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	stopped := bytes.ReplaceAll(readFixture(t, "chat_reasoning_only_stream.txt"), []byte(`"finish_reason":"length"`), []byte(`"finish_reason":"stop"`))
	cases := map[string]struct {
		body   []byte
		status string
	}{
		"reasoning only, stop":   {stopped, "completed"},
		"reasoning only, length": {readFixture(t, "chat_reasoning_only_stream.txt"), "incomplete"},
		"text":                   {readFixture(t, "chat_stream.txt"), "completed"},
	}
	for name, tc := range cases {
		gateway := installFakeGateway(t, func(hostHTTPRequest) gatewayReply { return gatewayReply{Status: 200, Body: tc.body} })
		model := "opencode-go/kimi-k3"
		raw, _ := executorExecuteStream(testCall{Source: "openai-response", Model: model, Stream: true, Payload: clientPayload("openai-response", model, true)}.raw(t))
		decodeResult[rpcExecutorStreamResponse](t, raw)
		chunks, errText := gateway.waitClosed("exec-1")
		if errText != "" {
			t.Fatalf("%s: closed with %q", name, errText)
		}
		var terminals []gjson.Result
		for _, event := range sseEvents(t, chunks) {
			switch event.Get("type").String() {
			case "response.completed", "response.incomplete", "response.failed":
				terminals = append(terminals, event)
			}
		}
		if len(terminals) != 1 || terminals[0].Get("response.status").String() != tc.status {
			t.Fatalf("%s: terminal events = %v", name, terminals)
		}
		if tc.status == "completed" && terminals[0].Get("response.incomplete_details").Type != gjson.Null && terminals[0].Get("response.incomplete_details").Exists() {
			t.Errorf("%s: incomplete_details = %s", name, terminals[0].Get("response.incomplete_details").Raw)
		}
	}
}
