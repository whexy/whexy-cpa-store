package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"html"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// catalogWithout is the config fixture with the named models removed and
// the added models (Chat Completions, no cost tiers) appended.
func catalogWithout(t *testing.T, removed []string, added ...string) []byte {
	t.Helper()
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(readFixture(t, "config.json"), &doc); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	models := doc["providers"].(map[string]any)[providerKey].(map[string]any)["models"].(map[string]any)
	for _, id := range removed {
		delete(models, id)
	}
	for _, id := range added {
		models[id] = map[string]any{"name": id, "limit": map[string]any{"context": 1000, "output": 100}}
	}
	return mustJSON(t, doc)
}

// serveCatalogs answers the catalog per bearer and usage for everyone.
func (fx *managementFixture) serveCatalogs(byBearer map[string][]byte) {
	fx.upstream = func(req hostHTTPRequest) (any, error) {
		switch req.URL {
		case configURL(defaultSettings()):
			body, ok := byBearer[strings.TrimPrefix(req.Headers.Get("Authorization"), "Bearer ")]
			if !ok {
				return upstreamJSON(401, `{"error":{"type":"AuthError","message":"bad key"}}`)
			}
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body}, nil
		case usageURL(defaultSettings()):
			return upstreamJSON(200, string(readFixture(fx.t, "usage.json")))
		}
		fx.t.Errorf("unexpected upstream request %s", req.URL)
		return upstreamJSON(404, `{}`)
	}
}

func parsedAuths(name, content string) []pluginapi.AuthData {
	resp := parseAuths(pluginapi.AuthParseRequest{Path: "/auths/" + name, RawJSON: []byte(content)})
	if len(resp.Auths) > 0 {
		return resp.Auths
	}
	return []pluginapi.AuthData{resp.Auth}
}

func modelsForAuth(t *testing.T, auth pluginapi.AuthData) {
	t.Helper()
	raw := mustJSON(t, rpcAuthModelRequest{AuthModelRequest: pluginapi.AuthModelRequest{
		AuthID: auth.ID, StorageJSON: auth.StorageJSON, Attributes: auth.Attributes,
	}})
	if _, errModels := handleModelsForAuth(raw); errModels != nil {
		t.Fatal(errModels)
	}
}

// registerOnSave makes host.auth.save behave like CPA: every auth whose
// parsed form changed gets a model.for_auth call, as CPA's file watcher
// makes. It returns the auth IDs asked for, in order.
func (fx *managementFixture) registerOnSave() *[]string {
	fx.t.Helper()
	asked := &[]string{}
	previous := map[string]pluginapi.AuthData{}
	for name, content := range fx.files {
		for _, auth := range parsedAuths(name, content) {
			previous[auth.ID] = auth
		}
	}
	fx.fake.mu.Lock()
	save := fx.fake.handlers[pluginabi.MethodHostAuthSave]
	fx.fake.mu.Unlock()
	fx.fake.handle(pluginabi.MethodHostAuthSave, func(payload json.RawMessage) (any, error) {
		result, errSave := save(payload)
		if errSave != nil {
			return nil, errSave
		}
		var req pluginapi.HostAuthSaveRequest
		_ = json.Unmarshal(payload, &req)
		for _, auth := range parsedAuths(req.Name, fx.files[req.Name]) {
			if old, ok := previous[auth.ID]; ok && reflect.DeepEqual(old, auth) {
				continue
			}
			previous[auth.ID] = auth
			if auth.Disabled {
				continue
			}
			*asked = append(*asked, auth.ID)
			modelsForAuth(fx.t, auth)
		}
		return result, nil
	})
	return asked
}

// registerAll runs model.for_auth for every enabled auth, as CPA does when
// it loads the plugin.
func (fx *managementFixture) registerAll() {
	fx.t.Helper()
	for name, content := range fx.files {
		for _, auth := range parsedAuths(name, content) {
			if !auth.Disabled {
				modelsForAuth(fx.t, auth)
			}
		}
	}
}

const testLoginName = "opencode-go-oauth-" + testOrgID + ".json"

