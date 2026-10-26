package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// codexAccount gives the state a Codex account read at t0 whose weekly window
// resets after week, with its 5-hour window used fiveUsed percent.
func codexAccount(s *state, id string, week time.Duration, fiveUsed float64) {
	a := s.accountLocked(id, providerCodex)
	a.Label = id + "@example.com"
	a.Quota = &quota{At: t0, Windows: []window{
		{Minutes: 300, Used: fiveUsed, ResetAt: t0.Add(3 * time.Hour)},
		{Minutes: 10080, Used: 50, ResetAt: t0.Add(week)},
	}}
}

func pickRequest(model string, ids ...string) pluginapi.SchedulerPickRequest {
	req := pluginapi.SchedulerPickRequest{Model: model}
	for _, id := range ids {
		req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: id, Provider: "codex", Priority: 0, Status: "active"})
	}
	return req
}

func TestPickPrefersTheSoonestReset(t *testing.T) {
	s := newState()
	codexAccount(s, "late", 5*24*time.Hour, 10)
	codexAccount(s, "soon", 2*24*time.Hour, 10)

	resp := s.pick(pickRequest("gpt-5-codex", "late", "soon"), t0)
	if !resp.Handled || resp.AuthID != "soon" {
		t.Fatalf("pick = %+v, want the account resetting in 2 days", resp)
	}
	d := s.decisions["gpt-5-codex"]
	if d.Reason != reasonSoonest || d.Chosen != "soon" || !d.Since.Equal(t0) {
		t.Errorf("decision = %+v", d)
	}
	if len(d.Accounts) != 2 || d.Accounts[0].ID != "soon" || d.Accounts[1].ID != "late" {
		t.Errorf("accounts = %+v, want the scheduler's order", d.Accounts)
	}
}

func TestPickPassesOverASpentWindow(t *testing.T) {
	s := newState()
	codexAccount(s, "late", 5*24*time.Hour, 10)
	codexAccount(s, "soon", 2*24*time.Hour, 100)

	resp := s.pick(pickRequest("gpt-5-codex", "soon", "late"), t0)
	if resp.AuthID != "late" || s.decisions["gpt-5-codex"].Reason != reasonOnlySpare {
		t.Fatalf("pick = %+v, decision %+v", resp, s.decisions["gpt-5-codex"])
	}
	// Once the 5-hour window resets, the model goes back.
	resp = s.pick(pickRequest("gpt-5-codex", "soon", "late"), t0.Add(3*time.Hour+time.Minute))
	if resp.AuthID != "soon" {
		t.Fatalf("after the 5h reset pick = %+v, want soon again", resp)
	}
}

// apiKeyAccounts adds accounts of a provider that reports no quota.
func apiKeyAccounts(s *state, ids ...string) {
	for _, id := range ids {
		s.accountLocked(id, "gemini")
	}
}

func TestPickLeavesUnknownQuotaToCPA(t *testing.T) {
	s := newState()
	apiKeyAccounts(s, "key-a", "key-b")
	resp := s.pick(pickRequest("gemini-3-pro", "key-a", "key-b"), t0)
	if resp.Handled {
		t.Fatalf("pick = %+v, want CPA to choose", resp)
	}
	if d := s.decisions["gemini-3-pro"]; !d.ByCPA || d.Reason != reasonNoQuota || d.Chosen != "" {
		t.Errorf("decision = %+v", d)
	}

	// An account with quota known goes before one whose quota is unknown.
	codexAccount(s, "known", 6*24*time.Hour, 0)
	resp = s.pick(pickRequest("gemini-3-pro", "key-a", "known"), t0)
	if resp.AuthID != "known" || s.decisions["gemini-3-pro"].Reason != reasonOnlySpare {
		t.Fatalf("pick = %+v", resp)
	}
}

func TestPickTrialsAnAccountNeverRead(t *testing.T) {
	s := newState()
	codexAccount(s, "known", 96*time.Hour, 0)
	if resp := s.pick(pickRequest("m", "known", "unread"), t0); resp.AuthID != "unread" || s.decisions["m"].Reason != reasonTrial {
		t.Fatalf("first pick = %+v, want a trial of the unread account", resp)
	}
	// One trial at a time: until its response is read, the model stays.
	if resp := s.pick(pickRequest("m", "known", "unread"), t0.Add(time.Minute)); resp.AuthID != "known" {
		t.Fatalf("pick during the trial = %+v", resp)
	}
	// A trial whose response brought no quota is tried again later.
	if resp := s.pick(pickRequest("m", "known", "unread"), t0.Add(trialSpacing)); resp.AuthID != "unread" {
		t.Fatalf("pick after the spacing = %+v", resp)
	}
	codexAccount(s, "unread", 200*time.Hour, 0)
	if resp := s.pick(pickRequest("m", "known", "unread"), t0.Add(2*trialSpacing)); resp.AuthID != "known" {
		t.Fatalf("pick once read = %+v", resp)
	}
	// Accounts whose provider reports no quota are never trialled.
	apiKeyAccounts(s, "key")
	if resp := s.pick(pickRequest("m", "known", "key"), t0.Add(3*trialSpacing)); resp.AuthID != "known" {
		t.Fatalf("pick with an API key = %+v", resp)
	}
}

