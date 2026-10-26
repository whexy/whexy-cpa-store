package main

// Host callbacks this plugin uses, as implemented by CLIProxyAPI v8.0.27 to
// v8.0.44:
//
// host.auth.list answers every file-backed and runtime credential, with the
// provider, email and routing state, and host.auth.get one credential's auth
// file, tokens included. Credentials synthesized from config.yaml api-keys are
// in neither, so they stay accounts whose quota is unknown.
//
// host.http.do without a callback ID runs detached from any client request
// and applies only the global proxy-url, never a credential's own proxy_url.
//
// None of these calls is made on the scheduler.pick path: the scheduler runs
// inside every credential selection, and answers from memory alone.

import (
	"encoding/json"
	"fmt"

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
// the plugin's own node of config.yaml.
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type hostLogRequest struct {
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

type hostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type hostAuthGetRequest struct {
	AuthIndex string `json:"auth_index"`
}

type hostAuthGetResponse struct {
	JSON json.RawMessage `json:"json"`
}

// callHost sends one raw callback to the host. Tests replace it with a fake.
var callHost func(method string, payload []byte) ([]byte, error)

// hostError is a failed host callback.
type hostError struct {
	Method  string
	Message string
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
		return &hostError{Method: method, Message: env.Error.Message}
	}
	if out == nil || len(env.Result) == 0 {
		return nil
	}
	if errDecode := json.Unmarshal(env.Result, out); errDecode != nil {
		return &hostError{Method: method, Message: "decode result: " + errDecode.Error()}
	}
	return nil
}

func hostAuthList() ([]pluginapi.HostAuthFileEntry, error) {
	var resp hostAuthListResponse
	if errCall := invokeHost(pluginabi.MethodHostAuthList, struct{}{}, &resp); errCall != nil {
		return nil, errCall
	}
	return resp.Files, nil
}

// hostAuthFile returns one credential's auth file as stored, secrets
// included.
func hostAuthFile(authIndex string) (json.RawMessage, error) {
	var resp hostAuthGetResponse
	if errCall := invokeHost(pluginabi.MethodHostAuthGet, hostAuthGetRequest{AuthIndex: authIndex}, &resp); errCall != nil {
		return nil, errCall
	}
	return resp.JSON, nil
}

// hostLog writes one line to CPA's log.
func hostLog(level, message string, fields map[string]any) {
	clean := map[string]any{"plugin": pluginID}
	for key, value := range fields {
		clean[key] = value
	}
	_ = invokeHost(pluginabi.MethodHostLog, hostLogRequest{Level: level, Message: message, Fields: clean}, nil)
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
