package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/host"
	"github.com/whexy/whexy-cpa-store/pluginkit/manage"
)

var (
	keysPath       = manage.APIPath(providerKey, "keys")
	loginsPath     = manage.APIPath(providerKey, "logins")
	modelsPath     = manage.APIPath(providerKey, "models")
	routingPath    = manage.APIPath(providerKey, "routing")
	freePath       = manage.APIPath(providerKey, "free")
	workspacesPath = manage.APIPath(providerKey, "workspaces")
)

const maxLabelLength = 64

// managementPage holds no data: everything it shows comes from the
// authenticated routes.
//
//go:embed management.html
var managementPage []byte

// logoSVG is the OpenCode mark; ../logo.svg is the copy the store registry
// links to.
//
//go:embed logo.svg
var logoSVG []byte

// Key routes name keys by their key ID, which is derived from the key and
// stays fixed; an auth_index shifts when an earlier key is removed. Login
// routes name a login by its file name. The host matches exact paths, so
// IDs travel in the query or body.
var router = manage.NewRouter(manage.Config{
	PluginID: providerKey,
	Name:     displayName,
	Page:     &manage.Page{Menu: displayName, Description: "Manage OpenCode Go logins, keys, models, OpenCode Free and routing, and see workspace usage.", HTML: managementPage},
	Logo:     logoSVG,
	Routes: []manage.Route{
		{Method: http.MethodGet, Path: keysPath, Description: "List OpenCode Go keys: masked key, label, state, workspace.", Handle: handleListKeys},
		{Method: http.MethodPost, Path: keysPath, Description: "Check and add an OpenCode Go key: {label, api_key}.", Handle: handleAddKey},
		{Method: http.MethodPatch, Path: keysPath, Description: "Rename, enable or disable an OpenCode Go key, or turn OpenCode Go or Zen on or off for it: {id, label?, disabled?, go?, zen?}.", Handle: handleUpdateKey},
		{Method: http.MethodDelete, Path: keysPath, Description: "Remove an OpenCode Go key: ?id=.", Handle: handleDeleteKey},
		{Method: http.MethodPost, Path: keysPath + "/test", Description: "Check a stored OpenCode Go key against the gateway: {id}.", Handle: handleTestKey},
		{Method: http.MethodGet, Path: keysPath + "/usage", Description: "Read the 5-hour, weekly and monthly usage of an OpenCode Go key: ?id=.", Handle: handleKeyUsage},
		{Method: http.MethodGet, Path: loginsPath, Description: "List OpenCode logins: workspace, account, token expiry, state.", Handle: handleListLogins},
		{Method: http.MethodPatch, Path: loginsPath, Description: "Enable or disable an OpenCode login, or turn OpenCode Go or Zen on or off for it: {id, disabled?, go?, zen?}.", Handle: handleUpdateLogin},
		{Method: http.MethodGet, Path: loginsPath + "/usage", Description: "Read the 5-hour, weekly and monthly usage of an OpenCode login: ?id=.", Handle: handleLoginUsage},
		{Method: http.MethodGet, Path: modelsPath, Description: "List the OpenCode Go and Zen models of the first working login or key, as models.dev and its workspace config describe them, and how they differ from the models CLIProxyAPI registered.", Handle: handleListModels},
		{Method: http.MethodPost, Path: modelsPath + "/refresh", Description: "Read every catalog again and make CLIProxyAPI register the current model lists.", Handle: handleRefreshModels},
		{Method: http.MethodGet, Path: routingPath, Description: "Read the OpenCode Go and OpenCode Zen priority and weight of every OpenCode Go auth file.", Handle: handleGetRouting},
		{Method: http.MethodPut, Path: routingPath, Description: "Set the OpenCode Go, Zen and Free priority and weight: {go: {priority, weight}, zen: {...}, free: {...}}, null for the default.", Handle: handleSetRouting},
		{Method: http.MethodGet, Path: freePath, Description: "Read OpenCode Free: its auth file, state and free models.", Handle: handleGetFree},
		{Method: http.MethodPut, Path: freePath, Description: "Turn OpenCode Free on or off: {enabled}.", Handle: handlePutFree},
		{Method: http.MethodGet, Path: workspacesPath, Description: "List the OpenCode workspaces the logins and keys reach, each with the login or key its usage is read with.", Handle: handleListWorkspaces},
		{Method: http.MethodGet, Path: workspacesPath + "/usage", Description: "Read the usage of a workspace over a range: totals, cost by hour or day, cost by model, web search and the latest requests: ?id=, range (today, 7d, 30d or all) and since (the start of today).", Handle: handleWorkspaceUsage},
	},
})

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

