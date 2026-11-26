package main

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/manage"
)

func serve(method, path string) pluginapi.ManagementResponse {
	return router.Serve(manage.Request{ManagementRequest: pluginapi.ManagementRequest{Method: method, Path: path}})
}

func TestRouteManagement(t *testing.T) {
	tests := []struct {
		method, path, contentType string
		status                    int
	}{
		{http.MethodGet, router.LogoPath(), "image/svg+xml", http.StatusOK},
		{"get", router.LogoPath(), "image/svg+xml", http.StatusOK},
		{http.MethodPost, router.LogoPath(), "application/json; charset=utf-8", http.StatusNotFound},
		{http.MethodGet, manage.ResourcePath(pluginID, "index.html"), "application/json; charset=utf-8", http.StatusNotFound},
	}
	for _, tt := range tests {
		resp := serve(tt.method, tt.path)
		if resp.StatusCode != tt.status || resp.Headers.Get("Content-Type") != tt.contentType {
			t.Errorf("%s %s = %d %q, want %d %q", tt.method, tt.path, resp.StatusCode, resp.Headers.Get("Content-Type"), tt.status, tt.contentType)
		}
	}
	if resp := serve(http.MethodGet, router.LogoPath()); !bytes.Equal(resp.Body, logoSVG) {
		t.Fatalf("logo body = %q", resp.Body)
	}
}
