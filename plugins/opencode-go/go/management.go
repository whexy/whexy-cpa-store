package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// resourcePagePath is the key management page Management Center embeds
	// under the "OpenCode Go" menu. Resource routes are not authenticated.
	resourcePagePath = "/v0/resource/plugins/" + providerKey + "/index.html"
	// logoPath is the plugin's Metadata.Logo. Management Center resolves a
	// path against its API base and shows it in the sidebar, on the OAuth
	// Login tile and on the Plugins page.
	logoPath = "/v0/resource/plugins/" + providerKey + "/logo.svg"
	// managementBasePath prefixes the authenticated key routes; the host
	// matches exact paths, so IDs travel in the query or body.
	managementBasePath = "/v0/management/" + providerKey
	keysPath           = managementBasePath + "/keys"
	loginsPath         = managementBasePath + "/logins"
	modelsPath         = managementBasePath + "/models"
	routingPath        = managementBasePath + "/routing"
	freePath           = managementBasePath + "/free"

	maxLabelLength = 64
)

// managementPage holds no data: everything it shows comes from the
// authenticated key routes.
//
//go:embed management.html
var managementPage []byte

var managementPageCSP = pageCSP(managementPage)

// logoSVG is the OpenCode mark; ../logo.svg is the copy the store registry
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
	handle      func(h host, req pluginapi.ManagementRequest) (int, any, error)
}

type resourceRoute struct {
	Path        string
	Menu        string
	Description string
}

// Key routes name keys by their key ID, which is derived from the key and
// stays fixed; an auth_index shifts when an earlier key is removed. Login
// routes name a login by its file name.
var managementRoutes = []managementRoute{
	{Method: http.MethodGet, Path: keysPath, Description: "List OpenCode Go keys: masked key, label, state, workspace.", handle: handleListKeys},
	{Method: http.MethodPost, Path: keysPath, Description: "Check and add an OpenCode Go key: {label, api_key}.", handle: handleAddKey},
	{Method: http.MethodPatch, Path: keysPath, Description: "Rename, enable or disable an OpenCode Go key, or turn OpenCode Go or Zen on or off for it: {id, label?, disabled?, go?, zen?}.", handle: handleUpdateKey},
	{Method: http.MethodDelete, Path: keysPath, Description: "Remove an OpenCode Go key: ?id=.", handle: handleDeleteKey},
	{Method: http.MethodPost, Path: keysPath + "/test", Description: "Check a stored OpenCode Go key against the gateway: {id}.", handle: handleTestKey},
	{Method: http.MethodGet, Path: keysPath + "/usage", Description: "Read the 5-hour, weekly and monthly usage of an OpenCode Go key: ?id=.", handle: handleKeyUsage},
	{Method: http.MethodGet, Path: loginsPath, Description: "List OpenCode logins: workspace, account, token expiry, state.", handle: handleListLogins},
	{Method: http.MethodPatch, Path: loginsPath, Description: "Enable or disable an OpenCode login, or turn OpenCode Go or Zen on or off for it: {id, disabled?, go?, zen?}.", handle: handleUpdateLogin},
	{Method: http.MethodGet, Path: loginsPath + "/usage", Description: "Read the 5-hour, weekly and monthly usage of an OpenCode login: ?id=.", handle: handleLoginUsage},
	{Method: http.MethodGet, Path: modelsPath, Description: "List the OpenCode Go and Zen models of the first working login or key, as models.dev and its workspace config describe them, and how they differ from the models CLIProxyAPI registered.", handle: handleListModels},
	{Method: http.MethodPost, Path: modelsPath + "/refresh", Description: "Read every catalog again and make CLIProxyAPI register the current model lists.", handle: handleRefreshModels},
	{Method: http.MethodGet, Path: routingPath, Description: "Read the OpenCode Go and OpenCode Zen priority and weight of every OpenCode Go auth file.", handle: handleGetRouting},
	{Method: http.MethodPut, Path: routingPath, Description: "Set the OpenCode Go, Zen and Free priority and weight: {go: {priority, weight}, zen: {...}, free: {...}}, null for the default.", handle: handleSetRouting},
	{Method: http.MethodGet, Path: freePath, Description: "Read OpenCode Free: its auth file, state and free models.", handle: handleGetFree},
	{Method: http.MethodPut, Path: freePath, Description: "Turn OpenCode Free on or off: {enabled}.", handle: handlePutFree},
}

