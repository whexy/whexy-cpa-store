package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Client keys used across tests, and their session.CallerScope values
// computed independently with sha256sum.
const (
	cursorKey   = "sk-client-cursor"
	cursorScope = "285a6eadf2a3d85b49cfad466ee9b126cafc3438d43e979e0f653d904282d6e7"
	n8nKey      = "sk-client-n8n"
	n8nScope    = "3f4eabdf9d6a68825cddbfa00e050079c136e691fa8717f58a3d0a64535a88a9"
)

var (
	invalidScope = strings.Repeat("a", 64)
	openScope    = strings.Repeat("b", 64)
)

// fixtureConfig restricts cursor to Claude and GPT-5 models without the
// Responses API, hides preview models from n8n, gives invalidScope an
// unknown policy and openScope no policy.
var fixtureConfig = `enabled: true
priority: 0
keys:
  ` + cursorScope + `:
    name: Cursor
    note: work laptop
    policies:
      models:
        allow: [claude-*, gpt-5*]
      responses-api:
        enabled: false
  ` + n8nScope + `:
    policies:
      models:
        deny: ["*-preview"]
  ` + invalidScope + `:
    policies:
      budget: {limit: 3}
  ` + openScope + `:
    name: Open
`

// useConfig applies configYAML through plugin.reconfigure, returns the
// registration it answered and restores the previous rules after the test.
// Tests touching package state must not run in parallel.
func useConfig(t *testing.T, configYAML string) registration {
	t.Helper()
	previous := currentRuleSet.Load()
	t.Cleanup(func() { currentRuleSet.Store(previous) })
	raw, errHandle := handleMethod(pluginabi.MethodPluginReconfigure, mustJSON(t, lifecycleRequest{ConfigYAML: []byte(configYAML)}))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	return decodeResult[registration](t, raw)
}

func useEmptyCatalog(t *testing.T) {
	t.Helper()
	previous := lastCatalog.Load()
	lastCatalog.Store(nil)
	t.Cleanup(func() { lastCatalog.Store(previous) })
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return raw
}

func decodeEnvelope(t *testing.T, raw []byte) envelope {
	t.Helper()
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("envelope %s: %v", raw, errUnmarshal)
	}
	return env
}

func decodeResult[T any](t *testing.T, raw []byte) T {
	t.Helper()
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("envelope not ok: %s", raw)
	}
	var out T
	if errUnmarshal := json.Unmarshal(env.Result, &out); errUnmarshal != nil {
		t.Fatalf("result %s: %v", env.Result, errUnmarshal)
	}
	return out
}

func TestRegistrationDeclaresCapabilities(t *testing.T) {
	useConfig(t, "enabled: true\npriority: 0\n")
	raw, errHandle := handleMethod(pluginabi.MethodPluginRegister, mustJSON(t, lifecycleRequest{}))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	reg := decodeResult[registration](t, raw)
	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema = %d, want %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}
	if reg.Metadata.Name != "Client keys" || reg.Metadata.Version != pluginVersion || reg.Metadata.Author != "Whexy" || reg.Metadata.GitHubRepository != "https://github.com/whexy/whexy-cpa-store" {
		t.Fatalf("metadata = %+v", reg.Metadata)
	}
	if !reg.Capabilities.RequestInterceptor || !reg.Capabilities.ManagementAPI || reg.Capabilities.ResponseInterceptor {
		t.Fatalf("capabilities = %+v", reg.Capabilities)
	}
	fields := reg.Metadata.ConfigFields
	if len(fields) != 1 || fields[0].Name != "keys" || fields[0].Type != pluginapi.ConfigFieldTypeObject {
		t.Fatalf("config fields = %+v", fields)
	}
}

func TestRegistrationWireNamesMatchHost(t *testing.T) {
	useConfig(t, fixtureConfig)
	raw := string(mustJSON(t, pluginRegistration()))
	for _, name := range []string{`"schema_version":6`, `"request_interceptor":true`, `"response_interceptor":true`, `"management_api":true`} {
		if !strings.Contains(raw, name) {
			t.Fatalf("registration %s lacks %s", raw, name)
		}
	}
}

