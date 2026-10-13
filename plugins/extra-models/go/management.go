package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// logoPath is Metadata.Logo: Management Center shows it on the Plugins page.
// Resource routes are not authenticated.
const logoPath = "/v0/resource/plugins/" + pluginID + "/logo.svg"

// logoSVG is the plugin mark; ../logo.svg is the copy the store registry
// links to.
//
//go:embed logo.svg
var logoSVG []byte

type managementRegistration struct {
	Resources []resourceRoute `json:"resources,omitempty"`
}

type resourceRoute struct {
	Path        string
	Description string
}

func managementRegister() ([]byte, error) {
	return okEnvelope(managementRegistration{
		Resources: []resourceRoute{{Path: logoPath, Description: displayName + " logo."}},
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
	if strings.EqualFold(req.Method, http.MethodGet) && req.Path == logoPath {
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
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusNotFound,
		Headers:    http.Header{"Content-Type": {"text/plain; charset=utf-8"}},
		Body:       []byte("not found\n"),
	}
}
