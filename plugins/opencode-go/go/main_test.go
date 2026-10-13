package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestRegistrationDeclaresProviderCapabilities(t *testing.T) {
	useSettings(t, defaultSettings())
	raw, errHandle := handleMethod(pluginabi.MethodPluginRegister, mustJSON(t, lifecycleRequest{}))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	reg := decodeResult[registration](t, raw)
	if reg.SchemaVersion != 6 {
		t.Fatalf("schema = %d, want 6", reg.SchemaVersion)
	}
	if reg.Metadata.Version != pluginVersion || reg.Metadata.Name != "OpenCode Go" || reg.Metadata.Author != "Whexy" {
		t.Fatalf("metadata = %+v", reg.Metadata)
	}
	caps := reg.Capabilities
	if !caps.AuthProvider || !caps.ModelProvider || !caps.Executor || !caps.QuotaProvider || !caps.ManagementAPI {
		t.Fatalf("capabilities = %+v", caps)
	}
	if caps.ExecutorModelScope != pluginapi.ExecutorModelScopeOAuth {
		t.Fatalf("scope = %q, want oauth so per-auth models register and static ones do not", caps.ExecutorModelScope)
	}
	want := []string{"openai", "claude", "openai-response"}
	if strings.Join(caps.ExecutorInputFormats, ",") != strings.Join(want, ",") || strings.Join(caps.ExecutorOutputFormats, ",") != strings.Join(want, ",") {
		t.Fatalf("formats = %v / %v", caps.ExecutorInputFormats, caps.ExecutorOutputFormats)
	}
	var names []string
	for _, field := range reg.Metadata.ConfigFields {
		names = append(names, field.Name)
	}
	if strings.Join(names, ",") != "base_url,console_url,model_prefix,user_agent" {
		t.Fatalf("config fields = %v", names)
	}
}

func TestRegistrationWireNamesMatchHost(t *testing.T) {
	raw := mustJSON(t, pluginRegistration())
	for _, name := range []string{`"schema_version":6`, `"executor_model_scope":"oauth"`, `"auth_provider":true`, `"quota_provider":true`, `"management_api":true`, `"model_provider":true`} {
		if !strings.Contains(string(raw), name) {
			t.Fatalf("registration %s lacks %s", raw, name)
		}
	}
}

