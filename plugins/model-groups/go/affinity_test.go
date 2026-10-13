package main

import (
	"strings"
	"testing"
	"time"
)

// roundRobinConfig cycles two members, so a session that keeps its member is
// visibly different from a fresh decision.
const roundRobinConfig = `
groups:
  - name: Cycle
    slug: cycle
    policy: round-robin
    members:
      - model: first
        providers: [p1]
      - model: second
        providers: [p2]
`

func TestSessionKeepsItsMemberAcrossRequests(t *testing.T) {
	host := newFakeHost()
	host.addProvider("p1", 1, credentialReady)
	host.addProvider("p2", 1, credentialReady)
	state := newTestState(t, roundRobinConfig, host)
	available := []string{"p1", "p2"}

	sessionA := map[string][]string{"X-Claude-Code-Session-Id": {"session-a"}}
	first, _ := routeFor(state, "auto-group/cycle", sessionA, available)
	if first.TargetModel != "first" {
		t.Fatalf("the first request chose %q", first.TargetModel)
	}
	for range 3 {
		again, handled := routeFor(state, "auto-group/cycle", sessionA, available)
		if !handled {
			t.Fatalf("the request was not routed")
		}
		if again.TargetModel != "first" {
			t.Fatalf("the session moved to %q", again.TargetModel)
		}
	}

	// A different session still rotates.
	other, _ := routeFor(state, "auto-group/cycle", map[string][]string{"X-Claude-Code-Session-Id": {"session-b"}}, available)
	if other.TargetModel != "second" {
		t.Fatalf("a new session chose %q, want second", other.TargetModel)
	}
}

func TestBindingsSurviveAPluginReload(t *testing.T) {
	dir := t.TempDir()
	host := newFakeHost()
	host.addProvider("p1", 1, credentialReady)
	host.addProvider("p2", 1, credentialReady)
	host.install(t)
	available := []string{"p1", "p2"}

	before := newPluginState(dir)
	before.applyConfig([]byte(roundRobinConfig))
	sessionA := map[string][]string{"X-Claude-Code-Session-Id": {"session-a"}}
	sessionB := map[string][]string{"X-Claude-Code-Session-Id": {"session-b"}}
	if resp, _ := routeFor(before, "auto-group/cycle", sessionA, available); resp.TargetModel != "first" {
		t.Fatalf("session-a chose %q", resp.TargetModel)
	}
	if resp, _ := routeFor(before, "auto-group/cycle", sessionB, available); resp.TargetModel != "second" {
		t.Fatalf("session-b chose %q", resp.TargetModel)
	}
	before.persist()

	after := newPluginState(dir)
	after.applyConfig([]byte(roundRobinConfig))
	resp, handled := routeFor(after, "auto-group/cycle", sessionB, available)
	if !handled {
		t.Fatalf("the request was not routed after the reload")
	}
	if resp.TargetModel != "second" {
		t.Fatalf("session-b landed on %q after the reload, want second", resp.TargetModel)
	}
}

func TestPruneBindingsDropsStaleSessions(t *testing.T) {
	bindings := map[string]*binding{
		bindingKey("auto-group/g", "fresh"): {Group: "auto-group/g", Session: "fresh", LastUsed: time.Now()},
		bindingKey("auto-group/g", "stale"): {Group: "auto-group/g", Session: "stale", LastUsed: time.Now().Add(-2 * time.Hour)},
	}
	pruneBindings(bindings, time.Now(), time.Hour, maxSessionKeys)
	if _, ok := bindings[bindingKey("auto-group/g", "fresh")]; !ok {
		t.Fatalf("a fresh binding was pruned")
	}
	if _, ok := bindings[bindingKey("auto-group/g", "stale")]; ok {
		t.Fatalf("a stale binding survived")
	}
}

func TestPruneBindingsCapsTheStoredSessions(t *testing.T) {
	now := time.Now()
	bindings := map[string]*binding{}
	for index := range 10 {
		session := string(rune('a' + index))
		bindings[bindingKey("auto-group/g", session)] = &binding{
			Group:    "auto-group/g",
			Session:  session,
			LastUsed: now.Add(-time.Duration(index) * time.Minute),
		}
	}
	pruneBindings(bindings, now, time.Hour, 4)
	if len(bindings) != 4 {
		t.Fatalf("kept %d bindings, want 4", len(bindings))
	}
	if _, ok := bindings[bindingKey("auto-group/g", "a")]; !ok {
		t.Fatalf("the most recently used binding was dropped")
	}
	if _, ok := bindings[bindingKey("auto-group/g", "j")]; ok {
		t.Fatalf("the least recently used binding survived")
	}
}

func TestReleaseDropsOneSessionOrAWholeGroup(t *testing.T) {
	host := newFakeHost()
	host.addProvider("p1", 1, credentialReady)
	host.addProvider("p2", 1, credentialReady)
	state := newTestState(t, roundRobinConfig, host)
	available := []string{"p1", "p2"}

	for _, session := range []string{"one", "two"} {
		routeFor(state, "auto-group/cycle", map[string][]string{"X-Claude-Code-Session-Id": {session}}, available)
	}
	if len(state.bindings) != 2 {
		t.Fatalf("bindings %d, want 2", len(state.bindings))
	}
	// The page releases a session by the exact identity the sessions route
	// reports, not by the shortened form it displays.
	sessionOne := sessionIdentity(pluginapiRouteRequest(map[string][]string{"X-Claude-Code-Session-Id": {"one"}}))
	if released := state.release("auto-group/cycle", sessionOne, false); released != 1 {
		t.Fatalf("released %d, want 1", released)
	}
	if released := state.release("auto-group/cycle", "", true); released != 1 {
		t.Fatalf("released %d, want 1", released)
	}
	if len(state.bindings) != 0 {
		t.Fatalf("bindings %d, want 0", len(state.bindings))
	}
}

func TestSessionIdentityFollowsCPAExtraction(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string][]string
		carries string
	}{
		{"claude header", map[string][]string{"X-Claude-Code-Session-Id": {"claude-root-123"}}, "claude-root-123"},
		{"codex header", map[string][]string{"Session-Id": {"0198d5a1-0000-7000-8000-000000000000"}}, "0198d5a1-0000-7000-8000-000000000000"},
		{"affinity header", map[string][]string{"X-Session-Affinity": {"affinity-42"}}, "affinity-42"},
	}
	for _, entry := range cases {
		// CPA's own extraction namespaces the identity (for example
		// "claude:..."); all this plugin needs is the same value CPA uses, so
		// it must be stable and carry the client's session.
		first := sessionIdentity(pluginapiRouteRequest(entry.headers))
		second := sessionIdentity(pluginapiRouteRequest(entry.headers))
		if first == "" || first != second {
			t.Fatalf("%s: identity %q then %q", entry.name, first, second)
		}
		if !strings.Contains(first, entry.carries) {
			t.Fatalf("%s: identity %q does not carry %q", entry.name, first, entry.carries)
		}
	}
	if got := sessionIdentity(pluginapiRouteRequest(nil)); got != "" {
		t.Fatalf("a request without a session produced %q", got)
	}
}
