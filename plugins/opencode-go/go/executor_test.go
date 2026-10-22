package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

// clientResult is what a client of one format would read from a response.
type clientResult struct {
	Text      string
	Reasoning string
	// Terminal reports that a stream carried its format's end event.
	Terminal bool
}

func parseClientResponse(t *testing.T, format string, payload []byte) clientResult {
	t.Helper()
	if !gjson.ValidBytes(payload) {
		t.Fatalf("%s response is not JSON: %s", format, payload)
	}
	root := gjson.ParseBytes(payload)
	var out clientResult
	switch format {
	case "openai":
		if root.Get("object").String() != "chat.completion" {
			t.Fatalf("not a chat completion: %s", payload)
		}
		out.Text = root.Get("choices.0.message.content").String()
		out.Reasoning = root.Get("choices.0.message.reasoning_content").String()
	case "claude":
		if root.Get("type").String() != "message" {
			t.Fatalf("not a Messages response: %s", payload)
		}
		for _, block := range root.Get("content").Array() {
			switch block.Get("type").String() {
			case "text":
				out.Text += block.Get("text").String()
			case "thinking":
				out.Reasoning += block.Get("thinking").String()
			}
		}
	case "openai-response":
		if root.Get("object").String() != "response" {
			t.Fatalf("not a Responses object: %s", payload)
		}
		for _, item := range root.Get("output").Array() {
			switch item.Get("type").String() {
			case "message":
				for _, part := range item.Get("content").Array() {
					out.Text += part.Get("text").String()
				}
			case "reasoning":
				for _, part := range append(item.Get("summary").Array(), item.Get("content").Array()...) {
					out.Reasoning += part.Get("text").String()
				}
			}
		}
	}
	return out
}

func parseClientStream(t *testing.T, format string, chunks [][]byte) clientResult {
	t.Helper()
	var events []gjson.Result
	if format == "openai" {
		for _, chunk := range chunks {
			if !gjson.ValidBytes(chunk) {
				t.Fatalf("chat stream chunk is not bare JSON: %q", chunk)
			}
			events = append(events, gjson.ParseBytes(chunk))
		}
	} else {
		joined := bytes.Join(chunks, nil)
		for _, line := range strings.Split(string(joined), "\n") {
			if data, ok := strings.CutPrefix(line, "data:"); ok {
				data = strings.TrimSpace(data)
				if !gjson.Valid(data) {
					t.Fatalf("%s stream data is not JSON: %q", format, data)
				}
				events = append(events, gjson.Parse(data))
			}
		}
	}
	var out clientResult
	for _, event := range events {
		switch format {
		case "openai":
			out.Text += event.Get("choices.0.delta.content").String()
			out.Reasoning += event.Get("choices.0.delta.reasoning_content").String()
			if event.Get("choices.0.finish_reason").String() != "" {
				out.Terminal = true
			}
		case "claude":
			switch event.Get("type").String() {
			case "content_block_delta":
				out.Text += event.Get("delta.text").String()
				out.Reasoning += event.Get("delta.thinking").String()
			case "message_stop":
				out.Terminal = true
			}
		case "openai-response":
			switch event.Get("type").String() {
			case "response.output_text.delta":
				out.Text += event.Get("delta").String()
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				out.Reasoning += event.Get("delta").String()
			case "response.completed", "response.incomplete":
				out.Terminal = true
			}
		}
	}
	return out
}

// fixtureContent is what each recorded upstream response says.
var fixtureContent = map[string]struct {
	Text      string
	Reasoning string
}{
	"openai":          {Text: "OK", Reasoning: "The user just wants me to reply with"},
	"claude":          {Text: "", Reasoning: "The user says"},
	"openai-response": {Text: "OK", Reasoning: ""},
}

var fixtureContentNonStream = map[string]struct {
	Text      string
	Reasoning string
}{
	"openai":          {Text: "OK", Reasoning: "We need answer user asks"},
	"claude":          {Text: "", Reasoning: "The user says"},
	"openai-response": {Text: "OK", Reasoning: ""},
}

// Models of the fixture catalog, one per native protocol.
var protocolModels = map[string]string{
	"openai":          "deepseek-v4-flash",
	"claude":          "qwen3.8-max",
	"openai-response": "gpt-6-luna",
}

