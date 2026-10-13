package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// fakeHost answers the host callbacks this plugin makes, so tests never need
// CLIProxyAPI itself.
type fakeHost struct {
	mu       sync.Mutex
	auths    []pluginapi.HostAuthFileEntry
	runtimes map[string]pluginapi.HostAuthFileEntry
	logs     []string
	calls    []string
}

func newFakeHost() *fakeHost {
	return &fakeHost{runtimes: map[string]pluginapi.HostAuthFileEntry{}}
}

// addProvider registers count credentials for one provider key.
func (h *fakeHost) addProvider(provider string, count int, state hostCredentialState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for index := range count {
		entry := pluginapi.HostAuthFileEntry{
			ID:       fmt.Sprintf("%s-%d", provider, index),
			Name:     fmt.Sprintf("%s-%d.json", provider, index),
			Type:     provider,
			Provider: provider,
			Status:   "active",
		}
		switch state {
		case credentialDisabled:
			entry.Disabled = true
			entry.Status = "disabled"
		case credentialCooling:
			entry.NextRetryAfter = time.Now().Add(time.Minute)
		}
		entry.AuthIndex = fmt.Sprintf("%d", len(h.auths))
		h.auths = append(h.auths, entry)
	}
}

type hostCredentialState int

const (
	credentialReady hostCredentialState = iota
	credentialDisabled
	credentialCooling
)

// addRuntime registers the credential a host would describe for one auth index.
func (h *fakeHost) addRuntime(authIndex, provider, email string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runtimes[authIndex] = pluginapi.HostAuthFileEntry{
		AuthIndex: authIndex,
		Provider:  provider,
		Email:     email,
	}
}

func (h *fakeHost) install(t *testing.T) {
	t.Helper()
	callHost = h.call
	t.Cleanup(func() { callHost = nil })
}

func (h *fakeHost) call(method string, payload []byte) ([]byte, error) {
	h.mu.Lock()
	h.calls = append(h.calls, method)
	h.mu.Unlock()

	switch method {
	case pluginabi.MethodHostAuthList:
		h.mu.Lock()
		files := append([]pluginapi.HostAuthFileEntry(nil), h.auths...)
		h.mu.Unlock()
		return envelopeOf(hostAuthListResponse{Files: files})

	case pluginabi.MethodHostAuthGetRuntime:
		var req pluginapi.HostAuthGetRequest
		if errUnmarshal := json.Unmarshal(payload, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		h.mu.Lock()
		entry, ok := h.runtimes[req.AuthIndex]
		h.mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("no credential %s", req.AuthIndex)
		}
		return envelopeOf(pluginapi.HostAuthGetRuntimeResponse{Auth: entry})

	case pluginabi.MethodHostLog:
		var req hostLogRequest
		_ = json.Unmarshal(payload, &req)
		h.mu.Lock()
		h.logs = append(h.logs, req.Message)
		h.mu.Unlock()
		return envelopeOf(struct{}{})
	}
	return nil, fmt.Errorf("unexpected host method %s", method)
}

func envelopeOf(value any) ([]byte, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

// newTestState builds a plugin state with the given config and a fake host.
func newTestState(t *testing.T, configYAML string, host *fakeHost) *pluginState {
	t.Helper()
	host.install(t)
	state := newPluginState("")
	state.applyConfig([]byte(configYAML))
	state.catalog.refresh()
	return state
}

// routeFor asks the state for a routing decision for a model, with optional
// request headers that carry a session.
func routeFor(state *pluginState, model string, headers map[string][]string, available []string) (pluginapi.ModelRouteResponse, bool) {
	req := pluginapi.ModelRouteRequest{
		RequestedModel:     model,
		SourceFormat:       "openai",
		AvailableProviders: available,
	}
	if len(headers) > 0 {
		req.Headers = http.Header(headers)
	}
	return state.route(req)
}

// observeAccount feeds the post-credential interceptor what CPA reports for a
// request, so the account column can be built from it.
func observeAccount(t *testing.T, requestedModel, sessionHeader, authID, authIndex string) {
	t.Helper()
	payload, errMarshal := json.Marshal(pluginapi.RequestInterceptRequest{
		RequestedModel: requestedModel,
		SourceFormat:   "claude",
		Headers:        http.Header{"X-Claude-Code-Session-Id": []string{sessionHeader}},
		Metadata: map[string]any{
			"selected_auth_id":    authID,
			"selected_auth_index": authIndex,
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal interceptor request: %v", errMarshal)
	}
	if _, errCall := interceptRequestAfterAuth(payload); errCall != nil {
		t.Fatalf("interceptor: %v", errCall)
	}
}

// pluginapiRouteRequest builds a route request with request headers, for the
// session extraction tests.
func pluginapiRouteRequest(headers http.Header) pluginapi.ModelRouteRequest {
	req := pluginapi.ModelRouteRequest{RequestedModel: "auto-group/cycle", SourceFormat: "claude"}
	if headers != nil {
		req.Headers = headers
	}
	return req
}

// multiProviderConfig is a two-member weighted group over two providers.
const multiProviderConfig = `
groups:
  - name: Fast
    slug: fast
    policy: weighted-round-robin
    members:
      - model: claude-haiku-5-5
        providers: [claude]
        weight: 3
      - model: kimi-k3
        providers: [opencode-go]
        weight: 1
`
