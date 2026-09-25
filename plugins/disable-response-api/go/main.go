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

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
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
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginVersion = "0.2.0"
	// The request interceptor capability has used the same RPC shape since
	// schema v1; advertising the SDK's latest schema would unnecessarily reject older hosts.
	pluginSchemaVersion uint32 = 1

	// Matches the host's handler type for /v1/responses and /v1/responses/compact.
	openAIResponsesFormat = "openai-response"
	disableHeader         = "WHEXY_CPA_DISABLE_RESPONSE_API"
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
	APIKeys []string `yaml:"api_keys"`
}

var blockedAPIKeys atomic.Pointer[map[string]struct{}]

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	RequestInterceptor bool `json:"request_interceptor"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
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
func cliproxyPluginShutdown() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginQuiesce:
		return okEnvelope(struct{}{})
	case pluginabi.MethodRequestInterceptBefore:
		return interceptBeforeAuth(request)
	case pluginabi.MethodRequestInterceptAfter:
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginSchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Disable Response API",
			Version:          pluginVersion,
			Author:           "Whexy",
			GitHubRepository: "https://github.com/whexy/whexy-cpa-store",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "api_keys", Type: pluginapi.ConfigFieldTypeArray, Description: "Client API keys whose OpenAI Responses API requests are rejected with an empty 404."},
			},
		},
		Capabilities: registrationCapability{RequestInterceptor: true},
	}
}

func configure(raw []byte) error {
	var request lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
			return fmt.Errorf("decode lifecycle request: %w", errUnmarshal)
		}
	}
	var cfg pluginConfig
	if len(request.ConfigYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(request.ConfigYAML, &cfg); errUnmarshal != nil {
			return fmt.Errorf("decode plugin config: %w", errUnmarshal)
		}
	}
	keys := make(map[string]struct{}, len(cfg.APIKeys))
	for _, key := range cfg.APIKeys {
		if key = strings.TrimSpace(key); key != "" {
			keys[key] = struct{}{}
		}
	}
	blockedAPIKeys.Store(&keys)
	return nil
}

func interceptBeforeAuth(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode request intercept request: %w", errUnmarshal)
	}
	return okEnvelope(decide(req))
}

func decide(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	if req.SourceFormat != openAIResponsesFormat || (!hasDisableHeader(req.Headers) && !usesBlockedAPIKey(req.Headers)) {
		return pluginapi.RequestInterceptResponse{}
	}
	return pluginapi.RequestInterceptResponse{
		Terminate:  true,
		StatusCode: http.StatusNotFound,
	}
}

// Go keeps underscore header names non-canonical, and reverse proxies such as
// nginx drop them by default, so the hyphenated spelling is accepted as well.
func hasDisableHeader(headers http.Header) bool {
	for key := range headers {
		if strings.EqualFold(strings.ReplaceAll(key, "-", "_"), disableHeader) {
			return true
		}
	}
	return false
}

// Mirrors the header sources of CLIProxyAPI's config API key provider. Keys sent
// as query parameters are not visible to request interceptors.
func usesBlockedAPIKey(headers http.Header) bool {
	keys := blockedAPIKeys.Load()
	if keys == nil || len(*keys) == 0 {
		return false
	}
	candidates := []string{
		bearerToken(headers.Get("Authorization")),
		headers.Get("X-Api-Key"),
		headers.Get("X-Goog-Api-Key"),
	}
	for _, candidate := range candidates {
		if _, ok := (*keys)[candidate]; ok && candidate != "" {
			return true
		}
	}
	return false
}

func bearerToken(header string) string {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return header
	}
	return strings.TrimSpace(token)
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
