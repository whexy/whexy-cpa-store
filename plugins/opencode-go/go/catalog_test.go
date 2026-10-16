package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestParseCatalogFixture(t *testing.T) {
	cat, errParse := parseCatalog(readFixture(t, "config.json"))
	if errParse != nil {
		t.Fatal(errParse)
	}
	if cat.Workspace != "wrk_TEST000000000000000000000" {
		t.Fatalf("workspace = %q", cat.Workspace)
	}
	var ids []string
	byID := map[string]catalogModel{}
	for _, model := range cat.Models {
		ids = append(ids, model.UpstreamID)
		byID[model.UpstreamID] = model
	}
	if strings.Join(ids, ",") != "claude-haiku-5-5,deepseek-v4-flash,elsewhere,gpt-6-luna,kimi-k3,longcat-2.5-preview-free,qwen3.8-max" {
		t.Fatalf("ids = %v (disabled models must be skipped)", ids)
	}
	tests := []struct {
		id       string
		protocol sdktranslator.Format
		free     bool
	}{
		{"claude-haiku-5-5", sdktranslator.FormatClaude, false},
		{"qwen3.8-max", sdktranslator.FormatClaude, false},
		{"gpt-6-luna", sdktranslator.FormatOpenAIResponse, false},
		{"deepseek-v4-flash", sdktranslator.FormatOpenAI, false},
		{"kimi-k3", sdktranslator.FormatOpenAI, false},
		{"longcat-2.5-preview-free", sdktranslator.FormatOpenAI, true},
	}
	for _, tt := range tests {
		got := byID[tt.id]
		if got.Protocol != tt.protocol || got.Free != tt.free {
			t.Fatalf("%s = %+v", tt.id, got)
		}
	}
	luna := byID["gpt-6-luna"]
	if luna.ContextLength != 1050000 || luna.InputLimit != 922000 || luna.OutputLimit != 128000 || strings.Join(luna.InputModalities, ",") != "text,image,pdf" {
		t.Fatalf("gpt-6-luna = %+v", luna)
	}
}

// The opencode provider of the same config holds the workspace's
// pay-as-you-go (OpenCode Zen) models. Disabled models and the API families
// the plugin cannot serve are dropped.
func TestParseCatalogZenFixture(t *testing.T) {
	cat, errParse := parseCatalog(readFixture(t, "config.json"))
	if errParse != nil {
		t.Fatal(errParse)
	}
	byID := map[string]catalogModel{}
	var ids []string
	for _, model := range cat.Zen {
		ids = append(ids, model.UpstreamID)
		byID[model.UpstreamID] = model
	}
	if strings.Join(ids, ",") != "cat-jam-free,claude-haiku-5-5,gpt-6-luna,kimi-k3" {
		t.Fatalf("zen ids = %v (disabled, Gemini and SystemOne models must be skipped)", ids)
	}
	tests := []struct {
		id       string
		protocol sdktranslator.Format
		free     bool
	}{
		{"cat-jam-free", sdktranslator.FormatOpenAI, true},
		{"claude-haiku-5-5", sdktranslator.FormatClaude, false},
		{"gpt-6-luna", sdktranslator.FormatOpenAIResponse, false},
		{"kimi-k3", sdktranslator.FormatOpenAI, false},
	}
	for _, tt := range tests {
		got := byID[tt.id]
		if got.Family != familyZen || got.Protocol != tt.protocol || got.Free != tt.free {
			t.Fatalf("%s = %+v", tt.id, got)
		}
	}
}

