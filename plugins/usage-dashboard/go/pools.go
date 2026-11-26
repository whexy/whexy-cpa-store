package main

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/whexy/whexy-cpa-store/pluginkit/host"
	"github.com/whexy/whexy-cpa-store/pluginkit/manage"
)

// Each provider's quota pool: the longest window its accounts report,
// summed over them, as recorded and as projected. Times inside are Unix
// milliseconds as float64, since the projection splits steps at fractional
// instants.

const (
	// defaultPoolStep spaces the recorded grid and the projected points
	// between events.
	defaultPoolStep = 10 * time.Minute
	minPoolStep     = 10 * time.Minute
	maxPoolStep     = 24 * time.Hour
	// poolHorizon is how far the stats look ahead, whatever span is shown.
	poolHorizon         = 14 * 24 * time.Hour
	defaultPoolLookback = 24 * time.Hour
	minPoolLookback     = time.Hour
	maxPoolLookback     = 7 * 24 * time.Hour
	maxUsageFactor      = 100
	// The default span is a fortnight from three days back.
	defaultPoolPast = 3 * 24 * time.Hour
	defaultPoolSpan = 14 * 24 * time.Hour
	maxPoolSpan     = 92 * 24 * time.Hour
	// An account read for less of the lookback than this has no rate yet.
	minRateCoverage = 15 * time.Minute
	// A window open for less than this has too little use behind it for an
	// average.
	minWindowAverage = time.Hour
	// A rise this large with no reset passed still restored quota.
	riseThreshold = 0.5
	// Rises below a point are rounding, not a reset.
	minJump     = 1.0
	poolEpsilon = 1e-6
)

const (
	msMinute = float64(time.Minute / time.Millisecond)
	msHour   = float64(time.Hour / time.Millisecond)
)

// poolProviders orders the providers and names them.
var poolProviders = []struct{ key, label string }{
	{"claude", "Claude"},
	{"codex", "Codex"},
	{"xai", "Grok"},
	{"meta", "Meta"},
	{"opencode-go", "OpenCode Go"},
}

// trackPoint is one reading of an account's pooled window. resetAt and
// period are zero when unknown.
type trackPoint struct {
	at, remaining, resetAt, period float64
}

// poolTrack is one account's readings of one window, oldest first.
func poolTrack(samples []quotaSampleView, windowID string) []trackPoint {
	var track []trackPoint
	for _, sample := range samples {
		for _, window := range sample.Windows {
			if window.ID != windowID {
				continue
			}
			track = append(track, trackPoint{
				at:        float64(sample.At),
				remaining: window.Remaining,
				resetAt:   float64(window.ResetAt),
				period:    float64(window.PeriodMinutes) * msMinute,
			})
			break
		}
	}
	sort.SliceStable(track, func(i, j int) bool { return track[i].at < track[j].at })
	return track
}

// indexAtOrBefore is the index of the last of n ascending times at or
// before t; -1 when all are later.
func indexAtOrBefore(n int, timeOf func(int) float64, t float64) int {
	return sort.Search(n, func(i int) bool { return timeOf(i) > t }) - 1
}

// trackValueAt is the window's figure at t: the reading then standing, or
// 100 once its reset passed. Every change of a pooled window is kept, so it
// held still between readings. False before the first reading.
func trackValueAt(track []trackPoint, t float64) (float64, bool) {
	index := indexAtOrBefore(len(track), func(i int) float64 { return track[i].at }, t)
	if index < 0 {
		return 0, false
	}
	point := track[index]
	if point.resetAt != 0 && point.resetAt <= t {
		return 100, true
	}
	return point.remaining, true
}

// usageBuckets is the points used in each step bucket from `from`, and how
// much of each bucket the readings cover, from the first reading to now. A
// drop between two readings is spread over the time between them; across a
// reset only the use after it is known.
func usageBuckets(track []trackPoint, from, step float64, count int, now float64) (used, covered []float64) {
	used = make([]float64, count)
	covered = make([]float64, count)
	spread := func(target []float64, start, end, amount float64) {
		if end <= start {
			bucket := int(math.Floor((end - from) / step))
			if bucket >= 0 && bucket < count {
				target[bucket] += amount
			}
			return
		}
		first := max(0, int(math.Floor((start-from)/step)))
		last := min(count-1, int(math.Floor((end-from-1)/step)))
		for bucket := first; bucket <= last; bucket++ {
			bucketStart := from + float64(bucket)*step
			overlap := min(end, bucketStart+step) - max(start, bucketStart)
			if overlap > 0 {
				target[bucket] += amount * overlap / (end - start)
			}
		}
	}
	if len(track) > 0 && now > track[0].at {
		spread(covered, track[0].at, now, now-track[0].at)
	}
	for index := 0; index+1 < len(track); index++ {
		before, after := track[index], track[index+1]
		reset := before.resetAt != 0 && before.resetAt > before.at && before.resetAt <= after.at
		start := before.at
		drop := before.remaining - after.remaining
		if reset || after.remaining > before.remaining+riseThreshold {
			drop = 100 - after.remaining
			if reset {
				start = before.resetAt
			}
		}
		if drop > 0 {
			spread(used, start, after.at, drop)
		}
	}
	return used, covered
}

