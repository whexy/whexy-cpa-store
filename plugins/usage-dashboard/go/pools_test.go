package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var (
	poolNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	nowMs   = float64(poolNow.UnixMilli())
)

const (
	msDay  = 24 * msHour
	msWeek = 7 * msDay
)

func hours(ms float64) float64 { return (ms - nowMs) / msHour }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// poolSample is one route sample: a 5-hour window and a weekly one.
func poolSample(at, weekly, weeklyReset float64) quotaSampleView {
	return quotaSampleView{At: int64(at), Windows: []quotaWindow{
		{ID: "5h", Remaining: 50, ResetAt: int64(at + msHour), PeriodMinutes: 300},
		{ID: "7d", Remaining: weekly, ResetAt: int64(weeklyReset), PeriodMinutes: 7 * 24 * 60},
	}}
}

func TestProjectionSplitsTheBurnAndHandsADryShareOn(t *testing.T) {
	run := projectPool([]poolMember{
		{remaining: 50, resetAt: nowMs + 30*msHour, period: msWeek},
		{remaining: 10, resetAt: nowMs + 100*msHour, period: msWeek},
	}, 4, nowMs, nowMs+72*msHour, 10*msMinute)
	// 2 points an hour each until the small one is dry at 5h, then 4 an hour.
	var empties [][2]float64
	for _, event := range run.empties {
		empties = append(empties, [2]float64{hours(event.at), float64(event.account)})
	}
	if !reflect.DeepEqual(empties, [][2]float64{{5, 1}, {15, 0}, {55, 0}}) {
		t.Fatalf("empties = %v", empties)
	}
	if hours(run.emptyAt) != 15 {
		t.Fatalf("pool empty at %vh", hours(run.emptyAt))
	}
	if !reflect.DeepEqual(run.resets, []poolEvent{{at: nowMs + 30*msHour, account: 0, gain: 100}}) {
		t.Fatalf("resets = %+v", run.resets)
	}
	// A reset repeats its instant, before and after.
	var at30 [][]float64
	for k, at := range run.times {
		if at == nowMs+30*msHour {
			at30 = append(at30, run.values[k])
		}
	}
	if !reflect.DeepEqual(at30, [][]float64{{0, 0}, {100, 0}}) {
		t.Fatalf("values at the reset = %v", at30)
	}
}

func TestProjectionRepeatsResetsEachPeriod(t *testing.T) {
	run := projectPool([]poolMember{{remaining: 80, resetAt: nowMs + msHour, period: 5 * msHour}},
		2, nowMs, nowMs+12*msHour, 10*msMinute)
	var resets [][2]float64
	for _, reset := range run.resets {
		resets = append(resets, [2]float64{hours(reset.at), math.Round(reset.gain)})
	}
	if !reflect.DeepEqual(resets, [][2]float64{{1, 22}, {6, 10}, {11, 10}}) {
		t.Fatalf("resets = %v", resets)
	}
	if run.emptyAt != 0 {
		t.Fatalf("a pool that never runs dry emptied at %vh", hours(run.emptyAt))
	}
}

func TestProjectionStartsAnUnstartedWindowWithItsFirstUse(t *testing.T) {
	run := projectPool([]poolMember{{remaining: 100, period: 5 * msHour}}, 10, nowMs, nowMs+6*msHour, 10*msMinute)
	if len(run.resets) != 1 || hours(run.resets[0].at) != 5 || !near(run.resets[0].gain, 50) {
		t.Fatalf("resets = %+v", run.resets)
	}
	idle := projectPool([]poolMember{{remaining: 100, period: 5 * msHour}}, 0, nowMs, nowMs+6*msHour, 10*msMinute)
	if len(idle.resets) != 0 {
		t.Fatalf("an unused window reset: %+v", idle.resets)
	}
}

func TestProjectionRestoresAReadingFromBeforeItsReset(t *testing.T) {
	run := projectPool([]poolMember{{remaining: 5, resetAt: nowMs - msHour, period: msWeek}}, 0, nowMs, nowMs+msHour, 10*msMinute)
	if run.pool[0] != 100 {
		t.Fatalf("pool = %v", run.pool[0])
	}
}