func TestParseCarriesCatalogMark(t *testing.T) {
	const mark = "2026-10-12T20:00:00.5Z"
	multi := `{"type":"opencode-go","catalog_refreshed_at":"` + mark + `","keys":[{"api_key":"` + testKeyA + `"},{"api_key":"` + testKeyB + `"}]}`
	single := `{"type":"opencode-go","catalog_refreshed_at":"` + mark + `","api_key":"` + testKeyC + `"}`
	login := string(loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, map[string]any{catalogRefreshedField: mark}))
	for name, raw := range map[string]string{"multi": multi, "single": single, "login": login} {
		auths := parsedAuths(name+".json", raw)
		later := parsedAuths(name+".json", strings.ReplaceAll(raw, "20:00:00.5Z", "21:00:00.5Z"))
		for index, auth := range auths {
			if auth.Attributes[catalogRefreshedField] != mark {
				t.Fatalf("%s auth %d attributes = %v", name, index, auth.Attributes)
			}
			// CPA re-registers an auth's models only when its parsed form
			// changes, so a new mark must change every auth, virtual ones too.
			if reflect.DeepEqual(auth, later[index]) {
				t.Fatalf("%s auth %d did not change with the mark", name, index)
			}
		}
	}
	unmarked := parsedAuths("x.json", `{"type":"opencode-go","api_key":"`+testKeyC+`"}`)[0]
	if _, ok := unmarked.Attributes[catalogRefreshedField]; ok {
		t.Fatalf("unmarked attributes = %v", unmarked.Attributes)
	}
}

func TestManagementListModels(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{
		testLoginName:      string(loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, nil)),
		"opencode-go.json": `{"type":"opencode-go","keys":[{"label":"Off","api_key":"` + testKeyB + `","disabled":true},{"label":"Work","api_key":"` + testKeyA + `"}]}`,
	})
	useLoginStore(t, time.Now())
	// The login cannot read the catalog, the disabled key is skipped, and
	// the enabled key answers.
	fx.serveCatalogs(map[string][]byte{testKeyA: readFixture(t, "config.json"), testKeyB: readFixture(t, "config.json")})

	resp := fx.call(http.MethodGet, modelsPath, nil, nil)
	view := decodeBodyAs[modelsView](t, resp)
	if resp.Status != http.StatusOK || view.Credential != "Work · oc_sk_…1111" || view.Error != "" || view.FetchedAt == "" || len(view.Models) != 11 {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
	byID := map[string]modelView{}
	for _, model := range view.Models {
		byID[model.ID] = model
	}
	if got := byID["opencode-go/claude-haiku-5-5"]; got.UpstreamID != "claude-haiku-5-5" || got.Name != "Claude Haiku 5.5" || got.Protocol != "Messages" || got.Context == 0 || got.Family != "go" {
		t.Fatalf("claude = %+v", got)
	}
	if byID["opencode-go/gpt-6-luna"].Protocol != "Responses" || byID["opencode-go/kimi-k3"].Protocol != "Chat Completions" || !byID["opencode-go/longcat-2.5-preview-free"].Free {
		t.Fatalf("models = %+v", view.Models)
	}
	// The workspace's enabled Zen models are listed under their own prefix;
	// Gemini is left out because the plugin cannot serve its endpoint.
	if got := byID["opencode/gpt-6-luna"]; got.Family != "zen" || got.UpstreamID != "gpt-6-luna" || got.Protocol != "Responses" {
		t.Fatalf("zen gpt = %+v", got)
	}
	if byID["opencode/kimi-k3"].Protocol != "Chat Completions" || !byID["opencode/cat-jam-free"].Free || byID["opencode/glm-5.3"].ID != "" {
		t.Fatalf("zen models = %+v", view.Models)
	}
	for _, model := range view.Models {
		if model.UpstreamID == "gemini-3.5-flash" || model.ID == "opencode/gemini-3.5-flash" {
			t.Fatalf("an unsupported Zen protocol was listed: %+v", model)
		}
	}
	for _, req := range fx.httpCalls() {
		if strings.Contains(req.Headers.Get("Authorization"), testKeyB) {
			t.Fatal("the disabled key was used")
		}
	}

	// CPA registered an older list for the key: it lacks two models and
	// still has one the catalog dropped.
	older, errParse := parseCatalog(catalogWithout(t, []string{"kimi-k3", "qwen3.8-max"}, "retired-two"))
	if errParse != nil {
		t.Fatal(errParse)
	}
	catalogs.markRegistered(keyIDFor(testKeyA), familyGo, defaultSettings(), older)
	view = decodeBodyAs[modelsView](t, fx.call(http.MethodGet, modelsPath, nil, nil))
	if strings.Join(view.Unregistered, ",") != "opencode-go/kimi-k3,opencode-go/qwen3.8-max" || strings.Join(view.Unlisted, ",") != "opencode-go/retired-two" {
		t.Fatalf("diff = %v / %v", view.Unregistered, view.Unlisted)
	}

	// With every credential failing, the last list read is shown with the
	// reason.
	fx.serveCatalogs(nil)
	catalogs.byKey = map[string]catalogEntry{}
	view = decodeBodyAs[modelsView](t, fx.call(http.MethodGet, modelsPath, nil, nil))
	if !strings.Contains(view.Error, "401") || len(view.Models) != 11 {
		t.Fatalf("failing view = %+v", view)
	}

	empty := newManagementFixture(t, map[string]string{})
	if view := decodeBodyAs[modelsView](t, empty.call(http.MethodGet, modelsPath, nil, nil)); len(view.Models) != 0 || view.Credential != "" {
		t.Fatalf("empty view = %+v", view)
	}
}

