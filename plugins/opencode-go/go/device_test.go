package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// cpaValidOAuthState repeats ValidateOAuthState of CLIProxyAPI v8.0.27
// (internal/api/handlers/management/oauth_sessions.go), which a plugin's
// login state must pass.
func cpaValidOAuthState(state string) bool {
	if state == "" || len(state) > 128 || strings.ContainsAny(state, `/\`) || strings.Contains(state, "..") {
		return false
	}
	for _, r := range state {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !ok {
			return false
		}
	}
	return true
}

// useDeviceFlows gives the test its own pending logins and a clock it moves.
func useDeviceFlows(t *testing.T, start time.Time) (*deviceFlows, func(time.Duration)) {
	t.Helper()
	previous := devices
	flows := newDeviceFlows()
	now := start
	flows.now = func() time.Time { return now }
	devices = flows
	t.Cleanup(func() { devices = previous })
	return flows, func(step time.Duration) { now = now.Add(step) }
}

// startLogin runs auth.login.start and returns the response with its
// metadata passed through JSON, as the host stores and returns it.
func startLogin(t *testing.T) pluginapi.AuthLoginStartResponse {
	t.Helper()
	raw, errHandle := handleMethod(pluginabi.MethodAuthLoginStart, mustJSON(t, rpcAuthLoginStartRequest{
		AuthLoginStartRequest: pluginapi.AuthLoginStartRequest{Provider: "opencode-go", BaseURL: "http://127.0.0.1/v8/management/oauth/callback"},
		HostCallbackID:        "cb-start",
	}))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	return decodeResult[pluginapi.AuthLoginStartResponse](t, raw)
}

func pollLogin(t *testing.T, start pluginapi.AuthLoginStartResponse) pluginapi.AuthLoginPollResponse {
	t.Helper()
	raw, errHandle := handleMethod(pluginabi.MethodAuthLoginPoll, mustJSON(t, rpcAuthLoginPollRequest{
		AuthLoginPollRequest: pluginapi.AuthLoginPollRequest{Provider: "opencode-go", State: start.State, Metadata: start.Metadata},
		HostCallbackID:       "cb-poll",
	}))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	return decodeResult[pluginapi.AuthLoginPollResponse](t, raw)
}

func TestLoginStartReturnsVerificationURL(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	useDeviceFlows(t, now)
	useSettings(t, defaultSettings())
	console := newFakeConsole()
	gateway := installFakeGateway(t, console.reply(t, nil))

	start := startLogin(t)
	if start.URL != "https://opencode.ai/console/device?user_code=ABCD-EFGH&client_id=whexy-cpa-opencode-go" {
		t.Fatalf("url = %q", start.URL)
	}
	if !cpaValidOAuthState(start.State) || start.Provider != "opencode-go" || !start.ExpiresAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("start = %+v", start)
	}
	if start.Metadata["device_code"] != "devcode0000000000000000000000000000000000" || start.Metadata["interval_seconds"] != float64(5) ||
		start.Metadata["console_url"] != "https://opencode.ai/console" || start.Metadata["expires_at"] != "2026-10-12T12:10:00Z" {
		t.Fatalf("metadata = %v", start.Metadata)
	}
	if console.deviceForm.Get("client_id") != "whexy-cpa-opencode-go" || console.deviceForm.Get("supports_org_scope") != "true" {
		t.Fatalf("device form = %v", console.deviceForm)
	}
	req := gateway.requests[0]
	if req.Method != "POST" || req.HostCallbackID != "cb-start" || req.Headers.Get("Authorization") != "" {
		t.Fatalf("device request = %+v", req)
	}
	if again := startLogin(t); again.State == start.State {
		t.Fatal("two logins share a state")
	}
}

func TestLoginStartRejectsBadAnswers(t *testing.T) {
	useDeviceFlows(t, time.Now())
	useSettings(t, defaultSettings())
	for name, reply := range map[string]gatewayReply{
		"error":      consoleJSON(400, `{"error":"invalid_client"}`),
		"no code":    consoleJSON(200, `{"user_code":"X"}`),
		"script url": consoleJSON(200, `{"device_code":"d","verification_uri_complete":"javascript:alert(1)"}`),
	} {
		t.Run(name, func(t *testing.T) {
			console := newFakeConsole()
			console.device = reply
			installFakeGateway(t, console.reply(t, nil))
			raw, _ := handleMethod(pluginabi.MethodAuthLoginStart, []byte(`{}`))
			if env := decodeEnvelope(t, raw); env.OK || env.Error.HTTPStatus != 502 {
				t.Fatalf("start = %s", raw)
			}
		})
	}
}

func TestLoginPollRespectsIntervalAndSucceeds(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	_, advance := useDeviceFlows(t, now)
	useLoginStore(t, now)
	useSettings(t, defaultSettings())
	console := newFakeConsole()
	answers := []gatewayReply{
		consoleJSON(400, `{"error":"authorization_pending"}`),
		consoleJSON(400, `{"error":"slow_down"}`),
		tokenJSON(testAccessToken, testRefreshToken, testOrgID),
	}
	console.token = func(form url.Values) gatewayReply {
		if form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || form.Get("device_code") != "devcode0000000000000000000000000000000000" || form.Get("client_id") != "whexy-cpa-opencode-go" {
			t.Errorf("token form = %v", form)
		}
		reply := answers[0]
		answers = answers[1:]
		return reply
	}
	gateway := installFakeGateway(t, console.reply(t, nil))
	start := startLogin(t)

	expectPending := func(step time.Duration, calls int) {
		t.Helper()
		advance(step)
		if got := pollLogin(t, start); got.Status != pluginapi.AuthLoginStatusPending {
			t.Fatalf("poll = %+v", got)
		}
		if got := len(console.tokenCalls()); got != calls {
			t.Fatalf("token calls = %d, want %d", got, calls)
		}
	}
	expectPending(3*time.Second, 0) // Management Center's 3s poll comes before the 5s interval.
	expectPending(2*time.Second, 1) // authorization_pending
	expectPending(5*time.Second, 2) // slow_down: the interval becomes 10s
	expectPending(5*time.Second, 2)
	advance(5 * time.Second)
	done := pollLogin(t, start)
	if done.Status != pluginapi.AuthLoginStatusSuccess || len(console.tokenCalls()) != 3 {
		t.Fatalf("done = %+v", done)
	}
	auth := done.Auth
	name := "opencode-go-oauth-" + testOrgID + ".json"
	if auth.ID != name || auth.FileName != name || auth.Label != "Acme" || auth.Provider != "opencode-go" || auth.Disabled {
		t.Fatalf("auth = %+v", auth)
	}
	file, errParse := parseLoginFile(auth.StorageJSON)
	wantExpiry := now.Add(20*time.Second + 2591999*time.Second)
	if errParse != nil || file.AccessToken != testAccessToken || file.RefreshToken != testRefreshToken || !file.ExpiresAt.Equal(wantExpiry) ||
		file.OrgID != testOrgID || file.Workspace != "Acme" || file.Email != "dev@example.test" {
		t.Fatalf("file = %+v, %v (%s)", file, errParse, auth.StorageJSON)
	}
	// Saving a login merges the old file's non-token fields into metadata
	// that lacks them, so the metadata names every field.
	for field, value := range map[string]any{"access_token": testAccessToken, "refresh_token": testRefreshToken, "auth_method": "oauth",
		"expires_at": wantExpiry.Format(time.RFC3339), "org_id": testOrgID, "label": "Acme", "email": "dev@example.test"} {
		if auth.Metadata[field] != value {
			t.Fatalf("metadata %s = %v", field, auth.Metadata[field])
		}
	}
	if auth.Attributes["refresh_interval_seconds"] != "1296000" || !auth.NextRefreshAfter.Equal(wantExpiry.Add(-15*24*time.Hour)) {
		t.Fatalf("refresh schedule = %v, %v", auth.Attributes, auth.NextRefreshAfter)
	}
	for _, req := range gateway.requests {
		if strings.HasSuffix(req.URL, "/api/user") || strings.HasSuffix(req.URL, "/api/orgs") {
			if req.Headers.Get("Authorization") != "Bearer "+testAccessToken {
				t.Fatalf("account lookup headers = %v", req.Headers)
			}
		}
	}

	advance(time.Second)
	if again := pollLogin(t, start); again.Status != pluginapi.AuthLoginStatusSuccess || len(console.tokenCalls()) != 3 {
		t.Fatalf("second poll after success = %+v", again)
	}
}

func TestLoginPollFailures(t *testing.T) {
	cases := map[string]struct {
		reply    gatewayReply
		wait     time.Duration
		message  string
		upstream int
	}{
		"denied":    {reply: consoleJSON(400, `{"error":"access_denied"}`), wait: 5 * time.Second, message: "denied", upstream: 1},
		"expired":   {reply: consoleJSON(400, `{"error":"expired_token"}`), wait: 5 * time.Second, message: "expired", upstream: 1},
		"other":     {reply: consoleJSON(400, `{"error":"invalid_client","error_description":"unknown client"}`), wait: 5 * time.Second, message: "unknown client", upstream: 1},
		"too late":  {reply: consoleJSON(400, `{"error":"authorization_pending"}`), wait: 11 * time.Minute, message: "expired", upstream: 0},
		"transient": {reply: consoleJSON(502, `bad gateway`), wait: 5 * time.Second, upstream: 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, advance := useDeviceFlows(t, time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC))
			useSettings(t, defaultSettings())
			console := newFakeConsole()
			console.token = func(url.Values) gatewayReply { return tc.reply }
			installFakeGateway(t, console.reply(t, nil))
			start := startLogin(t)
			advance(tc.wait)
			got := pollLogin(t, start)
			if len(console.tokenCalls()) != tc.upstream {
				t.Fatalf("token calls = %d", len(console.tokenCalls()))
			}
			if tc.message == "" {
				if got.Status != pluginapi.AuthLoginStatusPending {
					t.Fatalf("poll = %+v", got)
				}
				return
			}
			if got.Status != pluginapi.AuthLoginStatusError || !strings.Contains(got.Message, tc.message) {
				t.Fatalf("poll = %+v", got)
			}
			advance(10 * time.Second)
			if again := pollLogin(t, start); again.Status != pluginapi.AuthLoginStatusError || len(console.tokenCalls()) != tc.upstream {
				t.Fatalf("finished login polled upstream again: %+v", again)
			}
		})
	}
}

func TestLoginPollAfterReloadUsesSessionMetadata(t *testing.T) {
	useDeviceFlows(t, time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC))
	useLoginStore(t, time.Now())
	useSettings(t, defaultSettings())
	console := newFakeConsole()
	console.token = func(url.Values) gatewayReply { return tokenJSON(testAccessToken, testRefreshToken, "") }
	console.user = consoleJSON(500, `{}`)
	installFakeGateway(t, console.reply(t, nil))
	start := startLogin(t)
	// A reloaded plugin knows nothing of the login but what the host keeps.
	useDeviceFlows(t, time.Date(2026, 10, 12, 12, 0, 1, 0, time.UTC))
	got := pollLogin(t, start)
	if got.Status != pluginapi.AuthLoginStatusSuccess || len(console.tokenCalls()) != 1 {
		t.Fatalf("poll = %+v", got)
	}
	// The token named no workspace: the first by name is used, as OpenCode's
	// client does, and the failed user lookup leaves the email empty.
	if got.Auth.FileName != "opencode-go-oauth-"+testOrgID+".json" || got.Auth.Label != "Acme" || got.Auth.Attributes["email"] != "" {
		t.Fatalf("auth = %+v", got.Auth)
	}
}

func TestLoginPollWithoutWorkspaceFails(t *testing.T) {
	_, advance := useDeviceFlows(t, time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC))
	useSettings(t, defaultSettings())
	console := newFakeConsole()
	console.token = func(url.Values) gatewayReply { return tokenJSON(testAccessToken, testRefreshToken, "") }
	console.orgs = consoleJSON(200, `[]`)
	installFakeGateway(t, console.reply(t, nil))
	start := startLogin(t)
	advance(5 * time.Second)
	got := pollLogin(t, start)
	if got.Status != pluginapi.AuthLoginStatusError || !strings.Contains(got.Message, "workspace") || strings.Contains(got.Message, testAccessToken) {
		t.Fatalf("poll = %+v", got)
	}
}

func TestLoginStartWireShape(t *testing.T) {
	useDeviceFlows(t, time.Now())
	useSettings(t, defaultSettings())
	installFakeGateway(t, newFakeConsole().reply(t, nil))
	raw, _ := handleMethod(pluginabi.MethodAuthLoginStart, []byte(`{}`))
	var wire struct {
		Result map[string]json.RawMessage `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &wire); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	for _, field := range []string{"Provider", "URL", "State", "ExpiresAt", "Metadata"} {
		if _, ok := wire.Result[field]; !ok {
			t.Fatalf("start result lacks %s: %s", field, raw)
		}
	}
}