func TestReadingHoldsUntilTheNextOrItsReset(t *testing.T) {
	track := poolTrack([]quotaSampleView{
		poolSample(nowMs+2*msHour, 60, nowMs+3*msHour),
		poolSample(nowMs, 70, nowMs+3*msHour),
	}, "7d")
	if len(track) != 2 || track[0].at != nowMs || track[1].at != nowMs+2*msHour {
		t.Fatalf("track = %+v", track)
	}
	if _, ok := trackValueAt(track, nowMs-1); ok {
		t.Fatal("a value before the first reading")
	}
	for _, c := range []struct{ at, want float64 }{
		{nowMs + msHour, 70}, {nowMs + 2*msHour, 60}, {nowMs + 3*msHour, 100},
	} {
		if got, _ := trackValueAt(track, c.at); got != c.want {
			t.Fatalf("at %vh = %v, want %v", hours(c.at), got, c.want)
		}
	}
}

func TestUseIsSpreadOverServedTimeNotIdleHours(t *testing.T) {
	reset := nowMs + msWeek
	// Read at 80 before a night off, again unchanged when requests resume,
	// and at 78 half an hour later.
	track := poolTrack([]quotaSampleView{
		poolSample(nowMs, 80, reset),
		poolSample(nowMs+10*msHour, 80, reset),
		poolSample(nowMs+10.5*msHour, 78, reset),
	}, "7d")
	used, covered := usageBuckets(track, nowMs, msHour, 11, nowMs+11*msHour)
	for k := range 10 {
		if used[k] != 0 {
			t.Fatalf("used[%d] = %v", k, used[k])
		}
	}
	if used[10] != 2 {
		t.Fatalf("used[10] = %v", used[10])
	}
	for k, value := range covered {
		if value != msHour {
			t.Fatalf("covered[%d] = %v", k, value)
		}
	}
}

func TestAcrossAResetOnlyUseAfterItCounts(t *testing.T) {
	track := poolTrack([]quotaSampleView{
		poolSample(nowMs, 10, nowMs+30*msMinute),
		poolSample(nowMs+msHour, 96, nowMs+msWeek),
	}, "7d")
	used, _ := usageBuckets(track, nowMs, 30*msMinute, 2, nowMs+msHour)
	if !reflect.DeepEqual(used, []float64{0, 4}) {
		t.Fatalf("used = %v", used)
	}
}

func TestARiseWithoutAResetStillRestoresQuota(t *testing.T) {
	track := poolTrack([]quotaSampleView{
		poolSample(nowMs, 10, 0),
		poolSample(nowMs+msHour, 96, 0),
	}, "7d")
	used, _ := usageBuckets(track, nowMs, msHour, 1, nowMs+msHour)
	if !reflect.DeepEqual(used, []float64{4}) {
		t.Fatalf("used = %v", used)
	}
}

var poolOpts = poolOptions{
	now:         nowMs,
	start:       nowMs - 3*msDay,
	end:         nowMs + 11*msDay,
	lookback:    3 * msHour,
	step:        10 * msMinute,
	usageFactor: 1,
}

// poolAccounts: a falls 3 points an hour over the last three hours; b was
// last read two days ago and has not been used since; c has no reading.
func poolAccounts() []quotaAccountView {
	resetAt := nowMs + 50*msHour
	a := quotaAccountView{Label: "a@example.com"}
	for index := range 10 {
		at := nowMs - 3*msHour + float64(index)*20*msMinute
		a.Samples = append(a.Samples, poolSample(at, 81+3*(nowMs-at)/msHour, resetAt))
	}
	return []quotaAccountView{
		a,
		{Label: "b@example.com", Samples: []quotaSampleView{poolSample(nowMs-2*msDay, 60, resetAt)}},
		{Label: "c@example.com"},
	}
}

