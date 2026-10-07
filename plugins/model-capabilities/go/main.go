package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginVersion       = "0.1.0"
	pluginSchemaVersion = 1
	resourcePath        = "/catalog"
	managementPath      = "/plugins/model-capabilities/catalog"

	// The protocol names are intentionally client-facing. They describe the
	// route a client should use against CPA, not the provider's private API.
	protocolOpenAIResponses = "openai-responses"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type pluginConfig struct {
	Enabled    bool                        `yaml:"enabled"`
	BaseURL    string                      `yaml:"base_url"`
	ModelsPath string                      `yaml:"models_path"`
	APIKey     string                      `yaml:"api_key"`
	Protocols  map[string]protocolOverride `yaml:"protocols"`
}

type protocolOverride struct {
	Preferred    string         `yaml:"preferred"`
	Supported    []string       `yaml:"supported"`
	Endpoints    []string       `yaml:"endpoints"`
	Capabilities map[string]any `yaml:"capabilities"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ManagementAPI bool `json:"management_api"`
}

type managementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
	Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
}

// rpcManagementRequest is the SDK request plus the callback identifier added
// by the native host adapter. The callback lets the plugin query CPA's own
// authenticated model-list endpoint without opening a second HTTP client.
type rpcManagementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type hostHTTPRequest struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	Method         string      `json:"method,omitempty"`
	URL            string      `json:"url,omitempty"`
	Headers        http.Header `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
}

type modelListResponse struct {
	Data []map[string]any `json:"data"`
}

type catalogDocument struct {
	SchemaVersion int              `json:"schema_version"`
	Object        string           `json:"object"`
	Revision      string           `json:"revision"`
	GeneratedAt   string           `json:"generated_at"`
	Source        catalogSource    `json:"source"`
	Models        []map[string]any `json:"models"`
}

type catalogSource struct {
	Type      string `json:"type"`
	ModelsURL string `json:"models_url"`
}

var activeConfig atomic.Pointer[pluginConfig]

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	activeConfig.Store(nil)
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginQuiesce:
		return okEnvelope(struct{}{})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	cfg := defaultPluginConfig()
	var request lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
			return fmt.Errorf("decode lifecycle request: %w", errUnmarshal)
		}
	}
	if len(request.ConfigYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(request.ConfigYAML, &cfg); errUnmarshal != nil {
			return fmt.Errorf("decode plugin config: %w", errUnmarshal)
		}
	}
	normalizeConfig(&cfg)
	activeConfig.Store(&cfg)
	return nil
}

func normalizeConfig(cfg *pluginConfig) {
	if cfg == nil {
		return
	}
	if cfg.Protocols == nil {
		cfg.Protocols = map[string]protocolOverride{}
	}
	normalized := make(map[string]protocolOverride, len(cfg.Protocols))
	for modelID, override := range cfg.Protocols {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			continue
		}
		override.Preferred = strings.TrimSpace(override.Preferred)
		override.Supported = normalizeStrings(override.Supported)
		override.Endpoints = normalizeStrings(override.Endpoints)
		if len(override.Capabilities) > 0 {
			override.Capabilities = cloneMap(override.Capabilities)
		}
		normalized[modelID] = override
	}
	cfg.Protocols = normalized
	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = "http://127.0.0.1:8317"
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if strings.TrimSpace(cfg.ModelsPath) == "" {
		cfg.ModelsPath = "/v1/models"
	}
	if !strings.HasPrefix(cfg.ModelsPath, "/") {
		cfg.ModelsPath = "/" + cfg.ModelsPath
	}
	cfg.ModelsPath = strings.TrimRight(strings.TrimSpace(cfg.ModelsPath), "/")
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
}

func defaultPluginConfig() pluginConfig {
	cfg := pluginConfig{
		Enabled:    true,
		BaseURL:    "http://127.0.0.1:8317",
		ModelsPath: "/v1/models",
		Protocols:  map[string]protocolOverride{},
	}
	return cfg
}