// usageTotals holds running totals of an account's buckets: entry k sums
// buckets before k.
type usageTotals struct{ used, covered []float64 }

func runningUsage(used, covered []float64) usageTotals {
	sum := func(values []float64) []float64 {
		out := make([]float64, len(values)+1)
		for i, value := range values {
			out[i+1] = out[i] + value
		}
		return out
	}
	return usageTotals{used: sum(used), covered: sum(covered)}
}

// poolRate is the pool's burn over buckets first..last: each account's own
// rate over the time its readings cover, summed. False when no account was
// read long enough.
func poolRate(usages []usageTotals, first, last int) (perHour, coveredMs float64, ok bool) {
	minCoverage := float64(minRateCoverage / time.Millisecond)
	for _, usage := range usages {
		low := max(0, first)
		high := min(last+1, len(usage.used)-1)
		if high <= low {
			continue
		}
		used := usage.used[high] - usage.used[low]
		covered := usage.covered[high] - usage.covered[low]
		coveredMs = max(coveredMs, covered)
		if covered < minCoverage {
			continue
		}
		ok = true
		perHour += used / (covered / msHour)
	}
	return perHour, coveredMs, ok
}

// upcomingReset is the first reset after `after`, moving a passed one on by
// whole periods; zero when it passed and the period is unknown.
func upcomingReset(resetAt, period, after float64) float64 {
	if resetAt > after {
		return resetAt
	}
	if period == 0 {
		return 0
	}
	return resetAt + math.Ceil((after-resetAt+1)/period)*period
}

// poolMember is an account in its pool, as of now.
type poolMember struct {
	name      string
	track     []trackPoint
	remaining float64
	// resetAt is the next reset after now; zero when unknown.
	resetAt float64
	period  float64
}

type poolEvent struct {
	at      float64
	account int
	gain    float64
}

type poolGain struct{ at, gain float64 }

// projection is where a pool goes: values[k][i] is account i at times[k],
// and a reset repeats its instant, before and after.
type projection struct {
	times   []float64
	values  [][]float64
	pool    []float64
	resets  []poolEvent
	empties []poolEvent
	// emptyAt is when every account is dry; zero when not before the end.
	emptyAt float64
}