func TestPickKeepsAnAccountWithinStickiness(t *testing.T) {
	s := newState()
	codexAccount(s, "a", 48*time.Hour, 0)
	codexAccount(s, "b", 49*time.Hour, 0)
	if resp := s.pick(pickRequest("m", "a", "b"), t0); resp.AuthID != "a" {
		t.Fatalf("first pick = %+v", resp)
	}
	// b now resets 30 minutes before a: not enough to move.
	codexAccount(s, "b", 47*time.Hour+30*time.Minute, 0)
	resp := s.pick(pickRequest("m", "a", "b"), t0)
	if resp.AuthID != "a" || s.decisions["m"].Reason != reasonKept {
		t.Fatalf("pick = %+v, decision %+v, want a kept", resp, s.decisions["m"])
	}
	// Two hours sooner is.
	codexAccount(s, "b", 46*time.Hour, 0)
	if resp := s.pick(pickRequest("m", "a", "b"), t0); resp.AuthID != "b" {
		t.Fatalf("pick = %+v, want b", resp)
	}
}

func TestPickSharesACardAcrossThinkingSuffixes(t *testing.T) {
	s := newState()
	codexAccount(s, "a", 48*time.Hour, 0)
	s.pick(pickRequest("gpt-5(high)", "a"), t0)
	s.pick(pickRequest("gpt-5", "a"), t0.Add(time.Minute))
	if len(s.decisions) != 1 || s.decisions["gpt-5"] == nil {
		t.Fatalf("decisions = %v", s.decisions)
	}
	if s.decisions["gpt-5"].Reason != reasonOnly {
		t.Errorf("reason = %q", s.decisions["gpt-5"].Reason)
	}
}

func TestDecisionKeepsAccountsNoLongerOffered(t *testing.T) {
	s := newState()
	codexAccount(s, "a", 48*time.Hour, 0)
	codexAccount(s, "b", 96*time.Hour, 0)
	s.pick(pickRequest("m", "a", "b"), t0)
	// a failed and CPA retries with b alone.
	s.pick(pickRequest("m", "b"), t0.Add(time.Second))
	d := s.decisions["m"]
	if d.Chosen != "b" || len(d.Accounts) != 2 {
		t.Fatalf("decision = %+v", d)
	}
	if !d.Accounts[0].Offered || d.Accounts[0].ID != "b" || d.Accounts[1].Offered || d.Accounts[1].ID != "a" {
		t.Errorf("accounts = %+v, want b offered and a listed after it", d.Accounts)
	}
	// After the history, a is forgotten.
	s.pick(pickRequest("m", "b"), t0.Add(history+time.Minute))
	if len(s.decisions["m"].Accounts) != 1 {
		t.Errorf("accounts = %+v, want a gone", s.decisions["m"].Accounts)
	}
}

func TestHandlePickSpeaksTheHostWire(t *testing.T) {
	current = newState()
	t.Cleanup(func() { current = newState() })
	now := time.Now()
	a := current.accountLocked("codex-a", providerCodex)
	a.Quota = &quota{At: now, Windows: []window{{Minutes: 10080, Used: 1, ResetAt: now.Add(time.Hour)}}}

	// The host marshals pluginapi.SchedulerPickRequest, which has no JSON tags.
	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Model:      "gpt-5",
		Providers:  []string{"codex"},
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "codex-a", Provider: "codex", Attributes: map[string]string{"plan_type": "pro"}}},
	})
	out, errPick := handlePick(raw)
	if errPick != nil {
		t.Fatal(errPick)
	}
	var env struct {
		OK     bool
		Result pluginapi.SchedulerPickResponse
	}
	if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil || !env.OK {
		t.Fatalf("envelope %s: %v", out, errUnmarshal)
	}
	if !env.Result.Handled || env.Result.AuthID != "codex-a" {
		t.Errorf("result = %+v", env.Result)
	}
	if current.accounts["codex-a"].Plan != "pro" {
		t.Errorf("plan from the candidate's attributes = %q", current.accounts["codex-a"].Plan)
	}
}

func TestDescribeNamesAccountsHostAuthListOmits(t *testing.T) {
	s := newState()
	cases := []struct {
		id         string
		attributes map[string]string
		want       string
	}{
		{"codex:apikey:07f883a0ac93", map[string]string{"base_url": "https://relay.example.com/v1/"}, "relay.example.com/v1"},
		{"codex:apikey:07f883a0ac93-2", nil, "API key 07f883"},
		{"openai-compatibility:team:0", map[string]string{"compat_name": "team"}, "team"},
		{"plain-id", nil, ""},
	}
	for _, c := range cases {
		a := s.accountLocked(c.id, "codex")
		s.describeLocked(a, c.attributes)
		if a.Label != c.want {
			t.Errorf("label of %s = %q, want %q", c.id, a.Label, c.want)
		}
	}
}
