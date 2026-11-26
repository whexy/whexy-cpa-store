// Package rpc encodes the JSON envelopes that CLIProxyAPI and its native
// plugins exchange in both directions, and the registration a plugin answers
// plugin.register with.
package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Error is the failure half of an Envelope.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// HTTPStatus is forwarded to the client; the host drops Code.
	HTTPStatus int `json:"http_status,omitempty"`
	// RetryAfterSeconds is how long the host cools the failed (auth, model)
	// down. Hosts that predate it apply their generic status backoff.
	RetryAfterSeconds float64 `json:"retry_after_seconds,omitempty"`
}

// Envelope wraps every call result and host callback response.
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// OK encodes value as a successful envelope.
func OK(value any) ([]byte, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(Envelope{OK: true, Result: raw})
}

// FailOption sets an optional field of a failed envelope.
type FailOption func(*Error)

// WithStatus sets the HTTP status the host returns to the client.
func WithStatus(status int) FailOption {
	return func(e *Error) { e.HTTPStatus = status }
}

// WithRetryAfter sets the cooldown the host applies to the failed auth. A
// duration that is not positive sets none.
func WithRetryAfter(d time.Duration) FailOption {
	return func(e *Error) {
		if d > 0 {
			e.RetryAfterSeconds = d.Seconds()
		}
	}
}

// Fail encodes a failed envelope. The message passes through the redactor
// set with SetRedactor.
func Fail(code, message string, opts ...FailOption) []byte {
	failure := &Error{Code: code, Message: Redact(message)}
	for _, opt := range opts {
		opt(failure)
	}
	raw, _ := json.Marshal(Envelope{Error: failure})
	return raw
}

// TransportFailure encodes an upstream request to provider that got no
// response. A canceled request is a 408, as the client is gone or the plugin
// is shutting down; any other failure is a 502 the host may retry elsewhere.
func TransportFailure(provider string, err error) []byte {
	if errors.Is(err, context.Canceled) {
		return Fail("canceled", provider+" request canceled", WithStatus(http.StatusRequestTimeout))
	}
	return Fail("upstream_unreachable", provider+" request failed: "+err.Error(), WithStatus(http.StatusBadGateway))
}

var redactor atomic.Pointer[func(string) string]

// SetRedactor installs the function that strips secrets from every message
// the plugin returns or logs through this kit. Upstream error bodies can echo
// credentials back, so provider plugins install one.
func SetRedactor(redact func(string) string) {
	if redact == nil {
		redactor.Store(nil)
		return
	}
	redactor.Store(&redact)
}

// Redact applies the installed redactor, if any.
func Redact(text string) string {
	if redact := redactor.Load(); redact != nil {
		return (*redact)(text)
	}
	return text
}

// LifecycleRequest is the payload of plugin.register and plugin.reconfigure:
// the plugin's own node of config.yaml and the host's RPC schema version.
type LifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// IdentifierResponse answers auth.identifier, executor.identifier and
// quota.identifier.
type IdentifierResponse struct {
	Identifier string `json:"identifier"`
}

// Registration answers plugin.register and plugin.reconfigure.
type Registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  Capabilities       `json:"capabilities"`
}

// Capabilities lists the host integrations a plugin implements. The host
// decodes it into a struct, so an omitted field means false.
type Capabilities struct {
	ModelRegistrar         bool                         `json:"model_registrar,omitempty"`
	ModelProvider          bool                         `json:"model_provider,omitempty"`
	AuthProvider           bool                         `json:"auth_provider,omitempty"`
	Scheduler              bool                         `json:"scheduler,omitempty"`
	ModelRouter            bool                         `json:"model_router,omitempty"`
	Executor               bool                         `json:"executor,omitempty"`
	ExecutorModelScope     pluginapi.ExecutorModelScope `json:"executor_model_scope,omitempty"`
	ExecutorInputFormats   []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats  []string                     `json:"executor_output_formats,omitempty"`
	RequestInterceptor     bool                         `json:"request_interceptor,omitempty"`
	RequestLifecyclePlugin bool                         `json:"request_lifecycle_plugin,omitempty"`
	ResponseInterceptor    bool                         `json:"response_interceptor,omitempty"`
	UsagePlugin            bool                         `json:"usage_plugin,omitempty"`
	ManagementAPI          bool                         `json:"management_api,omitempty"`
	QuotaProvider          bool                         `json:"quota_provider,omitempty"`
}

// Store metadata shared by every plugin in this repository.
const (
	author     = "Whexy"
	repository = "https://github.com/whexy/whexy-cpa-store"
)

// NewMetadata returns the metadata a store plugin registers. logo is the
// resource path of the plugin's mark.
func NewMetadata(name, version, logo string, fields ...pluginapi.ConfigField) pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:             name,
		Version:          version,
		Author:           author,
		GitHubRepository: repository,
		Logo:             logo,
		ConfigFields:     fields,
	}
}
