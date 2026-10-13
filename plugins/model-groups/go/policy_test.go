package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestWeightedRoundRobinSharesTrafficByWeight(t *testing.T) {
	host := newFakeHost()
	host.addProvider("claude", 1, credentialReady)
	host.addProvider("opencode-go", 1, credentialReady)
	state := newTestState(t, multiProviderConfig, host)

	counts := map[string]int{}
	for range 100 {
		resp, handled := routeFor(state, "auto-group/fast", nil, []string{"claude", "opencode-go"})
		if !handled {
			t.Fatalf("the group request was not routed")
		}
		counts[resp.TargetModel]++
	}
	if counts["claude-haiku-5-5"] != 75 || counts["kimi-k3"] != 25 {
		t.Fatalf("weighted round robin distributed %v, want 75/25", counts)
	}
}

func TestRoundRobinCyclesMembers(t *testing.T) {
	config := `
groups:
  - name: Cycle
    slug: cycle
    policy: round-robin
    members:
      - model: a
        providers: [p1]
      - model: b
        providers: [p1]
      - model: c
        providers: [p1]
`
	host := newFakeHost()
	host.addProvider("p1", 1, credentialReady)
	state := newTestState(t, config, host)

	var order []string
	for range 6 {
		resp, handled := routeFor(state, "auto-group/cycle", nil, []string{"p1"})
		if !handled {
			t.Fatalf("the group request was not routed")
		}
		order = append(order, resp.TargetModel)
	}
	want := []string{"a", "b", "c", "a", "b", "c"}
	for index := range want {
		if order[index] != want[index] {
			t.Fatalf("round robin order %v, want %v", order, want)
		}
	}
}

func TestFillFirstKeepsTheFirstMember(t *testing.T) {
	config := `
groups:
  - name: Primary
    slug: primary
    policy: fill-first
    members:
      - model: first
        providers: [p1]
      - model: second
        providers: [p2]
`
	host := newFakeHost()
	host.addProvider("p1", 1, credentialReady)
	host.addProvider("p2", 1, credentialReady)
	state := newTestState(t, config, host)

	for range 5 {
		resp, handled := routeFor(state, "auto-group/primary", nil, []string{"p1", "p2"})
		if !handled {
			t.Fatalf("the group request was not routed")
		}
		if resp.TargetModel != "first" {
			t.Fatalf("fill first chose %q, want first", resp.TargetModel)
		}
	}
}

func TestFillFirstSkipsAMemberWithoutCredentials(t *testing.T) {
	config := `
groups:
  - name: Primary
    slug: primary
    policy: fill-first
    members:
      - model: first
        providers: [p1]
      - model: second
        providers: [p2]
`
	host := newFakeHost()
	host.addProvider("p1", 1, credentialDisabled)
	host.addProvider("p2", 1, credentialReady)
	state := newTestState(t, config, host)

	resp, handled := routeFor(state, "auto-group/primary", nil, []string{"p2"})
	if !handled {
		t.Fatalf("the group request was not routed")
	}
	if resp.TargetModel != "second" || resp.Target != "p2" {
		t.Fatalf("chose %s@%s, want second@p2", resp.TargetModel, resp.Target)
	}
}

func TestBreakerMovesFreshSessionsOffAFailingMember(t *testing.T) {
	config := `
groups:
  - name: Primary
    slug: primary
    policy: fill-first
    members:
      - model: first
        providers: [p1]
      - model: second
        providers: [p2]
`
	host := newFakeHost()
	host.addProvider("p1", 1, credentialReady)
	host.addProvider("p2", 1, credentialReady)
	state := newTestState(t, config, host)

	resp, _ := routeFor(state, "auto-group/primary", nil, []string{"p1", "p2"})
	if resp.TargetModel != "first" {
		t.Fatalf("first request chose %q", resp.TargetModel)
	}
	for range memberBreakerThreshold {
		state.observeCompletion(pluginapi.RequestCompletion{
			RequestedModel: "auto-group/primary",
			Model:          "first",
			Outcome:        pluginapi.RequestCompletionFailed,
			StatusCode:     429,
			Error:          "rate limited",
		})
	}
	resp, handled := routeFor(state, "auto-group/primary", nil, []string{"p1", "p2"})
	if !handled {
		t.Fatalf("the group request was not routed")
	}
	if resp.TargetModel != "second" {
		t.Fatalf("after the breaker tripped the request chose %q, want second", resp.TargetModel)
	}
	if state.groups[0].stats[0].breakerRemaining("p1", time.Now()) <= 0 {
		t.Fatalf("the failing provider has no breaker window")
	}
}

func TestSessionKeepsItsMemberAndProviderAcrossFailures(t *testing.T) {
	host := newFakeHost()
	host.addProvider("claude", 1, credentialReady)
	host.addProvider("opencode-go", 1, credentialReady)
	state := newTestState(t, multiProviderConfig, host)

	headers := map[string][]string{"X-Claude-Code-Session-Id": {"session-a"}}
	first, _ := routeFor(state, "auto-group/fast", headers, []string{"claude", "opencode-go"})
	for range 4 {
		state.observeCompletion(pluginapi.RequestCompletion{
			RequestedModel: "auto-group/fast",
			Model:          first.TargetModel,
			Outcome:        pluginapi.RequestCompletionFailed,
			StatusCode:     429,
			Error:          "rate limited",
		})
	}
	again, handled := routeFor(state, "auto-group/fast", headers, []string{"claude", "opencode-go"})
	if !handled {
		t.Fatalf("the group request was not routed")
	}
	if again.TargetModel != first.TargetModel || again.Target != first.Target {
		t.Fatalf("the session moved from %s@%s to %s@%s", first.TargetModel, first.Target, again.TargetModel, again.Target)
	}
}

