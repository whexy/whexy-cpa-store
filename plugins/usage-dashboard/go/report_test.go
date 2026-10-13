package main

import (
	"fmt"
	"math"
	"testing"
	"time"
)

func reportRow(at time.Time, account, provider, model string, c counters, sessions ...string) storedRow {
	return storedRow{At: at.UTC().Truncate(time.Hour), Key: rowKey{Hour: at.UTC().Hour(), Account: account, Provider: provider, Model: model}, Counters: c, Sessions: sessions}
}

func TestWindowUsesLocalDays(t *testing.T) {
	chicago, errZone := time.LoadLocation("America/Chicago")
	if errZone != nil {
		t.Fatal(errZone)
	}
	now := time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC) // Oct 11, 22:00 in Chicago
	from, to, points, step := window(ranges["30d"], now, chicago)
	if step != "day" || len(points) != 30 || points[0] != "2026-09-12" || points[29] != "2026-10-11" {
		t.Fatalf("step=%s points=%d first=%s last=%s", step, len(points), points[0], points[len(points)-1])
	}
	if want := time.Date(2026, 9, 12, 5, 0, 0, 0, time.UTC); !from.Equal(want) {
		t.Fatalf("from = %v, want %v", from, want)
	}
	if want := time.Date(2026, 10, 12, 5, 0, 0, 0, time.UTC); !to.Equal(want) {
		t.Fatalf("to = %v, want %v", to, want)
	}

	from, to, points, step = window(ranges["24h"], time.Date(2026, 10, 12, 3, 20, 0, 0, time.UTC), chicago)
	if step != "hour" || len(points) != 24 || !to.Equal(time.Date(2026, 10, 12, 4, 0, 0, 0, time.UTC)) || to.Sub(from) != 24*time.Hour {
		t.Fatalf("24h: step=%s points=%d from=%v to=%v", step, len(points), from, to)
	}
}

func testCatalog() *priceCatalog {
	cacheRead := 0.5
	catalog := &priceCatalog{Models: map[string]map[string]price{
		"anthropic": {"claude-opus-5-5": {Input: 5, Output: 25, CacheRead: &cacheRead}},
	}}
	catalog.index()
	return catalog
}

func TestBuildReportByFamily(t *testing.T) {
	now := time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC)
	yesterday := now.Add(-24 * time.Hour)
	rows := []storedRow{
		reportRow(now, "a", "claude", "claude-opus-5-5", counters{Requests: 2, Input: 1_000_000, CacheRead: 2_000_000, Output: 1_000_000}, "s1", "s2"),
		reportRow(yesterday, "b", "claude", "claude-opus-5-5", counters{Requests: 1, Input: 1_000_000}, "s2"),
		reportRow(now, "a", "codex", "gpt-6.1-sol", counters{Requests: 1, Failed: 1, Input: 500}, "s3"),
		reportRow(now.AddDate(0, 0, -40), "a", "codex", "gpt-6.1-sol", counters{Requests: 1, Input: 999}),
	}
	report := buildReport(ranges["7d"], groupByFamily, time.UTC, reportInputs{
		Now: now, Rows: rows, History: rows, Catalog: testCatalog(), Retention: 30,
	})
	if report.Totals.Requests != 4 || report.Totals.Failed != 1 || report.Totals.Sessions != 3 {
		t.Fatalf("totals = %+v", report.Totals)
	}
	if report.Totals.Processed != 5_000_500 {
		t.Fatalf("processed = %d", report.Totals.Processed)
	}
	// 2M input at $5 + 2M cache reads at $0.5 + 1M output at $25.
	if math.Abs(report.Totals.Cost-36) > 1e-9 || math.Abs(report.Totals.CacheSavings-9) > 1e-9 {
		t.Fatalf("cost=%v savings=%v", report.Totals.Cost, report.Totals.CacheSavings)
	}
	if report.Totals.UnpricedTokens != 500 {
		t.Fatalf("unpriced = %d", report.Totals.UnpricedTokens)
	}
	if len(report.Groups) != 2 || report.Groups[0].Label != "Claude" || report.Groups[1].Label != "GPT" {
		t.Fatalf("groups = %+v", report.Groups)
	}
	claude := report.Groups[0]
	if claude.Color != familySlots["Claude"] || claude.Detail != "1 model" || claude.Sessions != 2 {
		t.Fatalf("claude = %+v", claude)
	}
	if len(claude.SeriesTokens) != 7 || claude.SeriesTokens[6] != 4_000_000 || claude.SeriesTokens[5] != 1_000_000 {
		t.Fatalf("claude tokens = %v", claude.SeriesTokens)
	}
	if report.Points[6].Requests != 3 || report.Points[5].Requests != 1 {
		t.Fatalf("points = %+v", report.Points)
	}
	if len(report.Models) != 2 || report.Models[0].Model != "claude-opus-5-5" || !report.Models[0].Priced || report.Models[1].Priced {
		t.Fatalf("models = %+v", report.Models)
	}
	if fmt.Sprint(report.Range.Ranges) != "[24h 7d 30d]" {
		t.Fatalf("ranges = %v", report.Range.Ranges)
	}
}

