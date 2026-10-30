package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

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

type hostAuthGetResponse struct {
	JSON json.RawMessage `json:"json"`
}

// hostAuthFile returns one credential's auth file as stored, secrets
// included. Callers read the one field they need and drop the rest.
func hostAuthFile(authIndex string) (json.RawMessage, error) {
	var resp hostAuthGetResponse
	if errCall := invokeHost(pluginabi.MethodHostAuthGet, hostAuthIndexRequest{AuthIndex: authIndex}, &resp); errCall != nil {
		return nil, errCall
	}
	return resp.JSON, nil
}

type hostHTTPRequest struct {
	OperationID string      `json:"operation_id,omitempty"`
	Method      string      `json:"method"`
	URL         string      `json:"url"`
	Headers     http.Header `json:"headers,omitempty"`
	Body        []byte      `json:"body,omitempty"`
}

type hostOperation struct {
	OperationID string `json:"operation_id,omitempty"`
}

const hostHTTPTimeout = 20 * time.Second

// hostHTTPGet fetches url through CLIProxyAPI, so its global proxy applies.
func hostHTTPGet(url string) (pluginapi.HTTPResponse, error) {
	return hostDo(context.Background(), http.MethodGet, url, http.Header{
		"Accept":     {"application/json"},
		"User-Agent": {"whexy-cpa-usage-dashboard/" + pluginVersion},
	}, nil)
}

// hostDo sends one request through the host's HTTP client under a host
// operation, because a blocked host.http.do can only be interrupted by
// cancelling its operation. It applies only the global proxy, never a
// credential's own proxy_url.
func hostDo(ctx context.Context, method, url string, headers http.Header, body []byte) (pluginapi.HTTPResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, hostHTTPTimeout)
	defer cancel()
	var opened hostOperation
	if errOpen := invokeHost(pluginabi.MethodHostHTTPOperationOpen, hostOperation{}, &opened); errOpen != nil {
		return pluginapi.HTTPResponse{}, errOpen
	}
	stop := context.AfterFunc(ctx, func() {
		_ = invokeHost(pluginabi.MethodHostHTTPCancel, opened, nil)
	})
	defer stop()
	var resp pluginapi.HTTPResponse
	errCall := invokeHost(pluginabi.MethodHostHTTPDo, hostHTTPRequest{OperationID: opened.OperationID, Method: method, URL: url, Headers: headers, Body: body}, &resp)
	if errCtx := ctx.Err(); errCtx != nil {
		return pluginapi.HTTPResponse{}, errCtx
	}
	return resp, errCall
}
