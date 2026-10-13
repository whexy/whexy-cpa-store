package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// withPluginState points the route handlers at a test state.
func withPluginState(t *testing.T, state *pluginState) {
	t.Helper()
	previous := pluginStateRef
	pluginStateRef = state
	t.Cleanup(func() { pluginStateRef = previous })
}

func TestStateRouteDescribesTheGroups(t *testing.T) {
	host := newFakeHost()
	host.addProvider("claude", 2, credentialReady)
	host.addProvider("opencode-go", 1, credentialCooling)
	state := newTestState(t, multiProviderConfig, host)
	withPluginState(t, state)

	for range 4 {
		routeFor(state, "auto-group/fast", nil, []string{"claude", "opencode-go"})
	}
	status, body, errHandle := handleState(pluginapi.ManagementRequest{})
	if errHandle != nil {
		t.Fatalf("state route: %v", errHandle)
	}
	if status != http.StatusOK {
		t.Fatalf("state route status %d", status)
	}
	view, ok := body.(stateView)
	if !ok {
		t.Fatalf("state route body %T", body)
	}
	if view.Prefix != defaultPrefix {
		t.Fatalf("prefix %q", view.Prefix)
	}
	if len(view.RegisteredModels) != 1 || view.RegisteredModels[0] != "auto-group/fast" {
		t.Fatalf("registered models %v", view.RegisteredModels)
	}
	if len(view.Groups) != 1 {
		t.Fatalf("groups %d", len(view.Groups))
	}
	group := view.Groups[0]
	if group.Slug != "fast" || group.ID != "auto-group/fast" || group.Policy != policyWeightedRR || !group.Enabled {
		t.Fatalf("group %+v", group)
	}
	if group.Requests1h != 4 {
		t.Fatalf("group requests %d, want 4", group.Requests1h)
	}
	if len(group.Members) != 2 {
		t.Fatalf("members %d", len(group.Members))
	}
	if group.Members[0].Share <= group.Members[1].Share {
		t.Fatalf("weighted member shares %v/%v", group.Members[0].Share, group.Members[1].Share)
	}
	total := group.Members[0].Share + group.Members[1].Share
	if total < 0.999 || total > 1.001 {
		t.Fatalf("shares add up to %f", total)
	}
	if group.Members[0].State != "ready" || group.Members[0].Credentials != 2 {
		t.Fatalf("first member %+v", group.Members[0])
	}
	if group.Members[1].Credentials != 1 {
		t.Fatalf("second member %+v", group.Members[1])
	}
}

func TestStateRouteWireKeys(t *testing.T) {
	host := newFakeHost()
	host.addProvider("claude", 1, credentialReady)
	host.addProvider("opencode-go", 1, credentialReady)
	state := newTestState(t, multiProviderConfig, host)
	withPluginState(t, state)

	_, body, _ := handleState(pluginapi.ManagementRequest{})
	raw, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	// The page reads these keys; the contract is part of the plugin.
	for _, key := range []string{
		`"prefix"`, `"config_error"`, `"affinity"`, `"enabled"`, `"rebind_on_failure"`,
		`"ttl_seconds"`, `"registered_models"`, `"providers"`, `"groups"`, `"slug"`, `"id"`,
		`"name"`, `"policy"`, `"requests_1h"`, `"sessions"`, `"members"`, `"model"`,
		`"providers":["claude"]`, `"weight"`, `"share"`, `"state"`, `"state_detail"`,
		`"credentials"`, `"last_error"`, `"last_used"`,
	} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("state response has no %s: %s", key, raw)
		}
	}
}

func TestSessionsRouteReportsTheBoundAccountAndReleasesIt(t *testing.T) {
	host := newFakeHost()
	host.addProvider("p1", 1, credentialReady)
	host.addProvider("p2", 1, credentialReady)
	state := newTestState(t, roundRobinConfig, host)
	withPluginState(t, state)

	headers := map[string][]string{"X-Claude-Code-Session-Id": {"session-a"}}
	resp, handled := routeFor(state, "auto-group/cycle", headers, []string{"p1", "p2"})
	if !handled {
		t.Fatalf("the group request was not routed")
	}
	session := sessionIdentity(pluginapiRouteRequest(map[string][]string{"X-Claude-Code-Session-Id": {"session-a"}}))
	host.addRuntime("7", "p1", "seven@example.com")
	observeAccount(t, "auto-group/cycle", "session-a", "p1-key-1", "7")

	status, body, errHandle := handleSessions(pluginapi.ManagementRequest{Query: url.Values{"group": {"cycle"}}})
	if errHandle != nil || status != http.StatusOK {
		t.Fatalf("sessions route: %v, status %d", errHandle, status)
	}
	sessions, ok := body.(map[string]any)["sessions"].([]sessionView)
	if !ok {
		t.Fatalf("sessions body %T", body)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions %d, want 1", len(sessions))
	}
	entry := sessions[0]
	if entry.Group != "cycle" || entry.Member != resp.TargetModel || entry.Provider != resp.Target {
		t.Fatalf("session %+v", entry)
	}
	if entry.SessionFull != session {
		t.Fatalf("session_full %q, want %q", entry.SessionFull, session)
	}
	if entry.Session == session || !strings.Contains(entry.Session, "…") {
		t.Fatalf("display session %q was not shortened", entry.Session)
	}
	if entry.Account.State != accountBound {
		t.Fatalf("account state %q", entry.Account.State)
	}
	if !strings.Contains(entry.Account.Label, "seven@example.com") {
		t.Fatalf("account label %q", entry.Account.Label)
	}
	if entry.Account.AuthIndex != "7" {
		t.Fatalf("account auth index %q", entry.Account.AuthIndex)
	}

	releaseBody, _ := json.Marshal(map[string]any{"group": "cycle", "session": session})
	statusRelease, released, errRelease := handleSessionsRelease(pluginapi.ManagementRequest{Body: releaseBody})
	if errRelease != nil || statusRelease != http.StatusOK {
		t.Fatalf("release route: %v, status %d", errRelease, statusRelease)
	}
	if count, ok := released.(map[string]int)["released"]; !ok || count != 1 {
		t.Fatalf("release answered %v", released)
	}
	if len(state.bindings) != 0 {
		t.Fatalf("bindings %d after the release, want 0", len(state.bindings))
	}
}

