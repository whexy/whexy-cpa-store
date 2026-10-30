package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	authKindAttribute = "auth_kind"
	userIDAttribute   = "user_id"
	systemIDAttribute = "system_id"
	loginFileName     = "zed-oauth-"
	// accessTokenExpiry is a far-future value: a Zed access token does not
	// expire, and setting one keeps CPA from treating a failed refresh as an
	// expired login.
	accessTokenExpiry = 10 * 365 * 24 * time.Hour
)

// authFile is one Zed account in the auth directory.
type authFile struct {
	UserID         string
	AccessToken    string
	SystemID       string
	OrganizationID string
	Username       string
	Plan           string
	Label          string
	Disabled       bool
	AddedAt        string
	// extra keeps fields the host or the user added (priority, prefix,
	// proxy_url, ...) across a rewrite.
	extra map[string]json.RawMessage
}

var authFields = map[string]struct{}{
	"type": {}, "auth_method": {}, "user_id": {}, "access_token": {}, "system_id": {},
	"organization_id": {}, "username": {}, "plan": {}, "label": {}, "disabled": {}, "added_at": {},
}

var errNotZedFile = errors.New("not a Zed auth file")

func isZedFile(raw []byte) bool {
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(head.Type), providerKey)
}

func parseAuthFile(raw []byte) (authFile, error) {
	if !isZedFile(raw) {
		return authFile{}, errNotZedFile
	}
	var top map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &top); errUnmarshal != nil {
		return authFile{}, fmt.Errorf("decode auth file: %w", errUnmarshal)
	}
	text := func(name string) string {
		var value string
		_ = json.Unmarshal(top[name], &value)
		return strings.TrimSpace(value)
	}
	file := authFile{
		UserID:         text("user_id"),
		AccessToken:    text("access_token"),
		SystemID:       text("system_id"),
		OrganizationID: text("organization_id"),
		Username:       text("username"),
		Plan:           text("plan"),
		Label:          text("label"),
		AddedAt:        text("added_at"),
		Disabled:       jsonTrue(top["disabled"]),
		extra:          make(map[string]json.RawMessage),
	}
	for name, value := range top {
		if _, owned := authFields[name]; !owned {
			file.extra[name] = value
		}
	}
	if file.UserID == "" || file.AccessToken == "" {
		return authFile{}, errors.New("auth file has no user_id or access_token")
	}
	if file.SystemID == "" {
		file.SystemID = derivedSystemID(file.UserID, file.AccessToken)
	}
	return file, nil
}

// derivedSystemID gives an imported auth a stable x-zed-system-id even when
// the file does not carry one. Zed binds the JWT to it, not to a machine.
func derivedSystemID(userID, token string) string {
	sum := sha256.Sum256([]byte("zed-system-id\x00" + userID + "\x00" + token))
	sum[6] = (sum[6] & 0x0f) | 0x40
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func (f authFile) credential() credential {
	return newCredential(f.UserID, f.AccessToken, f.SystemID, f.OrganizationID, f.Username, f.Plan)
}

func (f authFile) displayLabel() string {
	name := f.Username
	if name == "" {
		name = f.Label
	}
	if name == "" {
		name = "Zed " + maskToken(f.AccessToken)
	}
	if plan := planLabel(f.Plan); plan != "" {
		return name + " · " + plan
	}
	return name
}

func planLabel(plan string) string {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "token_based_zed_pro", "zed_pro":
		return "Pro"
	case "token_based_zed_student", "zed_student":
		return "Student"
	case "token_based_zed_business", "zed_business":
		return "Business"
	case "zed_free", "token_based_zed_free":
		return "Free"
	}
	return strings.TrimSpace(plan)
}

func maskToken(token string) string {
	token = strings.TrimSpace(token)
	if len(token) <= 6 {
		return "…"
	}
	return "…" + token[len(token)-4:]
}

func (f authFile) marshal() ([]byte, error) {
	out := make(map[string]any, len(f.extra)+10)
	for name, value := range f.extra {
		out[name] = value
	}
	out["type"] = providerKey
	out["auth_method"] = "oauth"
	out["user_id"] = f.UserID
	out["access_token"] = f.AccessToken
	out["system_id"] = f.SystemID
	if f.OrganizationID != "" {
		out["organization_id"] = f.OrganizationID
	}
	if f.Username != "" {
		out["username"] = f.Username
	}
	if f.Plan != "" {
		out["plan"] = f.Plan
	}
	if f.Label != "" {
		out["label"] = f.Label
	}
	if f.Disabled {
		out["disabled"] = true
	}
	if f.AddedAt != "" {
		out["added_at"] = f.AddedAt
	}
	return json.MarshalIndent(out, "", "  ")
}