// A Zen model keeps its own prefix and reaches the pay-as-you-go paths, and
// only the models the page turned on resolve at all.
func TestZenRoutes(t *testing.T) {
	cat, _ := parseCatalog(readFixture(t, "config.json"))
	cfg := settings{BaseURL: defaultBaseURL, ModelPrefix: defaultModelPrefix, ZenModels: []string{"kimi-k3", "gpt-6-luna", "claude-haiku-5-5"}}
	tests := []struct {
		model    string
		upstream string
		endpoint string
	}{
		{"opencode/kimi-k3", "kimi-k3", "https://opencode.ai/inference/openai/v1/chat/completions"},
		{"opencode/gpt-6-luna", "gpt-6-luna", "https://opencode.ai/inference/openai/v1/responses"},
		{"opencode/claude-haiku-5-5", "claude-haiku-5-5", "https://opencode.ai/inference/anthropic/v1/messages"},
	}
	for _, tt := range tests {
		model, ok := cat.find(cfg, tt.model, familyZen)
		if !ok || model.Family != familyZen {
			t.Fatalf("%s -> %+v %v", tt.model, model, ok)
		}
		route := model.route(cfg)
		if route.Endpoint != tt.endpoint || route.ID != tt.model || route.UpstreamID != tt.upstream {
			t.Fatalf("route = %+v", route)
		}
	}
	// The same upstream ID still routes to the Go gateway under its own prefix.
	if route, ok := cat.find(cfg, "opencode-go/kimi-k3", familyGo); !ok || route.Family != familyGo {
		t.Fatalf("go model = %+v %v", route, ok)
	}
	// Each family's auth resolves only its own models, so a shared bare ID
	// reaches the gateway of the auth CPA picked.
	if model, ok := cat.find(cfg, "kimi-k3", familyZen); !ok || model.Family != familyZen {
		t.Fatalf("bare kimi-k3 on the Zen auth = %+v %v", model, ok)
	}
	if model, ok := cat.find(cfg, "kimi-k3(high)", familyGo); !ok || model.Family != familyGo {
		t.Fatalf("bare kimi-k3 on the Go auth = %+v %v", model, ok)
	}
	if _, ok := cat.find(cfg, "opencode-go/kimi-k3", familyZen); ok {
		t.Fatal("the Zen auth resolved a Go model")
	}
	if _, ok := cat.find(cfg, "opencode/kimi-k3", familyGo); ok {
		t.Fatal("the Go auth resolved a Zen model")
	}
	// A Zen model the page has not turned on does not resolve.
	if _, ok := cat.find(cfg, "opencode/cat-jam-free", familyZen); ok {
		t.Fatal("a Zen model that is not enabled resolved")
	}
	// A mirror moves both gateways, since the Zen base is the Go base without
	// its Go path.
	mirror := settings{BaseURL: "https://mirror.test/x/go", ModelPrefix: defaultModelPrefix, ZenModels: []string{"kimi-k3"}}
	model, ok := cat.find(mirror, "opencode/kimi-k3", familyZen)
	if !ok || model.route(mirror).Endpoint != "https://mirror.test/x/openai/v1/chat/completions" {
		t.Fatalf("mirrored route = %+v %v", model, ok)
	}
}

// A Zen catalog base that names no protocol family would aim the request at
// a path the gateway does not serve it from, so the model's protocol decides.
func TestZenEndpointRebasing(t *testing.T) {
	cfg := defaultSettings()
	cases := []struct {
		base     string
		protocol sdktranslator.Format
		want     string
	}{
		{"https://opencode.ai/inference/openai/v1", sdktranslator.FormatOpenAI, "https://opencode.ai/inference/openai/v1/chat/completions"},
		{"https://opencode.ai/inference/openai/v1", sdktranslator.FormatOpenAIResponse, "https://opencode.ai/inference/openai/v1/responses"},
		{"https://opencode.ai/inference/anthropic/v1", sdktranslator.FormatClaude, "https://opencode.ai/inference/anthropic/v1/messages"},
		{"https://opencode.ai/inference/v1", sdktranslator.FormatOpenAI, "https://opencode.ai/inference/openai/v1/chat/completions"},
		{"https://opencode.ai/inference/go/openai/v1", sdktranslator.FormatOpenAI, "https://opencode.ai/inference/openai/v1/chat/completions"},
		{"https://elsewhere.test/v1", sdktranslator.FormatOpenAI, "https://opencode.ai/inference/openai/v1/chat/completions"},
		{"", sdktranslator.FormatClaude, "https://opencode.ai/inference/anthropic/v1/messages"},
	}
	for _, tt := range cases {
		if got := endpointFor(cfg, familyZen, tt.protocol, tt.base); got != tt.want {
			t.Fatalf("zen %q %s -> %s, want %s", tt.base, tt.protocol, got, tt.want)
		}
	}
	// The Go gateway serves every protocol under its own subtree, so any
	// other opencode.ai base is ignored and any Go base is kept.
	if got := endpointFor(cfg, familyGo, sdktranslator.FormatClaude, "https://opencode.ai/inference/openai/v1"); got != "https://opencode.ai/inference/go/anthropic/v1/messages" {
		t.Fatalf("go foreign base -> %s", got)
	}
	if got := endpointFor(cfg, familyGo, sdktranslator.FormatClaude, "https://opencode.ai/inference/go/anthropic/v1"); got != "https://opencode.ai/inference/go/anthropic/v1/messages" {
		t.Fatalf("go own base -> %s", got)
	}
}

