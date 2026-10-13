package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Model lists as the host's list handlers write them.
const (
	openAIList = `{"data":[{"created":1,"id":"gpt-5.4","object":"model","owned_by":"openai"},{"id":"o3","object":"model"},{"id":"claude-sonnet-4-5","object":"model"},{"id":"gemini-3-pro-preview","object":"model"}],"object":"list"}`
	grokList   = `{"object":"list","data":[{"id":"gpt-5.4","model":"gpt-5.4","name":"GPT 5.4","api_backend":"responses","supported_in_api":true},{"id":"grok-4","model":"grok-4","name":"Grok 4","api_backend":"responses","supported_in_api":true}]}`
	// claudeList shows gpt-5.4 and o3 encoded, as Claude clients see them.
	claudeList  = `{"data":[{"display_name":"Claude Sonnet 4.5","id":"claude-sonnet-4-5","type":"model"},{"display_name":"gpt-5.4","id":"claude-fable-5-dd-4.5-tpg","type":"model"},{"display_name":"o3","id":"claude-fable-5-dd-3o","type":"model"}],"first_id":"claude-sonnet-4-5","has_more":false,"last_id":"claude-fable-5-dd-3o"}`
	codexList   = `{"models":[{"slug":"gpt-5.4","display_name":"GPT-5.4","priority":1},{"slug":"o3","display_name":"o3"},{"slug":"gpt-5.4-codex","display_name":"GPT-5.4 Codex"}]}`
	geminiList  = `{"models":[{"description":"g","displayName":"Gemini 3 Pro Preview","name":"models/gemini-3-pro-preview","supportedGenerationMethods":["generateContent"]},{"displayName":"Gemini 2.5 Pro","name":"models/gemini-2.5-pro"}]}`
	unknownList = `{"items":[{"id":"o3"}]}`
)

func listCall(format, body string, headers http.Header) pluginapi.ResponseInterceptRequest {
	return pluginapi.ResponseInterceptRequest{
		RequestID:       "req-1",
		SourceFormat:    format,
		RequestHeaders:  headers,
		ResponseHeaders: http.Header{"Content-Type": {"application/json; charset=utf-8"}},
		Body:            []byte(body),
		StatusCode:      http.StatusOK,
	}
}

func bearer(key string) http.Header {
	return http.Header{"Authorization": {"Bearer " + key}}
}

// interceptList runs response.intercept_after through the ABI dispatch and
// returns the body the client gets.
func interceptList(t *testing.T, req pluginapi.ResponseInterceptRequest) string {
	t.Helper()
	raw, errHandle := handleMethod(pluginabi.MethodResponseInterceptAfter, mustJSON(t, req))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	resp := decodeResult[pluginapi.ResponseInterceptResponse](t, raw)
	if resp.Headers != nil || resp.ClearHeaders != nil {
		t.Fatalf("list response changes headers: %+v", resp)
	}
	if len(resp.Body) == 0 {
		return string(req.Body)
	}
	return string(resp.Body)
}

