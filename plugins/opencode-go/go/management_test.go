package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// managementFixture runs key routes against an in-memory auth directory
// and a fake OpenCode behind host.http.do.
type managementFixture struct {
	t     *testing.T
	fake  *fakeHost
	files map[string]string
	// runtime overrides the host's runtime record per auth ID; other
	// opencode-go auths report "active".
	runtime  map[string]pluginapi.HostAuthFileEntry
	upstream func(req hostHTTPRequest) (any, error)
}

type managementResult struct {
	Status  int
	Headers http.Header
	Body    []byte
}

func newManagementFixture(t *testing.T, files map[string]string) *managementFixture {
	t.Helper()
	useSettings(t, defaultSettings())
	useCatalogStore(t)
	useModelsDev(t)
	fx := &managementFixture{t: t, files: files, runtime: map[string]pluginapi.HostAuthFileEntry{}}
	fx.fake = keyStoreHost(t, files)
	fx.fake.mu.Lock()
	list := fx.fake.handlers[pluginabi.MethodHostAuthList]
	fx.fake.mu.Unlock()
	fx.fake.handle(pluginabi.MethodHostAuthList, func(payload json.RawMessage) (any, error) {
		result, errList := list(payload)
		if errList != nil {
			return nil, errList
		}
		resp := result.(hostAuthListResponse)
		for index, entry := range resp.Files {
			if entry.Provider != providerKey {
				continue
			}
			resp.Files[index].Status = "active"
			if override, ok := fx.runtime[entry.ID]; ok {
				resp.Files[index].Status = override.Status
				resp.Files[index].StatusMessage = override.StatusMessage
				resp.Files[index].Disabled = override.Disabled
				resp.Files[index].Unavailable = override.Unavailable
				resp.Files[index].NextRetryAfter = override.NextRetryAfter
				resp.Files[index].Success = override.Success
				resp.Files[index].Failed = override.Failed
			}
		}
		return resp, nil
	})
	usage, config, models := readFixture(t, "usage.json"), readFixture(t, "config.json"), readFixture(t, "models_dev.json")
	fx.upstream = func(req hostHTTPRequest) (any, error) {
		switch req.URL {
		case usageURL(defaultSettings()):
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: usage}, nil
		case configURL(defaultSettings()):
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: config}, nil
		case modelsDevURL:
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: models}, nil
		}
		t.Errorf("unexpected upstream request %s", req.URL)
		return pluginapi.HTTPResponse{StatusCode: http.StatusNotFound}, nil
	}
	fx.fake.handle(pluginabi.MethodHostHTTPDo, func(payload json.RawMessage) (any, error) {
		var req hostHTTPRequest
		if errUnmarshal := json.Unmarshal(payload, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		return fx.upstream(req)
	})
	return fx
}

func (fx *managementFixture) call(method, path string, query url.Values, body any) managementResult {
	fx.t.Helper()
	var raw []byte
	switch value := body.(type) {
	case nil:
	case string:
		raw = []byte(value)
	default:
		raw = mustJSON(fx.t, value)
	}
	req := mustJSON(fx.t, rpcManagementRequest{
		ManagementRequest: pluginapi.ManagementRequest{Method: method, Path: path, Query: query, Body: raw},
		HostCallbackID:    "cb-mgmt",
	})
	out, errHandle := handleMethod(pluginabi.MethodManagementHandle, req)
	if errHandle != nil {
		fx.t.Fatal(errHandle)
	}
	resp := decodeResult[pluginapi.ManagementResponse](fx.t, out)
	result := managementResult{Status: resp.StatusCode, Headers: resp.Headers, Body: resp.Body}
	assertNoFullKey(fx.t, result.Body)
	if !strings.HasPrefix(path, "/v0/resource/") {
		if resp.Headers.Get("Content-Type") != "application/json; charset=utf-8" || resp.Headers.Get("Cache-Control") != "no-store" {
			fx.t.Fatalf("%s %s headers = %v", method, path, resp.Headers)
		}
	}
	return result
}

// serveConsole answers OpenCode Console requests from console. Token
// requests run under a timeout, so they open a cancellable host operation.
func (fx *managementFixture) serveConsole(console *fakeConsole) {
	ok := func(json.RawMessage) (any, error) { return struct{}{}, nil }
	fx.fake.handle(pluginabi.MethodHostHTTPOperationOpen, func(json.RawMessage) (any, error) {
		return hostOperationResponse{OperationID: "op-1"}, nil
	})
	fx.fake.handle(pluginabi.MethodHostHTTPCancel, ok)
	fx.fake.handle(pluginabi.MethodHostLog, ok)
	reply := console.reply(fx.t, nil)
	fx.upstream = func(req hostHTTPRequest) (any, error) {
		got := reply(req)
		return pluginapi.HTTPResponse{StatusCode: got.Status, Headers: got.Headers, Body: got.Body}, nil
	}
}