// Only the Zen models the page turned on are registered with CPA, each
// family's with its own auths, and every model's route stays inside its own
// gateway.
func TestZenServingIsOptIn(t *testing.T) {
	cat, _ := parseCatalog(readFixture(t, "config.json"))
	ids := func(cfg settings, family modelFamily) string {
		var out []string
		for _, info := range cat.modelInfos(cfg, family) {
			out = append(out, info.ID)
		}
		return strings.Join(out, ",")
	}
	const goIDs = "opencode-go/claude-haiku-5-5,opencode-go/deepseek-v4-flash,opencode-go/elsewhere,opencode-go/gpt-6-luna,opencode-go/kimi-k3,opencode-go/longcat-2.5-preview-free,opencode-go/qwen3.8-max"
	if got := ids(defaultSettings(), familyGo); got != goIDs {
		t.Fatalf("Go models = %s", got)
	}
	if got := ids(defaultSettings(), familyZen); got != "" {
		t.Fatalf("Zen models without any enabled = %s", got)
	}
	cfg := defaultSettings()
	cfg.ZenModels = []string{"KIMI-K3", "gpt-6-luna"}
	const zenIDs = "opencode/gpt-6-luna,opencode/kimi-k3"
	if got := ids(cfg, familyGo); got != goIDs {
		t.Fatalf("Go models with Zen enabled = %s", got)
	}
	if got := ids(cfg, familyZen); got != zenIDs {
		t.Fatalf("Zen models = %s\nwant %s", got, zenIDs)
	}
	if got := strings.Join(cat.exposedIDs(cfg, familyZen), ","); got != zenIDs {
		t.Fatalf("exposed IDs = %s\nwant %s", got, zenIDs)
	}
	for _, family := range families {
		for _, info := range cat.modelInfos(cfg, family) {
			model, ok := cat.find(cfg, info.ID, family)
			if !ok || model.Family != family {
				t.Fatalf("%s does not resolve for %s: %+v", info.ID, family, model)
			}
			root := defaultBaseURL + "/"
			if model.Family == familyZen {
				root = defaultZenBaseURL + "/"
			}
			if route := model.route(cfg); !strings.HasPrefix(route.Endpoint, root) {
				t.Fatalf("%s routes outside its gateway: %s", info.ID, route.Endpoint)
			}
		}
	}
	// Turning a Zen model off changes the Zen fingerprint alone, so the next
	// refresh rewrites the auth files and CPA registers the list again.
	before, goBefore := cat.fingerprint(cfg, familyZen), cat.fingerprint(cfg, familyGo)
	cfg.ZenModels = []string{"KIMI-K3"}
	if after := cat.fingerprint(cfg, familyZen); after == before {
		t.Fatal("fingerprint ignores the Zen model set")
	}
	if cat.fingerprint(cfg, familyGo) != goBefore {
		t.Fatal("the Go fingerprint depends on the Zen model set")
	}
}

func TestEmbeddedSnapshotMatchesVerifiedProtocols(t *testing.T) {
	if strings.Contains(string(snapshotJSON), "wrk_") || strings.Contains(string(snapshotJSON), "oc_sk_") {
		t.Fatal("snapshot carries workspace IDs or keys")
	}
	cat, errParse := parseCatalog(snapshotJSON)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if len(cat.Models) < 20 || cat.Workspace != "" {
		t.Fatalf("snapshot = %d models, workspace %q", len(cat.Models), cat.Workspace)
	}
	// Protocol table verified live (knowledge: go-gateway-api.md).
	want := map[string]sdktranslator.Format{
		"deepseek-v4.1-flash":        sdktranslator.FormatOpenAI,
		"glm-5.3":                    sdktranslator.FormatOpenAI,
		"kimi-k3":                    sdktranslator.FormatOpenAI,
		"space-bunny":                sdktranslator.FormatOpenAI,
		"gpt-6-luna":                 sdktranslator.FormatOpenAIResponse,
		"gpt-5.6-luna":               sdktranslator.FormatOpenAIResponse,
		"grok-4.7":                   sdktranslator.FormatOpenAIResponse,
		"muse-spark-1.3-contributor": sdktranslator.FormatOpenAIResponse,
		"claude-haiku-5-5":           sdktranslator.FormatClaude,
		"minimax-m2.7":               sdktranslator.FormatClaude,
		"qwen3.7-plus":               sdktranslator.FormatClaude,
		"qwen3.8-max":                sdktranslator.FormatClaude,
	}
	cfg := defaultSettings()
	for id, protocol := range want {
		model, ok := cat.find(cfg, id, familyGo)
		if !ok || model.Protocol != protocol {
			t.Fatalf("%s = %+v, want %s", id, model, protocol)
		}
	}
	snapshot, source := newCatalogStore().cached("any")
	if source != catalogSnapshot || len(snapshot.Models) != len(cat.Models) {
		t.Fatalf("cold store served %s with %d models", source, len(snapshot.Models))
	}
}