// projectPool runs a pool on from `from`. The burn is split evenly over the
// accounts with quota left, as CLIProxyAPI's default round-robin routing
// spreads requests; an account that runs dry hands its share to the rest,
// and a reset puts it back at 100%.
func projectPool(members []poolMember, burnPerHour, from, until, step float64) projection {
	n := len(members)
	remaining := make([]float64, n)
	nextReset := make([]float64, n)
	for i, member := range members {
		remaining[i] = min(100, max(0, member.remaining))
		switch {
		case member.resetAt == 0:
		case member.resetAt > from:
			nextReset[i] = member.resetAt
		default:
			// The reading predates its reset, which has since restored it.
			remaining[i] = 100
			nextReset[i] = upcomingReset(member.resetAt, member.period, from)
		}
	}
	var p projection
	push := func(at float64) {
		p.times = append(p.times, at)
		p.values = append(p.values, append([]float64(nil), remaining...))
		total := 0.0
		for _, value := range remaining {
			total += value
		}
		p.pool = append(p.pool, total)
	}
	allDry := func() bool {
		for _, value := range remaining {
			if value > poolEpsilon {
				return false
			}
		}
		return true
	}
	markEmpty := func(at float64) {
		if p.emptyAt == 0 && n > 0 {
			p.emptyAt = at
		}
	}
	ratePerMs := max(0, burnPerHour) / msHour
	drain := func(start, end float64) {
		amount := ratePerMs * (end - start)
		cursor := start
		for amount > poolEpsilon {
			var active []int
			for i, value := range remaining {
				if value > poolEpsilon {
					active = append(active, i)
				}
			}
			if len(active) == 0 {
				markEmpty(cursor)
				return
			}
			// A window that has not started yet starts with its first use.
			for _, i := range active {
				if nextReset[i] == 0 && members[i].resetAt == 0 && members[i].period != 0 {
					nextReset[i] = cursor + members[i].period
				}
			}
			share := amount / float64(len(active))
			smallest := math.Inf(1)
			for _, i := range active {
				smallest = min(smallest, remaining[i])
			}
			taken := min(share, smallest)
			if share <= smallest {
				cursor = end
			} else {
				cursor += smallest * float64(len(active)) / ratePerMs
			}
			amount -= taken * float64(len(active))
			emptied := false
			for _, i := range active {
				remaining[i] -= taken
				if remaining[i] <= poolEpsilon {
					remaining[i] = 0
					p.empties = append(p.empties, poolEvent{at: cursor, account: i})
					emptied = true
				}
			}
			if !emptied {
				return
			}
			push(cursor)
			if allDry() {
				markEmpty(cursor)
			}
		}
	}

	if n > 0 && allDry() {
		markEmpty(from)
	}
	push(from)
	for cursor := from; cursor < until; {
		due := math.Inf(1)
		for _, at := range nextReset {
			if at != 0 && at > cursor && at < due {
				due = at
			}
		}
		next := min(cursor+step, until, due)
		drain(cursor, next)
		cursor = next
		var resetting []int
		for i, at := range nextReset {
			if at != 0 && at <= cursor {
				resetting = append(resetting, i)
			}
		}
		if len(resetting) > 0 {
			push(cursor)
			for _, i := range resetting {
				p.resets = append(p.resets, poolEvent{at: cursor, account: i, gain: 100 - remaining[i]})
				remaining[i] = 100
				nextReset[i] = upcomingReset(nextReset[i], members[i].period, cursor)
			}
		}
		if p.times[len(p.times)-1] != cursor || len(resetting) > 0 {
			push(cursor)
		}
	}
	return p
}

// windowAverage is the pool's burn since each account's window opened;
// false when no window has been open long enough.
func windowAverage(members []poolMember, now float64) (float64, bool) {
	perHour, measured := 0.0, false
	for _, member := range members {
		if member.resetAt == 0 || member.period == 0 {
			continue
		}
		elapsed := now - (member.resetAt - member.period)
		if elapsed < float64(minWindowAverage/time.Millisecond) {
			continue
		}
		measured = true
		perHour += (100 - member.remaining) / (elapsed / msHour)
	}
	return perHour, measured
}

// groupGains puts events at one instant together, in order of their first
// event, keeping those that restore a point.
func groupGains(events []poolEvent) []poolGain {
	var out []poolGain
	index := make(map[float64]int)
	for _, event := range events {
		if i, ok := index[event.at]; ok {
			out[i].gain += event.gain
			continue
		}
		index[event.at] = len(out)
		out = append(out, poolGain{at: event.at, gain: event.gain})
	}
	kept := out[:0]
	for _, gain := range out {
		if gain.gain >= minJump {
			kept = append(kept, gain)
		}
	}
	return kept
}

// poolOptions are a pools request's parameters; times in Unix milliseconds.
type poolOptions struct {
	now, start, end float64
	lookback, step  float64
	usageFactor     float64
}

type poolJump struct {
	at, gain, top float64
	projected     bool
}

// poolSeries holds one value per time per account; the pool is null where
// no account was read yet.
type poolSeries struct {
	times    []float64
	pool     []*float64
	accounts [][]*float64
}

// pool is one provider's pool, as the route serves it.
type pool struct {
	windowID      string
	windowMinutes int
	members       []poolMember
	unseen        []string
	poolNow       float64
	recorded      poolSeries
	drainTimes    []float64
	drainValues   []*float64
	burn          *poolBurn
	projectedBurn *float64
	projection    *projection
	jumps         []poolJump
	emptyAt       float64
	firstEmpty    *poolEvent
	nextReset     *poolGain
}

type poolBurn struct {
	perHour   float64
	source    string
	coveredMs float64
}

