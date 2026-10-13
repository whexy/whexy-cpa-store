package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// fakeHost stands in for CPA's host callbacks. Tests that install it must
// not run in parallel, because callHost is package state.
type fakeHost struct {
	mu       sync.Mutex
	calls    []fakeCall
	handlers map[string]func(payload json.RawMessage) (any, error)
}

type fakeCall struct {
	Method  string
	Payload json.RawMessage
}

func installFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	fake := &fakeHost{handlers: make(map[string]func(json.RawMessage) (any, error))}
	previous := callHost
	callHost = fake.call
	t.Cleanup(func() { callHost = previous })
	return fake
}

func (f *fakeHost) handle(method string, fn func(payload json.RawMessage) (any, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method] = fn
}

func (f *fakeHost) call(method string, payload []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Method: method, Payload: append(json.RawMessage(nil), payload...)})
	fn := f.handlers[method]
	f.mu.Unlock()
	if fn == nil {
		return errorEnvelope("host_call_failed", "unsupported host callback "+method), nil
	}
	result, errCall := fn(payload)
	if errCall != nil {
		var hostErr *hostCallError
		if errors.As(errCall, &hostErr) {
			return errorEnvelope(hostErr.Code, hostErr.Message, hostErr.Status), nil
		}
		return errorEnvelope("host_call_failed", errCall.Error()), nil
	}
	return okEnvelope(result)
}

func (f *fakeHost) callsTo(method string) []json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []json.RawMessage
	for _, call := range f.calls {
		if call.Method == method {
			out = append(out, call.Payload)
		}
	}
	return out
}

// fakeDoer answers HTTP requests in process.
type fakeDoer struct {
	mu       sync.Mutex
	requests []pluginapi.HTTPRequest
	respond  func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

func (d *fakeDoer) Do(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	d.mu.Lock()
	d.requests = append(d.requests, req)
	d.mu.Unlock()
	return d.respond(req)
}

func jsonResponse(status int, body []byte) pluginapi.HTTPResponse {
	return pluginapi.HTTPResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json"}}, Body: body}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, errRead := os.ReadFile(filepath.Join("testdata", name))
	if errRead != nil {
		t.Fatal(errRead)
	}
	return raw
}

// useSettings installs cfg for the test and restores the previous settings.
func useSettings(t *testing.T, cfg settings) {
	t.Helper()
	previous := currentConfig.Load()
	currentConfig.Store(&cfg)
	t.Cleanup(func() { currentConfig.Store(previous) })
}

// useCatalogStore gives the test an empty catalog cache.
func useCatalogStore(t *testing.T) *catalogStore {
	t.Helper()
	previous := catalogs
	catalogs = newCatalogStore()
	t.Cleanup(func() { catalogs = previous })
	return catalogs
}

func decodeEnvelope(t *testing.T, raw []byte) envelope {
	t.Helper()
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("envelope %s: %v", raw, errUnmarshal)
	}
	return env
}

func decodeResult[T any](t *testing.T, raw []byte) T {
	t.Helper()
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("envelope failed: %+v", env.Error)
	}
	var out T
	if errUnmarshal := json.Unmarshal(env.Result, &out); errUnmarshal != nil {
		t.Fatalf("result %s: %v", env.Result, errUnmarshal)
	}
	return out
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return raw
}

const (
	testKeyA = "oc_sk_test_aaaaaaaaaaaaaaaaaaaa1111"
	testKeyB = "oc_sk_test_bbbbbbbbbbbbbbbbbbbb2222"
	testKeyC = "oc_sk_test_cccccccccccccccccccc3333"
)
