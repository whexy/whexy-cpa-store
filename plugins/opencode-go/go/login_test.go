package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	testOrgID         = "wrk_TESTLOGIN000000000000000"
	testAccessToken   = "st_test_access_0000000000000000000001"
	testRefreshToken  = "rt_test_refresh_000000000000000000001"
	testAccessToken2  = "st_test_access_0000000000000000000002"
	testRefreshToken2 = "rt_test_refresh_000000000000000000002"
	testAccessToken3  = "st_test_access_0000000000000000000003"
	testRefreshToken3 = "rt_test_refresh_000000000000000000003"
)

var testLoginExpiry = time.Date(2099, 1, 31, 0, 0, 0, 0, time.UTC)

// loginFileJSON is a login file as auth.login.poll saves it, plus fields
// the host and the user add.
func loginFileJSON(t *testing.T, access, refresh string, expires time.Time, extra map[string]any) []byte {
	t.Helper()
	file := map[string]any{
		"type":          "opencode-go",
		"auth_method":   "oauth",
		"access_token":  access,
		"refresh_token": refresh,
		"expires_at":    expires.UTC().Format(time.RFC3339),
		"org_id":        testOrgID,
		"label":         "Acme",
		"email":         "dev@example.test",
	}
	for name, value := range extra {
		file[name] = value
	}
	return mustJSON(t, file)
}

// useLoginStore gives the test its own token memory and clock.
func useLoginStore(t *testing.T, now time.Time) *loginStore {
	t.Helper()
	previous := logins
	store := newLoginStore()
	store.now = func() time.Time { return now }
	logins = store
	t.Cleanup(func() { logins = previous })
	return store
}

// fakeConsole answers the OpenCode Console endpoints the login code calls.
type fakeConsole struct {
	mu         sync.Mutex
	device     gatewayReply
	token      func(form url.Values) gatewayReply
	user       gatewayReply
	orgs       gatewayReply
	tokenForms []url.Values
	deviceForm url.Values
}

func newFakeConsole() *fakeConsole {
	return &fakeConsole{
		device: consoleJSON(200, `{"device_code":"devcode0000000000000000000000000000000000","user_code":"ABCD-EFGH","verification_uri":"/console/device","verification_uri_complete":"/console/device?user_code=ABCD-EFGH&client_id=whexy-cpa-opencode-go","expires_in":600,"interval":5}`),
		token: func(url.Values) gatewayReply {
			return consoleJSON(400, `{"error":"authorization_pending"}`)
		},
		user: consoleJSON(200, `{"id":"usr_1","email":"dev@example.test"}`),
		orgs: consoleJSON(200, `[{"id":"wrk_OTHER","name":"Zeta"},{"id":"`+testOrgID+`","name":"Acme"}]`),
	}
}

func consoleJSON(status int, body string) gatewayReply {
	return gatewayReply{Status: status, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
}

func tokenJSON(access, refresh string, orgID string) gatewayReply {
	org := `null`
	if orgID != "" {
		org = `"` + orgID + `"`
	}
	return consoleJSON(200, `{"access_token":"`+access+`","refresh_token":"`+refresh+`","token_type":"Bearer","expires_in":2591999,"org_id":`+org+`}`)
}

func (c *fakeConsole) reply(t *testing.T, next func(req hostHTTPRequest) gatewayReply) func(req hostHTTPRequest) gatewayReply {
	return func(req hostHTTPRequest) gatewayReply {
		form, _ := url.ParseQuery(string(req.Body))
		c.mu.Lock()
		defer c.mu.Unlock()
		switch req.URL {
		case "https://opencode.ai/console/auth/device/code":
			c.deviceForm = form
			return c.device
		case "https://opencode.ai/console/auth/device/token":
			if req.Method != http.MethodPost || req.Headers.Get("Content-Type") != "application/x-www-form-urlencoded" {
				t.Errorf("token request %s %v", req.Method, req.Headers)
			}
			c.tokenForms = append(c.tokenForms, form)
			return c.token(form)
		case "https://opencode.ai/console/api/user":
			return c.user
		case "https://opencode.ai/console/api/orgs":
			return c.orgs
		}
		if next != nil {
			return next(req)
		}
		t.Errorf("unexpected upstream request %s", req.URL)
		return consoleJSON(404, `{"error":"not_found"}`)
	}
}

func (c *fakeConsole) tokenCalls() []url.Values {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]url.Values(nil), c.tokenForms...)
}

