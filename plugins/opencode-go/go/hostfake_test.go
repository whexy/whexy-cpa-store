package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

// The wire shapes of the host callbacks the fakes answer.
type (
	hostHTTPRequest struct {
		HostCallbackID string      `json:"host_callback_id,omitempty"`
		OperationID    string      `json:"operation_id,omitempty"`
		Method         string      `json:"method,omitempty"`
		URL            string      `json:"url,omitempty"`
		Headers        http.Header `json:"headers,omitempty"`
		Body           []byte      `json:"body,omitempty"`
	}
	hostHTTPStreamStart struct {
		StatusCode int         `json:"status_code"`
		Headers    http.Header `json:"headers,omitempty"`
		StreamID   string      `json:"stream_id,omitempty"`
	}
	hostStreamIDRequest struct {
		StreamID string `json:"stream_id"`
	}
	hostStreamReadResponse struct {
		Payload []byte `json:"payload,omitempty"`
		Error   string `json:"error,omitempty"`
		Done    bool   `json:"done,omitempty"`
	}
	hostStreamEmitRequest struct {
		StreamID string `json:"stream_id"`
		Payload  []byte `json:"payload,omitempty"`
		Error    string `json:"error,omitempty"`
	}
	hostAuthListResponse struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
)

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

// useCatalogStore gives the test an empty catalog cache and an empty
// models.dev cache.
func useCatalogStore(t *testing.T) *catalogStore {
	t.Helper()
	previous, previousModelsDev := catalogs, modelsDev
	catalogs, modelsDev = newCatalogStore(), newModelsDevStore()
	t.Cleanup(func() { catalogs, modelsDev = previous, previousModelsDev })
	return catalogs
}

// useModelsDev gives the test a models.dev cache holding
// testdata/models_dev.json, read just now.
func useModelsDev(t *testing.T) *modelsDevStore {
	t.Helper()
	previous := modelsDev
	modelsDev = newModelsDevStore()
	modelsDev.entry = &modelsDevEntry{doc: fixtureModelsDev(t), fetchedAt: time.Now()}
	t.Cleanup(func() { modelsDev = previous })
	return modelsDev
}

func decodeEnvelope(t *testing.T, raw []byte) rpc.Envelope {
	t.Helper()
	var env rpc.Envelope
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
