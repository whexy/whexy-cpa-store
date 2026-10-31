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
	pluginVersion = "0.1.0"

	providerKey = "zed"
	displayName = "Zed"
	// defaultBaseURL is Zed's cloud edge, where account calls and the LLM
	// gateway share a host.
	defaultBaseURL     = "https://cloud.zed.dev"
	defaultSignInURL   = "https://zed.dev/native_app_signin"
	defaultModelPrefix = "zed"

	// shutdownGrace bounds how long background work may observe cancellation
	// before the host unloads the library underneath it.
	shutdownGrace = 5 * time.Second
)

var executorFormats = []string{"claude", "openai", "openai-response", "gemini"}

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

// settings are the plugin-owned values under plugins.configs.zed.
type settings struct {
	BaseURL     string
	SignInURL   string
	ModelPrefix string
	// DisablePrefix serves every model under its bare ID only.
	DisablePrefix bool
	// DisabledModels are upstream model IDs the plugin keeps out of CPA.
	DisabledModels []string
	// DashboardCookie is a dashboard.zed.dev Cookie header value. Zed has no
	// API for plan usage; this cookie is the only way to read it.
	DashboardCookie string
	// DashboardAccount is the Zed username the cookie belongs to. Usage is
	// never shown for another auth, since the dashboard endpoint has no
	// parameter naming the account.
	DashboardAccount string
	UserAgent        string
}

type rawSettings struct {
	BaseURL          string   `yaml:"base_url"`
	SignInURL        string   `yaml:"sign_in_url"`
	ModelPrefix      string   `yaml:"model_prefix"`
	DisablePrefix    bool     `yaml:"disable_prefix"`
	DisabledModels   []string `yaml:"disabled_models"`
	DashboardCookie  string   `yaml:"dashboard_cookie"`
	DashboardAccount string   `yaml:"dashboard_account"`
	UserAgent        string   `yaml:"user_agent"`
}

var currentConfig atomic.Pointer[settings]

func currentSettings() settings {
	if cfg := currentConfig.Load(); cfg != nil {
		return *cfg
	}
	return defaultSettings()
}

func defaultSettings() settings {
	return settings{
		BaseURL:     defaultBaseURL,
		SignInURL:   defaultSignInURL,
		ModelPrefix: defaultModelPrefix,
	}
}

// modelPrefix is the prefix exposed model IDs carry, empty when the page
// turned disable_prefix on.
func (s settings) modelPrefix() string {
	if s.DisablePrefix {
		return ""
	}
	return s.ModelPrefix
}

// exposedID is the model ID CLIProxyAPI lists and clients request.
func (s settings) exposedID(upstreamID string) string {
	if prefix := s.modelPrefix(); prefix != "" {
		return prefix + "/" + upstreamID
	}
	return upstreamID
}

// upstreamID turns a client model ID back into the ID Zed names, accepting
// the bare ID as well as the prefixed one.
func (s settings) upstreamID(model string) string {
	model = strings.TrimSpace(model)
	// Accept the configured prefix even while bare IDs are served, so a
	// client that kept the prefixed ID after the switch still resolves.
	prefix := s.ModelPrefix
	if prefix == "" {
		prefix = defaultModelPrefix
	}
	if trimmed, ok := strings.CutPrefix(model, prefix+"/"); ok {
		return trimmed
	}
	return model
}

// dashboardApplies reports whether the dashboard cookie belongs to this
// auth. The dashboard endpoint has no account parameter, so a cookie must be
// pinned to one username.
func (s settings) dashboardApplies(cred credential) bool {
	return s.DashboardCookie != "" && s.DashboardAccount != "" && strings.EqualFold(s.DashboardAccount, cred.Username)
}

func (s settings) modelDisabled(upstreamID string) bool {
	return containsFold(s.DisabledModels, upstreamID)
}

func containsFold(ids []string, id string) bool {
	for _, other := range ids {
		if strings.EqualFold(other, id) {
			return true
		}
	}
	return false
}

