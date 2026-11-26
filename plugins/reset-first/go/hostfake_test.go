package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/host"
	"github.com/whexy/whexy-cpa-store/pluginkit/host/hosttest"
)

// fakeHost serves the credentials and upstream answers a poll reads.
type fakeHost struct {
	mu    sync.Mutex
	auths []pluginapi.HostAuthFileEntry
	// files are the auth files by auth index.
	files map[string]string
	// answers are the upstream responses by URL.
	answers  map[string]pluginapi.HTTPResponse
	requests []upstreamRequest
}

// upstreamRequest is the host.http.do payload.
type upstreamRequest struct {
	OperationID string      `json:"operation_id"`
	Method      string      `json:"method"`
	URL         string      `json:"url"`
	Headers     http.Header `json:"headers"`
}

func installFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	h := &fakeHost{files: map[string]string{}, answers: map[string]pluginapi.HTTPResponse{}}
	fake := hosttest.Install(t)
	fake.Handle(pluginabi.MethodHostAuthList, func(json.RawMessage) (any, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		return map[string]any{"files": h.auths}, nil
	})
	fake.Handle(pluginabi.MethodHostAuthGet, func(payload json.RawMessage) (any, error) {
		var req pluginapi.HostAuthGetRequest
		if errUnmarshal := json.Unmarshal(payload, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		file, ok := h.files[req.AuthIndex]
		if !ok {
			return nil, &host.CallError{Code: "not_found", Message: "no auth " + req.AuthIndex}
		}
		return pluginapi.HostAuthGetResponse{AuthIndex: req.AuthIndex, JSON: json.RawMessage(file)}, nil
	})
	fake.Handle(pluginabi.MethodHostHTTPDo, func(payload json.RawMessage) (any, error) {
		var req upstreamRequest
		if errUnmarshal := json.Unmarshal(payload, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.requests = append(h.requests, req)
		answer, ok := h.answers[req.URL]
		if !ok {
			return nil, errors.New("connection refused")
		}
		return answer, nil
	})
	return h
}

func (h *fakeHost) answer(url string, status int, body string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answers[url] = pluginapi.HTTPResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
}

func (h *fakeHost) lastRequest(t *testing.T) upstreamRequest {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.requests) == 0 {
		t.Fatal("no upstream request was sent")
	}
	return h.requests[len(h.requests)-1]
}
