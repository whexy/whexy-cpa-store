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

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// pagePath is the page Management Center embeds under the "Usage" menu.
	// Resource routes are not authenticated, so the page holds no data.
	pagePath = "/v0/resource/plugins/" + pluginID + "/index.html"
	// logoPath is Metadata.Logo: Management Center shows it on the sidebar
	// entry and on the Plugins page.
	logoPath = "/v0/resource/plugins/" + pluginID + "/logo.svg"
	// summaryPath is the authenticated route the page reads.
	summaryPath = "/v0/management/" + pluginID + "/summary"
	// pricesPath reads and replaces the prices set on the page.
	pricesPath = "/v0/management/" + pluginID + "/prices"
	// quotaPath reads the quota samples behind the page's Quota section.
	quotaPath = "/v0/management/" + pluginID + "/quota"
)

//go:embed dashboard.html
var dashboardPage []byte

var dashboardCSP = pageCSP(dashboardPage)

// logoSVG is the plugin mark; ../logo.svg is the copy the store registry
// links to.
//
//go:embed logo.svg
var logoSVG []byte

type rpcManagementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

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

func managementRegister(_ []byte) ([]byte, error) {
	return okEnvelope(managementRegistration{
		Routes: []managementRoute{
			{
				Method:      http.MethodGet,
				Path:        summaryPath,
				Description: "Usage totals, per-group series and per-model rows: ?range=24h|7d|30d|90d&group=family|account&tz=<IANA zone>.",
			},
			{
				Method:      http.MethodGet,
				Path:        pricesPath,
				Description: "Every model in retained usage with its custom and models.dev price.",
			},
			{
				Method:      http.MethodGet,
				Path:        quotaPath,
				Description: "Accounts of every provider whose quota is recorded, with their readings: ?from=<RFC 3339 time>.",
			},
			{
				Method:      http.MethodPut,
				Path:        pricesPath,
				Description: `Replace the custom prices: {"models":{"<model>":{"input":..,"output":..,"cache_read":..,"cache_write":..}}}, USD per million tokens.`,
			},
		},
		Resources: []resourceRoute{
			{Path: pagePath, Menu: menuLabel, Description: "Token usage and estimated cost by model family or account, and the quota left in each provider's accounts."},
			{Path: logoPath, Description: displayName + " logo."},
		},
	})
}

func managementHandle(raw []byte) ([]byte, error) {
	var req rpcManagementRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode management request: %w", errUnmarshal)
	}
	return okEnvelope(routeManagement(req.ManagementRequest))
}

func routeManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	switch {
	case method == http.MethodGet && req.Path == pagePath:
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":            {"text/html; charset=utf-8"},
				"Cache-Control":           {"no-cache"},
				"Content-Security-Policy": {dashboardCSP},
				"X-Content-Type-Options":  {"nosniff"},
				"Referrer-Policy":         {"no-referrer"},
			},
			Body: dashboardPage,
		}
	case method == http.MethodGet && req.Path == logoPath:
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
	case method == http.MethodGet && req.Path == summaryPath:
		report, errReport := summary(reportQuery{
			Range:    req.Query.Get("range"),
			GroupBy:  req.Query.Get("group"),
			TimeZone: req.Query.Get("tz"),
		})
		if errReport != nil {
			return managementJSON(http.StatusBadRequest, map[string]string{"error": errReport.Error()})
		}
		return managementJSON(http.StatusOK, report)
	case method == http.MethodGet && req.Path == quotaPath:
		report, errReport := quotaReport(req.Query.Get("from"))
		if errReport != nil {
			return managementJSON(http.StatusBadRequest, map[string]string{"error": errReport.Error()})
		}
		return managementJSON(http.StatusOK, report)
	case method == http.MethodGet && req.Path == pricesPath:
		return priceListResponse()
	case method == http.MethodPut && req.Path == pricesPath:
		var body struct {
			Models map[string]customPriceInput `json:"models"`
		}
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil || body.Models == nil {
			return managementJSON(http.StatusBadRequest, map[string]string{"error": `body must be {"models": {"<model>": {"input": .., "output": ..}}}`})
		}
		models, errValidate := validateCustomPrices(body.Models)
		if errValidate != nil {
			return managementJSON(http.StatusBadRequest, map[string]string{"error": errValidate.Error()})
		}
		if errSave := customPrices.save(models); errSave != nil {
			return managementJSON(http.StatusInternalServerError, map[string]string{"error": errSave.Error()})
		}
		return priceListResponse()
	}
	return managementJSON(http.StatusNotFound, map[string]string{"error": "no " + displayName + " route " + method + " " + req.Path})
}

func priceListResponse() pluginapi.ManagementResponse {
	list, errList := priceList()
	if errList != nil {
		return managementJSON(http.StatusServiceUnavailable, map[string]string{"error": errList.Error()})
	}
	return managementJSON(http.StatusOK, list)
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

// pageCSP admits only the page's own inline scripts and style and requests
// to its own origin, so injected markup can neither run nor send the
// management key elsewhere. It sets no frame-ancestors because Management
// Center may be served from another origin than CLIProxyAPI.
func pageCSP(page []byte) string {
	return "default-src 'none'; script-src " + inlineHashes(page, "script") +
		"; style-src " + inlineHashes(page, "style") +
		"; connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'"
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
