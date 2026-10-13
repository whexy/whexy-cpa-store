package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// decodeInterceptResult unwraps the RPC envelope and decodes its result.
func decodeInterceptResult(t *testing.T, raw []byte) pluginapi.RequestInterceptResponse {
	t.Helper()
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %s", string(raw))
	}
	var resp pluginapi.RequestInterceptResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("decode intercept response: %v", errUnmarshal)
	}
	return resp
}

func assertEmptyIntercept(t *testing.T, resp pluginapi.RequestInterceptResponse) {
	t.Helper()
	if resp.Terminate || resp.StatusCode != 0 || resp.Path != "" || resp.Body != nil || resp.Headers != nil || resp.ResponseBody != nil {
		t.Fatalf("intercept response modifies the request: %+v", resp)
	}
}

func interceptRequestPayload(t *testing.T, req pluginapi.RequestInterceptRequest) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(req)
	if errMarshal != nil {
		t.Fatalf("marshal intercept request: %v", errMarshal)
	}
	return raw
}

func TestInterceptBeforeIsANoOp(t *testing.T) {
	host := newFakeHost()
	host.addProvider("p1", 1, credentialReady)
	state := newTestState(t, roundRobinConfig, host)
	withPluginState(t, state)

	// Even a group request with a session must pass through untouched and
	// record nothing: only the after-auth hook records the credential.
	payload := interceptRequestPayload(t, pluginapi.RequestInterceptRequest{
		RequestedModel: "auto-group/cycle",
		SourceFormat:   "claude",
		Headers:        http.Header{"X-Claude-Code-Session-Id": []string{"session-a"}},
		Metadata:       map[string]any{"selected_auth_id": "key-1", "selected_auth_index": "0"},
	})
	raw, errHandle := handleMethod(pluginabi.MethodRequestInterceptBefore, payload)
	if errHandle != nil {
		t.Fatalf("intercept_before: %v", errHandle)
	}
	assertEmptyIntercept(t, decodeInterceptResult(t, raw))
	if len(state.accounts) != 0 {
		t.Fatalf("intercept_before recorded %d accounts, want none", len(state.accounts))
	}
}

func TestInterceptAfterIgnoresNonGroupModel(t *testing.T) {
	host := newFakeHost()
	state := newTestState(t, roundRobinConfig, host)
	withPluginState(t, state)

	payload := interceptRequestPayload(t, pluginapi.RequestInterceptRequest{
		RequestedModel: "gpt-5.1-codex",
		SourceFormat:   "claude",
		Headers:        http.Header{"X-Claude-Code-Session-Id": []string{"session-a"}},
		Metadata:       map[string]any{"selected_auth_id": "key-1", "selected_auth_index": "0"},
	})
	raw, errHandle := handleMethod(pluginabi.MethodRequestInterceptAfter, payload)
	if errHandle != nil {
		t.Fatalf("intercept_after: %v", errHandle)
	}
	assertEmptyIntercept(t, decodeInterceptResult(t, raw))
	if len(state.accounts) != 0 {
		t.Fatalf("intercept_after recorded %d accounts, want none", len(state.accounts))
	}
}