func TestLoginFileRoundTrip(t *testing.T) {
	raw := loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, map[string]any{"priority": 3, "disabled": true, "proxy_url": "socks5://p:1"})
	if !isLoginFile(raw) || !isKeyFile(raw) {
		t.Fatal("login file not recognized")
	}
	login, errParse := parseLoginFile(raw)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if login.AccessToken != testAccessToken || login.RefreshToken != testRefreshToken || !login.ExpiresAt.Equal(testLoginExpiry) ||
		login.OrgID != testOrgID || login.Workspace != "Acme" || login.Email != "dev@example.test" || !login.Disabled {
		t.Fatalf("login = %+v", login)
	}
	out, errMarshal := login.marshal()
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var top map[string]any
	if errUnmarshal := json.Unmarshal(out, &top); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if top["type"] != "opencode-go" || top["auth_method"] != "oauth" || top["priority"] != float64(3) || top["proxy_url"] != "socks5://p:1" || top["disabled"] != true {
		t.Fatalf("marshal = %s", out)
	}
	again, errAgain := parseLoginFile(out)
	if errAgain != nil || again.AccessToken != login.AccessToken || !again.ExpiresAt.Equal(login.ExpiresAt) || len(again.extra) != 2 {
		t.Fatalf("round trip = %+v, %v", again, errAgain)
	}
	for name, raw := range map[string]string{
		"no tokens":   `{"type":"opencode-go","auth_method":"oauth","org_id":"wrk_1"}`,
		"no org":      `{"type":"opencode-go","auth_method":"oauth","access_token":"a","refresh_token":"b"}`,
		"bad expires": `{"type":"opencode-go","auth_method":"oauth","access_token":"a","refresh_token":"b","org_id":"w","expires_at":"soon"}`,
	} {
		if _, errBad := parseLoginFile([]byte(raw)); errBad == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if isLoginFile([]byte(`{"type":"opencode-go","api_key":"` + testKeyA + `"}`)) {
		t.Fatal("key file taken for a login")
	}
}

func TestParseAuthsLoginFile(t *testing.T) {
	useSettings(t, defaultSettings())
	name := "opencode-go-oauth-" + testOrgID + ".json"
	raw := loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, nil)
	// OpenCode Go only, by default: one ordinary file auth.
	resp := parseAuths(pluginapi.AuthParseRequest{Path: "/auths/" + name, RawJSON: raw})
	if !resp.Handled || len(resp.Auths) != 0 || resp.Auth.ID != name || resp.Auth.Label != "Acme" || resp.Auth.Attributes["family"] != "go" || string(resp.Auth.StorageJSON) != string(raw) {
		t.Fatalf("resp = %+v", resp)
	}
	both := loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, map[string]any{"zen": true})
	resp = parseAuths(pluginapi.AuthParseRequest{Path: "/auths/" + name, RawJSON: both})
	if !resp.Handled || len(resp.Auths) != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	goAuth, zenAuth := resp.Auths[0], resp.Auths[1]
	if goAuth.ID != name || goAuth.Provider != "opencode-go" || goAuth.Label != "Acme" || goAuth.Disabled {
		t.Fatalf("go auth = %+v", goAuth)
	}
	if zenAuth.ID != name+"#zen" || zenAuth.Label != "Acme · OpenCode Zen" || zenAuth.Attributes["family"] != "zen" {
		t.Fatalf("zen auth = %+v", zenAuth)
	}
	for _, auth := range resp.Auths {
		if string(auth.StorageJSON) != string(both) {
			t.Fatalf("storage = %s", auth.StorageJSON)
		}
		if auth.Metadata["access_token"] != testAccessToken || auth.Metadata["refresh_token"] != testRefreshToken || len(auth.Metadata) != 2 {
			t.Fatalf("metadata = %v", auth.Metadata)
		}
		for name, value := range map[string]string{"auth_kind": "oauth", "org_id": testOrgID, "email": "dev@example.test"} {
			if auth.Attributes[name] != value {
				t.Fatalf("attributes = %v", auth.Attributes)
			}
		}
		// CPA never writes back a file that yields two auths, so it must not
		// rotate the login's tokens on its own schedule; the plugin does.
		if _, ok := auth.Attributes["refresh_interval_seconds"]; ok || !auth.NextRefreshAfter.IsZero() {
			t.Fatalf("refresh schedule = %v, %v", auth.Attributes, auth.NextRefreshAfter)
		}
	}

	broken := parseAuths(pluginapi.AuthParseRequest{FileName: "x.json", RawJSON: []byte(`{"type":"opencode-go","auth_method":"oauth"}`)})
	if !broken.Handled || !broken.Auth.Disabled || broken.Auth.ID != "x.json" || len(broken.Auth.Metadata) != 0 || len(broken.Auths) != 0 {
		t.Fatalf("broken = %+v", broken)
	}
}