// authFileBase is the file name used in auth IDs. The host passes a path
// relative to the auth directory on save but a base name on load, so only
// the base name is stable.
func authFileBase(path, fileName string) string {
	if strings.TrimSpace(path) != "" {
		return filepath.Base(strings.TrimSpace(path))
	}
	if strings.TrimSpace(fileName) != "" {
		return filepath.Base(strings.TrimSpace(fileName))
	}
	return ""
}

func authIDFor(base string, file authFile) string {
	if base != "" {
		return base
	}
	return providerKey + "-" + file.UserID
}

func fileNameFor(base string, file authFile) string {
	if base != "" {
		return base
	}
	name := file.Username
	if name == "" {
		name = file.UserID
	}
	return loginFileName + sanitizeFileName(name) + ".json"
}

func sanitizeFileName(name string) string {
	var builder strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	cleaned := strings.Trim(builder.String(), "-.")
	if cleaned == "" {
		cleaned = "account"
	}
	if len(cleaned) > 48 {
		cleaned = cleaned[:48]
	}
	return cleaned
}

// handleAuthParse recognizes one Zed account file.
func handleAuthParse(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode auth parse request: %w", errUnmarshal)
	}
	if !isZedFile(req.RawJSON) {
		return okEnvelope(pluginapi.AuthParseResponse{})
	}
	base := authFileBase(req.Path, req.FileName)
	file, errParse := parseAuthFile(req.RawJSON)
	if errParse != nil {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: placeholderAuth(base, displayName+" (invalid auth file)", req.RawJSON)})
	}
	auth := file.authData(base, req.RawJSON)
	return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: auth})
}

// placeholderAuth keeps a file the plugin owns but cannot use visible to the
// host as a disabled auth, instead of letting the host register the raw file.
func placeholderAuth(base, label string, raw []byte) pluginapi.AuthData {
	id := base
	if id == "" {
		id = providerKey
	}
	return pluginapi.AuthData{
		Provider:    providerKey,
		ID:          id,
		FileName:    base,
		Label:       label,
		Disabled:    true,
		StorageJSON: bytes.Clone(raw),
		Attributes:  map[string]string{authKindAttribute: "oauth"},
	}
}

func (f authFile) authData(base string, raw []byte) pluginapi.AuthData {
	id := authIDFor(base, f)
	return pluginapi.AuthData{
		Provider:         providerKey,
		ID:               id,
		FileName:         fileNameFor(base, f),
		Label:            f.displayLabel(),
		Disabled:         f.Disabled,
		StorageJSON:      bytes.Clone(raw),
		Metadata:         f.metadata(),
		Attributes:       f.attributes(),
		NextRefreshAfter: time.Now().Add(accessTokenExpiry),
	}
}

func (f authFile) metadata() map[string]any {
	// access_token plus expires_at classify the auth as OAuth and let the host
	// decide a 401 is worth a refresh.
	return map[string]any{
		"access_token":    f.AccessToken,
		"expires_at":      time.Now().Add(accessTokenExpiry).UTC().Format(time.RFC3339),
		"user_id":         f.UserID,
		"system_id":       f.SystemID,
		"organization_id": f.OrganizationID,
		"username":        f.Username,
		"plan":            f.Plan,
	}
}

func (f authFile) attributes() map[string]string {
	return map[string]string{
		authKindAttribute: "oauth",
		userIDAttribute:   f.UserID,
		systemIDAttribute: f.SystemID,
	}
}

