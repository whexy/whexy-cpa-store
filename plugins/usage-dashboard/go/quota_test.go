package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestQuotaFromClaudeHeaders(t *testing.T) {
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider:    "claude",
		AuthIndex:   "a1",
		RequestedAt: at,
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-5h-Utilization":    {"0.0"},
			"Anthropic-Ratelimit-Unified-5h-Reset":          {"1787296800"},
			"Anthropic-Ratelimit-Unified-7d-Utilization":    {"0.53"},
			"Anthropic-Ratelimit-Unified-7d-Reset":          {"1787695200"},
			"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"1.02"},
		},
	}
	sample, ok := quotaFromRecord(record)
	if !ok {
		t.Fatal("Claude headers must yield a sample")
	}
	want := quotaSample{At: at.UnixMilli(), Account: "a1", Provider: "claude", Windows: []quotaWindow{
		{ID: "5h", Remaining: 100, ResetAt: 1787296800000, PeriodMinutes: 300},
		{ID: "7d", Remaining: 47, ResetAt: 1787695200000, PeriodMinutes: 10080},
	}}
	if !reflect.DeepEqual(sample, want) {
		t.Fatalf("sample = %+v, want %+v", sample, want)
	}
}

func TestQuotaFromCodexHeaders(t *testing.T) {
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	// Keys as a JSON round trip may leave them: not canonical.
	record := pluginapi.UsageRecord{
		Provider:    "Codex",
		AuthIndex:   "c1",
		RequestedAt: at,
		ResponseHeaders: http.Header{
			"x-codex-primary-used-percent":        {"81"},
			"x-codex-primary-window-minutes":      {"300"},
			"x-codex-primary-reset-after-seconds": {"3600"},
			"X-Codex-Secondary-Used-Percent":      {"12.5"},
			"X-Codex-Secondary-Window-Minutes":    {"10080"},
			"X-Codex-Secondary-Reset-At":          {"1787695200"},
		},
	}
	sample, ok := quotaFromRecord(record)
	if !ok {
		t.Fatal("Codex headers must yield a sample")
	}
	want := []quotaWindow{
		{ID: "5h", Remaining: 19, ResetAt: at.Add(time.Hour).UnixMilli(), PeriodMinutes: 300},
		{ID: "7d", Remaining: 87.5, ResetAt: 1787695200000, PeriodMinutes: 10080},
	}
	if sample.Provider != "codex" || !reflect.DeepEqual(sample.Windows, want) {
		t.Fatalf("sample = %+v", sample)
	}

	// A plan with only a weekly window reports it as primary; it still pools
	// with the others' 7d.
	weekly, _ := quotaFromRecord(pluginapi.UsageRecord{
		Provider:  "codex",
		AuthIndex: "c2",
		ResponseHeaders: http.Header{
			"X-Codex-Primary-Used-Percent":   {"40"},
			"X-Codex-Primary-Window-Minutes": {"10080"},
		},
	})
	if len(weekly.Windows) != 1 || weekly.Windows[0].ID != "7d" || weekly.Windows[0].ResetAt != 0 {
		t.Fatalf("weekly-only = %+v", weekly.Windows)
	}
}

func TestQuotaFromRecordSkipsOthers(t *testing.T) {
	headers := http.Header{"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.5"}}
	for name, record := range map[string]pluginapi.UsageRecord{
		"no account":       {Provider: "claude", ResponseHeaders: headers},
		"other provider":   {Provider: "xai", AuthIndex: "x", ResponseHeaders: http.Header{"X-Ratelimit-Remaining-Tokens": {"9"}}},
		"no quota headers": {Provider: "claude", AuthIndex: "a", ResponseHeaders: http.Header{"Content-Type": {"application/json"}}},
		"unreadable":       {Provider: "claude", AuthIndex: "a", ResponseHeaders: http.Header{"Anthropic-Ratelimit-Unified-7d-Utilization": {"half"}}},
	} {
		if _, ok := quotaFromRecord(record); ok {
			t.Errorf("%s: must yield no sample", name)
		}
	}
}

func testSample(at time.Time, account string, fiveHour, weekly float64, weeklyReset time.Time) quotaSample {
	return quotaSample{At: at.UnixMilli(), Account: account, Provider: "claude", Windows: []quotaWindow{
		{ID: "5h", Remaining: fiveHour, ResetAt: at.Add(2 * time.Hour).Truncate(time.Hour).UnixMilli(), PeriodMinutes: 300},
		{ID: "7d", Remaining: weekly, ResetAt: weeklyReset.UnixMilli(), PeriodMinutes: 10080},
	}}
}

