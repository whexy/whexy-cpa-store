// Package host calls back into CLIProxyAPI from a plugin: HTTP through the
// host's transport, logging, stream output and credential storage.
//
// The host opens a callback ID for executor.execute, execute_stream,
// count_tokens, http_request, model.for_auth, auth.login.*, auth.refresh,
// quota.fetch, quota.reset and management.handle, and closes it when that call
// returns; for execute_stream it stays open until the plugin closes the
// stream. Only host.http.* and host.log read the ID. HTTP made with it runs
// under the client request's context, so CPA's request log records it and a
// client disconnect cancels it. A closed ID is rejected, so work that outlives
// its call must use Detached. host.http.* never applies an auth's own
// proxy_url, only the global proxy.
package host

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

// Transport sends one raw callback to the host and returns its envelope.
type Transport func(method string, payload []byte) ([]byte, error)

var (
	transport atomic.Pointer[Transport]
	pluginID  atomic.Pointer[string]
)

// SetTransport installs t and returns a function restoring the previous one.
// The plugin package installs the native host; tests install a fake.
func SetTransport(t Transport) (restore func()) {
	var next *Transport
	if t != nil {
		next = &t
	}
	previous := transport.Swap(next)
	return func() { transport.Store(previous) }
}

// SetPluginID sets the plugin ID that Log tags every line with.
func SetPluginID(id string) {
	pluginID.Store(&id)
}

// CallError is a failed host callback. Status is the HTTP status the host
// attached, if any.
type CallError struct {
	Method  string
	Code    string
	Message string
	Status  int
}

func (e *CallError) Error() string {
	return e.Method + ": " + rpc.Redact(e.Message)
}

// Invoke marshals req, calls method and decodes the result into out, which
// may be nil.
func Invoke(method string, req, out any) error {
	payload, errMarshal := json.Marshal(req)
	if errMarshal != nil {
		return fmt.Errorf("marshal %s request: %w", method, errMarshal)
	}
	call := transport.Load()
	if call == nil {
		return &CallError{Method: method, Message: "host API is unavailable"}
	}
	raw, errCall := (*call)(method, payload)
	if errCall != nil {
		return &CallError{Method: method, Message: errCall.Error()}
	}
	var env rpc.Envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		return &CallError{Method: method, Message: "undecodable host response"}
	}
	if !env.OK {
		if env.Error == nil {
			return &CallError{Method: method, Message: "host callback failed"}
		}
		return &CallError{Method: method, Code: env.Error.Code, Message: env.Error.Message, Status: env.Error.HTTPStatus}
	}
	if out == nil || len(env.Result) == 0 {
		return nil
	}
	if errUnmarshal := json.Unmarshal(env.Result, out); errUnmarshal != nil {
		return &CallError{Method: method, Message: "decode result: " + errUnmarshal.Error()}
	}
	return nil
}

// Client issues the callbacks that read a callback ID, on behalf of one
// plugin call. The zero Client is detached.
type Client struct {
	callbackID string
}

// New returns a client bound to the host callback ID of the current call.
func New(callbackID string) Client {
	return Client{callbackID: strings.TrimSpace(callbackID)}
}

// Detached returns a client for work that outlives any plugin call.
func Detached() Client { return Client{} }

type logRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level"`
	Message        string         `json:"message"`
	Fields         map[string]any `json:"fields,omitempty"`
}

// Log writes to CPA's log, tagged with the plugin ID and, when the client
// has a callback ID, with the client request ID. Messages and string fields
// pass through the installed redactor.
func (c Client) Log(level, message string, fields map[string]any) {
	clean := make(map[string]any, len(fields)+1)
	for key, value := range fields {
		if text, ok := value.(string); ok {
			value = rpc.Redact(text)
		}
		clean[key] = value
	}
	if id := pluginID.Load(); id != nil && *id != "" {
		clean["plugin"] = *id
	}
	_ = Invoke(pluginabi.MethodHostLog, logRequest{
		HostCallbackID: c.callbackID,
		Level:          level,
		Message:        rpc.Redact(message),
		Fields:         clean,
	}, nil)
}

// Log writes one detached line to CPA's log.
func Log(level, message string, fields map[string]any) {
	Detached().Log(level, message, fields)
}

type streamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

// EmitStream sends one chunk of an executor.execute_stream response.
func EmitStream(streamID string, payload []byte) error {
	return Invoke(pluginabi.MethodHostStreamEmit, streamEmitRequest{StreamID: streamID, Payload: payload}, nil)
}

// CloseStream ends an executor.execute_stream response. A non-empty
// errorMessage reaches the client as a stream error without HTTP status.
func CloseStream(streamID, errorMessage string) {
	_ = Invoke(pluginabi.MethodHostStreamClose, streamEmitRequest{StreamID: streamID, Error: rpc.Redact(errorMessage)}, nil)
}

type authListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

// AuthList lists every runtime credential the host has registered, with its
// provider key and routing state.
func AuthList() ([]pluginapi.HostAuthFileEntry, error) {
	var resp authListResponse
	if errCall := Invoke(pluginabi.MethodHostAuthList, struct{}{}, &resp); errCall != nil {
		return nil, errCall
	}
	return resp.Files, nil
}

// AuthGet returns the physical file behind an auth.
func AuthGet(authIndex string) (pluginapi.HostAuthGetResponse, error) {
	var resp pluginapi.HostAuthGetResponse
	errCall := Invoke(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex}, &resp)
	return resp, errCall
}

// AuthGetRuntime returns the runtime entry of an auth: its provider, label
// and routing state.
func AuthGetRuntime(authIndex string) (pluginapi.HostAuthGetRuntimeResponse, error) {
	var resp pluginapi.HostAuthGetRuntimeResponse
	errCall := Invoke(pluginabi.MethodHostAuthGetRuntime, pluginapi.HostAuthGetRequest{AuthIndex: authIndex}, &resp)
	return resp, errCall
}

// AuthSave writes name in the auth directory and re-registers its auths
// through auth.parse.
func AuthSave(name string, raw []byte) (pluginapi.HostAuthSaveResponse, error) {
	var resp pluginapi.HostAuthSaveResponse
	errCall := Invoke(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{Name: name, JSON: raw}, &resp)
	return resp, errCall
}

// ResetCooldown clears CPA's quota and cooldown state for one auth.
func ResetCooldown(authIndex string) (pluginapi.HostRoutingResetCooldownResponse, error) {
	var resp pluginapi.HostRoutingResetCooldownResponse
	errCall := Invoke(pluginabi.MethodHostRoutingResetCooldown, pluginapi.HostRoutingResetCooldownRequest{AuthIndex: authIndex}, &resp)
	return resp, errCall
}
