package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// oddKey pins JSON.stringify's escaping: <, >, & and U+2028/U+2029 stay
// raw, a literal backslash before "u2028" is escaped, \b and \f use short
// escapes, other control characters lowercase \u00XX, DEL stays raw.
const oddKey = "sk-<&>\u2028\u2029-\\u2028-\b\f\u0001\u007f-é-\"q\""

func manage(t *testing.T, method, path string, body string) pluginapi.ManagementResponse {
	t.Helper()
	req := mustJSON(t, struct {
		pluginapi.ManagementRequest
		HostCallbackID string `json:"host_callback_id"`
	}{pluginapi.ManagementRequest{Method: method, Path: path, Body: []byte(body)}, "cb-1"})
	raw, errHandle := handleMethod(pluginabi.MethodManagementHandle, req)
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	resp := decodeResult[pluginapi.ManagementResponse](t, raw)
	if path != resourcePagePath && path != logoPath && (resp.Headers.Get("Content-Type") != "application/json; charset=utf-8" || resp.Headers.Get("Cache-Control") != "no-store") {
		t.Fatalf("%s %s headers = %v", method, path, resp.Headers)
	}
	return resp
}

func errorText(t *testing.T, resp pluginapi.ManagementResponse) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(resp.Body, &body); errUnmarshal != nil || body.Error == "" {
		t.Fatalf("error body %s: %v", resp.Body, errUnmarshal)
	}
	return body.Error
}

func TestManagementRegistrationWireShape(t *testing.T) {
	raw, errHandle := handleMethod(pluginabi.MethodManagementRegister, []byte(`{}`))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	if strings.Contains(string(raw), "handle") {
		t.Fatalf("registration leaks handler fields: %s", raw)
	}
	reg := decodeResult[pluginapi.ManagementRegistrationResponse](t, raw)
	var routes []string
	for _, route := range reg.Routes {
		routes = append(routes, route.Method+" "+route.Path)
	}
	want := "GET /v0/management/client-keys/status,GET /v0/management/client-keys/models,POST /v0/management/client-keys/fingerprints"
	if strings.Join(routes, ",") != want {
		t.Fatalf("routes = %v", routes)
	}
	if len(reg.Resources) != 2 || reg.Resources[0].Path != "/v0/resource/plugins/client-keys/index.html" || reg.Resources[0].Menu != "Client keys" || reg.Resources[0].Description == "" ||
		reg.Resources[1].Path != "/v0/resource/plugins/client-keys/logo.svg" || reg.Resources[1].Menu != "" {
		t.Fatalf("resources = %+v", reg.Resources)
	}
}

func TestLogo(t *testing.T) {
	resp := manage(t, "GET", logoPath, "")
	if resp.StatusCode != http.StatusOK || !bytes.Equal(resp.Body, logoSVG) || resp.Headers.Get("Content-Type") != "image/svg+xml" || resp.Headers.Get("Content-Security-Policy") != "default-src 'none'" {
		t.Fatalf("logo = %d %v", resp.StatusCode, resp.Headers)
	}
	var svg struct {
		XMLName xml.Name
		ViewBox string `xml:"viewBox,attr"`
	}
	if errParse := xml.Unmarshal(logoSVG, &svg); errParse != nil || svg.XMLName.Local != "svg" || svg.ViewBox != "0 0 24 24" {
		t.Fatalf("logo is not a 24x24 SVG: %+v %v", svg, errParse)
	}
	if pluginRegistration().Metadata.Logo != logoPath {
		t.Fatalf("Metadata.Logo = %q", pluginRegistration().Metadata.Logo)
	}
}

func TestManagementPage(t *testing.T) {
	resp := manage(t, "get", resourcePagePath, "")
	if resp.StatusCode != http.StatusOK || string(resp.Body) != string(managementPage) {
		t.Fatalf("page = %d %.80s", resp.StatusCode, resp.Body)
	}
	for header, want := range map[string]string{
		"Content-Type":           "text/html; charset=utf-8",
		"Cache-Control":          "no-cache",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := resp.Headers.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	csp := resp.Headers.Get("Content-Security-Policy")
	// A page whose inline script or style the hashes miss would render
	// without it, so both must be found.
	for _, want := range []string{"default-src 'none'", "script-src 'sha256-", "style-src 'sha256-", "connect-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP %q lacks %q", csp, want)
		}
	}
}

