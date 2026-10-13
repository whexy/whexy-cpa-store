package main

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func rate(v float64) *float64 { return &v }

func TestValidateCustomPrices(t *testing.T) {
	got, errValidate := validateCustomPrices(map[string]customPriceInput{
		" opencode-go/kimi-k3 ": {Input: rate(0.6), Output: rate(2.5), CacheRead: rate(0.15)},
		"free-model":            {Input: rate(0), Output: rate(0)},
	})
	if errValidate != nil {
		t.Fatal(errValidate)
	}
	kimi, ok := got["opencode-go/kimi-k3"]
	if !ok || kimi.Input != 0.6 || kimi.Output != 2.5 || kimi.cacheRead() != 0.15 || kimi.cacheWrite() != 0.6 {
		t.Fatalf("kimi = %+v (ok %v)", kimi, ok)
	}
	if free := got["free-model"]; free.Input != 0 || free.Output != 0 {
		t.Fatalf("free = %+v", free)
	}

	for name, in := range map[string]map[string]customPriceInput{
		"empty ID":       {" ": {Input: rate(1), Output: rate(1)}},
		"no input":       {"m": {Output: rate(1)}},
		"no output":      {"m": {Input: rate(1)}},
		"negative":       {"m": {Input: rate(1), Output: rate(1), CacheWrite: rate(-1)}},
		"too large":      {"m": {Input: rate(math.MaxFloat64), Output: rate(1)}},
		"duplicate trim": {"m": {Input: rate(1), Output: rate(1)}, " m": {Input: rate(2), Output: rate(2)}},
		"long ID":        {strings.Repeat("x", maxCustomModelLength+1): {Input: rate(1), Output: rate(1)}},
	} {
		if _, errValidate := validateCustomPrices(in); errValidate == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCustomPricesPersist(t *testing.T) {
	dir := t.TempDir()
	first := &customPricing{now: time.Now}
	first.configure(dir)
	if len(first.current()) != 0 {
		t.Fatalf("fresh dir has prices %v", first.current())
	}
	models, _ := validateCustomPrices(map[string]customPriceInput{"kimi-k3": {Input: rate(0.6), Output: rate(2.5), CacheWrite: rate(0)}})
	if errSave := first.save(models); errSave != nil {
		t.Fatal(errSave)
	}
	info, errStat := os.Stat(filepath.Join(dir, customPricesFileName))
	if errStat != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", info, errStat)
	}

	second := &customPricing{now: time.Now}
	second.configure(dir)
	got := second.current()["kimi-k3"]
	if got.Input != 0.6 || got.Output != 2.5 || got.CacheRead != nil || got.CacheWrite == nil || *got.CacheWrite != 0 {
		t.Fatalf("reloaded = %+v", got)
	}
}

func TestCustomPricesMovesUnreadableFileAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, customPricesFileName)
	if errWrite := os.WriteFile(path, []byte(`{"version":1,"models":{"m":{"output":1}}}`), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	c := &customPricing{now: func() time.Time { return time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC) }}
	c.configure(dir)
	if len(c.current()) != 0 {
		t.Fatalf("loaded %v from an invalid file", c.current())
	}
	if _, errStat := os.Stat(path + ".corrupt-20261013T090000"); errStat != nil {
		t.Fatalf("unreadable file not kept: %v", errStat)
	}
}

func TestCustomPriceOverridesCatalog(t *testing.T) {
	now := time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC)
	rows := []storedRow{
		reportRow(now, "a", "claude", "claude-opus-5-5", counters{Requests: 1, Input: 1_000_000}),
		reportRow(now, "a", "opencode", "opencode-go/kimi-k3", counters{Requests: 1, Input: 1_000_000, Output: 1_000_000}),
		reportRow(now, "a", "opencode", "kimi-k3", counters{Requests: 1, Input: 1_000_000}),
	}
	custom := map[string]price{
		"claude-opus-5-5":     {Input: 1, Output: 1},
		"opencode-go/kimi-k3": {Input: 0.5, Output: 2},
	}
	report := buildReport(ranges["7d"], groupByFamily, time.UTC, reportInputs{
		Now: now, Rows: rows, History: rows, Catalog: testCatalog(), Custom: custom, Retention: 30,
	})
	byModel := make(map[string]modelView)
	for _, m := range report.Models {
		byModel[m.Model] = m
	}
	if m := byModel["claude-opus-5-5"]; !m.Priced || !m.CustomPriced || math.Abs(m.Cost-1) > 1e-9 {
		t.Fatalf("claude = %+v, want the custom $1 instead of the catalog $5", m)
	}
	if m := byModel["opencode-go/kimi-k3"]; !m.Priced || !m.CustomPriced || math.Abs(m.Cost-2.5) > 1e-9 {
		t.Fatalf("prefixed kimi = %+v", m)
	}
	// Matching is exact: the bare ID is a different model.
	if m := byModel["kimi-k3"]; m.Priced || m.CustomPriced {
		t.Fatalf("bare kimi = %+v", m)
	}
	if report.Totals.UnpricedTokens != 1_000_000 || math.Abs(report.Totals.Cost-3.5) > 1e-9 {
		t.Fatalf("totals = %+v", report.Totals)
	}
}