func TestRoutes(t *testing.T) {
	cat, _ := parseCatalog(readFixture(t, "config.json"))
	tests := []struct {
		name     string
		cfg      settings
		model    string
		endpoint string
		id       string
	}{
		{"chat default", defaultSettings(), "kimi-k3", "https://opencode.ai/inference/go/openai/v1/chat/completions", "opencode-go/kimi-k3"},
		{"responses default", defaultSettings(), "gpt-6-luna", "https://opencode.ai/inference/go/openai/v1/responses", "opencode-go/gpt-6-luna"},
		{"messages default", defaultSettings(), "claude-haiku-5-5", "https://opencode.ai/inference/go/anthropic/v1/messages", "opencode-go/claude-haiku-5-5"},
		{"custom base", settings{BaseURL: "http://mirror.test/go", ModelPrefix: "go"}, "qwen3.8-max", "http://mirror.test/go/anthropic/v1/messages", "go/qwen3.8-max"},
		{"non-Go catalog base", defaultSettings(), "elsewhere", "https://opencode.ai/inference/go/openai/v1/chat/completions", "opencode-go/elsewhere"},
		{"no prefix", settings{BaseURL: defaultBaseURL}, "kimi-k3", "https://opencode.ai/inference/go/openai/v1/chat/completions", "kimi-k3"},
		{"prefix disabled", settings{BaseURL: defaultBaseURL, ModelPrefix: "go", DisablePrefix: true}, "kimi-k3", "https://opencode.ai/inference/go/openai/v1/chat/completions", "kimi-k3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, ok := cat.find(tt.cfg, tt.model, familyGo)
			if !ok {
				t.Fatalf("%s not found", tt.model)
			}
			route := model.route(tt.cfg)
			if route.Endpoint != tt.endpoint || route.ID != tt.id || route.UpstreamID != tt.model {
				t.Fatalf("route = %+v", route)
			}
		})
	}
}

func TestCatalogFindAcceptsExposedBareAndSuffixedIDs(t *testing.T) {
	cat, _ := parseCatalog(readFixture(t, "config.json"))
	cfg := defaultSettings()
	for _, requested := range []string{"opencode-go/kimi-k3", "kimi-k3", "opencode-go/kimi-k3(high)", " kimi-k3(8192) "} {
		if model, ok := cat.find(cfg, requested, familyGo); !ok || model.UpstreamID != "kimi-k3" {
			t.Fatalf("%q -> %+v %v", requested, model, ok)
		}
	}
	for _, requested := range []string{"other/kimi-k3", "kimi-k9", "retired-model"} {
		if _, ok := cat.find(cfg, requested, familyGo); ok {
			t.Fatalf("%q resolved", requested)
		}
	}
}

// With disable_prefix on, CPA registers bare IDs only and refuses a
// client's prefixed request itself; the prefixed form still resolves
// because a kept Bare ID alias hands it to the executor.
func TestCatalogFindWithPrefixDisabled(t *testing.T) {
	cat, _ := parseCatalog(readFixture(t, "config.json"))
	cfg := settings{BaseURL: defaultBaseURL, ModelPrefix: "go", DisablePrefix: true}
	for _, requested := range []string{"kimi-k3", "kimi-k3(high)", "go/kimi-k3"} {
		if model, ok := cat.find(cfg, requested, familyGo); !ok || model.UpstreamID != "kimi-k3" {
			t.Fatalf("%q -> %+v %v", requested, model, ok)
		}
	}
	if _, ok := cat.find(cfg, "opencode-go/kimi-k3", familyGo); ok {
		t.Fatal("a prefix other than the configured one resolved")
	}
}

