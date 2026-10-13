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
	"sync"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	pluginVersion = "0.1.2"

	// Background work gets this long to observe cancellation before the host
	// unloads the library underneath it.
	shutdownGrace = 2 * time.Second
	// providerRefreshInterval keeps the cached credential view fresh enough
	// for the page without polling the host on the request path.
	providerRefreshInterval = 15 * time.Second
	// persistInterval bounds how much a crash can lose of the session
	// bindings.
	persistInterval = 30 * time.Second
)

var (
	// pluginStateRef is the process-wide state; a plugin library has exactly
	// one.
	pluginStateRef = newPluginState(defaultDataDir())
	pluginStart    = time.Now()
	loopsOnce      sync.Once
)

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelRegistrar         bool `json:"model_registrar"`
	ModelRouter            bool `json:"model_router"`
	RequestInterceptor     bool `json:"request_interceptor"`
	RequestLifecyclePlugin bool `json:"request_lifecycle_plugin"`
	ManagementAPI          bool `json:"management_api"`
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
			// CPA HTML-escapes these descriptions and Management Center shows
			// the escapes literally, so they avoid angle brackets, ampersands
			// and quotes.
			ConfigFields: []pluginapi.ConfigField{
				{Name: "prefix", Type: pluginapi.ConfigFieldTypeString, Description: "Prefix of every group model ID, as in " + defaultPrefix + "/fast. Default " + defaultPrefix + "."},
				{Name: "affinity.enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Keep a session on the member it first used, which is what keeps CLIProxyAPI on one account for that session. Default true."},
				{Name: "affinity.ttl", Type: pluginapi.ConfigFieldTypeString, Description: "How long a session keeps its member, as a duration such as 1h or 24h. Default 1h, CLIProxyAPI's own default for routing.session-affinity-ttl; keep it at least that long."},
				{Name: "affinity.rebind_on_failure", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Move a session to another member as soon as its member fails. Default false, so a session only moves once its member has no credentials left."},
				{Name: "groups", Type: pluginapi.ConfigFieldTypeArray, Description: "The groups and their members. Manage them on the plugin page."},
			},
		},
		Capabilities: registrationCapability{
			ModelRegistrar:         true,
			ModelRouter:            true,
			RequestInterceptor:     true,
			RequestLifecyclePlugin: true,
			ManagementAPI:          true,
		},
	}
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

// safeHandleMethod contains panics: an unrecovered panic in a c-shared library
// aborts the whole host process.
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
	pluginStateRef.persist()
	stopBackground(shutdownGrace)
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		startBackgroundLoops()
		return okEnvelope(pluginRegistration())

	case pluginabi.MethodPluginQuiesce:
		return okEnvelope(struct{}{})

	case pluginabi.MethodPluginShutdown:
		pluginStateRef.persist()
		return okEnvelope(struct{}{})

	case pluginabi.MethodModelRegister:
		return okEnvelope(registerModels())

	case pluginabi.MethodModelRoute:
		return routeModel(request)

	case pluginabi.MethodRequestInterceptAfter:
		return interceptRequestAfterAuth(request)

	case pluginabi.MethodRequestComplete:
		return handleRequestComplete(request)

	case pluginabi.MethodManagementRegister:
		return managementRegister()

	case pluginabi.MethodManagementHandle:
		return managementHandle(request)

	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// configure reads the plugin's own node of config.yaml, which the host forces
// enabled and priority into.
func configure(raw []byte) error {
	var request lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
			return fmt.Errorf("decode lifecycle request: %w", errUnmarshal)
		}
	}
	pluginStateRef.applyConfig(request.ConfigYAML)
	return nil
}

// registerModels publishes one model ID per enabled group.
func registerModels() pluginapi.ModelRegistrationResponse {
	cfg := pluginStateRef.settingsSnapshot()
	models := make([]pluginapi.ModelInfo, 0, len(cfg.Groups))
	for _, group := range cfg.Groups {
		if !group.Enabled {
			continue
		}
		models = append(models, pluginapi.ModelInfo{
			ID:          group.ID,
			Object:      "model",
			Created:     pluginStart.Unix(),
			OwnedBy:     pluginID,
			DisplayName: group.Name,
		})
	}
	return pluginapi.ModelRegistrationResponse{Provider: providerKey, Models: models}
}

func handleRequestComplete(raw []byte) ([]byte, error) {
	var completion pluginapi.RequestCompletion
	if errUnmarshal := json.Unmarshal(raw, &completion); errUnmarshal != nil {
		return nil, fmt.Errorf("decode request completion: %w", errUnmarshal)
	}
	pluginStateRef.observeCompletion(completion)
	return okEnvelope(struct{}{})
}

// Work that outlives a plugin call (the refresh and persistence loops) runs
// under backgroundCtx so shutdown can stop it before the host unloads the
// library.
var (
	backgroundCtx, cancelBackground = context.WithCancel(context.Background())
	backgroundWork                  sync.WaitGroup
)

func startBackgroundLoops() {
	loopsOnce.Do(func() {
		startBackground(func(ctx context.Context) {
			pluginStateRef.catalog.refresh()
			ticker := time.NewTicker(providerRefreshInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					pluginStateRef.catalog.refresh()
				}
			}
		})
		startBackground(func(ctx context.Context) {
			ticker := time.NewTicker(persistInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					pluginStateRef.persist()
				}
			}
		})
	})
}

// startBackground runs fn on its own goroutine under the plugin lifetime
// context; fn must return promptly once ctx is done. A panic in fn is logged
// instead of aborting the host.
func startBackground(fn func(ctx context.Context)) {
	backgroundWork.Add(1)
	go func() {
		defer backgroundWork.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				hostLog("error", fmt.Sprintf("model-groups: background task panicked: %v", recovered), nil)
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