func handleListKeys(manage.Request) (int, any, error) {
	files, errLoad := loadKeyFiles()
	if errLoad != nil {
		return 0, nil, manage.Errorf(http.StatusInternalServerError, "Could not read the OpenCode Go auth files: %v", errLoad)
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

func handleAddKey(req manage.Request) (int, any, error) {
	h := req.Host()
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
		return 0, nil, manage.Errorf(http.StatusInternalServerError, "Could not read the OpenCode Go auth files: %v", errLoad)
	}
	id := keyIDFor(apiKey)
	if _, existing, found := locateKey(files, id); found {
		return 0, nil, manage.Errorf(http.StatusConflict, "This key is already added as %q.", existing.displayLabel())
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
		return 0, nil, manage.Errorf(http.StatusConflict, "This key is already added.")
	}
	if errSave != nil {
		return 0, nil, manage.Errorf(http.StatusInternalServerError, "Could not save %s: %v", keyFileName, errSave)
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

func handleUpdateKey(req manage.Request) (int, any, error) {
	var body updateKeyRequest
	if errDecode := decodeBody(req.Body, &body); errDecode != nil {
		return 0, nil, errDecode
	}
	if body.Label == nil && body.Disabled == nil && body.Go == nil && body.Zen == nil {
		return 0, nil, manage.Errorf(http.StatusBadRequest, "Nothing to change: send label, disabled, go or zen.")
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

func handleDeleteKey(req manage.Request) (int, any, error) {
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
func handleTestKey(req manage.Request) (int, any, error) {
	h := req.Host()
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

func handleKeyUsage(req manage.Request) (int, any, error) {
	h := req.Host()
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

func handleListLogins(manage.Request) (int, any, error) {
	files, errLoad := loadAuthFiles()
	if errLoad != nil {
		return 0, nil, manage.Errorf(http.StatusInternalServerError, "Could not read the OpenCode Go auth files: %v", errLoad)
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
func handleUpdateLogin(req manage.Request) (int, any, error) {
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
		return 0, nil, manage.Errorf(http.StatusBadRequest, "Nothing to change: send disabled, go or zen.")
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
		return 0, nil, manage.Errorf(http.StatusNotFound, "That login is no longer stored. Reload the list.")
	}
	if errSave != nil {
		return 0, nil, manage.Errorf(http.StatusInternalServerError, "Could not save %s: %v", stored.Name, errSave)
	}
	saved, errReload := findStoredLogin(body.ID)
	if errReload != nil {
		return http.StatusOK, map[string]any{"login": nil}, nil
	}
	return http.StatusOK, map[string]any{"login": newLoginView(saved)}, nil
}

func handleLoginUsage(req manage.Request) (int, any, error) {
	h := req.Host()
	stored, errFind := findStoredLogin(strings.TrimSpace(req.Query.Get("id")))
	if errFind != nil {
		return 0, nil, errFind
	}
	login := logins.current(stored.Login)
	if login.expired(time.Now()) {
		return 0, nil, manage.Errorf(http.StatusUnprocessableEntity, "The login's token expired; %s.", logInAgain)
	}
	report, errFetch := fetchUsage(context.Background(), h, currentSettings(), login.AccessToken)
	var upstream *upstreamError
	if errors.As(errFetch, &upstream) && upstream.Status == http.StatusUnauthorized {
		return 0, nil, manage.Errorf(http.StatusUnprocessableEntity, "OpenCode rejected the login's token: %s. If CLIProxyAPI cannot refresh it, %s.", upstream.Error(), logInAgain)
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
		return storedLogin{}, manage.Errorf(http.StatusBadRequest, "Missing login id.")
	}
	stored, errFind := lookupLogin(id)
	if errors.Is(errFind, errFileGone) {
		return storedLogin{}, manage.Errorf(http.StatusNotFound, "That login is no longer stored. Reload the list.")
	}
	if errFind != nil {
		return storedLogin{}, manage.Errorf(http.StatusInternalServerError, "Could not read the OpenCode Go auth files: %v", errFind)
	}
	return stored, nil
}

func decodeBody(raw []byte, out any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return manage.Errorf(http.StatusBadRequest, "The request body is empty.")
	}
	if errUnmarshal := json.Unmarshal(raw, out); errUnmarshal != nil {
		return manage.Errorf(http.StatusBadRequest, "The request body is not valid JSON: %v", errUnmarshal)
	}
	return nil
}

var keyIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// findStoredKey reads the auth files and returns the one holding key id.
func findStoredKey(id string) (storedKeyFile, keyEntry, error) {
	if !keyIDPattern.MatchString(id) {
		return storedKeyFile{}, keyEntry{}, manage.Errorf(http.StatusBadRequest, "Missing or malformed key id.")
	}
	files, errLoad := loadKeyFiles()
	if errLoad != nil {
		return storedKeyFile{}, keyEntry{}, manage.Errorf(http.StatusInternalServerError, "Could not read the OpenCode Go auth files: %v", errLoad)
	}
	index, entry, found := locateKey(files, id)
	if !found {
		return storedKeyFile{}, keyEntry{}, manage.Errorf(http.StatusNotFound, "That key is no longer stored. Reload the list.")
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
		return manage.Errorf(http.StatusNotFound, "That key is no longer stored. Reload the list.")
	}
	return manage.Errorf(http.StatusInternalServerError, "Could not save %s: %v", name, err)
}

// apiKeyPattern is the key shape redactSecrets recognizes; a key outside it
// could leak through error text unredacted.
var apiKeyPattern = regexp.MustCompile(`^oc_sk_[A-Za-z0-9_\-]+$`)

func validateNewKey(raw string) (string, error) {
	apiKey := strings.TrimSpace(raw)
	switch {
	case apiKey == "":
		return "", manage.Errorf(http.StatusBadRequest, "Paste an OpenCode key.")
	case !strings.HasPrefix(apiKey, "oc_sk_"):
		return "", manage.Errorf(http.StatusBadRequest, "That is not an OpenCode service-account key; those start with oc_sk_.")
	case !apiKeyPattern.MatchString(apiKey):
		return "", manage.Errorf(http.StatusBadRequest, "The key contains characters OpenCode keys do not use.")
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
		return "", manage.Errorf(http.StatusBadRequest, "Labels are at most %d characters.", maxLabelLength)
	}
	if secretPattern.MatchString(label) {
		return "", manage.Errorf(http.StatusBadRequest, "The label contains a key. Put the key in the key field.")
	}
	return label, nil
}

type keyCheck struct {
	Usage  usageReport
	Config workspaceConfig
}

// checkKey proves a key can read its Go usage and its workspace's Console
// config for OpenCode Go. Both endpoints are free.
func checkKey(doer host.Doer, apiKey string) (keyCheck, error) {
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
			return manage.Errorf(http.StatusUnprocessableEntity, "OpenCode rejected the key: %s", upstream.Error())
		case upstream.Status >= http.StatusInternalServerError:
			return manage.Errorf(http.StatusBadGateway, "OpenCode is not answering: %s", upstream.Error())
		default:
			return manage.Errorf(http.StatusUnprocessableEntity, "OpenCode refused the check: %s", upstream.Error())
		}
	}
	var hostErr *host.CallError
	if errors.As(err, &hostErr) {
		return manage.Errorf(http.StatusBadGateway, "Could not reach OpenCode: %s", hostErr.Message)
	}
	if configStage {
		return manage.Errorf(http.StatusUnprocessableEntity, "OpenCode sent no OpenCode Go config for the key's workspace; check that it has the Go subscription (%v).", err)
	}
	return manage.Errorf(http.StatusBadGateway, "OpenCode sent an unreadable usage report: %v", err)
}
