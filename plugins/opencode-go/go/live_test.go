//go:build live

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Live tests read OPENCODE_API_KEY and call only free endpoints: models.dev,
// the workspace config and the Go gateway's /v1/usage.
//
//	set -a; . /path/to/.env; set +a; go test -tags live -run Live ./...

type netHTTPDoer struct{}

func (netHTTPDoer) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	httpReq, errNew := http.NewRequestWithContext(ctx, req.Method, req.URL, bytes.NewReader(req.Body))
	if errNew != nil {
		return pluginapi.HTTPResponse{}, errNew
	}
	httpReq.Header = req.Headers.Clone()
	resp, errDo := http.DefaultClient.Do(httpReq)
	if errDo != nil {
		return pluginapi.HTTPResponse{}, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(resp.Body)
	return pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Body: body}, errRead
}

func liveKey(t *testing.T) string {
	t.Helper()
	key := strings.TrimSpace(os.Getenv("OPENCODE_API_KEY"))
	if key == "" {
		t.Skip("OPENCODE_API_KEY is not set")
	}
	return key
}

// liveCatalog reads models.dev and the key's workspace config.
func liveCatalog(t *testing.T, key string) (catalog, *modelsDevCatalog, workspaceConfig) {
	t.Helper()
	doc, errDoc := fetchModelsDev(context.Background(), netHTTPDoer{}, defaultSettings())
	if errDoc != nil {
		t.Fatal(errDoc)
	}
	config, errConfig := fetchWorkspaceConfig(context.Background(), netHTTPDoer{}, defaultSettings(), key)
	if errConfig != nil {
		t.Fatal(redactSecrets(errConfig.Error()))
	}
	return buildCatalog(doc, config), doc, config
}

func TestLiveCatalog(t *testing.T) {
	key := liveKey(t)
	cat, _, _ := liveCatalog(t, key)
	if len(cat.Models) == 0 || cat.Workspace == "" {
		t.Fatalf("catalog has %d models, workspace set: %v", len(cat.Models), cat.Workspace != "")
	}
	snapshotDoc, errSnapshot := parseModelsDev(modelsDevSnapshotJSON)
	if errSnapshot != nil {
		t.Fatal(errSnapshot)
	}
	snapshot := buildCatalog(snapshotDoc, workspaceConfig{})
	cfg := defaultSettings()
	for _, model := range cat.Models {
		route := model.route(cfg)
		if !strings.HasPrefix(route.Endpoint, defaultBaseURL+"/") {
			t.Fatalf("%s routes outside the Go gateway: %s", model.UpstreamID, route.Endpoint)
		}
		if known, ok := snapshot.find(cfg, model.UpstreamID, model.Family); ok && known.Protocol != model.Protocol {
			t.Errorf("%s protocol changed: snapshot %s, live %s", model.UpstreamID, known.Protocol, model.Protocol)
		}
	}
	// No OpenCode Zen model may be aimed at the Go gateway, which would draw
	// on the subscription instead of Console credits.
	for _, model := range cat.Zen {
		route := model.route(cfg)
		if !strings.HasPrefix(route.Endpoint, defaultZenBaseURL+"/") || strings.HasPrefix(route.Endpoint, defaultBaseURL+"/") {
			t.Fatalf("zen %s routes to %s", model.UpstreamID, route.Endpoint)
		}
		t.Logf("zen %s: %s (%s)", model.UpstreamID, route.Endpoint, model.Protocol)
	}
	t.Logf("live catalog: %d Go models, %d Zen models", len(cat.Models), len(cat.Zen))
}

func TestLiveUsage(t *testing.T) {
	key := liveKey(t)
	report, errFetch := fetchUsage(context.Background(), netHTTPDoer{}, defaultSettings(), key)
	if errFetch != nil {
		t.Fatal(redactSecrets(errFetch.Error()))
	}
	if len(report.Windows) != 3 {
		t.Fatalf("windows = %+v", report.Windows)
	}
	for _, window := range report.Windows {
		if window.ResetsAt.IsZero() {
			t.Errorf("%s has no reset time", window.Name)
		}
		t.Logf("%s: status %s, %.1f%% used, resets %s", window.Name, window.Status, window.Percent, window.ResetsAt)
	}
}
