package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// fakeHost answers the host callbacks this plugin makes, so tests never need
// CLIProxyAPI itself.
type fakeHost struct {
	mu    sync.Mutex
	auths []pluginapi.HostAuthFileEntry
	// files are the auth files by auth index.
	files map[string]string
	// answers are the upstream responses by URL.
	answers  map[string]hostHTTPResponse
	requests []hostHTTPRequest
	opened   int
}

func newFakeHost() *fakeHost {
	return &fakeHost{files: map[string]string{}, answers: map[string]hostHTTPResponse{}}
}

func (h *fakeHost) install(t *testing.T) {
	t.Helper()
	callHost = h.call
	t.Cleanup(func() { callHost = nil })
}

func (h *fakeHost) call(method string, payload []byte) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch method {
	case pluginabi.MethodHostAuthList:
		return okEnvelope(hostAuthListResponse{Files: h.auths})
	case pluginabi.MethodHostAuthGet:
		var req hostAuthGetRequest
		_ = json.Unmarshal(payload, &req)
		file, ok := h.files[req.AuthIndex]
		if !ok {
			return errorEnvelope("not_found", "no auth "+req.AuthIndex), nil
		}
		return okEnvelope(hostAuthGetResponse{JSON: json.RawMessage(file)})
	case pluginabi.MethodHostHTTPOperationOpen:
		h.opened++
		return okEnvelope(hostOperation{OperationID: fmt.Sprintf("op-%d", h.opened)})
	case pluginabi.MethodHostHTTPCancel, pluginabi.MethodHostLog:
		return okEnvelope(struct{}{})
	case pluginabi.MethodHostHTTPDo:
		var req hostHTTPRequest
		_ = json.Unmarshal(payload, &req)
		h.requests = append(h.requests, req)
		answer, ok := h.answers[req.URL]
		if !ok {
			return errorEnvelope("http_error", "connection refused"), nil
		}
		return okEnvelope(answer)
	}
	return errorEnvelope("unknown_method", method), nil
}

func (h *fakeHost) answer(url string, status int, body string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answers[url] = hostHTTPResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
}

func (h *fakeHost) lastRequest(t *testing.T) hostHTTPRequest {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.requests) == 0 {
		t.Fatal("no upstream request was sent")
	}
	return h.requests[len(h.requests)-1]
}