func TestLoginFamiliesRoundTrip(t *testing.T) {
	login, errParse := parseLoginFile(loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, nil))
	if errParse != nil || login.families() != (familySet{Go: true}) {
		t.Fatalf("default = %+v, %v", login.families(), errParse)
	}
	login.Go, login.Zen = false, true
	raw, _ := login.marshal()
	again, _ := parseLoginFile(raw)
	if again.families() != (familySet{Zen: true}) || len(again.extra) != 0 {
		t.Fatalf("round trip = %+v (%s)", again, raw)
	}
}

func TestResolveCredentialForLogin(t *testing.T) {
	cred, errResolve := resolveCredential(loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, nil), nil, nil)
	if errResolve != nil {
		t.Fatal(errResolve)
	}
	if cred.ID != "login:"+testOrgID || cred.Bearer != testAccessToken || cred.Name != "Acme · dev@example.test" {
		t.Fatalf("cred = %+v", cred)
	}
	expiredAt := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	cred, errResolve = resolveCredential(loginFileJSON(t, testAccessToken, testRefreshToken, expiredAt, nil), nil, nil)
	var expired *loginExpiredError
	if !errors.As(errResolve, &expired) || cred.ID != "login:"+testOrgID || !strings.Contains(errResolve.Error(), "log in again") {
		t.Fatalf("expired = %+v, %v", cred, errResolve)
	}
	key, errKey := resolveCredential([]byte(`{"type":"opencode-go","api_key":"`+testKeyA+`"}`), nil, nil)
	if errKey != nil || key.ID != keyIDFor(testKeyA) || key.Bearer != testKeyA {
		t.Fatalf("key = %+v, %v", key, errKey)
	}
}

// refreshRequest mirrors what the host sends: StorageJSON is the file with
// the auth metadata merged over it, and Metadata is that auth metadata.
func refreshRequest(t *testing.T, access, refresh string) []byte {
	t.Helper()
	metadata := map[string]any{"type": "opencode-go", "disabled": false, "priority": float64(3), "access_token": access, "refresh_token": refresh}
	storage := loginFileJSON(t, access, refresh, testLoginExpiry, metadata)
	return mustJSON(t, rpcAuthRefreshRequest{
		AuthRefreshRequest: pluginapi.AuthRefreshRequest{
			AuthID:       "opencode-go-oauth-" + testOrgID + ".json",
			AuthProvider: "opencode-go",
			StorageJSON:  storage,
			Metadata:     metadata,
		},
		HostCallbackID: "cb-refresh",
	})
}

