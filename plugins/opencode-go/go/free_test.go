package main

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// useFreeCatalog gives the test a free catalog store holding the fixture.
func useFreeCatalog(t *testing.T) *freeCatalogStore {
	t.Helper()
	previous := freeCatalogs
	store := newFreeCatalogStore()
	cat, errParse := parseFreeCatalog(readFixture(t, "models_dev.json"), nil)
	if errParse != nil {
		t.Fatal(errParse)
	}
	store.entry = &catalogEntry{catalog: cat, fetchedAt: time.Now()}
	freeCatalogs = store
	t.Cleanup(func() { freeCatalogs = previous })
	return store
}

func freeIDs(cat catalog) string {
	var ids []string
	for _, model := range cat.Free {
		ids = append(ids, model.UpstreamID+":"+string(model.Protocol))
	}
	return strings.Join(ids, ",")
}

// Free models are the models.dev OpenCode models priced at zero that the
// gateway serves now, in a protocol the plugin speaks.
func TestParseFreeCatalog(t *testing.T) {
	live := map[string]bool{}
	for _, id := range gjson.GetBytes(readFixture(t, "live_models.json"), "data.#.id").Array() {
		live[id.String()] = true
	}
	cat, errParse := parseFreeCatalog(readFixture(t, "models_dev.json"), live)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if got := freeIDs(cat); got != "free-chat:openai,free-messages:claude,free-responses:openai-response" {
		t.Fatalf("free models = %s", got)
	}
	chat := cat.Free[0]
	if chat.Family != familyFree || !chat.Free || chat.Name != "Free Chat" || chat.ContextLength != 200000 || chat.OutputLimit != 32000 ||
		chat.BaseURL != "https://opencode.ai/inference/openai/v1" || strings.Join(chat.InputModalities, ",") != "text,image" {
		t.Fatalf("chat = %+v", chat)
	}
	if route := cat.Free[1].route(defaultSettings()); route.Endpoint != "https://opencode.ai/inference/anthropic/v1/messages" || route.ID != "opencode-free/free-messages" {
		t.Fatalf("messages route = %+v", route)
	}
	// Without the live list, models.dev's own list stands.
	all, _ := parseFreeCatalog(readFixture(t, "models_dev.json"), nil)
	if !strings.Contains(freeIDs(all), "free-not-live") {
		t.Fatalf("without the live list = %s", freeIDs(all))
	}
	if _, errEmpty := parseFreeCatalog([]byte(`{"opencode":{"models":{"paid":{"cost":{"input":1}}}}}`), nil); errEmpty == nil {
		t.Fatal("a catalog without free models was accepted")
	}

	snapshot, errSnapshot := parseFreeCatalog(freeSnapshotJSON, nil)
	if errSnapshot != nil || len(snapshot.Free) < 5 {
		t.Fatalf("snapshot = %d models, %v", len(snapshot.Free), errSnapshot)
	}
}

func TestFreeAuthParse(t *testing.T) {
	cfg := defaultSettings()
	cfg.FreeBareModels = []string{"free-chat"}
	useSettings(t, cfg)
	raw := `{"type":"opencode-go","free":true,"priority":-2,"catalog_refreshed_at":"2026-10-16T00:00:00Z"}`
	resp := parseAuths(pluginapi.AuthParseRequest{Path: "/auths/" + freeFileName, RawJSON: []byte(raw)})
	auth := resp.Auth
	if !resp.Handled || len(resp.Auths) != 0 || auth.ID != freeFileName || auth.Label != "OpenCode Free" || auth.Disabled || string(auth.StorageJSON) != raw {
		t.Fatalf("resp = %+v", resp)
	}
	if auth.Attributes["family"] != "free" || auth.Attributes[catalogRefreshedField] != "2026-10-16T00:00:00Z" ||
		auth.Attributes["model_aliases"] != `[{"name":"opencode-free/free-chat","alias":"free-chat","fork":true}]` || len(auth.Metadata) != 0 {
		t.Fatalf("attributes = %v", auth.Attributes)
	}
	// CPA applies the file's own priority, so the plugin adds none.
	if _, ok := auth.Attributes["priority"]; ok {
		t.Fatalf("attributes = %v", auth.Attributes)
	}
	if disabled := parsedAuths(freeFileName, `{"type":"opencode-go","free":true,"disabled":true}`)[0]; !disabled.Disabled {
		t.Fatalf("disabled = %+v", disabled)
	}
	cred, errCred := resolveCredential([]byte(raw), auth.Attributes, nil)
	if errCred != nil || cred != freeCredential || cred.Bearer != "public" {
		t.Fatalf("credential = %+v, %v", cred, errCred)
	}
}