// pooledWindow is the window a provider's pool is drawn from: the longest
// one its accounts last reported, since a fortnight of 5-hour windows is a
// sawtooth.
func pooledWindow(accounts []quotaAccountView) (quotaWindow, bool) {
	var pooled quotaWindow
	found := false
	for _, account := range accounts {
		if len(account.Samples) == 0 {
			continue
		}
		for _, window := range account.Samples[len(account.Samples)-1].Windows {
			if !found || window.PeriodMinutes > pooled.PeriodMinutes {
				pooled, found = window, true
			}
		}
	}
	return pooled, found
}

// buildPool works out one provider's pool: what was recorded, how fast it
// burns and where it goes from now. False when no account has a reading.
func buildPool(accounts []quotaAccountView, opts poolOptions) (pool, bool) {
	pooled, found := pooledWindow(accounts)
	if !found {
		return pool{}, false
	}
	now, start, end, step := opts.now, opts.start, opts.end, opts.step
	out := pool{windowID: pooled.ID, windowMinutes: pooled.PeriodMinutes, unseen: []string{}}
	for _, account := range accounts {
		track := poolTrack(account.Samples, pooled.ID)
		if len(track) == 0 {
			out.unseen = append(out.unseen, account.Label)
			continue
		}
		last := track[len(track)-1]
		remaining, ok := trackValueAt(track, now)
		if !ok {
			remaining = last.remaining
		}
		member := poolMember{name: account.Label, track: track, remaining: remaining, period: last.period}
		if last.resetAt != 0 {
			member.resetAt = upcomingReset(last.resetAt, last.period, now)
		}
		out.members = append(out.members, member)
	}
	if len(out.members) == 0 {
		return pool{}, false
	}
	members := out.members
	for _, member := range members {
		out.poolNow += member.remaining
	}

	// Recorded: the grid through the span up to now, then now itself.
	recordedEnd := min(now, end)
	rec := poolSeries{accounts: make([][]*float64, len(members))}
	addRecorded := func(at float64, valueOf func(poolMember) (float64, bool)) {
		rec.times = append(rec.times, at)
		total, any := 0.0, false
		for i, member := range members {
			value, ok := valueOf(member)
			if !ok {
				rec.accounts[i] = append(rec.accounts[i], nil)
				continue
			}
			rec.accounts[i] = append(rec.accounts[i], &value)
			total += value
			any = true
		}
		if any {
			rec.pool = append(rec.pool, &total)
		} else {
			rec.pool = append(rec.pool, nil)
		}
	}
	for at := start; at <= recordedEnd; at += step {
		if at != now {
			addRecorded(at, func(member poolMember) (float64, bool) { return trackValueAt(member.track, at) })
		}
	}
	if now >= start && now <= end {
		addRecorded(now, func(member poolMember) (float64, bool) { return member.remaining, true })
	}
	out.recorded = rec

	var jumps []poolJump
	for k := 1; k < len(rec.times); k++ {
		gain := 0.0
		for i := range members {
			value, before := rec.accounts[i][k], rec.accounts[i][k-1]
			if value != nil && before != nil && *value > *before {
				gain += *value - *before
			}
		}
		if gain < minJump {
			continue
		}
		// A reset spread over neighbouring steps reads as one jump.
		if len(jumps) > 0 && jumps[len(jumps)-1].at == rec.times[k-1] {
			previous := &jumps[len(jumps)-1]
			previous.at = rec.times[k]
			previous.gain += gain
			if rec.pool[k] != nil {
				previous.top = *rec.pool[k]
			}
			continue
		}
		top := 0.0
		if rec.pool[k] != nil {
			top = *rec.pool[k]
		}
		jumps = append(jumps, poolJump{at: rec.times[k], gain: gain, top: top})
	}

	// Burn over the lookback, else the average since each window opened.
	lookback := opts.lookback
	recent := make([]usageTotals, len(members))
	for i, member := range members {
		recent[i] = runningUsage(usageBuckets(member.track, now-lookback, lookback, 1, now))
	}
	if perHour, covered, ok := poolRate(recent, 0, 0); ok {
		out.burn = &poolBurn{perHour: perHour, source: "recorded", coveredMs: covered}
	} else if average, ok := windowAverage(members, now); ok {
		out.burn = &poolBurn{perHour: average, source: "window"}
	}

	// Drain: the burn over the lookback ending at each step.
	lookbackSteps := max(1, int(math.Round(lookback/step)))
	drainSteps := max(0, int(math.Ceil((recordedEnd-start)/step)))
	drainUsages := make([]usageTotals, len(members))
	for i, member := range members {
		drainUsages[i] = runningUsage(usageBuckets(member.track, start-float64(lookbackSteps)*step, step, lookbackSteps+drainSteps, now))
	}
	out.drainTimes = make([]float64, drainSteps)
	out.drainValues = make([]*float64, drainSteps)
	for k := range drainSteps {
		out.drainTimes[k] = min(recordedEnd, start+float64(k+1)*step)
		if perHour, _, ok := poolRate(drainUsages, k+1, k+lookbackSteps); ok {
			out.drainValues[k] = &perHour
		}
	}

	horizonEnd := now + float64(poolHorizon/time.Millisecond)
	if out.burn != nil {
		projected := out.burn.perHour * opts.usageFactor
		out.projectedBurn = &projected
		run := projectPool(members, projected, now, max(end, horizonEnd), step)
		out.projection = &run
		for _, gain := range groupGains(run.resets) {
			top := 0.0
			for k := len(run.times) - 1; k >= 0; k-- {
				if run.times[k] == gain.at {
					top = run.pool[k]
					break
				}
			}
			jumps = append(jumps, poolJump{at: gain.at, gain: gain.gain, top: top, projected: true})
		}
		if run.emptyAt != 0 && run.emptyAt <= horizonEnd {
			out.emptyAt = run.emptyAt
		}
	}
	out.jumps = []poolJump{}
	for _, jump := range jumps {
		if jump.at >= start && jump.at <= end {
			out.jumps = append(out.jumps, jump)
		}
	}

	for i, member := range members {
		if member.remaining <= 0 {
			out.firstEmpty = &poolEvent{at: now, account: i}
			break
		}
	}
	if out.firstEmpty == nil && out.projection != nil {
		for _, event := range out.projection.empties {
			if event.at <= horizonEnd {
				out.firstEmpty = &event
				break
			}
		}
	}

	if out.projection != nil {
		for _, gain := range groupGains(out.projection.resets) {
			if gain.at > now {
				out.nextReset = &gain
				break
			}
		}
	} else {
		var events []poolEvent
		for _, member := range members {
			if member.resetAt != 0 && member.resetAt > now {
				events = append(events, poolEvent{at: member.resetAt, gain: 100 - member.remaining})
			}
		}
		gains := groupGains(events)
		sort.SliceStable(gains, func(i, j int) bool { return gains[i].at < gains[j].at })
		if len(gains) > 0 {
			out.nextReset = &gains[0]
		}
	}
	return out, true
}