func TestBuildReportByAccountNames(t *testing.T) {
	now := time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC)
	rows := []storedRow{
		reportRow(now, "live", "claude", "claude-opus-5-5", counters{Requests: 1, Input: 30}),
		reportRow(now, "gone", "codex", "gpt-6.1-sol", counters{Requests: 1, Input: 20}),
		reportRow(now, "0123456789abcdef", "codex", "gpt-6.1-sol", counters{Requests: 1, Input: 10}),
		reportRow(now, "", "gemini", "gemini-3-pro", counters{Requests: 1, Input: 5}),
	}
	report := buildReport(ranges["24h"], groupByAccount, time.UTC, reportInputs{
		Now:      now,
		Rows:     rows,
		History:  rows,
		Live:     map[string]accountInfo{"live": {Label: "me@example.com", Provider: "claude"}},
		Accounts: map[string]accountInfo{"live": {Label: "old name"}, "gone": {Label: "team@example.com", Provider: "codex"}},
	})
	want := []string{"me@example.com", "team@example.com", "Account 01234567", "Unattributed"}
	for i, label := range want {
		if report.Groups[i].Label != label {
			t.Fatalf("group %d = %q, want %q (all: %+v)", i, report.Groups[i].Label, label, report.Groups)
		}
	}
	if report.Groups[0].Detail != "claude" || report.Groups[0].Color != 1 || report.Groups[1].Color != 2 {
		t.Fatalf("groups = %+v", report.Groups)
	}
	if report.Models[0].Color != 0 {
		t.Fatal("model rows stay neutral when grouping by account")
	}
}

func TestColorsFoldPastThePalette(t *testing.T) {
	now := time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC)
	var rows []storedRow
	for i := range 10 {
		rows = append(rows, reportRow(now, fmt.Sprintf("acct-%02d", i), "claude", "claude-opus-5-5", counters{Requests: 1, Input: int64(100 - i)}))
	}
	report := buildReport(ranges["24h"], groupByAccount, time.UTC, reportInputs{Now: now, Rows: rows, History: rows})
	if len(report.Groups) != 9 {
		t.Fatalf("groups = %d, want 8 coloured and Other", len(report.Groups))
	}
	other := report.Groups[8]
	if other.Key != otherGroupKey || other.Color != 0 || other.Processed != 91+92 {
		t.Fatalf("other = %+v", other)
	}
}

func TestColorsFollowHistoryNotRange(t *testing.T) {
	now := time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC)
	old := reportRow(now.AddDate(0, 0, -20), "", "x", "phi-4", counters{Requests: 1, Input: 1000})
	recent := reportRow(now, "", "x", "mistral-large", counters{Requests: 1, Input: 1})
	recentOther := reportRow(now, "", "x", "phi-4", counters{Requests: 1, Input: 1})
	history := []storedRow{old, recent, recentOther}
	weekly := buildReport(ranges["7d"], groupByFamily, time.UTC, reportInputs{Now: now, Rows: history, History: history})
	colors := map[string]int{}
	for _, g := range weekly.Groups {
		colors[g.Label] = g.Color
	}
	// Phi leads the history, so it takes the first free slot even though
	// both families used one token this week; Mistral keeps no pinned slot.
	if colors["Phi"] != 1 || colors["Mistral"] != 2 {
		t.Fatalf("colors = %v", colors)
	}
}

func TestParseQuery(t *testing.T) {
	if _, _, _, errQuery := parseQuery(reportQuery{Range: "1y"}); errQuery == nil {
		t.Fatal("unknown range must fail")
	}
	if _, _, _, errQuery := parseQuery(reportQuery{GroupBy: "model"}); errQuery == nil {
		t.Fatal("unknown grouping must fail")
	}
	if _, _, _, errQuery := parseQuery(reportQuery{TimeZone: "Mars/Olympus"}); errQuery == nil {
		t.Fatal("unknown zone must fail")
	}
	spec, groupBy, loc, errQuery := parseQuery(reportQuery{TimeZone: "Asia/Shanghai"})
	if errQuery != nil || spec.Key != "30d" || groupBy != groupByFamily || loc.String() != "Asia/Shanghai" {
		t.Fatalf("defaults: %v %v %v %v", spec, groupBy, loc, errQuery)
	}
}