// With the prefix disabled the page gets bare IDs and the mode; disabled
// models stay listed but are not expected among the registered ones.
func TestManagementListModelsWithPrefixDisabled(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{"opencode-go.json": `{"type":"opencode-go","api_key":"` + testKeyA + `"}`})
	cfg := settings{BaseURL: defaultBaseURL, ModelPrefix: defaultModelPrefix, DisablePrefix: true, DisabledModels: []string{"kimi-k3"}}
	useSettings(t, cfg)
	fx.registerAll()
	view := decodeBodyAs[modelsView](t, fx.call(http.MethodGet, modelsPath, nil, nil))
	if !view.DisablePrefix || len(view.Models) != 11 || len(view.Unregistered)+len(view.Unlisted) != 0 {
		t.Fatalf("view = %+v", view)
	}
	for _, model := range view.Models {
		if model.Family == "zen" {
			if model.ID != "opencode/"+model.UpstreamID {
				t.Fatalf("zen model %+v lost its prefix", model)
			}
			continue
		}
		if model.ID != model.UpstreamID {
			t.Fatalf("model %+v is not bare", model)
		}
	}
	reg, _ := catalogs.registeredFor(keyIDFor(testKeyA), familyGo)
	if slices.Contains(reg.ids, "kimi-k3") || !slices.Contains(reg.ids, "deepseek-v4-flash") {
		t.Fatalf("registered = %v", reg.ids)
	}
}

