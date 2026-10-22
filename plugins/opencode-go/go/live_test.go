//go:build live

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Live tests read OPENCODE_API_KEY and call only free endpoints: models.dev,
// the workspace config, the Go gateway's /v1/usage and the Console's usage
// reports.
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

// TestLiveWorkspaceUsage reads every range of the key's workspace. A key
// cannot read the request log, so it must come back as needing a login.
func TestLiveWorkspaceUsage(t *testing.T) {
	key := liveKey(t)
	store := newUsageStore()
	id, errWorkspace := keyWorkspace(context.Background(), netHTTPDoer{}, defaultSettings(), keyEntry{ID: keyIDFor(key), APIKey: key})
	if errWorkspace != nil || !strings.HasPrefix(id, "wrk_") {
		t.Fatalf("workspace %q, %v", id, errWorkspace)
	}
	reader := workspaceReader{cred: credential{ID: "live", Bearer: key, Name: "live key"}}
	workspace := usageWorkspace{ID: id, readers: []workspaceReader{reader}}
	for _, name := range []string{rangeToday, range7d, range30d, rangeAll} {
		q, errQuery := parseUsageQuery(url.Values{"range": {name}}, time.Now())
		if errQuery != nil {
			t.Fatal(errQuery)
		}
		view := store.read(context.Background(), netHTTPDoer{}, defaultSettings(), workspace, q)
		for part, failure := range map[string]string{"summary": view.Summary.Error, "chart": view.Chart.Error, "models": view.Models.Error, "request log": view.RequestLog.Error} {
			if failure != "" {
				t.Fatalf("%s %s: %s", name, part, failure)
			}
		}
		if !view.RequestLog.NeedsLogin {
			t.Errorf("%s: a key read the request log: %d entries", name, len(view.RequestLog.Entries))
		}
		var charted, modelled int64
		for _, bucket := range view.Chart.Buckets {
			charted += bucket.Requests
		}
		for _, row := range view.Models.Rows {
			modelled += row.Requests
		}
		t.Logf("%s: %d requests, $%.2f, %d tokens, %d web searches; %d %s buckets holding %d requests; %d models holding %d requests",
			name, view.Summary.Requests, view.Summary.Cost, view.Summary.Tokens, view.Summary.WebSearches,
			len(view.Chart.Buckets), view.Chart.Bucket, charted, len(view.Models.Rows), modelled)
	}
}