// The host replaces a plugin's capabilities with those plugin.reconfigure
// returns, so the response interceptor follows the configuration.
func TestResponseInterceptorFollowsConfig(t *testing.T) {
	scope := strings.Repeat("c", 64)
	tests := []struct {
		name   string
		config string
		want   bool
	}{
		{name: "no keys", config: "enabled: true\n", want: false},
		{name: "key without policies", config: "keys:\n  " + scope + ":\n    name: x\n", want: false},
		{name: "responses-api only", config: "keys:\n  " + scope + ":\n    policies:\n      responses-api: {enabled: false}\n", want: false},
		{name: "models policy", config: "keys:\n  " + scope + ":\n    policies:\n      models: {deny: [o1]}\n", want: true},
		{name: "invalid key", config: "keys:\n  " + scope + ":\n    policies: [models]\n", want: true},
		{name: "unmatchable name only", config: "keys:\n  cursor:\n    policies: [models]\n", want: false},
		{name: "unreadable config", config: "keys: [\n", want: true},
		{name: "back to no keys", config: "keys: {}\n", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := useConfig(t, tt.config).Capabilities.ResponseInterceptor; got != tt.want {
				t.Fatalf("response_interceptor = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLifecycleNeverFails(t *testing.T) {
	previous := currentRuleSet.Load()
	t.Cleanup(func() { currentRuleSet.Store(previous) })
	requests := [][]byte{
		nil,
		[]byte(`{}`),
		[]byte(`not json`),
		[]byte(`{"config_yaml":"not base64"}`),
		mustJSON(t, lifecycleRequest{ConfigYAML: []byte("keys: [\n")}),
		mustJSON(t, lifecycleRequest{ConfigYAML: []byte("- a\n- b\n")}),
		mustJSON(t, lifecycleRequest{ConfigYAML: []byte("keys:\n  " + invalidScope + ": &a [*a]\n")}),
		mustJSON(t, lifecycleRequest{ConfigYAML: []byte("keys:\n  ? [a, b]\n  : x\n")}),
	}
	for _, method := range []string{pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure} {
		for _, request := range requests {
			raw, errHandle := safeHandleMethod(method, request)
			if errHandle != nil {
				t.Fatalf("%s %q: %v", method, request, errHandle)
			}
			reg := decodeResult[registration](t, raw)
			if !reg.Capabilities.RequestInterceptor || !reg.Capabilities.ManagementAPI {
				t.Fatalf("%s %q registration = %+v", method, request, reg)
			}
		}
	}
}

func TestLifecycleAndUnknownMethods(t *testing.T) {
	for _, method := range []string{pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown} {
		raw, errHandle := handleMethod(method, nil)
		if errHandle != nil {
			t.Fatal(errHandle)
		}
		if env := decodeEnvelope(t, raw); !env.OK || string(env.Result) != "{}" {
			t.Fatalf("%s = %s", method, raw)
		}
	}
	raw, errHandle := handleMethod(pluginabi.MethodRequestInterceptAfter, []byte(`{"SourceFormat":"openai-response"}`))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	if resp := decodeResult[pluginapi.RequestInterceptResponse](t, raw); resp.Terminate || resp.Body != nil || resp.Headers != nil {
		t.Fatalf("intercept_after = %+v", resp)
	}
	raw, errHandle = handleMethod("no.such.method", []byte(`{}`))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	if env := decodeEnvelope(t, raw); env.OK || env.Error == nil || env.Error.Code != "unknown_method" {
		t.Fatalf("unknown method = %s", raw)
	}
	for _, method := range []string{pluginabi.MethodResponseInterceptAfter, pluginabi.MethodManagementHandle} {
		if _, errHandle := safeHandleMethod(method, []byte(`not json`)); errHandle == nil {
			t.Fatalf("%s accepted an undecodable request", method)
		}
	}
}

func TestConfigureCountsGenerations(t *testing.T) {
	useConfig(t, "keys: {}\n")
	first := currentRules()
	useConfig(t, "keys: [\n")
	second := currentRules()
	if second.generation != first.generation+1 || first.appliedAt.IsZero() || second.appliedAt.Before(first.appliedAt) {
		t.Fatalf("generations = %d at %v, then %d at %v", first.generation, first.appliedAt, second.generation, second.appliedAt)
	}
}