func TestManagementRefreshModels(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{
		testLoginName:      string(loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, map[string]any{"priority": 4, "zen": true})),
		"opencode-go.json": `{"type":"opencode-go","weight":2,"keys":[{"label":"Work","api_key":"` + testKeyA + `"},{"label":"Home","api_key":"` + testKeyB + `"}]}`,
	})
	store := useLoginStore(t, time.Now())
	original := readFixture(t, "config.json")
	fx.serveCatalogs(map[string][]byte{testAccessToken: original, testKeyA: original, testKeyB: original})
	fx.registerAll()
	asked := fx.registerOnSave()

	// The login's tokens rotated in this process since the file was read;
	// the rewrite must not put the spent ones back.
	previous, _ := parseLoginFile([]byte(fx.files[testLoginName]))
	next := previous
	next.AccessToken, next.RefreshToken = testAccessToken2, testRefreshToken2
	store.rotated(previous, next)
	changed := catalogWithout(t, []string{"kimi-k3"}, "new-model")
	fx.serveCatalogs(map[string][]byte{testAccessToken2: changed, testKeyA: changed, testKeyB: changed})

	resp := fx.call(http.MethodPost, modelsPath+"/refresh", nil, nil)
	report := decodeBodyAs[refreshReport](t, resp)
	if resp.Status != http.StatusOK || strings.Join(report.Added, ",") != "opencode-go/new-model" || strings.Join(report.Removed, ",") != "opencode-go/kimi-k3" ||
		strings.Join(report.Files, ",") != testLoginName+",opencode-go.json" || !report.Registered || len(report.Failures) != 0 || report.FetchedAt.IsZero() {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
	if len(*asked) != 4 {
		t.Fatalf("model.for_auth asked for %v, want the Go auth of both keys and both auths of the login", *asked)
	}
	for _, name := range []string{testLoginName, "opencode-go.json"} {
		if !strings.Contains(fx.topLevel(name)[catalogRefreshedField], "T") {
			t.Fatalf("%s = %s", name, fx.files[name])
		}
	}
	saved, _ := parseLoginFile([]byte(fx.files[testLoginName]))
	if saved.AccessToken != testAccessToken2 || saved.RefreshToken != testRefreshToken2 || string(saved.extra["priority"]) != "4" {
		t.Fatalf("login after refresh = %+v", saved)
	}
	if file := fx.storedFile("opencode-go.json"); len(file.Keys) != 2 || string(file.extra["weight"]) != "2" {
		t.Fatalf("key file after refresh = %s", fx.files["opencode-go.json"])
	}
	reg, _ := catalogs.registeredFor(keyIDFor(testKeyB), familyGo)
	if !slices.Contains(reg.ids, "opencode-go/new-model") || slices.Contains(reg.ids, "opencode-go/kimi-k3") {
		t.Fatalf("registered = %v", reg.ids)
	}

	// A refresh from the page re-registers an unchanged list too.
	*asked = nil
	report = decodeBodyAs[refreshReport](t, fx.call(http.MethodPost, modelsPath+"/refresh", nil, nil))
	if len(report.Added)+len(report.Removed) != 0 || len(report.Files) != 2 || len(*asked) != 4 || !report.Registered {
		t.Fatalf("second refresh = %+v, asked %v", report, *asked)
	}

	fx.serveCatalogs(nil)
	if resp := fx.call(http.MethodPost, modelsPath+"/refresh", nil, nil); resp.Status != http.StatusBadGateway || !strings.Contains(resp.errorText(t), "401") {
		t.Fatalf("failing refresh: status %d: %s", resp.Status, resp.Body)
	}
	empty := newManagementFixture(t, map[string]string{})
	if resp := empty.call(http.MethodPost, modelsPath+"/refresh", nil, nil); resp.Status != http.StatusConflict {
		t.Fatalf("empty refresh: status %d: %s", resp.Status, resp.Body)
	}
}

func TestRefreshReportsWhenCPADoesNotAsk(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{"opencode-go.json": `{"type":"opencode-go","api_key":"` + testKeyA + `"}`})
	fx.serveCatalogs(map[string][]byte{testKeyA: readFixture(t, "config.json")})
	previous := registrationWait
	registrationWait = 50 * time.Millisecond
	t.Cleanup(func() { registrationWait = previous })
	report := decodeBodyAs[refreshReport](t, fx.call(http.MethodPost, modelsPath+"/refresh", nil, nil))
	if report.Registered || len(report.Files) != 1 {
		t.Fatalf("report = %+v", report)
	}
}

