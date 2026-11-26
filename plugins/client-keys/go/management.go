package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/whexy/whexy-cpa-store/pluginkit/manage"
)

const maxFingerprintKeys = 1000

// managementPage holds no data: it reads keys and policies through the
// authenticated routes and the host's config API.
//
//go:embed management.html
var managementPage []byte

// logoSVG is the plugin mark; ../logo.svg is the copy the store registry
// links to.
//
//go:embed logo.svg
var logoSVG []byte

var (
	fingerprintsPath = manage.APIPath(pluginID, "fingerprints")
	modelsPath       = manage.APIPath(pluginID, "models")
	statusPath       = manage.APIPath(pluginID, "status")
)

var router = manage.NewRouter(manage.Config{
	PluginID: pluginID,
	Name:     displayName,
	Page:     &manage.Page{Menu: displayName, Description: "Set model and Responses API policies per client API key.", HTML: managementPage},
	Logo:     logoSVG,
	Routes: []manage.Route{
		{Method: http.MethodGet, Path: statusPath, Description: "Read the applied client key policies, their errors and the config generation.", Handle: handleStatus},
		{Method: http.MethodGet, Path: modelsPath, Description: "List the model IDs of the last unfiltered OpenAI model list.", Handle: handleModels},
		{Method: http.MethodPost, Path: fingerprintsPath, Description: "Compute caller scopes and Management Center name fingerprints of client keys: {keys, api_base?}.", Handle: handleFingerprints},
	},
})

type statusView struct {
	Generation          uint64                 `json:"generation"`
	AppliedAt           string                 `json:"applied_at,omitempty"`
	Error               string                 `json:"error,omitempty"`
	ResponseInterceptor bool                   `json:"response_interceptor"`
	Keys                map[string]entryStatus `json:"keys"`
}

// handleStatus reports the configuration as the plugin applied it, so the
// page can tell a saved config from an enforced one.
func handleStatus(manage.Request) (int, any, error) {
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

func handleModels(manage.Request) (int, any, error) {
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
func handleFingerprints(req manage.Request) (int, any, error) {
	if len(bytes.TrimSpace(req.Body)) == 0 {
		return 0, nil, manage.Errorf(http.StatusBadRequest, "The request body is empty.")
	}
	var body fingerprintsRequest
	if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
		return 0, nil, manage.Errorf(http.StatusBadRequest, `The request body must be {"keys": ["..."], "api_base": "..."}.`)
	}
	if body.Keys == nil {
		return 0, nil, manage.Errorf(http.StatusBadRequest, "keys is required.")
	}
	if len(body.Keys) > maxFingerprintKeys {
		return 0, nil, manage.Errorf(http.StatusBadRequest, "At most %d keys per request.", maxFingerprintKeys)
	}
	views := make([]fingerprintView, 0, len(body.Keys))
	for index, key := range body.Keys {
		scope := session.CallerScope(key)
		if scope == "" {
			return 0, nil, manage.Errorf(http.StatusBadRequest, "keys[%d] is empty.", index)
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
