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

// The host keeps its API table alive until the plugin is shut down.
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
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginVersion = "0.0.0" // x-release-please-version

	providerKey        = "opencode-go"
	displayName        = "OpenCode Go"
	defaultBaseURL     = "https://opencode.ai/inference/go"
	defaultModelPrefix = "opencode-go"
	// Management Center turns an emptied string field into null, so an empty
	// model_prefix cannot mean "no prefix".
	noModelPrefix = "none"

	// Background work gets this long to observe cancellation before the host
	// unloads the library underneath it.
	shutdownGrace = 5 * time.Second
)

var executorFormats = []string{"openai", "claude", "openai-response"}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	AuthProvider          bool                         `json:"auth_provider"`
	ModelProvider         bool                         `json:"model_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats"`
	QuotaProvider         bool                         `json:"quota_provider"`
	ManagementAPI         bool                         `json:"management_api"`
}

// settings are the plugin-owned values under plugins.configs.opencode-go.
type settings struct {
	BaseURL     string
	ModelPrefix string
	UserAgent   string
}

type rawSettings struct {
	BaseURL     string `yaml:"base_url"`
	ModelPrefix string `yaml:"model_prefix"`
	UserAgent   string `yaml:"user_agent"`
}

var currentConfig atomic.Pointer[settings]

func currentSettings() settings {
	if cfg := currentConfig.Load(); cfg != nil {
		return *cfg
	}
	return defaultSettings()
}

func defaultSettings() settings {
	return settings{BaseURL: defaultBaseURL, ModelPrefix: defaultModelPrefix}
}

// userAgent is the User-Agent for requests the plugin originates; the
// executor prefers the client's own.
func (s settings) userAgent() string {
	if s.UserAgent != "" {
		return s.UserAgent
	}
	return "whexy-cpa-opencode-go/" + pluginVersion
}

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
	raw, errHandle := safeHandleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

// safeHandleMethod contains panics: an unrecovered panic in a c-shared
// library aborts the whole host process.
func safeHandleMethod(method string, request []byte) (raw []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			raw = nil
			err = fmt.Errorf("%s panicked: %v", method, recovered)
		}
	}()
	return handleMethod(method, request)
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	stopBackground(shutdownGrace)
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return okEnvelope(struct{}{})

	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier, pluginabi.MethodQuotaIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerKey})
	case pluginabi.MethodAuthParse:
		return handleAuthParse(request)
	case pluginabi.MethodAuthLoginStart, pluginabi.MethodAuthLoginPoll:
		return errorEnvelope("unsupported", "OpenCode Go has no login flow; add keys on the OpenCode Go page in Management Center", 400), nil
	case pluginabi.MethodAuthRefresh:
		// Keys do not expire; an empty auth tells the host to keep the record as is.
		return okEnvelope(pluginapi.AuthRefreshResponse{})

	case pluginabi.MethodModelStatic:
		return okEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	case pluginabi.MethodModelForAuth:
		return handleModelsForAuth(request)

	case pluginabi.MethodExecutorExecute:
		return executorExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return executorExecuteStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return executorCountTokens(request)
	case pluginabi.MethodExecutorHTTPRequest:
		return executorHTTPRequest(request)

	case pluginabi.MethodQuotaDescribe:
		return okEnvelope(pluginapi.QuotaDescribeResponse{SupportedProviders: []string{providerKey}, DisplayName: displayName})
	case pluginabi.MethodQuotaFetch:
		return handleQuotaFetch(request)
	case pluginabi.MethodQuotaReset:
		return errorEnvelope("unsupported", "OpenCode Go usage cannot be reset", 400), nil

	case pluginabi.MethodManagementRegister:
		return managementRegister(request)
	case pluginabi.MethodManagementHandle:
		return managementHandle(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             displayName,
			Version:          pluginVersion,
			Author:           "Whexy",
			GitHubRepository: "https://github.com/whexy/whexy-cpa-store",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "base_url", Type: pluginapi.ConfigFieldTypeString, Description: "OpenCode Go gateway base URL. Default " + defaultBaseURL + "."},
				{Name: "model_prefix", Type: pluginapi.ConfigFieldTypeString, Description: "Prefix of the exposed model IDs, as in opencode-go/kimi-k3. Default " + defaultModelPrefix + ". Set to none to expose bare IDs, which then share routing with any other provider serving the same ID."},
				{Name: "user_agent", Type: pluginapi.ConfigFieldTypeString, Description: "User-Agent sent upstream when the client sends none. Default whexy-cpa-opencode-go/<version>."},
			},
		},
		Capabilities: registrationCapability{
			AuthProvider:  true,
			ModelProvider: true,
			Executor:      true,
			// Only per-auth models: the catalog depends on the key, and static
			// models whose ID a native provider serves are dropped by the host.
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  executorFormats,
			ExecutorOutputFormats: executorFormats,
			QuotaProvider:         true,
			ManagementAPI:         true,
		},
	}
}

