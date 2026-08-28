package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPluginRegistrationDeclaresUsageAndManagement(t *testing.T) {
	registration := pluginRegistration()
	if registration.SchemaVersion != pluginabi.SchemaVersion || !registration.Capabilities.UsagePlugin || !registration.Capabilities.ManagementAPI {
		t.Fatalf("registration = %+v", registration)
	}
	if registration.Metadata.Version != pluginVersion {
		t.Fatalf("version = %q, want %q", registration.Metadata.Version, pluginVersion)
	}
	foundPricing := false
	for _, field := range registration.Metadata.ConfigFields {
		if field.Name == "pricing_enabled" {
			foundPricing = true
		}
	}
	if !foundPricing {
		t.Fatal("pricing configuration fields are missing")
	}
}

func TestHandleManagementSummaryAndDashboard(t *testing.T) {
	cliproxyPluginShutdownForTest()
	dataFile := filepath.Join(t.TempDir(), "usage.jsonl")
	lifecycle, errMarshal := json.Marshal(lifecycleRequest{ConfigYAML: []byte("enabled: true\ndata_file: " + dataFile + "\nrecent_records: 5\npricing_enabled: false\n")})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if _, errConfigure := handleMethod(pluginabi.MethodPluginRegister, lifecycle); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	t.Cleanup(cliproxyPluginShutdownForTest)

	usageRaw, errUsage := json.Marshal(pluginapi.UsageRecord{Provider: "codex", Model: "gpt-test", Detail: pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}})
	if errUsage != nil {
		t.Fatal(errUsage)
	}
	if _, errHandle := handleMethod(pluginabi.MethodUsageHandle, usageRaw); errHandle != nil {
		t.Fatal(errHandle)
	}

	for _, testCase := range []struct {
		path        string
		contentType string
		contains    string
	}{
		{path: "/v0/management/plugins/usage-insights/summary", contentType: "application/json", contains: `"requests": 1`},
		{path: "/v0/management/plugins/usage-insights/dashboard", contentType: "text/html", contains: "Usage Insights"},
	} {
		request, errRequest := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: testCase.path})
		if errRequest != nil {
			t.Fatal(errRequest)
		}
		raw, errHandle := handleMethod(pluginabi.MethodManagementHandle, request)
		if errHandle != nil {
			t.Fatal(errHandle)
		}
		var env envelope
		if errDecode := json.Unmarshal(raw, &env); errDecode != nil {
			t.Fatal(errDecode)
		}
		var response pluginapi.ManagementResponse
		if errDecode := json.Unmarshal(env.Result, &response); errDecode != nil {
			t.Fatal(errDecode)
		}
		if !strings.Contains(response.Headers.Get("Content-Type"), testCase.contentType) || !strings.Contains(string(response.Body), testCase.contains) {
			t.Fatalf("response for %s = headers=%v body=%s", testCase.path, response.Headers, response.Body)
		}
	}
}

func cliproxyPluginShutdownForTest() {
	configureMu.Lock()
	old := activeStore.Swap(nil)
	configureMu.Unlock()
	_ = old.close()
}