func TestPoolPoolsTheLongestWindowAndMeasuresItsBurn(t *testing.T) {
	p, ok := buildPool(poolAccounts(), poolOpts)
	if !ok {
		t.Fatal("no pool")
	}
	if p.windowMinutes != 7*24*60 || len(p.members) != 2 || p.members[0].name != "a@example.com" || p.members[1].name != "b@example.com" {
		t.Fatalf("window %d, members %+v", p.windowMinutes, p.members)
	}
	if !reflect.DeepEqual(p.unseen, []string{"c@example.com"}) || p.poolNow != 141 {
		t.Fatalf("unseen %v, pool %v", p.unseen, p.poolNow)
	}
	if p.burn == nil || p.burn.source != "recorded" || !near(p.burn.perHour, 3) || p.burn.coveredMs != 3*msHour {
		t.Fatalf("burn = %+v", p.burn)
	}
	// 1.5 points an hour each: b is dry at 40h, then a's 21 last 7h more.
	if p.firstEmpty == nil || p.firstEmpty.at != nowMs+40*msHour || p.firstEmpty.account != 1 {
		t.Fatalf("first empty = %+v", p.firstEmpty)
	}
	if math.Abs(p.emptyAt-(nowMs+47*msHour)) > msMinute {
		t.Fatalf("pool empty at %vh", hours(p.emptyAt))
	}
	if p.nextReset == nil || *p.nextReset != (poolGain{at: nowMs + 50*msHour, gain: 200}) {
		t.Fatalf("next reset = %+v", p.nextReset)
	}
	last := len(p.recorded.times) - 1
	if p.recorded.times[last] != nowMs || *p.recorded.pool[last] != 141 {
		t.Fatalf("recorded ends at %vh with %v", hours(p.recorded.times[last]), *p.recorded.pool[last])
	}
	if p.recorded.pool[0] != nil {
		t.Fatalf("the pool before any reading is %v", *p.recorded.pool[0])
	}
	// The drain at now is the burn over the lookback before it.
	lastDrain := len(p.drainValues) - 1
	if p.drainTimes[lastDrain] != nowMs || !near(*p.drainValues[lastDrain], 3) {
		t.Fatalf("drain ends at %vh with %v", hours(p.drainTimes[lastDrain]), *p.drainValues[lastDrain])
	}
}

func TestPoolScalesTheProjectedBurn(t *testing.T) {
	opts := poolOpts
	opts.usageFactor = 2
	p, _ := buildPool(poolAccounts(), opts)
	if !near(*p.projectedBurn, 6) || p.firstEmpty.at != nowMs+20*msHour {
		t.Fatalf("projected burn %v, first empty at %vh", *p.projectedBurn, hours(p.firstEmpty.at))
	}
	opts.usageFactor = 0
	p, _ = buildPool(poolAccounts(), opts)
	if p.emptyAt != 0 || p.firstEmpty != nil || len(p.projection.empties) != 0 {
		t.Fatalf("an unused pool emptied: %+v", p.firstEmpty)
	}
}

func TestPoolWithoutEnoughRecordingBurnsAtTheWindowAverage(t *testing.T) {
	resetAt := nowMs + 50*msHour
	p, _ := buildPool([]quotaAccountView{{Label: "a", Samples: []quotaSampleView{poolSample(nowMs-5*msMinute, 70, resetAt)}}}, poolOpts)
	opened := resetAt - msWeek
	if p.burn == nil || p.burn.source != "window" || !near(p.burn.perHour, 30/((nowMs-opened)/msHour)) {
		t.Fatalf("burn = %+v", p.burn)
	}
}

func TestPoolWithoutAnyBurnHasNoProjection(t *testing.T) {
	p, ok := buildPool([]quotaAccountView{{Label: "a", Samples: []quotaSampleView{poolSample(nowMs-5*msMinute, 70, 0)}}}, poolOpts)
	if !ok || p.burn != nil || p.projection != nil || p.nextReset != nil || p.emptyAt != 0 {
		t.Fatalf("pool = %+v", p)
	}
}

func TestPoolNamesADryAccountEmptyNow(t *testing.T) {
	resetAt := nowMs + 50*msHour
	p, _ := buildPool([]quotaAccountView{
		{Label: "a", Samples: []quotaSampleView{poolSample(nowMs-msHour, 40, resetAt)}},
		{Label: "b", Samples: []quotaSampleView{poolSample(nowMs-msHour, 0, resetAt)}},
	}, poolOpts)
	if p.firstEmpty == nil || p.firstEmpty.at != nowMs || p.firstEmpty.account != 1 {
		t.Fatalf("first empty = %+v", p.firstEmpty)
	}
}