func managementRegister(raw []byte) ([]byte, error) {
	return okEnvelope(managementRegistration{
		Routes: managementRoutes,
		Resources: []resourceRoute{
			{Path: resourcePagePath, Menu: displayName, Description: "Manage OpenCode Go logins, keys, models, OpenCode Free and routing."},
			{Path: logoPath, Description: "OpenCode Go logo."},
		},
	})
}

func managementHandle(raw []byte) ([]byte, error) {
	var req rpcManagementRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode management request: %w", errUnmarshal)
	}
	return okEnvelope(routeManagement(newHost(req.HostCallbackID), req.ManagementRequest))
}

func routeManagement(h host, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
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
			status, body, errHandle := route.handle(h, req)
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
	return managementJSON(http.StatusNotFound, map[string]string{"error": "no OpenCode Go route " + method + " " + req.Path})
}

// managementJSON encodes a key route response. Every body passes through
// redactSecrets, so no response can carry a full key.
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
		Body: []byte(redactSecrets(string(raw))),
	}
}

// pageCSP admits only the page's own inline scripts and styles and requests
// to its own origin, so injected markup can neither run nor send the
// management key elsewhere. It sets no frame-ancestors because Management
// Center may be served from another origin than CLIProxyAPI.
func pageCSP(page []byte) string {
	return "default-src 'none'; script-src " + inlineHashes(page, "script") +
		"; style-src " + inlineHashes(page, "style") +
		"; connect-src 'self'; base-uri 'none'; form-action 'none'"
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

// apiError is a key route failure shown to the user as is.
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return e.message }

func failf(status int, format string, args ...any) error {
	return &apiError{status: status, message: fmt.Sprintf(format, args...)}
}

type keyView struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	MaskedKey string `json:"masked_key"`
	Disabled  bool   `json:"disabled"`
	AddedAt   string `json:"added_at,omitempty"`
	File      string `json:"file"`
	Workspace string `json:"workspace,omitempty"`
	Models    int    `json:"models,omitempty"`
	// Go and Zen say which families the key serves; Runtime and ZenRuntime
	// are the states of their auths.
	Go         bool         `json:"go"`
	Zen        bool         `json:"zen"`
	Runtime    *runtimeView `json:"runtime,omitempty"`
	ZenRuntime *runtimeView `json:"zen_runtime,omitempty"`
}

// runtimeView is CLIProxyAPI's routing state of one of a credential's auths.
type runtimeView struct {
	State          string `json:"state"`
	Message        string `json:"message,omitempty"`
	Unavailable    bool   `json:"unavailable,omitempty"`
	NextRetryAfter string `json:"next_retry_after,omitempty"`
	Success        int64  `json:"success"`
	Failed         int64  `json:"failed"`
}

type usageView struct {
	Windows []usageWindowView `json:"windows"`
}

type usageWindowView struct {
	Name        string  `json:"name"`
	Label       string  `json:"label"`
	UsedPercent float64 `json:"used_percent"`
	Status      string  `json:"status,omitempty"`
	Exhausted   bool    `json:"exhausted"`
	ResetsAt    string  `json:"resets_at,omitempty"`
}

func newKeyView(file storedKeyFile, entry keyEntry) keyView {
	view := keyView{
		ID:        entry.ID,
		Label:     entry.Label,
		MaskedKey: maskKey(entry.APIKey),
		Disabled:  entry.Disabled,
		AddedAt:   entry.AddedAt,
		File:      file.Name,
		Go:        entry.families().Go,
		Zen:       entry.families().Zen,
	}
	if cat, ok := catalogs.ownCatalog(entry.ID); ok {
		view.Workspace = cat.Workspace
		view.Models = cat.servedCount(currentSettings(), entry.families())
	}
	if auth, ok := file.runtimeAuth(entry.ID, familyGo); ok {
		view.Runtime = newRuntimeView(auth)
	}
	if auth, ok := file.runtimeAuth(entry.ID, familyZen); ok {
		view.ZenRuntime = newRuntimeView(auth)
	}
	return view
}

func newRuntimeView(auth pluginapi.HostAuthFileEntry) *runtimeView {
	view := &runtimeView{
		State:       auth.Status,
		Message:     auth.StatusMessage,
		Unavailable: auth.Unavailable,
		Success:     auth.Success,
		Failed:      auth.Failed,
	}
	if !auth.NextRetryAfter.IsZero() {
		view.NextRetryAfter = auth.NextRetryAfter.UTC().Format(time.RFC3339)
	}
	return view
}

