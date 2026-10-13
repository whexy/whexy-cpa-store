package main

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type interceptCase struct {
	format  string
	scope   string
	model   string
	headers http.Header
}

func (c interceptCase) request() pluginapi.RequestInterceptRequest {
	req := pluginapi.RequestInterceptRequest{SourceFormat: c.format, RequestedModel: c.model, Model: c.model, Headers: c.headers, Body: []byte(`{"model":"x"}`)}
	if c.scope != "" {
		req.Metadata = map[string]any{"caller_scope": c.scope, "request_path": "/v1/x"}
	}
	return req
}

// intercept runs request.intercept_before through the ABI dispatch.
func intercept(t *testing.T, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	t.Helper()
	raw, errHandle := handleMethod(pluginabi.MethodRequestInterceptBefore, mustJSON(t, req))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	return decodeResult[pluginapi.RequestInterceptResponse](t, raw)
}

const (
	passes          = "pass"
	emptyNotFound   = "404 empty"
	modelRefused    = "404 model_not_found"
	invalidRefused  = "403 client_key_policy_invalid"
	rulesRefused    = "503 client_key_policy_unavailable"
	disableResponse = "Whexy-Cpa-Disable-Response-Api"
)

func outcome(t *testing.T, resp pluginapi.RequestInterceptResponse) string {
	t.Helper()
	if !resp.Terminate {
		if resp.StatusCode != 0 || resp.ResponseBody != nil || resp.Body != nil || resp.Headers != nil {
			t.Fatalf("pass-through response modifies the request: %+v", resp)
		}
		return passes
	}
	if len(resp.ResponseBody) == 0 {
		if resp.ResponseHeaders != nil {
			t.Fatalf("empty denial carries headers: %v", resp.ResponseHeaders)
		}
		return strconv.Itoa(resp.StatusCode) + " empty"
	}
	if resp.ResponseHeaders.Get("Content-Type") != "application/json" {
		t.Fatalf("denial headers = %v", resp.ResponseHeaders)
	}
	body := string(resp.ResponseBody)
	for _, code := range []string{"model_not_found", "client_key_policy_invalid", "client_key_policy_unavailable"} {
		if strings.Contains(body, `"code":"`+code+`"`) {
			return strconv.Itoa(resp.StatusCode) + " " + code
		}
	}
	t.Fatalf("unrecognized denial body %s", body)
	return ""
}