func TestModelListFiltering(t *testing.T) {
	useEmptyCatalog(t)
	useConfig(t, fixtureConfig)
	cursor, n8n := bearer(cursorKey), bearer(n8nKey)
	tests := []struct {
		name    string
		format  string
		body    string
		headers http.Header
		want    string
	}{
		{
			name: "openai allow list", format: "openai", body: openAIList, headers: cursor,
			want: `{"data":[{"created":1,"id":"gpt-5.4","object":"model","owned_by":"openai"},{"id":"claude-sonnet-4-5","object":"model"}],"object":"list"}`,
		},
		{
			name: "openai deny list", format: "openai", body: openAIList, headers: n8n,
			want: `{"data":[{"created":1,"id":"gpt-5.4","object":"model","owned_by":"openai"},{"id":"o3","object":"model"},{"id":"claude-sonnet-4-5","object":"model"}],"object":"list"}`,
		},
		{
			name: "grok shell", format: "openai", body: grokList, headers: cursor,
			want: `{"object":"list","data":[{"id":"gpt-5.4","model":"gpt-5.4","name":"GPT 5.4","api_backend":"responses","supported_in_api":true}]}`,
		},
		{
			name: "claude with encoded IDs", format: "claude", body: claudeList, headers: http.Header{"X-Api-Key": {cursorKey}},
			want: `{"data":[{"display_name":"Claude Sonnet 4.5","id":"claude-sonnet-4-5","type":"model"},{"display_name":"gpt-5.4","id":"claude-fable-5-dd-4.5-tpg","type":"model"}],"first_id":"claude-sonnet-4-5","has_more":false,"last_id":"claude-fable-5-dd-4.5-tpg"}`,
		},
		{
			name: "codex catalog", format: "openai", body: codexList, headers: cursor,
			want: `{"models":[{"slug":"gpt-5.4","display_name":"GPT-5.4","priority":1},{"slug":"gpt-5.4-codex","display_name":"GPT-5.4 Codex"}]}`,
		},
		{
			name: "gemini names", format: "gemini", body: geminiList, headers: http.Header{"X-Goog-Api-Key": {n8nKey}},
			want: `{"models":[{"displayName":"Gemini 2.5 Pro","name":"models/gemini-2.5-pro"}]}`,
		},
		{name: "unknown shape", format: "openai", body: unknownList, headers: cursor, want: unknownList},
		{name: "not JSON", format: "openai", body: "not json", headers: cursor, want: "not json"},
		{name: "top-level array", format: "openai", body: `[{"id":"o3"}]`, headers: cursor, want: `[{"id":"o3"}]`},
		{name: "key without policies", format: "openai", body: openAIList, headers: bearer("sk-unknown"), want: openAIList},
		{name: "no key", format: "openai", body: openAIList, headers: http.Header{}, want: openAIList},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := interceptList(t, listCall(tt.format, tt.body, tt.headers)); got != tt.want {
				t.Fatalf("body =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestModelListsOfRefusedCallersAreEmpty(t *testing.T) {
	useEmptyCatalog(t)
	invalid := "sk-invalid"
	useConfig(t, keyConfig("d39f8647a7c296777ec92feb92f618d9d81821cf653871a4e464e79b49387b68", "    policies:\n      budget: {}\n"))
	if got := interceptList(t, listCall("openai", openAIList, bearer(invalid))); got != `{"data":[],"object":"list"}` {
		t.Fatalf("invalid key list = %s", got)
	}
	useConfig(t, keyConfig(cursorScope, "    policies:\n      models: {allow: [nothing]}\n"))
	if got := interceptList(t, listCall("claude", claudeList, bearer(cursorKey))); got != `{"data":[],"first_id":"","has_more":false,"last_id":""}` {
		t.Fatalf("claude list without allowed models = %s", got)
	}
	useConfig(t, "keys: [\n")
	tests := []struct {
		format, body, want string
	}{
		{format: "openai", body: openAIList, want: `{"data":[],"object":"list"}`},
		{format: "claude", body: claudeList, want: `{"data":[],"first_id":"","has_more":false,"last_id":""}`},
		{format: "openai", body: codexList, want: `{"models":[]}`},
		{format: "gemini", body: geminiList, want: `{"models":[]}`},
	}
	for _, tt := range tests {
		if got := interceptList(t, listCall(tt.format, tt.body, bearer(invalid))); got != tt.want {
			t.Fatalf("%s list with unreadable config = %s", tt.format, got)
		}
	}
	if got := interceptList(t, listCall("openai", openAIList, nil)); got != openAIList {
		t.Fatalf("anonymous list with unreadable config = %s", got)
	}
}

func TestResponsesAPIOnlyKeysSeeFullLists(t *testing.T) {
	useEmptyCatalog(t)
	useConfig(t, keyConfig(cursorScope, "    policies:\n      responses-api: {enabled: false}\n"))
	if got := interceptList(t, listCall("openai", openAIList, bearer(cursorKey))); got != openAIList {
		t.Fatalf("list = %s", got)
	}
}

func TestExecutionResponsesAreUntouched(t *testing.T) {
	useEmptyCatalog(t)
	useConfig(t, fixtureConfig)
	tests := []struct {
		name   string
		modify func(req *pluginapi.ResponseInterceptRequest)
	}{
		{name: "model", modify: func(req *pluginapi.ResponseInterceptRequest) { req.Model = "o3" }},
		{name: "requested model", modify: func(req *pluginapi.ResponseInterceptRequest) { req.RequestedModel = "o3" }},
		{name: "request body", modify: func(req *pluginapi.ResponseInterceptRequest) { req.RequestBody = []byte(`{}`) }},
		{name: "original request", modify: func(req *pluginapi.ResponseInterceptRequest) { req.OriginalRequest = []byte(`{}`) }},
	}
	for _, tt := range tests {
		req := listCall("openai", openAIList, bearer(cursorKey))
		tt.modify(&req)
		raw, errHandle := handleMethod(pluginabi.MethodResponseInterceptAfter, mustJSON(t, req))
		if errHandle != nil {
			t.Fatal(errHandle)
		}
		if resp := decodeResult[pluginapi.ResponseInterceptResponse](t, raw); resp.Body != nil || resp.Headers != nil || resp.ClearHeaders != nil {
			t.Fatalf("%s: response = %+v", tt.name, resp)
		}
	}
	if lastCatalog.Load() != nil {
		t.Fatal("an execution response was captured as the catalog")
	}
}

func TestCallerIdentification(t *testing.T) {
	useEmptyCatalog(t)
	useConfig(t, fixtureConfig)
	filtered := `{"data":[{"created":1,"id":"gpt-5.4","object":"model","owned_by":"openai"},{"id":"claude-sonnet-4-5","object":"model"}],"object":"list"}`
	n8nList := `{"data":[{"created":1,"id":"gpt-5.4","object":"model","owned_by":"openai"},{"id":"o3","object":"model"},{"id":"claude-sonnet-4-5","object":"model"}],"object":"list"}`
	tests := []struct {
		name    string
		headers http.Header
		want    string
	}{
		{name: "bearer", headers: http.Header{"Authorization": {"Bearer " + cursorKey}}, want: filtered},
		{name: "lowercase scheme and padding", headers: http.Header{"Authorization": {"bearer   " + cursorKey + " "}}, want: filtered},
		{name: "authorization without scheme", headers: http.Header{"Authorization": {cursorKey}}, want: filtered},
		{name: "x-api-key", headers: http.Header{"X-Api-Key": {cursorKey}}, want: filtered},
		{name: "x-goog-api-key", headers: http.Header{"X-Goog-Api-Key": {cursorKey}}, want: filtered},
		{name: "proxy basic auth before the client key", headers: http.Header{"Authorization": {"Basic dXNlcjpwYXNz"}, "X-Api-Key": {cursorKey}}, want: filtered},
		{name: "unconfigured bearer before the client key", headers: http.Header{"Authorization": {"Bearer sk-other"}, "X-Goog-Api-Key": {cursorKey}}, want: filtered},
		{name: "configured key after an unconfigured one", headers: http.Header{"X-Goog-Api-Key": {cursorKey}, "X-Api-Key": {"sk-other"}}, want: filtered},
		{name: "authorization before x-api-key", headers: http.Header{"Authorization": {"Bearer " + n8nKey}, "X-Api-Key": {cursorKey}}, want: n8nList},
		{name: "x-goog-api-key before x-api-key", headers: http.Header{"X-Goog-Api-Key": {n8nKey}, "X-Api-Key": {cursorKey}}, want: n8nList},
		{name: "no candidate has an entry", headers: http.Header{"Authorization": {"Basic dXNlcjpwYXNz"}, "X-Api-Key": {"sk-other"}}, want: openAIList},
		{name: "query parameters are not visible", headers: http.Header{"Accept": {"application/json"}}, want: openAIList},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := interceptList(t, listCall("openai", openAIList, tt.headers)); got != tt.want {
				t.Fatalf("list = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestCatalogCapture(t *testing.T) {
	useEmptyCatalog(t)
	useConfig(t, fixtureConfig)
	before := time.Now().UTC().Add(-time.Second)
	interceptList(t, listCall("openai", openAIList, bearer(cursorKey)))
	captured := lastCatalog.Load()
	if captured == nil || strings.Join(captured.models, ",") != "gpt-5.4,o3,claude-sonnet-4-5,gemini-3-pro-preview" {
		t.Fatalf("catalog = %+v, want the unfiltered list", captured)
	}
	if captured.capturedAt.Before(before) || captured.capturedAt.Location() != time.UTC {
		t.Fatalf("captured at %v", captured.capturedAt)
	}
	interceptList(t, listCall("openai", `{"object":"list","data":[{"id":"o3"},{"id":""},{"id":5},{"name":"x"}]}`, nil))
	if got := strings.Join(lastCatalog.Load().models, ","); got != "o3" {
		t.Fatalf("catalog from an anonymous list = %q", got)
	}
	for _, call := range []pluginapi.ResponseInterceptRequest{
		listCall("claude", claudeList, nil),
		listCall("gemini", geminiList, nil),
		listCall("openai", codexList, nil),
		listCall("openai", "not json", nil),
	} {
		interceptList(t, call)
		if got := strings.Join(lastCatalog.Load().models, ","); got != "o3" {
			t.Fatalf("%s %s replaced the catalog: %q", call.SourceFormat, call.Body, got)
		}
	}
}

func TestDecodeClaudeModelID(t *testing.T) {
	tests := map[string]string{
		"claude-fable-5-dd-4.5-tpg":       "gpt-5.4",
		"claude-fable-5-dd-4.5-tpg(high)": "gpt-5.4(high)",
		"claude-fable-5-dd-":              "claude-fable-5-dd-",
		"claude-fable-5-dd-ézy":           "yzé",
		"claude-sonnet-4-5":               "claude-sonnet-4-5",
		"gpt-5.4":                         "gpt-5.4",
	}
	for id, want := range tests {
		if got := decodeClaudeModelID(id); got != want {
			t.Errorf("decodeClaudeModelID(%q) = %q, want %q", id, got, want)
		}
	}
}