// persistedFile is what the host writes after a refresh: StorageJSON with
// the metadata laid over it.
func persistedFile(t *testing.T, auth pluginapi.AuthData) loginCredential {
	t.Helper()
	merged := map[string]any{}
	if errUnmarshal := json.Unmarshal(auth.StorageJSON, &merged); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	for name, value := range auth.Metadata {
		merged[name] = value
	}
	login, errParse := parseLoginFile(mustJSON(t, merged))
	if errParse != nil {
		t.Fatal(errParse)
	}
	return login
}

func TestAuthRefreshRotatesAndPersistsTokens(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	useLoginStore(t, now)
	useSettings(t, defaultSettings())
	console := newFakeConsole()
	console.token = func(form url.Values) gatewayReply {
		switch form.Get("refresh_token") {
		case testRefreshToken:
			return tokenJSON(testAccessToken2, testRefreshToken2, testOrgID)
		case testRefreshToken2:
			return tokenJSON(testAccessToken3, testRefreshToken3, testOrgID)
		}
		return consoleJSON(400, `{"error":"invalid_grant"}`)
	}
	installFakeGateway(t, console.reply(t, nil))

	refresh := func(access, refresh string) pluginapi.AuthRefreshResponse {
		t.Helper()
		raw, errHandle := handleMethod(pluginabi.MethodAuthRefresh, refreshRequest(t, access, refresh))
		if errHandle != nil {
			t.Fatal(errHandle)
		}
		return decodeResult[pluginapi.AuthRefreshResponse](t, raw)
	}

	first := refresh(testAccessToken, testRefreshToken)
	calls := console.tokenCalls()
	if len(calls) != 1 || calls[0].Get("grant_type") != "refresh_token" || calls[0].Get("refresh_token") != testRefreshToken || calls[0].Get("client_id") != "whexy-cpa-opencode-go" {
		t.Fatalf("token calls = %v", calls)
	}
	saved := persistedFile(t, first.Auth)
	wantExpiry := now.Add(2591999 * time.Second)
	if saved.AccessToken != testAccessToken2 || saved.RefreshToken != testRefreshToken2 || !saved.ExpiresAt.Equal(wantExpiry) ||
		saved.OrgID != testOrgID || saved.Workspace != "Acme" || saved.Email != "dev@example.test" || string(saved.extra["priority"]) != "3" {
		t.Fatalf("persisted = %+v", saved)
	}
	if first.Auth.Metadata["priority"] != float64(3) || first.Auth.Metadata["type"] != "opencode-go" {
		t.Fatalf("host metadata dropped: %v", first.Auth.Metadata)
	}
	if first.Auth.ID != "" || first.Auth.Label != "" || first.Auth.FileName != "" {
		t.Fatalf("refresh renames the auth: %+v", first.Auth)
	}
	if !first.NextRefreshAfter.IsZero() {
		t.Fatalf("next refresh = %v", first.NextRefreshAfter)
	}

	// The host handing back the spent token gets the tokens it bought,
	// without a request that would revoke the session.
	stale := refresh(testAccessToken, testRefreshToken)
	if got := persistedFile(t, stale.Auth); got.RefreshToken != testRefreshToken2 || got.AccessToken != testAccessToken2 || len(console.tokenCalls()) != 1 {
		t.Fatalf("stale refresh = %+v after %d calls", got, len(console.tokenCalls()))
	}

	second := refresh(testAccessToken2, testRefreshToken2)
	if got := persistedFile(t, second.Auth); got.RefreshToken != testRefreshToken3 || len(console.tokenCalls()) != 2 {
		t.Fatalf("second refresh = %+v after %d calls", got, len(console.tokenCalls()))
	}
	// A token two rotations old resolves to the newest tokens.
	if got := persistedFile(t, refresh(testAccessToken, testRefreshToken).Auth); got.RefreshToken != testRefreshToken3 || len(console.tokenCalls()) != 2 {
		t.Fatalf("oldest refresh = %+v", got)
	}
}