// quotaWindowLabel names a window by its length: "7-day limit".
func quotaWindowLabel(minutes int) string {
	switch {
	case minutes%(24*60) == 0:
		return strconv.Itoa(minutes/(24*60)) + "-day limit"
	case minutes%60 == 0:
		return strconv.Itoa(minutes/60) + "-hour limit"
	default:
		return strconv.Itoa(minutes) + "-minute limit"
	}
}

// Series detail a pools request asks for.
const (
	poolSeriesAccounts = "accounts"
	poolSeriesPool     = "pool"
	poolSeriesNone     = "none"
)

type poolsQuery struct {
	Burn, Usage, From, Until, Step, Series string
}

// poolsRequest is a parsed pools query.
type poolsRequest struct {
	lookback, step time.Duration
	usageFactor    float64
	from, until    time.Time
	// fromSet marks a from the caller gave, which a default until follows.
	fromSet, untilSet bool
	series            string
}

func parsePoolsQuery(query poolsQuery) (poolsRequest, error) {
	req := poolsRequest{lookback: defaultPoolLookback, step: defaultPoolStep, usageFactor: 1, series: poolSeriesAccounts}
	duration := func(name, value string, low, high time.Duration, target *time.Duration) error {
		if value = strings.TrimSpace(value); value == "" {
			return nil
		}
		parsed, errParse := time.ParseDuration(value)
		if errParse != nil || parsed < low || parsed > high {
			return fmt.Errorf("%s must be a duration from %s to %s, got %q", name, shortDuration(low), shortDuration(high), value)
		}
		*target = parsed
		return nil
	}
	if errBurn := duration("burn", query.Burn, minPoolLookback, maxPoolLookback, &req.lookback); errBurn != nil {
		return req, errBurn
	}
	if errStep := duration("step", query.Step, minPoolStep, maxPoolStep, &req.step); errStep != nil {
		return req, errStep
	}
	if value := strings.TrimSpace(query.Usage); value != "" {
		factor, errParse := strconv.ParseFloat(value, 64)
		if errParse != nil || math.IsNaN(factor) || factor < 0 || factor > maxUsageFactor {
			return req, fmt.Errorf("usage must be a number from 0 to %d, got %q", maxUsageFactor, value)
		}
		req.usageFactor = factor
	}
	instant := func(name, value string, target *time.Time, set *bool) error {
		if value = strings.TrimSpace(value); value == "" {
			return nil
		}
		parsed, errParse := time.Parse(time.RFC3339, value)
		if errParse != nil {
			return fmt.Errorf("%s must be an RFC 3339 time, got %q", name, value)
		}
		*target, *set = parsed, true
		return nil
	}
	if errFrom := instant("from", query.From, &req.from, &req.fromSet); errFrom != nil {
		return req, errFrom
	}
	if errUntil := instant("until", query.Until, &req.until, &req.untilSet); errUntil != nil {
		return req, errUntil
	}
	switch series := strings.TrimSpace(query.Series); series {
	case "":
	case poolSeriesAccounts, poolSeriesPool, poolSeriesNone:
		req.series = series
	default:
		return req, fmt.Errorf("series must be accounts, pool or none")
	}
	return req, nil
}