func TestPoolLabelsARecordedResetOnceWithWhatItRestored(t *testing.T) {
	opts := poolOpts
	opts.usageFactor = 0
	p, _ := buildPool([]quotaAccountView{{Label: "a", Samples: []quotaSampleView{
		poolSample(nowMs-2*msHour, 12, nowMs-90*msMinute),
		poolSample(nowMs-msHour, 100, nowMs-90*msMinute+msWeek),
	}}}, opts)
	var recorded []poolJump
	for _, jump := range p.jumps {
		if !jump.projected {
			recorded = append(recorded, jump)
		}
	}
	if !reflect.DeepEqual(recorded, []poolJump{{at: nowMs - 90*msMinute, gain: 88, top: 100}}) {
		t.Fatalf("recorded jumps = %+v", recorded)
	}
}

func TestNoPoolWithoutAReading(t *testing.T) {
	if _, ok := buildPool([]quotaAccountView{{Label: "a"}}, poolOpts); ok {
		t.Fatal("a pool without a reading")
	}
}

func TestKeepStepsDropsOnlyTheInsideOfFlatRuns(t *testing.T) {
	v := func(value float64) *float64 { return &value }
	times := []float64{0, 1, 2, 3, 4, 5, 6}
	pool := []*float64{nil, nil, nil, v(5), v(5), v(5), v(4)}
	keep := keepSteps(times, [][]*float64{pool})
	if !reflect.DeepEqual(keep, []bool{true, false, true, true, false, true, true}) {
		t.Fatalf("keep = %v", keep)
	}
}

func TestKeepBendsDropsPointsOnTheLine(t *testing.T) {
	// A line, a bend, a reset repeating its instant, and a flat tail.
	times := []float64{0, 1, 2, 3, 4, 4, 5, 6}
	pool := []float64{10, 8, 6, 5, 4, 100, 100, 100}
	keep := keepBends(times, [][]float64{pool})
	if !reflect.DeepEqual(keep, []bool{true, false, true, false, true, true, false, true}) {
		t.Fatalf("keep = %v", keep)
	}
}

func TestParsePoolsQuery(t *testing.T) {
	req, errParse := parsePoolsQuery(poolsQuery{})
	if errParse != nil || req.lookback != 24*time.Hour || req.step != 10*time.Minute || req.usageFactor != 1 || req.series != poolSeriesAccounts {
		t.Fatalf("defaults = %+v, %v", req, errParse)
	}
	from, until, _ := req.span(poolNow)
	if !from.Equal(poolNow.Add(-72*time.Hour)) || until.Sub(from) != 14*24*time.Hour {
		t.Fatalf("default span %v – %v", from, until)
	}
	req, errParse = parsePoolsQuery(poolsQuery{Burn: "5h", Usage: "0.5", Step: "1h", Series: "pool", From: "2026-10-20T00:00:00Z"})
	if errParse != nil || req.lookback != 5*time.Hour || req.step != time.Hour || req.usageFactor != 0.5 || req.series != poolSeriesPool {
		t.Fatalf("parsed = %+v, %v", req, errParse)
	}
	if _, until, _ := req.span(poolNow); !until.Equal(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("until = %v", until)
	}
	for _, bad := range []poolsQuery{
		{Burn: "30m"}, {Burn: "8d"}, {Burn: "soon"}, {Usage: "-1"}, {Usage: "NaN"},
		{Step: "1m"}, {Series: "all"}, {From: "yesterday"},
	} {
		if _, errBad := parsePoolsQuery(bad); errBad == nil {
			t.Fatalf("%+v parsed", bad)
		}
	}
	req, _ = parsePoolsQuery(poolsQuery{From: "2026-10-20T00:00:00Z", Until: "2026-10-19T00:00:00Z"})
	if _, _, errSpan := req.span(poolNow); errSpan == nil {
		t.Fatal("until before from")
	}
	req, _ = parsePoolsQuery(poolsQuery{From: "2026-01-01T00:00:00Z", Until: "2026-10-19T00:00:00Z"})
	if _, _, errSpan := req.span(poolNow); errSpan == nil {
		t.Fatal("a span past 92 days")
	}
}