func TestAuthRefreshInvalidGrantNeedsNewLogin(t *testing.T) {
	useLoginStore(t, time.Now())
	useSettings(t, defaultSettings())
	console := newFakeConsole()
	console.token = func(url.Values) gatewayReply {
		return consoleJSON(400, `{"error":"invalid_grant","error_description":"refresh token `+testRefreshToken+` was revoked"}`)
	}
	installFakeGateway(t, console.reply(t, nil))
	for attempt := 0; attempt < 2; attempt++ {
		raw, errHandle := handleMethod(pluginabi.MethodAuthRefresh, refreshRequest(t, testAccessToken, testRefreshToken))
		if errHandle != nil {
			t.Fatal(errHandle)
		}
		env := decodeEnvelope(t, raw)
		if env.OK || env.Error.HTTPStatus != http.StatusUnauthorized || !strings.Contains(env.Error.Message, "invalid_grant") || !strings.Contains(env.Error.Message, "log in again") {
			t.Fatalf("attempt %d = %s", attempt, raw)
		}
		if strings.Contains(string(raw), testRefreshToken) {
			t.Fatalf("error leaks the refresh token: %s", raw)
		}
	}
	if calls := console.tokenCalls(); len(calls) != 1 {
		t.Fatalf("rejected token sent %d times", len(calls))
	}
}

func TestAuthRefreshTransientFailureIsRetried(t *testing.T) {
	useLoginStore(t, time.Now())
	useSettings(t, defaultSettings())
	console := newFakeConsole()
	failing := true
	console.token = func(url.Values) gatewayReply {
		if failing {
			return consoleJSON(503, `upstream unavailable`)
		}
		return tokenJSON(testAccessToken2, testRefreshToken2, testOrgID)
	}
	installFakeGateway(t, console.reply(t, nil))
	raw, _ := handleMethod(pluginabi.MethodAuthRefresh, refreshRequest(t, testAccessToken, testRefreshToken))
	if env := decodeEnvelope(t, raw); env.OK || env.Error.HTTPStatus != http.StatusBadGateway || strings.Contains(env.Error.Message, "invalid_grant") {
		t.Fatalf("transient = %s", raw)
	}
	failing = false
	raw, _ = handleMethod(pluginabi.MethodAuthRefresh, refreshRequest(t, testAccessToken, testRefreshToken))
	if got := persistedFile(t, decodeResult[pluginapi.AuthRefreshResponse](t, raw).Auth); got.RefreshToken != testRefreshToken2 {
		t.Fatalf("retry = %+v", got)
	}
}

// A login file yields two auths, which CPA never writes back, so a refresh
// saves the rotated tokens before CPA gets them. CPA then parses the file
// again and both auths carry them.
func TestAuthRefreshSavesLoginFile(t *testing.T) {
	fx := newManagementFixture(t, map[string]string{
		testLoginName: string(loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, map[string]any{"go_priority": 5})),
	})
	useLoginStore(t, time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC))
	console := newFakeConsole()
	console.token = func(form url.Values) gatewayReply {
		if form.Get("refresh_token") == testRefreshToken {
			return tokenJSON(testAccessToken2, testRefreshToken2, testOrgID)
		}
		return consoleJSON(400, `{"error":"invalid_grant"}`)
	}
	fx.serveConsole(console)
	refresh := func() pluginapi.AuthRefreshResponse {
		t.Helper()
		raw, errHandle := handleMethod(pluginabi.MethodAuthRefresh, mustJSON(t, rpcAuthRefreshRequest{AuthRefreshRequest: pluginapi.AuthRefreshRequest{
			AuthID:      testLoginName + zenAuthSuffix,
			StorageJSON: loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, nil),
			Attributes:  map[string]string{familyAttribute: "zen"},
			Metadata:    map[string]any{"access_token": testAccessToken, "refresh_token": testRefreshToken},
		}}))
		if errHandle != nil {
			t.Fatal(errHandle)
		}
		return decodeResult[pluginapi.AuthRefreshResponse](t, raw)
	}
	refresh()
	saved, _ := parseLoginFile([]byte(fx.files[testLoginName]))
	if saved.AccessToken != testAccessToken2 || saved.RefreshToken != testRefreshToken2 || string(saved.extra["go_priority"]) != "5" || fx.saves() != 1 {
		t.Fatalf("saved = %+v after %d saves", saved, fx.saves())
	}
	// The other auth's stale token is answered from memory, and the file
	// already holds the tokens.
	if got := persistedFile(t, refresh().Auth); got.RefreshToken != testRefreshToken2 || len(console.tokenCalls()) != 1 || fx.saves() != 1 {
		t.Fatalf("second refresh = %+v, %d token calls, %d saves", got, len(console.tokenCalls()), fx.saves())
	}
}

