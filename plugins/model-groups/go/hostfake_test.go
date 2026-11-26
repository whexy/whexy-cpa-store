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
	"github.com/whexy/whexy-cpa-store/pluginkit/host/hosttest"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

// fakeHost answers the credential callbacks this plugin makes from the
// credentials a test registers, so tests never need CLIProxyAPI itself.
type fakeHost struct {
	*hosttest.Fake

	mu       sync.Mutex
	auths    []pluginapi.HostAuthFileEntry
	runtimes map[string]pluginapi.HostAuthFileEntry
}

// newFakeHost installs a fake host until the test ends.
func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	h := &fakeHost{Fake: hosttest.Install(t), runtimes: map[string]pluginapi.HostAuthFileEntry{}}
	h.Handle(pluginabi.MethodHostAuthList, func(json.RawMessage) (any, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		return map[string]any{"files": append([]pluginapi.HostAuthFileEntry(nil), h.auths...)}, nil
	})
	h.Handle(pluginabi.MethodHostAuthGetRuntime, func(payload json.RawMessage) (any, error) {
		var req pluginapi.HostAuthGetRequest
		if errUnmarshal := json.Unmarshal(payload, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		entry, ok := h.runtimes[req.AuthIndex]
		if !ok {
			return nil, fmt.Errorf("no credential %s", req.AuthIndex)
		}
		return pluginapi.HostAuthGetRuntimeResponse{Auth: entry}, nil
	})
	return h
}

type hostCredentialState int

const (
	credentialReady hostCredentialState = iota
	credentialDisabled
	credentialCooling
)

// addProvider registers count credentials for one provider key.
func (h *fakeHost) addProvider(provider string, count int, state hostCredentialState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for index := range count {
		entry := pluginapi.HostAuthFileEntry{
			ID:        fmt.Sprintf("%s-%d", provider, index),
			Name:      fmt.Sprintf("%s-%d.json", provider, index),
			Type:      provider,
			Provider:  provider,
			Status:    "active",
			AuthIndex: fmt.Sprintf("%d", len(h.auths)),
		}
		switch state {
		case credentialDisabled:
			entry.Disabled = true
			entry.Status = "disabled"
		case credentialCooling:
			entry.NextRetryAfter = time.Now().Add(time.Minute)
		}
		h.auths = append(h.auths, entry)
	}
}

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

// lifecycle wraps the plugin's config node in a plugin.register request.
func lifecycle(configYAML string) []byte {
	raw, _ := json.Marshal(rpc.LifecycleRequest{ConfigYAML: []byte(configYAML)})
	return raw
}

func parseYAML(configYAML []byte) (settings, error) {
	return parseSettings(lifecycle(string(configYAML)))
}

// newTestState builds a plugin state with the given config over the
// installed host.
func newTestState(t *testing.T, configYAML string) *pluginState {
	t.Helper()
	state := newPluginState("")
	state.applyConfig(lifecycle(configYAML))
	state.catalog.refresh()
	return state
}

// decodeResult unwraps a successful RPC envelope into out.
func decodeResult(t *testing.T, raw []byte, out any) {
	t.Helper()
	var env rpc.Envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %s", raw)
	}
	if errUnmarshal := json.Unmarshal(env.Result, out); errUnmarshal != nil {
		t.Fatalf("decode result: %v", errUnmarshal)
	}
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
