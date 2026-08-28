package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestNormalizeUsageRecordOpenAISubsetCache(t *testing.T) {
	record := normalizeUsageRecord(pluginapi.UsageRecord{
		Provider: "codex",
		Detail: pluginapi.UsageDetail{
			InputTokens:     100,
			OutputTokens:    25,
			ReasoningTokens: 5,
			CacheReadTokens: 40,
			TotalTokens:     125,
		},
	}, false)

	if record.UncachedInput != 60 || record.CacheReadTokens != 40 || record.TotalTokens != 125 {
		t.Fatalf("normalized record = %+v", record)
	}
	if record.CacheRatio != 0.4 {
		t.Fatalf("cache ratio = %v, want 0.4", record.CacheRatio)
	}
}

func TestNormalizeUsageRecordClaudeIndependentCache(t *testing.T) {
	record := normalizeUsageRecord(pluginapi.UsageRecord{
		Provider: "claude",
		Detail: pluginapi.UsageDetail{
			InputTokens:         100,
			OutputTokens:        25,
			CacheReadTokens:     50,
			CacheCreationTokens: 20,
			TotalTokens:         195,
		},
	}, false)

	if record.UncachedInput != 100 || record.CacheReadTokens != 50 || record.CacheWriteTokens != 20 || record.TotalTokens != 195 {
		t.Fatalf("normalized record = %+v", record)
	}
	if record.CacheRatio != float64(70)/170 {
		t.Fatalf("cache ratio = %v, want %v", record.CacheRatio, float64(70)/170)
	}
}

func TestUsageStorePersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	cfg := pluginConfig{Enabled: true, DataFile: path, RecentRecords: 10, PricingEnabled: false}
	store, errStore := newUsageStore(cfg)
	if errStore != nil {
		t.Fatal(errStore)
	}
	requestedAt := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	if errAppend := store.append(pluginapi.UsageRecord{
		Provider:    "codex",
		Model:       "gpt-test",
		AuthIndex:   "credential-1",
		RequestedAt: requestedAt,
		Latency:     1500 * time.Millisecond,
		TTFT:        250 * time.Millisecond,
		Generate:    true,
		Detail: pluginapi.UsageDetail{
			InputTokens:     100,
			OutputTokens:    20,
			CacheReadTokens: 25,
			TotalTokens:     120,
		},
	}); errAppend != nil {
		t.Fatal(errAppend)
	}
	if errClose := store.close(); errClose != nil {
		t.Fatal(errClose)
	}

	file, errOpen := os.Open(path)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		t.Fatal("missing persisted line")
	}
	var persisted persistedRecord
	if errDecode := json.Unmarshal(scanner.Bytes(), &persisted); errDecode != nil {
		t.Fatal(errDecode)
	}
	_ = file.Close()
	if persisted.APIKey != "" || persisted.UncachedInput != 75 {
		t.Fatalf("persisted = %+v", persisted)
	}

	reloaded, errReload := newUsageStore(cfg)
	if errReload != nil {
		t.Fatal(errReload)
	}
	defer func() { _ = reloaded.close() }()
	snapshot := reloaded.snapshot()
	if snapshot.Overall.Requests != 1 || snapshot.Overall.UnpricedRequests != 1 || snapshot.Overall.TotalTokens != 120 || snapshot.Overall.CacheReadTokens != 25 {
		t.Fatalf("snapshot = %+v", snapshot.Overall)
	}
	if len(snapshot.ByModel) != 1 || snapshot.ByModel[0].Key != "gpt-test" {
		t.Fatalf("by model = %+v", snapshot.ByModel)
	}
	if len(snapshot.Recent) != 1 || !snapshot.Recent[0].RequestedAt.Equal(requestedAt) {
		t.Fatalf("recent = %+v", snapshot.Recent)
	}
}

func TestNormalizeUsageRecordUnknownCachedTokensRemainUnclassified(t *testing.T) {
	record := normalizeUsageRecord(pluginapi.UsageRecord{
		Provider: "custom-provider",
		Detail:   pluginapi.UsageDetail{CachedTokens: 20, TotalTokens: 20},
	}, false)
	if record.CacheReadTokens != 0 || record.UncachedInput != 0 || record.TotalTokens != 20 {
		t.Fatalf("normalized record = %+v", record)
	}
}

func TestNormalizeUsageRecordAPIKeyOptIn(t *testing.T) {
	raw := pluginapi.UsageRecord{APIKey: "frontend-key"}
	if got := normalizeUsageRecord(raw, false).APIKey; got != "" {
		t.Fatalf("api key persisted without opt-in: %q", got)
	}
	if got := normalizeUsageRecord(raw, true).APIKey; got != "frontend-key" {
		t.Fatalf("api key = %q", got)
	}
}