// The plugin refreshes each enabled login at half its token's life, once
// per workspace, and saves every file still holding a spent token.
func TestRefreshDueLogins(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	const (
		dueOrg      = "wrk_DUE"
		laterOrg    = "wrk_LATER"
		disabledOrg = "wrk_OFF"
	)
	login := func(org string, expires time.Time, extra map[string]any) string {
		fields := map[string]any{"org_id": org}
		for name, value := range extra {
			fields[name] = value
		}
		return string(loginFileJSON(t, "st_"+org+"_access_00000000001", "rt_"+org+"_refresh_0000000001", expires, fields))
	}
	fx := newManagementFixture(t, map[string]string{
		"opencode-go-oauth-" + dueOrg + ".json":      login(dueOrg, now.Add(14*24*time.Hour), nil),
		"opencode-go-oauth-" + laterOrg + ".json":    login(laterOrg, now.Add(16*24*time.Hour), nil),
		"opencode-go-oauth-" + disabledOrg + ".json": login(disabledOrg, now.Add(time.Hour), map[string]any{"disabled": true}),
	})
	store := useLoginStore(t, now)
	console := newFakeConsole()
	console.token = func(form url.Values) gatewayReply {
		org := strings.TrimSuffix(strings.TrimPrefix(form.Get("refresh_token"), "rt_"), "_refresh_0000000001")
		return tokenJSON("st_"+org+"_access_00000000002", "rt_"+org+"_refresh_0000000002", "")
	}
	fx.serveConsole(console)

	refreshDueLogins(context.Background(), newHost(""))
	calls := console.tokenCalls()
	if len(calls) != 1 || calls[0].Get("refresh_token") != "rt_"+dueOrg+"_refresh_0000000001" {
		t.Fatalf("token calls = %v", calls)
	}
	due, _ := parseLoginFile([]byte(fx.files["opencode-go-oauth-"+dueOrg+".json"]))
	if due.RefreshToken != "rt_"+dueOrg+"_refresh_0000000002" || !due.ExpiresAt.Equal(now.Add(2591999*time.Second)) || fx.saves() != 1 {
		t.Fatalf("due login = %+v after %d saves", due, fx.saves())
	}
	refreshDueLogins(context.Background(), newHost(""))
	if len(console.tokenCalls()) != 1 || fx.saves() != 1 {
		t.Fatalf("a fresh login was refreshed again: %d calls, %d saves", len(console.tokenCalls()), fx.saves())
	}

	// A refresh CPA asked for whose file was not saved is saved on the next
	// check, without another refresh.
	later, _ := parseLoginFile([]byte(fx.files["opencode-go-oauth-"+laterOrg+".json"]))
	next := later
	next.RefreshToken = "rt_" + laterOrg + "_refresh_0000000002"
	store.rotated(later, next)
	refreshDueLogins(context.Background(), newHost(""))
	if saved, _ := parseLoginFile([]byte(fx.files["opencode-go-oauth-"+laterOrg+".json"])); saved.RefreshToken != next.RefreshToken || len(console.tokenCalls()) != 1 {
		t.Fatalf("later login = %+v after %d calls", saved, len(console.tokenCalls()))
	}
}

