package main

import (
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestTokenCountersFollowProviderSemantics(t *testing.T) {
	cases := []struct {
		name   string
		record pluginapi.UsageRecord
		want   counters
	}{
		{
			name: "claude reports cache separately from input",
			record: pluginapi.UsageRecord{Provider: "claude", ExecutorType: "ClaudeExecutor", Detail: pluginapi.UsageDetail{
				InputTokens: 10, CacheReadTokens: 900, CacheCreationTokens: 50, CachedTokens: 50, OutputTokens: 40,
			}},
			want: counters{Input: 10, CacheRead: 900, CacheWrite: 50, Output: 40},
		},
		{
			name: "codex input includes cached tokens and output includes reasoning",
			record: pluginapi.UsageRecord{Provider: "codex", ExecutorType: "CodexExecutor", Detail: pluginapi.UsageDetail{
				InputTokens: 1000, CachedTokens: 800, CacheReadTokens: 800, OutputTokens: 60, ReasoningTokens: 20,
			}},
			want: counters{Input: 200, CacheRead: 800, Output: 60, Reasoning: 20},
		},
		{
			name: "openai-style parser that fills only CachedTokens",
			record: pluginapi.UsageRecord{Provider: "openai-compatibility", Detail: pluginapi.UsageDetail{
				InputTokens: 500, CachedTokens: 300, OutputTokens: 10,
			}},
			want: counters{Input: 200, CacheRead: 300, Output: 10},
		},
		{
			name: "gemini reasoning is separate from output",
			record: pluginapi.UsageRecord{Provider: "gemini-cli", Detail: pluginapi.UsageDetail{
				InputTokens: 700, CacheReadTokens: 200, OutputTokens: 30, ReasoningTokens: 70,
			}},
			want: counters{Input: 500, CacheRead: 200, Output: 100, Reasoning: 70},
		},
		{
			name: "unknown provider whose cache exceeds input is independent",
			record: pluginapi.UsageRecord{Provider: "some-plugin", Detail: pluginapi.UsageDetail{
				InputTokens: 5, CacheReadTokens: 400, OutputTokens: 7,
			}},
			want: counters{Input: 5, CacheRead: 400, Output: 7},
		},
		{
			name: "unknown provider defaults to cache inside input",
			record: pluginapi.UsageRecord{Provider: "opencode-go", Detail: pluginapi.UsageDetail{
				InputTokens: 400, CacheReadTokens: 100, OutputTokens: 7,
			}},
			want: counters{Input: 300, CacheRead: 100, Output: 7},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tokenCounters(tc.record); got != tc.want {
				t.Fatalf("tokenCounters = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestEventFromRecord(t *testing.T) {
	at := time.Date(2026, 10, 12, 15, 4, 5, 0, time.FixedZone("CDT", -5*3600))
	record := pluginapi.UsageRecord{
		Provider:    "Claude",
		Model:       "claude-opus-5-5",
		AuthIndex:   "a1b2c3",
		SessionID:   "session-1",
		Generate:    true,
		Failed:      true,
		RequestedAt: at,
		Detail:      pluginapi.UsageDetail{InputTokens: 3, OutputTokens: 4},
	}
	lookups := 0
	event, ok := eventFromRecord(record, func(index string) accountInfo {
		lookups++
		return accountInfo{Label: "me@example.com", Provider: "claude"}
	})
	if !ok || lookups != 1 {
		t.Fatalf("ok=%v lookups=%d", ok, lookups)
	}
	if !event.At.Equal(at) || event.At.Location() != time.UTC {
		t.Fatalf("At = %v, want %v in UTC", event.At, at)
	}
	if event.Provider != "claude" || event.Account != "a1b2c3" || event.Identity.Label != "me@example.com" {
		t.Fatalf("event = %+v", event)
	}
	if event.Session == "" || event.Session == "session-1" {
		t.Fatalf("session must be a digest, got %q", event.Session)
	}
	if event.Counters.Requests != 1 || event.Counters.Failed != 1 {
		t.Fatalf("counters = %+v", event.Counters)
	}

	record.Generate = false
	if _, ok := eventFromRecord(record, nil); ok {
		t.Fatal("a record without generation must be skipped")
	}
}

func TestAccountLabelHidesAPIKeys(t *testing.T) {
	cases := []struct {
		entry pluginapi.HostAuthFileEntry
		want  string
	}{
		{pluginapi.HostAuthFileEntry{AccountType: "oauth", Account: "me@example.com", Email: "me@example.com", Label: "Claude"}, "me@example.com"},
		{pluginapi.HostAuthFileEntry{AccountType: "api_key", Account: "sk-ant-0123456789abcdef", Label: "claude-apikey", Provider: "claude"}, "API key …cdef"},
		{pluginapi.HostAuthFileEntry{AccountType: "api_key", Account: "oc_sk_0123456789abcdef", Label: "Work key", Provider: "opencode-go"}, "Work key …cdef"},
		{pluginapi.HostAuthFileEntry{AccountType: "api_key", Account: "short", Label: "openrouter", Provider: "openai-compatible-openrouter"}, "API key"},
		{pluginapi.HostAuthFileEntry{Name: "codex-team.json"}, "codex-team"},
	}
	for _, tc := range cases {
		if got := accountLabel(tc.entry); got != tc.want {
			t.Errorf("accountLabel(%+v) = %q, want %q", tc.entry, got, tc.want)
		}
	}
}

func TestIdentityCacheRespectsTTL(t *testing.T) {
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	calls := 0
	failing := true
	cache := &identityCache{
		now: func() time.Time { return now },
		resolve: func(string) (pluginapi.HostAuthFileEntry, error) {
			calls++
			if failing {
				return pluginapi.HostAuthFileEntry{}, errors.New("not found")
			}
			return pluginapi.HostAuthFileEntry{Email: "a@b.c", Provider: "codex"}, nil
		},
	}
	if info := cache.lookup("x"); info.Label != "" || calls != 1 {
		t.Fatalf("first lookup: %+v calls=%d", info, calls)
	}
	cache.lookup("x")
	if calls != 1 {
		t.Fatalf("a failure is cached for a while, calls=%d", calls)
	}
	failing = false
	now = now.Add(identityFailureTTL)
	if info := cache.lookup("x"); info.Label != "a@b.c" || info.Provider != "codex" || calls != 2 {
		t.Fatalf("after failure TTL: %+v calls=%d", info, calls)
	}
	now = now.Add(identityTTL - time.Second)
	cache.lookup("x")
	if calls != 2 {
		t.Fatalf("success is cached for identityTTL, calls=%d", calls)
	}
}

func TestRecordIdentityWhenHostCannotNameTheAccount(t *testing.T) {
	cases := []struct {
		record pluginapi.UsageRecord
		want   accountInfo
	}{
		{pluginapi.UsageRecord{Provider: "openai-compatible-ai-proxy", AuthType: "apikey", Source: "sk-proxy-0000000000beef"}, accountInfo{Label: "API key …beef", Provider: "ai-proxy"}},
		{pluginapi.UsageRecord{Provider: "codex", AuthType: "oauth", Source: "me@example.com"}, accountInfo{Label: "me@example.com", Provider: "codex"}},
		{pluginapi.UsageRecord{Provider: "gemini", AuthType: "oauth", Source: "project-123"}, accountInfo{Provider: "gemini"}},
	}
	for _, tc := range cases {
		if got := recordIdentity(tc.record); got != tc.want {
			t.Errorf("recordIdentity(%+v) = %+v, want %+v", tc.record, got, tc.want)
		}
	}
	event, _ := eventFromRecord(pluginapi.UsageRecord{
		Generate: true, AuthIndex: "idx", Provider: "claude", AuthType: "apikey", Source: "sk-ant-api03-xxxxxxxxc0de",
	}, func(string) accountInfo { return accountInfo{} })
	if event.Identity.Label != "API key …c0de" || event.Identity.Provider != "claude" {
		t.Fatalf("identity = %+v", event.Identity)
	}
}
