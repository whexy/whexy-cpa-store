package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	loginAuthMethod = "oauth"
	loginFilePrefix = providerKey + "-oauth-"

	// loginRefreshInterval is half the 30-day access token OpenCode issues.
	// Refreshing at half-life leaves 15 days to retry a failed refresh, and
	// rotates the single-use refresh token only twice a month.
	loginRefreshInterval = 15 * 24 * time.Hour

	// CPA schedules a plugin auth's refresh only through this attribute:
	// it has no refresh lead for plugin providers, and it clears
	// NextRefreshAfter after every successful refresh.
	refreshIntervalAttribute = "refresh_interval_seconds"
	// CPA refreshes an interval-scheduled auth at once when it does not know
	// when it last refreshed it, which is the case for an auth registered
	// before this plugin parsed it. This attribute tells it.
	lastRefreshAttribute = "last_refresh"
	authKindAttribute    = "auth_kind"
	orgIDAttribute       = "org_id"
	emailAttribute       = "email"
)

// loginCredential is an OpenCode Console login, stored as
// opencode-go-oauth-<org_id>.json in the auth directory.
type loginCredential struct {
	AccessToken  string
	RefreshToken string
	// ExpiresAt is when the access token expires; zero if unknown.
	ExpiresAt time.Time
	OrgID     string
	// Workspace is the workspace name, stored as "label".
	Workspace string
	Email     string
	Disabled  bool
	// extra keeps fields the host or the user added (priority, prefix,
	// proxy_url, ...) across a rewrite.
	extra map[string]json.RawMessage
}

var loginFields = map[string]struct{}{
	"type": {}, "auth_method": {}, "access_token": {}, "refresh_token": {}, "expires_at": {},
	"org_id": {}, "label": {}, "email": {}, "disabled": {},
}

var errNotLoginFile = errors.New("not an OpenCode login file")