func TestFreeCatalogStoreFallbacks(t *testing.T) {
	failing := false
	doer := &fakeDoer{respond: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if failing {
			return pluginapi.HTTPResponse{}, errors.New("connection refused")
		}
		switch req.URL {
		case modelsDevURL:
			return jsonResponse(200, readFixture(t, "models_dev.json")), nil
		case "https://opencode.ai/inference/v1/models":
			if req.Headers.Get("Authorization") != "" {
				t.Errorf("the live list was asked with credentials: %v", req.Headers)
			}
			return jsonResponse(200, readFixture(t, "live_models.json")), nil
		}
		t.Errorf("unexpected request %s", req.URL)
		return jsonResponse(404, nil), nil
	}}
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	store := newFreeCatalogStore()
	store.now = func() time.Time { return now }
	cfg := defaultSettings()

	cold, source := store.cached()
	if source != catalogSnapshot || len(cold.Free) < 5 {
		t.Fatalf("cold = %s with %d models", source, len(cold.Free))
	}
	cat, source, errFetch := store.get(context.Background(), doer, cfg)
	if source != catalogFresh || errFetch != nil || len(cat.Free) != 3 || len(doer.requests) != 2 {
		t.Fatalf("first = %s %v, %d models after %d requests", source, errFetch, len(cat.Free), len(doer.requests))
	}
	if _, source, _ := store.get(context.Background(), doer, cfg); source != catalogCached || len(doer.requests) != 2 {
		t.Fatalf("within ttl = %s", source)
	}
	now = now.Add(catalogTTL)
	failing = true
	if stale, source, errFetch := store.get(context.Background(), doer, cfg); source != catalogStale || errFetch == nil || len(stale.Free) != 3 {
		t.Fatalf("expired + failing = %s %v", source, errFetch)
	}
}

func TestModelsForAuthFree(t *testing.T) {
	cfg := defaultSettings()
	cfg.FreeDisabledModels = []string{"free-messages"}
	useSettings(t, cfg)
	useCatalogStore(t)
	useFreeCatalog(t)
	auth := parsedAuths(freeFileName, `{"type":"opencode-go","free":true}`)[0]
	raw, errHandle := handleMethod(pluginabi.MethodModelForAuth, mustJSON(t, rpcAuthModelRequest{AuthModelRequest: pluginapi.AuthModelRequest{
		AuthID: auth.ID, StorageJSON: auth.StorageJSON, Attributes: auth.Attributes,
	}}))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	var ids []string
	for _, model := range decodeResult[pluginapi.ModelResponse](t, raw).Models {
		ids = append(ids, model.ID)
	}
	if strings.Join(ids, ",") != "opencode-free/free-chat,opencode-free/free-not-live,opencode-free/free-responses" {
		t.Fatalf("registered %v", ids)
	}
	if reg, ok := catalogs.registeredFor(freeCredentialID, familyFree); !ok || !slices.Equal(reg.ids, ids) {
		t.Fatalf("recorded %+v", reg)
	}
}

// OpenCode Free sends no credential of the user's: the gateway gets the key
// OpenCode's own client sends without an account, and the cloak its gate
// needs.
func TestExecutorServesFreeWithoutKey(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	useFreeCatalog(t)
	stream := readFixture(t, "chat_stream.txt")
	gateway := installFakeGateway(t, func(hostHTTPRequest) gatewayReply {
		return gatewayReply{Status: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, Body: stream}
	})
	raw, _ := executorExecute(testCall{Source: "openai", Model: "opencode-free/free-chat", Payload: clientPayload("openai", "opencode-free/free-chat", false)}.raw(t))
	resp := decodeResult[pluginapi.ExecutorResponse](t, raw)
	if got := parseClientResponse(t, "openai", resp.Payload); got.Text != "OK" {
		t.Fatalf("response = %+v", got)
	}
	sent := gateway.inferenceRequests()
	if len(sent) != 1 {
		t.Fatalf("requests = %d", len(sent))
	}
	req := sent[0]
	if req.URL != "https://opencode.ai/inference/openai/v1/chat/completions" || req.Headers.Get("Authorization") != "Bearer public" ||
		req.Headers.Get("User-Agent") != cloakUserAgent || !validOpenCodeSession(req.Headers.Get("x-opencode-session")) {
		t.Fatalf("request = %s %v", req.URL, req.Headers)
	}
	if gjson.GetBytes(req.Body, "model").String() != "free-chat" || gjson.GetBytes(req.Body, "tools.0.function.name").String() != cloakToolName {
		t.Fatalf("body = %s", req.Body)
	}

	// A disabled free model and a Go model are not the free auth's.
	cfg := defaultSettings()
	cfg.FreeDisabledModels = []string{"free-chat"}
	useSettings(t, cfg)
	for _, model := range []string{"opencode-free/free-chat", "opencode-go/kimi-k3"} {
		raw, _ := executorExecute(testCall{Source: "openai", Model: model, Family: familyFree, Payload: clientPayload("openai", model, false)}.raw(t))
		if env := decodeEnvelope(t, raw); env.OK || env.Error.HTTPStatus != http.StatusNotFound {
			t.Fatalf("%s = %s", model, raw)
		}
	}
	if route, ok := lookupModel("free-responses", freeCredentialID, familyFree); !ok || route.Protocol != sdktranslator.FormatOpenAIResponse {
		t.Fatalf("bare lookup = %+v %v", route, ok)
	}
}

