package main

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Codex and Claude are the providers whose subscription quota can be read:
// both report each window's use in the headers of every response, and both
// answer a usage endpoint for an account that is idle. Every other account's
// quota is unknown.
const (
	providerCodex  = "codex"
	providerClaude = "claude"
)

func quotaProvider(provider string) bool {
	return provider == providerCodex || provider == providerClaude
}

// window is one rate-limit window of an account as last read.
type window struct {
	Minutes int `json:"minutes"`
	// Used is the percentage of the window's allowance spent, 0 to 100.
	Used float64 `json:"used"`
	// ResetAt is zero when the reading named no reset.
	ResetAt time.Time `json:"reset_at,omitzero"`
}

// quota is what one reading said of an account's windows.
type quota struct {
	Windows []window  `json:"windows"`
	At      time.Time `json:"at"`
}

func codexQuotaFromHeaders(headers http.Header, at time.Time) *quota {
	var windows []window
	for _, slot := range []string{"Primary", "Secondary"} {
		prefix := "X-Codex-" + slot + "-"
		used, okUsed := headerFloat(headers, prefix+"Used-Percent")
		minutes, okMinutes := headerFloat(headers, prefix+"Window-Minutes")
		if !okUsed || !okMinutes || minutes < 1 {
			continue
		}
		reset := headerInstant(headers, prefix+"Reset-At")
		if reset.IsZero() {
			if after, ok := headerFloat(headers, prefix+"Reset-After-Seconds"); ok && after >= 0 {
				reset = at.Add(time.Duration(after * float64(time.Second)))
			}
		}
		windows = append(windows, window{Minutes: int(minutes), Used: clampPercent(used), ResetAt: reset})
	}
	return newQuota(windows, at)
}

// claudeQuotaFromHeaders reads the unified 5-hour and weekly windows every
// subscription has. Utilization there is a fraction, and a window the
// provider refuses is spent whatever its utilization says.
func claudeQuotaFromHeaders(headers http.Header, at time.Time) *quota {
	var windows []window
	for _, slot := range []struct {
		name    string
		minutes int
	}{{"5h", 5 * 60}, {"7d", 7 * 24 * 60}} {
		prefix := "Anthropic-Ratelimit-Unified-" + slot.name + "-"
		utilization, ok := headerFloat(headers, prefix+"Utilization")
		if !ok {
			continue
		}
		used := clampPercent(100 * utilization)
		if strings.EqualFold(headerValue(headers, prefix+"Status"), "rejected") {
			used = 100
		}
		windows = append(windows, window{Minutes: slot.minutes, Used: used, ResetAt: headerInstant(headers, prefix+"Reset")})
	}
	return newQuota(windows, at)
}

// codexUsage is the part of chatgpt.com/backend-api/wham/usage read here.
type codexUsage struct {
	PlanType  string `json:"plan_type"`
	RateLimit *struct {
		PrimaryWindow   *codexUsageWindow `json:"primary_window"`
		SecondaryWindow *codexUsageWindow `json:"secondary_window"`
	} `json:"rate_limit"`
}

type codexUsageWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds float64  `json:"limit_window_seconds"`
	ResetAfterSeconds  *float64 `json:"reset_after_seconds"`
	ResetAt            float64  `json:"reset_at"`
}

func codexQuotaFromUsage(body []byte, at time.Time) (*quota, string, bool) {
	var usage codexUsage
	if json.Unmarshal(body, &usage) != nil || usage.RateLimit == nil {
		return nil, "", false
	}
	var windows []window
	for _, raw := range []*codexUsageWindow{usage.RateLimit.PrimaryWindow, usage.RateLimit.SecondaryWindow} {
		if raw == nil || raw.UsedPercent == nil || raw.LimitWindowSeconds < 60 {
			continue
		}
		reset := unixSeconds(raw.ResetAt)
		if reset.IsZero() && raw.ResetAfterSeconds != nil && *raw.ResetAfterSeconds >= 0 {
			reset = at.Add(time.Duration(*raw.ResetAfterSeconds * float64(time.Second)))
		}
		windows = append(windows, window{Minutes: int(raw.LimitWindowSeconds / 60), Used: clampPercent(*raw.UsedPercent), ResetAt: reset})
	}
	q := newQuota(windows, at)
	return q, strings.TrimSpace(usage.PlanType), q != nil
}