func TestPoolsViewIsCompactAndRenderReady(t *testing.T) {
	accounts := poolAccounts()
	for i := range accounts {
		accounts[i].Provider = "claude"
	}
	accounts = append(accounts, quotaAccountView{Label: "x", Provider: "xai"})
	req, _ := parsePoolsQuery(poolsQuery{Burn: "3h"})
	from := poolNow.Add(-72 * time.Hour)
	view := buildPoolsView(poolNow, from, from.Add(14*24*time.Hour), req, accounts)
	if len(view.Providers) != 2 || view.Providers[0].Provider != "claude" || view.Providers[1].Provider != "xai" {
		t.Fatalf("providers = %+v", view.Providers)
	}
	claude, xai := view.Providers[0], view.Providers[1]
	if xai.Window != nil || !reflect.DeepEqual(xai.Unreported, []string{"x"}) || xai.Recorded != nil {
		t.Fatalf("an unread provider = %+v", xai)
	}
	if claude.Label != "Claude" || claude.Window.Label != "7-day limit" || claude.AccountCount != 2 || claude.Capacity != 200 || claude.Remaining != 141 {
		t.Fatalf("claude = %+v", claude)
	}
	if *claude.PoolEmptyAt != int64(math.Round(nowMs+47*msHour)) || claude.FirstEmpty.Account != "b@example.com" {
		t.Fatalf("empty %v, first %+v", *claude.PoolEmptyAt, claude.FirstEmpty)
	}
	// A week of flat readings and a straight projection come down to their
	// corners.
	if n := len(claude.Recorded.Times); n > 30 {
		t.Fatalf("%d recorded points", n)
	}
	if n := len(claude.Projected.Times); n > 30 {
		t.Fatalf("%d projected points", n)
	}
	if len(claude.Recorded.Accounts) != 2 || len(claude.Recorded.Accounts[0]) != len(claude.Recorded.Times) {
		t.Fatalf("recorded accounts = %v", claude.Recorded.Accounts)
	}

	req.series = poolSeriesNone
	bare := buildPoolsView(poolNow, from, from.Add(14*24*time.Hour), req, accounts)
	raw, _ := json.Marshal(bare.Providers[0])
	for _, absent := range []string{`"recorded":{`, `"drain":{`, `"projected":{`} {
		if strings.Contains(string(raw), absent) {
			t.Fatalf("series=none sent %s: %s", absent, raw)
		}
	}
}

func TestPoolsRoute(t *testing.T) {
	active := newStore(t.TempDir(), 30)
	previous := current.Load()
	current.Store(active)
	t.Cleanup(func() {
		current.Store(previous)
		active.close()
	})
	at := time.Now().Add(-time.Hour)
	active.addQuota(quotaSample{At: at.UnixMilli(), Account: "a1", Provider: "claude", Windows: []quotaWindow{
		{ID: "7d", Remaining: 40, ResetAt: at.Add(48 * time.Hour).UnixMilli(), PeriodMinutes: 10080},
	}})
	query := url.Values{"burn": {"never"}}
	resp := routeManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: poolsPath, Query: query})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(resp.Body), "burn must be a duration") {
		t.Fatalf("bad burn = %d %s", resp.StatusCode, resp.Body)
	}
	resp = routeManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: poolsPath})
	var view poolsView
	if errUnmarshal := json.Unmarshal(resp.Body, &view); resp.StatusCode != http.StatusOK || errUnmarshal != nil {
		t.Fatalf("pools = %d %s", resp.StatusCode, resp.Body)
	}
	if view.BurnHours != 24 || view.StepMinutes != 10 || view.Horizon-view.Now != poolHorizon.Milliseconds() || view.Until-view.From != defaultPoolSpan.Milliseconds() {
		t.Fatalf("view = %+v", view)
	}
	if len(view.Providers) != 1 || view.Providers[0].Remaining != 40 || view.Providers[0].NextReset == nil {
		t.Fatalf("providers = %+v", view.Providers)
	}
}