func (fx *managementFixture) httpCalls() []hostHTTPRequest {
	var out []hostHTTPRequest
	for _, payload := range fx.fake.callsTo(pluginabi.MethodHostHTTPDo) {
		var req hostHTTPRequest
		_ = json.Unmarshal(payload, &req)
		out = append(out, req)
	}
	return out
}

func (fx *managementFixture) saves() int {
	return len(fx.fake.callsTo(pluginabi.MethodHostAuthSave))
}

func (fx *managementFixture) storedFile(name string) keyFile {
	fx.t.Helper()
	file, errParse := parseKeyFile([]byte(fx.files[name]))
	if errParse != nil {
		fx.t.Fatalf("%s: %v", name, errParse)
	}
	return file
}

// topLevel returns the raw top-level fields of a stored auth file.
func (fx *managementFixture) topLevel(name string) map[string]string {
	fx.t.Helper()
	var top map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal([]byte(fx.files[name]), &top); errUnmarshal != nil {
		fx.t.Fatalf("%s: %v", name, errUnmarshal)
	}
	out := make(map[string]string, len(top))
	for field, value := range top {
		out[field] = string(value)
	}
	return out
}

func assertNoFullKey(t *testing.T, body []byte) {
	t.Helper()
	for _, key := range []string{testKeyA, testKeyB, testKeyC} {
		if strings.Contains(string(body), key) {
			t.Fatalf("response carries a full key: %s", body)
		}
	}
	if secretPattern.Match(body) {
		t.Fatalf("response carries an oc_sk_ token: %s", body)
	}
	if loginTokenPattern.Match(body) {
		t.Fatalf("response carries a login token: %s", body)
	}
}

func decodeBodyAs[T any](t *testing.T, result managementResult) T {
	t.Helper()
	var out T
	if errUnmarshal := json.Unmarshal(result.Body, &out); errUnmarshal != nil {
		t.Fatalf("body %s: %v", result.Body, errUnmarshal)
	}
	return out
}

func (r managementResult) errorText(t *testing.T) string {
	t.Helper()
	return decodeBodyAs[struct {
		Error string `json:"error"`
	}](t, r).Error
}

func upstreamJSON(status int, body string) (any, error) {
	return pluginapi.HTTPResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}, nil
}

func TestManagementRegistrationWireShape(t *testing.T) {
	raw, errHandle := handleMethod(pluginabi.MethodManagementRegister, []byte(`{}`))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	if !strings.Contains(string(raw), `"Path":"/v0/resource/plugins/opencode-go/index.html"`) || !strings.Contains(string(raw), `"Menu":"OpenCode Go"`) {
		t.Fatalf("registration = %s", raw)
	}
	if strings.Contains(string(raw), "handle") {
		t.Fatalf("registration leaks handler fields: %s", raw)
	}
	reg := decodeResult[pluginapi.ManagementRegistrationResponse](t, raw)
	var got []string
	for _, route := range reg.Routes {
		got = append(got, route.Method+" "+route.Path)
	}
	want := []string{
		"GET /v0/management/opencode-go/keys",
		"POST /v0/management/opencode-go/keys",
		"PATCH /v0/management/opencode-go/keys",
		"DELETE /v0/management/opencode-go/keys",
		"POST /v0/management/opencode-go/keys/test",
		"GET /v0/management/opencode-go/keys/usage",
		"GET /v0/management/opencode-go/logins",
		"PATCH /v0/management/opencode-go/logins",
		"GET /v0/management/opencode-go/logins/usage",
		"GET /v0/management/opencode-go/models",
		"POST /v0/management/opencode-go/models/refresh",
		"GET /v0/management/opencode-go/routing",
		"PUT /v0/management/opencode-go/routing",
		"GET /v0/management/opencode-go/free",
		"PUT /v0/management/opencode-go/free",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("routes =\n%s", strings.Join(got, "\n"))
	}
	if len(reg.Resources) != 2 || reg.Resources[0].Path != resourcePagePath || reg.Resources[0].Menu != displayName || reg.Resources[1].Path != logoPath || reg.Resources[1].Menu != "" {
		t.Fatalf("resources = %+v", reg.Resources)
	}
}

