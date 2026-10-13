package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The CSP hashes only the first inline script and style.
func TestPageHasOneInlineScriptAndStyle(t *testing.T) {
	page := string(managementPage)
	for _, tag := range []string{"script", "style"} {
		if count := strings.Count(page, "<"+tag); count != 1 {
			t.Errorf("page has %d <%s> elements, want 1", count, tag)
		}
	}
	script := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(page)[1]
	sum := sha256.Sum256([]byte(script))
	if want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; !strings.Contains(managementPageCSP, "script-src "+want) {
		t.Fatalf("CSP %q does not admit the page script %s", managementPageCSP, want)
	}
}

func TestRouteManagement(t *testing.T) {
	configureYAML(t, "rules: [{models: [a], prompt: b}, {models: [], prompt: c}]")
	tests := []struct {
		method, path, contentType string
		status                    int
	}{
		{http.MethodGet, resourcePagePath, "text/html; charset=utf-8", http.StatusOK},
		{http.MethodGet, logoPath, "image/svg+xml", http.StatusOK},
		{"get", statusPath, "application/json; charset=utf-8", http.StatusOK},
		{http.MethodPost, statusPath, "application/json; charset=utf-8", http.StatusNotFound},
	}
	for _, tt := range tests {
		resp := routeManagement(pluginapi.ManagementRequest{Method: tt.method, Path: tt.path})
		if resp.StatusCode != tt.status || resp.Headers.Get("Content-Type") != tt.contentType {
			t.Errorf("%s %s = %d %q, want %d %q", tt.method, tt.path, resp.StatusCode, resp.Headers.Get("Content-Type"), tt.status, tt.contentType)
		}
	}

	resp := routeManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: statusPath})
	var view statusView
	if errUnmarshal := json.Unmarshal(resp.Body, &view); errUnmarshal != nil {
		t.Fatalf("decode status: %v", errUnmarshal)
	}
	if view.Generation == 0 || view.AppliedAt == "" || view.Rules != 1 || len(view.Problems) != 1 || view.Problems[0].Rule != 2 {
		t.Fatalf("status = %+v", view)
	}
}