func configure(raw []byte) error {
	var request lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
			return fmt.Errorf("decode lifecycle request: %w", errUnmarshal)
		}
	}
	cfg, errParse := parseSettings(request.ConfigYAML)
	if errParse != nil {
		return errParse
	}
	currentConfig.Store(&cfg)
	return nil
}

func parseSettings(configYAML []byte) (settings, error) {
	var raw rawSettings
	if len(configYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(configYAML, &raw); errUnmarshal != nil {
			return settings{}, fmt.Errorf("decode plugin config: %w", errUnmarshal)
		}
	}
	cfg := defaultSettings()
	if base := strings.TrimRight(strings.TrimSpace(raw.BaseURL), "/"); base != "" {
		parsed, errParse := url.Parse(base)
		if errParse != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			return settings{}, fmt.Errorf("base_url must be an absolute http(s) URL, got %q", base)
		}
		cfg.BaseURL = base
	}
	if prefix := strings.Trim(strings.TrimSpace(raw.ModelPrefix), "/"); prefix != "" {
		if strings.EqualFold(prefix, noModelPrefix) {
			prefix = ""
		} else if strings.ContainsAny(prefix, " \t\r\n()") {
			return settings{}, fmt.Errorf("model_prefix must not contain spaces or parentheses, got %q", prefix)
		}
		cfg.ModelPrefix = prefix
	}
	cfg.UserAgent = strings.TrimSpace(raw.UserAgent)
	return cfg, nil
}

// Work that outlives a plugin call (stream pumps) runs under backgroundCtx so
// shutdown can stop it before the host unloads the library.
var (
	backgroundCtx, cancelBackground = context.WithCancel(context.Background())
	backgroundWork                  sync.WaitGroup
)

// startBackground runs fn on its own goroutine under the plugin lifetime
// context; fn must return promptly once ctx is done. A panic in fn is
// logged instead of aborting the host.
func startBackground(fn func(ctx context.Context)) {
	backgroundWork.Add(1)
	go func() {
		defer backgroundWork.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				newHost("").Log("error", fmt.Sprintf("background task panicked: %v", recovered), nil)
			}
		}()
		fn(backgroundCtx)
	}()
}

func stopBackground(grace time.Duration) {
	cancelBackground()
	done := make(chan struct{})
	go func() {
		backgroundWork.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
	}
}

var secretPattern = regexp.MustCompile(`oc_sk_[A-Za-z0-9_\-]+`)

// redactSecrets removes OpenCode keys from text the plugin returns or logs;
// upstream error bodies may echo the key back.
func redactSecrets(text string) string {
	return secretPattern.ReplaceAllString(text, "oc_sk_[redacted]")
}

func okEnvelope(value any) ([]byte, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

// errorEnvelope builds a failed RPC result. The host forwards httpStatus to
// the client and to its cooldown logic, and drops the code.
func errorEnvelope(code, message string, httpStatus ...int) []byte {
	status := 0
	if len(httpStatus) > 0 {
		status = httpStatus[0]
	}
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: redactSecrets(message), HTTPStatus: status}})
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

func init() {
	callHost = callNativeHost
}

func callNativeHost(method string, payload []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var requestPtr *C.uint8_t
	if len(payload) > 0 {
		cPayload := C.CBytes(payload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback payload for %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	var response C.cliproxy_buffer
	code := C.call_host_api(cMethod, requestPtr, C.size_t(len(payload)), &response)
	var raw []byte
	if response.ptr != nil && response.len > 0 {
		raw = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response (code %d)", method, int(code))
	}
	return raw, nil
}
