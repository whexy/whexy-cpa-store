package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPricingEstimateUsesProviderMappingAndCacheRates(t *testing.T) {
	raw := []byte(`{
		"openai": {
			"models": {
				"gpt-test": {
					"id": "gpt-test",
					"name": "GPT Test",
					"last_updated": "2026-08-27",
					"cost": {"input": 2, "output": 10, "cache_read": 0.2, "cache_write": 2.5}
				}
			}
		}
	}`)
	catalog, errCatalog := decodePricingCatalog(raw, time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC))
	if errCatalog != nil {
		t.Fatal(errCatalog)
	}
	manager := &pricingManager{enabled: true, url: defaultPricingURL}
	manager.catalog.Store(catalog)
	record := pluginapi.UsageRecord{Provider: "codex", Model: "gpt-test"}
	usage := persistedRecord{UncachedInput: 1_000_000, CacheReadTokens: 500_000, CacheWriteTokens: 100_000, OutputTokens: 200_000}
	estimate := manager.estimate(record, usage)
	if estimate == nil {
		t.Fatal("expected pricing estimate")
	}
	if estimate.Provider != "openai" || estimate.Model != "gpt-test" {
		t.Fatalf("lookup = %s/%s", estimate.Provider, estimate.Model)
	}
	if estimate.InputCostUSD != 2 || estimate.CacheReadCostUSD != 0.1 || estimate.CacheWriteCostUSD != 0.25 || estimate.OutputCostUSD != 2 || estimate.TotalCostUSD != 4.35 {
		t.Fatalf("estimate = %+v", estimate)
	}
}

func TestPricingEstimateSeparatesReasoningOnlyWithDedicatedRate(t *testing.T) {
	raw := []byte(`{"openai":{"models":{"gpt-test":{"cost":{"input":1,"output":10,"reasoning":5}}}}}`)
	catalog, errCatalog := decodePricingCatalog(raw, time.Now())
	if errCatalog != nil {
		t.Fatal(errCatalog)
	}
	manager := &pricingManager{enabled: true}
	manager.catalog.Store(catalog)
	estimate := manager.estimate(
		pluginapi.UsageRecord{Provider: "openai", Model: "gpt-test"},
		persistedRecord{OutputTokens: 100_000, ReasoningTokens: 40_000},
	)
	if estimate == nil || estimate.OutputCostUSD != 0.6 || estimate.ReasoningCostUSD != 0.2 || estimate.TotalCostUSD != 0.8 {
		t.Fatalf("estimate = %+v", estimate)
	}
}

func TestPricingEstimateUsesContextTier(t *testing.T) {
	raw := []byte(`{"openai":{"models":{"gpt-test":{"cost":{"input":1,"output":2,"tiers":[{"input":3,"output":4,"tier":{"type":"context","size":200000}}]}}}}}`)
	catalog, errCatalog := decodePricingCatalog(raw, time.Now())
	if errCatalog != nil {
		t.Fatal(errCatalog)
	}
	manager := &pricingManager{enabled: true}
	manager.catalog.Store(catalog)
	estimate := manager.estimate(
		pluginapi.UsageRecord{Provider: "openai", Model: "gpt-test"},
		persistedRecord{UncachedInput: 250_000, OutputTokens: 10_000},
	)
	if estimate == nil || estimate.TierContextTokens != 200_000 || estimate.InputPerMillionUSD != 3 || estimate.OutputPerMillionUSD != 4 {
		t.Fatalf("estimate = %+v", estimate)
	}
}

func TestPricingModelOverrideSupportsNestedModelIDs(t *testing.T) {
	raw := []byte(`{"openrouter":{"models":{"openai/gpt-test":{"cost":{"input":1,"output":2}}}}}`)
	catalog, errCatalog := decodePricingCatalog(raw, time.Now())
	if errCatalog != nil {
		t.Fatal(errCatalog)
	}
	manager := &pricingManager{
		enabled:  true,
		modelMap: map[string]string{"custom/alias": "openrouter/openai/gpt-test"},
	}
	manager.catalog.Store(catalog)
	estimate := manager.estimate(
		pluginapi.UsageRecord{Provider: "custom", Model: "alias"},
		persistedRecord{UncachedInput: 1_000_000},
	)
	if estimate == nil || estimate.Provider != "openrouter" || estimate.Model != "openai/gpt-test" || estimate.TotalCostUSD != 1 {
		t.Fatalf("estimate = %+v", estimate)
	}
}

func TestPricingCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models-dev.json")
	raw := []byte(`{"openai":{"models":{"gpt-test":{"cost":{"input":1,"output":2}}}}}`)
	if errWrite := writePricingCache(path, raw); errWrite != nil {
		t.Fatal(errWrite)
	}
	catalog, errLoad := loadPricingCatalog(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if _, ok := catalog.Providers["openai"].Models["gpt-test"]; !ok {
		encoded, _ := json.Marshal(catalog.Providers)
		t.Fatalf("catalog = %s", encoded)
	}
}