var protocolEndpoints = map[string]string{
	"openai":          "https://opencode.ai/inference/go/openai/v1/chat/completions",
	"claude":          "https://opencode.ai/inference/go/anthropic/v1/messages",
	"openai-response": "https://opencode.ai/inference/go/openai/v1/responses",
}

func TestExecutorRoutingMatrix(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	formats := []string{"openai", "claude", "openai-response"}
	for _, client := range formats {
		for _, protocol := range formats {
			for _, stream := range []bool{false, true} {
				name := fmt.Sprintf("%s->%s/stream=%v", client, protocol, stream)
				t.Run(name, func(t *testing.T) {
					gateway := installFakeGateway(t, fixtureGateway(t))
					model := "opencode-go/" + protocolModels[protocol]
					call := testCall{Source: client, Model: model, Stream: stream, Payload: clientPayload(client, model, stream)}
					var got clientResult
					if stream {
						raw, _ := executorExecuteStream(call.raw(t))
						result := decodeResult[rpcExecutorStreamResponse](t, raw)
						if result.Headers.Get("X-Opencode-Log-Id") != "log-test" || result.Headers.Get("Content-Type") != "" {
							t.Errorf("stream headers = %v", result.Headers)
						}
						chunks, errText := gateway.waitClosed("exec-1")
						if errText != "" {
							t.Fatalf("stream closed with %q", errText)
						}
						got = parseClientStream(t, client, chunks)
						if !got.Terminal {
							t.Errorf("stream has no terminal event: %q", bytes.Join(chunks, []byte("|")))
						}
					} else {
						raw, _ := executorExecute(call.raw(t))
						resp := decodeResult[pluginapi.ExecutorResponse](t, raw)
						if resp.Headers.Get("Set-Cookie") != "" || resp.Headers.Get("X-Opencode-Log-Id") != "log-test" {
							t.Errorf("response headers = %v", resp.Headers)
						}
						got = parseClientResponse(t, client, resp.Payload)
					}

					sent := gateway.inferenceRequests()
					if len(sent) != 1 {
						t.Fatalf("upstream requests = %d", len(sent))
					}
					checkUpstreamRequest(t, sent[0], client, protocol, stream)

					want := fixtureContentNonStream[protocol]
					if stream {
						want = fixtureContent[protocol]
					}
					if got.Text != want.Text {
						t.Errorf("text = %q, want %q", got.Text, want.Text)
					}
					if want.Reasoning == "" {
						if got.Reasoning != "" {
							t.Errorf("reasoning = %q, want none", got.Reasoning)
						}
					} else if !strings.Contains(got.Reasoning, want.Reasoning) {
						t.Errorf("reasoning = %q, want it to contain %q", got.Reasoning, want.Reasoning)
					}
				})
			}
		}
	}
}

func checkUpstreamRequest(t *testing.T, req hostHTTPRequest, client, protocol string, stream bool) {
	t.Helper()
	if req.Method != http.MethodPost || req.URL != protocolEndpoints[protocol] {
		t.Errorf("upstream %s %s", req.Method, req.URL)
	}
	if req.HostCallbackID != "cb-exec" {
		t.Errorf("host callback id = %q", req.HostCallbackID)
	}
	body := gjson.ParseBytes(req.Body)
	if !body.IsObject() {
		t.Fatalf("upstream body is not an object: %s", req.Body)
	}
	if got := body.Get("model").String(); got != protocolModels[protocol] {
		t.Errorf("upstream model = %q", got)
	}
	if got := body.Get("stream").Bool(); got != stream {
		t.Errorf("upstream stream = %v in %s", got, req.Body)
	}
	switch protocol {
	case "openai":
		if !body.Get("messages").IsArray() || body.Get("max_tokens").Int()+body.Get("max_completion_tokens").Int() != 32 {
			t.Errorf("chat body = %s", req.Body)
		}
	case "claude":
		if !body.Get("messages").IsArray() || body.Get("max_tokens").Int() != 32 {
			t.Errorf("messages body = %s", req.Body)
		}
	case "openai-response":
		if !body.Get("input").Exists() || body.Get("max_output_tokens").Int() != 32 {
			t.Errorf("responses body = %s", req.Body)
		}
	}
	if client != protocol && !strings.Contains(string(req.Body), "Be brief.") {
		t.Errorf("translated body lost the system prompt: %s", req.Body)
	}
	if req.Headers.Get("Authorization") != "Bearer "+testKeyA {
		t.Errorf("authorization = %q", req.Headers.Get("Authorization"))
	}
	if req.Headers.Get("Cookie") != "" {
		t.Error("client cookie forwarded")
	}
	wantAccept := "application/json"
	if stream {
		wantAccept = "text/event-stream"
	}
	if req.Headers.Get("Accept") != wantAccept {
		t.Errorf("accept = %q", req.Headers.Get("Accept"))
	}
	if (protocol == "claude") != (req.Headers.Get("Anthropic-Version") != "") {
		t.Errorf("anthropic-version = %q on %s", req.Headers.Get("Anthropic-Version"), protocol)
	}
}