// resolveCredential finds the account a host call is about. Executor and
// model.for_auth calls receive the auth's StorageJSON; refresh and quota
// calls may receive metadata instead.
func resolveCredential(storageJSON []byte, attributes map[string]string, metadata map[string]any) (credential, error) {
	if len(bytes.TrimSpace(storageJSON)) > 0 {
		if isZedFile(storageJSON) {
			file, errParse := parseAuthFile(storageJSON)
			if errParse == nil {
				return file.credential(), nil
			}
			return credential{}, errParse
		}
		// A login token store may persist the account fields directly.
		var file authFile
		if errUnmarshal := json.Unmarshal(storageJSON, &file); errUnmarshal == nil && file.UserID != "" && file.AccessToken != "" {
			if file.SystemID == "" {
				file.SystemID = derivedSystemID(file.UserID, file.AccessToken)
			}
			return file.credential(), nil
		}
	}
	userID := strings.TrimSpace(attributes[userIDAttribute])
	token := metadataString(metadata, "access_token")
	systemID := strings.TrimSpace(attributes[systemIDAttribute])
	if systemID == "" {
		systemID = metadataString(metadata, "system_id")
	}
	if userID == "" {
		userID = metadataString(metadata, "user_id")
	}
	if userID == "" || token == "" {
		return credential{}, errors.New("no Zed access token")
	}
	if systemID == "" {
		systemID = derivedSystemID(userID, token)
	}
	return newCredential(userID, token, systemID, metadataString(metadata, "organization_id"), metadataString(metadata, "username"), metadataString(metadata, "plan")), nil
}

func metadataString(metadata map[string]any, name string) string {
	if metadata == nil {
		return ""
	}
	if value, ok := metadata[name].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

// handleAuthRefresh validates a stored account and refreshes its plan and
// organization. A Zed access token does not rotate, so this is a health
// check rather than a token exchange.
func handleAuthRefresh(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode auth refresh request: %w", errUnmarshal)
	}
	cred, errCred := resolveCredential(req.StorageJSON, req.Attributes, req.Metadata)
	if errCred != nil {
		return errorEnvelope("invalid_auth", "Zed auth "+req.AuthID+": "+errCred.Error(), http.StatusUnauthorized), nil
	}
	h := newHost("")
	cfg := currentSettings()
	info, errInfo := fetchAccountInfo(context.Background(), h, cfg, cred)
	if errInfo != nil {
		status := http.StatusBadGateway
		var failure *zedFailure
		if errors.As(errInfo, &failure) {
			status = failure.Status
		}
		return errorEnvelope("refresh_failed", "Zed auth "+req.AuthID+": "+errInfo.Error(), status), nil
	}
	file, errFile := authFileFromStorage(req.StorageJSON, cred)
	if errFile != nil {
		return errorEnvelope("refresh_failed", errFile.Error(), http.StatusInternalServerError), nil
	}
	file.Username = info.Username
	file.Plan = info.Plan
	file.OrganizationID = info.OrganizationID
	cred.Username = info.Username
	cred.Plan = info.Plan
	cred.OrganizationID = info.OrganizationID
	llmTokens.drop(llmTokens.key(cred))
	storage, _ := file.marshal()
	base := req.AuthID
	if !strings.HasSuffix(base, ".json") {
		base = fileNameFor("", file)
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             file.authData(base, storage),
		NextRefreshAfter: time.Now().Add(accessTokenExpiry),
	})
}

func authFileFromStorage(storageJSON []byte, cred credential) (authFile, error) {
	if len(bytes.TrimSpace(storageJSON)) > 0 && isZedFile(storageJSON) {
		return parseAuthFile(storageJSON)
	}
	return authFile{
		UserID:         cred.UserID,
		AccessToken:    cred.AccessToken,
		SystemID:       cred.SystemID,
		OrganizationID: cred.OrganizationID,
		Username:       cred.Username,
		Plan:           cred.Plan,
		AddedAt:        time.Now().UTC().Format(time.RFC3339),
		extra:          make(map[string]json.RawMessage),
	}, nil
}

// modelCache keeps the last catalog per account for a few minutes. The host
// asks for an auth's models only when the auth or the plugin config changes.
type modelCache struct {
	mu      sync.Mutex
	entries map[string]modelCacheEntry
	now     func() time.Time
}

type modelCacheEntry struct {
	models  []zedModel
	expires time.Time
}

var modelsByAuth = &modelCache{entries: make(map[string]modelCacheEntry), now: time.Now}

const modelCacheTTL = 5 * time.Minute

func (c *modelCache) get(key string) ([]zedModel, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || c.now().After(entry.expires) {
		return nil, false
	}
	return entry.models, true
}

func (c *modelCache) put(key string, models []zedModel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = modelCacheEntry{models: models, expires: c.now().Add(modelCacheTTL)}
}

func (c *modelCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// jsonTrue reports a JSON field that is explicitly true, so an absent field
// keeps its default.
func jsonTrue(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return false
	}
	var flag bool
	if json.Unmarshal(raw, &flag) == nil {
		return flag
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.EqualFold(strings.TrimSpace(text), "true")
	}
	return false
}
