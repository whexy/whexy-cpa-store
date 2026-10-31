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
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	resourcePagePath   = "/v0/resource/plugins/" + providerKey + "/index.html"
	logoPath           = "/v0/resource/plugins/" + providerKey + "/logo.svg"
	managementBasePath = "/v0/management/" + providerKey
	accountsPath       = managementBasePath + "/accounts"
	modelsPath         = managementBasePath + "/models"
	loginPath          = managementBasePath + "/login"
	maxLabelLength     = 64
)

//go:embed management.html
var managementPage []byte

var managementPageCSP = pageCSP(managementPage)

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

var managementRoutes = []managementRoute{
	{Method: http.MethodGet, Path: accountsPath, Description: "List Zed accounts: username, plan, state and model count.", handle: handleListAccounts},
	{Method: http.MethodPatch, Path: accountsPath, Description: "Rename or enable/disable a Zed account: {id, label?, disabled?}.", handle: handleUpdateAccount},
	{Method: http.MethodGet, Path: modelsPath, Description: "List the models of the first working Zed account, with their enabled state.", handle: handleListModels},
	{Method: http.MethodPost, Path: modelsPath + "/refresh", Description: "Read every account's catalog again and re-register the model lists.", handle: handleRefreshModels},
	{Method: http.MethodPost, Path: loginPath + "/start", Description: "Start a Zed sign-in and return the URL and state for the callback paste box.", handle: handleStartLogin},
	{Method: http.MethodPost, Path: loginPath + "/finish", Description: "Finish a captured sign-in and save the account: {state}.", handle: handleFinishLogin},
	{Method: http.MethodPost, Path: loginPath + "/complete", Description: "Finish a sign-in with the callback URL a remote browser produced: {state, callback_url}.", handle: handleCompleteLogin},
}

