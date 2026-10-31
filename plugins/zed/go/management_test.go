package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestAccountsRouteNeverReturnsTheAccessToken(t *testing.T) {
	fake := installFakeHost(t)
	fake.handle(pluginabi.MethodHostAuthList, func(json.RawMessage) (any, error) {
		return hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{
			ID:        "zed-oauth-tester.json",
			AuthIndex: "idx1",
			Name:      "zed-oauth-tester.json",
			Type:      providerKey,
			Provider:  providerKey,
			Label:     "tester",
		}}}, nil
	})
	fake.handle(pluginabi.MethodHostAuthGet, func(json.RawMessage) (any, error) {
		return pluginapi.HostAuthGetResponse{Name: "zed-oauth-tester.json", JSON: testAuthJSON(t)}, nil
	})
	response := routeManagement(newHost(""), pluginapi.ManagementRequest{Method: http.MethodGet, Path: accountsPath})
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
