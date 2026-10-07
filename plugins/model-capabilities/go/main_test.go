package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPluginRegistrationDeclaresCatalogRoutes(t *testing.T) {
	registration := pluginRegistration()
	if registration.SchemaVersion != pluginSchemaVersion || !registration.Capabilities.ManagementAPI {
		t.Fatalf("registration = %+v", registration)
	}
	if registration.Metadata.Version != pluginVersion {
		t.Fatalf("version = %q, want %q", registration.Metadata.Version, pluginVersion)
	}
	routes := managementRegistration()
	if len(routes.Routes) != 1 || routes.Routes[0].Path != managementPath {
		t.Fatalf("management routes = %+v", routes.Routes)
	}
	if len(routes.Resources) != 1 || routes.Resources[0].Path != resourcePath {
		t.Fatalf("resource routes = %+v", routes.Resources)
	}
}

func TestBuildModelMapsPreservesRegistryMetadataAndDoesNotExposeConfig(t *testing.T) {
	models := buildModelMaps([]map[string]any{
		{"id": "gpt-unknown", "object": "model", "owned_by": "openai", "config": map[string]any{"override_header": map[string]any{"x-secret": "value"}}},
		{"id": "gpt-known", "object": "model", "owned_by": "openai"},
	}, pluginConfig{
		Enabled: true,
		Protocols: map[string]protocolOverride{
			"gpt-known": {Preferred: protocolOpenAIResponses, Capabilities: map[string]any{"tools": true}},
		},
	})
	if len(models) != 2 || models[0]["id"] != "gpt-known" || models[1]["id"] != "gpt-unknown" {
		t.Fatalf("models = %#v", models)
	}
	known := models[0]
	if known["protocol"] != protocolOpenAIResponses {
		t.Fatalf("known protocol = %#v", known["protocol"])
	}
	if got := known["endpoints"]; !strings.Contains(toJSON(t, got), "/v1/responses") {
		t.Fatalf("known endpoints = %#v", got)
	}
	if _, ok := models[1]["config"]; ok {
		t.Fatal("runtime model config leaked into catalog")
	}
	if models[1]["protocol"] != nil || models[1]["protocol_source"] != "unknown" {
		t.Fatalf("unknown protocol = %#v", models[1])
	}
}

func TestRenderCatalogUsesETag(t *testing.T) {
	activeConfig.Store(&pluginConfig{Enabled: true, Protocols: map[string]protocolOverride{}})
	t.Cleanup(func() { activeConfig.Store(nil) })

	first := renderCatalogModels(nil, defaultPluginConfig(), nil)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d", first.StatusCode)
	}
	if first.Headers.Get("ETag") == "" {
		t.Fatal("first response has no ETag")
	}
	var document catalogDocument
	if errDecode := json.Unmarshal(first.Body, &document); errDecode != nil {
		t.Fatal(errDecode)
	}
	if document.Object != "model_capabilities" || document.SchemaVersion != 1 {
		t.Fatalf("document = %+v", document)
	}

	second := renderCatalogModels(http.Header{"If-None-Match": []string{first.Headers.Get("ETag")}}, defaultPluginConfig(), nil)
	if second.StatusCode != http.StatusNotModified || len(second.Body) != 0 {
		t.Fatalf("conditional response = status %d body %q", second.StatusCode, second.Body)
	}
}

func TestDisabledCatalogReturnsNotFound(t *testing.T) {
	activeConfig.Store(&pluginConfig{Enabled: false})
	t.Cleanup(func() { activeConfig.Store(nil) })
	response := renderCatalogModels(nil, pluginConfig{Enabled: false}, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}
}

func TestHandleManagementRejectsUnexpectedMethod(t *testing.T) {
	request, errMarshal := json.Marshal(rpcManagementRequest{ManagementRequest: pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   managementPath,
	}})
	if errMarshal != nil {
		t.Fatal(errMarshal)
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
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", response.StatusCode)
	}
}

func toJSON(t *testing.T, value any) string {
	t.Helper()
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return string(raw)
}
