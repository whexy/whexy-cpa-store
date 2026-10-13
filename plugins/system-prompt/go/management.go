package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// resourcePagePath is the page Management Center embeds under the
	// plugin's menu entry. Resource routes are not authenticated.
	resourcePagePath = "/v0/resource/plugins/" + pluginID + "/index.html"
	// logoPath is Metadata.Logo: Management Center shows it on the sidebar
	// entry and on the Plugins page.
	logoPath = "/v0/resource/plugins/" + pluginID + "/logo.svg"
	// managementBasePath prefixes the authenticated routes; the host
	// matches exact paths.
	managementBasePath = "/v0/management/" + pluginID
	statusPath         = managementBasePath + "/status"
)

// managementPage holds no data: it reads rules through the host's config API
// and what the plugin applied through the status route.
//
//go:embed management.html
var managementPage []byte

var managementPageCSP = pageCSP(managementPage)

// logoSVG is the plugin mark; ../logo.svg is the copy the store registry
// links to.
//
//go:embed logo.svg
var logoSVG []byte

type managementRegistration struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

type managementRoute struct {
	Method      string
	Path        string
	Description string
	handle      func(req pluginapi.ManagementRequest) (int, any)
}

type resourceRoute struct {
	Path        string
	Menu        string
	Description string
}

var managementRoutes = []managementRoute{
	{Method: http.MethodGet, Path: statusPath, Description: "Read the applied rules, the rules that do not apply and why, and the config generation.", handle: handleStatus},
}

func managementRegister() ([]byte, error) {
	return okEnvelope(managementRegistration{
		Routes: managementRoutes,
		Resources: []resourceRoute{
			{Path: resourcePagePath, Menu: displayName, Description: "Append a system prompt to requests for the models you choose."},
			{Path: logoPath, Description: displayName + " logo."},
		},
	})
}

func managementHandle(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode management request: %w", errUnmarshal)
	}
	return okEnvelope(routeManagement(req))
}

func routeManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == http.MethodGet && req.Path == resourcePagePath {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":            {"text/html; charset=utf-8"},
				"Cache-Control":           {"no-cache"},
				"Content-Security-Policy": {managementPageCSP},
				"X-Content-Type-Options":  {"nosniff"},
				"Referrer-Policy":         {"no-referrer"},
			},
			Body: managementPage,
		}
	}
	if method == http.MethodGet && req.Path == logoPath {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":            {"image/svg+xml"},
				"Cache-Control":           {"public, max-age=3600"},
				"Content-Security-Policy": {"default-src 'none'"},
				"X-Content-Type-Options":  {"nosniff"},
			},
			Body: logoSVG,
		}
	}
	for _, route := range managementRoutes {
		if route.Method == method && route.Path == req.Path {
			return managementJSON(route.handle(req))
		}
	}
	return managementJSON(http.StatusNotFound, map[string]string{"error": "no " + displayName + " route " + method + " " + req.Path})
}

func managementJSON(status int, body any) pluginapi.ManagementResponse {
	raw, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		status = http.StatusInternalServerError
		raw, _ = json.Marshal(map[string]string{"error": "encode response: " + errMarshal.Error()})
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":           {"application/json; charset=utf-8"},
			"Cache-Control":          {"no-store"},
			"X-Content-Type-Options": {"nosniff"},
		},
		Body: raw,
	}
}

// pageCSP admits only the page's own inline script and style and requests
// to its own origin, so injected markup can neither run nor send the
// management key elsewhere. It sets no frame-ancestors because Management
// Center may be served from another origin than CLIProxyAPI.
func pageCSP(page []byte) string {
	return "default-src 'none'; script-src " + inlineHash(page, "script") +
		"; style-src " + inlineHash(page, "style") +
		"; connect-src 'self'; base-uri 'none'; form-action 'none'"
}

func inlineHash(page []byte, tag string) string {
	open, end := []byte("<"+tag+">"), []byte("</"+tag+">")
	start, stop := bytes.Index(page, open), bytes.Index(page, end)
	if start < 0 || stop < start {
		return "'none'"
	}
	sum := sha256.Sum256(page[start+len(open) : stop])
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

type statusView struct {
	Generation uint64        `json:"generation"`
	AppliedAt  string        `json:"applied_at,omitempty"`
	Error      string        `json:"error,omitempty"`
	Rules      int           `json:"rules"`
	Problems   []ruleProblem `json:"problems"`
}

// handleStatus reports the configuration as the plugin applied it, so the
// page can tell a saved rule from one that is in effect.
func handleStatus(pluginapi.ManagementRequest) (int, any) {
	rules := currentRules()
	view := statusView{
		Generation: rules.generation,
		Error:      rules.err,
		Rules:      len(rules.rules),
		Problems:   rules.problems,
	}
	if view.Problems == nil {
		view.Problems = []ruleProblem{}
	}
	if !rules.appliedAt.IsZero() {
		view.AppliedAt = rules.appliedAt.Format(time.RFC3339)
	}
	return http.StatusOK, view
}