func TestBackgroundRecheckRewritesOnlyChangedFiles(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{
		testLoginName:      string(loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, nil)),
		"opencode-go.json": `{"type":"opencode-go","keys":[{"label":"Work","api_key":"` + testKeyA + `"},{"api_key":"` + testKeyB + `","disabled":true}]}`,
	})
	useLoginStore(t, time.Now())
	original := readFixture(t, "config.json")
	fx.serveCatalogs(map[string][]byte{testAccessToken: original, testKeyA: original, testKeyB: original})
	fx.registerAll()
	asked := fx.registerOnSave()

	recheckCatalogs(context.Background(), newHost(""))
	if fx.saves() != 0 || len(*asked) != 0 {
		t.Fatalf("unchanged catalogs rewrote files: %d saves", fx.saves())
	}

	// Only the key's workspace changed, and a token limit is a change.
	var doc map[string]any
	_ = json.Unmarshal(original, &doc)
	doc["providers"].(map[string]any)[providerKey].(map[string]any)["models"].(map[string]any)["kimi-k3"].(map[string]any)["limit"] = map[string]any{"context": 1, "output": 1}
	fx.serveCatalogs(map[string][]byte{testAccessToken: original, testKeyA: mustJSON(t, doc), testKeyB: original})
	recheckCatalogs(context.Background(), newHost(""))
	if fx.saves() != 1 || strings.Join(*asked, ",") != authIDFor(keyFileName, keyIDFor(testKeyA)) {
		t.Fatalf("saves %d, asked %v", fx.saves(), *asked)
	}
	if _, ok := fx.topLevel(testLoginName)[catalogRefreshedField]; ok {
		t.Fatal("the unchanged login was rewritten")
	}
	logged := false
	for _, payload := range fx.fake.callsTo(pluginabi.MethodHostLog) {
		logged = logged || strings.Contains(string(payload), "catalog changed")
	}
	if !logged {
		t.Fatal("the re-registration was not logged")
	}
	for _, req := range fx.httpCalls() {
		if strings.Contains(req.Headers.Get("Authorization"), testKeyB) {
			t.Fatal("the disabled key was used")
		}
	}
}

// Turning Bare ID on for a Zen model makes every auth file parse again, so
// the Zen auths carry the alias and CPA registers it.
func TestReparseAuthFilesAppliesZenAliases(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{
		testLoginName:      string(loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, map[string]any{"zen": true})),
		"opencode-go.json": `{"type":"opencode-go","keys":[{"label":"Work","api_key":"` + testKeyA + `","zen":true},{"api_key":"` + testKeyB + `","disabled":true}]}`,
	})
	useLoginStore(t, time.Now())
	fx.serveCatalogs(map[string][]byte{testAccessToken: readFixture(t, "config.json"), testKeyA: readFixture(t, "config.json")})
	cfg := defaultSettings()
	cfg.ZenModels = []string{"kimi-k3"}
	useSettings(t, cfg)
	fx.registerAll()
	asked := fx.registerOnSave()
	fetched := len(fx.httpCalls())

	cfg.ZenBareModels = []string{"kimi-k3"}
	useSettings(t, cfg)
	reparseAuthFiles(context.Background(), newHost(""))
	keyA := authIDFor(keyFileName, keyIDFor(testKeyA))
	want := []string{testLoginName, testLoginName + zenAuthSuffix, keyA, keyA + zenAuthSuffix}
	slices.Sort(*asked)
	slices.Sort(want)
	if fx.saves() != 2 || !slices.Equal(*asked, want) || len(fx.httpCalls()) != fetched {
		t.Fatalf("saves %d, asked %v, upstream calls %d", fx.saves(), *asked, len(fx.httpCalls())-fetched)
	}
	for name, content := range fx.files {
		for _, auth := range parsedAuths(name, content) {
			aliases := auth.Attributes["model_aliases"]
			if (authFamily(auth.Attributes) == familyZen) != strings.Contains(aliases, `"alias":"kimi-k3"`) {
				t.Fatalf("%s aliases = %q", auth.ID, aliases)
			}
		}
	}
}

func TestRefreshStopsWithContext(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{"opencode-go.json": `{"type":"opencode-go","api_key":"` + testKeyA + `"}`})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, errRefresh := refreshCatalogs(ctx, newHost(""), refreshOptions{})
	if errRefresh != nil || len(report.Failures) != 1 || !strings.Contains(report.Failures[0], context.Canceled.Error()) || fx.saves() != 0 || len(fx.httpCalls()) != 0 {
		t.Fatalf("report = %+v, %v", report, errRefresh)
	}
}

