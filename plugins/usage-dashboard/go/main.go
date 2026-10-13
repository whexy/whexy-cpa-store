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
	"path"
	"path/filepath"
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
	pluginVersion = "0.3.0"

	pluginID    = "usage-dashboard"
	displayName = "Usage Dashboard"
	menuLabel   = "Usage"

	defaultRetentionDays = 30
	maxRetentionDays     = 400
	defaultPricingURL    = "https://models.dev/api.json"

	// Background work gets this long to observe cancellation before the host
	// unloads the library underneath it.
	shutdownGrace = 5 * time.Second
)

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

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	UsagePlugin   bool `json:"usage_plugin"`
	ManagementAPI bool `json:"management_api"`
}

// settings are the plugin-owned values under plugins.configs.usage-dashboard.
type settings struct {
	// DataDir is absolute; empty until configure resolves the default.
	DataDir       string
	RetentionDays int
	PricingURL    string
	// FamilyRules are the configured model_families overrides, matched in
	// pattern order before the built-in rules.
	FamilyRules []familyRule
}

type rawSettings struct {
	DataDir       string            `yaml:"data_dir"`
	RetentionDays int               `yaml:"retention_days"`
	Pricing       *bool             `yaml:"pricing"`
	PricingURL    string            `yaml:"pricing_url"`
	ModelFamilies map[string]string `yaml:"model_families"`
}

var currentConfig atomic.Pointer[settings]

func currentSettings() settings {
	if cfg := currentConfig.Load(); cfg != nil {
		return *cfg
	}
	return settings{RetentionDays: defaultRetentionDays, PricingURL: defaultPricingURL}
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
	shutdown()
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginQuiesce:
		if active := activeStore(); active != nil {
			active.quiesce()
		}
		return okEnvelope(struct{}{})
	case pluginabi.MethodPluginShutdown:
		shutdown()
		return okEnvelope(struct{}{})

	case pluginabi.MethodUsageHandle:
		return handleUsage(request)

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
			Logo:             logoPath,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "data_dir", Type: pluginapi.ConfigFieldTypeString, Description: "Directory for the usage files. Default: data/usage-dashboard beside the plugins directory that holds this plugin, so it lands on the same volume. Relative paths resolve from CLIProxyAPI's working directory."},
				{Name: "retention_days", Type: pluginapi.ConfigFieldTypeInteger, Description: fmt.Sprintf("Days of usage to keep, 1 to %d. Default %d. The page offers the 90-day range only when this is at least 90.", maxRetentionDays, defaultRetentionDays)},
				{Name: "pricing", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Estimate cost from models.dev prices. Default true."},
				{Name: "pricing_url", Type: pluginapi.ConfigFieldTypeString, Description: "models.dev-format price catalog. Default " + defaultPricingURL + "."},
				{Name: "model_families", Type: pluginapi.ConfigFieldTypeObject, Description: "Extra model family rules: a glob over the model ID (without any provider prefix) mapped to a family name, e.g. {\"my-coder-*\": \"Qwen\"}. Checked before the built-in rules."},
			},
		},
		Capabilities: registrationCapability{
			UsagePlugin:   true,
			ManagementAPI: true,
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
	if cfg.DataDir == "" {
		cfg.DataDir = defaultDataDir()
	}
	currentConfig.Store(&cfg)
	return startRuntime(cfg)
}

func parseSettings(configYAML []byte) (settings, error) {
	var raw rawSettings
	if len(configYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(configYAML, &raw); errUnmarshal != nil {
			return settings{}, fmt.Errorf("decode plugin config: %w", errUnmarshal)
		}
	}
	cfg := settings{RetentionDays: defaultRetentionDays, PricingURL: defaultPricingURL}
	if dir := strings.TrimSpace(raw.DataDir); dir != "" {
		abs, errAbs := filepath.Abs(dir)
		if errAbs != nil {
			return settings{}, fmt.Errorf("resolve data_dir %q: %w", dir, errAbs)
		}
		cfg.DataDir = abs
	}
	if raw.RetentionDays != 0 {
		if raw.RetentionDays < 1 || raw.RetentionDays > maxRetentionDays {
			return settings{}, fmt.Errorf("retention_days must be between 1 and %d, got %d", maxRetentionDays, raw.RetentionDays)
		}
		cfg.RetentionDays = raw.RetentionDays
	}
	if raw.Pricing != nil && !*raw.Pricing {
		cfg.PricingURL = ""
	} else if value := strings.TrimSpace(raw.PricingURL); value != "" {
		parsed, errParse := url.Parse(value)
		if errParse != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			return settings{}, fmt.Errorf("pricing_url must be an absolute http(s) URL, got %q", value)
		}
		cfg.PricingURL = value
	}
	rules, errRules := parseFamilyRules(raw.ModelFamilies)
	if errRules != nil {
		return settings{}, errRules
	}
	cfg.FamilyRules = rules
	return cfg, nil
}

func parseFamilyRules(values map[string]string) ([]familyRule, error) {
	rules := make([]familyRule, 0, len(values))
	for pattern, family := range values {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		family = strings.TrimSpace(family)
		if pattern == "" || family == "" {
			continue
		}
		if _, errMatch := path.Match(pattern, ""); errMatch != nil {
			return nil, fmt.Errorf("model_families pattern %q: %w", pattern, errMatch)
		}
		rules = append(rules, familyRule{Pattern: pattern, Family: family})
	}
	sortFamilyRules(rules)
	return rules, nil
}

// Work that outlives a plugin call (flushing, price refresh) runs under
// backgroundCtx so shutdown can stop it before the host unloads the library.
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
				hostLog("error", fmt.Sprintf("background task panicked: %v", recovered))
			}
		}()
		fn(backgroundCtx)
	}()
}

var shutdownOnce sync.Once

// shutdown stops background work and writes everything still in memory. The
// host may call it twice: plugin.shutdown, then the C shutdown hook.
func shutdown() {
	shutdownOnce.Do(func() {
		cancelBackground()
		done := make(chan struct{})
		go func() {
			backgroundWork.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(shutdownGrace):
		}
		if active := activeStore(); active != nil {
			active.close()
		}
	})
}

func okEnvelope(value any) ([]byte, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string, httpStatus ...int) []byte {
	status := 0
	if len(httpStatus) > 0 {
		status = httpStatus[0]
	}
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message, HTTPStatus: status}})
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