func TestDecideCheckOrder(t *testing.T) {
	useConfig(t, fixtureConfig)
	trigger := http.Header{disableResponse: {"1"}}
	tests := []struct {
		name string
		call interceptCase
		want string
	}{
		{name: "no caller scope", call: interceptCase{format: "openai", model: "o3"}, want: passes},
		{name: "unconfigured scope", call: interceptCase{format: "openai", scope: strings.Repeat("d", 64), model: "o3"}, want: passes},
		{name: "header trigger without scope", call: interceptCase{format: "openai-response", model: "o3", headers: trigger}, want: emptyNotFound},
		{name: "header trigger on chat completions", call: interceptCase{format: "openai", model: "o3", headers: trigger}, want: passes},
		{name: "header trigger before invalid key", call: interceptCase{format: "openai-response", scope: invalidScope, model: "o3", headers: trigger}, want: emptyNotFound},
		{name: "invalid key", call: interceptCase{format: "openai", scope: invalidScope, model: "gpt-5"}, want: invalidRefused},
		{name: "invalid key without model", call: interceptCase{format: "openai-video", scope: invalidScope}, want: invalidRefused},
		{name: "responses-api disabled", call: interceptCase{format: "openai-response", scope: cursorScope, model: "gpt-5"}, want: emptyNotFound},
		{name: "responses-api before models", call: interceptCase{format: "openai-response", scope: cursorScope, model: "o3"}, want: emptyNotFound},
		{name: "allowed model", call: interceptCase{format: "openai", scope: cursorScope, model: "gpt-5.4"}, want: passes},
		{name: "allowed claude model", call: interceptCase{format: "claude", scope: cursorScope, model: "Claude-Sonnet-4"}, want: passes},
		{name: "model outside allow list", call: interceptCase{format: "openai", scope: cursorScope, model: "o3"}, want: modelRefused},
		{name: "allow list without model", call: interceptCase{format: "openai-video", scope: cursorScope}, want: modelRefused},
		{name: "denied model", call: interceptCase{format: "openai", scope: n8nScope, model: "gemini-3-pro-preview"}, want: modelRefused},
		{name: "model outside deny list", call: interceptCase{format: "openai", scope: n8nScope, model: "gemini-3-pro"}, want: passes},
		{name: "deny list without model", call: interceptCase{format: "openai-video", scope: n8nScope}, want: passes},
		{name: "responses api without responses-api policy", call: interceptCase{format: "openai-response", scope: n8nScope, model: "o3"}, want: passes},
		{name: "key without policies", call: interceptCase{format: "openai-response", scope: openScope, model: "anything"}, want: passes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := outcome(t, intercept(t, tt.call.request())); got != tt.want {
				t.Fatalf("outcome = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDecideWithUnreadableConfig(t *testing.T) {
	useConfig(t, "keys: [\n")
	trigger := http.Header{disableResponse: {"1"}}
	tests := []struct {
		name string
		call interceptCase
		want string
	}{
		{name: "caller scope", call: interceptCase{format: "openai", scope: openScope, model: "gpt-5"}, want: rulesRefused},
		{name: "caller scope before header trigger", call: interceptCase{format: "openai-response", scope: openScope, model: "gpt-5", headers: trigger}, want: rulesRefused},
		{name: "no caller scope", call: interceptCase{format: "openai", model: "gpt-5"}, want: passes},
		{name: "header trigger without scope", call: interceptCase{format: "openai-response", model: "gpt-5", headers: trigger}, want: emptyNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := outcome(t, intercept(t, tt.call.request())); got != tt.want {
				t.Fatalf("outcome = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRequestedModel(t *testing.T) {
	tests := []struct {
		requested, model, want string
	}{
		{requested: "gpt-5", model: "gpt-5", want: "gpt-5"},
		{requested: "  gpt-5(high)  ", model: "gpt-5", want: "gpt-5"},
		{requested: "models/gemini-2.5-pro", model: "gemini-2.5-pro", want: "gemini-2.5-pro"},
		{requested: "models/gemini-2.5-pro(8192)", want: "gemini-2.5-pro"},
		{requested: "openrouter/gpt-5", model: "gpt-5", want: "openrouter/gpt-5"},
		{requested: "a(b)(c)", want: "a(b)"},
		{requested: "gpt-5 (high)", want: "gpt-5"},
		{requested: "gpt-5\t(high)", want: "gpt-5"},
		{requested: "models/ gpt-5 (8192)", want: "gpt-5"},
		{requested: "a(b)c", want: "a(b)c"},
		{requested: "", model: "gpt-5(low)", want: "gpt-5"},
		{requested: "auto", model: "claude-sonnet-4(high)", want: "claude-sonnet-4"},
		{requested: "AUTO(high)", model: "models/gemini-2.5-pro", want: "gemini-2.5-pro"},
		{requested: "auto", model: "", want: ""},
		{requested: "", model: "", want: ""},
	}
	for _, tt := range tests {
		got := requestedModel(pluginapi.RequestInterceptRequest{RequestedModel: tt.requested, Model: tt.model})
		if got != tt.want {
			t.Errorf("requestedModel(%q, %q) = %q, want %q", tt.requested, tt.model, got, tt.want)
		}
	}
}

func TestModelsPolicyMatching(t *testing.T) {
	compile := func(t *testing.T, settings string) modelsPolicy {
		t.Helper()
		rules := compileRules([]byte(keyConfig(cursorScope, "    policies:\n      models: "+settings+"\n")))
		key := rules.keys[cursorScope]
		if key.err != "" {
			t.Fatal(key.err)
		}
		return key.policies[0].rule.(modelsPolicy)
	}
	tests := []struct {
		settings string
		model    string
		want     bool
	}{
		{settings: "{allow: [gpt-5]}", model: "gpt-5", want: true},
		{settings: "{allow: [gpt-5]}", model: "gpt-5.4", want: false},
		{settings: "{allow: [GPT-5*]}", model: "gpt-5.4-mini", want: true},
		{settings: "{allow: [gpt-5*]}", model: "GPT-5", want: true},
		{settings: "{allow: [claude-*-4*]}", model: "claude-sonnet-4-5", want: true},
		{settings: "{allow: [claude-*-4*]}", model: "claude-sonnet-3", want: false},
		{settings: "{allow: ['*/gpt-*']}", model: "openrouter/gpt-5", want: true},
		{settings: "{allow: ['*/gpt-*']}", model: "gpt-5", want: false},
		{settings: "{allow: ['*']}", model: "anything", want: true},
		{settings: "{allow: ['*']}", model: "", want: false},
		{settings: "{allow: ['a**b']}", model: "ab", want: true},
		{settings: "{allow: ['a*b*c']}", model: "aXbYbZc", want: true},
		{settings: "{allow: ['a*b*c']}", model: "aXcYb", want: false},
		{settings: "{allow: ['gpt-5?']}", model: "gpt-5x", want: false},
		{settings: "{allow: ['gpt-[5]']}", model: "gpt-[5]", want: true},
		{settings: "{allow: ['ÉCLAIR-*']}", model: "éclair-1", want: true},
		{settings: "{allow: ['  o3  ']}", model: "o3", want: true},
		{settings: "{deny: ['*-preview', o1]}", model: "gemini-3-pro-preview", want: false},
		{settings: "{deny: ['*-preview', o1]}", model: "O1", want: false},
		{settings: "{deny: ['*-preview', o1]}", model: "o1-mini", want: true},
		{settings: "{deny: ['*']}", model: "", want: true},
	}
	for _, tt := range tests {
		if got := compile(t, tt.settings).lists(tt.model); got != tt.want {
			t.Errorf("%s lists %q = %v, want %v", tt.settings, tt.model, got, tt.want)
		}
	}
}

func TestDenialBodies(t *testing.T) {
	useConfig(t, fixtureConfig)
	openAIModel := `{"error":{"message":"The model ` + "`o3`" + ` does not exist or you do not have access to it.","type":"invalid_request_error","param":"model","code":"model_not_found"}}`
	openAIInvalid := `{"error":{"message":"` + keyInvalidMessage + `","type":"permission_error","param":null,"code":"client_key_policy_invalid"}}`
	googleModel := `{"error":{"code":404,"message":"models/o3 is not available for this API key.","status":"NOT_FOUND"}}`
	googleInvalid := `{"error":{"code":403,"message":"` + keyInvalidMessage + `","status":"PERMISSION_DENIED"}}`
	tests := []struct {
		format string
		scope  string
		status int
		body   string
	}{
		{format: "claude", scope: cursorScope, status: 404, body: `{"type":"error","error":{"type":"not_found_error","message":"model: o3"}}`},
		{format: "claude", scope: invalidScope, status: 403, body: `{"type":"error","error":{"type":"permission_error","message":"` + keyInvalidMessage + `"}}`},
		{format: "gemini", scope: cursorScope, status: 404, body: googleModel},
		{format: "gemini", scope: invalidScope, status: 403, body: googleInvalid},
		{format: "interactions", scope: cursorScope, status: 404, body: googleModel},
		{format: "interactions", scope: invalidScope, status: 403, body: googleInvalid},
		{format: "openai", scope: cursorScope, status: 404, body: openAIModel},
		{format: "openai", scope: invalidScope, status: 403, body: openAIInvalid},
		{format: "openai-image", scope: cursorScope, status: 404, body: openAIModel},
		{format: "openai-video", scope: cursorScope, status: 404, body: openAIModel},
		{format: "openai-speech", scope: cursorScope, status: 404, body: openAIModel},
		{format: "openai-response", scope: invalidScope, status: 403, body: openAIInvalid},
		{format: "", scope: cursorScope, status: 404, body: openAIModel},
		{format: "future-format", scope: invalidScope, status: 403, body: openAIInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.format+"/"+tt.scope[:4], func(t *testing.T) {
			resp := intercept(t, interceptCase{format: tt.format, scope: tt.scope, model: "models/o3(high)"}.request())
			if !resp.Terminate || resp.StatusCode != tt.status || string(resp.ResponseBody) != tt.body {
				t.Fatalf("denial = %d %s, want %d %s", resp.StatusCode, resp.ResponseBody, tt.status, tt.body)
			}
			if resp.ResponseHeaders.Get("Content-Type") != "application/json" {
				t.Fatalf("headers = %v", resp.ResponseHeaders)
			}
		})
	}
}

func TestUnavailableBodies(t *testing.T) {
	useConfig(t, "keys: [\n")
	tests := []struct {
		format string
		body   string
	}{
		{format: "claude", body: `{"type":"error","error":{"type":"api_error","message":"` + rulesUnavailableMessage + `"}}`},
		{format: "gemini", body: `{"error":{"code":503,"message":"` + rulesUnavailableMessage + `","status":"UNAVAILABLE"}}`},
		{format: "openai", body: `{"error":{"message":"` + rulesUnavailableMessage + `","type":"server_error","param":null,"code":"client_key_policy_unavailable"}}`},
	}
	for _, tt := range tests {
		resp := intercept(t, interceptCase{format: tt.format, scope: openScope, model: "o3"}.request())
		if !resp.Terminate || resp.StatusCode != http.StatusServiceUnavailable || string(resp.ResponseBody) != tt.body {
			t.Fatalf("%s denial = %d %s", tt.format, resp.StatusCode, resp.ResponseBody)
		}
	}
}

func TestWebSocketTurnsAreRefusedWith400(t *testing.T) {
	useConfig(t, fixtureConfig)
	upgrade := http.Header{"Upgrade": {"WebSocket"}, "Connection": {"Upgrade"}}
	tests := []struct {
		name  string
		scope string
		want  string
	}{
		{name: "denied model", scope: n8nScope, want: "400 model_not_found"},
		{name: "invalid key", scope: invalidScope, want: "400 client_key_policy_invalid"},
	}
	for _, tt := range tests {
		resp := intercept(t, interceptCase{format: "openai-response", scope: tt.scope, model: "o1-preview", headers: upgrade}.request())
		if got := outcome(t, resp); got != tt.want {
			t.Fatalf("%s: outcome = %q, want %q", tt.name, got, tt.want)
		}
	}
	useConfig(t, "keys: [\n")
	resp := intercept(t, interceptCase{format: "openai-response", scope: openScope, model: "o3", headers: http.Header{"Upgrade": {"h2c, websocket"}}}.request())
	if got := outcome(t, resp); got != "400 client_key_policy_unavailable" {
		t.Fatalf("unreadable config outcome = %q", got)
	}
	if isWebSocketUpgrade(http.Header{"Upgrade": {"h2c"}}) || isWebSocketUpgrade(http.Header{"Connection": {"Upgrade"}}) {
		t.Fatal("non-WebSocket upgrade detected as WebSocket")
	}
}

func TestHeaderTrigger(t *testing.T) {
	useConfig(t, "keys: {}\n")
	tests := []struct {
		name    string
		format  string
		headers http.Header
		want    string
	}{
		{name: "canonicalized underscore spelling", format: "openai-response", headers: http.Header{"Whexy_cpa_disable_response_api": {""}}, want: emptyNotFound},
		{name: "raw underscore spelling", format: "openai-response", headers: http.Header{"WHEXY_CPA_DISABLE_RESPONSE_API": {"1"}}, want: emptyNotFound},
		{name: "hyphenated spelling", format: "openai-response", headers: http.Header{"Whexy-Cpa-Disable-Response-Api": {"x"}}, want: emptyNotFound},
		{name: "lowercase", format: "openai-response", headers: http.Header{"whexy-cpa-disable-response-api": {"x"}}, want: emptyNotFound},
		{name: "without header", format: "openai-response", headers: http.Header{"Authorization": {"Bearer k"}}, want: passes},
		{name: "chat completions", format: "openai", headers: http.Header{disableResponse: {""}}, want: passes},
		{name: "messages", format: "claude", headers: http.Header{disableResponse: {""}}, want: passes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := outcome(t, intercept(t, interceptCase{format: tt.format, model: "gpt-5", headers: tt.headers}.request())); got != tt.want {
				t.Fatalf("outcome = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResponsesAPIPolicyLeavesOtherFormats(t *testing.T) {
	useConfig(t, keyConfig(cursorScope, "    policies:\n      responses-api: {enabled: false}\n"))
	for _, format := range []string{"openai", "claude", "gemini", "interactions", "openai-image", "openai-video", "openai-speech"} {
		if got := outcome(t, intercept(t, interceptCase{format: format, scope: cursorScope, model: "anything"}.request())); got != passes {
			t.Fatalf("%s outcome = %q", format, got)
		}
	}
	if got := outcome(t, intercept(t, interceptCase{format: "openai-response", scope: cursorScope, model: "anything"}.request())); got != emptyNotFound {
		t.Fatalf("responses outcome = %q", got)
	}
	useConfig(t, keyConfig(cursorScope, "    policies:\n      responses-api: {enabled: true}\n"))
	if got := outcome(t, intercept(t, interceptCase{format: "openai-response", scope: cursorScope, model: "anything"}.request())); got != passes {
		t.Fatalf("enabled responses outcome = %q", got)
	}
}

func TestThinkingSuffixCannotBypassDenyList(t *testing.T) {
	useConfig(t, keyConfig(cursorScope, "    policies:\n      models: {deny: [gpt-5]}\n"))
	for _, model := range []string{"gpt-5", "gpt-5(high)", "gpt-5 (high)", "gpt-5\t(high)", " GPT-5 (low) "} {
		if got := outcome(t, intercept(t, interceptCase{format: "openai", scope: cursorScope, model: model}.request())); got != modelRefused {
			t.Errorf("%q outcome = %q", model, got)
		}
	}
}

// interceptAfter runs request.intercept_after through the ABI dispatch, as
// the host sends it once per credential attempt.
func interceptAfter(t *testing.T, format, scope, requested, upstream string, headers http.Header) pluginapi.RequestInterceptResponse {
	t.Helper()
	req := pluginapi.RequestInterceptRequest{SourceFormat: format, ToFormat: "openai", RequestedModel: requested, Model: upstream, Headers: headers, Body: []byte(`{}`)}
	if scope != "" {
		req.Metadata = map[string]any{"caller_scope": scope, "canonical_session_id": "s-1"}
	}
	raw, errHandle := handleMethod(pluginabi.MethodRequestInterceptAfter, mustJSON(t, req))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	return decodeResult[pluginapi.RequestInterceptResponse](t, raw)
}

func TestDecideAfterAuth(t *testing.T) {
	useConfig(t, fixtureConfig)
	trigger := http.Header{disableResponse: {"1"}}
	tests := []struct {
		name                string
		format, scope       string
		requested, upstream string
		headers             http.Header
		want                string
	}{
		{name: "alias to a denied model", format: "openai", scope: n8nScope, requested: "fast", upstream: "gemini-3-pro-preview", want: modelRefused},
		{name: "pool member with thinking suffix", format: "openai", scope: n8nScope, requested: "fast(high)", upstream: "gemini-3-pro-preview (high)", want: modelRefused},
		{name: "gemini path prefix", format: "openai", scope: n8nScope, requested: "fast", upstream: "models/gemini-3-pro-preview", want: modelRefused},
		{name: "alias to an allowed model", format: "openai", scope: n8nScope, requested: "fast", upstream: "gemini-3-pro", want: passes},
		{name: "no upstream model", format: "openai-video", scope: n8nScope, requested: "", upstream: "", want: passes},
		{name: "allow lists match only the requested name", format: "openai", scope: cursorScope, requested: "gpt-5-fast", upstream: "o3", want: passes},
		{name: "invalid key", format: "openai", scope: invalidScope, requested: "o3", upstream: "o3", want: passes},
		{name: "key without policies", format: "openai", scope: openScope, requested: "a", upstream: "gemini-3-pro-preview", want: passes},
		{name: "no caller scope", format: "openai", requested: "fast", upstream: "gemini-3-pro-preview", want: passes},
		{name: "header trigger is checked before auth only", format: "openai-response", scope: n8nScope, requested: "o3", upstream: "o3", headers: trigger, want: passes},
		{name: "responses-api is checked before auth only", format: "openai-response", scope: cursorScope, requested: "gpt-5", upstream: "gpt-5", want: passes},
		{name: "websocket turn", format: "openai-response", scope: n8nScope, requested: "fast", upstream: "o1-preview", headers: http.Header{"Upgrade": {"websocket"}}, want: "400 model_not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := outcome(t, interceptAfter(t, tt.format, tt.scope, tt.requested, tt.upstream, tt.headers)); got != tt.want {
				t.Fatalf("outcome = %q, want %q", got, tt.want)
			}
		})
	}
	useConfig(t, "keys: [\n")
	if got := outcome(t, interceptAfter(t, "openai", n8nScope, "fast", "gemini-3-pro-preview", nil)); got != passes {
		t.Fatalf("unreadable config after auth = %q", got)
	}
}

func TestAfterAuthRefusalQuotesTheRequestedName(t *testing.T) {
	useConfig(t, fixtureConfig)
	tests := []struct {
		format, requested, upstream, body string
	}{
		{format: "claude", requested: "fast(high)", upstream: "gemini-3-pro-preview", body: `{"type":"error","error":{"type":"not_found_error","message":"model: fast"}}`},
		{format: "gemini", requested: "models/fast", upstream: "gemini-3-pro-preview", body: `{"error":{"code":404,"message":"models/fast is not available for this API key.","status":"NOT_FOUND"}}`},
		{format: "openai", requested: "", upstream: "gemini-3-pro-preview(low)", body: `{"error":{"message":"The model ` + "`gemini-3-pro-preview`" + ` does not exist or you do not have access to it.","type":"invalid_request_error","param":"model","code":"model_not_found"}}`},
	}
	for _, tt := range tests {
		resp := interceptAfter(t, tt.format, n8nScope, tt.requested, tt.upstream, nil)
		if !resp.Terminate || resp.StatusCode != http.StatusNotFound || string(resp.ResponseBody) != tt.body {
			t.Fatalf("%s refusal = %d %s", tt.format, resp.StatusCode, resp.ResponseBody)
		}
	}
}

func TestOversizedRequests(t *testing.T) {
	for _, method := range []string{pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter} {
		raw, errOversized := oversizedRequest(method, maxRequestLength+1)
		if errOversized != nil {
			t.Fatal(errOversized)
		}
		resp := decodeResult[pluginapi.RequestInterceptResponse](t, raw)
		if !resp.Terminate || resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(string(resp.ResponseBody), `"code":"request_too_large"`) || resp.ResponseHeaders.Get("Content-Type") != "application/json" {
			t.Fatalf("%s = %+v %s", method, resp, resp.ResponseBody)
		}
	}
	for _, method := range []string{pluginabi.MethodResponseInterceptAfter, pluginabi.MethodManagementHandle, pluginabi.MethodPluginReconfigure} {
		if _, errOversized := oversizedRequest(method, maxRequestLength+1); errOversized == nil || !strings.Contains(errOversized.Error(), "too large") {
			t.Fatalf("%s error = %v", method, errOversized)
		}
	}
	if maxRequestLength != 1<<31-1 {
		t.Fatalf("maxRequestLength = %d, want the largest C int", maxRequestLength)
	}
}

func TestUndecodableInterceptRequests(t *testing.T) {
	useConfig(t, fixtureConfig)
	withScope := func(format, scope string) []byte {
		return []byte(`{"SourceFormat":"` + format + `","Metadata":{"caller_scope":"` + scope + `"},"Body":"not base64!"}`)
	}
	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{name: "scope with an entry", raw: withScope("openai", openScope), want: "503 client_key_policy_unavailable"},
		{name: "unknown format", raw: withScope("", openScope), want: "503 client_key_policy_unavailable"},
		{name: "scope without an entry", raw: withScope("openai", strings.Repeat("d", 64)), want: passes},
		{name: "no scope", raw: []byte(`{"SourceFormat":"openai","Body":5}`), want: passes},
		{name: "not JSON", raw: []byte(`not json`), want: passes},
	}
	for _, method := range []string{pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter} {
		for _, tt := range tests {
			raw, errHandle := safeHandleMethod(method, tt.raw)
			if errHandle != nil {
				t.Fatalf("%s %s: %v", method, tt.name, errHandle)
			}
			if got := outcome(t, decodeResult[pluginapi.RequestInterceptResponse](t, raw)); got != tt.want {
				t.Fatalf("%s %s = %q, want %q", method, tt.name, got, tt.want)
			}
		}
	}
	raw, _ := handleMethod(pluginabi.MethodRequestInterceptBefore, withScope("claude", openScope))
	want := `{"type":"error","error":{"type":"api_error","message":"` + requestUnreadableMessage + `"}}`
	if resp := decodeResult[pluginapi.RequestInterceptResponse](t, raw); resp.StatusCode != http.StatusServiceUnavailable || string(resp.ResponseBody) != want {
		t.Fatalf("claude refusal = %d %s", resp.StatusCode, resp.ResponseBody)
	}
	useConfig(t, "keys: [\n")
	raw, _ = handleMethod(pluginabi.MethodRequestInterceptBefore, []byte(`not json`))
	resp := decodeResult[pluginapi.RequestInterceptResponse](t, raw)
	if got := outcome(t, resp); got != "503 client_key_policy_unavailable" || !strings.Contains(string(resp.ResponseBody), rulesUnavailableMessage) {
		t.Fatalf("undecodable request with an unreadable config = %q %s", got, resp.ResponseBody)
	}
}
