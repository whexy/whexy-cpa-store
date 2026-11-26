package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/manage"
)

func serve(method, path string) pluginapi.ManagementResponse {
	return router.Serve(manage.Request{ManagementRequest: pluginapi.ManagementRequest{Method: method, Path: path}})
}

func TestRouteManagement(t *testing.T) {
	installHost(t).HandleAuths()
	tests := []struct {
		method, path, contentType string
		status                    int
	}{
		{http.MethodGet, router.PagePath(), "text/html; charset=utf-8", http.StatusOK},
		{http.MethodGet, router.LogoPath(), "image/svg+xml", http.StatusOK},
		{"get", accountsPath, "application/json; charset=utf-8", http.StatusOK},
		{http.MethodDelete, accountsPath, "application/json; charset=utf-8", http.StatusMethodNotAllowed},
		{http.MethodGet, loginPath + "/unknown", "application/json; charset=utf-8", http.StatusNotFound},
	}
	for _, tt := range tests {
		resp := serve(tt.method, tt.path)
		if resp.StatusCode != tt.status || resp.Headers.Get("Content-Type") != tt.contentType {
			t.Errorf("%s %s = %d %q, want %d %q", tt.method, tt.path, resp.StatusCode, resp.Headers.Get("Content-Type"), tt.status, tt.contentType)
		}
	}
}

func TestAccountsRouteNeverReturnsTheAccessToken(t *testing.T) {
	fake := installHost(t)
	fake.HandleAuths(pluginapi.HostAuthFileEntry{
		ID:        "zed-oauth-tester.json",
		AuthIndex: "idx1",
		Name:      "zed-oauth-tester.json",
		Type:      providerKey,
		Provider:  providerKey,
		Label:     "tester",
	})
	fake.Handle(pluginabi.MethodHostAuthGet, func(json.RawMessage) (any, error) {
		return pluginapi.HostAuthGetResponse{Name: "zed-oauth-tester.json", JSON: testAuthJSON(t)}, nil
	})
	response := serve(http.MethodGet, accountsPath)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, response.Body)
	}
	if strings.Contains(string(response.Body), "access-token-value") {
		t.Fatalf("account route leaked the access token: %s", response.Body)
	}
	if !strings.Contains(string(response.Body), `"username":"tester"`) {
		t.Fatalf("account route lost the username: %s", response.Body)
	}
}

func TestQuotaIsNotShownForAnotherAccount(t *testing.T) {
	cfg := defaultSettings()
	cfg.DashboardCookie = "session=abc"
	cfg.DashboardAccount = "someone-else"
	useSettings(t, cfg)
	raw, _ := json.Marshal(pluginapi.QuotaFetchRequest{
		AuthID:      "zed-oauth-tester.json",
		Provider:    providerKey,
		StorageJSON: testAuthJSON(t),
	})
	out, errFetch := handleQuotaFetch(raw)
	if errFetch != nil {
		t.Fatal(errFetch)
	}
	response := decodeResult[pluginapi.QuotaFetchResponse](t, out)
	if len(response.Summary) != 0 || len(response.Groups) != 0 {
		t.Fatalf("another account's usage leaked into the quota card: %+v", response)
	}
}
