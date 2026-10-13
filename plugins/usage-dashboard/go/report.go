package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // the page's IANA zone must load even in images without tzdata
)

const (
	groupByFamily  = "family"
	groupByAccount = "account"
	otherGroupKey  = "__other__"
	paletteSlots   = 8
)

// rangeSpec is one of the page's time ranges.
type rangeSpec struct {
	Key  string
	Days int // zero for the hourly 24-hour range
}

var ranges = map[string]rangeSpec{
	"24h": {Key: "24h"},
	"7d":  {Key: "7d", Days: 7},
	"30d": {Key: "30d", Days: 30},
	"90d": {Key: "90d", Days: 90},
}

// familySlots pins well-known families to a palette slot (1-based, see the
// page's --series-N colours) so a family keeps its colour across ranges and
// installs. Other families take the free slots in order of total use.
var familySlots = map[string]int{
	"Gemini":   1,
	"Claude":   2,
	"GPT":      3,
	"Kimi":     4,
	"Grok":     5,
	"GLM":      6,
	"DeepSeek": 7,
	"Qwen":     8,
}

type reportQuery struct {
	Range    string
	GroupBy  string
	TimeZone string
}

type aggregate struct {
	counters
	sessions       map[string]struct{}
	cost           float64
	savings        float64
	unpricedTokens int64
}

func newAggregate() *aggregate {
	return &aggregate{sessions: make(map[string]struct{})}
}

func (a *aggregate) add(r storedRow, p price, priced bool) {
	a.counters.add(r.Counters)
	for _, session := range r.Sessions {
		a.sessions[session] = struct{}{}
	}
	if priced {
		a.cost += p.cost(r.Counters)
		a.savings += p.savings(r.Counters)
	} else {
		a.unpricedTokens += r.Counters.processed()
	}
}

func (a *aggregate) merge(other *aggregate) {
	a.counters.add(other.counters)
	for session := range other.sessions {
		a.sessions[session] = struct{}{}
	}
	a.cost += other.cost
	a.savings += other.savings
	a.unpricedTokens += other.unpricedTokens
}

// totalsView is an aggregate as the page reads it.
type totalsView struct {
	Requests       int64   `json:"requests"`
	Failed         int64   `json:"failed"`
	Sessions       int     `json:"sessions"`
	Input          int64   `json:"input"`
	CacheRead      int64   `json:"cache_read"`
	CacheWrite     int64   `json:"cache_write"`
	Output         int64   `json:"output"`
	Reasoning      int64   `json:"reasoning"`
	Processed      int64   `json:"processed"`
	Cost           float64 `json:"cost"`
	CacheSavings   float64 `json:"cache_savings"`
	UnpricedTokens int64   `json:"unpriced_tokens"`
}

func (a *aggregate) view() totalsView {
	return totalsView{
		Requests:       a.Requests,
		Failed:         a.Failed,
		Sessions:       len(a.sessions),
		Input:          a.Input,
		CacheRead:      a.CacheRead,
		CacheWrite:     a.CacheWrite,
		Output:         a.Output,
		Reasoning:      a.Reasoning,
		Processed:      a.processed(),
		Cost:           a.cost,
		CacheSavings:   a.savings,
		UnpricedTokens: a.unpricedTokens,
	}
}

type groupView struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
	// Color is a palette slot, 1 to 8; 0 is the neutral "Other" colour.
	Color int `json:"color"`
	totalsView
	// SeriesTokens and SeriesCost hold one value per point of the range.
	SeriesTokens []int64   `json:"series_tokens"`
	SeriesCost   []float64 `json:"series_cost"`
}

type modelView struct {
	Model  string `json:"model"`
	Family string `json:"family"`
	Color  int    `json:"color"`
	Priced bool   `json:"priced"`
	totalsView
}

type pointView struct {
	Point string `json:"point"`
	totalsView
}

