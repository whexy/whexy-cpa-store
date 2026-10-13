package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// resourcePagePath is the page Management Center embeds under the
	// "Client keys" menu. Resource routes are not authenticated.
	resourcePagePath = "/v0/resource/plugins/" + pluginID + "/index.html"
	// logoPath is Metadata.Logo: Management Center shows it on the sidebar
	// entry and on the Plugins page.
	logoPath = "/v0/resource/plugins/" + pluginID + "/logo.svg"
	// managementBasePath prefixes the authenticated routes; the host
	// matches exact paths.
	managementBasePath = "/v0/management/" + pluginID
	fingerprintsPath   = managementBasePath + "/fingerprints"
	modelsPath         = managementBasePath + "/models"
	statusPath         = managementBasePath + "/status"

	maxFingerprintKeys = 1000
)

// managementPage holds no data: it reads keys and policies through the
// authenticated routes and the host's config API.
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
	handle      func(req pluginapi.ManagementRequest) (int, any, error)
}

type resourceRoute struct {
	Path        string
	Menu        string
	Description string
}

var managementRoutes = []managementRoute{
	{Method: http.MethodGet, Path: statusPath, Description: "Read the applied client key policies, their errors and the config generation.", handle: handleStatus},
	{Method: http.MethodGet, Path: modelsPath, Description: "List the model IDs of the last unfiltered OpenAI model list.", handle: handleModels},
	{Method: http.MethodPost, Path: fingerprintsPath, Description: "Compute caller scopes and Management Center name fingerprints of client keys: {keys, api_base?}.", handle: handleFingerprints},
}

func managementRegister() ([]byte, error) {
	return okEnvelope(managementRegistration{
		Routes: managementRoutes,
		Resources: []resourceRoute{
			{Path: resourcePagePath, Menu: displayName, Description: "Set model and Responses API policies per client API key."},
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
			status, body, errHandle := route.handle(req)
			if errHandle != nil {
				var failure *apiError
				if errors.As(errHandle, &failure) {
					return managementJSON(failure.status, map[string]string{"error": failure.message})
				}
				return managementJSON(http.StatusInternalServerError, map[string]string{"error": errHandle.Error()})
			}
			return managementJSON(status, body)
		}
	}
	return managementJSON(http.StatusNotFound, map[string]string{"error": "no Client keys route " + method + " " + req.Path})
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

// apiError is a route failure shown to the user as is.
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return e.message }

func failf(status int, format string, args ...any) error {
	return &apiError{status: status, message: fmt.Sprintf(format, args...)}
}

type statusView struct {
	Generation          uint64                 `json:"generation"`
	AppliedAt           string                 `json:"applied_at,omitempty"`
	Error               string                 `json:"error,omitempty"`
	ResponseInterceptor bool                   `json:"response_interceptor"`
	Keys                map[string]entryStatus `json:"keys"`
}

// handleStatus reports the configuration as the plugin applied it, so the
// page can tell a saved config from an enforced one.
func handleStatus(pluginapi.ManagementRequest) (int, any, error) {
	rules := currentRules()
	view := statusView{
		Generation:          rules.generation,
		Error:               rules.err,
		ResponseInterceptor: rules.filtersModelLists,
		Keys:                rules.entries,
	}
	if !rules.appliedAt.IsZero() {
		view.AppliedAt = rules.appliedAt.Format(time.RFC3339)
	}
	return http.StatusOK, view, nil
}

type modelsView struct {
	Models     []string `json:"models"`
	CapturedAt *string  `json:"captured_at"`
}

func handleModels(pluginapi.ManagementRequest) (int, any, error) {
	captured := lastCatalog.Load()
	if captured == nil {
		return http.StatusOK, modelsView{Models: []string{}}, nil
	}
	at := captured.capturedAt.Format(time.RFC3339)
	return http.StatusOK, modelsView{Models: captured.models, CapturedAt: &at}, nil
}

type fingerprintsRequest struct {
	Keys    []string `json:"keys"`
	APIBase *string  `json:"api_base"`
}

type fingerprintView struct {
	Scope           string `json:"scope"`
	NameFingerprint string `json:"name_fingerprint,omitempty"`
}

// handleFingerprints lets the page name keys without the plugin storing
// them: the scope is what the config is keyed by, the name fingerprint is
// how Management Center stores key names in the browser. Error messages
// never quote a key.
func handleFingerprints(req pluginapi.ManagementRequest) (int, any, error) {
	if len(bytes.TrimSpace(req.Body)) == 0 {
		return 0, nil, failf(http.StatusBadRequest, "The request body is empty.")
	}
	var body fingerprintsRequest
	if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
		return 0, nil, failf(http.StatusBadRequest, `The request body must be {"keys": ["..."], "api_base": "..."}.`)
	}
	if body.Keys == nil {
		return 0, nil, failf(http.StatusBadRequest, "keys is required.")
	}
	if len(body.Keys) > maxFingerprintKeys {
		return 0, nil, failf(http.StatusBadRequest, "At most %d keys per request.", maxFingerprintKeys)
	}
	views := make([]fingerprintView, 0, len(body.Keys))
	for index, key := range body.Keys {
		scope := session.CallerScope(key)
		if scope == "" {
			return 0, nil, failf(http.StatusBadRequest, "keys[%d] is empty.", index)
		}
		view := fingerprintView{Scope: scope}
		if body.APIBase != nil {
			view.NameFingerprint = nameFingerprint(*body.APIBase, key)
		}
		views = append(views, view)
	}
	return http.StatusOK, map[string]any{"fingerprints": views}, nil
}

// nameFingerprint is Management Center's apiKeyNameFingerprint:
// hex(sha256(JSON.stringify(["api-key-name", apiBase, key]))).
func nameFingerprint(apiBase, key string) string {
	sum := sha256.Sum256(jsonStringify([]string{"api-key-name", apiBase, key}))
	return hex.EncodeToString(sum[:])
}

// jsonStringify encodes strings as JavaScript's JSON.stringify does. Go
// additionally escapes <, >, & (turned off here) and U+2028 and U+2029
// (undone here).
func jsonStringify(values []string) []byte {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(values)
	encoded := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))

	// Escapes are consumed whole, so a key's own backslash followed by
	// "u2028" stays as it is.
	out := make([]byte, 0, len(encoded))
	for index := 0; index < len(encoded); index++ {
		rest := encoded[index:]
		switch {
		case rest[0] != '\\':
			out = append(out, rest[0])
		case bytes.HasPrefix(rest, []byte(`\u2028`)):
			out = append(out, "\u2028"...)
			index += 5
		case bytes.HasPrefix(rest, []byte(`\u2029`)):
			out = append(out, "\u2029"...)
			index += 5
		default:
			out = append(out, rest[0], rest[1])
			index++
		}
	}
	return out
}