func TestFingerprints(t *testing.T) {
	resp := manage(t, http.MethodPost, fingerprintsPath, string(mustJSON(t, map[string]any{
		"keys":     []string{cursorKey, oddKey, n8nKey},
		"api_base": "https://cpa.example.com/",
	})))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d %s", resp.StatusCode, resp.Body)
	}
	var got struct {
		Fingerprints []fingerprintView `json:"fingerprints"`
	}
	if errUnmarshal := json.Unmarshal(resp.Body, &got); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if len(got.Fingerprints) != 3 || got.Fingerprints[0].Scope != cursorScope || got.Fingerprints[2].Scope != n8nScope || got.Fingerprints[1].Scope != session.CallerScope(oddKey) {
		t.Fatalf("scopes = %+v", got.Fingerprints)
	}
	// Computed with Node: sha256 of JSON.stringify(["api-key-name", apiBase, key]).
	if got.Fingerprints[1].NameFingerprint != "8c6d82a34d3aa646aa42ffaa659eb25992f8dd447b5e16a3756b0d5f658a78a6" {
		t.Fatalf("name fingerprint of the odd key = %s", got.Fingerprints[1].NameFingerprint)
	}
	for _, key := range []string{cursorKey, n8nKey, "sk-"} {
		if strings.Contains(string(resp.Body), key) {
			t.Fatalf("response echoes a key: %s", resp.Body)
		}
	}
}

func TestNameFingerprintVectors(t *testing.T) {
	// Computed with Node: sha256 of JSON.stringify(["api-key-name", apiBase, key]).
	tests := []struct {
		apiBase, key, want string
	}{
		{apiBase: "http://localhost:8317", key: cursorKey, want: "4c21758f13f0788bf7b559f727c717d194c6a6d70dbc2c126fa65cadc9e07bce"},
		{apiBase: "https://cpa.example.com/", key: oddKey, want: "8c6d82a34d3aa646aa42ffaa659eb25992f8dd447b5e16a3756b0d5f658a78a6"},
		{apiBase: "", key: cursorKey, want: "3a5995c16b060dee175776c1e920336364adc54a121914b837ae82c4c3a345a9"},
	}
	for _, tt := range tests {
		if got := nameFingerprint(tt.apiBase, tt.key); got != tt.want {
			t.Errorf("nameFingerprint(%q, %q) = %s, want %s", tt.apiBase, tt.key, got, tt.want)
		}
	}
	if got := string(jsonStringify([]string{oddKey})); got != "[\"sk-<&>\u2028\u2029-\\\\u2028-\\b\\f\\u0001\u007f-é-\\\"q\\\"\"]" {
		t.Fatalf("jsonStringify = %s", got)
	}
}

func TestFingerprintsWithoutAPIBase(t *testing.T) {
	for _, body := range []string{`{"keys":["` + cursorKey + `"]}`, `{"keys":["` + cursorKey + `"],"api_base":null}`} {
		resp := manage(t, http.MethodPost, fingerprintsPath, body)
		want := `{"fingerprints":[{"scope":"` + cursorScope + `"}]}`
		if resp.StatusCode != http.StatusOK || string(resp.Body) != want {
			t.Fatalf("%s = %d %s", body, resp.StatusCode, resp.Body)
		}
	}
	resp := manage(t, http.MethodPost, fingerprintsPath, `{"keys":[]}`)
	if resp.StatusCode != http.StatusOK || string(resp.Body) != `{"fingerprints":[]}` {
		t.Fatalf("no keys = %d %s", resp.StatusCode, resp.Body)
	}
}

func TestFingerprintsRejectsBadBodies(t *testing.T) {
	tooMany := make([]string, maxFingerprintKeys+1)
	for index := range tooMany {
		tooMany[index] = "sk-many"
	}
	tests := []struct {
		name, body, want string
	}{
		{name: "empty body", body: "", want: "empty"},
		{name: "not JSON", body: `{"keys":["sk-secret-value"`, want: "must be"},
		{name: "keys missing", body: `{"api_base":"x"}`, want: "keys is required"},
		{name: "keys not strings", body: `{"keys":[1]}`, want: "must be"},
		{name: "blank key", body: `{"keys":["sk-secret-value","  "]}`, want: "keys[1] is empty"},
		{name: "too many keys", body: string(mustJSON(t, map[string]any{"keys": tooMany})), want: "At most 1000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := manage(t, http.MethodPost, fingerprintsPath, tt.body)
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(errorText(t, resp), tt.want) {
				t.Fatalf("response = %d %s", resp.StatusCode, resp.Body)
			}
			if strings.Contains(string(resp.Body), "sk-") {
				t.Fatalf("error echoes a key: %s", resp.Body)
			}
		})
	}
	resp := manage(t, http.MethodPost, fingerprintsPath, string(mustJSON(t, map[string]any{"keys": tooMany[:maxFingerprintKeys]})))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("1000 keys = %d", resp.StatusCode)
	}
}