// span resolves the shown span against now.
func (req poolsRequest) span(now time.Time) (time.Time, time.Time, error) {
	from, until := req.from, req.until
	switch {
	case req.fromSet && req.untilSet:
	case req.fromSet:
		until = from.Add(defaultPoolSpan)
	case req.untilSet:
		from = until.Add(-defaultPoolSpan)
	default:
		from = now.Add(-defaultPoolPast)
		until = from.Add(defaultPoolSpan)
	}
	if !until.After(from) {
		return from, until, fmt.Errorf("until must be after from")
	}
	if until.Sub(from) > maxPoolSpan {
		return from, until, fmt.Errorf("from and until may be at most %d days apart", int(maxPoolSpan/(24*time.Hour)))
	}
	return from, until, nil
}

// shortDuration prints 10m0s as 10m and 168h0m0s as 168h.
func shortDuration(d time.Duration) string {
	text := d.String()
	if strings.HasSuffix(text, "m0s") {
		text = text[:len(text)-2]
	}
	if strings.HasSuffix(text, "h0m") {
		text = text[:len(text)-2]
	}
	return text
}

// poolsView answers the pools route. Times are Unix milliseconds and
// figures are percentage points: an account holds 0 to 100, a pool 100 per
// account.
type poolsView struct {
	GeneratedAt time.Time  `json:"generated_at"`
	Now         int64      `json:"now"`
	From        int64      `json:"from"`
	Until       int64      `json:"until"`
	Horizon     int64      `json:"horizon"`
	BurnHours   float64    `json:"burn_hours"`
	UsageFactor float64    `json:"usage_factor"`
	StepMinutes float64    `json:"step_minutes"`
	Providers   []poolView `json:"providers"`
}

type poolView struct {
	Provider      string              `json:"provider"`
	Label         string              `json:"label"`
	Window        *poolWindowView     `json:"window"`
	AccountCount  int                 `json:"account_count"`
	Accounts      []poolAccountView   `json:"accounts"`
	Unreported    []string            `json:"unreported"`
	Capacity      int                 `json:"capacity"`
	Remaining     float64             `json:"remaining"`
	Burn          *poolBurnView       `json:"burn"`
	ProjectedBurn *float64            `json:"projected_burn"`
	PoolEmptyAt   *int64              `json:"pool_empty_at"`
	FirstEmpty    *poolFirstEmptyView `json:"first_empty"`
	NextReset     *poolResetView      `json:"next_reset"`
	Jumps         []poolJumpView      `json:"jumps"`
	Recorded      *poolSeriesView     `json:"recorded,omitempty"`
	Drain         *poolDrainView      `json:"drain,omitempty"`
	Projected     *poolSeriesView     `json:"projected,omitempty"`
}

type poolWindowView struct {
	ID      string `json:"id"`
	Minutes int    `json:"minutes"`
	Label   string `json:"label"`
}

type poolAccountView struct {
	Label     string  `json:"label"`
	Remaining float64 `json:"remaining"`
	ResetAt   *int64  `json:"reset_at"`
}

type poolBurnView struct {
	PerHour   float64 `json:"per_hour"`
	Source    string  `json:"source"`
	CoveredMs int64   `json:"covered_ms"`
}

type poolFirstEmptyView struct {
	At      int64  `json:"at"`
	Account string `json:"account"`
}

type poolResetView struct {
	At   int64   `json:"at"`
	Gain float64 `json:"gain"`
}

