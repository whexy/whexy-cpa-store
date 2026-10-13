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
		t.Fatalf("ids = %v (disabled models must be skipped, opencode provider ignored)", ids)
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
		model, ok := cat.find(cfg, id)
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, ok := cat.find(tt.cfg, tt.model)
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
		if model, ok := cat.find(cfg, requested); !ok || model.UpstreamID != "kimi-k3" {
			t.Fatalf("%q -> %+v %v", requested, model, ok)
		}
	}
	for _, requested := range []string{"other/kimi-k3", "kimi-k9", "retired-model"} {
		if _, ok := cat.find(cfg, requested); ok {
			t.Fatalf("%q resolved", requested)
		}
	}
}

func TestModelInfos(t *testing.T) {
	cat, _ := parseCatalog(readFixture(t, "config.json"))
	infos := cat.modelInfos(defaultSettings())
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
	keyA := keyEntry{ID: keyIDFor(testKeyA), APIKey: testKeyA}
	keyB := keyEntry{ID: keyIDFor(testKeyB), APIKey: testKeyB}

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
	auths := parseAuths(pluginapi.AuthParseRequest{FileName: "opencode-go.json", RawJSON: multiKeyFile(t)}).Auths
	req := mustJSON(t, rpcAuthModelRequest{
		AuthModelRequest: pluginapi.AuthModelRequest{
			AuthID:       auths[1].ID,
			AuthProvider: "opencode-go",
			StorageJSON:  hostStorageJSON(t, auths[1].StorageJSON, nil),
			Attributes:   auths[1].Attributes,
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
	route, ok := lookupModel("opencode-go/gpt-6-luna", keyIDFor(testKeyB))
	if !ok || route.Protocol != sdktranslator.FormatOpenAIResponse || route.UpstreamID != "gpt-6-luna" {
		t.Fatalf("lookup = %+v %v", route, ok)
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

func TestModelsForAuthWithoutKeyReturnsNoModels(t *testing.T) {
	req := mustJSON(t, rpcAuthModelRequest{AuthModelRequest: pluginapi.AuthModelRequest{
		StorageJSON: []byte(`{"type":"opencode-go","keys":[]}`),
	}})
	raw, _ := handleMethod(pluginabi.MethodModelForAuth, req)
	if resp := decodeResult[pluginapi.ModelResponse](t, raw); resp.Provider != "opencode-go" || len(resp.Models) != 0 {
		t.Fatalf("resp = %+v", resp)
	}
}