func TestManagementRouting(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{
		testLoginName:      string(loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, map[string]any{"proxy_url": "socks5://p:1", "priority": -1})),
		"opencode-go.json": `{"type":"opencode-go","prefix":"x","keys":[{"label":"Work","api_key":"` + testKeyA + `"},{"label":"Home","api_key":"` + testKeyB + `"}]}`,
		"single.json":      `{"type":"opencode-go","api_key":"` + testKeyC + `"}`,
		"claude.json":      `{"type":"claude","priority":9}`,
	})
	useLoginStore(t, time.Now())
	fx.serveCatalogs(map[string][]byte{testAccessToken: readFixture(t, "config.json"), testKeyA: readFixture(t, "config.json"), testKeyB: readFixture(t, "config.json"), testKeyC: readFixture(t, "config.json")})
	value := func(v *int64) string {
		if v == nil {
			return "-"
		}
		return strconv.FormatInt(*v, 10)
	}
	routing := func(values *routingValues) string {
		if values == nil {
			return "none"
		}
		return value(values.Priority) + "/" + value(values.Weight)
	}

	// CPA applies the login's top-level priority to both of its auths.
	view := decodeBodyAs[routingView](t, fx.call(http.MethodGet, routingPath, nil, nil))
	if len(view.Files) != 3 || view.Files[0].File != testLoginName || !view.Files[0].Shared || routing(view.Files[0].Go) != "-1/-" || routing(view.Files[0].Zen) != "-1/-" ||
		!view.Go.Mixed || !view.Zen.Mixed || view.Files[1].Shared || routing(view.Files[1].Zen) != "-/-" {
		t.Fatalf("initial = %+v", view)
	}

	resp := fx.call(http.MethodPut, routingPath, nil, map[string]any{"go": map[string]any{"priority": 5, "weight": 3}, "zen": map[string]any{"priority": -2}})
	view = decodeBodyAs[routingView](t, resp)
	if resp.Status != http.StatusOK || routing(&view.Go.routingValues) != "5/3" || routing(&view.Zen.routingValues) != "-2/-" || view.Go.Mixed || view.Zen.Mixed || view.Files[0].Shared {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
	for _, name := range []string{testLoginName, "opencode-go.json", "single.json"} {
		top := fx.topLevel(name)
		if top["go_priority"] != "5" || top["go_weight"] != "3" || top["zen_priority"] != "-2" {
			t.Fatalf("%s = %s", name, fx.files[name])
		}
		for _, field := range []string{"zen_weight", "priority", "weight"} {
			if _, ok := top[field]; ok {
				t.Fatalf("%s keeps %s: %s", name, field, fx.files[name])
			}
		}
		// Each family's auths carry their own routing, which CPA no longer
		// overrides with a top-level field.
		for _, auth := range parsedAuths(name, fx.files[name]) {
			want := map[modelFamily]string{familyGo: "5/3", familyZen: "-2/"}[authFamily(auth.Attributes)]
			if got := auth.Attributes["priority"] + "/" + auth.Attributes["weight"]; got != want {
				t.Fatalf("%s auth %s routing = %s, want %s", name, auth.ID, got, want)
			}
		}
	}
	if fx.topLevel("claude.json")["priority"] != "9" || fx.topLevel("opencode-go.json")["prefix"] != `"x"` || fx.topLevel(testLoginName)["proxy_url"] != `"socks5://p:1"` {
		t.Fatal("routing touched an unrelated field or file")
	}
	if login, _ := parseLoginFile([]byte(fx.files[testLoginName])); login.AccessToken != testAccessToken || login.RefreshToken != testRefreshToken {
		t.Fatalf("login tokens changed: %+v", login)
	}

	// The plugin's own rewrites keep the fields: rename, add and disable a
	// key, disable the login, and a model refresh.
	if resp := fx.call(http.MethodPatch, keysPath, nil, map[string]any{"id": keyIDFor(testKeyA), "label": "Renamed", "disabled": true}); resp.Status != http.StatusOK {
		t.Fatalf("rename: %d %s", resp.Status, resp.Body)
	}
	if resp := fx.call(http.MethodPatch, loginsPath, nil, map[string]any{"id": testLoginName, "disabled": true}); resp.Status != http.StatusOK {
		t.Fatalf("disable login: %d %s", resp.Status, resp.Body)
	}
	if _, errRefresh := refreshCatalogs(context.Background(), newHost(""), refreshOptions{force: true}); errRefresh != nil {
		t.Fatal(errRefresh)
	}
	for _, name := range []string{testLoginName, "opencode-go.json", "single.json"} {
		if top := fx.topLevel(name); top["go_priority"] != "5" || top["go_weight"] != "3" || top["zen_priority"] != "-2" {
			t.Fatalf("%s lost routing fields: %s", name, fx.files[name])
		}
	}

	// null restores the default by removing the field; a field left out
	// stays as it is.
	view = decodeBodyAs[routingView](t, fx.call(http.MethodPut, routingPath, nil, map[string]any{"go": map[string]any{"priority": nil}, "zen": map[string]any{"weight": 2}}))
	if routing(&view.Go.routingValues) != "-/3" || routing(&view.Zen.routingValues) != "-2/2" {
		t.Fatalf("after null = %+v", view)
	}
	if _, ok := fx.topLevel("single.json")["go_priority"]; ok {
		t.Fatalf("go_priority kept: %s", fx.files["single.json"])
	}

	// A top-level field written by hand applies to both families; saving
	// moves it into the family fields, so only what was sent changes.
	fx.files["single.json"] = `{"type":"opencode-go","api_key":"` + testKeyC + `","priority":"7","weight":3}`
	view = decodeBodyAs[routingView](t, fx.call(http.MethodGet, routingPath, nil, nil))
	if !view.Go.Mixed || !view.Zen.Mixed || !view.Files[2].Shared || routing(view.Files[2].Go) != "7/3" || routing(view.Files[2].Zen) != "7/3" {
		t.Fatalf("mixed = %+v", view)
	}
	decodeBodyAs[routingView](t, fx.call(http.MethodPut, routingPath, nil, map[string]any{"zen": map[string]any{"priority": -2}}))
	if top := fx.topLevel("single.json"); top["go_priority"] != "7" || top["go_weight"] != "3" || top["zen_priority"] != "-2" || top["zen_weight"] != "3" || top["priority"] != "" || top["weight"] != "" {
		t.Fatalf("moved = %s", fx.files["single.json"])
	}

	for _, body := range []string{`{}`, `{"priority":5}`, `{"go":{}}`, `{"go":5}`, `{"go":null}`, `{"go":{"weight":-1}}`, `{"zen":{"weight":1.5}}`, `{"go":{"weight":1000001}}`, `{"go":{"priority":"x"}}`, `{"zen":{"priority":3000000000}}`, `[]`} {
		if resp := fx.call(http.MethodPut, routingPath, nil, body); resp.Status != http.StatusBadRequest {
			t.Fatalf("%s: status %d: %s", body, resp.Status, resp.Body)
		}
	}
	empty := newManagementFixture(t, map[string]string{})
	if resp := empty.call(http.MethodPut, routingPath, nil, map[string]any{"go": map[string]any{"weight": 1}}); resp.Status != http.StatusConflict {
		t.Fatalf("no files: status %d", resp.Status)
	}
}

