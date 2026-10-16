package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// loginClientID names this plugin on OpenCode's device approval page.
	loginClientID   = "whexy-cpa-opencode-go"
	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

	defaultPollInterval = 5 * time.Second
	defaultDeviceExpiry = 10 * time.Minute
	slowDownStep        = 5 * time.Second
	// finishedLoginKept is how long a finished login keeps answering polls
	// after its code expires.
	finishedLoginKept = time.Minute

	sessionDeviceCode = "device_code"
	sessionInterval   = "interval_seconds"
	sessionExpiresAt  = "expires_at"
	sessionConsoleURL = "console_url"
)

type rpcAuthLoginStartRequest struct {
	pluginapi.AuthLoginStartRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcAuthLoginPollRequest struct {
	pluginapi.AuthLoginPollRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type deviceCodeResponse struct {
	DeviceCode              string  `json:"device_code"`
	UserCode                string  `json:"user_code"`
	VerificationURI         string  `json:"verification_uri"`
	VerificationURIComplete string  `json:"verification_uri_complete"`
	ExpiresIn               float64 `json:"expires_in"`
	Interval                float64 `json:"interval"`
}

// deviceLogin is this process's view of one pending login. The device code
// stays in the host's session metadata, which never reaches the browser.
type deviceLogin struct {
	mu        sync.Mutex
	interval  time.Duration
	nextPoll  time.Time
	expiresAt time.Time
	// result is the final answer; later polls get it again rather than
	// spending the device code twice.
	result *pluginapi.AuthLoginPollResponse
}

type deviceFlows struct {
	mu    sync.Mutex
	now   func() time.Time
	flows map[string]*deviceLogin
}

func newDeviceFlows() *deviceFlows {
	return &deviceFlows{now: time.Now, flows: make(map[string]*deviceLogin)}
}

var devices = newDeviceFlows()

func handleLoginStart(raw []byte) ([]byte, error) {
	var req rpcAuthLoginStartRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode login start request: %w", errUnmarshal)
	}
	resp, errStart := devices.start(backgroundCtx, newHost(req.HostCallbackID), currentSettings())
	if errStart != nil {
		return errorEnvelope("login_start_failed", "start OpenCode login: "+errStart.Error(), http.StatusBadGateway), nil
	}
	return okEnvelope(resp)
}

func handleLoginPoll(raw []byte) ([]byte, error) {
	var req rpcAuthLoginPollRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode login poll request: %w", errUnmarshal)
	}
	h := newHost(req.HostCallbackID)
	return okEnvelope(devices.poll(backgroundCtx, h, currentSettings(), strings.TrimSpace(req.State), req.Metadata))
}

// start asks the Console for a device code. The URL it returns carries the
// user code, so the approval page needs no typing.
func (d *deviceFlows) start(ctx context.Context, doer httpDoer, cfg settings) (pluginapi.AuthLoginStartResponse, error) {
	resp, errDo := postConsoleForm(ctx, doer, cfg, "/auth/device/code", url.Values{
		"client_id":          {loginClientID},
		"supports_org_scope": {"true"},
	})
	if errDo != nil {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("reach OpenCode Console: %w", errDo)
	}
	var code deviceCodeResponse
	errDecode := json.Unmarshal(resp.Body, &code)
	if resp.StatusCode != http.StatusOK {
		var failure tokenResponse
		_ = json.Unmarshal(resp.Body, &failure)
		return pluginapi.AuthLoginStartResponse{}, &oauthError{Status: resp.StatusCode, Code: failure.Error, Description: failure.ErrorDescription}
	}
	if errDecode != nil || strings.TrimSpace(code.DeviceCode) == "" {
		return pluginapi.AuthLoginStartResponse{}, errors.New("OpenCode Console sent no device code")
	}
	verify, errURL := verificationURL(cfg, code)
	if errURL != nil {
		return pluginapi.AuthLoginStartResponse{}, errURL
	}
	state, errState := newLoginState()
	if errState != nil {
		return pluginapi.AuthLoginStartResponse{}, errState
	}
	now := d.now()
	interval := seconds(code.Interval, defaultPollInterval)
	expiresAt := now.Add(seconds(code.ExpiresIn, defaultDeviceExpiry)).UTC().Truncate(time.Second)
	d.mu.Lock()
	d.pruneLocked(now)
	d.flows[state] = &deviceLogin{interval: interval, nextPoll: now.Add(interval), expiresAt: expiresAt}
	d.mu.Unlock()
	return pluginapi.AuthLoginStartResponse{
		Provider:  providerKey,
		URL:       verify,
		State:     state,
		ExpiresAt: expiresAt,
		Metadata: map[string]any{
			sessionDeviceCode: code.DeviceCode,
			sessionInterval:   int64(interval / time.Second),
			sessionExpiresAt:  expiresAt.Format(time.RFC3339),
			sessionConsoleURL: cfg.consoleURL(),
		},
	}, nil
}

