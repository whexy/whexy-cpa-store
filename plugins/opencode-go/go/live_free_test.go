//go:build live

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The OpenCode Free tests need no key: they read the public catalogs and
// send one tiny request ("Reply with OK", at most 32 output tokens) with the
// key OpenCode's own client sends without an account.

// liveFreeCatalog reads models.dev and the gateway's live model list.
func liveFreeCatalog(t *testing.T) catalog {
	t.Helper()
	doc, errDoc := fetchModelsDev(context.Background(), netHTTPDoer{}, defaultSettings())
	if errDoc != nil {
		t.Fatal(errDoc)
	}
	cat, errFetch := fetchFreeCatalog(context.Background(), netHTTPDoer{}, defaultSettings(), doc)
	if errFetch != nil {
		t.Fatal(errFetch)
	}
	return cat
}

func TestLiveFreeCatalog(t *testing.T) {
	cat := liveFreeCatalog(t)
	if len(cat.Free) == 0 {
		t.Fatal("no free model")
	}
	for _, model := range cat.Free {
		route := model.route(defaultSettings())
		if !model.Free || !strings.HasPrefix(route.Endpoint, defaultZenBaseURL+"/") || strings.HasPrefix(route.Endpoint, defaultBaseURL+"/") {
			t.Fatalf("%s routes to %s", model.UpstreamID, route.Endpoint)
		}
	}
	t.Logf("%d free models: %s", len(cat.Free), freeIDs(cat))
}

// liveFreeGateway forwards the executor's requests to the pay-as-you-go
// gateway, refusing anything that carries a credential, which a free
// request must not.
func liveFreeGateway(t *testing.T) func(req hostHTTPRequest) gatewayReply {
	return func(req hostHTTPRequest) gatewayReply {
		if !strings.HasPrefix(req.URL, defaultZenBaseURL+"/") || strings.HasPrefix(req.URL, defaultBaseURL+"/") {
			t.Errorf("refusing %s", req.URL)
			return gatewayReply{Status: 599}
		}
		if got := req.Headers.Get("Authorization"); got != "Bearer public" {
			t.Errorf("refusing a request with Authorization %q", got)
			return gatewayReply{Status: 599}
		}
		resp, errDo := netHTTPDoer{}.Do(context.Background(), pluginapi.HTTPRequest{Method: req.Method, URL: req.URL, Headers: req.Headers, Body: req.Body})
		if errDo != nil {
			t.Errorf("%s: %v", req.URL, errDo)
			return gatewayReply{Status: 599}
		}
		return gatewayReply{Status: resp.StatusCode, Headers: resp.Headers, Body: resp.Body}
	}
}

func TestLiveExecutorFreeWithoutKey(t *testing.T) {
	useSettings(t, defaultSettings())
	useCatalogStore(t)
	store := useFreeCatalog(t)
	cat := liveFreeCatalog(t)
	store.entry = &catalogEntry{catalog: cat, fetchedAt: time.Now()}
	var model string
	for _, candidate := range cat.Free {
		if candidate.Protocol == "openai" {
			model = freeModelPrefix + "/" + candidate.UpstreamID
			break
		}
	}
	if model == "" {
		t.Skip("no free Chat Completions model")
	}
	installFakeGateway(t, liveFreeGateway(t))
	raw, _ := executorExecute(testCall{Source: "openai", Model: model, Payload: clientPayload("openai", model, false)}.raw(t))
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("%s: %+v", model, env.Error)
	}
	resp := decodeResult[pluginapi.ExecutorResponse](t, raw)
	got := parseClientResponse(t, "openai", resp.Payload)
	if strings.TrimSpace(got.Text+got.Reasoning) == "" {
		t.Fatalf("%s answered nothing: %s", model, resp.Payload)
	}
	t.Logf("%s answered %q", model, got.Text)
}
