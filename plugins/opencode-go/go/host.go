package main

// Host callback IDs, as implemented by CLIProxyAPI v8.0.27 (internal/pluginhost):
//
// The host opens an ID for executor.execute, execute_stream, count_tokens,
// http_request, model.for_auth, auth.login.*, auth.refresh, quota.fetch,
// quota.reset and management.handle (management and resource routes alike),
// and closes it when that call returns; for execute_stream it stays open until
// the plugin closes the stream. auth.parse, model.static, quota.describe and
// management.register carry none.
//
// Only host.http.* and host.log read the ID. HTTP made with it runs under the
// client request's context, so CPA's request log records it and a client
// disconnect cancels it. A closed ID is rejected, so work that outlives its
// call must use newHost(""), which runs detached. host.auth.*,
// host.routing.reset_cooldown, host.stream.* and host.http.stream_* take no ID.
// host.http.* never applies an auth's own proxy_url, only the global proxy.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// callHost sends one raw callback to the host and returns its envelope.
// Tests replace it with a fake host.
var callHost func(method string, payload []byte) ([]byte, error)

// hostCallError is a failed host callback. Status is the HTTP status the host
// attached, if any.
type hostCallError struct {
	Method  string
	Code    string
	Message string
	Status  int
}

func (e *hostCallError) Error() string {
	return e.Method + ": " + redactSecrets(e.Message)
}

// invokeHost marshals req, calls method and decodes the result into out
// (which may be nil).
func invokeHost(method string, req any, out any) error {
	payload, errMarshal := json.Marshal(req)
	if errMarshal != nil {
		return fmt.Errorf("marshal %s request: %w", method, errMarshal)
	}
	if callHost == nil {
		return &hostCallError{Method: method, Message: "host API is unavailable"}
	}
	raw, errCall := callHost(method, payload)
	if errCall != nil {
		return &hostCallError{Method: method, Message: errCall.Error()}
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		return &hostCallError{Method: method, Message: "undecodable host response"}
	}
	if !env.OK {
		if env.Error == nil {
			return &hostCallError{Method: method, Message: "host callback failed"}
		}
		return &hostCallError{Method: method, Code: env.Error.Code, Message: env.Error.Message, Status: env.Error.HTTPStatus}
	}
	if out == nil || len(env.Result) == 0 {
		return nil
	}
	if errUnmarshal := json.Unmarshal(env.Result, out); errUnmarshal != nil {
		return &hostCallError{Method: method, Message: "decode result: " + errUnmarshal.Error()}
	}
	return nil
}