type poolJumpView struct {
	At        int64   `json:"at"`
	Gain      float64 `json:"gain"`
	Top       float64 `json:"top"`
	Projected bool    `json:"projected"`
}

type poolSeriesView struct {
	Times    []int64      `json:"times"`
	Pool     []*float64   `json:"pool"`
	Accounts [][]*float64 `json:"accounts,omitempty"`
}

type poolDrainView struct {
	Times  []int64    `json:"times"`
	Values []*float64 `json:"values"`
}

// quotaPools answers the pools route from live state.
// A failure is a *manage.Error carrying the status it answers with.
func quotaPools(query poolsQuery) (poolsView, error) {
	req, errQuery := parsePoolsQuery(query)
	if errQuery != nil {
		return poolsView{}, manage.Errorf(http.StatusBadRequest, "%v", errQuery)
	}
	now := time.Now()
	from, until, errSpan := req.span(now)
	if errSpan != nil {
		return poolsView{}, manage.Errorf(http.StatusBadRequest, "%v", errSpan)
	}
	active := activeStore()
	if active == nil {
		return poolsView{}, manage.Errorf(http.StatusServiceUnavailable, "%v", errStoreNotRunning)
	}
	// The burn and the drain series look back from the span's start.
	readFrom := from
	if now.Before(readFrom) {
		readFrom = now
	}
	readFrom = readFrom.Add(-req.lookback - req.step)
	samples, saved := active.quotaSnapshot(readFrom.UnixMilli())
	entries, errList := host.AuthList()
	report := buildQuotaReport(now, samples, saved, entries, errList == nil)
	return buildPoolsView(now, from, until, req, report.Accounts), nil
}

func buildPoolsView(now, from, until time.Time, req poolsRequest, accounts []quotaAccountView) poolsView {
	// A reading stamped after now would come from a clock ahead of this one.
	nowMs := now.UnixMilli()
	for _, account := range accounts {
		for _, sample := range account.Samples {
			nowMs = max(nowMs, sample.At)
		}
	}
	opts := poolOptions{
		now:         float64(nowMs),
		start:       float64(from.UnixMilli()),
		end:         float64(until.UnixMilli()),
		lookback:    float64(req.lookback / time.Millisecond),
		step:        float64(req.step / time.Millisecond),
		usageFactor: req.usageFactor,
	}
	view := poolsView{
		GeneratedAt: now.UTC(),
		Now:         nowMs,
		From:        from.UnixMilli(),
		Until:       until.UnixMilli(),
		Horizon:     nowMs + poolHorizon.Milliseconds(),
		BurnHours:   req.lookback.Hours(),
		UsageFactor: req.usageFactor,
		StepMinutes: req.step.Minutes(),
		Providers:   []poolView{},
	}
	for _, provider := range poolProviders {
		var members []quotaAccountView
		for _, account := range accounts {
			if account.Provider == provider.key {
				members = append(members, account)
			}
		}
		if len(members) == 0 {
			continue
		}
		item := poolView{Provider: provider.key, Label: provider.label, Accounts: []poolAccountView{}, Unreported: []string{}, Jumps: []poolJumpView{}}
		built, ok := buildPool(members, opts)
		if !ok {
			for _, member := range members {
				item.Unreported = append(item.Unreported, member.Label)
			}
			view.Providers = append(view.Providers, item)
			continue
		}
		fillPoolView(&item, built, req.series)
		view.Providers = append(view.Providers, item)
	}
	return view
}