func TestQuotaFetchFree(t *testing.T) {
	auth := parsedAuths(freeFileName, `{"type":"opencode-go","free":true}`)[0]
	raw, _ := handleMethod(pluginabi.MethodQuotaFetch, mustJSON(t, rpcQuotaFetchRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{
		AuthID: auth.ID, Provider: "opencode-go", StorageJSON: auth.StorageJSON, Attributes: auth.Attributes,
	}}))
	if resp := decodeResult[pluginapi.QuotaFetchResponse](t, raw); resp.Subscription == nil || resp.Subscription.Plan != "OpenCode Free" || len(resp.Groups) != 0 {
		t.Fatalf("quota = %+v", resp)
	}
}

// The page turns OpenCode Free on by creating its file, and off by
// disabling it; its routing is the file's own priority and weight.
func TestManagementFree(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{
		testLoginName: string(loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, nil)),
	})
	useLoginStore(t, time.Now())
	useFreeCatalog(t)

	view := decodeBodyAs[freeView](t, fx.call(http.MethodGet, freePath, nil, nil))
	if view.File != "" || len(view.Models) != 0 {
		t.Fatalf("before = %+v", view)
	}
	if resp := fx.call(http.MethodPut, freePath, nil, map[string]any{}); resp.Status != http.StatusBadRequest {
		t.Fatalf("empty body: %d", resp.Status)
	}
	view = decodeBodyAs[freeView](t, fx.call(http.MethodPut, freePath, nil, map[string]any{"enabled": true}))
	if view.File != freeFileName || view.Disabled || len(view.Models) != 4 || view.Models[0].ID != "opencode-free/free-chat" || view.Models[0].Family != "free" || !view.Models[0].Free {
		t.Fatalf("on = %+v", view)
	}
	if top := fx.topLevel(freeFileName); top["type"] != `"opencode-go"` || top["free"] != "true" {
		t.Fatalf("file = %s", fx.files[freeFileName])
	}

	resp := fx.call(http.MethodPut, routingPath, nil, map[string]any{"go": map[string]any{"priority": -1}, "free": map[string]any{"priority": -3, "weight": 2}})
	routing := decodeBodyAs[routingView](t, resp)
	if resp.Status != http.StatusOK || routing.Free == nil || *routing.Free.Priority != -3 || *routing.Free.Weight != 2 || *routing.Go.Priority != -1 {
		t.Fatalf("routing = %s", resp.Body)
	}
	if top := fx.topLevel(freeFileName); top["priority"] != "-3" || top["weight"] != "2" || top["go_priority"] != "" {
		t.Fatalf("free file = %s", fx.files[freeFileName])
	}
	if top := fx.topLevel(testLoginName); top["go_priority"] != "-1" || top["free_priority"] != "" || top["priority"] != "" {
		t.Fatalf("login = %s", fx.files[testLoginName])
	}

	view = decodeBodyAs[freeView](t, fx.call(http.MethodPut, freePath, nil, map[string]any{"enabled": false}))
	if view.File != freeFileName || !view.Disabled || len(view.Models) != 0 || fx.topLevel(freeFileName)["priority"] != "-3" {
		t.Fatalf("off = %+v, file %s", view, fx.files[freeFileName])
	}
}

