package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPluginRegistrationDeclaresRequestInterceptor(t *testing.T) {
	registration := pluginRegistration()
	if registration.SchemaVersion != pluginSchemaVersion || !registration.Capabilities.RequestInterceptor {
		t.Fatalf("registration = %+v", registration)
	}
	if registration.Metadata.Version != pluginVersion {
		t.Fatalf("version = %q, want %q", registration.Metadata.Version, pluginVersion)
	}
}

func TestInterceptBeforeAuth(t *testing.T) {
	tests := []struct {
		name          string
		sourceFormat  string
		headers       http.Header
		wantTerminate bool
	}{
		{name: "responses with header", sourceFormat: "openai-response", headers: http.Header{"Whexy_cpa_disable_response_api": {""}}, wantTerminate: true},
		{name: "responses with raw header spelling", sourceFormat: "openai-response", headers: http.Header{"WHEXY_CPA_DISABLE_RESPONSE_API": {"1"}}, wantTerminate: true},
		{name: "responses with hyphenated header", sourceFormat: "openai-response", headers: http.Header{"Whexy-Cpa-Disable-Response-Api": {"x"}}, wantTerminate: true},
		{name: "responses without header", sourceFormat: "openai-response", headers: http.Header{"Authorization": {"Bearer k"}}},
		{name: "chat completions with header", sourceFormat: "openai", headers: http.Header{"Whexy_cpa_disable_response_api": {""}}},
		{name: "claude with header", sourceFormat: "claude", headers: http.Header{"Whexy_cpa_disable_response_api": {""}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, errMarshal := json.Marshal(pluginapi.RequestInterceptRequest{
				SourceFormat: tt.sourceFormat,
				Headers:      tt.headers,
				Body:         []byte(`{"model":"gpt-5.4"}`),
			})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			out, errHandle := handleMethod(pluginabi.MethodRequestInterceptBefore, raw)
			if errHandle != nil {
				t.Fatal(errHandle)
			}
			var env envelope
			if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil || !env.OK {
				t.Fatalf("envelope = %s, err = %v", out, errUnmarshal)
			}
			var resp pluginapi.RequestInterceptResponse
			if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if resp.Terminate != tt.wantTerminate {
				t.Fatalf("terminate = %v, want %v", resp.Terminate, tt.wantTerminate)
			}
			if tt.wantTerminate && (resp.StatusCode != http.StatusNotFound || len(resp.ResponseBody) != 0) {
				t.Fatalf("response = %+v, want 404 with empty body", resp)
			}
			if !tt.wantTerminate && (resp.Headers != nil || len(resp.Body) != 0) {
				t.Fatalf("pass-through response modified request: %+v", resp)
			}
		})
	}
}

func TestInterceptBeforeAuthBlocksConfiguredAPIKeys(t *testing.T) {
	registerRequest, errMarshal := json.Marshal(lifecycleRequest{
		ConfigYAML: []byte("enabled: true\npriority: 0\napi_keys:\n  - n8n-key\n  - \" padded-key \"\n  - \"\"\n"),
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if _, errRegister := handleMethod(pluginabi.MethodPluginRegister, registerRequest); errRegister != nil {
		t.Fatal(errRegister)
	}
	t.Cleanup(func() { blockedAPIKeys.Store(nil) })

	tests := []struct {
		name          string
		sourceFormat  string
		headers       http.Header
		wantTerminate bool
	}{
		{name: "bearer key", sourceFormat: "openai-response", headers: http.Header{"Authorization": {"Bearer n8n-key"}}, wantTerminate: true},
		{name: "lowercase bearer scheme", sourceFormat: "openai-response", headers: http.Header{"Authorization": {"bearer n8n-key"}}, wantTerminate: true},
		{name: "trimmed configured key", sourceFormat: "openai-response", headers: http.Header{"Authorization": {"Bearer padded-key"}}, wantTerminate: true},
		{name: "x-api-key", sourceFormat: "openai-response", headers: http.Header{"X-Api-Key": {"n8n-key"}}, wantTerminate: true},
		{name: "x-goog-api-key", sourceFormat: "openai-response", headers: http.Header{"X-Goog-Api-Key": {"n8n-key"}}, wantTerminate: true},
		{name: "other key", sourceFormat: "openai-response", headers: http.Header{"Authorization": {"Bearer codex-key"}}},
		{name: "no key", sourceFormat: "openai-response", headers: http.Header{}},
		{name: "blocked key on chat completions", sourceFormat: "openai", headers: http.Header{"Authorization": {"Bearer n8n-key"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := decide(pluginapi.RequestInterceptRequest{SourceFormat: tt.sourceFormat, Headers: tt.headers})
			if resp.Terminate != tt.wantTerminate {
				t.Fatalf("terminate = %v, want %v", resp.Terminate, tt.wantTerminate)
			}
			if tt.wantTerminate && (resp.StatusCode != http.StatusNotFound || len(resp.ResponseBody) != 0) {
				t.Fatalf("response = %+v, want 404 with empty body", resp)
			}
		})
	}
}

func TestReconfigureReplacesAPIKeys(t *testing.T) {
	t.Cleanup(func() { blockedAPIKeys.Store(nil) })
	for _, cfg := range []string{"api_keys: [old-key]\n", "api_keys: [new-key]\n"} {
		raw, errMarshal := json.Marshal(lifecycleRequest{ConfigYAML: []byte(cfg)})
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		if _, errConfigure := handleMethod(pluginabi.MethodPluginReconfigure, raw); errConfigure != nil {
			t.Fatal(errConfigure)
		}
	}
	responses := func(key string) pluginapi.RequestInterceptRequest {
		return pluginapi.RequestInterceptRequest{SourceFormat: "openai-response", Headers: http.Header{"Authorization": {"Bearer " + key}}}
	}
	if decide(responses("old-key")).Terminate {
		t.Fatal("old key still blocked after reconfigure")
	}
	if !decide(responses("new-key")).Terminate {
		t.Fatal("new key not blocked after reconfigure")
	}
}