func managementRegister(raw []byte) ([]byte, error) {
	return okEnvelope(managementRegistration{
		Routes: managementRoutes,
		Resources: []resourceRoute{
			{Path: resourcePagePath, Menu: displayName, Description: "Manage Zed accounts, models and settings."},
			{Path: logoPath, Description: "Zed logo."},
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
	return managementJSON(http.StatusNotFound, map[string]string{"error": "no Zed route " + method + " " + req.Path})
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
		Body: []byte(redactSecrets(string(raw))),
	}
}

type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return e.message }

func failf(status int, format string, args ...any) error {
	return &apiError{status: status, message: fmt.Sprintf(format, args...)}
}

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

func handleListAccounts(h host, req pluginapi.ManagementRequest) (int, any, error) {
	files, errList := hostAuthList()
	if errList != nil {
		return 0, nil, errList
	}
	accounts := []zedAccount{}
	for _, entry := range files {
		if !strings.EqualFold(strings.TrimSpace(entry.Type), providerKey) && !strings.EqualFold(strings.TrimSpace(entry.Provider), providerKey) {
			continue
		}
		if entry.RuntimeOnly {
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
		if raw, errGet := hostAuthGet(entry.AuthIndex); errGet == nil {
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

func handleUpdateAccount(h host, req pluginapi.ManagementRequest) (int, any, error) {
	var body struct {
		ID       string  `json:"id"`
		Label    *string `json:"label"`
		Disabled *bool   `json:"disabled"`
	}
	if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
		return 0, nil, failf(http.StatusBadRequest, "decode request: %v", errUnmarshal)
	}
	if strings.TrimSpace(body.ID) == "" {
		return 0, nil, failf(http.StatusBadRequest, "id is required")
	}
	raw, errGet := hostAuthGet(body.ID)
	if errGet != nil {
		return 0, nil, failf(http.StatusNotFound, "read the Zed account: %v", errGet)
	}
	file, errParse := parseAuthFile(raw.JSON)
	if errParse != nil {
		return 0, nil, failf(http.StatusBadRequest, "parse the Zed account: %v", errParse)
	}
	if body.Label != nil {
		label := strings.TrimSpace(*body.Label)
		if len(label) > maxLabelLength {
			return 0, nil, failf(http.StatusBadRequest, "label is longer than %d characters", maxLabelLength)
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
	if _, errSave := hostAuthSave(name, updated); errSave != nil {
		return 0, nil, failf(http.StatusInternalServerError, "save the Zed account: %v", errSave)
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

func firstCredential(h host) (credential, string, error) {
	files, errList := hostAuthList()
	if errList != nil {
		return credential{}, "", errList
	}
	for _, entry := range files {
		if entry.Disabled || entry.RuntimeOnly {
			continue
		}
		if !strings.EqualFold(entry.Type, providerKey) && !strings.EqualFold(entry.Provider, providerKey) {
			continue
		}
		raw, errGet := hostAuthGet(entry.AuthIndex)
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

func handleListModels(h host, req pluginapi.ManagementRequest) (int, any, error) {
	cred, account, errCred := firstCredential(h)
	if errCred != nil {
		return http.StatusOK, map[string]any{"models": []zedModelView{}, "account": "", "error": errCred.Error()}, nil
	}
	cfg := currentSettings()
	models, errModels := catalogFor(context.Background(), h, cfg, cred, false)
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

func handleRefreshModels(h host, req pluginapi.ManagementRequest) (int, any, error) {
	files, errList := hostAuthList()
	if errList != nil {
		return 0, nil, errList
	}
	refreshed := 0
	var failures []string
	for _, entry := range files {
		if entry.RuntimeOnly || (!strings.EqualFold(entry.Type, providerKey) && !strings.EqualFold(entry.Provider, providerKey)) {
			continue
		}
		raw, errGet := hostAuthGet(entry.AuthIndex)
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
		if _, errCatalog := catalogFor(context.Background(), h, currentSettings(), cred, true); errCatalog != nil {
			failures = append(failures, raw.Name+": "+errCatalog.Error())
			continue
		}
		// CPA re-reads an auth's models only when the auth changes; a save
		// with a new catalog mark makes it ask again.
		file.extra["catalog_refreshed_at"] = json.RawMessage(strconv.Quote(time.Now().UTC().Format(time.RFC3339Nano)))
		if updated, errMarshal := file.marshal(); errMarshal == nil {
			if _, errSave := hostAuthSave(raw.Name, updated); errSave != nil {
				return 0, nil, failf(http.StatusInternalServerError, "save %s: %v", raw.Name, errSave)
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

func handleStartLogin(h host, req pluginapi.ManagementRequest) (int, any, error) {
	resp, errStart := logins.start(currentSettings())
	if errStart != nil {
		return 0, nil, failf(http.StatusBadGateway, "start the Zed sign-in: %v", errStart)
	}
	return http.StatusOK, map[string]any{
		"url":        resp.URL,
		"state":      resp.State,
		"expires_at": resp.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"),
	}, nil
}

func handleFinishLogin(h host, req pluginapi.ManagementRequest) (int, any, error) {
	var body struct {
		State string `json:"state"`
	}
	if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
		return 0, nil, failf(http.StatusBadRequest, "decode request: %v", errUnmarshal)
	}
	if strings.TrimSpace(body.State) == "" {
		return 0, nil, failf(http.StatusBadRequest, "state is required")
	}
	resp := logins.poll(context.Background(), h, currentSettings(), strings.TrimSpace(body.State))
	switch resp.Status {
	case pluginapi.AuthLoginStatusPending:
		return http.StatusOK, map[string]any{"status": "pending", "message": resp.Message}, nil
	case pluginapi.AuthLoginStatusSuccess:
		name := resp.Auth.FileName
		if name == "" {
			name = fileNameFor("", authFile{UserID: metadataString(resp.Auth.Metadata, "user_id"), Username: metadataString(resp.Auth.Metadata, "username")})
		}
		saved, errSave := hostAuthSave(name, resp.Auth.StorageJSON)
		if errSave != nil {
			return 0, nil, failf(http.StatusInternalServerError, "save the Zed account: %v", errSave)
		}
		return http.StatusOK, map[string]any{"status": "success", "saved": saved.Name}, nil
	default:
		return http.StatusOK, map[string]any{"status": "error", "message": resp.Message}, nil
	}
}

func handleCompleteLogin(h host, req pluginapi.ManagementRequest) (int, any, error) {
	var body struct {
		State       string `json:"state"`
		CallbackURL string `json:"callback_url"`
	}
	if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
		return 0, nil, failf(http.StatusBadRequest, "decode request: %v", errUnmarshal)
	}
	if strings.TrimSpace(body.State) == "" || strings.TrimSpace(body.CallbackURL) == "" {
		return 0, nil, failf(http.StatusBadRequest, "state and callback_url are required")
	}
	if errComplete := logins.completeFromCallback(body.State, body.CallbackURL); errComplete != nil {
		return 0, nil, failf(http.StatusBadRequest, "%v", errComplete)
	}
	cfg := currentSettings()
	resp := logins.poll(context.Background(), h, cfg, body.State)
	if resp.Status != pluginapi.AuthLoginStatusSuccess {
		return 0, nil, failf(http.StatusBadGateway, "finish the Zed sign-in: %s", resp.Message)
	}
	name := resp.Auth.FileName
	if name == "" {
		name = fileNameFor("", authFile{UserID: metadataString(resp.Auth.Metadata, "user_id"), Username: metadataString(resp.Auth.Metadata, "username")})
	}
	saved, errSave := hostAuthSave(name, resp.Auth.StorageJSON)
	if errSave != nil {
		return 0, nil, failf(http.StatusInternalServerError, "save the Zed account: %v", errSave)
	}
	return http.StatusOK, map[string]any{"ok": true, "saved": saved.Name, "message": "signed in as " + metadataString(resp.Auth.Metadata, "username")}, nil
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