func TestAuthRefreshKeepsRoutingFieldsAndMark(t *testing.T) {
	useSettings(t, defaultSettings())
	useLoginStore(t, time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC))
	console := newFakeConsole()
	console.token = func(url.Values) gatewayReply { return tokenJSON(testAccessToken2, testRefreshToken2, testOrgID) }
	installFakeGateway(t, console.reply(t, nil))

	storage := loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, map[string]any{"priority": 6, "go_weight": 4, "zen_priority": -2, catalogRefreshedField: "2026-10-12T20:00:00Z"})
	raw, errHandle := handleMethod(pluginabi.MethodAuthRefresh, mustJSON(t, rpcAuthRefreshRequest{AuthRefreshRequest: pluginapi.AuthRefreshRequest{
		AuthID: testLoginName + zenAuthSuffix, StorageJSON: storage, Attributes: map[string]string{familyAttribute: "zen"},
		Metadata: map[string]any{"priority": float64(6), "access_token": testAccessToken, "refresh_token": testRefreshToken},
	}}))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	resp := decodeResult[pluginapi.AuthRefreshResponse](t, raw)
	persisted := persistedFile(t, resp.Auth)
	if persisted.RefreshToken != testRefreshToken2 || string(persisted.extra["priority"]) != "6" || string(persisted.extra["go_weight"]) != "4" || string(persisted.extra["zen_priority"]) != "-2" {
		t.Fatalf("persisted = %+v", persisted)
	}
	// The refreshed auth stays its family's.
	if attrs := resp.Auth.Attributes; attrs[catalogRefreshedField] != "2026-10-12T20:00:00Z" || attrs[familyAttribute] != "zen" || attrs["priority"] != "-2" || attrs["weight"] != "" {
		t.Fatalf("attributes = %v", resp.Auth.Attributes)
	}
}