type rangeView struct {
	Key    string    `json:"key"`
	Step   string    `json:"step"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Zone   string    `json:"tz"`
	Ranges []string  `json:"ranges"`
}

type reportView struct {
	GeneratedAt time.Time     `json:"generated_at"`
	Range       rangeView     `json:"range"`
	GroupBy     string        `json:"group_by"`
	Totals      totalsView    `json:"totals"`
	Groups      []groupView   `json:"groups"`
	Models      []modelView   `json:"models"`
	Points      []pointView   `json:"points"`
	Pricing     pricingStatus `json:"pricing"`
	Store       storeStatus   `json:"store"`
}

// reportInputs is everything buildReport reads, so tests can supply it.
type reportInputs struct {
	Now       time.Time
	Rows      []storedRow
	History   []storedRow
	Accounts  map[string]accountInfo
	Live      map[string]accountInfo
	Catalog   *priceCatalog
	Rules     []familyRule
	Retention int
}

// window resolves a range to its half-open interval and point labels. Day
// ranges end with today in loc; the 24-hour range ends with the current hour.
func window(spec rangeSpec, now time.Time, loc *time.Location) (from, to time.Time, points []string, step string) {
	if spec.Days == 0 {
		to = now.UTC().Truncate(time.Hour).Add(time.Hour)
		from = to.Add(-24 * time.Hour)
		for at := from; at.Before(to); at = at.Add(time.Hour) {
			points = append(points, at.Format(time.RFC3339))
		}
		return from, to, points, "hour"
	}
	local := now.In(loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	from = today.AddDate(0, 0, -(spec.Days - 1))
	to = today.AddDate(0, 0, 1)
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		points = append(points, d.Format(dayLayout))
	}
	return from.UTC(), to.UTC(), points, "day"
}

func parseQuery(query reportQuery) (rangeSpec, string, *time.Location, error) {
	spec, ok := ranges[strings.TrimSpace(query.Range)]
	if !ok {
		if strings.TrimSpace(query.Range) != "" {
			return rangeSpec{}, "", nil, fmt.Errorf("range must be one of 24h, 7d, 30d, 90d")
		}
		spec = ranges["30d"]
	}
	groupBy := strings.TrimSpace(query.GroupBy)
	switch groupBy {
	case "":
		groupBy = groupByFamily
	case groupByFamily, groupByAccount:
	default:
		return rangeSpec{}, "", nil, fmt.Errorf("group must be family or account")
	}
	loc := time.UTC
	if zone := strings.TrimSpace(query.TimeZone); zone != "" {
		loaded, errZone := time.LoadLocation(zone)
		if errZone != nil {
			return rangeSpec{}, "", nil, fmt.Errorf("unknown time zone %q", zone)
		}
		loc = loaded
	}
	return spec, groupBy, loc, nil
}

// availableRanges lists the ranges retention can fill.
func availableRanges(retention int) []string {
	out := []string{"24h"}
	for _, key := range []string{"7d", "30d", "90d"} {
		if ranges[key].Days <= retention {
			out = append(out, key)
		}
	}
	return out
}

func buildReport(spec rangeSpec, groupBy string, loc *time.Location, in reportInputs) reportView {
	from, to, points, step := window(spec, in.Now, loc)
	pointIndex := make(map[string]int, len(points))
	for i, point := range points {
		pointIndex[point] = i
	}
	indexOf := func(at time.Time) int {
		if step == "hour" {
			return int(at.Sub(from) / time.Hour)
		}
		if i, ok := pointIndex[at.In(loc).Format(dayLayout)]; ok {
			return i
		}
		return -1
	}

	type priceKey struct{ provider, model string }
	priceCache := make(map[priceKey]struct {
		p  price
		ok bool
	})
	priceOf := func(r storedRow, family string) (price, bool) {
		key := priceKey{r.Key.Provider, r.Key.Model}
		if cached, hit := priceCache[key]; hit {
			return cached.p, cached.ok
		}
		p, ok := in.Catalog.lookup(r.Key.Provider, r.Key.Model, family)
		priceCache[key] = struct {
			p  price
			ok bool
		}{p, ok}
		return p, ok
	}
	familyCache := make(map[string]string)
	familyOf := func(model string) string {
		if family, hit := familyCache[model]; hit {
			return family
		}
		family := modelFamily(model, in.Rules)
		familyCache[model] = family
		return family
	}
	groupOf := func(r storedRow) string {
		if groupBy == groupByAccount {
			return r.Key.Account
		}
		return familyOf(r.Key.Model)
	}

	colors := assignColors(groupBy, in.History, groupOf)

	totals := newAggregate()
	groups := make(map[string]*aggregate)
	groupTokens := make(map[string][]int64)
	groupCost := make(map[string][]float64)
	models := make(map[string]*aggregate)
	modelPriced := make(map[string]bool)
	pointAggs := make([]*aggregate, len(points))
	for i := range pointAggs {
		pointAggs[i] = newAggregate()
	}

	for _, r := range in.Rows {
		if r.At.Before(from) || !r.At.Before(to) {
			continue
		}
		i := indexOf(r.At)
		if i < 0 || i >= len(points) {
			continue
		}
		family := familyOf(r.Key.Model)
		p, priced := priceOf(r, family)
		key := groupOf(r)
		if _, colored := colors[key]; !colored {
			key = otherGroupKey
		}
		if groups[key] == nil {
			groups[key] = newAggregate()
			groupTokens[key] = make([]int64, len(points))
			groupCost[key] = make([]float64, len(points))
		}
		groups[key].add(r, p, priced)
		groupTokens[key][i] += r.Counters.processed()
		if priced {
			groupCost[key][i] += p.cost(r.Counters)
		}
		model := r.Key.Model
		if models[model] == nil {
			models[model] = newAggregate()
			modelPriced[model] = true
		}
		models[model].add(r, p, priced)
		modelPriced[model] = modelPriced[model] && priced
		pointAggs[i].add(r, p, priced)
		totals.add(r, p, priced)
	}

	report := reportView{
		GeneratedAt: in.Now.UTC(),
		Range:       rangeView{Key: spec.Key, Step: step, From: from, To: to, Zone: loc.String(), Ranges: availableRanges(in.Retention)},
		GroupBy:     groupBy,
		Totals:      totals.view(),
		Groups:      make([]groupView, 0, len(groups)),
		Models:      make([]modelView, 0, len(models)),
		Points:      make([]pointView, 0, len(points)),
	}
	familyModels := make(map[string]int)
	for model := range models {
		familyModels[familyOf(model)]++
	}
	for key, agg := range groups {
		view := groupView{Key: key, Color: colors[key], totalsView: agg.view(), SeriesTokens: groupTokens[key], SeriesCost: groupCost[key]}
		switch {
		case key == otherGroupKey:
			view.Label = "Other"
		case groupBy == groupByFamily:
			view.Label = key
			view.Detail = plural(familyModels[key], "model")
		default:
			info := accountName(key, in.Live, in.Accounts)
			view.Label, view.Detail = info.Label, info.Provider
		}
		report.Groups = append(report.Groups, view)
	}
	sort.Slice(report.Groups, func(i, j int) bool {
		a, b := report.Groups[i], report.Groups[j]
		if (a.Key == otherGroupKey) != (b.Key == otherGroupKey) {
			return b.Key == otherGroupKey
		}
		if a.Processed != b.Processed {
			return a.Processed > b.Processed
		}
		return a.Label < b.Label
	})
	for model, agg := range models {
		family := familyOf(model)
		report.Models = append(report.Models, modelView{Model: model, Family: family, Color: familySlotFor(family, groupBy, colors), Priced: modelPriced[model], totalsView: agg.view()})
	}
	sort.Slice(report.Models, func(i, j int) bool {
		if report.Models[i].Processed != report.Models[j].Processed {
			return report.Models[i].Processed > report.Models[j].Processed
		}
		return report.Models[i].Model < report.Models[j].Model
	})
	for i, point := range points {
		report.Points = append(report.Points, pointView{Point: point, totalsView: pointAggs[i].view()})
	}
	return report
}

// familySlotFor colours a model row by its family when the page groups by
// family; under account grouping model rows stay neutral.
func familySlotFor(family, groupBy string, colors map[string]int) int {
	if groupBy != groupByFamily {
		return 0
	}
	return colors[family]
}

// assignColors gives each group a palette slot from its use over the whole
// retained history, so a group keeps its colour when the range changes.
// Groups past the palette fold into "Other".
func assignColors(groupBy string, history []storedRow, groupOf func(storedRow) string) map[string]int {
	totals := make(map[string]int64)
	for _, r := range history {
		totals[groupOf(r)] += r.Counters.processed()
	}
	keys := make([]string, 0, len(totals))
	for key := range totals {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if totals[keys[i]] != totals[keys[j]] {
			return totals[keys[i]] > totals[keys[j]]
		}
		return keys[i] < keys[j]
	})
	colors := make(map[string]int, len(keys))
	taken := make(map[int]bool)
	if groupBy == groupByFamily {
		for _, key := range keys {
			if slot, pinned := familySlots[key]; pinned && !taken[slot] {
				colors[key] = slot
				taken[slot] = true
			}
		}
	}
	for _, key := range keys {
		if _, done := colors[key]; done {
			continue
		}
		for slot := 1; slot <= paletteSlots; slot++ {
			if !taken[slot] {
				colors[key] = slot
				taken[slot] = true
				break
			}
		}
	}
	return colors
}

// accountName prefers the live name from CLIProxyAPI, then the name saved
// with the usage, then a placeholder from the auth index.
func accountName(key string, live, saved map[string]accountInfo) accountInfo {
	if key == "" {
		return accountInfo{Label: "Unattributed"}
	}
	if info, ok := live[key]; ok && info.Label != "" {
		return info
	}
	if info, ok := saved[key]; ok && info.Label != "" {
		return info
	}
	short := key
	if len(short) > 8 {
		short = short[:8]
	}
	return accountInfo{Label: "Account " + short}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// summary assembles the report for a management request from live state.
func summary(query reportQuery) (reportView, error) {
	spec, groupBy, loc, errQuery := parseQuery(query)
	if errQuery != nil {
		return reportView{}, errQuery
	}
	active := activeStore()
	if active == nil {
		return reportView{}, fmt.Errorf("usage store is not running")
	}
	cfg := currentSettings()
	now := time.Now()
	all, saved := active.snapshot(time.Time{}, now.Add(48*time.Hour))
	in := reportInputs{
		Now:       now,
		Rows:      all,
		History:   all,
		Accounts:  saved,
		Rules:     cfg.FamilyRules,
		Retention: cfg.RetentionDays,
	}
	if groupBy == groupByAccount {
		in.Live = make(map[string]accountInfo)
		if entries, errList := hostAuthList(); errList == nil {
			for _, entry := range entries {
				if index := strings.TrimSpace(entry.AuthIndex); index != "" {
					in.Live[index] = accountInfoFromEntry(entry)
				}
			}
		}
	}
	catalog, pricingState := prices.current()
	in.Catalog = catalog
	report := buildReport(spec, groupBy, loc, in)
	report.Pricing = pricingState
	report.Store = active.status()
	return report, nil
}
