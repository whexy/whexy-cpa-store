package main

// Host callbacks this plugin uses, as implemented by CLIProxyAPI v8.0.27:
//
// host.auth.list carries no host_callback_id and answers every runtime
// credential the host has registered, with the provider key and its routing
// state. It is the plugin's only view of CPA's credentials: a plugin runs in
// its own Go runtime, so the host's model registry and selector are not
// reachable from here.
//
// There is deliberately no host.affinity.lookup call here: the host installs
// itself as the plugin scheduler, which makes that callback answer
// "unsupported" for every request. The credential CPA selected is observed
// through the post-credential request interceptor instead (interceptor.go).
//
// host.log writes to CPA's log; without a callback ID it is not correlated
// with a client request, which is fine for plugin-level messages.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// envelope is the RPC result wrapper used in both directions.
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

// lifecycleRequest is the payload of plugin.register and plugin.reconfigure:
// the plugin's own node of config.yaml, plus the host's RPC schema version.
type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type hostLogRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level"`
	Message        string         `json:"message"`
	Fields         map[string]any `json:"fields,omitempty"`
}

type hostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

// callHost sends one raw callback to the host. Tests replace it with a fake.
var callHost func(method string, payload []byte) ([]byte, error)

// hostError is a failed host callback.
type hostError struct {
	Method  string
	Code    string
	Message string
	Status  int
}

func (e *hostError) Error() string { return e.Method + ": " + e.Message }

// invokeHost marshals req, calls method and decodes the result into out
// (which may be nil).
func invokeHost(method string, req any, out any) error {
	payload, errMarshal := json.Marshal(req)
	if errMarshal != nil {
		return fmt.Errorf("marshal %s request: %w", method, errMarshal)
	}
	if callHost == nil {
		return &hostError{Method: method, Message: "host API is unavailable"}
	}
	raw, errCall := callHost(method, payload)
	if errCall != nil {
		return &hostError{Method: method, Message: errCall.Error()}
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		return &hostError{Method: method, Message: "undecodable host response"}
	}
	if !env.OK {
		if env.Error == nil {
			return &hostError{Method: method, Message: "host callback failed"}
		}
		return &hostError{Method: method, Code: env.Error.Code, Message: env.Error.Message, Status: env.Error.HTTPStatus}
	}
	if out == nil || len(env.Result) == 0 {
		return nil
	}
	if errDecode := json.Unmarshal(env.Result, out); errDecode != nil {
		return &hostError{Method: method, Message: "decode result: " + errDecode.Error()}
	}
	return nil
}

// hostAuthList lists every runtime credential the host has registered.
func hostAuthList() ([]pluginapi.HostAuthFileEntry, error) {
	var resp hostAuthListResponse
	if errCall := invokeHost(pluginabi.MethodHostAuthList, struct{}{}, &resp); errCall != nil {
		return nil, errCall
	}
	return resp.Files, nil
}

// hostLog writes one line to CPA's log.
func hostLog(level, message string, fields map[string]any) {
	clean := fields
	if len(fields) > 0 {
		clean = make(map[string]any, len(fields)+1)
		for key, value := range fields {
			clean[key] = value
		}
	}
	if clean == nil {
		clean = map[string]any{}
	}
	clean["plugin"] = pluginID
	_ = invokeHost(pluginabi.MethodHostLog, hostLogRequest{Level: level, Message: message, Fields: clean}, nil)
}

func okEnvelope(value any) ([]byte, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

// errorEnvelope builds a failed RPC result. The host forwards httpStatus to
// the client and drops the code.
func errorEnvelope(code, message string, httpStatus ...int) []byte {
	status := 0
	if len(httpStatus) > 0 {
		status = httpStatus[0]
	}
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message, HTTPStatus: status}})
	return raw
}

// providerKeyOf normalizes the provider keys this plugin compares.
func providerKeyOf(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
