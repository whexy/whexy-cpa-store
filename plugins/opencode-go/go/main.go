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
	"slices"
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

	providerKey = "opencode-go"
	displayName = "OpenCode Go"
	// zenProviderKey is the Console provider that holds the pay-as-you-go
	// (OpenCode Zen) models of the same workspace, next to providerKey's
	// subscription models in one config document.
	zenProviderKey = "opencode"
	defaultBaseURL = "https://opencode.ai/inference/go"
	// defaultZenBaseURL is the root of the pay-as-you-go inference paths; the
	// Go gateway is its /go subtree.
	defaultZenBaseURL     = "https://opencode.ai/inference"
	defaultConsoleURL     = "https://opencode.ai/console"
	defaultModelPrefix    = "opencode-go"
	defaultZenModelPrefix = "opencode"

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
	// RetryAfterSeconds tells CPA how long to cool the failed (auth, model)
	// down; older CPA versions ignore it and use their generic 429 backoff.
	RetryAfterSeconds float64 `json:"retry_after_seconds,omitempty"`
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
	BaseURL string
	// ConsoleURL is empty unless configured; consoleURL derives it.
	ConsoleURL string
	// ModelPrefix is the configured prefix; prefix says whether IDs carry it.
	ModelPrefix   string
	DisablePrefix bool
	// DisabledModels are Go model IDs the plugin keeps out of CPA. The plugin
	// owns this list because CPA's oauth-excluded-models matches registered
	// IDs, whose form depends on DisablePrefix.
	DisabledModels []string
	// ZenModelPrefix is the prefix of OpenCode Zen (pay-as-you-go) model IDs.
	ZenModelPrefix string
	// ZenModels are the bare OpenCode Zen model IDs the plugin serves as
	// <ZenModelPrefix>/<id>. The page edits this list; a Zen model stays out
	// of CPA until it is named here, since those calls bill Console credits.
	ZenModels []string
	UserAgent string
}

type rawSettings struct {
	BaseURL        string   `yaml:"base_url"`
	ConsoleURL     string   `yaml:"console_url"`
	ModelPrefix    string   `yaml:"model_prefix"`
	DisablePrefix  bool     `yaml:"disable_prefix"`
	DisabledModels []string `yaml:"disabled_models"`
	ZenModelPrefix string   `yaml:"zen_model_prefix"`
	ZenModels      []string `yaml:"zen_models"`
	UserAgent      string   `yaml:"user_agent"`
}

var currentConfig atomic.Pointer[settings]

func currentSettings() settings {
	if cfg := currentConfig.Load(); cfg != nil {
		return *cfg
	}
	return defaultSettings()
}

func defaultSettings() settings {
	return settings{BaseURL: defaultBaseURL, ModelPrefix: defaultModelPrefix, ZenModelPrefix: defaultZenModelPrefix}
}

// prefix is the prefix every exposed model ID carries, empty while
// disable_prefix is on.
func (s settings) prefix() string {
	if s.DisablePrefix {
		return ""
	}
	return s.ModelPrefix
}

// zenPrefix is the prefix of OpenCode Zen model IDs. It is never empty: the
// Zen and Go catalogs share upstream IDs, so a bare Zen ID would be
// indistinguishable from the Go one.
func (s settings) zenPrefix() string {
	if s.ZenModelPrefix == "" {
		return defaultZenModelPrefix
	}
	return s.ZenModelPrefix
}

// zenBaseURL is the OpenCode Zen gateway root. It is the base URL without the
// Go path, so a base_url pointing at a mirror moves both.
func (s settings) zenBaseURL() string {
	if root, ok := strings.CutSuffix(strings.TrimRight(s.BaseURL, "/"), "/go"); ok && root != "" {
		return root
	}
	return defaultZenBaseURL
}

// gatewayBase is the canonical root the catalog's base URLs are rebased from,
// and the configured base they are rebased onto.
func (s settings) gatewayBase(family modelFamily) (root, base string) {
	if family == familyZen {
		return defaultZenBaseURL, s.zenBaseURL()
	}
	return defaultBaseURL, s.BaseURL
}

func (s settings) modelDisabled(upstreamID string) bool {
	return containsFold(s.DisabledModels, upstreamID)
}

// zenEnabled reports whether the page has turned on the serving of one
// OpenCode Zen model.
func (s settings) zenEnabled(upstreamID string) bool {
	return containsFold(s.ZenModels, upstreamID)
}

func containsFold(ids []string, id string) bool {
	for _, other := range ids {
		if strings.EqualFold(other, id) {
			return true
		}
	}
	return false
}

// withDisabled returns a copy of s that also disables ids.
func (s settings) withDisabled(ids []string) settings {
	if len(ids) > 0 {
		s.DisabledModels = append(slices.Clip(s.DisabledModels), ids...)
	}
	return s
}

// userAgent is the User-Agent for requests the plugin originates; the
// executor prefers the client's own.
func (s settings) userAgent() string {
	if s.UserAgent != "" {
		return s.UserAgent
	}
	return "whexy-cpa-opencode-go/" + pluginVersion
}

