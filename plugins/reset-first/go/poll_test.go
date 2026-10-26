package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestRefreshPollsNewAccounts(t *testing.T) {
	host := newFakeHost()
	host.install(t)
	current = newState()
	t.Cleanup(func() { current = newState() })
	host.auths = []pluginapi.HostAuthFileEntry{
		{ID: "codex-a", AuthIndex: "1", Provider: "codex", Email: "a@example.com"},
		{ID: "claude-b", AuthIndex: "2", Provider: "claude", Email: "b@example.com"},
		{ID: "codex-off", AuthIndex: "3", Provider: "codex", Disabled: true},
		{ID: "gemini-c", AuthIndex: "4", Provider: "gemini-cli"},
	}
	host.files["1"] = `{"type":"codex","access_token":"tok-a","account_id":"acct-a"}`
	host.files["2"] = `{"type":"claude","access_token":"tok-b"}`
	host.answer(codexUsageURL, 200, `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":5,"limit_window_seconds":18000,"reset_after_seconds":300},"secondary_window":{"used_percent":30,"limit_window_seconds":604800,"reset_after_seconds":200000}}}`)
	host.answer(claudeUsageURL, 401, `{"error":{"type":"authentication_error"}}`)

	refresh(context.Background())

	if len(host.requests) != 2 {
		t.Fatalf("requests = %+v, want one per enabled Codex or Claude account", host.requests)
	}
	codex := current.accounts["codex-a"]
	if codex.Quota == nil || codex.Plan != "pro" || codex.Label != "a@example.com" || codex.Problem != "" {
		t.Errorf("codex account = %+v", codex)
	}
	// The 5-hour window resets in 5 minutes, before the poll interval ends.
	if wait := time.Until(codex.nextPoll); wait > 8*time.Minute || wait < 6*time.Minute {
		t.Errorf("next poll in %v, want just after the 5h reset", wait)
	}
	claude := current.accounts["claude-b"]
	if claude.Quota != nil || !strings.Contains(claude.Problem, "401") {
		t.Errorf("claude account = %+v", claude)
	}

	for _, req := range host.requests {
		switch req.URL {
		case codexUsageURL:
			if req.Headers.Get("Authorization") != "Bearer tok-a" || req.Headers.Get("Chatgpt-Account-Id") != "acct-a" {
				t.Errorf("codex headers = %v", req.Headers)
			}
		case claudeUsageURL:
			if req.Headers.Get("Authorization") != "Bearer tok-b" || req.Headers.Get("Anthropic-Beta") != "oauth-2025-04-20" {
				t.Errorf("claude headers = %v", req.Headers)
			}
		}
		if req.OperationID == "" {
			t.Errorf("request to %s carries no operation, so it cannot be cancelled", req.URL)
		}
	}

	// Nothing is due on the next round.
	refresh(context.Background())
	if len(host.requests) != 2 {
		t.Errorf("a second round sent %d requests", len(host.requests)-2)
	}
}

func TestPollSkipsAccountsWithTheirOwnRoute(t *testing.T) {
	host := newFakeHost()
	host.install(t)
	host.files["1"] = `{"access_token":"tok","proxy_url":"socks5://10.0.0.1:1080"}`
	result := poll(context.Background(), pollTarget{id: "a", authIndex: "1", provider: providerCodex}, t0)
	if result.quota != nil || !strings.Contains(result.problem, "proxy") || len(host.requests) != 0 {
		t.Fatalf("result = %+v, requests %d", result, len(host.requests))
	}
	host.files["2"] = `{"refresh_token":"only"}`
	if result := poll(context.Background(), pollTarget{id: "b", authIndex: "2", provider: providerClaude}, t0); !strings.Contains(result.problem, "access token") {
		t.Errorf("result = %+v", result)
	}
}

func TestPollIsOffWithoutAnInterval(t *testing.T) {
	host := newFakeHost()
	host.install(t)
	current = newState()
	t.Cleanup(func() {
		current = newState()
		currentConfig.Store(nil)
	})
	currentConfig.Store(&settings{})
	host.auths = []pluginapi.HostAuthFileEntry{{ID: "codex-a", AuthIndex: "1", Provider: "codex"}}
	host.files["1"] = `{"access_token":"tok"}`
	refresh(context.Background())
	if len(host.requests) != 0 || current.accounts["codex-a"] == nil {
		t.Errorf("requests %d, accounts %v", len(host.requests), current.accounts)
	}
}