func TestModelInfos(t *testing.T) {
	cat, _ := parseCatalog(readFixture(t, "config.json"))
	infos := cat.modelInfos(defaultSettings(), familyGo)
	var haiku, kimi pluginapi.ModelInfo
	for _, info := range infos {
		switch info.Name {
		case "claude-haiku-5-5":
			haiku = info
		case "kimi-k3":
			kimi = info
		}
	}
	if haiku.ID != "opencode-go/claude-haiku-5-5" || haiku.DisplayName != "Claude Haiku 5.5" || haiku.OwnedBy != "opencode-go" || haiku.Object != "model" {
		t.Fatalf("haiku = %+v", haiku)
	}
	if haiku.ContextLength != 1000000 || haiku.InputTokenLimit != 1000000 || haiku.MaxCompletionTokens != 128000 || haiku.OutputTokenLimit != 128000 {
		t.Fatalf("haiku limits = %+v", haiku)
	}
	if strings.Join(kimi.SupportedInputModalities, ",") != "text,image,video" || strings.Join(kimi.SupportedOutputModalities, ",") != "text" {
		t.Fatalf("kimi modalities = %+v", kimi)
	}
}

func TestModelInfosAndExposedIDsPerMode(t *testing.T) {
	cat, _ := parseCatalog(readFixture(t, "config.json"))
	all := "claude-haiku-5-5,deepseek-v4-flash,elsewhere,gpt-6-luna,kimi-k3,longcat-2.5-preview-free,qwen3.8-max"
	prefixed := "opencode-go/" + strings.ReplaceAll(all, ",", ",opencode-go/")
	disabled := []string{"KIMI-K3", "gpt-6-luna", "not-in-catalog"}
	tests := []struct {
		name string
		cfg  settings
		want string
	}{
		{"prefixed", defaultSettings(), prefixed},
		{"bare", settings{BaseURL: defaultBaseURL, ModelPrefix: "go", DisablePrefix: true}, all},
		{"prefixed with disabled models", settings{BaseURL: defaultBaseURL, ModelPrefix: defaultModelPrefix, DisabledModels: disabled},
			"opencode-go/claude-haiku-5-5,opencode-go/deepseek-v4-flash,opencode-go/elsewhere,opencode-go/longcat-2.5-preview-free,opencode-go/qwen3.8-max"},
		{"bare with disabled models", settings{BaseURL: defaultBaseURL, ModelPrefix: defaultModelPrefix, DisablePrefix: true, DisabledModels: disabled},
			"claude-haiku-5-5,deepseek-v4-flash,elsewhere,longcat-2.5-preview-free,qwen3.8-max"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ids []string
			for _, info := range cat.modelInfos(tt.cfg, familyGo) {
				ids = append(ids, info.ID)
			}
			if strings.Join(ids, ",") != tt.want {
				t.Fatalf("model infos = %v\nwant %s", ids, tt.want)
			}
			if got := strings.Join(cat.exposedIDs(tt.cfg, familyGo), ","); got != tt.want {
				t.Fatalf("exposed IDs = %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestFetchCatalogRequest(t *testing.T) {
	doer := &fakeDoer{respond: func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return jsonResponse(200, readFixture(t, "config.json")), nil
	}}
	cfg := settings{BaseURL: "https://opencode.ai/inference/go", UserAgent: "ua/1"}
	if _, errFetch := fetchCatalog(context.Background(), doer, cfg, testKeyA); errFetch != nil {
		t.Fatal(errFetch)
	}
	req := doer.requests[0]
	if req.Method != http.MethodGet || req.URL != "https://opencode.ai/console/api/v2/config" {
		t.Fatalf("request = %s %s", req.Method, req.URL)
	}
	if req.Headers.Get("Authorization") != "Bearer "+testKeyA || req.Headers.Get("User-Agent") != "ua/1" {
		t.Fatalf("headers = %v", req.Headers)
	}
	if got := configURL(settings{BaseURL: "http://mirror.test:8080/x/go"}); got != "http://mirror.test:8080/console/api/v2/config" {
		t.Fatalf("config url = %q", got)
	}
}

func TestFetchCatalogErrors(t *testing.T) {
	doer := &fakeDoer{respond: func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		resp := jsonResponse(401, []byte(`{"type":"error","error":{"type":"AuthError","message":"Invalid API key `+testKeyA+`"}}`))
		resp.Headers.Set("x-opencode-log-id", "log-x")
		return resp, nil
	}}
	_, errFetch := fetchCatalog(context.Background(), doer, defaultSettings(), testKeyA)
	var upstream *upstreamError
	if !errors.As(errFetch, &upstream) || upstream.Status != 401 || upstream.Type != "AuthError" || upstream.LogID != "log-x" {
		t.Fatalf("err = %#v", errFetch)
	}
	if strings.Contains(errFetch.Error(), testKeyA) {
		t.Fatalf("error leaks key: %v", errFetch)
	}
}

func TestCatalogStoreFallbacks(t *testing.T) {
	config := readFixture(t, "config.json")
	failing := false
	doer := &fakeDoer{respond: func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if failing {
			return pluginapi.HTTPResponse{}, errors.New("connection refused")
		}
		return jsonResponse(200, config), nil
	}}
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	store := newCatalogStore()
	store.now = func() time.Time { return now }
	cfg := defaultSettings()
	keyA := keyCredential(keyEntry{ID: keyIDFor(testKeyA), APIKey: testKeyA})
	keyB := keyCredential(keyEntry{ID: keyIDFor(testKeyB), APIKey: testKeyB})

	if _, source, errFetch := store.forKey(context.Background(), doer, cfg, keyA); source != catalogFresh || errFetch != nil {
		t.Fatalf("first = %s %v", source, errFetch)
	}
	if _, source, _ := store.forKey(context.Background(), doer, cfg, keyA); source != catalogCached || len(doer.requests) != 1 {
		t.Fatalf("within ttl = %s after %d requests", source, len(doer.requests))
	}
	now = now.Add(catalogTTL)
	failing = true
	cat, source, errFetch := store.forKey(context.Background(), doer, cfg, keyA)
	if source != catalogStale || errFetch == nil || cat.Workspace != "wrk_TEST000000000000000000000" {
		t.Fatalf("expired + failing = %s %v", source, errFetch)
	}
	if _, source, _ := store.forKey(context.Background(), doer, cfg, keyB); source != catalogLastGood {
		t.Fatalf("new key while failing = %s", source)
	}
	if _, source, _ := newCatalogStore().forKey(context.Background(), doer, cfg, keyB); source != catalogSnapshot {
		t.Fatalf("cold store while failing = %s", source)
	}
	failing = false
	if _, source, _ := store.forKey(context.Background(), doer, cfg, keyA); source != catalogFresh {
		t.Fatalf("recovered = %s", source)
	}
}

func TestModelsForAuthThroughHost(t *testing.T) {
	useSettings(t, defaultSettings())
	useCatalogStore(t)
	fake := installFakeHost(t)
	config := readFixture(t, "config.json")
	fake.handle(pluginabi.MethodHostHTTPDo, func(payload json.RawMessage) (any, error) {
		var req hostHTTPRequest
		_ = json.Unmarshal(payload, &req)
		if req.HostCallbackID != "cb-m" || req.Headers.Get("Authorization") != "Bearer "+testKeyB {
			t.Errorf("request = %+v", req)
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: config}, nil
	})
	keyB := parseAuths(pluginapi.AuthParseRequest{FileName: "opencode-go.json", RawJSON: multiKeyFile(t)}).Auths[1]
	req := mustJSON(t, rpcAuthModelRequest{
		AuthModelRequest: pluginapi.AuthModelRequest{
			AuthID:       keyB.ID,
			AuthProvider: "opencode-go",
			StorageJSON:  hostStorageJSON(t, keyB.StorageJSON, nil),
			Attributes:   keyB.Attributes,
		},
		HostCallbackID: "cb-m",
	})
	raw, errHandle := handleMethod(pluginabi.MethodModelForAuth, req)
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	resp := decodeResult[pluginapi.ModelResponse](t, raw)
	if resp.Provider != "opencode-go" || len(resp.Models) != 7 {
		t.Fatalf("resp = %+v", resp)
	}
	for _, model := range resp.Models {
		if !strings.HasPrefix(model.ID, "opencode-go/") {
			t.Fatalf("unprefixed model %q", model.ID)
		}
	}
	route, ok := lookupModel("opencode-go/gpt-6-luna", keyIDFor(testKeyB), familyGo)
	if !ok || route.Protocol != sdktranslator.FormatOpenAIResponse || route.UpstreamID != "gpt-6-luna" {
		t.Fatalf("lookup = %+v %v", route, ok)
	}
}

// model.for_auth drops disabled_models in both modes. With the prefix
// disabled it also drops the models 0.2.0 disabled through exact prefixed
// oauth-excluded-models entries, which CPA no longer matches; with the prefix
// on, CPA applies those entries itself.
func TestModelsForAuthDropsDisabledModels(t *testing.T) {
	useTestCatalog(t)
	excluded := map[string][]string{
		" OpenCode-Go ": {"opencode-go/KIMI-K3", "go/qwen3.8-max", "opencode-go/claude-*", "deepseek-v4-flash", "opencode-go/"},
		"codex":         {"opencode-go/elsewhere"},
	}
	registered := func(cfg settings) string {
		t.Helper()
		useSettings(t, cfg)
		raw, errHandle := handleMethod(pluginabi.MethodModelForAuth, mustJSON(t, rpcAuthModelRequest{AuthModelRequest: pluginapi.AuthModelRequest{
			StorageJSON: []byte(`{"type":"opencode-go","api_key":"` + testKeyA + `"}`),
			Host:        pluginapi.HostConfigSummary{ExcludedModels: excluded},
		}}))
		if errHandle != nil {
			t.Fatal(errHandle)
		}
		var ids []string
		for _, model := range decodeResult[pluginapi.ModelResponse](t, raw).Models {
			ids = append(ids, model.ID)
		}
		reg, _ := catalogs.registeredFor(keyIDFor(testKeyA), familyGo)
		if strings.Join(reg.ids, ",") != strings.Join(ids, ",") {
			t.Fatalf("recorded %v, registered %v", reg.ids, ids)
		}
		return strings.Join(ids, ",")
	}
	on := settings{BaseURL: defaultBaseURL, ModelPrefix: "go", DisabledModels: []string{"gpt-6-luna"}}
	if got := registered(on); got != "go/claude-haiku-5-5,go/deepseek-v4-flash,go/elsewhere,go/kimi-k3,go/longcat-2.5-preview-free,go/qwen3.8-max" {
		t.Fatalf("prefix on: %s", got)
	}
	off := on
	off.DisablePrefix = true
	if got := registered(off); got != "claude-haiku-5-5,deepseek-v4-flash,elsewhere,longcat-2.5-preview-free" {
		t.Fatalf("prefix off: %s", got)
	}
}

// Each family's auth registers its own models: the Go auth every Go model,
// the Zen auth the Zen models the page turned on.
func TestModelsForAuthPerFamily(t *testing.T) {
	useTestCatalog(t)
	cfg := defaultSettings()
	cfg.ZenModels = []string{"kimi-k3", "gpt-6-luna"}
	useSettings(t, cfg)
	auths := parseAuths(pluginapi.AuthParseRequest{FileName: "k.json", RawJSON: []byte(`{"type":"opencode-go","api_key":"` + testKeyA + `","zen":true}`)}).Auths
	if len(auths) != 2 {
		t.Fatalf("auths = %+v", auths)
	}
	for _, auth := range auths {
		raw, errHandle := handleMethod(pluginabi.MethodModelForAuth, mustJSON(t, rpcAuthModelRequest{AuthModelRequest: pluginapi.AuthModelRequest{
			AuthID: auth.ID, StorageJSON: auth.StorageJSON, Attributes: auth.Attributes,
		}}))
		if errHandle != nil {
			t.Fatal(errHandle)
		}
		family := authFamily(auth.Attributes)
		var ids []string
		for _, model := range decodeResult[pluginapi.ModelResponse](t, raw).Models {
			ids = append(ids, model.ID)
		}
		want := strings.Join(fixtureCatalog(t).exposedIDs(cfg, family), ",")
		if strings.Join(ids, ",") != want {
			t.Fatalf("%s registered %v, want %s", family, ids, want)
		}
		if reg, ok := catalogs.registeredFor(keyIDFor(testKeyA), family); !ok || strings.Join(reg.ids, ",") != want {
			t.Fatalf("%s recorded %+v", family, reg)
		}
	}
	if got, _ := catalogs.registeredFor(keyIDFor(testKeyA), familyZen); strings.Join(got.ids, ",") != "opencode/gpt-6-luna,opencode/kimi-k3" {
		t.Fatalf("zen registered %v", got.ids)
	}
}

func fixtureCatalog(t *testing.T) catalog {
	t.Helper()
	parsed, errParse := parseCatalog(readFixture(t, "config.json"))
	if errParse != nil {
		t.Fatal(errParse)
	}
	return parsed
}

// An auth CPA loaded with an earlier plugin version has no family; the
// plugin has CPA parse the files again, once per process.
func TestModelsForAuthReparsesAuthsOfAnEarlierVersion(t *testing.T) {
	useTestCatalog(t)
	useSettings(t, defaultSettings())
	reparses := 0
	previous := reparseLater
	reparseLater = func() { reparses++ }
	staleReparsed.Store(false)
	t.Cleanup(func() {
		reparseLater = previous
		staleReparsed.Store(false)
	})
	ask := func(attributes map[string]string) {
		t.Helper()
		if _, errHandle := handleMethod(pluginabi.MethodModelForAuth, mustJSON(t, rpcAuthModelRequest{AuthModelRequest: pluginapi.AuthModelRequest{
			StorageJSON: []byte(`{"type":"opencode-go","api_key":"` + testKeyA + `"}`),
			Attributes:  attributes,
		}})); errHandle != nil {
			t.Fatal(errHandle)
		}
	}
	ask(map[string]string{"path": "/auths/k.json", "family": "go"})
	ask(map[string]string{"key_id": keyIDFor(testKeyA)})
	if reparses != 0 {
		t.Fatalf("current auths caused %d reparses", reparses)
	}
	ask(map[string]string{"path": "/auths/k.json"})
	ask(map[string]string{"path": "/auths/other.json"})
	if reparses != 1 {
		t.Fatalf("%d reparses, want 1", reparses)
	}
}

func TestModelsForAuthFallsBackAndLogs(t *testing.T) {
	useSettings(t, defaultSettings())
	useCatalogStore(t)
	fake := installFakeHost(t)
	fake.handle(pluginabi.MethodHostHTTPDo, func(json.RawMessage) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: 503, Body: []byte("upstream down")}, nil
	})
	fake.handle(pluginabi.MethodHostLog, func(json.RawMessage) (any, error) { return struct{}{}, nil })
	req := mustJSON(t, rpcAuthModelRequest{AuthModelRequest: pluginapi.AuthModelRequest{
		StorageJSON: []byte(`{"type":"opencode-go","api_key":"` + testKeyA + `"}`),
	}})
	raw, _ := handleMethod(pluginabi.MethodModelForAuth, req)
	if resp := decodeResult[pluginapi.ModelResponse](t, raw); len(resp.Models) < 20 {
		t.Fatalf("snapshot fallback served %d models", len(resp.Models))
	}
	logs := fake.callsTo(pluginabi.MethodHostLog)
	if len(logs) != 1 || !strings.Contains(string(logs[0]), "snapshot") {
		t.Fatalf("logs = %s", logs)
	}
}