// Passthrough keeps the client's body apart from the model ID.
func TestExecutorPassthroughKeepsBody(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	gateway := installFakeGateway(t, fixtureGateway(t))
	payload := `{"model":"opencode-go/deepseek-v4-flash","messages":[{"role":"user","content":"Reply with OK"}],"max_tokens":32,"temperature":0.2,"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"vendor_extra":{"x":1}}`
	raw, _ := executorExecute(testCall{Source: "openai", Model: "opencode-go/deepseek-v4-flash", Payload: payload}.raw(t))
	decodeResult[pluginapi.ExecutorResponse](t, raw)
	sent := gateway.inferenceRequests()[0]
	want := strings.Replace(payload, "opencode-go/deepseek-v4-flash", "deepseek-v4-flash", 1)
	if string(sent.Body) != want {
		t.Fatalf("body = %s\nwant %s", sent.Body, want)
	}
}

func TestExecutorAcceptsBareAndSuffixedModelIDs(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	for _, model := range []string{"deepseek-v4-flash", "opencode-go/deepseek-v4-flash", "opencode-go/deepseek-v4-flash(high)"} {
		gateway := installFakeGateway(t, fixtureGateway(t))
		raw, _ := executorExecute(testCall{Source: "openai", Model: model, Payload: clientPayload("openai", model, false)}.raw(t))
		decodeResult[pluginapi.ExecutorResponse](t, raw)
		if got := gjson.GetBytes(gateway.inferenceRequests()[0].Body, "model").String(); got != "deepseek-v4-flash" {
			t.Errorf("%s sent as %q", model, got)
		}
	}
}

func TestExecutorCustomPrefix(t *testing.T) {
	useSettings(t, settings{BaseURL: "https://gateway.test/inference/go", ModelPrefix: "go"})
	useTestCatalog(t)
	gateway := installFakeGateway(t, fixtureGateway(t))
	raw, _ := executorExecute(testCall{Source: "claude", Model: "go/claude-haiku-5-5", Payload: clientPayload("claude", "go/claude-haiku-5-5", false)}.raw(t))
	decodeResult[pluginapi.ExecutorResponse](t, raw)
	sent := gateway.inferenceRequests()[0]
	if sent.URL != "https://gateway.test/inference/go/anthropic/v1/messages" || gjson.GetBytes(sent.Body, "model").String() != "claude-haiku-5-5" {
		t.Fatalf("sent %s %s", sent.URL, sent.Body)
	}
}

// The executor resolves the same IDs in both modes; which of them clients
// reach is decided by the IDs model.for_auth registers.
func TestExecutorRoutesPrefixedAndBareIDsPerMode(t *testing.T) {
	on := settings{BaseURL: defaultBaseURL, ModelPrefix: "go"}
	off := on
	off.DisablePrefix = true
	tests := []struct {
		name   string
		cfg    settings
		model  string
		served bool
	}{
		{"prefix on, prefixed", on, "go/deepseek-v4-flash", true},
		{"prefix on, bare", on, "deepseek-v4-flash", true},
		{"prefix on, other prefix", on, "opencode-go/deepseek-v4-flash", false},
		{"prefix off, bare", off, "deepseek-v4-flash", true},
		{"prefix off, bare with suffix", off, "deepseek-v4-flash(high)", true},
		{"prefix off, configured prefix", off, "go/deepseek-v4-flash", true},
		{"prefix off, other prefix", off, "opencode-go/deepseek-v4-flash", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useSettings(t, tt.cfg)
			useTestCatalog(t)
			gateway := installFakeGateway(t, fixtureGateway(t))
			raw, _ := executorExecute(testCall{Source: "openai", Model: tt.model, Payload: clientPayload("openai", tt.model, false)}.raw(t))
			env := decodeEnvelope(t, raw)
			sent := gateway.inferenceRequests()
			if !tt.served {
				if env.OK || env.Error.HTTPStatus != http.StatusNotFound || len(sent) != 0 {
					t.Fatalf("envelope %+v after %d upstream requests", env.Error, len(sent))
				}
				return
			}
			if !env.OK || len(sent) != 1 || gjson.GetBytes(sent[0].Body, "model").String() != "deepseek-v4-flash" {
				t.Fatalf("envelope %+v, sent %d", env.Error, len(sent))
			}
		})
	}
}

