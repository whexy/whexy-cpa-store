package main

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestRouteManagement(t *testing.T) {
	tests := []struct {
		method, path, contentType string
		status                    int
	}{
		{http.MethodGet, logoPath, "image/svg+xml", http.StatusOK},
		{"get", logoPath, "image/svg+xml", http.StatusOK},
		{http.MethodPost, logoPath, "text/plain; charset=utf-8", http.StatusNotFound},
		{http.MethodGet, "/v0/resource/plugins/" + pluginID + "/other", "text/plain; charset=utf-8", http.StatusNotFound},
	}
	for _, tt := range tests {
		resp := routeManagement(pluginapi.ManagementRequest{Method: tt.method, Path: tt.path})
		if resp.StatusCode != tt.status || resp.Headers.Get("Content-Type") != tt.contentType {
			t.Errorf("%s %s = %d %q, want %d %q", tt.method, tt.path, resp.StatusCode, resp.Headers.Get("Content-Type"), tt.status, tt.contentType)
		}
	}
	if resp := routeManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: logoPath}); !bytes.Equal(resp.Body, logoSVG) {
		t.Fatalf("logo body = %q", resp.Body)
	}
}
