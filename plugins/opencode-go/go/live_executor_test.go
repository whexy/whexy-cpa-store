//go:build live

package main

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// Live executor tests send one tiny inference request each ("Reply with
// OK", at most 32 output tokens) through the executor to the Go gateway.
// The e2e smoke script covers the routes not listed here.

// liveGateway forwards the executor's upstream requests to opencode.ai,
// refusing anything outside the Go gateway, which would bill Console
// credits instead of the subscription.
func liveGateway(t *testing.T) func(req hostHTTPRequest) gatewayReply {
	return func(req hostHTTPRequest) gatewayReply {
		if !strings.HasPrefix(req.URL, defaultBaseURL+"/") {
			t.Errorf("refusing non-Go URL %s", req.URL)
			return gatewayReply{Status: 599}
		}
		resp, errDo := netHTTPDoer{}.Do(context.Background(), pluginapi.HTTPRequest{Method: req.Method, URL: req.URL, Headers: req.Headers, Body: req.Body})
		if errDo != nil {
			t.Errorf("%s: %s", req.URL, redactSecrets(errDo.Error()))
			return gatewayReply{Status: 599}
		}
		return gatewayReply{Status: resp.StatusCode, Headers: resp.Headers, Body: resp.Body}
	}
}

func useLiveCatalog(t *testing.T, key string) {
	t.Helper()
	store := useCatalogStore(t)
	cat, errFetch := fetchCatalog(context.Background(), netHTTPDoer{}, defaultSettings(), key)
	if errFetch != nil {
		t.Fatal(redactSecrets(errFetch.Error()))
	}
	store.byKey[keyIDFor(key)] = catalogEntry{catalog: cat, fetchedAt: time.Now()}
}

// liveFreeZenModel is the first free OpenCode Zen model the workspace serves.
// The cloak exists for these, so a workspace without one has nothing to test.
func liveFreeZenModel(t *testing.T, cat catalog) string {
	t.Helper()
	for _, model := range cat.Zen {
		if model.Free {
			return model.UpstreamID
		}
	}
	t.Skip("the workspace serves no free OpenCode Zen model")
	return ""
}

// TestLiveExecutorCloakedFreeModel sends one tiny request through the executor
// to a free Zen model, streaming and buffered. Without the cloak the gateway
// answers both with 403 FreeTierError.
func TestLiveExecutorCloakedFreeModel(t *testing.T) {
	key := liveKey(t)
	cat, errFetch := fetchCatalog(context.Background(), netHTTPDoer{}, defaultSettings(), key)
	if errFetch != nil {
		t.Fatal(redactSecrets(errFetch.Error()))
	}
	model := liveFreeZenModel(t, cat)
	cfg := defaultSettings()
	cfg.ZenModels = []string{model}
	useSettings(t, cfg)
	useLiveCatalog(t, key)
	useExhaustion(t, time.Now())
	gateway := installFakeGateway(t, func(req hostHTTPRequest) gatewayReply {
		if !strings.HasPrefix(req.URL, defaultZenBaseURL+"/") {
			t.Errorf("refusing non-Zen URL %s", req.URL)
			return gatewayReply{Status: 599}
		}
		resp, errDo := netHTTPDoer{}.Do(context.Background(), pluginapi.HTTPRequest{Method: req.Method, URL: req.URL, Headers: req.Headers, Body: req.Body})
		if errDo != nil {
			t.Errorf("%s: %s", req.URL, redactSecrets(errDo.Error()))
			return gatewayReply{Status: 599}
		}
		return gatewayReply{Status: resp.StatusCode, Headers: resp.Headers, Body: resp.Body}
	})
	gateway.readSize = 512
	ref := "opencode/" + model

	for _, stream := range []bool{false, true} {
		name := "buffered"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			// A free model reasons about the placeholder before answering, so
			// the budget is roomier than the other live calls.
			payload := `{"model":"` + ref + `","messages":[{"role":"user","content":"Reply with OK"}],"max_tokens":64,"stream":` + strconv.FormatBool(stream) + `}`
			call := testCall{Source: "openai", Model: ref, Stream: stream, APIKey: key, Payload: payload}
			if stream {
				raw, _ := executorExecuteStream(call.raw(t))
				decodeResult[rpcExecutorStreamResponse](t, raw)
				chunks, errText := gateway.waitClosed("exec-1")
				if errText != "" {
					t.Fatalf("stream closed with %q", errText)
				}
				got := parseClientStream(t, "openai", chunks)
				if !got.Terminal || (got.Text == "" && got.Reasoning == "") {
					t.Fatalf("stream = %+v", got)
				}
				t.Logf("stream: text %q, reasoning %d chars", got.Text, len(got.Reasoning))
				return
			}
			raw, _ := executorExecute(call.raw(t))
			env := decodeEnvelope(t, raw)
			if !env.OK {
				t.Fatalf("error %d: %s", env.Error.HTTPStatus, env.Error.Message)
			}
			resp := decodeResult[pluginapi.ExecutorResponse](t, raw)
			got := parseClientResponse(t, "openai", resp.Payload)
			if got.Text == "" && got.Reasoning == "" {
				t.Fatalf("response = %+v", got)
			}
			t.Logf("buffered: text %q, reasoning %d chars", got.Text, len(got.Reasoning))
		})
	}
}

