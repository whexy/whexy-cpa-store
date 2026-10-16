package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
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
	// loginCheckInterval is how often the plugin looks for a login due a
	// refresh; loginCheckDelay gives CPA time to load the auth files first.
	loginCheckInterval = 30 * time.Minute
	loginCheckDelay    = time.Minute

	authKindAttribute = "auth_kind"
	orgIDAttribute    = "org_id"
	emailAttribute    = "email"
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
	// Go and Zen say which families the login serves; Go is on unless the
	// file says false, Zen only when it says true.
	Go, Zen bool
	// extra keeps fields the host or the user added (priority, prefix,
	// proxy_url, ...) across a rewrite.
	extra map[string]json.RawMessage
}

var loginFields = map[string]struct{}{
	"type": {}, "auth_method": {}, "access_token": {}, "refresh_token": {}, "expires_at": {},
	"org_id": {}, "label": {}, "email": {}, "disabled": {}, "go": {}, "zen": {},
}

func (c loginCredential) families() familySet {
	return familySet{Go: c.Go, Zen: c.Zen}
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
		Go:           !jsonFalse(top["go"]),
		Zen:          jsonTrue(top["zen"]),
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
	if !c.Go {
		out["go"] = false
	}
	if c.Zen {
		out["zen"] = true
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

// authData is the login's auth before familyAuth makes it one family's.
// It carries no refresh schedule: CPA never writes back a file that yields
// two auths, so the plugin refreshes logins and saves the tokens itself.
func (c loginCredential) authData(id string) pluginapi.AuthData {
	storage, _ := c.marshal()
	attributes := map[string]string{
		authKindAttribute: loginAuthMethod,
		orgIDAttribute:    c.OrgID,
	}
	if c.Email != "" {
		attributes[emailAttribute] = c.Email
	}
	if mark := extraString(c.extra, catalogRefreshedField); mark != "" {
		attributes[catalogRefreshedField] = mark
	}
	return pluginapi.AuthData{
		Provider:    providerKey,
		ID:          id,
		Label:       c.displayName(),
		Disabled:    c.Disabled,
		StorageJSON: storage,
		Metadata:    c.tokenMetadata(),
		Attributes:  attributes,
	}
}

// loginAuthData is a fresh login as auth.login.poll returns it: the Go auth
// of the file CPA saves it as, which CPA then parses into both families'
// auths. Saving it merges the existing file's non-token fields into the
// metadata unless the metadata already holds them, and metadata wins over
// StorageJSON on disk, so every field the file owns is set here.
func (c loginCredential) loginAuthData() (pluginapi.AuthData, error) {
	name, errName := loginFileName(c.OrgID)
	if errName != nil {
		return pluginapi.AuthData{}, errName
	}
	auth := familyAuth(c.authData(name), c.extra, familyGo)
	auth.FileName = name
	for field, value := range c.fieldMetadata() {
		auth.Metadata[field] = value
	}
	return auth, nil
}

// refreshedAuthData is the auth.refresh result for one family's auth. The
// host keeps no metadata of its own once a refresh returns some, so the
// request's metadata is carried over with the login's fields replaced.
func (c loginCredential) refreshedAuthData(current map[string]any, family modelFamily) pluginapi.AuthData {
	auth := familyAuth(c.authData(""), c.extra, family)
	auth.ID = ""
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
	// refreshedAt is when this process last rotated each workspace's tokens.
	refreshedAt map[string]time.Time
}

func newLoginStore() *loginStore {
	return &loginStore{
		now:         time.Now,
		locks:       make(map[string]*sync.Mutex),
		spent:       make(map[[32]byte]string),
		latest:      make(map[string]loginCredential),
		rejected:    make(map[[32]byte]string),
		refreshedAt: make(map[string]time.Time),
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
	s.refreshedAt[next.OrgID] = s.now()
}

// due reports whether login should be refreshed now: at half the access
// token's life, or, when its expiry is unknown, once per refresh interval
// of this process.
func (s *loginStore) due(login loginCredential) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if !login.ExpiresAt.IsZero() {
		return !now.Before(login.ExpiresAt.Add(-loginRefreshInterval))
	}
	last, ok := s.refreshedAt[login.OrgID]
	return !ok || now.Sub(last) >= loginRefreshInterval
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

// handleAuthRefresh rotates a login's tokens; CPA asks when a request got a
// 401. Keys never expire, so a key auth gets an empty answer, which tells the
// host to keep it as is.
//
// The login file yields two auths, which CPA never writes back, so the new
// tokens are saved here before CPA gets them: OpenCode revokes the session
// when a spent refresh token is presented again, which a restart would do
// with tokens CPA only held in memory. Saving makes CPA parse the file
// again, which hands both auths the new tokens; CPA then keeps those over
// this answer.
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
	if errSave := saveRotatedLogins(refreshed.OrgID); errSave != nil {
		h.Log("warn", "OpenCode login refreshed but its file was not saved; the plugin tries again", map[string]any{"auth_id": req.AuthID, "error": errSave.Error()})
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth: refreshed.refreshedAuthData(req.Metadata, authFamily(req.Attributes)),
	})
}

// saveRotatedLogins rewrites every login file of the workspace whose
// refresh token this process has already spent. orgID "" covers every
// workspace.
func saveRotatedLogins(orgID string) error {
	files, errLoad := loadAuthFiles()
	if errLoad != nil {
		return errLoad
	}
	var failures []string
	for _, stored := range files.Logins {
		if orgID != "" && stored.Login.OrgID != orgID {
			continue
		}
		if logins.current(stored.Login).RefreshToken == stored.Login.RefreshToken {
			continue
		}
		if errSave := editLoginFile(stored.Name, func(*loginCredential) {}); errSave != nil {
			failures = append(failures, stored.Name+": "+errSave.Error())
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

var loginRefreshOnce sync.Once

// startLoginRefresh refreshes due logins for the plugin's lifetime.
func startLoginRefresh() {
	loginRefreshOnce.Do(func() {
		startBackground(func(ctx context.Context) {
			timer := time.NewTimer(loginCheckDelay)
			defer timer.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
					refreshDueLogins(ctx, newHost(""))
					timer.Reset(loginCheckInterval)
				}
			}
		})
	})
}

// refreshDueLogins refreshes each enabled login that is due, once per
// workspace, and saves every login file holding a spent refresh token. A
// token OpenCode rejected is left alone: the login needs a new one.
func refreshDueLogins(ctx context.Context, h host) {
	files, errLoad := loadAuthFiles()
	if errLoad != nil {
		h.Log("warn", "OpenCode login check could not read the auth files", map[string]any{"error": errLoad.Error()})
		return
	}
	cfg := currentSettings()
	checked := map[string]bool{}
	for _, stored := range files.Logins {
		login := logins.current(stored.Login)
		if login.Disabled || checked[login.OrgID] {
			continue
		}
		checked[login.OrgID] = true
		if _, rejected := logins.rejection(login.RefreshToken); rejected || !logins.due(login) {
			continue
		}
		if _, errRefresh := logins.refresh(ctx, h, cfg, login); errRefresh != nil {
			h.Log("warn", "OpenCode login refresh failed", map[string]any{"workspace": login.OrgID, "error": errRefresh.Error()})
		}
	}
	if errSave := saveRotatedLogins(""); errSave != nil {
		h.Log("warn", "OpenCode login refreshed but its file was not saved; the plugin tries again", map[string]any{"error": errSave.Error()})
	}
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
