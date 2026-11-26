package manage

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

const testPage = `<!doctype html>
<html><head><style>.own{}</style></head>
<body><script>const own = 1;</script><script>const dom = 2;</script></body></html>`

func newTestRouter() *Router {
	return NewRouter(Config{
		PluginID: "demo",
		Name:     "Demo",
		Page:     &Page{Menu: "Demo", Description: "A demo page.", HTML: []byte(testPage)},
		Logo:     []byte("<svg/>"),
		Routes: []Route{
			{Method: http.MethodGet, Path: APIPath("demo", "status"), Description: "Status.", Handle: func(Request) (int, any, error) {
				return http.StatusOK, map[string]int{"n": 1}, nil
			}},
			{Method: http.MethodPost, Path: APIPath("demo", "status"), Description: "Fail.", Handle: func(Request) (int, any, error) {
				return 0, nil, Errorf(http.StatusConflict, "busy %d", 2)
			}},
			{Method: http.MethodDelete, Path: APIPath("demo", "status"), Description: "Crash.", Handle: func(Request) (int, any, error) {
				return 0, nil, errors.New("boom")
			}},
		},
	})
}

func get(r *Router, method, path string) pluginapi.ManagementResponse {
	return r.Serve(Request{ManagementRequest: pluginapi.ManagementRequest{Method: method, Path: path}})
}

func TestRegisterWireShape(t *testing.T) {
	raw, errRegister := newTestRouter().Register()
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	var env struct {
		OK     bool
		Result struct {
			Routes    []map[string]any `json:"routes"`
			Resources []map[string]any `json:"resources"`
		}
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil || !env.OK {
		t.Fatalf("envelope %s: %v", raw, errUnmarshal)
	}
	if len(env.Result.Routes) != 3 || env.Result.Routes[0]["Path"] != "/v0/management/demo/status" || env.Result.Routes[0]["Method"] != "GET" {
		t.Errorf("routes = %v", env.Result.Routes)
	}
	if _, leaked := env.Result.Routes[0]["Handle"]; leaked {
		t.Error("route handler is serialized")
	}
	resources := env.Result.Resources
	if len(resources) != 2 || resources[0]["Path"] != "/v0/resource/plugins/demo/index.html" || resources[0]["Menu"] != "Demo" || resources[1]["Path"] != "/v0/resource/plugins/demo/logo.svg" {
		t.Errorf("resources = %v", resources)
	}
}

func TestServePageInlinesTheKitUnderItsCSP(t *testing.T) {
	r := newTestRouter()
	resp := get(r, "get", r.PagePath())
	if resp.StatusCode != http.StatusOK || resp.Headers.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("page %d %v", resp.StatusCode, resp.Headers)
	}
	body := string(resp.Body)
	if !strings.Contains(body, "globalThis.kit") || strings.Index(body, "--bg-page") > strings.Index(body, ".own{}") {
		t.Error("page lacks the kit ahead of its own blocks")
	}
	csp := resp.Headers.Get("Content-Security-Policy")
	if strings.Count(csp, "'sha256-") != 5 || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP %q, want a hash per inline block", csp)
	}
}

func TestServeLogo(t *testing.T) {
	r := newTestRouter()
	resp := get(r, http.MethodGet, r.LogoPath())
	if resp.StatusCode != http.StatusOK || string(resp.Body) != "<svg/>" || resp.Headers.Get("Content-Type") != "image/svg+xml" {
		t.Errorf("logo %d %v %s", resp.StatusCode, resp.Headers, resp.Body)
	}
}

func TestServeRoutes(t *testing.T) {
	r := newTestRouter()
	path := APIPath("demo", "/status")
	cases := []struct {
		method string
		path   string
		status int
		body   string
	}{
		{http.MethodGet, path, http.StatusOK, `{"n":1}`},
		{http.MethodPost, path, http.StatusConflict, `{"error":"busy 2"}`},
		{http.MethodDelete, path, http.StatusInternalServerError, `{"error":"boom"}`},
		{http.MethodPut, path, http.StatusMethodNotAllowed, `{"error":"PUT is not allowed on /v0/management/demo/status"}`},
		{http.MethodGet, path + "/x", http.StatusNotFound, `{"error":"no Demo route GET /v0/management/demo/status/x"}`},
	}
	for _, c := range cases {
		resp := get(r, c.method, c.path)
		if resp.StatusCode != c.status || string(resp.Body) != c.body {
			t.Errorf("%s %s = %d %s, want %d %s", c.method, c.path, resp.StatusCode, resp.Body, c.status, c.body)
		}
		if resp.StatusCode != http.StatusMethodNotAllowed && resp.Headers.Get("Cache-Control") != "no-store" {
			t.Errorf("%s %s is cacheable", c.method, c.path)
		}
	}
	if allow := get(r, http.MethodPut, path).Headers.Get("Allow"); allow != "GET, POST, DELETE" {
		t.Errorf("Allow = %q", allow)
	}
}

func TestHandleDecodesTheCallbackID(t *testing.T) {
	var seen string
	r := NewRouter(Config{PluginID: "demo", Routes: []Route{{Method: http.MethodGet, Path: "/p", Handle: func(req Request) (int, any, error) {
		seen = req.HostCallbackID
		return http.StatusNoContent, nil, nil
	}}}})
	raw, errHandle := r.Handle([]byte(`{"Method":"GET","Path":"/p","host_callback_id":"cb-1"}`))
	if errHandle != nil || seen != "cb-1" || !strings.Contains(string(raw), `"ok":true`) {
		t.Errorf("Handle = %s, %v; callback %q", raw, errHandle, seen)
	}
	if _, errBad := r.Handle([]byte("{")); errBad == nil {
		t.Error("undecodable request accepted")
	}
}

func TestJSONRedacts(t *testing.T) {
	rpc.SetRedactor(func(s string) string { return strings.ReplaceAll(s, "secret", "[redacted]") })
	t.Cleanup(func() { rpc.SetRedactor(nil) })
	if body := string(JSON(http.StatusOK, map[string]string{"k": "a secret"}).Body); body != `{"k":"a [redacted]"}` {
		t.Errorf("body = %s", body)
	}
}

func TestPageCSPWithoutInlineBlocks(t *testing.T) {
	if csp := PageCSP([]byte("<p>hi</p>")); !strings.Contains(csp, "script-src 'none'; style-src 'none'") {
		t.Errorf("CSP %q", csp)
	}
}