func TestQuotaSampleDue(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 10, 0, 0, time.UTC)
	reset := now.Add(72 * time.Hour)
	prev := testSample(now, "a", 80, 60, reset)
	for name, tc := range map[string]struct {
		next quotaSample
		want bool
	}{
		"older reading":            {testSample(now.Add(-time.Minute), "a", 70, 50, reset), false},
		"5-hour change only":       {testSample(now.Add(time.Minute), "a", 79, 60, reset), false},
		"weekly change":            {testSample(now.Add(time.Minute), "a", 79, 59, reset), true},
		"weekly reset moved":       {testSample(now.Add(time.Minute), "a", 80, 60, reset.Add(7*24*time.Hour)), true},
		"reset jitter":             {testSample(now.Add(time.Minute), "a", 80, 60, reset.Add(3*time.Second)), false},
		"unchanged after an hour":  {testSample(now.Add(time.Hour), "a", 80, 60, reset), true},
		"window missing this time": {quotaSample{At: now.Add(time.Minute).UnixMilli(), Account: "a", Windows: prev.Windows[1:]}, true},
	} {
		if got := quotaSampleDue(&prev, tc.next); got != tc.want {
			t.Errorf("%s: due = %v, want %v", name, got, tc.want)
		}
	}
	if !quotaSampleDue(nil, prev) {
		t.Error("an account's first sample must be kept")
	}
}

func TestStoreKeepsQuotaSamples(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 26, 0, 20, 0, 0, time.UTC)
	reset := now.Add(72 * time.Hour)
	first := newStore(dir, 30)
	first.now = fixedClock(now)
	first.tick()
	first.addQuota(testSample(now.Add(-time.Hour), "a", 90, 70, reset))
	first.addQuota(testSample(now.Add(-50*time.Minute), "a", 88, 70, reset))
	first.addQuota(testSample(now.Add(-40*time.Minute), "a", 85, 69, reset))
	first.addQuota(testSample(now, "b", 50, 20, reset))
	first.tick()
	first.close()

	raw, errRead := os.ReadFile(filepath.Join(dir, "usage-2026-10-25.json"))
	if errRead != nil || !strings.Contains(string(raw), `"quota":[`) {
		t.Fatalf("day file holds no quota: %v %s", errRead, raw)
	}

	second := newStore(dir, 30)
	second.now = fixedClock(now)
	second.tick()
	// The reloaded store knows a's last reading, so an unchanged one is not
	// kept again.
	unchanged := testSample(now.Add(-40*time.Minute), "a", 85, 69, reset)
	unchanged.At = now.Add(-39 * time.Minute).UnixMilli()
	second.addQuota(unchanged)
	samples, _ := second.quotaSnapshot(now.Add(-45 * time.Minute).UnixMilli())
	var got []string
	for _, sample := range samples {
		got = append(got, sample.Account+"@"+time.UnixMilli(sample.At).UTC().Format("15:04"))
	}
	// a's 23:20 reading still stands at 23:35, so it comes along.
	want := []string{"a@23:20", "a@23:40", "b@00:20"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %v, want %v", got, want)
	}
	second.close()
}

func TestStoreQuotaFromSpills(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	holder := newStore(dir, 30)
	holder.now = fixedClock(now)
	holder.tick()
	other := newStore(dir, 30)
	other.now = fixedClock(now)
	other.addQuota(testSample(now, "a", 90, 70, now.Add(time.Hour)))
	other.tick()
	holder.tick()
	samples, _ := holder.quotaSnapshot(0)
	if len(samples) != 1 || samples[0].Account != "a" {
		t.Fatalf("samples after merging the spill = %+v", samples)
	}
	holder.close()
	other.close()
}

func TestBuildQuotaReport(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	samples := []quotaSample{
		testSample(now, "claude-1", 90, 70, now),
		testSample(now, "removed", 90, 70, now),
		testSample(now, "codex-off", 90, 70, now),
	}
	entries := []pluginapi.HostAuthFileEntry{
		{AuthIndex: "claude-1", Provider: "claude", Email: "one@example.com"},
		{AuthIndex: "codex-new", Provider: "codex", Email: "new@example.com"},
		{AuthIndex: "codex-off", Provider: "codex", Email: "off@example.com", Disabled: true},
		{AuthIndex: "grok", Provider: "xai", Email: "grok@example.com"},
	}
	saved := map[string]accountInfo{"removed": {Label: "gone@example.com", Provider: "claude"}}

	view := buildQuotaReport(now, samples, saved, entries, true)
	var got []string
	for _, account := range view.Accounts {
		got = append(got, account.Provider+" "+account.Label+" "+strings.Repeat("*", len(account.Samples)))
	}
	want := []string{"claude one@example.com *", "codex new@example.com "}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listed accounts = %q, want %q", got, want)
	}

	unlisted := buildQuotaReport(now, samples, saved, nil, false)
	if len(unlisted.Accounts) != 3 || unlisted.Accounts[2].Label != "gone@example.com" {
		t.Fatalf("without the host's list = %+v", unlisted.Accounts)
	}
}

// The host sends the record with Go field names; ResponseHeaders must
// survive that.
func TestHandleUsageKeepsQuota(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	active := newStore(dir, 30)
	active.now = fixedClock(now)
	active.tick()
	previous := current.Load()
	current.Store(active)
	t.Cleanup(func() {
		current.Store(previous)
		active.close()
	})
	raw, _ := json.Marshal(pluginapi.UsageRecord{
		Provider:    "claude",
		AuthIndex:   "a1",
		RequestedAt: now,
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.25"},
		},
	})
	if _, errHandle := handleUsage(raw); errHandle != nil {
		t.Fatal(errHandle)
	}
	samples, _ := active.quotaSnapshot(0)
	if len(samples) != 1 || samples[0].Windows[0].Remaining != 75 {
		t.Fatalf("samples = %+v", samples)
	}
}
