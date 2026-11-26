package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/host"
	"github.com/whexy/whexy-cpa-store/pluginkit/manage"
)

const maxLabelLength = 64

//go:embed management.html
var managementPage []byte

// logoSVG is the plugin mark; ../logo.svg is the copy the store registry
// links to.
//
//go:embed logo.svg
var logoSVG []byte

var (
	accountsPath = manage.APIPath(providerKey, "accounts")
	modelsPath   = manage.APIPath(providerKey, "models")
	loginPath    = manage.APIPath(providerKey, "login")
)

var router = manage.NewRouter(manage.Config{
	PluginID: providerKey,
	Name:     displayName,
	Page:     &manage.Page{Menu: displayName, Description: "Manage Zed accounts, models and settings.", HTML: managementPage},
	Logo:     logoSVG,
	Routes: []manage.Route{
		{Method: http.MethodGet, Path: accountsPath, Description: "List Zed accounts: username, plan and state.", Handle: handleListAccounts},
		{Method: http.MethodPatch, Path: accountsPath, Description: "Rename or enable/disable a Zed account: {id, label?, disabled?}.", Handle: handleUpdateAccount},
		{Method: http.MethodGet, Path: modelsPath, Description: "List the models of the first working Zed account, with their enabled state.", Handle: handleListModels},
		{Method: http.MethodPost, Path: modelsPath + "/refresh", Description: "Read every account's catalog again and re-register the model lists.", Handle: handleRefreshModels},
		{Method: http.MethodPost, Path: loginPath + "/start", Description: "Start a Zed sign-in and return the URL and state for the callback paste box.", Handle: handleStartLogin},
		{Method: http.MethodPost, Path: loginPath + "/finish", Description: "Finish a captured sign-in and save the account: {state}.", Handle: handleFinishLogin},
		{Method: http.MethodPost, Path: loginPath + "/complete", Description: "Finish a sign-in with the callback URL a remote browser produced: {state, callback_url}.", Handle: handleCompleteLogin},
	},
})