func TestLoginIdentitySurvivesRotation(t *testing.T) {
	before, _ := resolveCredential(loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, nil), nil, nil)
	after, _ := resolveCredential(loginFileJSON(t, testAccessToken2, testRefreshToken2, testLoginExpiry.Add(time.Hour), nil), nil, nil)
	if before.ID != after.ID || before.Bearer == after.Bearer {
		t.Fatalf("before %+v after %+v", before, after)
	}
	tracker, _, _ := useExhaustion(t, time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	tracker.block(before.ID, "auth-1", time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC), []string{"weekly"})
	if _, blocked := tracker.active(after.ID, "auth-1"); !blocked {
		t.Fatal("rotation lifted the usage block")
	}
}

// CPA skips oauth-excluded-models, which the user's model-disable rules for
// opencode-go rely on, for auths it classifies as API keys: those with an
// api_key attribute or an auth_kind of apikey.
func TestAuthDataNeverClassifiesAsAPIKey(t *testing.T) {
	useLoginStore(t, time.Now())
	useSettings(t, defaultSettings())
	console := newFakeConsole()
	console.token = func(url.Values) gatewayReply { return tokenJSON(testAccessToken2, testRefreshToken2, testOrgID) }
	installFakeGateway(t, console.reply(t, nil))

	login := loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, nil)
	auths := map[string]pluginapi.AuthData{}
	for index, auth := range parsedAuths("opencode-go.json", strings.Replace(string(multiKeyFile(t)), `"label": "Work",`, `"label": "Work", "zen": true,`, 1)) {
		auths["key "+strconv.Itoa(index)] = auth
	}
	for index, auth := range parsedAuths("k.json", `{"type":"opencode-go","api_key":"`+testKeyC+`","zen":true}`) {
		auths["single key "+strconv.Itoa(index)] = auth
	}
	for index, auth := range parsedAuths("l.json", string(loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, map[string]any{"zen": true}))) {
		auths["login parse "+strconv.Itoa(index)] = auth
	}
	parsed, _ := parseLoginFile(login)
	auths["login poll"], _ = parsed.loginAuthData()
	raw, _ := handleMethod(pluginabi.MethodAuthRefresh, refreshRequest(t, testAccessToken, testRefreshToken))
	auths["login refresh"] = decodeResult[pluginapi.AuthRefreshResponse](t, raw).Auth

	for name, auth := range auths {
		if _, ok := auth.Attributes["api_key"]; ok {
			t.Fatalf("%s has an api_key attribute: %v", name, auth.Attributes)
		}
		kind, _ := auth.Metadata["auth_kind"].(string)
		if strings.EqualFold(auth.Attributes["auth_kind"], "apikey") || strings.EqualFold(kind, "apikey") {
			t.Fatalf("%s is classified as an API key: %v %v", name, auth.Attributes, auth.Metadata)
		}
	}
	for _, name := range []string{"key 0", "key 1", "key 2", "key 3", "single key 0", "single key 1"} {
		if _, ok := auths[name].Attributes["auth_kind"]; ok {
			t.Fatalf("%s sets auth_kind: %v", name, auths[name].Attributes)
		}
	}
	for _, name := range []string{"login parse 0", "login parse 1", "login poll", "login refresh"} {
		if auths[name].Attributes["auth_kind"] != "oauth" {
			t.Fatalf("%s kind = %v", name, auths[name].Attributes)
		}
	}
}

func TestRedactSecretsCoversLoginTokens(t *testing.T) {
	text := redactSecrets("access " + testAccessToken + ", refresh " + testRefreshToken + ", key " + testKeyA)
	for _, secret := range []string{testAccessToken, testRefreshToken, testKeyA} {
		if strings.Contains(text, secret) {
			t.Fatalf("redacted = %q", text)
		}
	}
	if !strings.Contains(text, "st_[redacted]") || !strings.Contains(text, "rt_[redacted]") {
		t.Fatalf("redacted = %q", text)
	}
	if got := redactSecrets("first_name_is_long_enough_here"); got != "first_name_is_long_enough_here" {
		t.Fatalf("over-redacted %q", got)
	}
}