func TestModelsForAuthWithLoginKeysCacheByWorkspace(t *testing.T) {
	useSettings(t, defaultSettings())
	store := useCatalogStore(t)
	fake := installFakeHost(t)
	config := readFixture(t, "config.json")
	var bearers []string
	fake.handle(pluginabi.MethodHostHTTPDo, func(payload json.RawMessage) (any, error) {
		var req hostHTTPRequest
		_ = json.Unmarshal(payload, &req)
		bearers = append(bearers, req.Headers.Get("Authorization"))
		return pluginapi.HTTPResponse{StatusCode: 200, Body: config}, nil
	})
	models := func(login []byte) []pluginapi.ModelInfo {
		t.Helper()
		auth := parsedAuths("opencode-go-oauth-"+testOrgID+".json", string(login))[0]
		raw, _ := handleMethod(pluginabi.MethodModelForAuth, mustJSON(t, rpcAuthModelRequest{AuthModelRequest: pluginapi.AuthModelRequest{
			AuthID:      auth.ID,
			StorageJSON: hostStorageJSON(t, auth.StorageJSON, auth.Metadata),
			Attributes:  auth.Attributes,
			Metadata:    auth.Metadata,
		}}))
		return decodeResult[pluginapi.ModelResponse](t, raw).Models
	}
	if got := models(loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, nil)); len(got) != 7 {
		t.Fatalf("models = %d", len(got))
	}
	if _, cached := store.ownCatalog("login:" + testOrgID); !cached || len(bearers) != 1 || bearers[0] != "Bearer "+testAccessToken {
		t.Fatalf("cache %v after %v", cached, bearers)
	}
	if got := models(loginFileJSON(t, testAccessToken2, testRefreshToken2, testLoginExpiry, nil)); len(got) != 7 || len(bearers) != 1 {
		t.Fatalf("rotated tokens refetched: %v", bearers)
	}
	expired := loginFileJSON(t, testAccessToken2, testRefreshToken2, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), nil)
	if got := models(expired); len(got) != 7 || len(bearers) != 1 {
		t.Fatalf("expired login: %d models after %v", len(got), bearers)
	}
}

func TestModelsForAuthWithoutKeyReturnsNoModels(t *testing.T) {
	req := mustJSON(t, rpcAuthModelRequest{AuthModelRequest: pluginapi.AuthModelRequest{
		StorageJSON: []byte(`{"type":"opencode-go","keys":[]}`),
	}})
	raw, _ := handleMethod(pluginabi.MethodModelForAuth, req)
	if resp := decodeResult[pluginapi.ModelResponse](t, raw); resp.Provider != "opencode-go" || len(resp.Models) != 0 {
		t.Fatalf("resp = %+v", resp)
	}
}