func TestManagementPageServed(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{})
	resp := fx.call(http.MethodGet, resourcePagePath, nil, nil)
	if resp.Status != http.StatusOK || string(resp.Body) != string(managementPage) {
		t.Fatalf("status %d, %d bytes", resp.Status, len(resp.Body))
	}
	if resp.Headers.Get("Content-Type") != "text/html; charset=utf-8" || resp.Headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers = %v", resp.Headers)
	}
	if len(fx.fake.calls) != 0 {
		t.Fatalf("serving the page called the host: %+v", fx.fake.calls)
	}

	page := string(managementPage)
	csp := resp.Headers.Get("Content-Security-Policy")
	// One style, and two scripts: the config-editing logic, then the UI.
	for tag, count := range map[string]int{"script": 2, "style": 1} {
		if strings.Count(page, "<"+tag) != count {
			t.Fatalf("page must hold %d inline <%s>", count, tag)
		}
		rest := page
		var hashes []string
		for range count {
			start := strings.Index(rest, "<"+tag+">") + len(tag) + 2
			stop := strings.Index(rest, "</"+tag+">")
			sum := sha256.Sum256([]byte(rest[start:stop]))
			hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
			rest = rest[stop+len(tag)+3:]
		}
		if want := tag + "-src " + strings.Join(hashes, " ") + ";"; !strings.Contains(csp, want) {
			t.Fatalf("CSP %q does not allow exactly the inline %ss", csp, tag)
		}
	}
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("CSP = %q", csp)
	}

	// The CSP blocks inline handlers and style attributes, so the markup
	// must not use them; the script sets styles through the CSSOM only.
	markup := page[:strings.Index(page, "<style>")] + page[strings.Index(page, "</style>"):strings.Index(page, "<script>")]
	if regexp.MustCompile(`\son[a-z]+\s*=|\sstyle\s*=`).MatchString(markup) {
		t.Fatal("page markup uses an inline handler or style attribute")
	}
	if strings.Contains(page, "setAttribute(\"style\"") || strings.Contains(page, "innerHTML") {
		t.Fatal("page script builds styles or markup from strings")
	}
	if strings.Contains(page, "\r") {
		t.Fatal("page has CR line endings, which browsers normalize before hashing")
	}

	if secretPattern.MatchString(page) {
		t.Fatal("page embeds an oc_sk_ token")
	}
	for _, asset := range []string{"<link", " src=", "@import", "url(", "http://"} {
		if strings.Contains(page, asset) {
			t.Fatalf("page references an external asset (%s)", asset)
		}
	}
	if strings.Count(page, "https://") != 1 || !strings.Contains(page, `href="https://opencode.ai/v2/docs/console/go/"`) {
		t.Fatal("page links anything but the OpenCode Go docs")
	}
}

func TestManagementListKeys(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{
		"opencode-go.json": strings.Replace(string(multiKeyFile(t)), `"label": "Work",`, `"label": "Work", "zen": true,`, 1),
		"a-single.json":    `{"type":"opencode-go","api_key":"` + testKeyC + `","label":"Solo"}`,
		"claude.json":      `{"type":"claude"}`,
	})
	idA, idB, idC := keyIDFor(testKeyA), keyIDFor(testKeyB), keyIDFor(testKeyC)
	retry := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	fx.runtime[authIDFor(keyFileName, idA)] = pluginapi.HostAuthFileEntry{Status: "active", Success: 12, Failed: 1}
	fx.runtime[authIDFor(keyFileName, idA)+zenAuthSuffix] = pluginapi.HostAuthFileEntry{Status: "error", StatusMessage: "quota", NextRetryAfter: retry}
	fx.runtime[authIDFor(keyFileName, idB)] = pluginapi.HostAuthFileEntry{Status: "disabled", Disabled: true}
	fx.runtime[authIDFor("a-single.json", idC)] = pluginapi.HostAuthFileEntry{
		Status:         "error",
		StatusMessage:  "OpenCode Go returned 401 AuthError: bad key " + testKeyC,
		Unavailable:    true,
		NextRetryAfter: retry,
	}
	catalogs.remember(idA, fixtureConfig(t))

	resp := fx.call(http.MethodGet, keysPath, nil, nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
	keys := decodeBodyAs[struct {
		Keys []keyView `json:"keys"`
	}](t, resp).Keys
	if len(keys) != 3 || keys[0].ID != idA || keys[1].ID != idB || keys[2].ID != idC {
		t.Fatalf("keys = %+v", keys)
	}
	a, b, c := keys[0], keys[1], keys[2]
	if a.Label != "Work" || a.MaskedKey != "oc_sk_…1111" || a.Disabled || a.File != keyFileName || a.AddedAt != "2026-10-12T00:00:00Z" {
		t.Fatalf("key A = %+v", a)
	}
	if a.Workspace != "wrk_TEST000000000000000000000" || a.Models != 7 {
		t.Fatalf("key A workspace = %q, models = %d", a.Workspace, a.Models)
	}
	if a.Runtime == nil || a.Runtime.State != "active" || a.Runtime.Success != 12 || a.Runtime.Failed != 1 {
		t.Fatalf("key A runtime = %+v", a.Runtime)
	}
	if !a.Go || !a.Zen || a.ZenRuntime == nil || a.ZenRuntime.State != "error" || a.ZenRuntime.NextRetryAfter != "2026-10-13T00:00:00Z" {
		t.Fatalf("key A zen = %v, runtime %+v", a.Zen, a.ZenRuntime)
	}
	if b.Label != "" || !b.Disabled || b.MaskedKey != "oc_sk_…2222" || b.Workspace != "" || b.Runtime == nil || b.Runtime.State != "disabled" {
		t.Fatalf("key B = %+v, runtime %+v", b, b.Runtime)
	}
	if c.File != "a-single.json" || c.Label != "Solo" || c.Runtime == nil || c.Runtime.State != "error" || !c.Runtime.Unavailable ||
		c.Runtime.NextRetryAfter != "2026-10-13T00:00:00Z" || !strings.Contains(c.Runtime.Message, "bad key oc_sk_[redacted]") {
		t.Fatalf("key C = %+v, runtime %+v", c, c.Runtime)
	}
	if len(fx.httpCalls()) != 0 {
		t.Fatal("listing keys called OpenCode")
	}
}