// claudeUsageWindow is one window of api.anthropic.com/api/oauth/usage, whose
// utilization, unlike the headers', is a percentage.
type claudeUsageWindow struct {
	Utilization *float64   `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
}

func claudeQuotaFromUsage(body []byte, at time.Time) (*quota, bool) {
	var usage struct {
		FiveHour *claudeUsageWindow `json:"five_hour"`
		SevenDay *claudeUsageWindow `json:"seven_day"`
	}
	if json.Unmarshal(body, &usage) != nil {
		return nil, false
	}
	var windows []window
	for _, slot := range []struct {
		raw     *claudeUsageWindow
		minutes int
	}{{usage.FiveHour, 5 * 60}, {usage.SevenDay, 7 * 24 * 60}} {
		if slot.raw == nil || slot.raw.Utilization == nil {
			continue
		}
		w := window{Minutes: slot.minutes, Used: clampPercent(*slot.raw.Utilization)}
		if slot.raw.ResetsAt != nil {
			w.ResetAt = *slot.raw.ResetsAt
		}
		windows = append(windows, w)
	}
	q := newQuota(windows, at)
	return q, q != nil
}

// newQuota keeps one window per length, shortest first; a reading without any
// window says nothing.
func newQuota(windows []window, at time.Time) *quota {
	if len(windows) == 0 {
		return nil
	}
	sort.SliceStable(windows, func(i, j int) bool { return windows[i].Minutes < windows[j].Minutes })
	kept := windows[:0]
	for i, w := range windows {
		if i > 0 && w.Minutes == kept[len(kept)-1].Minutes {
			continue
		}
		kept = append(kept, w)
	}
	return &quota{Windows: kept, At: at}
}

type standingKind string

const (
	// standingSpare: every window has allowance left.
	standingSpare standingKind = "spare"
	// standingSpent: a window is used up until its reset.
	standingSpent   standingKind = "spent"
	standingUnknown standingKind = "unknown"
)

// windowView is a window as it stands now. A window whose reset has passed
// since it was read is renewed: nothing of it is spent, and its next reset is
// at the latest one window length away, since Codex starts a window only at
// the account's first request after the reset.
type windowView struct {
	Minutes int       `json:"minutes"`
	Used    float64   `json:"used"`
	ResetAt time.Time `json:"reset_at,omitzero"`
	Renewed bool      `json:"renewed,omitempty"`
}

// standing is an account's quota as of one instant.
type standing struct {
	Kind    standingKind `json:"kind"`
	Windows []windowView `json:"windows,omitempty"`
	// Decides is the length of the window whose reset orders the account: its
	// longest, because what that window has left is what the reset loses.
	Decides int `json:"decides,omitempty"`
	// ResetAt is the deciding window's reset, zero when unknown.
	ResetAt time.Time `json:"reset_at,omitzero"`
	// BackAt is when a spent account can serve again, zero when unknown.
	BackAt time.Time `json:"back_at,omitzero"`
}

func evaluate(q *quota, now time.Time) standing {
	if q == nil || len(q.Windows) == 0 {
		return standing{Kind: standingUnknown}
	}
	st := standing{Kind: standingSpare, Windows: make([]windowView, 0, len(q.Windows))}
	spentUnknownBack := false
	for _, w := range q.Windows {
		view := windowView{Minutes: w.Minutes, Used: w.Used, ResetAt: w.ResetAt}
		if !w.ResetAt.IsZero() && !w.ResetAt.After(now) {
			view = windowView{Minutes: w.Minutes, ResetAt: now.Add(time.Duration(w.Minutes) * time.Minute), Renewed: true}
		}
		st.Windows = append(st.Windows, view)
		if view.Used < 100 {
			continue
		}
		st.Kind = standingSpent
		if view.ResetAt.IsZero() {
			spentUnknownBack = true
		} else if view.ResetAt.After(st.BackAt) {
			st.BackAt = view.ResetAt
		}
	}
	if spentUnknownBack {
		st.BackAt = time.Time{}
	}
	longest := st.Windows[len(st.Windows)-1]
	st.Decides = longest.Minutes
	st.ResetAt = longest.ResetAt
	return st
}

// clampPercent keeps one decimal, and never rounds up to 100 while some of
// the window is left: 100 is what makes an account spent.
func clampPercent(value float64) float64 {
	value = min(max(value, 0), 100)
	rounded := math.Round(value*10) / 10
	if rounded >= 100 && value < 100 {
		return 99.9
	}
	return rounded
}

// headerValue matches the name in any case: the headers arrive as JSON, so
// nothing guarantees canonical keys.
func headerValue(headers http.Header, name string) string {
	if values := headers.Values(name); len(values) > 0 {
		return strings.TrimSpace(values[len(values)-1])
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return strings.TrimSpace(values[len(values)-1])
		}
	}
	return ""
}

func headerFloat(headers http.Header, name string) (float64, bool) {
	value, errParse := strconv.ParseFloat(headerValue(headers, name), 64)
	if errParse != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

// headerInstant reads Unix seconds or an RFC 3339 time; zero when the header
// is missing or unreadable.
func headerInstant(headers http.Header, name string) time.Time {
	value := headerValue(headers, name)
	if seconds, errParse := strconv.ParseFloat(value, 64); errParse == nil {
		return unixSeconds(seconds)
	}
	if parsed, errParse := time.Parse(time.RFC3339, value); errParse == nil {
		return parsed
	}
	return time.Time{}
}

func unixSeconds(seconds float64) time.Time {
	if seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return time.Time{}
	}
	return time.UnixMilli(int64(seconds * 1000))
}