// isLoginFile reports whether raw is an opencode-go login rather than a
// key file.
func isLoginFile(raw []byte) bool {
	var head struct {
		Type         string `json:"type"`
		AuthMethod   string `json:"auth_method"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(raw, &head) != nil || !strings.EqualFold(strings.TrimSpace(head.Type), providerKey) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(head.AuthMethod), loginAuthMethod) || strings.TrimSpace(head.RefreshToken) != ""
}

func parseLoginFile(raw []byte) (loginCredential, error) {
	if !isLoginFile(raw) {
		return loginCredential{}, errNotLoginFile
	}
	var top map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &top); errUnmarshal != nil {
		return loginCredential{}, fmt.Errorf("decode login file: %w", errUnmarshal)
	}
	text := func(name string) string {
		var value string
		_ = json.Unmarshal(top[name], &value)
		return strings.TrimSpace(value)
	}
	login := loginCredential{
		AccessToken:  text("access_token"),
		RefreshToken: text("refresh_token"),
		OrgID:        text("org_id"),
		Workspace:    text("label"),
		Email:        text("email"),
		Disabled:     jsonTrue(top["disabled"]),
		extra:        make(map[string]json.RawMessage),
	}
	for name, value := range top {
		if _, owned := loginFields[name]; !owned {
			login.extra[name] = value
		}
	}
	if expires := text("expires_at"); expires != "" {
		parsed, errParse := time.Parse(time.RFC3339Nano, expires)
		if errParse != nil {
			return loginCredential{}, fmt.Errorf("login file has an unreadable expires_at %q", expires)
		}
		login.ExpiresAt = parsed.UTC()
	}
	switch {
	case login.AccessToken == "" || login.RefreshToken == "":
		return loginCredential{}, errors.New("login file has no access or refresh token")
	case login.OrgID == "":
		return loginCredential{}, errors.New("login file names no workspace (org_id)")
	}
	return login, nil
}

func (c loginCredential) marshal() ([]byte, error) {
	out := make(map[string]any, len(c.extra)+len(loginFields))
	for name, value := range c.extra {
		out[name] = value
	}
	out["type"] = providerKey
	out["auth_method"] = loginAuthMethod
	out["access_token"] = c.AccessToken
	out["refresh_token"] = c.RefreshToken
	if !c.ExpiresAt.IsZero() {
		out["expires_at"] = c.ExpiresAt.UTC().Format(time.RFC3339)
	}
	out["org_id"] = c.OrgID
	out["label"] = c.Workspace
	out["email"] = c.Email
	if c.Disabled {
		out["disabled"] = true
	}
	return json.MarshalIndent(out, "", "  ")
}

func (c loginCredential) displayName() string {
	if c.Workspace != "" {
		return c.Workspace
	}
	return displayName + " " + c.OrgID
}

func (c loginCredential) credential() credential {
	name := c.displayName()
	if c.Email != "" {
		name += " · " + c.Email
	}
	return credential{ID: "login:" + c.OrgID, Bearer: c.AccessToken, Name: name}
}

func (c loginCredential) expired(now time.Time) bool {
	return !c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt)
}

// refreshDue is when the host should first refresh the login; zero means
// at once.
func (c loginCredential) refreshDue() time.Time {
	if c.ExpiresAt.IsZero() {
		return time.Time{}
	}
	return c.ExpiresAt.Add(-loginRefreshInterval)
}

var orgIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// loginFileName names the file after the workspace, so logging in to the
// same workspace again replaces the earlier login.
func loginFileName(orgID string) (string, error) {
	if !orgIDPattern.MatchString(orgID) {
		return "", fmt.Errorf("OpenCode returned an unusable workspace id %q", orgID)
	}
	return loginFilePrefix + orgID + ".json", nil
}

// tokenMetadata mirrors the tokens into the auth metadata: CPA decides
// whether an auth holds a usable access token, whether a 401 may be
// answered with a refresh, and whether a refresh raced another credential
// change from metadata alone. The host writes metadata over StorageJSON
// when it persists, so every AuthData this plugin returns carries the
// current tokens in both.
func (c loginCredential) tokenMetadata() map[string]any {
	return map[string]any{"access_token": c.AccessToken, "refresh_token": c.RefreshToken}
}

// fieldMetadata is the login's other file fields as metadata values.
func (c loginCredential) fieldMetadata() map[string]any {
	expires := ""
	if !c.ExpiresAt.IsZero() {
		expires = c.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"auth_method": loginAuthMethod,
		"expires_at":  expires,
		"org_id":      c.OrgID,
		"label":       c.Workspace,
		"email":       c.Email,
	}
}

func (c loginCredential) authData(id string) pluginapi.AuthData {
	storage, _ := c.marshal()
	attributes := map[string]string{
		authKindAttribute:        loginAuthMethod,
		orgIDAttribute:           c.OrgID,
		refreshIntervalAttribute: strconv.FormatInt(int64(loginRefreshInterval/time.Second), 10),
	}
	if c.Email != "" {
		attributes[emailAttribute] = c.Email
	}
	if !c.ExpiresAt.IsZero() {
		attributes[lastRefreshAttribute] = c.ExpiresAt.Add(-2 * loginRefreshInterval).UTC().Format(time.RFC3339)
	}
	if mark := extraString(c.extra, catalogRefreshedField); mark != "" {
		attributes[catalogRefreshedField] = mark
	}
	return pluginapi.AuthData{
		Provider:         providerKey,
		ID:               id,
		Label:            c.displayName(),
		Disabled:         c.Disabled,
		StorageJSON:      storage,
		Metadata:         c.tokenMetadata(),
		Attributes:       attributes,
		NextRefreshAfter: c.refreshDue(),
	}
}

// parseLoginAuth turns a login file into its auth. StorageJSON is the file
// as read, because the host may write it back over the file.
func parseLoginAuth(base string, raw []byte) pluginapi.AuthData {
	login, errParse := parseLoginFile(raw)
	if errParse != nil {
		return pluginapi.AuthData{
			Provider:    providerKey,
			ID:          base,
			Label:       displayName + " (invalid login file)",
			Disabled:    true,
			StorageJSON: bytes.Clone(raw),
		}
	}
	auth := login.authData(base)
	auth.StorageJSON = bytes.Clone(raw)
	return auth
}

// loginAuthData is a fresh login as auth.login.poll returns it. Saving it
// merges the existing file's non-token fields into the metadata unless the
// metadata already holds them, and metadata wins over StorageJSON on disk,
// so every field the file owns is set here.
func (c loginCredential) loginAuthData() (pluginapi.AuthData, error) {
	name, errName := loginFileName(c.OrgID)
	if errName != nil {
		return pluginapi.AuthData{}, errName
	}
	auth := c.authData(name)
	auth.FileName = name
	for field, value := range c.fieldMetadata() {
		auth.Metadata[field] = value
	}
	return auth, nil
}

// refreshedAuthData is the auth.refresh result. The host keeps no metadata
// of its own once a refresh returns some, so the request's metadata is
// carried over with the login's fields replaced.
func (c loginCredential) refreshedAuthData(current map[string]any) pluginapi.AuthData {
	auth := c.authData("")
	auth.Label = ""
	metadata := make(map[string]any, len(current)+2)
	for field, value := range current {
		metadata[field] = value
	}
	for field, value := range c.fieldMetadata() {
		if _, present := metadata[field]; present {
			metadata[field] = value
		}
	}
	for field, value := range c.tokenMetadata() {
		metadata[field] = value
	}
	auth.Metadata = metadata
	return auth
}

// loginExpiredError reports a login whose access token has expired.
type loginExpiredError struct {
	name string
	at   time.Time
}

func (e *loginExpiredError) Error() string {
	return fmt.Sprintf("the OpenCode login for %s expired at %s; %s", e.name, e.at.UTC().Format(time.RFC3339), logInAgain)
}

const logInAgain = "log in again from Management Center → OAuth Login → OpenCode Go"

// oauthError is an error answer from the Console token endpoints.
type oauthError struct {
	Status      int
	Code        string
	Description string
}

func (e *oauthError) Error() string {
	text := fmt.Sprintf("OpenCode Console returned %d", e.Status)
	if e.Code != "" {
		text += " " + e.Code
	}
	if e.Description != "" {
		text += ": " + e.Description
	}
	return redactSecrets(text)
}

// tokenResponse is a /auth/device/token answer, success or error.
type tokenResponse struct {
	AccessToken      string  `json:"access_token"`
	RefreshToken     string  `json:"refresh_token"`
	ExpiresIn        float64 `json:"expires_in"`
	OrgID            *string `json:"org_id"`
	Error            string  `json:"error"`
	ErrorDescription string  `json:"error_description"`
}

// consoleRequestTimeout bounds each Console call of the login and refresh
// flows; CPA allows timeouts while credentials are being acquired.
const consoleRequestTimeout = 30 * time.Second

// postConsoleForm posts a form to a Console endpoint the way OpenCode's own
// client does.
func postConsoleForm(ctx context.Context, doer httpDoer, cfg settings, path string, form url.Values) (pluginapi.HTTPResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, consoleRequestTimeout)
	defer cancel()
	return doer.Do(ctx, pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    cfg.consoleURL() + path,
		Headers: http.Header{
			"Content-Type": {"application/x-www-form-urlencoded"},
			"Accept":       {"application/json"},
			"User-Agent":   {cfg.userAgent()},
		},
		Body: []byte(form.Encode()),
	})
}

// requestToken calls the token endpoint. A non-2xx answer becomes an
// *oauthError; a 2xx answer must carry both tokens.
func requestToken(ctx context.Context, doer httpDoer, cfg settings, form url.Values) (tokenResponse, error) {
	resp, errDo := postConsoleForm(ctx, doer, cfg, "/auth/device/token", form)
	if errDo != nil {
		return tokenResponse{}, fmt.Errorf("reach OpenCode Console: %w", errDo)
	}
	var token tokenResponse
	errDecode := json.Unmarshal(resp.Body, &token)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		out := &oauthError{Status: resp.StatusCode, Code: strings.TrimSpace(token.Error), Description: strings.TrimSpace(token.ErrorDescription)}
		if errDecode != nil && out.Code == "" {
			out.Description = http.StatusText(resp.StatusCode)
		}
		return tokenResponse{}, out
	}
	if errDecode != nil {
		return tokenResponse{}, fmt.Errorf("decode OpenCode token response: %w", errDecode)
	}
	if strings.TrimSpace(token.AccessToken) == "" || strings.TrimSpace(token.RefreshToken) == "" {
		return tokenResponse{}, errors.New("OpenCode token response holds no tokens")
	}
	return token, nil
}

func (t tokenResponse) expiresAt(now time.Time) time.Time {
	if t.ExpiresIn <= 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(t.ExpiresIn * float64(time.Second))).UTC().Truncate(time.Second)
}

func isInvalidGrant(err error) bool {
	var oauth *oauthError
	return errors.As(err, &oauth) && oauth.Code == "invalid_grant"
}

// loginStore serializes token work per workspace and remembers refresh
// tokens it has spent. OpenCode revokes the whole session when a used
// refresh token is presented again, so a stale token handed back by the
// host is answered from memory instead of upstream.
type loginStore struct {
	mu    sync.Mutex
	now   func() time.Time
	locks map[string]*sync.Mutex
	// spent maps a used refresh token's hash to its workspace.
	spent map[[32]byte]string
	// latest holds the newest tokens of each workspace this process got.
	latest map[string]loginCredential
	// rejected holds the error OpenCode gave for a refresh token.
	rejected map[[32]byte]string
}

func newLoginStore() *loginStore {
	return &loginStore{
		now:      time.Now,
		locks:    make(map[string]*sync.Mutex),
		spent:    make(map[[32]byte]string),
		latest:   make(map[string]loginCredential),
		rejected: make(map[[32]byte]string),
	}
}

var logins = newLoginStore()

func tokenHash(token string) [32]byte {
	return sha256.Sum256([]byte(token))
}

// lock holds the workspace's token lock until the returned func runs.
func (s *loginStore) lock(orgID string) func() {
	s.mu.Lock()
	lock, ok := s.locks[orgID]
	if !ok {
		lock = &sync.Mutex{}
		s.locks[orgID] = lock
	}
	s.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

// current replaces spent tokens with the newest ones this process holds.
// Callers hold the workspace lock.
func (s *loginStore) current(login loginCredential) loginCredential {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spent[tokenHash(login.RefreshToken)] != login.OrgID {
		return login
	}
	latest, ok := s.latest[login.OrgID]
	if !ok {
		return login
	}
	login.AccessToken = latest.AccessToken
	login.RefreshToken = latest.RefreshToken
	login.ExpiresAt = latest.ExpiresAt
	return login
}

func (s *loginStore) rotated(previous, next loginCredential) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous.RefreshToken != next.RefreshToken {
		s.spent[tokenHash(previous.RefreshToken)] = previous.OrgID
	}
	s.latest[next.OrgID] = next
}

func (s *loginStore) rejection(refreshToken string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	message, ok := s.rejected[tokenHash(refreshToken)]
	return message, ok
}

func (s *loginStore) reject(refreshToken, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejected[tokenHash(refreshToken)] = message
}

// refresh exchanges the login's refresh token once. A token this process
// already spent yields the tokens it was exchanged for, and a token
// OpenCode rejected yields the same rejection, both without a request.
func (s *loginStore) refresh(ctx context.Context, doer httpDoer, cfg settings, login loginCredential) (loginCredential, error) {
	unlock := s.lock(login.OrgID)
	defer unlock()
	if advanced := s.current(login); advanced.RefreshToken != login.RefreshToken {
		return advanced, nil
	}
	if message, rejected := s.rejection(login.RefreshToken); rejected {
		return loginCredential{}, &refreshRejectedError{message: message}
	}
	token, errToken := requestToken(ctx, doer, cfg, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {login.RefreshToken},
		"client_id":     {loginClientID},
	})
	if errToken != nil {
		if isInvalidGrant(errToken) {
			message := errToken.Error()
			s.reject(login.RefreshToken, message)
			return loginCredential{}, &refreshRejectedError{message: message}
		}
		return loginCredential{}, errToken
	}
	next := login
	next.AccessToken = token.AccessToken
	next.RefreshToken = token.RefreshToken
	next.ExpiresAt = token.expiresAt(s.now())
	s.rotated(login, next)
	return next, nil
}

// refreshRejectedError is an invalid_grant answer: the login is over. Its
// text carries "invalid_grant" because CPA recognizes the case by it.
type refreshRejectedError struct {
	message string
}

func (e *refreshRejectedError) Error() string {
	return "OpenCode rejected the login's refresh token (" + e.message + "); " + logInAgain
}

type rpcAuthRefreshRequest struct {
	pluginapi.AuthRefreshRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleAuthRefresh rotates a login's tokens. Keys never expire, so a key
// auth gets an empty answer, which tells the host to keep it as is.
//
// CPA runs at most one refresh per auth at a time and writes the result to
// the auth file before it releases that auth's refresh lock.
func handleAuthRefresh(raw []byte) ([]byte, error) {
	var req rpcAuthRefreshRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode auth refresh request: %w", errUnmarshal)
	}
	if !isLoginFile(req.StorageJSON) {
		return okEnvelope(pluginapi.AuthRefreshResponse{})
	}
	login, errParse := parseLoginFile(req.StorageJSON)
	if errParse != nil {
		return errorEnvelope("invalid_auth", "OpenCode Go auth "+req.AuthID+": "+errParse.Error(), http.StatusBadRequest), nil
	}
	h := newHost(req.HostCallbackID)
	refreshed, errRefresh := logins.refresh(backgroundCtx, h, currentSettings(), login)
	if errRefresh != nil {
		var rejected *refreshRejectedError
		if errors.As(errRefresh, &rejected) {
			h.Log("warn", "OpenCode login signed out; it needs a new login", map[string]any{"auth_id": req.AuthID, "workspace": login.OrgID})
			return errorEnvelope("invalid_grant", errRefresh.Error(), http.StatusUnauthorized), nil
		}
		return errorEnvelope("refresh_failed", "refresh OpenCode login: "+errRefresh.Error(), refreshErrorStatus(errRefresh)), nil
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             refreshed.refreshedAuthData(req.Metadata),
		NextRefreshAfter: refreshed.refreshDue(),
	})
}

// refreshErrorStatus keeps a failed refresh retryable: CPA backs off and
// tries again while the access token still works.
func refreshErrorStatus(err error) int {
	var oauth *oauthError
	if errors.As(err, &oauth) && oauth.Status >= 400 && oauth.Status < 500 && oauth.Status != http.StatusUnauthorized {
		return oauth.Status
	}
	return http.StatusBadGateway
}