func TestRebindOnFailureMovesTheSession(t *testing.T) {
	config := `
affinity:
  rebind_on_failure: true
groups:
  - name: Primary
    slug: primary
    policy: fill-first
    members:
      - model: first
        providers: [p1]
      - model: second
        providers: [p2]
`
	host := newFakeHost()
	host.addProvider("p1", 1, credentialReady)
	host.addProvider("p2", 1, credentialReady)
	state := newTestState(t, config, host)

	headers := map[string][]string{"X-Claude-Code-Session-Id": {"session-a"}}
	first, _ := routeFor(state, "auto-group/primary", headers, []string{"p1", "p2"})
	state.observeCompletion(pluginapi.RequestCompletion{
		RequestedModel: "auto-group/primary",
		Model:          first.TargetModel,
		Outcome:        pluginapi.RequestCompletionFailed,
		StatusCode:     500,
		Error:          "upstream failed",
	})
	again, handled := routeFor(state, "auto-group/primary", headers, []string{"p1", "p2"})
	if !handled {
		t.Fatalf("the group request was not routed")
	}
	if again.TargetModel != "second" {
		t.Fatalf("the session stayed on %q, want second", again.TargetModel)
	}
}

func TestSessionKeepsItsMemberWhenItsProviderLosesCredentials(t *testing.T) {
	config := `
groups:
  - name: Wide
    slug: wide
    policy: fill-first
    members:
      - model: shared
        providers: [p1, p2]
`
	host := newFakeHost()
	host.addProvider("p1", 1, credentialReady)
	host.addProvider("p2", 1, credentialReady)
	state := newTestState(t, config, host)

	headers := map[string][]string{"X-Claude-Code-Session-Id": {"session-a"}}
	first, _ := routeFor(state, "auto-group/wide", headers, []string{"p1", "p2"})
	if first.Target != "p1" {
		t.Fatalf("first request used provider %q, want p1", first.Target)
	}
	// p1 loses its credentials: the session must stay on the same model, so
	// the same account binding survives.
	again, handled := routeFor(state, "auto-group/wide", headers, []string{"p2"})
	if !handled {
		t.Fatalf("the group request was not routed")
	}
	if again.TargetModel != "shared" || again.Target != "p2" {
		t.Fatalf("chose %s@%s, want shared@p2", again.TargetModel, again.Target)
	}
}

func TestUnknownModelIsNotHandled(t *testing.T) {
	host := newFakeHost()
	host.addProvider("claude", 1, credentialReady)
	state := newTestState(t, multiProviderConfig, host)

	for _, model := range []string{"claude-haiku-5-5", "auto-group", "", "auto-group/unknown"} {
		if _, handled := routeFor(state, model, nil, []string{"claude"}); handled {
			t.Fatalf("model %q was handled, want it left to CLIProxyAPI", model)
		}
	}
}

func TestDisabledGroupIsNotRouted(t *testing.T) {
	config := `
groups:
  - name: Off
    slug: off
    enabled: false
    members:
      - model: first
        providers: [p1]
`
	host := newFakeHost()
	host.addProvider("p1", 1, credentialReady)
	state := newTestState(t, config, host)

	if _, handled := routeFor(state, "auto-group/off", nil, []string{"p1"}); handled {
		t.Fatalf("a disabled group was routed")
	}
	if ids := state.settingsSnapshot().registeredModelIDs(); len(ids) != 0 {
		t.Fatalf("a disabled group was registered: %v", ids)
	}
}

func TestThinkingSuffixIsPreserved(t *testing.T) {
	host := newFakeHost()
	host.addProvider("claude", 1, credentialReady)
	host.addProvider("opencode-go", 1, credentialReady)
	state := newTestState(t, multiProviderConfig, host)

	resp, handled := routeFor(state, "auto-group/fast(high)", nil, []string{"claude", "opencode-go"})
	if !handled {
		t.Fatalf("a suffixed group request was not routed")
	}
	if resp.TargetModel != "claude-haiku-5-5(high)" {
		t.Fatalf("target model %q, want claude-haiku-5-5(high)", resp.TargetModel)
	}
	if resp.TargetKind != pluginapi.ModelRouteTargetProvider {
		t.Fatalf("target kind %q, want provider", resp.TargetKind)
	}
}

func TestRoutingSurvivesWhenNoProviderHasCredentials(t *testing.T) {
	host := newFakeHost()
	state := newTestState(t, multiProviderConfig, host)

	resp, handled := routeFor(state, "auto-group/fast", nil, nil)
	if !handled {
		t.Fatalf("the group request was dropped instead of being routed best effort")
	}
	if resp.Target != "claude" || resp.TargetModel != "claude-haiku-5-5" {
		t.Fatalf("chose %s@%s, want the first member", resp.TargetModel, resp.Target)
	}
}
