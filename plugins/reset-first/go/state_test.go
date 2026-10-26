package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestStateSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	s := newState()
	s.load(dir)
	codexAccount(s, "a", 48*time.Hour, 0)
	codexAccount(s, "b", 96*time.Hour, 0)
	s.pick(pickRequest("gpt-5", "a", "b"), time.Now())
	s.persist(time.Now())

	restarted := newState()
	restarted.load(dir)
	d := restarted.decisions["gpt-5"]
	if d == nil || d.Chosen != "a" || len(d.Accounts) != 2 {
		t.Fatalf("decision after restart = %+v", d)
	}
	if a := restarted.accounts["a"]; a == nil || a.Quota == nil || a.Label != "a@example.com" {
		t.Fatalf("account after restart = %+v", a)
	}
	// The model stays on its account rather than deciding afresh.
	codexAccount(restarted, "b", 47*time.Hour+30*time.Minute, 0)
	if resp := restarted.pick(pickRequest("gpt-5", "a", "b"), time.Now()); resp.AuthID != "a" {
		t.Errorf("pick after restart = %+v, want a kept", resp)
	}
}

func TestStateIgnoresAnUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	if errWrite := os.WriteFile(filepath.Join(dir, stateFileName), []byte("{"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	s := newState()
	s.load(dir)
	if len(s.decisions) != 0 || len(s.accounts) != 0 {
		t.Errorf("state = %+v", s)
	}
}

func TestPruneForgetsTheHistory(t *testing.T) {
	s := newState()
	codexAccount(s, "a", 48*time.Hour, 0)
	s.pick(pickRequest("old", "a"), t0)
	s.accountLocked("gone", "gemini")
	s.syncAccounts([]pluginapi.HostAuthFileEntry{{ID: "listed", Provider: "gemini"}}, t0)

	s.pruneLocked(t0.Add(history + time.Minute))
	if s.decisions["old"] != nil {
		t.Error("a decision older than the history was kept")
	}
	if s.accounts["gone"] != nil {
		t.Error("an account nothing refers to was kept")
	}
	if s.accounts["listed"] == nil {
		t.Error("an account CPA lists was dropped")
	}
}

func TestSyncAccountsSchedulesPolls(t *testing.T) {
	s := newState()
	s.syncAccounts([]pluginapi.HostAuthFileEntry{
		{ID: "a", AuthIndex: "1", Provider: "codex", Email: "a@example.com"},
		{ID: "b", AuthIndex: "2", Type: "claude", Label: "Team"},
		{ID: "c", AuthIndex: "3", Provider: "codex", Disabled: true},
	}, t0)
	due := s.pollDue(t0)
	if len(due) != 2 {
		t.Fatalf("due = %+v, want the two enabled accounts", due)
	}
	if s.accounts["b"].Provider != "claude" || s.accounts["b"].Label != "Team" {
		t.Errorf("account b = %+v", s.accounts["b"])
	}
	s.applyPoll(due[0], pollResult{problem: "x"}, t0, defaultPollInterval)
	s.applyPoll(due[1], pollResult{problem: "x"}, t0, defaultPollInterval)
	if again := s.pollDue(t0.Add(time.Minute)); len(again) != 0 {
		t.Errorf("due again after a minute: %+v", again)
	}
	if again := s.pollDue(t0.Add(defaultPollInterval)); len(again) != 2 {
		t.Errorf("due after the interval: %+v", again)
	}
}