func TestBuildPriceList(t *testing.T) {
	now := time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC)
	history := []storedRow{
		reportRow(now, "a", "claude", "claude-opus-5-5", counters{Input: 300}),
		reportRow(now.AddDate(0, 0, -20), "a", "opencode", "kimi-k3", counters{Input: 100}),
	}
	list := buildPriceList(history, testCatalog(), map[string]price{
		"kimi-k3":   {Input: 0.6, Output: 2.5},
		"retired-x": {Input: 1, Output: 1},
	}, nil)
	if len(list.Models) != 3 {
		t.Fatalf("models = %+v", list.Models)
	}
	claude, kimi, retired := list.Models[0], list.Models[1], list.Models[2]
	if claude.Model != "claude-opus-5-5" || claude.Processed != 300 || claude.Custom != nil || claude.Catalog == nil || claude.Catalog.Input != 5 {
		t.Fatalf("claude = %+v", claude)
	}
	if kimi.Model != "kimi-k3" || kimi.Custom == nil || kimi.Custom.Input != 0.6 || kimi.Catalog != nil || kimi.Family != "Kimi" {
		t.Fatalf("kimi = %+v", kimi)
	}
	if retired.Model != "retired-x" || retired.Processed != 0 || retired.Custom == nil {
		t.Fatalf("retired = %+v", retired)
	}
}

func TestPricesRoutes(t *testing.T) {
	dir := t.TempDir()
	previousStore, previousCustom := current.Load(), customPrices
	t.Cleanup(func() {
		current.Store(previousStore)
		customPrices = previousCustom
	})
	s := newStore(dir, 30)
	s.add(usageEvent{At: time.Now().UTC(), Provider: "opencode", Model: "kimi-k3", Counters: counters{Requests: 1, Input: 1_000_000}})
	current.Store(s)
	customPrices = &customPricing{now: time.Now}
	customPrices.configure(dir)

	put := func(body string) pluginapi.ManagementResponse {
		return routeManagement(pluginapi.ManagementRequest{Method: http.MethodPut, Path: pricesPath, Body: []byte(body)})
	}
	for _, body := range []string{`not json`, `{}`, `{"models":{"kimi-k3":{"output":1}}}`} {
		if resp := put(body); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("PUT %s = %d %s", body, resp.StatusCode, resp.Body)
		}
	}
	if resp := put(`{"models":{"kimi-k3":{"input":0.6,"output":2.5}}}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d %s", resp.StatusCode, resp.Body)
	}

	resp := routeManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: pricesPath})
	var list priceListView
	if errUnmarshal := json.Unmarshal(resp.Body, &list); errUnmarshal != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d %s: %v", resp.StatusCode, resp.Body, errUnmarshal)
	}
	if len(list.Models) != 1 || list.Models[0].Model != "kimi-k3" || list.Models[0].Custom == nil || list.Models[0].Custom.Output != 2.5 {
		t.Fatalf("list = %+v", list.Models)
	}

	report, errSummary := summary(reportQuery{Range: "24h"})
	if errSummary != nil {
		t.Fatal(errSummary)
	}
	if report.Pricing.Custom != 1 || math.Abs(report.Totals.Cost-0.6) > 1e-9 || report.Totals.UnpricedTokens != 0 {
		t.Fatalf("summary pricing=%+v totals=%+v", report.Pricing, report.Totals)
	}

	if resp := put(`{"models":{}}`); resp.StatusCode != http.StatusOK || len(customPrices.current()) != 0 {
		t.Fatalf("clearing = %d %s", resp.StatusCode, resp.Body)
	}
}