func TestManagementListKeysWithoutFiles(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{})
	resp := fx.call(http.MethodGet, keysPath, nil, nil)
	if resp.Status != http.StatusOK || string(resp.Body) != `{"keys":[]}` {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
}

func TestManagementAddKey(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{})
	resp := fx.call(http.MethodPost, keysPath, nil, map[string]string{"label": "  Work ", "api_key": " " + testKeyA + "\n"})
	if resp.Status != http.StatusCreated {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
	if strings.Contains(string(resp.Body), "[redacted]") {
		t.Fatalf("response needed redaction: %s", resp.Body)
	}
	added := decodeBodyAs[struct {
		Key   *keyView  `json:"key"`
		Usage usageView `json:"usage"`
	}](t, resp)
	id := keyIDFor(testKeyA)
	if added.Key == nil || added.Key.ID != id || added.Key.Label != "Work" || added.Key.MaskedKey != "oc_sk_…1111" || added.Key.Disabled {
		t.Fatalf("key = %+v", added.Key)
	}
	if added.Key.Workspace != "wrk_TEST000000000000000000000" || added.Key.Models != 7 || added.Key.Runtime == nil || added.Key.Runtime.State != "active" {
		t.Fatalf("key = %+v", added.Key)
	}
	if len(added.Usage.Windows) != 3 || added.Usage.Windows[0].Label != "5-hour" || added.Usage.Windows[1].UsedPercent != 37.5 ||
		added.Usage.Windows[2].ResetsAt != "2026-11-12T23:27:39Z" || added.Usage.Windows[0].Exhausted {
		t.Fatalf("usage = %+v", added.Usage)
	}

	calls := fx.httpCalls()
	if len(calls) != 2 || calls[0].URL != usageURL(defaultSettings()) || calls[1].URL != configURL(defaultSettings()) {
		t.Fatalf("upstream calls = %+v", calls)
	}
	for _, call := range calls {
		if call.HostCallbackID != "cb-mgmt" || call.Headers.Get("Authorization") != "Bearer "+testKeyA {
			t.Fatalf("upstream call = %s %q %v", call.URL, call.HostCallbackID, call.Headers)
		}
	}

	stored := fx.storedFile(keyFileName)
	if len(stored.Keys) != 1 || stored.Keys[0].APIKey != testKeyA || stored.Keys[0].Label != "Work" {
		t.Fatalf("stored = %+v", stored.Keys)
	}
	if _, errTime := time.Parse(time.RFC3339, stored.Keys[0].AddedAt); errTime != nil {
		t.Fatalf("added_at = %q", stored.Keys[0].AddedAt)
	}

	// The host loads models for the saved auth next; the check's catalog
	// serves it without another fetch.
	auth := parsedAuths(keyFileName, fx.files[keyFileName])[0]
	models, _ := handleMethod(pluginabi.MethodModelForAuth, mustJSON(t, rpcAuthModelRequest{AuthModelRequest: pluginapi.AuthModelRequest{
		AuthID: auth.ID, StorageJSON: auth.StorageJSON, Attributes: auth.Attributes,
	}}))
	if resp := decodeResult[pluginapi.ModelResponse](t, models); len(resp.Models) != 7 || len(fx.httpCalls()) != 2 {
		t.Fatalf("model refresh served %d models after %d upstream calls", len(resp.Models), len(fx.httpCalls()))
	}

	second := fx.call(http.MethodPost, keysPath, nil, map[string]string{"api_key": testKeyB})
	if second.Status != http.StatusCreated {
		t.Fatalf("status %d: %s", second.Status, second.Body)
	}
	if stored := fx.storedFile(keyFileName); len(stored.Keys) != 2 || stored.Keys[1].APIKey != testKeyB || stored.Keys[1].Label != "" {
		t.Fatalf("stored = %+v", stored.Keys)
	}
}

func TestManagementAddKeyRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name string
		body any
		want string
	}{
		{"empty body", "", "body is empty"},
		{"not json", "{", "not valid JSON"},
		{"no key", map[string]string{"label": "x"}, "Paste an OpenCode key"},
		{"foreign key", map[string]string{"api_key": "sk-ant-api03-abcdef"}, "start with oc_sk_"},
		{"odd characters", map[string]string{"api_key": "oc_sk_test/abc"}, "characters"},
		{"key in label", map[string]string{"api_key": testKeyA, "label": "copy of " + testKeyB}, "label contains a key"},
		{"long label", map[string]string{"api_key": testKeyA, "label": strings.Repeat("x", maxLabelLength+1)}, "at most 64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newManagementFixture(t, map[string]string{})
			resp := fx.call(http.MethodPost, keysPath, nil, tc.body)
			if resp.Status != http.StatusBadRequest || !strings.Contains(resp.errorText(t), tc.want) {
				t.Fatalf("status %d: %s", resp.Status, resp.Body)
			}
			if len(fx.httpCalls()) != 0 || fx.saves() != 0 {
				t.Fatal("invalid input reached OpenCode or the auth directory")
			}
		})
	}
}

func TestManagementAddKeyRejectsDuplicates(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{
		"opencode-go.json": string(multiKeyFile(t)),
		"a-single.json":    `{"type":"opencode-go","api_key":"` + testKeyC + `","label":"Solo"}`,
	})
	for key, label := range map[string]string{testKeyA: `"Work"`, testKeyC: `"Solo"`, testKeyB: `"OpenCode Go oc_sk_…2222"`} {
		resp := fx.call(http.MethodPost, keysPath, nil, map[string]string{"api_key": key, "label": "again"})
		if resp.Status != http.StatusConflict || !strings.Contains(resp.errorText(t), "already added as "+label) {
			t.Fatalf("status %d: %s", resp.Status, resp.Body)
		}
	}
	if len(fx.httpCalls()) != 0 || fx.saves() != 0 {
		t.Fatal("a duplicate reached OpenCode or the auth directory")
	}
}

func TestManagementAddKeyCheckFailures(t *testing.T) {
	cases := []struct {
		name   string
		usage  func() (any, error)
		config func() (any, error)
		status int
		want   []string
	}{
		{
			name: "key rejected",
			usage: func() (any, error) {
				return upstreamJSON(401, `{"type":"error","error":{"type":"AuthError","message":"Invalid API key `+testKeyA+`"}}`)
			},
			status: http.StatusUnprocessableEntity,
			want:   []string{"OpenCode rejected the key", "AuthError", "oc_sk_[redacted]"},
		},
		{
			name:   "gateway down",
			usage:  func() (any, error) { return upstreamJSON(503, `upstream connect error`) },
			status: http.StatusBadGateway,
			want:   []string{"OpenCode is not answering", "503"},
		},
		{
			name:   "unreachable",
			usage:  func() (any, error) { return nil, errors.New("dial tcp: connection refused") },
			status: http.StatusBadGateway,
			want:   []string{"Could not reach OpenCode", "connection refused"},
		},
		{
			name:   "unreadable usage",
			usage:  func() (any, error) { return upstreamJSON(200, `<html>`) },
			status: http.StatusBadGateway,
			want:   []string{"unreadable usage report"},
		},
		{
			name:   "workspace without Go",
			config: func() (any, error) { return upstreamJSON(200, `{"providers":{"opencode":{"models":{}}}}`) },
			status: http.StatusUnprocessableEntity,
			want:   []string{"no OpenCode Go config", "Go subscription"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newManagementFixture(t, map[string]string{})
			serve := fx.upstream
			fx.upstream = func(req hostHTTPRequest) (any, error) {
				if req.URL == usageURL(defaultSettings()) && tc.usage != nil {
					return tc.usage()
				}
				if req.URL == configURL(defaultSettings()) && tc.config != nil {
					return tc.config()
				}
				return serve(req)
			}
			resp := fx.call(http.MethodPost, keysPath, nil, map[string]string{"api_key": testKeyA})
			if resp.Status != tc.status {
				t.Fatalf("status %d: %s", resp.Status, resp.Body)
			}
			message := resp.errorText(t)
			for _, want := range tc.want {
				if !strings.Contains(message, want) {
					t.Fatalf("message %q lacks %q", message, want)
				}
			}
			if fx.saves() != 0 || fx.files[keyFileName] != "" {
				t.Fatal("a key that failed its check was saved")
			}
		})
	}
}