// liveModelFor is the first model of the workspace's Go catalog that speaks
// one protocol. The live tests name no model directly: OpenCode retires Go
// models, and a hardcoded ID turns that into a failing test.
func liveModelFor(t *testing.T, cat catalog, protocol sdktranslator.Format) string {
	t.Helper()
	for _, model := range cat.Models {
		if model.Protocol == protocol {
			return model.UpstreamID
		}
	}
	t.Skipf("the workspace serves no %s Go model", protocol)
	return ""
}

func TestLiveExecutorCrossProtocol(t *testing.T) {
	key := liveKey(t)
	cat, errFetch := fetchCatalog(context.Background(), netHTTPDoer{}, defaultSettings(), key)
	if errFetch != nil {
		t.Fatal(redactSecrets(errFetch.Error()))
	}
	responsesModel := liveModelFor(t, cat, sdktranslator.FormatOpenAIResponse)
	messagesModel := liveModelFor(t, cat, sdktranslator.FormatClaude)
	useSettings(t, defaultSettings())
	useLiveCatalog(t, key)
	useExhaustion(t, time.Now())
	cases := []struct {
		client, model string
		stream        bool
		payload       string
	}{
		{client: "openai", model: responsesModel},
		{client: "openai", model: responsesModel, stream: true},
		{client: "openai-response", model: messagesModel},
		// Claude Code probes with max_tokens 1; Responses models need 16.
		{client: "claude", model: responsesModel, payload: `{"model":"opencode-go/` + responsesModel + `","max_tokens":1,"messages":[{"role":"user","content":"Reply with OK"}]}`},
	}
	for _, tc := range cases {
		name := tc.client + "->" + tc.model
		if tc.stream {
			name += "/stream"
		}
		t.Run(name, func(t *testing.T) {
			gateway := installFakeGateway(t, liveGateway(t))
			gateway.readSize = 512
			model := "opencode-go/" + tc.model
			payload := tc.payload
			if payload == "" {
				payload = clientPayload(tc.client, model, tc.stream)
			}
			call := testCall{Source: tc.client, Model: model, Stream: tc.stream, APIKey: key, Payload: payload}
			if tc.stream {
				raw, _ := executorExecuteStream(call.raw(t))
				decodeResult[rpcExecutorStreamResponse](t, raw)
				chunks, errText := gateway.waitClosed("exec-1")
				if errText != "" {
					t.Fatalf("stream closed with %q", errText)
				}
				got := parseClientStream(t, tc.client, chunks)
				t.Logf("text %q, reasoning %d chars, terminal %v", got.Text, len(got.Reasoning), got.Terminal)
				if !got.Terminal {
					t.Fatal("stream has no terminal event")
				}
				return
			}
			raw, _ := executorExecute(call.raw(t))
			env := decodeEnvelope(t, raw)
			if !env.OK {
				t.Fatalf("error %d: %s", env.Error.HTTPStatus, env.Error.Message)
			}
			resp := decodeResult[pluginapi.ExecutorResponse](t, raw)
			got := parseClientResponse(t, tc.client, resp.Payload)
			t.Logf("text %q, reasoning %d chars, log id %s", got.Text, len(got.Reasoning), resp.Headers.Get("X-Opencode-Log-Id"))
		})
	}
}