func TestExecutorUnknownModel(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	gateway := installFakeGateway(t, fixtureGateway(t))
	raw, _ := executorExecute(testCall{Source: "openai", Model: "opencode-go/retired-model", Payload: clientPayload("openai", "x", false)}.raw(t))
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error.HTTPStatus != http.StatusNotFound {
		t.Fatalf("envelope = %+v", env.Error)
	}
	if len(gateway.inferenceRequests()) != 0 {
		t.Fatal("unknown model reached the gateway")
	}
}

func TestExecutorUsesLoginToken(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	catalogs.remember("login:"+testOrgID, fixtureConfig(t))
	gateway := installFakeGateway(t, fixtureGateway(t))
	login := loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, nil)
	raw, _ := executorExecute(testCall{Source: "openai", Model: "opencode-go/deepseek-v4-flash", Payload: clientPayload("openai", "x", false), Login: login}.raw(t))
	decodeResult[pluginapi.ExecutorResponse](t, raw)
	sent := gateway.inferenceRequests()
	if len(sent) != 1 || sent[0].Headers.Get("Authorization") != "Bearer "+testAccessToken {
		t.Fatalf("sent = %+v", sent)
	}

	// A usage block on the workspace holds after the tokens rotate.
	tracker, _, _ := useExhaustion(t, time.Now())
	tracker.block("login:"+testOrgID, "", time.Now().Add(time.Hour), []string{"weekly"})
	rotated := loginFileJSON(t, testAccessToken2, testRefreshToken2, testLoginExpiry, nil)
	raw, _ = executorExecute(testCall{Source: "openai", Model: "opencode-go/deepseek-v4-flash", Payload: clientPayload("openai", "x", false), Login: rotated}.raw(t))
	if env := decodeEnvelope(t, raw); env.OK || env.Error.HTTPStatus != http.StatusTooManyRequests || len(gateway.inferenceRequests()) != 1 {
		t.Fatalf("blocked = %s", raw)
	}
}