func TestManagementUpdateKey(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{
		"opencode-go.json": string(multiKeyFile(t)),
		"a-single.json":    `{"type":"opencode-go","api_key":"` + testKeyC + `","label":"Solo"}`,
	})
	idA, idB, idC := keyIDFor(testKeyA), keyIDFor(testKeyB), keyIDFor(testKeyC)

	resp := fx.call(http.MethodPatch, keysPath, nil, map[string]any{"id": idA, "label": "  Personal\t"})
	view := decodeBodyAs[struct {
		Key *keyView `json:"key"`
	}](t, resp).Key
	if resp.Status != http.StatusOK || view == nil || view.Label != "Personal" || view.Disabled {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
	if fx.topLevel(keyFileName)["priority"] != "3" || fx.storedFile(keyFileName).Keys[0].Label != "Personal" {
		t.Fatalf("stored = %s", fx.files[keyFileName])
	}

	resp = fx.call(http.MethodPatch, keysPath, nil, map[string]any{"id": idB, "disabled": false})
	if resp.Status != http.StatusOK || fx.storedFile(keyFileName).Keys[1].Disabled {
		t.Fatalf("enable: status %d: %s", resp.Status, resp.Body)
	}

	before := fx.files[keyFileName]
	resp = fx.call(http.MethodPatch, keysPath, nil, map[string]any{"id": idC, "disabled": true, "label": ""})
	view = decodeBodyAs[struct {
		Key *keyView `json:"key"`
	}](t, resp).Key
	if resp.Status != http.StatusOK || view == nil || !view.Disabled || view.Label != "" || view.File != "a-single.json" {
		t.Fatalf("disable: status %d: %s", resp.Status, resp.Body)
	}
	if single := fx.storedFile("a-single.json"); len(single.Keys) != 1 || !single.Keys[0].Disabled || fx.files[keyFileName] != before {
		t.Fatalf("a-single.json = %s", fx.files["a-single.json"])
	}

	failures := []struct {
		body   any
		status int
		want   string
	}{
		{map[string]any{"id": idA}, http.StatusBadRequest, "Nothing to change"},
		{map[string]any{"id": "zzz", "label": "x"}, http.StatusBadRequest, "malformed key id"},
		{map[string]any{"id": keyIDFor("oc_sk_test_gone"), "label": "x"}, http.StatusNotFound, "no longer stored"},
		{map[string]any{"id": idA, "label": testKeyB}, http.StatusBadRequest, "label contains a key"},
	}
	saves := fx.saves()
	for _, tc := range failures {
		resp := fx.call(http.MethodPatch, keysPath, nil, tc.body)
		if resp.Status != tc.status || !strings.Contains(resp.errorText(t), tc.want) {
			t.Fatalf("%v: status %d: %s", tc.body, resp.Status, resp.Body)
		}
	}
	if fx.saves() != saves || len(fx.httpCalls()) != 0 {
		t.Fatal("a rejected update saved or called OpenCode")
	}
}

func TestManagementDeleteKey(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{"opencode-go.json": string(multiKeyFile(t))})
	idA, idB := keyIDFor(testKeyA), keyIDFor(testKeyB)

	resp := fx.call(http.MethodDelete, keysPath, url.Values{"id": {idA}}, nil)
	if resp.Status != http.StatusOK || string(resp.Body) != `{"deleted":"`+idA+`"}` {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
	if stored := fx.storedFile(keyFileName); len(stored.Keys) != 1 || stored.Keys[0].ID != idB {
		t.Fatalf("stored = %s", fx.files[keyFileName])
	}
	if resp := fx.call(http.MethodDelete, keysPath, url.Values{"id": {idA}}, nil); resp.Status != http.StatusNotFound {
		t.Fatalf("second delete: status %d: %s", resp.Status, resp.Body)
	}
	if resp := fx.call(http.MethodDelete, keysPath, nil, nil); resp.Status != http.StatusBadRequest {
		t.Fatalf("delete without id: status %d: %s", resp.Status, resp.Body)
	}

	// The host has no delete callback, so the last key leaves an empty file.
	if resp := fx.call(http.MethodDelete, keysPath, url.Values{"id": {idB}}, nil); resp.Status != http.StatusOK {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
	if stored := fx.storedFile(keyFileName); len(stored.Keys) != 0 || fx.topLevel(keyFileName)["keys"] != "[]" {
		t.Fatalf("stored = %s", fx.files[keyFileName])
	}
	if len(fx.httpCalls()) != 0 {
		t.Fatal("deleting called OpenCode")
	}
}

func TestManagementTestKey(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{"opencode-go.json": string(multiKeyFile(t))})
	idA, idB := keyIDFor(testKeyA), keyIDFor(testKeyB)
	serve := fx.upstream
	fx.upstream = func(req hostHTTPRequest) (any, error) {
		if req.Headers.Get("Authorization") == "Bearer "+testKeyB {
			return upstreamJSON(401, `{"type":"error","error":{"type":"AuthError","message":"Key `+testKeyB+` revoked"}}`)
		}
		return serve(req)
	}

	resp := fx.call(http.MethodPost, keysPath+"/test", nil, map[string]string{"id": idA})
	result := decodeBodyAs[struct {
		OK        bool      `json:"ok"`
		Usage     usageView `json:"usage"`
		Workspace string    `json:"workspace"`
		Models    int       `json:"models"`
	}](t, resp)
	if resp.Status != http.StatusOK || !result.OK || len(result.Usage.Windows) != 3 || result.Workspace != "wrk_TEST000000000000000000000" || result.Models != 7 {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
	if _, cached := catalogs.ownCatalog(idA); !cached {
		t.Fatal("a passing test did not cache the catalog")
	}

	resp = fx.call(http.MethodPost, keysPath+"/test", nil, map[string]string{"id": idB})
	failed := decodeBodyAs[struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}](t, resp)
	if resp.Status != http.StatusOK || failed.OK || !strings.Contains(failed.Error, "OpenCode rejected the key") || !strings.Contains(failed.Error, "oc_sk_[redacted] revoked") {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}

	if resp := fx.call(http.MethodPost, keysPath+"/test", nil, map[string]string{"id": keyIDFor("oc_sk_test_gone")}); resp.Status != http.StatusNotFound {
		t.Fatalf("unknown key: status %d: %s", resp.Status, resp.Body)
	}
	if fx.saves() != 0 {
		t.Fatal("testing a key rewrote the auth file")
	}
}