// TestLiveExecutorFreeModelWithHarnessTools checks the harnesses the cloak
// exists for. Pi declares its own "read", so the plugin adds nothing and Pi's
// tool survives; Codex declares none, so the placeholder is added and the
// model uses Codex's own tools instead of it.
func TestLiveExecutorFreeModelWithHarnessTools(t *testing.T) {
	key := liveKey(t)
	cat, errFetch := fetchCatalog(context.Background(), netHTTPDoer{}, defaultSettings(), key)
	if errFetch != nil {
		t.Fatal(redactSecrets(errFetch.Error()))
	}
	model := liveFreeZenModel(t, cat)
	cfg := defaultSettings()
	cfg.ZenModels = []string{model}
	useSettings(t, cfg)
	useLiveCatalog(t, key)
	useExhaustion(t, time.Now())
	gateway := installFakeGateway(t, func(req hostHTTPRequest) gatewayReply {
		if !strings.HasPrefix(req.URL, defaultZenBaseURL+"/") {
			t.Errorf("refusing non-Zen URL %s", req.URL)
			return gatewayReply{Status: 599}
		}
		resp, errDo := netHTTPDoer{}.Do(context.Background(), pluginapi.HTTPRequest{Method: req.Method, URL: req.URL, Headers: req.Headers, Body: req.Body})
		if errDo != nil {
			t.Errorf("%s: %s", req.URL, redactSecrets(errDo.Error()))
			return gatewayReply{Status: 599}
		}
		return gatewayReply{Status: resp.StatusCode, Headers: resp.Headers, Body: resp.Body}
	})
	gateway.readSize = 512
	ref := "opencode/" + model
	const prompt = "Read the file /etc/hostname and tell me what is in it."

	// Pi: read, bash, edit, write, exactly as its own client declares them.
	piRead := `{"type":"function","function":{"name":"read","description":"Pi's own read tool.","parameters":{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["path"]}}}`
	piTools := `[` + piRead + `,{"type":"function","function":{"name":"bash","description":"b","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}},{"type":"function","function":{"name":"edit","description":"e","parameters":{"type":"object","properties":{}}}},{"type":"function","function":{"name":"write","description":"w","parameters":{"type":"object","properties":{}}}}]`
	piPayload := `{"model":"` + ref + `","messages":[{"role":"user","content":` + strconv.Quote(prompt) + `}],"tools":` + piTools + `,"stream":false}`
	raw, _ := executorExecute(testCall{Source: "openai", Model: ref, Payload: piPayload, APIKey: key}.raw(t))
	if env := decodeEnvelope(t, raw); !env.OK {
		t.Fatalf("pi: %d %s", env.Error.HTTPStatus, env.Error.Message)
	}
	piSent := gateway.inferenceRequests()[0]
	if got := len(gjson.GetBytes(piSent.Body, "tools").Array()); got != 4 {
		t.Fatalf("pi tools = %s", gjson.GetBytes(piSent.Body, "tools").Raw)
	}
	if got := gjson.GetBytes(piSent.Body, "tools.0.function.description").String(); got != "Pi's own read tool." {
		t.Fatalf("pi's read tool was rewritten: %q", got)
	}
	t.Logf("pi answered: %s", decodeResult[pluginapi.ExecutorResponse](t, raw).Payload)

	// Codex: shell and apply_patch, and no read tool at all.
	codexTools := `[{"type":"function","name":"shell","description":"Runs a command","parameters":{"type":"object","properties":{"command":{"type":"array","items":{"type":"string"}}},"required":["command"]}},{"type":"function","name":"apply_patch","description":"Edits a file","parameters":{"type":"object","properties":{"patch":{"type":"string"}},"required":["patch"]}}]`
	codexPayload := `{"model":"` + ref + `","instructions":"Be brief.","input":` + strconv.Quote(prompt) + `,"tools":` + codexTools + `,"stream":false}`
	raw, _ = executorExecute(testCall{Source: "openai-response", Model: ref, Payload: codexPayload, APIKey: key}.raw(t))
	if env := decodeEnvelope(t, raw); !env.OK {
		t.Fatalf("codex: %d %s", env.Error.HTTPStatus, env.Error.Message)
	}
	codexSent := gateway.inferenceRequests()[1]
	codexUpstream := gjson.GetBytes(codexSent.Body, "tools").Array()
	if len(codexUpstream) != 3 {
		t.Fatalf("codex tools = %s", gjson.GetBytes(codexSent.Body, "tools").Raw)
	}
	if got := codexUpstream[2].Get("function.name").String(); got != cloakToolName {
		t.Fatalf("the placeholder is missing: %s", gjson.GetBytes(codexSent.Body, "tools").Raw)
	}
	if got := codexUpstream[2].Get("function.description").String(); got != cloakToolDescription {
		t.Fatalf("placeholder description = %q", got)
	}
	codexAnswer := decodeResult[pluginapi.ExecutorResponse](t, raw).Payload
	for _, item := range gjson.GetBytes(codexAnswer, "output").Array() {
		if item.Get("type").String() == "function_call" && item.Get("name").String() == cloakToolName {
			t.Fatalf("the model called the placeholder: %s", codexAnswer)
		}
	}
	t.Logf("codex answered: %s", codexAnswer)
}