// httpDoer is the HTTP surface the catalog, usage and executor code use, so
// tests can substitute a fake or net/http.
type httpDoer interface {
	Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

// host issues HTTP and log callbacks on behalf of one plugin call.
type host struct {
	callbackID string
}

func newHost(callbackID string) host {
	return host{callbackID: strings.TrimSpace(callbackID)}
}

type hostHTTPRequest struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	OperationID    string      `json:"operation_id,omitempty"`
	Method         string      `json:"method,omitempty"`
	URL            string      `json:"url,omitempty"`
	Headers        http.Header `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
}

type hostOperationRequest struct {
	HostCallbackID string `json:"host_callback_id,omitempty"`
	OperationID    string `json:"operation_id,omitempty"`
}

type hostOperationResponse struct {
	OperationID string `json:"operation_id"`
}

type hostHTTPStreamStart struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

type hostStreamIDRequest struct {
	StreamID string `json:"stream_id"`
}

type hostStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type hostStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
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

// Do performs a buffered upstream request through host.http.do.
func (h host) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	var resp pluginapi.HTTPResponse
	errCall := h.withOperation(ctx, func(operationID string) error {
		return invokeHost(pluginabi.MethodHostHTTPDo, h.httpRequest(req, operationID), &resp)
	})
	if errCall != nil {
		return pluginapi.HTTPResponse{}, errCall
	}
	return resp, nil
}

// DoStream starts a streaming upstream request through host.http.do_stream.
// The caller must Close the returned stream; cancelling ctx closes it too.
func (h host) DoStream(ctx context.Context, req pluginapi.HTTPRequest) (*hostHTTPStream, error) {
	var start hostHTTPStreamStart
	errCall := h.withOperation(ctx, func(operationID string) error {
		return invokeHost(pluginabi.MethodHostHTTPDoStream, h.httpRequest(req, operationID), &start)
	})
	if errCall != nil {
		return nil, errCall
	}
	if start.StreamID == "" {
		return nil, &hostCallError{Method: pluginabi.MethodHostHTTPDoStream, Message: "host returned no stream_id"}
	}
	stream := &hostHTTPStream{StatusCode: start.StatusCode, Headers: start.Headers, ID: start.StreamID, ctx: ctx}
	stop := context.AfterFunc(ctx, stream.Close)
	stream.mu.Lock()
	stream.stopWatch = stop
	stream.mu.Unlock()
	return stream, nil
}

func (h host) httpRequest(req pluginapi.HTTPRequest, operationID string) hostHTTPRequest {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	return hostHTTPRequest{
		HostCallbackID: h.callbackID,
		OperationID:    operationID,
		Method:         method,
		URL:            req.URL,
		Headers:        req.Headers,
		Body:           req.Body,
	}
}

// withOperation runs call under a host HTTP operation that is cancelled when
// ctx is, because a blocked host.http.do cannot otherwise be interrupted from
// the plugin side.
func (h host) withOperation(ctx context.Context, call func(operationID string) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return errCtx
	}
	if ctx.Done() == nil {
		return call("")
	}
	var opened hostOperationResponse
	if errOpen := invokeHost(pluginabi.MethodHostHTTPOperationOpen, hostOperationRequest{HostCallbackID: h.callbackID}, &opened); errOpen != nil {
		return errOpen
	}
	stop := context.AfterFunc(ctx, func() {
		_ = invokeHost(pluginabi.MethodHostHTTPCancel, hostOperationRequest{HostCallbackID: h.callbackID, OperationID: opened.OperationID}, nil)
	})
	errCall := call(opened.OperationID)
	stop()
	if errCtx := ctx.Err(); errCtx != nil {
		return errCtx
	}
	return errCall
}

// hostHTTPStream is an open host.http.do_stream response body.
type hostHTTPStream struct {
	StatusCode int
	Headers    http.Header
	ID         string

	ctx       context.Context
	mu        sync.Mutex
	stopWatch func() bool
	closeOnce sync.Once
}

// Read returns the next body chunk. done reports the end of the body; a
// transport error mid-body is returned as err with done set.
func (s *hostHTTPStream) Read() (payload []byte, done bool, err error) {
	if errCtx := s.ctx.Err(); errCtx != nil {
		return nil, true, errCtx
	}
	var resp hostStreamReadResponse
	if errRead := invokeHost(pluginabi.MethodHostHTTPStreamRead, hostStreamIDRequest{StreamID: s.ID}, &resp); errRead != nil {
		if errCtx := s.ctx.Err(); errCtx != nil {
			return nil, true, errCtx
		}
		return nil, true, errRead
	}
	if resp.Error != "" {
		return resp.Payload, true, fmt.Errorf("upstream stream: %s", redactSecrets(resp.Error))
	}
	return resp.Payload, resp.Done, nil
}

// ReadAll drains the body, for error responses.
func (s *hostHTTPStream) ReadAll() ([]byte, error) {
	var body []byte
	for {
		chunk, done, errRead := s.Read()
		body = append(body, chunk...)
		if errRead != nil {
			return body, errRead
		}
		if done {
			return body, nil
		}
	}
}

// Close releases the upstream connection; it unblocks a pending Read.
func (s *hostHTTPStream) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		stop := s.stopWatch
		s.mu.Unlock()
		if stop != nil {
			stop()
		}
		_ = invokeHost(pluginabi.MethodHostHTTPStreamClose, hostStreamIDRequest{StreamID: s.ID}, nil)
	})
}

// Log writes to CPA's log, tagged with the client request ID when the call
// has a callback ID.
func (h host) Log(level, message string, fields map[string]any) {
	clean := make(map[string]any, len(fields))
	for key, value := range fields {
		if text, ok := value.(string); ok {
			value = redactSecrets(text)
		}
		clean[key] = value
	}
	clean["plugin"] = providerKey
	_ = invokeHost(pluginabi.MethodHostLog, hostLogRequest{
		HostCallbackID: h.callbackID,
		Level:          level,
		Message:        redactSecrets(message),
		Fields:         clean,
	}, nil)
}

// hostStreamEmit sends one chunk of an executor.execute_stream response.
func hostStreamEmit(streamID string, payload []byte) error {
	return invokeHost(pluginabi.MethodHostStreamEmit, hostStreamEmitRequest{StreamID: streamID, Payload: payload}, nil)
}

// hostStreamClose ends an executor.execute_stream response; a non-empty
// errorMessage reaches the client as a stream error without HTTP status.
func hostStreamClose(streamID, errorMessage string) {
	_ = invokeHost(pluginabi.MethodHostStreamClose, hostStreamEmitRequest{StreamID: streamID, Error: redactSecrets(errorMessage)}, nil)
}

func hostAuthList() ([]pluginapi.HostAuthFileEntry, error) {
	var resp hostAuthListResponse
	if errCall := invokeHost(pluginabi.MethodHostAuthList, struct{}{}, &resp); errCall != nil {
		return nil, errCall
	}
	return resp.Files, nil
}

// hostAuthGet returns the physical file behind an auth, which for keys
// expanded from opencode-go.json is the whole multi-key file.
func hostAuthGet(authIndex string) (pluginapi.HostAuthGetResponse, error) {
	var resp pluginapi.HostAuthGetResponse
	errCall := invokeHost(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex}, &resp)
	return resp, errCall
}

// hostAuthSave writes name in the auth directory and re-registers its auths
// through auth.parse.
func hostAuthSave(name string, raw []byte) (pluginapi.HostAuthSaveResponse, error) {
	var resp pluginapi.HostAuthSaveResponse
	errCall := invokeHost(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{Name: name, JSON: raw}, &resp)
	return resp, errCall
}

// hostResetCooldown clears CPA's quota and cooldown state for one auth.
func hostResetCooldown(authIndex string) (pluginapi.HostRoutingResetCooldownResponse, error) {
	var resp pluginapi.HostRoutingResetCooldownResponse
	errCall := invokeHost(pluginabi.MethodHostRoutingResetCooldown, pluginapi.HostRoutingResetCooldownRequest{AuthIndex: authIndex}, &resp)
	return resp, errCall
}
