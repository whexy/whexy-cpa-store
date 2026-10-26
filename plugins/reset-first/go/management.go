package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	_ "embed"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// resourcePagePath is the page Management Center embeds under the plugin's
	// sidebar entry. Resource routes are not authenticated.
	resourcePagePath = "/v0/resource/plugins/" + pluginID + "/index.html"
	// logoPath is Metadata.Logo: Management Center shows it on the sidebar
	// entry, on the Plugins page and in the store listing.
	logoPath = "/v0/resource/plugins/" + pluginID + "/logo.svg"
	// decisionsPath is authenticated: the cards name accounts by email.
	decisionsPath = "/v0/management/" + pluginID + "/decisions"
)

// managementPage holds no data: it reads the decisions route.
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
}

type resourceRoute struct {
	Path        string
	Menu        string
	Description string
}

func managementRegister() ([]byte, error) {
	return okEnvelope(managementRegistration{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: decisionsPath, Description: "Read the latest routing decision of every model routed in the past 24 hours."},
		},
		Resources: []resourceRoute{
			{Path: resourcePagePath, Menu: displayName, Description: "See which account each model was routed to and why."},
			{Path: logoPath, Description: displayName + " logo."},
		},
	})
}

func managementHandle(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode management request: %w", errUnmarshal)
	}
	return okEnvelope(routeManagement(req, time.Now()))
}

func routeManagement(req pluginapi.ManagementRequest, now time.Time) pluginapi.ManagementResponse {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method != http.MethodGet {
		return managementJSON(http.StatusMethodNotAllowed, map[string]string{"error": "only GET is served"})
	}
	switch req.Path {
	case resourcePagePath:
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
	case logoPath:
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
	case decisionsPath:
		return managementJSON(http.StatusOK, current.decisionsView(now))
	}
	return managementJSON(http.StatusNotFound, map[string]string{"error": "no " + pluginID + " route " + method + " " + req.Path})
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

// pageCSP admits only the page's own inline scripts and styles and requests to
// its own origin, so injected markup can neither run nor send the management
// key elsewhere. It sets no frame-ancestors because Management Center may be
// served from another origin than CLIProxyAPI.
func pageCSP(page []byte) string {
	return "default-src 'none'; script-src " + inlineHashes(page, "script") +
		"; style-src " + inlineHashes(page, "style") +
		"; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'"
}

// inlineHashes lists the hash of every <tag> block of the page.
func inlineHashes(page []byte, tag string) string {
	open, end := []byte("<"+tag+">"), []byte("</"+tag+">")
	var hashes []string
	for {
		start := bytes.Index(page, open)
		if start < 0 {
			break
		}
		page = page[start+len(open):]
		stop := bytes.Index(page, end)
		if stop < 0 {
			break
		}
		sum := sha256.Sum256(page[:stop])
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
		page = page[stop+len(end):]
	}
	if len(hashes) == 0 {
		return "'none'"
	}
	return strings.Join(hashes, " ")
}