func currentConfig() pluginConfig {
	cfg := activeConfig.Load()
	if cfg == nil {
		return defaultPluginConfig()
	}
	return *cfg
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginSchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Model Capabilities",
			Version:          pluginVersion,
			Author:           "Whexy",
			GitHubRepository: "https://github.com/whexy/whexy-cpa-store",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Expose the live model capability catalog."},
				{Name: "base_url", Type: pluginapi.ConfigFieldTypeString, Description: "CPA base URL used by the host HTTP callback to fetch the live model list."},
				{Name: "models_path", Type: pluginapi.ConfigFieldTypeString, Description: "CPA model-list path, normally /v1/models."},
				{Name: "api_key", Type: pluginapi.ConfigFieldTypeString, Description: "Optional API key sent to CPA when its model-list route requires authentication."},
				{Name: "protocols", Type: pluginapi.ConfigFieldTypeObject, Description: "Per-model preferred/supported CPA protocols, endpoints, and capability metadata."},
			},
		},
		Capabilities: registrationCapability{ManagementAPI: true},
	}
}

func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{{
			Method:      http.MethodGet,
			Path:        managementPath,
			Description: "Live model capability catalog sourced from CLIProxyAPI's model registry.",
		}},
		Resources: []pluginapi.ResourceRoute{{
			Path:        resourcePath,
			Description: "Live model capability catalog sourced from CLIProxyAPI's model registry.",
		}},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var request rpcManagementRequest
	if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
		return nil, fmt.Errorf("decode management request: %w", errUnmarshal)
	}
	if request.Method != "" && !strings.EqualFold(request.Method, http.MethodGet) {
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusMethodNotAllowed,
			Headers:    jsonHeaders(),
			Body:       []byte(`{"error":"method not allowed"}`),
		})
	}
	if !strings.HasSuffix(strings.TrimRight(request.Path, "/"), resourcePath) {
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusNotFound,
			Headers:    jsonHeaders(),
			Body:       []byte(`{"error":"unknown catalog route"}`),
		})
	}
	return okEnvelope(renderCatalog(request.Headers, request.HostCallbackID))
}

func renderCatalog(headers http.Header, hostCallbackID string) pluginapi.ManagementResponse {
	cfg := currentConfig()
	if !cfg.Enabled {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusNotFound,
			Headers:    jsonHeaders(),
			Body:       []byte(`{"error":"model capabilities are disabled"}`),
		}
	}

	models, errFetch := fetchModels(cfg, hostCallbackID)
	if errFetch != nil {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusBadGateway,
			Headers:    jsonHeaders(),
			Body:       []byte(fmt.Sprintf(`{"error":%q}`, errFetch.Error())),
		}
	}
	return renderCatalogModels(headers, cfg, models)
}

func renderCatalogModels(headers http.Header, cfg pluginConfig, models []map[string]any) pluginapi.ManagementResponse {
	if !cfg.Enabled {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusNotFound,
			Headers:    jsonHeaders(),
			Body:       []byte(`{"error":"model capabilities are disabled"}`),
		}
	}
	modelMaps := buildModelMaps(models, cfg)
	etag := catalogETag(modelMaps)
	responseHeaders := jsonHeaders()
	responseHeaders.Set("ETag", etag)
	responseHeaders.Set("Cache-Control", "no-cache")
	responseHeaders.Set("Vary", "If-None-Match")
	if etagMatches(headers.Get("If-None-Match"), etag) {
		return pluginapi.ManagementResponse{StatusCode: http.StatusNotModified, Headers: responseHeaders}
	}

	document := catalogDocument{
		SchemaVersion: 1,
		Object:        "model_capabilities",
		Revision:      strings.Trim(etag, `"`),
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		Source: catalogSource{
			Type:      "cliproxyapi-models-endpoint",
			ModelsURL: modelsURL(cfg),
		},
		Models: modelMaps,
	}
	body, errMarshal := json.Marshal(document)
	if errMarshal != nil {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusInternalServerError,
			Headers:    jsonHeaders(),
			Body:       []byte(`{"error":"encode model catalog"}`),
		}
	}
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: responseHeaders, Body: body}
}

