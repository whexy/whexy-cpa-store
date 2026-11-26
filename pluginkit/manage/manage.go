// Package manage serves a plugin's Management API routes and its browser
// resources: the page Management Center embeds under the plugin's menu entry
// and the plugin's logo.
package manage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/host"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
	"github.com/whexy/whexy-cpa-store/pluginkit/ui"
)

// APIPath returns the authenticated Management API path of sub under the
// plugin's prefix, e.g. APIPath("zed", "accounts"). The host matches exact
// paths.
func APIPath(pluginID, sub string) string {
	return "/v0/management/" + pluginID + "/" + strings.TrimPrefix(sub, "/")
}

// ResourcePath returns the browser path of the plugin resource name.
// Resource routes are not authenticated.
func ResourcePath(pluginID, name string) string {
	return "/v0/resource/plugins/" + pluginID + "/" + name
}

// Request is a management.handle call.
type Request struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// Host returns a host client bound to the request's callback ID.
func (r Request) Host() host.Client { return host.New(r.HostCallbackID) }

// Handler answers one route with a status and a JSON body. An *Error sets the
// status of a failure; any other error is a 500.
type Handler func(req Request) (status int, body any, err error)

// Route is one authenticated Management API route.
type Route struct {
	Method      string
	Path        string
	Description string
	Handle      Handler `json:"-"`
}

// Error is a handler failure with the HTTP status it answers with.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// Errorf returns an *Error with status and a formatted message.
func Errorf(status int, format string, args ...any) error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...)}
}

// Page is the plugin's Management Center page.
type Page struct {
	// Menu is the sidebar label Management Center shows for the page.
	Menu string
	// Description explains the page on the Plugins page.
	Description string
	// HTML is the page source; the kit's stylesheet and script are inlined
	// into it when the router is built.
	HTML []byte
}

// Config describes what a Router serves.
type Config struct {
	PluginID string
	// Name is the plugin's display name, used in descriptions and errors.
	Name   string
	Page   *Page
	Logo   []byte
	Routes []Route
}

// Router answers management.register and management.handle.
type Router struct {
	cfg      Config
	pagePath string
	logoPath string
	page     []byte
	pageCSP  string
	// fonts maps the resource path of each kit font to its file.
	fonts map[string][]byte
}

// NewRouter builds a router. It assembles the page once, so build routers
// at package initialization.
func NewRouter(cfg Config) *Router {
	r := &Router{
		cfg:      cfg,
		pagePath: ResourcePath(cfg.PluginID, "index.html"),
		logoPath: ResourcePath(cfg.PluginID, "logo.svg"),
	}
	if cfg.Page != nil {
		r.page = ui.Page(cfg.Page.HTML)
		r.pageCSP = PageCSP(r.page)
		r.fonts = kitFonts(cfg.PluginID)
	}
	return r
}

func kitFonts(pluginID string) map[string][]byte {
	fonts := map[string][]byte{}
	files, _ := fs.Glob(ui.Fonts, "fonts/*.woff2")
	for _, file := range files {
		raw, errRead := fs.ReadFile(ui.Fonts, file)
		if errRead != nil {
			panic("manage: read embedded " + file + ": " + errRead.Error())
		}
		fonts[ResourcePath(pluginID, file)] = raw
	}
	return fonts
}

// PagePath is the browser path of the page.
func (r *Router) PagePath() string { return r.pagePath }

// LogoPath is the browser path of the logo, which Metadata.Logo names.
func (r *Router) LogoPath() string { return r.logoPath }

// fontPaths lists the font resources in a stable order.
func (r *Router) fontPaths() []string {
	paths := make([]string, 0, len(r.fonts))
	for fontPath := range r.fonts {
		paths = append(paths, fontPath)
	}
	slices.Sort(paths)
	return paths
}

// PageHTML is the page as served, with the kit inlined.
func (r *Router) PageHTML() []byte { return r.page }

type resource struct {
	Path        string
	Menu        string `json:",omitempty"`
	Description string
}