// A Zen model reaches the pay-as-you-go gateway under its own prefix, keeps
// working while the Go subscription quota is exhausted, and stays
// unreachable until the page has turned it on.
func TestExecutorServesZenModels(t *testing.T) {
	cfg := defaultSettings()
	cfg.ZenModels = []string{"kimi-k3", "gpt-6-luna", "claude-haiku-5-5"}
	useSettings(t, cfg)
	useTestCatalog(t)
	gateway := installFakeGateway(t, fixtureGateway(t))

	tests := []struct {
		model    string
		upstream string
		endpoint string
	}{
		{"opencode/kimi-k3", "kimi-k3", "https://opencode.ai/inference/openai/v1/chat/completions"},
		{"opencode/gpt-6-luna", "gpt-6-luna", "https://opencode.ai/inference/openai/v1/responses"},
		{"opencode/claude-haiku-5-5", "claude-haiku-5-5", "https://opencode.ai/inference/anthropic/v1/messages"},
	}
	for _, tt := range tests {
		payload := clientPayload("openai", tt.model, false)
		raw, _ := executorExecute(testCall{Source: "openai", Model: tt.model, Payload: payload}.raw(t))
		if env := decodeEnvelope(t, raw); !env.OK {
			t.Fatalf("%s = %+v", tt.model, env.Error)
		}
		sent := gateway.inferenceRequests()
		last := sent[len(sent)-1]
		if last.URL != tt.endpoint || gjson.GetBytes(last.Body, "model").String() != tt.upstream {
			t.Fatalf("%s reached %s as %s", tt.model, last.URL, gjson.GetBytes(last.Body, "model").String())
		}
	}
	if len(gateway.inferenceRequests()) != 3 {
		t.Fatalf("requests = %d", len(gateway.inferenceRequests()))
	}

	// The Go subscription quota covers Go models only, so Zen calls still go
	// through while a credential is blocked.
	tracker, _, _ := useExhaustion(t, time.Now())
	tracker.block(keyIDFor(testKeyA), authIDFor(keyFileName, keyIDFor(testKeyA)), time.Now().Add(time.Hour), []string{"weekly"})
	raw, _ := executorExecute(testCall{Source: "openai", Model: "opencode/kimi-k3", Payload: clientPayload("openai", "opencode/kimi-k3", false)}.raw(t))
	if env := decodeEnvelope(t, raw); !env.OK {
		t.Fatalf("Zen call blocked by the Go quota: %+v", env.Error)
	}
	raw, _ = executorExecute(testCall{Source: "openai", Model: "opencode-go/deepseek-v4-flash", Payload: clientPayload("openai", "opencode-go/deepseek-v4-flash", false)}.raw(t))
	if env := decodeEnvelope(t, raw); env.OK || env.Error.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("Go call not blocked: %+v", env.Error)
	}

	// A Zen model the page has not turned on does not resolve.
	raw, _ = executorExecute(testCall{Source: "openai", Model: "opencode/free-chat", Payload: clientPayload("openai", "opencode/free-chat", false)}.raw(t))
	if env := decodeEnvelope(t, raw); env.OK || env.Error.HTTPStatus != http.StatusNotFound {
		t.Fatalf("disabled Zen model = %+v", env.Error)
	}
}

func TestExecutorExpiredLoginFailsFast(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	gateway := installFakeGateway(t, fixtureGateway(t))
	expired := loginFileJSON(t, testAccessToken, testRefreshToken, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), nil)
	raw, _ := executorExecute(testCall{Source: "openai", Model: "opencode-go/deepseek-v4-flash", Payload: clientPayload("openai", "x", false), Login: expired}.raw(t))
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error.HTTPStatus != http.StatusUnauthorized || !strings.Contains(env.Error.Message, "log in again") || strings.Contains(string(raw), testAccessToken) {
		t.Fatalf("expired = %s", raw)
	}
	if len(gateway.requests) != 0 {
		t.Fatalf("expired login reached OpenCode: %+v", gateway.requests)
	}
}

func TestExecutorWithoutKey(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	installFakeGateway(t, fixtureGateway(t))
	var req rpcExecutorRequest
	_ = json.Unmarshal(testCall{Source: "openai", Model: "deepseek-v4-flash", Payload: "{}"}.raw(t), &req)
	req.StorageJSON = []byte(`{"type":"opencode-go","keys":[]}`)
	req.AuthAttributes = nil
	raw, _ := executorExecute(mustJSON(t, req))
	if env := decodeEnvelope(t, raw); env.OK || env.Error.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("envelope = %+v", env.Error)
	}
}

func TestExecutorStreamRequiresStreamID(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	installFakeGateway(t, fixtureGateway(t))
	var req rpcExecutorRequest
	_ = json.Unmarshal(testCall{Source: "openai", Model: "deepseek-v4-flash", Payload: clientPayload("openai", "deepseek-v4-flash", true)}.raw(t), &req)
	req.StreamID = ""
	raw, _ := executorExecuteStream(mustJSON(t, req))
	if env := decodeEnvelope(t, raw); env.OK || env.Error.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("envelope = %+v", env.Error)
	}
}

func TestCountTokensAndHTTPRequestAreDeclined(t *testing.T) {
	raw, _ := executorCountTokens([]byte(`{}`))
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error.HTTPStatus != http.StatusNotFound || strings.HasPrefix(strings.TrimSpace(env.Error.Message), "{") {
		t.Fatalf("count tokens = %+v", env.Error)
	}
	raw, _ = executorHTTPRequest([]byte(`{}`))
	if env := decodeEnvelope(t, raw); env.OK || env.Error.HTTPStatus != http.StatusNotImplemented {
		t.Fatalf("http request = %+v", env.Error)
	}
}