func fetchModels(cfg pluginConfig, hostCallbackID string) ([]map[string]any, error) {
	if strings.TrimSpace(hostCallbackID) == "" {
		return nil, fmt.Errorf("host callback is unavailable")
	}
	request := hostHTTPRequest{
		HostCallbackID: hostCallbackID,
		Method:         http.MethodGet,
		URL:            modelsURL(cfg),
		Headers:        http.Header{"Accept": []string{"application/json"}},
	}
	if cfg.APIKey != "" {
		request.Headers.Set("Authorization", "Bearer "+strings.TrimPrefix(strings.TrimSpace(cfg.APIKey), "Bearer "))
	}
	raw, errCall := callHost(pluginabi.MethodHostHTTPDo, request)
	if errCall != nil {
		return nil, fmt.Errorf("fetch model list: %w", errCall)
	}
	var response pluginapi.HTTPResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		return nil, fmt.Errorf("decode model-list response: %w", errUnmarshal)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("model-list endpoint returned HTTP %d", response.StatusCode)
	}
	var payload modelListResponse
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode model-list JSON: %w", errUnmarshal)
	}
	return payload.Data, nil
}

func modelsURL(cfg pluginConfig) string {
	return strings.TrimRight(cfg.BaseURL, "/") + "/" + strings.TrimLeft(cfg.ModelsPath, "/")
}

func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal host callback %s: %w", method, errMarshal)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response, code=%d", method, int(callCode))
	}

	var env envelope
	if errUnmarshal := json.Unmarshal(rawResponse, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host envelope %s: %w", method, errUnmarshal)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, int(callCode))
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

func buildModelMaps(models []map[string]any, cfg pluginConfig) []map[string]any {
	result := make([]map[string]any, 0, len(models))
	for _, model := range models {
		modelID, _ := model["id"].(string)
		modelID = strings.TrimSpace(modelID)
		if model == nil || modelID == "" {
			continue
		}
		modelMap := cloneMap(model)
		// ModelConfig contains runtime header overrides and is not catalog metadata.
		delete(modelMap, "config")
		if override, ok := cfg.Protocols[modelID]; ok {
			applyProtocolOverride(modelMap, override)
		} else {
			modelMap["protocol"] = nil
			modelMap["protocols"] = []string{}
			modelMap["endpoints"] = []string{}
			modelMap["capabilities"] = nil
			modelMap["protocol_source"] = "unknown"
		}
		result = append(result, modelMap)
	}
	sort.Slice(result, func(i, j int) bool {
		left, _ := result[i]["id"].(string)
		right, _ := result[j]["id"].(string)
		return left < right
	})
	return result
}

func applyProtocolOverride(model map[string]any, override protocolOverride) {
	preferred := strings.TrimSpace(override.Preferred)
	if preferred == "" {
		model["protocol"] = nil
	} else {
		model["protocol"] = preferred
	}
	supported := append([]string(nil), override.Supported...)
	if len(supported) == 0 && preferred != "" {
		supported = []string{preferred}
	}
	model["protocols"] = supported
	endpoints := append([]string(nil), override.Endpoints...)
	if len(endpoints) == 0 && preferred != "" {
		endpoints = defaultEndpoints(preferred)
	}
	model["endpoints"] = endpoints
	if len(override.Capabilities) == 0 {
		model["capabilities"] = nil
	} else {
		model["capabilities"] = cloneMap(override.Capabilities)
	}
	model["protocol_source"] = "config"
}

func defaultEndpoints(protocol string) []string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case protocolOpenAIResponses:
		return []string{"/v1/responses"}
	case "openai-chat-completions":
		return []string{"/v1/chat/completions"}
	case "anthropic-messages":
		return []string{"/v1/messages"}
	case "google-generative-ai":
		return []string{"/v1beta/models/*:generateContent"}
	default:
		return nil
	}
}

func catalogETag(models []map[string]any) string {
	canonical, errMarshal := json.Marshal(struct {
		SchemaVersion int              `json:"schema_version"`
		Models        []map[string]any `json:"models"`
	}{SchemaVersion: 1, Models: models})
	if errMarshal != nil {
		return `"0"`
	}
	digest := sha256.Sum256(canonical)
	return `"` + hex.EncodeToString(digest[:]) + `"`
}

func etagMatches(value, etag string) bool {
	for _, candidate := range strings.Split(value, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

func normalizeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func cloneMap(value map[string]any) map[string]any {
	if len(value) == 0 {
		return nil
	}
	result := make(map[string]any, len(value))
	for key, item := range value {
		switch nested := item.(type) {
		case map[string]any:
			result[key] = cloneMap(nested)
		case []any:
			result[key] = append([]any(nil), nested...)
		default:
			result[key] = item
		}
	}
	return result
}

func jsonHeaders() http.Header {
	return http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}
}

func okEnvelope(value any) ([]byte, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