// A model refresh reads the free catalog while OpenCode Free is on, and
// rewrites its file when CPA's list is out of date.
func TestRefreshCatalogsCoversFree(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{freeFileName: `{"type":"opencode-go","free":true}`})
	useFreeCatalog(t)
	fx.upstream = func(req hostHTTPRequest) (any, error) {
		switch req.URL {
		case modelsDevURL:
			return upstreamJSON(200, string(readFixture(t, "models_dev.json")))
		case liveModelsURL(defaultSettings()):
			return upstreamJSON(200, string(readFixture(t, "live_models.json")))
		}
		t.Errorf("unexpected upstream request %s", req.URL)
		return upstreamJSON(404, `{}`)
	}
	asked := fx.registerOnSave()
	report, errRefresh := refreshCatalogs(context.Background(), newHost(""), refreshOptions{wait: time.Second})
	if errRefresh != nil || strings.Join(report.Files, ",") != freeFileName || !report.Registered || strings.Join(*asked, ",") != freeFileName {
		t.Fatalf("report = %+v, %v, asked %v", report, errRefresh, *asked)
	}
	if reg, _ := catalogs.registeredFor(freeCredentialID, familyFree); len(reg.ids) != 3 {
		t.Fatalf("registered %v", reg.ids)
	}
	*asked = nil
	if report, _ := refreshCatalogs(context.Background(), newHost(""), refreshOptions{}); len(report.Files) != 0 || len(*asked) != 0 {
		t.Fatalf("unchanged refresh = %+v", report)
	}
	fx.files[freeFileName] = `{"type":"opencode-go","free":true,"disabled":true}`
	if report, _ := refreshCatalogs(context.Background(), newHost(""), refreshOptions{force: true}); len(report.Files) != 0 {
		t.Fatalf("disabled refresh = %+v", report)
	}
}

// The page turns OpenCode Go and Zen on and off per login and key; each
// family it turns on is an auth of its own.
func TestManagementCredentialFamilies(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{
		testLoginName:      string(loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, nil)),
		"opencode-go.json": `{"type":"opencode-go","keys":[{"label":"Work","api_key":"` + testKeyA + `"}]}`,
	})
	useLoginStore(t, time.Now())
	ids := func(name string) string {
		var out []string
		for _, auth := range parsedAuths(name, fx.files[name]) {
			if !auth.Disabled {
				out = append(out, auth.ID)
			}
		}
		return strings.Join(out, ",")
	}
	keyA := authIDFor(keyFileName, keyIDFor(testKeyA))

	resp := fx.call(http.MethodPatch, keysPath, nil, map[string]any{"id": keyIDFor(testKeyA), "zen": true})
	key := decodeBodyAs[struct {
		Key *keyView `json:"key"`
	}](t, resp).Key
	if resp.Status != http.StatusOK || key == nil || !key.Go || !key.Zen || ids(keyFileName) != keyA+","+keyA+"#zen" {
		t.Fatalf("zen on: %d %s, auths %s", resp.Status, resp.Body, ids(keyFileName))
	}
	fx.call(http.MethodPatch, keysPath, nil, map[string]any{"id": keyIDFor(testKeyA), "go": false})
	if ids(keyFileName) != keyA+"#zen" {
		t.Fatalf("go off: auths %s, file %s", ids(keyFileName), fx.files[keyFileName])
	}

	resp = fx.call(http.MethodPatch, loginsPath, nil, map[string]any{"id": testLoginName, "go": false, "zen": true})
	login := decodeBodyAs[struct {
		Login *loginView `json:"login"`
	}](t, resp).Login
	if resp.Status != http.StatusOK || login == nil || login.Go || !login.Zen || ids(testLoginName) != testLoginName+"#zen" {
		t.Fatalf("login: %d %s, auths %s", resp.Status, resp.Body, ids(testLoginName))
	}
	fx.call(http.MethodPatch, loginsPath, nil, map[string]any{"id": testLoginName, "zen": false})
	if ids(testLoginName) != "" {
		t.Fatalf("both off: auths %s", ids(testLoginName))
	}
	// The login stays listed, so it can be turned on again.
	logins := decodeBodyAs[struct {
		Logins []loginView `json:"logins"`
	}](t, fx.call(http.MethodGet, loginsPath, nil, nil)).Logins
	if len(logins) != 1 || logins[0].Go || logins[0].Zen {
		t.Fatalf("logins = %+v", logins)
	}
	if saved, _ := parseLoginFile([]byte(fx.files[testLoginName])); saved.AccessToken != testAccessToken {
		t.Fatalf("tokens changed: %+v", saved)
	}
}