func TestParseSettings(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    settings
		wantErr string
	}{
		{name: "defaults", yaml: "enabled: true\npriority: 0\n", want: settings{BaseURL: defaultBaseURL, ModelPrefix: "opencode-go"}},
		{name: "values", yaml: "base_url: https://example.test/go/\nmodel_prefix: /go/\nuser_agent: my-agent/1\n", want: settings{BaseURL: "https://example.test/go", ModelPrefix: "go", UserAgent: "my-agent/1"}},
		{name: "null fields keep defaults", yaml: "base_url: null\nmodel_prefix: null\n", want: settings{BaseURL: defaultBaseURL, ModelPrefix: "opencode-go"}},
		{name: "none is a prefix like any other", yaml: "model_prefix: none\n", want: settings{BaseURL: defaultBaseURL, ModelPrefix: "none"}},
		{name: "console url", yaml: "console_url: https://console.test/c/\n", want: settings{BaseURL: defaultBaseURL, ConsoleURL: "https://console.test/c", ModelPrefix: "opencode-go"}},
		{name: "relative base url", yaml: "base_url: opencode.ai/inference/go\n", wantErr: "base_url"},
		{name: "relative console url", yaml: "console_url: opencode.ai/console\n", wantErr: "console_url"},
		{name: "prefix with space", yaml: "model_prefix: open code\n", wantErr: "model_prefix"},
		{name: "prefix with wildcard", yaml: "model_prefix: go*\n", wantErr: "model_prefix"},
		{name: "bad yaml", yaml: "base_url: [\n", wantErr: "decode plugin config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, errParse := parseSettings([]byte(tt.yaml))
			if tt.wantErr != "" {
				if errParse == nil || !strings.Contains(errParse.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", errParse, tt.wantErr)
				}
				return
			}
			if errParse != nil {
				t.Fatal(errParse)
			}
			if got != tt.want {
				t.Fatalf("settings = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestReconfigureStoresSettings(t *testing.T) {
	useSettings(t, defaultSettings())
	if _, errHandle := handleMethod(pluginabi.MethodPluginReconfigure, mustJSON(t, lifecycleRequest{ConfigYAML: []byte("model_prefix: go\nuser_agent: ua/2\n")})); errHandle != nil {
		t.Fatal(errHandle)
	}
	cfg := currentSettings()
	if cfg.ModelPrefix != "go" || cfg.userAgent() != "ua/2" {
		t.Fatalf("settings = %+v", cfg)
	}
	if defaultSettings().userAgent() != "whexy-cpa-opencode-go/"+pluginVersion {
		t.Fatalf("default user agent = %q", defaultSettings().userAgent())
	}
	if _, errHandle := handleMethod(pluginabi.MethodPluginReconfigure, mustJSON(t, lifecycleRequest{ConfigYAML: []byte("base_url: ftp://x\n")})); errHandle == nil {
		t.Fatal("invalid base_url accepted")
	}
}

func TestIdentifiers(t *testing.T) {
	for _, method := range []string{pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier, pluginabi.MethodQuotaIdentifier} {
		raw, errHandle := handleMethod(method, nil)
		if errHandle != nil {
			t.Fatal(errHandle)
		}
		if got := decodeResult[identifierResponse](t, raw); got.Identifier != "opencode-go" {
			t.Fatalf("%s = %q", method, got.Identifier)
		}
	}
}

func TestUnsupportedMethodsReturnStatusErrors(t *testing.T) {
	tests := []struct {
		method string
		status int
	}{
		{pluginabi.MethodQuotaReset, http.StatusBadRequest},
		{"no.such.method", 0},
	}
	for _, tt := range tests {
		raw, errHandle := handleMethod(tt.method, []byte(`{}`))
		if errHandle != nil {
			t.Fatal(errHandle)
		}
		env := decodeEnvelope(t, raw)
		if env.OK || env.Error == nil || env.Error.HTTPStatus != tt.status {
			t.Fatalf("%s envelope = %s", tt.method, raw)
		}
	}
}

func TestAuthRefreshKeepsAuth(t *testing.T) {
	raw, errHandle := handleMethod(pluginabi.MethodAuthRefresh, []byte(`{"AuthID":"opencode-go.json#abc"}`))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	resp := decodeResult[pluginapi.AuthRefreshResponse](t, raw)
	if resp.Auth.ID != "" || len(resp.Auth.StorageJSON) != 0 || !resp.NextRefreshAfter.IsZero() {
		t.Fatalf("refresh = %+v, want empty so the host keeps the auth", resp)
	}
}

func TestQuotaDescribeAndStaticModels(t *testing.T) {
	raw, _ := handleMethod(pluginabi.MethodQuotaDescribe, []byte(`{}`))
	desc := decodeResult[pluginapi.QuotaDescribeResponse](t, raw)
	if desc.DisplayName != "OpenCode Go" || len(desc.SupportedProviders) != 1 || desc.SupportedProviders[0] != "opencode-go" || desc.SupportsReset {
		t.Fatalf("describe = %+v", desc)
	}
	raw, _ = handleMethod(pluginabi.MethodModelStatic, []byte(`{}`))
	if static := decodeResult[pluginapi.ModelResponse](t, raw); static.Provider != "opencode-go" || len(static.Models) != 0 {
		t.Fatalf("static = %+v", static)
	}
}

func TestExecutorMethodsAreDispatched(t *testing.T) {
	want := map[string]int{
		pluginabi.MethodExecutorExecute:       http.StatusUnauthorized,
		pluginabi.MethodExecutorExecuteStream: http.StatusUnauthorized,
		pluginabi.MethodExecutorCountTokens:   http.StatusNotFound,
		pluginabi.MethodExecutorHTTPRequest:   http.StatusNotImplemented,
	}
	for method, status := range want {
		raw, errHandle := handleMethod(method, []byte(`{}`))
		if errHandle != nil {
			t.Fatal(errHandle)
		}
		if env := decodeEnvelope(t, raw); env.OK || env.Error.HTTPStatus != status {
			t.Fatalf("%s = %s", method, raw)
		}
	}
}

func TestErrorEnvelopeRedactsKeys(t *testing.T) {
	raw := errorEnvelope("upstream_error", "bad key "+testKeyA+" rejected", 401)
	if strings.Contains(string(raw), testKeyA) || !strings.Contains(string(raw), "oc_sk_[redacted]") {
		t.Fatalf("envelope leaks key: %s", raw)
	}
	var env pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil || env.Error.HTTPStatus != 401 {
		t.Fatalf("envelope = %s", raw)
	}
}

func TestSafeHandleMethodContainsPanics(t *testing.T) {
	useCatalogStore(t)
	fake := installFakeHost(t)
	fake.handle(pluginabi.MethodHostHTTPDo, func(json.RawMessage) (any, error) { panic("boom") })
	req := mustJSON(t, rpcAuthModelRequest{AuthModelRequest: pluginapi.AuthModelRequest{
		StorageJSON: []byte(`{"type":"opencode-go","api_key":"` + testKeyA + `"}`),
	}})
	_, errHandle := safeHandleMethod(pluginabi.MethodModelForAuth, req)
	if errHandle == nil || !strings.Contains(errHandle.Error(), "panicked") {
		t.Fatalf("err = %v", errHandle)
	}
}