// userAgent is the User-Agent sent to Zed; the edge gates on the Zed name.
func (s settings) userAgent() string {
	if s.UserAgent != "" {
		return s.UserAgent
	}
	return "Zed/" + zedVersion() + " (linux; x86_64)"
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
	logins.closeAll()
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
		return errorEnvelope("unsupported", "Zed usage cannot be reset", 400), nil

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
				{Name: "base_url", Type: pluginapi.ConfigFieldTypeString, Description: "Zed cloud base URL. Default " + defaultBaseURL + "."},
				{Name: "sign_in_url", Type: pluginapi.ConfigFieldTypeString, Description: "The Zed sign-in page the OAuth Login tile opens. Default " + defaultSignInURL + "."},
				{Name: "model_prefix", Type: pluginapi.ConfigFieldTypeString, Description: "Prefix of every Zed model ID, as in " + defaultModelPrefix + "/claude-sonnet-5-5. Default " + defaultModelPrefix + ". disable_prefix overrides this setting."},
				{Name: "disable_prefix", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Serve every Zed model under its bare ID only, as in claude-sonnet-5-5, and ignore model_prefix. A bare ID then shares routing by priority and weight with every other provider that serves the same ID."},
				{Name: "dashboard_cookie", Type: pluginapi.ConfigFieldTypeString, Description: "The Cookie header of a signed-in dashboard.zed.dev session. Zed exposes plan usage only to the dashboard, so the Plugin tab shows spend and limit only when this and dashboard_account are set. It is sent only to the dashboard usage endpoint, not to /completions."},
				{Name: "dashboard_account", Type: pluginapi.ConfigFieldTypeString, Description: "The Zed username the dashboard cookie belongs to. Usage meters are shown only for that account; the dashboard endpoint does not name an account, so another auth would otherwise see this account's spend."},
				{Name: "user_agent", Type: pluginapi.ConfigFieldTypeString, Description: "User-Agent sent to Zed. Default Zed/" + clientVersion + " (linux; x86_64); the edge identifies the client from it."},
			},
		},
		Capabilities: registrationCapability{
			AuthProvider:  true,
			ModelProvider: true,
			Executor:      true,
			// Models depend on the account's plan, and a static ID another
			// provider already serves is dropped by the host.
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
	previous := currentSettings()
	if previous.BaseURL != cfg.BaseURL {
		// A JWT and a catalog are only valid for the cloud that issued them.
		llmTokens.clear()
		modelsByAuth.clear()
	}
	currentConfig.Store(&cfg)
	versionOnce.Do(func() {
		startBackground(versionLoop)
	})
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
	if base, errBase := absoluteURLSetting("base_url", raw.BaseURL); errBase != nil {
		return settings{}, errBase
	} else if base != "" {
		cfg.BaseURL = base
	}
	if signIn, errSignIn := absoluteURLSetting("sign_in_url", raw.SignInURL); errSignIn != nil {
		return settings{}, errSignIn
	} else if signIn != "" {
		cfg.SignInURL = signIn
	}
	if prefix, errPrefix := cleanPrefix("model_prefix", raw.ModelPrefix); errPrefix != nil {
		return settings{}, errPrefix
	} else if prefix != "" {
		cfg.ModelPrefix = prefix
	}
	cfg.DisablePrefix = raw.DisablePrefix
	for _, id := range raw.DisabledModels {
		if id = strings.TrimSpace(id); id != "" && !cfg.modelDisabled(id) {
			cfg.DisabledModels = append(cfg.DisabledModels, id)
		}
	}
	cfg.DashboardCookie = strings.TrimSpace(raw.DashboardCookie)
	cfg.DashboardAccount = strings.TrimSpace(raw.DashboardAccount)
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

// Work that outlives a plugin call (stream pumps) runs under backgroundCtx
// so shutdown can stop it before the host unloads the library.
var (
	backgroundCtx, cancelBackground = context.WithCancel(context.Background())
	backgroundWork                  sync.WaitGroup
	versionOnce                     sync.Once
)

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
	// jwtPattern matches a Zed LLM JWT, which is a long three-part base64
	// string beginning with an HS256 header.
	jwtPattern = regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`)
	// accessTokenPattern matches a native-app access token: a long base64url
	// blob without dots.
	accessTokenPattern = regexp.MustCompile(`"(access_token|dashboard_cookie)"\s*:\s*"[^"]{16,}"`)
)

// redactSecrets removes Zed tokens and the dashboard cookie from text the
// plugin returns or logs; upstream error bodies may echo them back.
func redactSecrets(text string) string {
	text = jwtPattern.ReplaceAllString(text, "eyJ[redacted]")
	return accessTokenPattern.ReplaceAllString(text, `"$1":"[redacted]"`)
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
	seconds := 0.0
	if retryAfter > 0 {
		seconds = retryAfter.Seconds()
	}
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: redactSecrets(message), HTTPStatus: httpStatus, RetryAfterSeconds: seconds}})
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
