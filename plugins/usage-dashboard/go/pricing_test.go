package main

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const sampleModelsDev = `{
  "anthropic": {"models": {
    "claude-opus-5-5": {"id": "claude-opus-5-5", "cost": {"input": 4, "output": 20, "cache_read": 0.2, "cache_write": 5}},
    "claude-haiku-5": {"id": "claude-haiku-5", "cost": {"input": 1, "output": 5}}
  }},
  "openai": {"models": {
    "gpt-6.1-sol": {"id": "gpt-6.1-sol", "cost": {"input": 2, "output": 10, "cache_read": 0.1}},
    "text-embedding-3-small": {"id": "text-embedding-3-small"}
  }},
  "deepseek": {"models": {"deepseek-v4-pro": {"id": "deepseek-v4-pro", "cost": {"input": 0.5, "output": 2}}}},
  "openrouter": {"models": {
    "deepseek-v4-pro": {"id": "deepseek-v4-pro", "cost": {"input": 9, "output": 9}},
    "mystery-model": {"id": "mystery-model", "cost": {"input": 3, "output": 3}}
  }}
}`

func TestPriceLookup(t *testing.T) {
	catalog, errParse := parseModelsDev([]byte(sampleModelsDev), "test", time.Now())
	if errParse != nil {
		t.Fatal(errParse)
	}
	cases := []struct {
		provider, model, family string
		input                   float64
		ok                      bool
	}{
		{"claude", "claude-opus-5-5", "Claude", 4, true},
		{"claude", "claude-opus-5-5-20261001", "Claude", 4, true},
		{"codex", "gpt-6.1-sol", "GPT", 2, true},
		// A third-party route of a DeepSeek model is priced at the vendor's list price.
		{"openai-compatibility", "ai-proxy/deepseek-v4-pro", "DeepSeek", 0.5, true},
		{"opencode-go", "mystery-model", "Mystery", 3, true},
		{"codex", "text-embedding-3-small", "Text", 0, false},
		{"claude", "claude-unknown", "Claude", 0, false},
	}
	for _, tc := range cases {
		p, ok := catalog.lookup(tc.provider, tc.model, tc.family)
		if ok != tc.ok || p.Input != tc.input {
			t.Errorf("lookup(%s, %s) = %+v, %v; want input %v, %v", tc.provider, tc.model, p, ok, tc.input, tc.ok)
		}
	}
	var none *priceCatalog
	if _, ok := none.lookup("claude", "claude-opus-5-5", "Claude"); ok {
		t.Fatal("a nil catalog prices nothing")
	}
}

func TestPriceCostAndSavings(t *testing.T) {
	cacheRead, cacheWrite := 0.2, 5.0
	p := price{Input: 4, Output: 20, CacheRead: &cacheRead, CacheWrite: &cacheWrite}
	c := counters{Input: 1_000_000, CacheRead: 10_000_000, CacheWrite: 1_000_000, Output: 500_000}
	if got := p.cost(c); math.Abs(got-21) > 1e-9 {
		t.Fatalf("cost = %v", got)
	}
	if got := p.savings(c); math.Abs(got-38) > 1e-9 {
		t.Fatalf("savings = %v", got)
	}
	// Without cache rates, cache tokens cost the input rate and save nothing.
	plain := price{Input: 1, Output: 5}
	if plain.cost(counters{CacheRead: 1_000_000, CacheWrite: 1_000_000}) != 2 || plain.savings(counters{CacheRead: 1_000_000}) != 0 {
		t.Fatal("missing cache rates must fall back to the input rate")
	}
}

func TestPricingRefreshAndCache(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	fetches := 0
	p := &pricing{now: func() time.Time { return now }, fetch: func(string) ([]byte, error) {
		fetches++
		return []byte(sampleModelsDev), nil
	}}
	p.configure("https://example.test/api.json", dir)
	p.refreshIfDue()
	p.refreshIfDue()
	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1 within a day", fetches)
	}
	catalog, status := p.current()
	if catalog == nil || status.FetchedAt == nil || !status.Enabled {
		t.Fatalf("status = %+v", status)
	}
	if _, errStat := os.Stat(filepath.Join(dir, pricesFileName)); errStat != nil {
		t.Fatal(errStat)
	}

	// A restart reads the cached catalog and waits for it to age.
	restarted := &pricing{now: func() time.Time { return now.Add(time.Hour) }, fetch: func(string) ([]byte, error) {
		return nil, errors.New("offline")
	}}
	restarted.configure("https://example.test/api.json", dir)
	restarted.refreshIfDue()
	if catalog, status := restarted.current(); catalog == nil || status.Error != "" {
		t.Fatalf("restarted: catalog=%v status=%+v", catalog != nil, status)
	}

	// A different source ignores the cache; a failure is reported.
	other := &pricing{now: func() time.Time { return now }, fetch: func(string) ([]byte, error) {
		return nil, errors.New("offline")
	}}
	other.configure("https://elsewhere.test/api.json", dir)
	other.refreshIfDue()
	if catalog, status := other.current(); catalog != nil || status.Error == "" {
		t.Fatalf("other: catalog=%v status=%+v", catalog != nil, status)
	}

	other.configure("", dir)
	if _, status := other.current(); status.Enabled {
		t.Fatal("an empty URL disables pricing")
	}
}