type registration struct {
	Routes    []Route    `json:"routes,omitempty"`
	Resources []resource `json:"resources,omitempty"`
}

// Register answers management.register.
func (r *Router) Register() ([]byte, error) {
	reg := registration{Routes: r.cfg.Routes}
	if r.cfg.Page != nil {
		reg.Resources = append(reg.Resources, resource{Path: r.pagePath, Menu: r.cfg.Page.Menu, Description: r.cfg.Page.Description})
	}
	if r.cfg.Logo != nil {
		reg.Resources = append(reg.Resources, resource{Path: r.logoPath, Description: r.cfg.Name + " logo."})
	}
	for _, fontPath := range r.fontPaths() {
		reg.Resources = append(reg.Resources, resource{Path: fontPath, Description: "Font of the " + r.cfg.Name + " page: " + path.Base(fontPath) + "."})
	}
	return rpc.OK(reg)
}

// Handle answers management.handle.
func (r *Router) Handle(raw []byte) ([]byte, error) {
	var req Request
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode management request: %w", errUnmarshal)
	}
	return rpc.OK(r.Serve(req))
}

// Serve answers one request.
func (r *Router) Serve(req Request) pluginapi.ManagementResponse {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == http.MethodGet && r.page != nil && req.Path == r.pagePath {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":            {"text/html; charset=utf-8"},
				"Cache-Control":           {"no-cache"},
				"Content-Security-Policy": {r.pageCSP},
				"X-Content-Type-Options":  {"nosniff"},
				"Referrer-Policy":         {"no-referrer"},
			},
			Body: r.page,
		}
	}
	if method == http.MethodGet && r.cfg.Logo != nil && req.Path == r.logoPath {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":            {"image/svg+xml"},
				"Cache-Control":           {"public, max-age=3600"},
				"Content-Security-Policy": {"default-src 'none'"},
				"X-Content-Type-Options":  {"nosniff"},
			},
			Body: r.cfg.Logo,
		}
	}
	if font, ok := r.fonts[req.Path]; ok && method == http.MethodGet {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":            {"font/woff2"},
				"Cache-Control":           {"public, max-age=86400"},
				"Content-Security-Policy": {"default-src 'none'"},
				"X-Content-Type-Options":  {"nosniff"},
			},
			Body: font,
		}
	}
	var allowed []string
	for _, route := range r.cfg.Routes {
		if route.Path != req.Path {
			continue
		}
		if route.Method == method {
			return respond(route.Handle(req))
		}
		allowed = append(allowed, route.Method)
	}
	if len(allowed) > 0 {
		resp := JSON(http.StatusMethodNotAllowed, errorBody(method+" is not allowed on "+req.Path))
		resp.Headers.Set("Allow", strings.Join(allowed, ", "))
		return resp
	}
	return JSON(http.StatusNotFound, errorBody("no "+r.cfg.Name+" route "+method+" "+req.Path))
}

func respond(status int, body any, errHandle error) pluginapi.ManagementResponse {
	if errHandle == nil {
		return JSON(status, body)
	}
	var failure *Error
	if errors.As(errHandle, &failure) {
		return JSON(failure.Status, errorBody(failure.Message))
	}
	return JSON(http.StatusInternalServerError, errorBody(errHandle.Error()))
}

// errorBody redacts message before JSON escapes it, because a redactor that
// matches JSON fields would not match their escaped quotes.
func errorBody(message string) map[string]string {
	return map[string]string{"error": rpc.Redact(message)}
}

// JSON encodes body as an uncacheable JSON response. The encoded body passes
// through the installed redactor.
func JSON(status int, body any) pluginapi.ManagementResponse {
	raw, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		status = http.StatusInternalServerError
		raw, _ = json.Marshal(errorBody("encode response: " + errMarshal.Error()))
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":           {"application/json; charset=utf-8"},
			"Cache-Control":          {"no-store"},
			"X-Content-Type-Options": {"nosniff"},
		},
		Body: []byte(rpc.Redact(string(raw))),
	}
}
