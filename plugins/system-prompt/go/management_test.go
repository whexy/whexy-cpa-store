package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/manage"
)

func serve(method, path string) pluginapi.ManagementResponse {
	return router.Serve(manage.Request{ManagementRequest: pluginapi.ManagementRequest{Method: method, Path: path}})
}

func TestRouteManagement(t *testing.T) {
	configureYAML(t, "rules: [{models: [a], prompt: b}, {models: [], prompt: c}]")
	tests := []struct {
		method, path, contentType string
		status                    int
	}{
		{http.MethodGet, router.PagePath(), "text/html; charset=utf-8", http.StatusOK},
		{http.MethodGet, router.LogoPath(), "image/svg+xml", http.StatusOK},
		{"get", statusPath, "application/json; charset=utf-8", http.StatusOK},
		{http.MethodPost, statusPath, "application/json; charset=utf-8", http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		resp := serve(tt.method, tt.path)
		if resp.StatusCode != tt.status || resp.Headers.Get("Content-Type") != tt.contentType {
			t.Errorf("%s %s = %d %q, want %d %q", tt.method, tt.path, resp.StatusCode, resp.Headers.Get("Content-Type"), tt.status, tt.contentType)
		}
	}

	var view statusView
	if errUnmarshal := json.Unmarshal(serve(http.MethodGet, statusPath).Body, &view); errUnmarshal != nil {
		t.Fatalf("decode status: %v", errUnmarshal)
	}
	if view.Generation == 0 || view.AppliedAt == "" || view.Rules != 1 || len(view.Problems) != 1 || view.Problems[0].Rule != 2 {
		t.Fatalf("status = %+v", view)
	}
}