// verificationURL makes the Console's verification URI absolute. It is a
// path such as /console/device?user_code=..., relative to the Console host.
func verificationURL(cfg settings, code deviceCodeResponse) (string, error) {
	target := strings.TrimSpace(code.VerificationURIComplete)
	if target == "" {
		target = strings.TrimSpace(code.VerificationURI)
		if target != "" && code.UserCode != "" {
			separator := "?"
			if strings.Contains(target, "?") {
				separator = "&"
			}
			target += separator + url.Values{"user_code": {code.UserCode}}.Encode()
		}
	}
	base, errBase := url.Parse(cfg.consoleURL() + "/")
	ref, errRef := url.Parse(target)
	if target == "" || errBase != nil || errRef != nil {
		return "", fmt.Errorf("OpenCode Console sent an unusable verification URI %q", target)
	}
	resolved := base.ResolveReference(ref)
	if (resolved.Scheme != "https" && resolved.Scheme != "http") || resolved.Host == "" {
		return "", fmt.Errorf("OpenCode Console sent an unusable verification URI %q", target)
	}
	return resolved.String(), nil
}

// newLoginState is the session key CPA stores the login under; it must pass
// CPA's ValidateOAuthState (at most 128 of [A-Za-z0-9._-]).
func newLoginState() (string, error) {
	var buf [16]byte
	if _, errRead := rand.Read(buf[:]); errRead != nil {
		return "", fmt.Errorf("generate login state: %w", errRead)
	}
	return "ocgo-" + hex.EncodeToString(buf[:]), nil
}

func seconds(value float64, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return time.Duration(value * float64(time.Second))
}

func (d *deviceFlows) pruneLocked(now time.Time) {
	for state, flow := range d.flows {
		if now.After(flow.expiresAt.Add(finishedLoginKept)) {
			delete(d.flows, state)
		}
	}
}

// flow returns the login for state, rebuilding it from the session
// metadata when this process did not start it.
func (d *deviceFlows) flow(state string, metadata map[string]any) *deviceLogin {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	d.pruneLocked(now)
	if flow, ok := d.flows[state]; ok {
		return flow
	}
	flow := &deviceLogin{interval: defaultPollInterval, nextPoll: now, expiresAt: now.Add(defaultDeviceExpiry)}
	if value, ok := metadata[sessionInterval].(float64); ok {
		flow.interval = seconds(value, defaultPollInterval)
	}
	if value, ok := metadata[sessionExpiresAt].(string); ok {
		if parsed, errParse := time.Parse(time.RFC3339, value); errParse == nil {
			flow.expiresAt = parsed
		}
	}
	d.flows[state] = flow
	return flow
}

func pollPending() pluginapi.AuthLoginPollResponse {
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}
}

func (flow *deviceLogin) finish(resp pluginapi.AuthLoginPollResponse) pluginapi.AuthLoginPollResponse {
	flow.result = &resp
	return resp
}

func pollFailed(message string) pluginapi.AuthLoginPollResponse {
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: redactSecrets(message)}
}

