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
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginVersion = "0.1.0"
	// The request interceptor capability has used the same RPC shape since
	// schema v1; advertising the SDK's latest schema would unnecessarily reject older hosts.
	pluginSchemaVersion uint32 = 1

	optInHeader     = "X-CPA-Session-Guard"
	ackHeader       = "X-CPA-Session-Migration-Ack"
	migrationHeader = "X-CPA-Session-Migration"

	defaultBindingTTL = 24 * time.Hour

	// Keys of the execution metadata the host publishes to request
	// interceptors after credential selection.
	metadataSelectedAuthID    = "selected_auth_id"
	metadataSelectedAuthIndex = "selected_auth_index"
	metadataSessionID         = "canonical_session_id"
	metadataCallerScope       = "caller_scope"
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
	BindingTTL string `yaml:"binding_ttl"`
}

var sessions = newGuard(defaultBindingTTL)

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	RequestInterceptor bool `json:"request_interceptor"`
}

// rejection is shaped to parse as both an Anthropic and an OpenAI error body.
type rejection struct {
	Type           string         `json:"type"`
	Error          rejectionError `json:"error"`
	SessionBinding *bindingView   `json:"session_binding,omitempty"`
}

type rejectionError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
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
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	case pluginabi.MethodRequestInterceptAfter:
		return interceptAfterAuth(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginSchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Session Credential Guard",
			Version:          pluginVersion,
			Author:           "Whexy",
			GitHubRepository: "https://github.com/whexy/whexy-cpa-store",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "binding_ttl", Type: pluginapi.ConfigFieldTypeString, Description: "How long an idle session keeps its credential binding, as a Go duration. Default 24h."},
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
	ttl := defaultBindingTTL
	if value := strings.TrimSpace(cfg.BindingTTL); value != "" {
		parsed, errParse := time.ParseDuration(value)
		if errParse != nil || parsed <= 0 {
			return fmt.Errorf("binding_ttl must be a positive duration, got %q", cfg.BindingTTL)
		}
		ttl = parsed
	}
	sessions.setTTL(ttl)
	return nil
}

// The host fails open when an interceptor returns an error, so every decision,
// including rejection, is returned as a successful envelope.
func interceptAfterAuth(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode request intercept request: %w", errUnmarshal)
	}
	return okEnvelope(decide(sessions, req))
}

func decide(g *guard, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	ack := headerValue(req.Headers, ackHeader)
	if ack == "" && !hasHeader(req.Headers, optInHeader) {
		return pluginapi.RequestInterceptResponse{}
	}
	// Without the host's session identity or selected credential there is
	// nothing to bind; such hosts or routes behave as if the plugin were absent.
	sessionID := metadataString(req.Metadata, metadataSessionID)
	authID := metadataString(req.Metadata, metadataSelectedAuthID)
	if sessionID == "" || authID == "" {
		return pluginapi.RequestInterceptResponse{}
	}
	model := req.RequestedModel
	if model == "" {
		model = req.Model
	}
	key := sessionKey{
		callerScope: metadataString(req.Metadata, metadataCallerScope),
		sessionID:   sessionID,
		model:       model,
	}
	sel := selection{authID: authID, authIndex: metadataString(req.Metadata, metadataSelectedAuthIndex)}
	v := g.decide(key, sel, ack)
	if v.allow {
		return pluginapi.RequestInterceptResponse{}
	}
	return rejectionResponse(v)
}

func rejectionResponse(v verdict) pluginapi.RequestInterceptResponse {
	body, _ := json.Marshal(rejection{
		Type: "error",
		Error: rejectionError{
			Type:    strings.ToLower(v.code),
			Code:    v.code,
			Message: rejectionMessage(v),
		},
		SessionBinding: v.binding,
	})
	headers := http.Header{"Content-Type": {"application/json"}}
	if v.binding != nil && v.binding.Pending != nil {
		headers.Set(migrationHeader, v.binding.Pending.ID)
	}
	return pluginapi.RequestInterceptResponse{
		Terminate:       true,
		StatusCode:      http.StatusConflict,
		ResponseHeaders: headers,
		ResponseBody:    body,
	}
}

func rejectionMessage(v verdict) string {
	switch v.code {
	case codeMigrationRequired:
		return fmt.Sprintf("CLIProxyAPI selected a different credential for this session. The request was not sent upstream. Retry with header %s: %s to move the session to that credential.", ackHeader, v.binding.Pending.ID)
	case codeTargetUnavailable:
		return fmt.Sprintf("The credential targeted by the acknowledged migration is no longer selected. The migration was invalidated and the session stays on its current credential. Retry without %s.", ackHeader)
	default:
		if v.binding != nil && v.binding.Pending != nil {
			return fmt.Sprintf("%s does not name this session's pending migration. Acknowledge %s instead, or retry without the header.", ackHeader, v.binding.Pending.ID)
		}
		return fmt.Sprintf("%s does not name a pending migration of this session. Retry without the header.", ackHeader)
	}
}

func hasHeader(headers http.Header, name string) bool {
	for key := range headers {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

func headerValue(headers http.Header, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

func metadataString(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
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