// zedAccount is the account view the page renders. It never carries the
// access token.
type zedAccount struct {
	ID          string `json:"id"`
	AuthIndex   string `json:"auth_index"`
	FileName    string `json:"file_name"`
	Label       string `json:"label"`
	Username    string `json:"username"`
	UserID      string `json:"user_id"`
	Plan        string `json:"plan"`
	PlanLabel   string `json:"plan_label"`
	Disabled    bool   `json:"disabled"`
	Status      string `json:"status"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	ModelsError string `json:"models_error,omitempty"`
}

// isZedEntry reports whether a host auth entry is one of this plugin's.
func isZedEntry(entry pluginapi.HostAuthFileEntry) bool {
	return strings.EqualFold(strings.TrimSpace(entry.Type), providerKey) || strings.EqualFold(strings.TrimSpace(entry.Provider), providerKey)
}

func handleListAccounts(manage.Request) (int, any, error) {
	files, errList := host.AuthList()
	if errList != nil {
		return 0, nil, errList
	}
	accounts := []zedAccount{}
	for _, entry := range files {
		if !isZedEntry(entry) || entry.RuntimeOnly {
			continue
		}
		account := zedAccount{
			ID:        entry.ID,
			AuthIndex: entry.AuthIndex,
			FileName:  entry.Name,
			Label:     entry.Label,
			Disabled:  entry.Disabled,
			Status:    entry.Status,
		}
		if !entry.UpdatedAt.IsZero() {
			account.UpdatedAt = entry.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		if raw, errGet := host.AuthGet(entry.AuthIndex); errGet == nil {
			if file, errParse := parseAuthFile(raw.JSON); errParse == nil {
				account.Username = file.Username
				account.UserID = file.UserID
				account.Plan = file.Plan
				account.PlanLabel = planLabel(file.Plan)
				if file.Label != "" {
					account.Label = file.Label
				}
				if account.Label == "" {
					account.Label = file.displayLabel()
				}
			} else {
				account.ModelsError = errParse.Error()
			}
		}
		accounts = append(accounts, account)
	}
	return http.StatusOK, map[string]any{"accounts": accounts}, nil
}

func handleUpdateAccount(req manage.Request) (int, any, error) {
	var body struct {
		ID       string  `json:"id"`
		Label    *string `json:"label"`
		Disabled *bool   `json:"disabled"`
	}
	if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
		return 0, nil, manage.Errorf(http.StatusBadRequest, "decode request: %v", errUnmarshal)
	}
	if strings.TrimSpace(body.ID) == "" {
		return 0, nil, manage.Errorf(http.StatusBadRequest, "id is required")
	}
	raw, errGet := host.AuthGet(body.ID)
	if errGet != nil {
		return 0, nil, manage.Errorf(http.StatusNotFound, "read the Zed account: %v", errGet)
	}
	file, errParse := parseAuthFile(raw.JSON)
	if errParse != nil {
		return 0, nil, manage.Errorf(http.StatusBadRequest, "parse the Zed account: %v", errParse)
	}
	if body.Label != nil {
		label := strings.TrimSpace(*body.Label)
		if len(label) > maxLabelLength {
			return 0, nil, manage.Errorf(http.StatusBadRequest, "label is longer than %d characters", maxLabelLength)
		}
		file.Label = label
	}
	if body.Disabled != nil {
		file.Disabled = *body.Disabled
	}
	updated, errMarshal := file.marshal()
	if errMarshal != nil {
		return 0, nil, errMarshal
	}
	name := raw.Name
	if name == "" {
		name = fileNameFor("", file)
	}
	if _, errSave := host.AuthSave(name, updated); errSave != nil {
		return 0, nil, manage.Errorf(http.StatusInternalServerError, "save the Zed account: %v", errSave)
	}
	return http.StatusOK, map[string]any{"saved": name}, nil
}

type zedModelView struct {
	ID            string   `json:"id"`
	UpstreamID    string   `json:"upstream_id"`
	Provider      string   `json:"provider"`
	DisplayName   string   `json:"display_name"`
	Protocol      string   `json:"protocol"`
	ContextLength int64    `json:"context_length"`
	OutputTokens  int64    `json:"output_tokens"`
	Tools         bool     `json:"tools"`
	Thinking      bool     `json:"thinking"`
	Images        bool     `json:"images"`
	Disabled      bool     `json:"disabled"`
	Account       string   `json:"account"`
	Efforts       []string `json:"efforts,omitempty"`
}

// firstCredential returns the first enabled Zed account that parses, and its
// display label.
func firstCredential() (credential, string, error) {
	files, errList := host.AuthList()
	if errList != nil {
		return credential{}, "", errList
	}
	for _, entry := range files {
		if entry.Disabled || entry.RuntimeOnly || !isZedEntry(entry) {
			continue
		}
		raw, errGet := host.AuthGet(entry.AuthIndex)
		if errGet != nil {
			continue
		}
		file, errParse := parseAuthFile(raw.JSON)
		if errParse != nil {
			continue
		}
		return file.credential(), file.displayLabel(), nil
	}
	return credential{}, "", errors.New("no enabled Zed account")
}

func handleListModels(req manage.Request) (int, any, error) {
	cred, account, errCred := firstCredential()
	if errCred != nil {
		return http.StatusOK, map[string]any{"models": []zedModelView{}, "account": "", "error": errCred.Error()}, nil
	}
	cfg := currentSettings()
	models, errModels := catalogFor(context.Background(), req.Host(), cfg, cred, false)
	if errModels != nil {
		return 0, nil, errModels
	}
	out := make([]zedModelView, 0, len(models))
	for _, model := range models {
		out = append(out, zedModelView{
			ID:            cfg.exposedID(model.ID),
			UpstreamID:    model.ID,
			Provider:      model.Provider,
			DisplayName:   model.DisplayName,
			Protocol:      model.nativeProtocol(),
			ContextLength: model.ContextLength,
			OutputTokens:  model.OutputTokens,
			Tools:         model.SupportsTools,
			Thinking:      model.SupportsThinking,
			Images:        model.SupportsImages,
			Disabled:      cfg.modelDisabled(model.ID),
			Account:       account,
			Efforts:       model.EffortLevels,
		})
	}
	return http.StatusOK, map[string]any{"models": out, "account": account, "prefix": cfg.modelPrefix()}, nil
}

func handleRefreshModels(req manage.Request) (int, any, error) {
	files, errList := host.AuthList()
	if errList != nil {
		return 0, nil, errList
	}
	refreshed := 0
	var failures []string
	for _, entry := range files {
		if entry.RuntimeOnly || !isZedEntry(entry) {
			continue
		}
		raw, errGet := host.AuthGet(entry.AuthIndex)
		if errGet != nil {
			continue
		}
		file, errParse := parseAuthFile(raw.JSON)
		if errParse != nil {
			failures = append(failures, raw.Name+": "+errParse.Error())
			continue
		}
		cred := file.credential()
		modelsByAuth.drop(llmTokens.key(cred))
		llmTokens.drop(llmTokens.key(cred))
		if _, errCatalog := catalogFor(context.Background(), req.Host(), currentSettings(), cred, true); errCatalog != nil {
			failures = append(failures, raw.Name+": "+errCatalog.Error())
			continue
		}
		// CPA re-reads an auth's models only when the auth changes; a save
		// with a new catalog mark makes it ask again.
		file.extra["catalog_refreshed_at"] = json.RawMessage(strconv.Quote(time.Now().UTC().Format(time.RFC3339Nano)))
		if updated, errMarshal := file.marshal(); errMarshal == nil {
			if _, errSave := host.AuthSave(raw.Name, updated); errSave != nil {
				return 0, nil, manage.Errorf(http.StatusInternalServerError, "save %s: %v", raw.Name, errSave)
			}
		}
		refreshed++
	}
	out := map[string]any{"accounts": refreshed}
	if len(failures) > 0 {
		out["errors"] = failures
	}
	return http.StatusOK, out, nil
}

func handleStartLogin(manage.Request) (int, any, error) {
	resp, errStart := logins.start(currentSettings())
	if errStart != nil {
		return 0, nil, manage.Errorf(http.StatusBadGateway, "start the Zed sign-in: %v", errStart)
	}
	return http.StatusOK, map[string]any{
		"url":        resp.URL,
		"state":      resp.State,
		"expires_at": resp.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"),
	}, nil
}

func handleFinishLogin(req manage.Request) (int, any, error) {
	var body struct {
		State string `json:"state"`
	}
	if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
		return 0, nil, manage.Errorf(http.StatusBadRequest, "decode request: %v", errUnmarshal)
	}
	if strings.TrimSpace(body.State) == "" {
		return 0, nil, manage.Errorf(http.StatusBadRequest, "state is required")
	}
	resp := logins.poll(context.Background(), req.Host(), currentSettings(), strings.TrimSpace(body.State))
	switch resp.Status {
	case pluginapi.AuthLoginStatusPending:
		return http.StatusOK, map[string]any{"status": "pending", "message": resp.Message}, nil
	case pluginapi.AuthLoginStatusSuccess:
		saved, errSave := saveSignedIn(resp.Auth)
		if errSave != nil {
			return 0, nil, errSave
		}
		return http.StatusOK, map[string]any{"status": "success", "saved": saved}, nil
	default:
		return http.StatusOK, map[string]any{"status": "error", "message": resp.Message}, nil
	}
}

func handleCompleteLogin(req manage.Request) (int, any, error) {
	var body struct {
		State       string `json:"state"`
		CallbackURL string `json:"callback_url"`
	}
	if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
		return 0, nil, manage.Errorf(http.StatusBadRequest, "decode request: %v", errUnmarshal)
	}
	if strings.TrimSpace(body.State) == "" || strings.TrimSpace(body.CallbackURL) == "" {
		return 0, nil, manage.Errorf(http.StatusBadRequest, "state and callback_url are required")
	}
	if errComplete := logins.completeFromCallback(body.State, body.CallbackURL); errComplete != nil {
		return 0, nil, manage.Errorf(http.StatusBadRequest, "%v", errComplete)
	}
	resp := logins.poll(context.Background(), req.Host(), currentSettings(), body.State)
	if resp.Status != pluginapi.AuthLoginStatusSuccess {
		return 0, nil, manage.Errorf(http.StatusBadGateway, "finish the Zed sign-in: %s", resp.Message)
	}
	saved, errSave := saveSignedIn(resp.Auth)
	if errSave != nil {
		return 0, nil, errSave
	}
	return http.StatusOK, map[string]any{"ok": true, "saved": saved, "message": "signed in as " + metadataString(resp.Auth.Metadata, "username")}, nil
}

// saveSignedIn writes a finished sign-in to the auth directory, where the
// host registers it, and returns the saved file name.
func saveSignedIn(auth pluginapi.AuthData) (string, error) {
	name := auth.FileName
	if name == "" {
		name = fileNameFor("", authFile{UserID: metadataString(auth.Metadata, "user_id"), Username: metadataString(auth.Metadata, "username")})
	}
	saved, errSave := host.AuthSave(name, auth.StorageJSON)
	if errSave != nil {
		return "", manage.Errorf(http.StatusInternalServerError, "save the Zed account: %v", errSave)
	}
	return saved.Name, nil
}