// consoleURL is the OpenCode Console base. By default it is on the
// gateway's host, so a base_url pointing at a mirror moves both.
func (s settings) consoleURL() string {
	if s.ConsoleURL != "" {
		return s.ConsoleURL
	}
	parsed, errParse := url.Parse(s.BaseURL)
	if errParse != nil || parsed.Host == "" {
		return defaultConsoleURL
	}
	return parsed.Scheme + "://" + parsed.Host + "/console"
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
	startCatalogWatch()
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
	case pluginabi.MethodAuthLoginStart:
		return handleLoginStart(request)
	case pluginabi.MethodAuthLoginPoll:
		return handleLoginPoll(request)
	case pluginabi.MethodAuthRefresh:
		return handleAuthRefresh(request)

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
			Logo:             logoPath,
			// CPA HTML-escapes these descriptions and Management Center shows
			// the escapes literally, so they avoid < > & and quotes.
			ConfigFields: []pluginapi.ConfigField{
				{Name: "base_url", Type: pluginapi.ConfigFieldTypeString, Description: "OpenCode Go gateway base URL. Default " + defaultBaseURL + "."},
				{Name: "console_url", Type: pluginapi.ConfigFieldTypeString, Description: "OpenCode Console base URL, used for login, token refresh and the model catalog. Default: the base_url host followed by /console, which is " + defaultConsoleURL + " for the default base_url."},
				{Name: "model_prefix", Type: pluginapi.ConfigFieldTypeString, Description: "Prefix of every OpenCode Go model ID, as in opencode-go/kimi-k3. Default " + defaultModelPrefix + ". To share a bare ID such as deepseek-v4-pro with another provider, turn on Bare ID for that model on the OpenCode Go page. disable_prefix overrides this setting."},
				{Name: "zen_model_prefix", Type: pluginapi.ConfigFieldTypeString, Description: "Prefix of every OpenCode Zen model ID, as in " + defaultZenModelPrefix + "/gpt-5.5. Default " + defaultZenModelPrefix + ". Zen models share upstream IDs with Go models, so their prefix is never dropped."},
				{Name: "disable_prefix", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Serve every OpenCode Go model under its bare ID only, as in kimi-k3, and ignore model_prefix. A bare ID shares routing with every other provider that serves the same ID, so disable the models you do not want on the OpenCode Go page. Zen model IDs keep their prefix."},
				{Name: "user_agent", Type: pluginapi.ConfigFieldTypeString, Description: "User-Agent sent upstream when the client sends none. Default whexy-cpa-opencode-go/" + pluginVersion + "."},
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
	base, errBase := absoluteURLSetting("base_url", raw.BaseURL)
	if errBase != nil {
		return settings{}, errBase
	}
	if base != "" {
		cfg.BaseURL = base
	}
	console, errConsole := absoluteURLSetting("console_url", raw.ConsoleURL)
	if errConsole != nil {
		return settings{}, errConsole
	}
	cfg.ConsoleURL = console
	if prefix, errPrefix := cleanPrefix("model_prefix", raw.ModelPrefix); errPrefix != nil {
		return settings{}, errPrefix
	} else if prefix != "" {
		cfg.ModelPrefix = prefix
	}
	if prefix, errPrefix := cleanPrefix("zen_model_prefix", raw.ZenModelPrefix); errPrefix != nil {
		return settings{}, errPrefix
	} else if prefix != "" {
		cfg.ZenModelPrefix = prefix
	}
	cfg.DisablePrefix = raw.DisablePrefix
	for _, id := range raw.DisabledModels {
		if id = strings.TrimSpace(id); id != "" && !cfg.modelDisabled(id) {
			cfg.DisabledModels = append(cfg.DisabledModels, id)
		}
	}
	for _, id := range raw.ZenModels {
		if id = strings.TrimSpace(id); id != "" && !cfg.zenEnabled(id) {
			cfg.ZenModels = append(cfg.ZenModels, id)
		}
	}
	cfg.UserAgent = strings.TrimSpace(raw.UserAgent)
	return cfg, nil
}

// cleanPrefix reads an optional model-ID prefix without its surrounding
// slashes, rejecting the characters an ID can never carry.
func cleanPrefix(name, value string) (string, error) {
	prefix := strings.Trim(strings.TrimSpace(value), "/")
	if prefix == "" {
		return "", nil
	}
	if strings.ContainsAny(prefix, " \t\r\n()*") {
		return "", fmt.Errorf("%s must not contain spaces, parentheses or *, got %q", name, prefix)
	}
	return prefix, nil
}

// absoluteURLSetting reads an optional http(s) URL setting without its
// trailing slash.
func absoluteURLSetting(name, value string) (string, error) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" {
		return "", nil
	}
	parsed, errParse := url.Parse(value)
	if errParse != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return "", fmt.Errorf("%s must be an absolute http(s) URL, got %q", name, value)
	}
	return value, nil
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

var (
	secretPattern = regexp.MustCompile(`oc_sk_[A-Za-z0-9_\-]+`)
	// loginTokenPattern matches OpenCode Console access (st_) and refresh
	// (rt_) tokens.
	loginTokenPattern = regexp.MustCompile(`\b(st|rt)_[A-Za-z0-9_\-]{16,}`)
)

// redactSecrets removes OpenCode keys and login tokens from text the plugin
// returns or logs; upstream error bodies may echo them back.
func redactSecrets(text string) string {
	text = secretPattern.ReplaceAllString(text, "oc_sk_[redacted]")
	return loginTokenPattern.ReplaceAllString(text, "${1}_[redacted]")
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

// errorEnvelopeWithRetryAfter is errorEnvelope plus the cooldown CPA should
// apply to the failed (auth, model) instead of its generic status backoff.
func errorEnvelopeWithRetryAfter(code, message string, httpStatus int, retryAfter time.Duration) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: redactSecrets(message), HTTPStatus: httpStatus, RetryAfterSeconds: retryAfter.Seconds()}})
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
