package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// callHost sends one raw callback to the host and returns its envelope.
// Tests replace it with a fake host.
var callHost func(method string, payload []byte) ([]byte, error)

// invokeHost marshals req, calls method and decodes the result into out
// (which may be nil).
func invokeHost(method string, req any, out any) error {
	payload, errMarshal := json.Marshal(req)
	if errMarshal != nil {
		return fmt.Errorf("marshal %s request: %w", method, errMarshal)
	}
	if callHost == nil {
		return fmt.Errorf("%s: host API is unavailable", method)
	}
	raw, errCall := callHost(method, payload)
	if errCall != nil {
		return fmt.Errorf("%s: %w", method, errCall)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		return fmt.Errorf("%s: undecodable host response", method)
	}
	if !env.OK {
		if env.Error == nil {
			return fmt.Errorf("%s: host callback failed", method)
		}
		return fmt.Errorf("%s: %s", method, env.Error.Message)
	}
	if out == nil || len(env.Result) == 0 {
		return nil
	}
	if errUnmarshal := json.Unmarshal(env.Result, out); errUnmarshal != nil {
		return fmt.Errorf("%s: decode result: %w", method, errUnmarshal)
	}
	return nil
}

type hostLogRequest struct {
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

func hostLog(level, message string) {
	_ = invokeHost(pluginabi.MethodHostLog, hostLogRequest{
		Level:   level,
		Message: message,
		Fields:  map[string]any{"plugin": pluginID},
	}, nil)
}

type hostAuthIndexRequest struct {
	AuthIndex string `json:"auth_index"`
}

type hostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

// hostAuthRuntime reads one credential's runtime entry, which carries its
// provider, label and account fields but no secrets.
func hostAuthRuntime(authIndex string) (pluginapi.HostAuthFileEntry, error) {
	var resp pluginapi.HostAuthGetRuntimeResponse
	if errCall := invokeHost(pluginabi.MethodHostAuthGetRuntime, hostAuthIndexRequest{AuthIndex: authIndex}, &resp); errCall != nil {
		return pluginapi.HostAuthFileEntry{}, errCall
	}
	return resp.Auth, nil
}

// hostAuthList lists every credential CLIProxyAPI knows, of any provider.
func hostAuthList() ([]pluginapi.HostAuthFileEntry, error) {
	var resp hostAuthListResponse
	if errCall := invokeHost(pluginabi.MethodHostAuthList, struct{}{}, &resp); errCall != nil {
		return nil, errCall
	}
	return resp.Files, nil
}

type hostHTTPRequest struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Headers http.Header `json:"headers,omitempty"`
}

// hostHTTPGet fetches url through CLIProxyAPI, so its global proxy applies.
func hostHTTPGet(url string) (pluginapi.HTTPResponse, error) {
	var resp pluginapi.HTTPResponse
	errCall := invokeHost(pluginabi.MethodHostHTTPDo, hostHTTPRequest{
		Method:  http.MethodGet,
		URL:     url,
		Headers: http.Header{"Accept": {"application/json"}, "User-Agent": {"whexy-cpa-usage-dashboard/" + pluginVersion}},
	}, &resp)
	return resp, errCall
}