func TestModelsRoute(t *testing.T) {
	useEmptyCatalog(t)
	resp := manage(t, http.MethodGet, modelsPath, "")
	if resp.StatusCode != http.StatusOK || string(resp.Body) != `{"models":[],"captured_at":null}` {
		t.Fatalf("empty catalog = %d %s", resp.StatusCode, resp.Body)
	}
	at := time.Date(2026, 10, 12, 8, 30, 0, 0, time.FixedZone("CST", 8*3600))
	captureCatalog([]byte(`{"object":"list","data":[{"id":"gpt-5.4"},{"id":"o3"}]}`), at)
	resp = manage(t, http.MethodGet, modelsPath, "")
	if string(resp.Body) != `{"models":["gpt-5.4","o3"],"captured_at":"2026-10-12T00:30:00Z"}` {
		t.Fatalf("catalog = %s", resp.Body)
	}
	captureCatalog([]byte(`{"object":"list","data":[]}`), at)
	resp = manage(t, http.MethodGet, modelsPath, "")
	if string(resp.Body) != `{"models":[],"captured_at":"2026-10-12T00:30:00Z"}` {
		t.Fatalf("empty captured catalog = %s", resp.Body)
	}
}

func TestStatusRoute(t *testing.T) {
	useConfig(t, fixtureConfig+"  cursor:\n    name: typo\n")
	resp := manage(t, http.MethodGet, statusPath, "")
	var view map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(resp.Body, &view); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	rules := currentRules()
	if string(view["generation"]) != strings.TrimSpace(string(mustJSON(t, rules.generation))) || string(view["applied_at"]) != `"`+rules.appliedAt.Format(time.RFC3339)+`"` {
		t.Fatalf("status = %s", resp.Body)
	}
	if _, hasError := view["error"]; hasError || string(view["response_interceptor"]) != "true" {
		t.Fatalf("status = %s", resp.Body)
	}
	wantKeys := `{"` + cursorScope + `":{"policies":["models","responses-api"]},"` + n8nScope + `":{"policies":["models"]},"` +
		invalidScope + `":{"error":"policies: unknown policy type \"budget\""},"` + openScope + `":{},"cursor":{"error":"the name is not a caller scope (64 hex characters), so no request matches it"}}`
	var gotKeys, wantDecoded map[string]entryStatus
	_ = json.Unmarshal(view["keys"], &gotKeys)
	_ = json.Unmarshal([]byte(wantKeys), &wantDecoded)
	if string(mustJSON(t, gotKeys)) != string(mustJSON(t, wantDecoded)) {
		t.Fatalf("keys = %s\nwant %s", view["keys"], wantKeys)
	}

	useConfig(t, "keys: [\n")
	resp = manage(t, http.MethodGet, statusPath, "")
	var failed statusView
	if errUnmarshal := json.Unmarshal(resp.Body, &failed); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !strings.Contains(failed.Error, "decode plugin config") || !failed.ResponseInterceptor || failed.Keys == nil || len(failed.Keys) != 0 || !strings.Contains(string(resp.Body), `"keys":{}`) {
		t.Fatalf("failed status = %s", resp.Body)
	}
}

func TestStatusBeforeRegistration(t *testing.T) {
	previous := currentRuleSet.Load()
	currentRuleSet.Store(nil)
	t.Cleanup(func() { currentRuleSet.Store(previous) })
	resp := manage(t, http.MethodGet, statusPath, "")
	if string(resp.Body) != `{"generation":0,"response_interceptor":false,"keys":{}}` {
		t.Fatalf("status = %s", resp.Body)
	}
}

func TestUnknownManagementRoute(t *testing.T) {
	for _, call := range [][2]string{{http.MethodGet, managementBasePath + "/keys"}, {http.MethodPost, statusPath}, {http.MethodGet, fingerprintsPath}} {
		resp := manage(t, call[0], call[1], "")
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(errorText(t, resp), "no Client keys route") {
			t.Fatalf("%s %s = %d %s", call[0], call[1], resp.StatusCode, resp.Body)
		}
	}
}