func TestManagementKeyUsage(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{"opencode-go.json": string(multiKeyFile(t))})
	idA, idB := keyIDFor(testKeyA), keyIDFor(testKeyB)
	serve := fx.upstream
	fx.upstream = func(req hostHTTPRequest) (any, error) {
		if req.Headers.Get("Authorization") == "Bearer "+testKeyB {
			return upstreamJSON(200, `{"usage":{"rolling":{"status":"limited","percent":100,"resetsAt":"2026-10-13T04:56:11.000Z"},"weekly":{"status":"ok","percent":60}}}`)
		}
		return serve(req)
	}

	resp := fx.call(http.MethodGet, keysPath+"/usage", url.Values{"id": {idA}}, nil)
	usage := decodeBodyAs[struct {
		Usage usageView `json:"usage"`
	}](t, resp).Usage
	if resp.Status != http.StatusOK || len(usage.Windows) != 3 || usage.Windows[0].Name != "rolling" || usage.Windows[0].Status != "ok" || usage.Windows[0].Exhausted {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
	calls := fx.httpCalls()
	if len(calls) != 1 || calls[0].URL != usageURL(defaultSettings()) || calls[0].HostCallbackID != "cb-mgmt" {
		t.Fatalf("upstream calls = %+v", calls)
	}

	resp = fx.call(http.MethodGet, keysPath+"/usage", url.Values{"id": {idB}}, nil)
	usage = decodeBodyAs[struct {
		Usage usageView `json:"usage"`
	}](t, resp).Usage
	if len(usage.Windows) != 2 || !usage.Windows[0].Exhausted || usage.Windows[0].Status != "limited" || usage.Windows[1].ResetsAt != "" || usage.Windows[1].Label != "Weekly" {
		t.Fatalf("usage = %s", resp.Body)
	}

	fx.upstream = func(hostHTTPRequest) (any, error) {
		return upstreamJSON(401, `{"error":{"type":"AuthError","message":"nope"}}`)
	}
	if resp := fx.call(http.MethodGet, keysPath+"/usage", url.Values{"id": {idA}}, nil); resp.Status != http.StatusUnprocessableEntity || !strings.Contains(resp.errorText(t), "rejected") {
		t.Fatalf("rejected key: status %d: %s", resp.Status, resp.Body)
	}
	if resp := fx.call(http.MethodGet, keysPath+"/usage", nil, nil); resp.Status != http.StatusBadRequest {
		t.Fatalf("no id: status %d: %s", resp.Status, resp.Body)
	}
}