// loginView is a login as the page shows it; it never carries a token.
type loginView struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace"`
	OrgID     string `json:"org_id"`
	Email     string `json:"email,omitempty"`
	Disabled  bool   `json:"disabled"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Expired   bool   `json:"expired,omitempty"`
	Models    int    `json:"models,omitempty"`
	// Go and Zen say which families the login serves; Runtime and
	// ZenRuntime are the states of their auths.
	Go         bool         `json:"go"`
	Zen        bool         `json:"zen"`
	Runtime    *runtimeView `json:"runtime,omitempty"`
	ZenRuntime *runtimeView `json:"zen_runtime,omitempty"`
}

func newLoginView(stored storedLogin) loginView {
	login := logins.current(stored.Login)
	view := loginView{
		ID:        stored.Name,
		Workspace: login.displayName(),
		OrgID:     login.OrgID,
		Email:     login.Email,
		Disabled:  login.Disabled,
		Expired:   login.expired(time.Now()),
		Go:        login.Go,
		Zen:       login.Zen,
	}
	if !login.ExpiresAt.IsZero() {
		view.ExpiresAt = login.ExpiresAt.Format(time.RFC3339)
	}
	if cat, ok := catalogs.ownCatalog(login.credential().ID); ok {
		view.Models = cat.servedCount(currentSettings(), login.families())
	}
	if stored.Auth.AuthIndex != "" {
		view.Runtime = newRuntimeView(stored.Auth)
	}
	if stored.ZenAuth.AuthIndex != "" {
		view.ZenRuntime = newRuntimeView(stored.ZenAuth)
	}
	return view
}

func newUsageView(report usageReport) usageView {
	windows := make([]usageWindowView, 0, len(report.Windows))
	for _, window := range report.Windows {
		label := window.Name
		if names, ok := quotaBucketNames[window.Name]; ok {
			label = names[1]
		}
		view := usageWindowView{
			Name:        window.Name,
			Label:       label,
			UsedPercent: window.Percent,
			Status:      window.Status,
			Exhausted:   window.isExhausted(),
		}
		if !window.ResetsAt.IsZero() {
			view.ResetsAt = window.ResetsAt.Format(time.RFC3339)
		}
		windows = append(windows, view)
	}
	return usageView{Windows: windows}
}

func handleListKeys(host, pluginapi.ManagementRequest) (int, any, error) {
	files, errLoad := loadKeyFiles()
	if errLoad != nil {
		return 0, nil, failf(http.StatusInternalServerError, "Could not read the OpenCode Go auth files: %v", errLoad)
	}
	keys := []keyView{}
	for _, file := range files {
		for _, entry := range file.File.Keys {
			keys = append(keys, newKeyView(file, entry))
		}
	}
	return http.StatusOK, map[string]any{"keys": keys}, nil
}

type addKeyRequest struct {
	Label  string `json:"label"`
	APIKey string `json:"api_key"`
}

func handleAddKey(h host, req pluginapi.ManagementRequest) (int, any, error) {
	var body addKeyRequest
	if errDecode := decodeBody(req.Body, &body); errDecode != nil {
		return 0, nil, errDecode
	}
	apiKey, errKey := validateNewKey(body.APIKey)
	if errKey != nil {
		return 0, nil, errKey
	}
	label, errLabel := cleanLabel(body.Label)
	if errLabel != nil {
		return 0, nil, errLabel
	}
	files, errLoad := loadKeyFiles()
	if errLoad != nil {
		return 0, nil, failf(http.StatusInternalServerError, "Could not read the OpenCode Go auth files: %v", errLoad)
	}
	id := keyIDFor(apiKey)
	if _, existing, found := locateKey(files, id); found {
		return 0, nil, failf(http.StatusConflict, "This key is already added as %q.", existing.displayLabel())
	}
	check, errCheck := checkKey(h, apiKey)
	if errCheck != nil {
		return 0, nil, errCheck
	}
	catalogs.remember(id, check.Config)
	errSave := mutateKeyFile(keyFileName, func(file *keyFile) error {
		_, errAdd := file.addKey(apiKey, label, time.Now())
		return errAdd
	})
	if errors.Is(errSave, errKeyExists) {
		return 0, nil, failf(http.StatusConflict, "This key is already added.")
	}
	if errSave != nil {
		return 0, nil, failf(http.StatusInternalServerError, "Could not save %s: %v", keyFileName, errSave)
	}
	return http.StatusCreated, map[string]any{"key": storedKeyView(id), "usage": newUsageView(check.Usage)}, nil
}

type updateKeyRequest struct {
	ID       string  `json:"id"`
	Label    *string `json:"label"`
	Disabled *bool   `json:"disabled"`
	Go       *bool   `json:"go"`
	Zen      *bool   `json:"zen"`
}

func handleUpdateKey(_ host, req pluginapi.ManagementRequest) (int, any, error) {
	var body updateKeyRequest
	if errDecode := decodeBody(req.Body, &body); errDecode != nil {
		return 0, nil, errDecode
	}
	if body.Label == nil && body.Disabled == nil && body.Go == nil && body.Zen == nil {
		return 0, nil, failf(http.StatusBadRequest, "Nothing to change: send label, disabled, go or zen.")
	}
	patch := keyPatch{Disabled: body.Disabled, Go: body.Go, Zen: body.Zen}
	if body.Label != nil {
		label, errLabel := cleanLabel(*body.Label)
		if errLabel != nil {
			return 0, nil, errLabel
		}
		patch.Label = &label
	}
	file, _, errFind := findStoredKey(body.ID)
	if errFind != nil {
		return 0, nil, errFind
	}
	errSave := mutateKeyFile(file.Name, func(stored *keyFile) error {
		_, errUpdate := stored.updateKey(body.ID, patch)
		return errUpdate
	})
	if errSave != nil {
		return 0, nil, saveFailure(file.Name, errSave)
	}
	return http.StatusOK, map[string]any{"key": storedKeyView(body.ID)}, nil
}

func handleDeleteKey(_ host, req pluginapi.ManagementRequest) (int, any, error) {
	id := strings.TrimSpace(req.Query.Get("id"))
	file, _, errFind := findStoredKey(id)
	if errFind != nil {
		return 0, nil, errFind
	}
	errSave := mutateKeyFile(file.Name, func(stored *keyFile) error {
		_, errRemove := stored.removeKey(id)
		return errRemove
	})
	if errSave != nil {
		return 0, nil, saveFailure(file.Name, errSave)
	}
	return http.StatusOK, map[string]string{"deleted": id}, nil
}

// handleTestKey reports a failed check in the body: the route worked, the
// key did not.
func handleTestKey(h host, req pluginapi.ManagementRequest) (int, any, error) {
	var body struct {
		ID string `json:"id"`
	}
	if errDecode := decodeBody(req.Body, &body); errDecode != nil {
		return 0, nil, errDecode
	}
	_, entry, errFind := findStoredKey(body.ID)
	if errFind != nil {
		return 0, nil, errFind
	}
	check, errCheck := checkKey(h, entry.APIKey)
	if errCheck != nil {
		return http.StatusOK, map[string]any{"ok": false, "error": errCheck.Error()}, nil
	}
	catalogs.remember(entry.ID, check.Config)
	cat, _ := catalogs.ownCatalog(entry.ID)
	return http.StatusOK, map[string]any{
		"ok":        true,
		"usage":     newUsageView(check.Usage),
		"workspace": check.Config.Workspace,
		"models":    cat.servedCount(currentSettings(), entry.families()),
	}, nil
}

func handleKeyUsage(h host, req pluginapi.ManagementRequest) (int, any, error) {
	_, entry, errFind := findStoredKey(strings.TrimSpace(req.Query.Get("id")))
	if errFind != nil {
		return 0, nil, errFind
	}
	report, errFetch := fetchUsage(context.Background(), h, currentSettings(), entry.APIKey)
	if errFetch != nil {
		return 0, nil, describeCheckFailure(errFetch, false)
	}
	return http.StatusOK, map[string]any{"usage": newUsageView(report)}, nil
}

func handleListLogins(host, pluginapi.ManagementRequest) (int, any, error) {
	files, errLoad := loadAuthFiles()
	if errLoad != nil {
		return 0, nil, failf(http.StatusInternalServerError, "Could not read the OpenCode Go auth files: %v", errLoad)
	}
	views := []loginView{}
	for _, stored := range files.Logins {
		views = append(views, newLoginView(stored))
	}
	return http.StatusOK, map[string]any{"logins": views}, nil
}

// handleUpdateLogin enables or disables a login, and turns OpenCode Go or
// Zen on or off for it. Deleting needs a host callback plugins lack; the
// page sends the user to Auth Files for that.
func handleUpdateLogin(_ host, req pluginapi.ManagementRequest) (int, any, error) {
	var body struct {
		ID       string `json:"id"`
		Disabled *bool  `json:"disabled"`
		Go       *bool  `json:"go"`
		Zen      *bool  `json:"zen"`
	}
	if errDecode := decodeBody(req.Body, &body); errDecode != nil {
		return 0, nil, errDecode
	}
	if body.Disabled == nil && body.Go == nil && body.Zen == nil {
		return 0, nil, failf(http.StatusBadRequest, "Nothing to change: send disabled, go or zen.")
	}
	stored, errFind := findStoredLogin(body.ID)
	if errFind != nil {
		return 0, nil, errFind
	}
	errSave := editLoginFile(stored.Name, func(login *loginCredential) {
		if body.Disabled != nil {
			login.Disabled = *body.Disabled
		}
		if body.Go != nil {
			login.Go = *body.Go
		}
		if body.Zen != nil {
			login.Zen = *body.Zen
		}
	})
	if errors.Is(errSave, errFileGone) {
		return 0, nil, failf(http.StatusNotFound, "That login is no longer stored. Reload the list.")
	}
	if errSave != nil {
		return 0, nil, failf(http.StatusInternalServerError, "Could not save %s: %v", stored.Name, errSave)
	}
	saved, errReload := findStoredLogin(body.ID)
	if errReload != nil {
		return http.StatusOK, map[string]any{"login": nil}, nil
	}
	return http.StatusOK, map[string]any{"login": newLoginView(saved)}, nil
}

func handleLoginUsage(h host, req pluginapi.ManagementRequest) (int, any, error) {
	stored, errFind := findStoredLogin(strings.TrimSpace(req.Query.Get("id")))
	if errFind != nil {
		return 0, nil, errFind
	}
	login := logins.current(stored.Login)
	if login.expired(time.Now()) {
		return 0, nil, failf(http.StatusUnprocessableEntity, "The login's token expired; %s.", logInAgain)
	}
	report, errFetch := fetchUsage(context.Background(), h, currentSettings(), login.AccessToken)
	var upstream *upstreamError
	if errors.As(errFetch, &upstream) && upstream.Status == http.StatusUnauthorized {
		return 0, nil, failf(http.StatusUnprocessableEntity, "OpenCode rejected the login's token: %s. If CLIProxyAPI cannot refresh it, %s.", upstream.Error(), logInAgain)
	}
	if errFetch != nil {
		return 0, nil, describeCheckFailure(errFetch, false)
	}
	return http.StatusOK, map[string]any{"usage": newUsageView(report)}, nil
}

// findStoredLogin reads the auth files and returns the login named id.
func findStoredLogin(id string) (storedLogin, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return storedLogin{}, failf(http.StatusBadRequest, "Missing login id.")
	}
	stored, errFind := lookupLogin(id)
	if errors.Is(errFind, errFileGone) {
		return storedLogin{}, failf(http.StatusNotFound, "That login is no longer stored. Reload the list.")
	}
	if errFind != nil {
		return storedLogin{}, failf(http.StatusInternalServerError, "Could not read the OpenCode Go auth files: %v", errFind)
	}
	return stored, nil
}

func decodeBody(raw []byte, out any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return failf(http.StatusBadRequest, "The request body is empty.")
	}
	if errUnmarshal := json.Unmarshal(raw, out); errUnmarshal != nil {
		return failf(http.StatusBadRequest, "The request body is not valid JSON: %v", errUnmarshal)
	}
	return nil
}

var keyIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// findStoredKey reads the auth files and returns the one holding key id.
func findStoredKey(id string) (storedKeyFile, keyEntry, error) {
	if !keyIDPattern.MatchString(id) {
		return storedKeyFile{}, keyEntry{}, failf(http.StatusBadRequest, "Missing or malformed key id.")
	}
	files, errLoad := loadKeyFiles()
	if errLoad != nil {
		return storedKeyFile{}, keyEntry{}, failf(http.StatusInternalServerError, "Could not read the OpenCode Go auth files: %v", errLoad)
	}
	index, entry, found := locateKey(files, id)
	if !found {
		return storedKeyFile{}, keyEntry{}, failf(http.StatusNotFound, "That key is no longer stored. Reload the list.")
	}
	return files[index], entry, nil
}

// storedKeyView re-reads the files after a save, so the view carries the
// runtime state the host registered for the saved key.
func storedKeyView(id string) *keyView {
	files, errLoad := loadKeyFiles()
	if errLoad != nil {
		return nil
	}
	index, entry, found := locateKey(files, id)
	if !found {
		return nil
	}
	view := newKeyView(files[index], entry)
	return &view
}

func saveFailure(name string, err error) error {
	if errors.Is(err, errKeyNotFound) {
		return failf(http.StatusNotFound, "That key is no longer stored. Reload the list.")
	}
	return failf(http.StatusInternalServerError, "Could not save %s: %v", name, err)
}

// apiKeyPattern is the key shape redactSecrets recognizes; a key outside it
// could leak through error text unredacted.
var apiKeyPattern = regexp.MustCompile(`^oc_sk_[A-Za-z0-9_\-]+$`)

func validateNewKey(raw string) (string, error) {
	apiKey := strings.TrimSpace(raw)
	switch {
	case apiKey == "":
		return "", failf(http.StatusBadRequest, "Paste an OpenCode key.")
	case !strings.HasPrefix(apiKey, "oc_sk_"):
		return "", failf(http.StatusBadRequest, "That is not an OpenCode service-account key; those start with oc_sk_.")
	case !apiKeyPattern.MatchString(apiKey):
		return "", failf(http.StatusBadRequest, "The key contains characters OpenCode keys do not use.")
	}
	return apiKey, nil
}

func cleanLabel(raw string) (string, error) {
	label := strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, raw))
	if utf8.RuneCountInString(label) > maxLabelLength {
		return "", failf(http.StatusBadRequest, "Labels are at most %d characters.", maxLabelLength)
	}
	if secretPattern.MatchString(label) {
		return "", failf(http.StatusBadRequest, "The label contains a key. Put the key in the key field.")
	}
	return label, nil
}

type keyCheck struct {
	Usage  usageReport
	Config workspaceConfig
}

// checkKey proves a key can read its Go usage and its workspace's Console
// config for OpenCode Go. Both endpoints are free.
func checkKey(doer httpDoer, apiKey string) (keyCheck, error) {
	cfg := currentSettings()
	usage, errUsage := fetchUsage(context.Background(), doer, cfg, apiKey)
	if errUsage != nil {
		return keyCheck{}, describeCheckFailure(errUsage, false)
	}
	config, errConfig := fetchWorkspaceConfig(context.Background(), doer, cfg, apiKey)
	if errConfig != nil {
		return keyCheck{}, describeCheckFailure(errConfig, true)
	}
	return keyCheck{Usage: usage, Config: config}, nil
}

// describeCheckFailure says in plain words why a call made with a key
// failed: 422 when OpenCode answered against the key, 502 when it could not
// be asked.
func describeCheckFailure(err error, configStage bool) error {
	var upstream *upstreamError
	if errors.As(err, &upstream) {
		switch {
		case upstream.Status == http.StatusUnauthorized || upstream.Status == http.StatusForbidden:
			return failf(http.StatusUnprocessableEntity, "OpenCode rejected the key: %s", upstream.Error())
		case upstream.Status >= http.StatusInternalServerError:
			return failf(http.StatusBadGateway, "OpenCode is not answering: %s", upstream.Error())
		default:
			return failf(http.StatusUnprocessableEntity, "OpenCode refused the check: %s", upstream.Error())
		}
	}
	var hostErr *hostCallError
	if errors.As(err, &hostErr) {
		return failf(http.StatusBadGateway, "Could not reach OpenCode: %s", hostErr.Message)
	}
	if configStage {
		return failf(http.StatusUnprocessableEntity, "OpenCode sent no OpenCode Go config for the key's workspace; check that it has the Go subscription (%v).", err)
	}
	return failf(http.StatusBadGateway, "OpenCode sent an unreadable usage report: %v", err)
}

// ownCatalog returns the catalog of the workspace config read with key keyID
// itself, never another key's config or none.
func (s *catalogStore) ownCatalog(keyID string) (catalog, bool) {
	base, _ := modelsDev.cached()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.byKey[keyID]
	if !ok {
		return catalog{}, false
	}
	return entry.catalogOf(base), true
}

// remember caches a workspace config a key read, so the model refresh that
// saving a checked key triggers does not read it again.
func (s *catalogStore) remember(keyID string, config workspaceConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := &configEntry{config: config, fetchedAt: s.now()}
	s.byKey[keyID] = entry
	s.lastGood = entry
}
