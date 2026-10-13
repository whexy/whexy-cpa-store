//go:build live

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
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

func TestLiveExecutorCrossProtocol(t *testing.T) {
	key := liveKey(t)
	useSettings(t, defaultSettings())
	useLiveCatalog(t, key)
	useExhaustion(t, time.Now())
	cases := []struct {
		client, model string
		stream        bool
		payload       string
	}{
		{client: "openai", model: "gpt-6-luna"},
		{client: "openai", model: "gpt-6-luna", stream: true},
		{client: "openai-response", model: "minimax-m2.7"},
		// Claude Code probes with max_tokens 1; Responses models need 16.
		{client: "claude", model: "gpt-6-luna", payload: `{"model":"opencode-go/gpt-6-luna","max_tokens":1,"messages":[{"role":"user","content":"Reply with OK"}]}`},
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