func TestLogoResource(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{})
	resp := fx.call(http.MethodGet, logoPath, nil, nil)
	if resp.Status != http.StatusOK || !bytes.Equal(resp.Body, logoSVG) || resp.Headers.Get("Content-Type") != "image/svg+xml" ||
		resp.Headers.Get("X-Content-Type-Options") != "nosniff" || resp.Headers.Get("Content-Security-Policy") != "default-src 'none'" {
		t.Fatalf("status %d, headers %v", resp.Status, resp.Headers)
	}
	if len(fx.fake.calls) != 0 {
		t.Fatalf("serving the logo called the host: %+v", fx.fake.calls)
	}
	var svg struct {
		XMLName xml.Name
		ViewBox string `xml:"viewBox,attr"`
	}
	if errParse := xml.Unmarshal(logoSVG, &svg); errParse != nil || svg.XMLName.Local != "svg" || svg.ViewBox != "0 0 512 512" {
		t.Fatalf("logo = %+v, %v", svg, errParse)
	}
	if bytes.Contains(logoSVG, []byte("<script")) || bytes.Contains(logoSVG, []byte("<style")) || bytes.Count(logoSVG, []byte("<svg")) != 1 {
		t.Fatal("logo must be one plain svg element")
	}

	meta := pluginRegistration().Metadata
	if meta.Logo != logoPath || html.EscapeString(meta.Logo) != meta.Logo {
		t.Fatalf("Metadata.Logo = %q", meta.Logo)
	}
	reg := decodeResult[pluginapi.ManagementRegistrationResponse](t, mustManagementRegister(t))
	found := false
	for _, resource := range reg.Resources {
		found = found || (resource.Path == logoPath && resource.Menu == "")
	}
	if !found {
		t.Fatalf("resources = %+v", reg.Resources)
	}

	// The store registry links ../logo.svg; the plugin serves this copy.
	store, errRead := os.ReadFile("../logo.svg")
	if errors.Is(errRead, os.ErrNotExist) {
		t.Skip("the store checkout is not present")
	}
	if errRead != nil || !bytes.Equal(store, logoSVG) {
		t.Fatalf("../logo.svg differs from go/logo.svg (%v)", errRead)
	}
}

func mustManagementRegister(t *testing.T) []byte {
	t.Helper()
	raw, errHandle := handleMethod(pluginabi.MethodManagementRegister, []byte(`{}`))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	return raw
}

// CPA HTML-escapes plugin metadata and route descriptions, and Management
// Center shows the escapes literally.
func TestRegistrationTextNeedsNoEscaping(t *testing.T) {
	meta := pluginRegistration().Metadata
	texts := []string{meta.Name, meta.Author, meta.Logo}
	for _, field := range meta.ConfigFields {
		texts = append(texts, field.Name, field.Description)
	}
	reg := decodeResult[pluginapi.ManagementRegistrationResponse](t, mustManagementRegister(t))
	for _, route := range reg.Routes {
		texts = append(texts, route.Description)
	}
	for _, resource := range reg.Resources {
		texts = append(texts, resource.Menu, resource.Description)
	}
	for _, text := range texts {
		if html.EscapeString(text) != text {
			t.Fatalf("%q changes when CPA escapes it", text)
		}
	}
}

// TestManagementPageLogic runs the page's config-editing script under node,
// which the nix build provides to the check phase.
func TestManagementPageLogic(t *testing.T) {
	node, errLook := exec.LookPath("node")
	if errLook != nil {
		t.Skip("node is not installed")
	}
	out, errRun := exec.Command(node, "--test", "testdata/page_test.mjs").CombinedOutput()
	if errRun != nil {
		t.Fatalf("node --test: %v\n%s", errRun, out)
	}
}