func TestManagementLogins(t *testing.T) {
	loginName := "opencode-go-oauth-" + testOrgID + ".json"
	fx := newManagementFixture(t, map[string]string{
		"opencode-go.json": `{"type":"opencode-go","keys":[{"label":"Work","api_key":"` + testKeyA + `"}]}`,
		loginName:          string(loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, map[string]any{"priority": 2})),
	})
	store := useLoginStore(t, time.Now())

	resp := fx.call(http.MethodGet, loginsPath, nil, nil)
	listed := decodeBodyAs[struct {
		Logins []loginView `json:"logins"`
	}](t, resp).Logins
	if resp.Status != http.StatusOK || len(listed) != 1 || strings.Contains(string(resp.Body), "redacted") {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
	got := listed[0]
	if got.ID != loginName || got.Workspace != "Acme" || got.Email != "dev@example.test" || got.OrgID != testOrgID ||
		got.ExpiresAt != "2099-01-31T00:00:00Z" || got.Expired || got.Disabled || got.Runtime == nil || got.Runtime.State != "active" {
		t.Fatalf("login = %+v", got)
	}
	keys := decodeBodyAs[struct {
		Keys []keyView `json:"keys"`
	}](t, fx.call(http.MethodGet, keysPath, nil, nil)).Keys
	if len(keys) != 1 || keys[0].Label != "Work" {
		t.Fatalf("keys = %+v", keys)
	}

	// The page writes the newest tokens this process holds, never the
	// spent ones still on disk.
	previous, _ := parseLoginFile([]byte(fx.files[loginName]))
	next := previous
	next.AccessToken, next.RefreshToken = testAccessToken2, testRefreshToken2
	store.rotated(previous, next)
	resp = fx.call(http.MethodPatch, loginsPath, nil, map[string]any{"id": loginName, "disabled": true})
	updated := decodeBodyAs[struct {
		Login loginView `json:"login"`
	}](t, resp).Login
	if resp.Status != http.StatusOK || !updated.Disabled {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
	saved, errParse := parseLoginFile([]byte(fx.files[loginName]))
	if errParse != nil || !saved.Disabled || saved.AccessToken != testAccessToken2 || saved.RefreshToken != testRefreshToken2 || string(saved.extra["priority"]) != "2" {
		t.Fatalf("saved = %+v, %v", saved, errParse)
	}

	fx.upstream = func(req hostHTTPRequest) (any, error) {
		if req.URL != usageURL(defaultSettings()) || req.Headers.Get("Authorization") != "Bearer "+testAccessToken2 {
			t.Errorf("usage request = %+v", req)
		}
		return upstreamJSON(200, string(readFixture(t, "usage.json")))
	}
	resp = fx.call(http.MethodGet, loginsPath+"/usage", url.Values{"id": {loginName}}, nil)
	if usage := decodeBodyAs[struct {
		Usage usageView `json:"usage"`
	}](t, resp).Usage; resp.Status != http.StatusOK || len(usage.Windows) != 3 {
		t.Fatalf("usage: status %d: %s", resp.Status, resp.Body)
	}

	for _, tc := range []struct {
		method string
		query  url.Values
		body   any
		status int
	}{
		{http.MethodPatch, nil, map[string]any{"id": loginName}, http.StatusBadRequest},
		{http.MethodPatch, nil, map[string]any{"id": "opencode-go.json", "disabled": true}, http.StatusNotFound},
		{http.MethodGet, url.Values{"id": {"nope.json"}}, nil, http.StatusNotFound},
		{http.MethodGet, nil, nil, http.StatusBadRequest},
	} {
		path := loginsPath
		if tc.method == http.MethodGet {
			path += "/usage"
		}
		if resp := fx.call(tc.method, path, tc.query, tc.body); resp.Status != tc.status {
			t.Fatalf("%s %v %v: status %d: %s", tc.method, tc.query, tc.body, resp.Status, resp.Body)
		}
	}
}

func TestManagementExpiredLoginUsage(t *testing.T) {
	loginName := "opencode-go-oauth-" + testOrgID + ".json"
	fx := newManagementFixture(t, map[string]string{
		loginName: string(loginFileJSON(t, testAccessToken, testRefreshToken, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), nil)),
	})
	useLoginStore(t, time.Now())
	listed := decodeBodyAs[struct {
		Logins []loginView `json:"logins"`
	}](t, fx.call(http.MethodGet, loginsPath, nil, nil)).Logins
	if len(listed) != 1 || !listed[0].Expired {
		t.Fatalf("logins = %+v", listed)
	}
	resp := fx.call(http.MethodGet, loginsPath+"/usage", url.Values{"id": {loginName}}, nil)
	if resp.Status != http.StatusUnprocessableEntity || !strings.Contains(resp.errorText(t), "log in again") || len(fx.httpCalls()) != 0 {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
}

func TestManagementUnknownRoutes(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{})
	for _, call := range [][2]string{
		{http.MethodGet, managementBasePath + "/nope"},
		{http.MethodPut, keysPath},
		{http.MethodPost, resourcePagePath},
	} {
		if resp := fx.call(call[0], call[1], nil, nil); resp.Status != http.StatusNotFound {
			t.Fatalf("%s %s: status %d: %s", call[0], call[1], resp.Status, resp.Body)
		}
	}
}