func fillPoolView(item *poolView, built pool, series string) {
	item.Window = &poolWindowView{ID: built.windowID, Minutes: built.windowMinutes, Label: quotaWindowLabel(built.windowMinutes)}
	item.AccountCount = len(built.members)
	for _, member := range built.members {
		account := poolAccountView{Label: member.name, Remaining: member.remaining}
		if member.resetAt != 0 {
			account.ResetAt = instant(member.resetAt)
		}
		item.Accounts = append(item.Accounts, account)
	}
	item.Unreported = built.unseen
	item.Capacity = 100 * len(built.members)
	item.Remaining = built.poolNow
	if built.burn != nil {
		item.Burn = &poolBurnView{PerHour: built.burn.perHour, Source: built.burn.source, CoveredMs: *instant(built.burn.coveredMs)}
	}
	if built.projectedBurn != nil {
		item.ProjectedBurn = built.projectedBurn
	}
	if built.emptyAt != 0 {
		item.PoolEmptyAt = instant(built.emptyAt)
	}
	if built.firstEmpty != nil {
		item.FirstEmpty = &poolFirstEmptyView{At: *instant(built.firstEmpty.at), Account: built.members[built.firstEmpty.account].name}
	}
	if built.nextReset != nil {
		item.NextReset = &poolResetView{At: *instant(built.nextReset.at), Gain: built.nextReset.gain}
	}
	for _, jump := range built.jumps {
		item.Jumps = append(item.Jumps, poolJumpView{At: *instant(jump.at), Gain: jump.gain, Top: jump.top, Projected: jump.projected})
	}
	if series == poolSeriesNone {
		return
	}
	withAccounts := series == poolSeriesAccounts

	recordedColumns := [][]*float64{built.recorded.pool}
	if withAccounts {
		recordedColumns = append(recordedColumns, built.recorded.accounts...)
	}
	keep := keepSteps(built.recorded.times, recordedColumns)
	item.Recorded = &poolSeriesView{Times: pick(keep, instants(built.recorded.times)), Pool: pick(keep, built.recorded.pool)}
	if withAccounts {
		item.Recorded.Accounts = make([][]*float64, len(built.recorded.accounts))
		for i, values := range built.recorded.accounts {
			item.Recorded.Accounts[i] = pick(keep, values)
		}
	}

	keep = keepSteps(built.drainTimes, [][]*float64{built.drainValues})
	item.Drain = &poolDrainView{Times: pick(keep, instants(built.drainTimes)), Values: pick(keep, built.drainValues)}

	if built.projection == nil {
		return
	}
	run := built.projection
	projectedColumns := [][]float64{run.pool}
	accountColumns := make([][]float64, len(built.members))
	for i := range accountColumns {
		accountColumns[i] = make([]float64, len(run.times))
		for k, values := range run.values {
			accountColumns[i][k] = values[i]
		}
	}
	if withAccounts {
		projectedColumns = append(projectedColumns, accountColumns...)
	}
	keep = keepBends(run.times, projectedColumns)
	item.Projected = &poolSeriesView{Times: pick(keep, instants(run.times)), Pool: pick(keep, pointers(run.pool))}
	if withAccounts {
		item.Projected.Accounts = make([][]*float64, len(accountColumns))
		for i, values := range accountColumns {
			item.Projected.Accounts[i] = pick(keep, pointers(values))
		}
	}
}

// seriesTolerance is far below the 0.1-point resolution of a reading.
const seriesTolerance = 1e-6

// keepSteps drops the points inside a flat run of a step series, which a
// client reading the last point at or before an instant cannot miss.
func keepSteps(times []float64, columns [][]*float64) []bool {
	keep := make([]bool, len(times))
	same := func(a, b *float64) bool {
		if a == nil || b == nil {
			return a == nil && b == nil
		}
		return math.Abs(*a-*b) <= seriesTolerance
	}
	for k := range times {
		if k == 0 || k == len(times)-1 {
			keep[k] = true
			continue
		}
		for _, column := range columns {
			if !same(column[k], column[k-1]) || !same(column[k], column[k+1]) {
				keep[k] = true
				break
			}
		}
	}
	return keep
}

// keepBends drops the points of a line series that lie on the line between
// their neighbours, which a client interpolating between points cannot miss.
func keepBends(times []float64, columns [][]float64) []bool {
	keep := make([]bool, len(times))
	last := 0
	for k := range times {
		if k == 0 || k == len(times)-1 {
			keep[k] = true
			continue
		}
		t0, t1, t2 := times[last], times[k], times[k+1]
		for _, column := range columns {
			expected := column[last]
			if t2 > t0 {
				expected += (column[k+1] - column[last]) * (t1 - t0) / (t2 - t0)
			}
			if math.Abs(column[k]-expected) > seriesTolerance {
				keep[k] = true
				break
			}
		}
		if keep[k] {
			last = k
		}
	}
	return keep
}

func pick[T any](keep []bool, values []T) []T {
	out := make([]T, 0, len(values))
	for k, value := range values {
		if keep[k] {
			out = append(out, value)
		}
	}
	return out
}

func pointers(values []float64) []*float64 {
	out := make([]*float64, len(values))
	for k := range values {
		out[k] = &values[k]
	}
	return out
}

func instant(ms float64) *int64 {
	rounded := int64(math.Round(ms))
	return &rounded
}

func instants(times []float64) []int64 {
	out := make([]int64, len(times))
	for k, at := range times {
		out[k] = *instant(at)
	}
	return out
}