// poll makes at most one token request per call, and none while the
// Console's interval has not passed: Management Center polls every three
// seconds, and the Console asks for five or more.
func (d *deviceFlows) poll(ctx context.Context, h host, cfg settings, state string, metadata map[string]any) pluginapi.AuthLoginPollResponse {
	flow := d.flow(state, metadata)
	flow.mu.Lock()
	defer flow.mu.Unlock()
	if flow.result != nil {
		return *flow.result
	}
	deviceCode, _ := metadata[sessionDeviceCode].(string)
	if strings.TrimSpace(deviceCode) == "" {
		return flow.finish(pollFailed("This login session has no device code. Start the login again."))
	}
	now := d.now()
	if !now.Before(flow.expiresAt) {
		return flow.finish(pollFailed("The login code expired before it was approved. Start the login again."))
	}
	if now.Before(flow.nextPoll) {
		return pollPending()
	}
	flow.nextPoll = now.Add(flow.interval)
	if console, ok := metadata[sessionConsoleURL].(string); ok && strings.TrimSpace(console) != "" {
		cfg.ConsoleURL = strings.TrimRight(strings.TrimSpace(console), "/")
	}
	token, errToken := requestToken(ctx, h, cfg, url.Values{
		"grant_type":  {deviceGrantType},
		"device_code": {deviceCode},
		"client_id":   {loginClientID},
	})
	if errToken != nil {
		var oauth *oauthError
		if !errors.As(errToken, &oauth) || oauth.Status >= 500 || oauth.Status == http.StatusTooManyRequests {
			h.Log("warn", "OpenCode login poll failed; retrying at the next interval", map[string]any{"error": errToken.Error()})
			return pollPending()
		}
		switch oauth.Code {
		case "authorization_pending":
			return pollPending()
		case "slow_down":
			flow.interval += slowDownStep
			flow.nextPoll = now.Add(flow.interval)
			return pollPending()
		case "access_denied":
			return flow.finish(pollFailed("The login was denied on the OpenCode approval page."))
		case "expired_token":
			return flow.finish(pollFailed("The login code expired before it was approved. Start the login again."))
		default:
			return flow.finish(pollFailed("OpenCode refused the login: " + oauth.Error()))
		}
	}
	login, errLogin := completeLogin(ctx, h, cfg, token, now)
	if errLogin != nil {
		return flow.finish(pollFailed(errLogin.Error()))
	}
	auth, errAuth := login.loginAuthData()
	if errAuth != nil {
		return flow.finish(pollFailed(errAuth.Error()))
	}
	logins.rotated(login, login)
	return flow.finish(pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "Logged in to " + login.displayName(),
		Auth:    auth,
	})
}

// completeLogin turns a device token into a login, naming it after the
// workspace and account when the Console says who they are.
func completeLogin(ctx context.Context, h host, cfg settings, token tokenResponse, now time.Time) (loginCredential, error) {
	login := loginCredential{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		ExpiresAt:    token.expiresAt(now),
		Go:           true,
		extra:        make(map[string]json.RawMessage),
	}
	if token.OrgID != nil {
		login.OrgID = strings.TrimSpace(*token.OrgID)
	}
	account, errAccount := lookupAccount(ctx, h, cfg, token.AccessToken)
	if errAccount != nil {
		h.Log("warn", "OpenCode account lookup failed; the login is named by its workspace id", map[string]any{"error": errAccount.Error()})
	}
	login.Email = account.Email
	if login.OrgID == "" {
		login.OrgID = account.defaultOrg()
	}
	if login.OrgID == "" {
		return loginCredential{}, errors.New("OpenCode did not say which workspace this login is for. Start the login again and pick a workspace.")
	}
	login.Workspace = account.orgName(login.OrgID)
	return login, nil
}

type consoleAccount struct {
	Email string
	Orgs  []consoleOrg
}

type consoleOrg struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (a consoleAccount) orgName(id string) string {
	for _, org := range a.Orgs {
		if org.ID == id {
			return strings.TrimSpace(org.Name)
		}
	}
	return ""
}

// defaultOrg picks the workspace OpenCode's own client falls back to when
// the token names none: the first by name.
func (a consoleAccount) defaultOrg() string {
	orgs := append([]consoleOrg(nil), a.Orgs...)
	sort.Slice(orgs, func(i, j int) bool {
		if orgs[i].Name != orgs[j].Name {
			return orgs[i].Name < orgs[j].Name
		}
		return orgs[i].ID < orgs[j].ID
	})
	if len(orgs) == 0 {
		return ""
	}
	return orgs[0].ID
}

// lookupAccount reads the signed-in user and their workspaces. Either may
// fail; the login works without them.
func lookupAccount(ctx context.Context, doer httpDoer, cfg settings, accessToken string) (consoleAccount, error) {
	var account consoleAccount
	var user struct {
		Email string `json:"email"`
	}
	errUser := getConsoleJSON(ctx, doer, cfg, "/api/user", accessToken, &user)
	account.Email = strings.TrimSpace(user.Email)
	errOrgs := getConsoleJSON(ctx, doer, cfg, "/api/orgs", accessToken, &account.Orgs)
	return account, errors.Join(errUser, errOrgs)
}

func getConsoleJSON(ctx context.Context, doer httpDoer, cfg settings, path, accessToken string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, consoleRequestTimeout)
	defer cancel()
	resp, errDo := doer.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     cfg.consoleURL() + path,
		Headers: goRequestHeaders(cfg, accessToken),
	})
	if errDo != nil {
		return fmt.Errorf("read %s: %w", path, errDo)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("read %s: %w", path, parseUpstreamError(resp.StatusCode, resp.Headers, resp.Body))
	}
	if errDecode := json.Unmarshal(resp.Body, out); errDecode != nil {
		return fmt.Errorf("decode %s: %w", path, errDecode)
	}
	return nil
}