func TestAccountIsRecordedOnlyForGroupRequestsWithASession(t *testing.T) {
	host := newFakeHost()
	host.addProvider("p1", 1, credentialReady)
	host.addProvider("p2", 1, credentialReady)
	state := newTestState(t, roundRobinConfig, host)
	withPluginState(t, state)

	// A model this plugin does not own, and a group request without a
	// session, are both ignored.
	observeAccount(t, "gpt-5.1-codex", "session-a", "key-1", "1")
	observeAccount(t, "auto-group/cycle", "", "key-1", "1")
	if len(state.accounts) != 0 {
		t.Fatalf("recorded %d accounts, want none", len(state.accounts))
	}

	routeFor(state, "auto-group/cycle", map[string][]string{"X-Claude-Code-Session-Id": {"session-a"}}, []string{"p1", "p2"})
	observeAccount(t, "auto-group/cycle", "session-a", "p1-key-1", "2")
	if len(state.accounts) != 1 {
		t.Fatalf("recorded %d accounts, want 1", len(state.accounts))
	}

	// The session list reports it, with the credential's own label.
	host.addRuntime("2", "p1", "two@example.com")
	_, body, errHandle := handleSessions(pluginapi.ManagementRequest{Query: url.Values{"group": {"cycle"}}})
	if errHandle != nil {
		t.Fatalf("sessions route: %v", errHandle)
	}
	sessions := body.(map[string]any)["sessions"].([]sessionView)
	if sessions[0].Account.State != accountBound || !strings.Contains(sessions[0].Account.Label, "two@example.com") {
		t.Fatalf("account %+v", sessions[0].Account)
	}
}

func TestMemberStateUsesTheProvidersCPAReports(t *testing.T) {
	// The credential cache cannot see keys that live in config.yaml, so a
	// member whose provider CPA lists as available must not read as
	// unavailable just because the cache is empty.
	host := newFakeHost()
	state := newTestState(t, roundRobinConfig, host)
	withPluginState(t, state)

	routeFor(state, "auto-group/cycle", nil, []string{"p1", "p2"})
	_, body, errHandle := handleState(pluginapi.ManagementRequest{})
	if errHandle != nil {
		t.Fatalf("state route: %v", errHandle)
	}
	view := body.(stateView)
	if view.Groups[0].Members[0].State != "ready" {
		t.Fatalf("member state %q, want ready", view.Groups[0].Members[0].State)
	}
	if len(view.Providers) != 2 {
		t.Fatalf("providers %v, want the two CPA reported", view.Providers)
	}

	// Once CPA stops listing both providers, the member reads as unavailable.
	routeFor(state, "auto-group/cycle", nil, []string{})
	_, body, _ = handleState(pluginapi.ManagementRequest{})
	view = body.(stateView)
	if view.Groups[0].Members[0].State != "unavailable" {
		t.Fatalf("member state %q, want unavailable", view.Groups[0].Members[0].State)
	}
}

func TestSessionsRouteRejectsAnUnknownGroup(t *testing.T) {
	host := newFakeHost()
	state := newTestState(t, roundRobinConfig, host)
	withPluginState(t, state)

	if _, _, errHandle := handleSessions(pluginapi.ManagementRequest{Query: url.Values{"group": {"nope"}}}); errHandle == nil {
		t.Fatalf("an unknown group was accepted")
	}
	if _, _, errHandle := handleSessions(pluginapi.ManagementRequest{Query: url.Values{"group": {"nope"}}}); !isAPIError(errHandle) {
		t.Fatalf("failure %T, want an apiError", errHandle)
	}
}

func isAPIError(err error) bool {
	var failure *apiError
	return errors.As(err, &failure)
}

func TestPageAndLogoRoutes(t *testing.T) {
	page := routeManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: resourcePagePath})
	if page.StatusCode != http.StatusOK || len(page.Body) == 0 {
		t.Fatalf("page status %d, %d bytes", page.StatusCode, len(page.Body))
	}
	if got := page.Headers.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("page content type %q", got)
	}
	csp := page.Headers.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'sha256-") || !strings.Contains(csp, "style-src 'sha256-") {
		t.Fatalf("page CSP %q does not hash both blocks", csp)
	}
	if !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("page CSP %q", csp)
	}

	logo := routeManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: logoPath})
	if logo.StatusCode != http.StatusOK || !strings.Contains(string(logo.Body), "<svg") {
		t.Fatalf("logo status %d", logo.StatusCode)
	}

	missing := routeManagement(pluginapi.ManagementRequest{Method: http.MethodPost, Path: statePath})
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route status %d, want 404", missing.StatusCode)
	}
}
