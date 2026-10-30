package main

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Claude and Codex report each subscription window's use in the headers of
// every response, and CLIProxyAPI hands those headers to usage plugins. No
// other provider reports subscription quota there; Grok, Meta and OpenCode
// Go are read from their usage endpoints instead (poll.go).

const (
	// While requests flow, an unchanged reading is kept this often, so the
	// page spreads a drop over the time requests were served, not over idle
	// hours before them.
	quotaHeartbeat = 30 * time.Minute
	// A change in a window this long is kept at once: the page pools these.
	// Shorter windows change with every request and wait for the heartbeat.
	quotaLongWindowMinutes = 24 * 60
	// Reset instants derived from reset-after-seconds move with the response
	// time; a reset that moves further is a new window.
	quotaResetTolerance     = 5 * time.Minute
	quotaRemainingTolerance = 0.05
	// The page's default fortnight starts three days back.
	defaultQuotaHistory = 17 * 24 * time.Hour
)

// quotaProviders are the providers whose headers carry quota, with their
// windows read from them.
var quotaProviders = map[string]func(http.Header, time.Time) []quotaWindow{
	"claude": claudeQuotaWindows,
	"codex":  codexQuotaWindows,
}

// quotaProviderKnown reports whether an account of this provider can have
// quota readings, from response headers or from polling.
func quotaProviderKnown(provider string) bool {
	if quotaProviders[provider] != nil {
		return true
	}
	_, polled := pollProviders[provider]
	return polled
}

// quotaWindow is one rate-limit window as a response reported it.
type quotaWindow struct {
	// ID names the window by its length, "5h" or "7d", so the same window
	// from accounts on different plans pools together.
	ID string `json:"id"`
	// Remaining is the percentage left, 0 to 100.
	Remaining float64 `json:"remaining"`
	// ResetAt is in Unix milliseconds; zero when the response named none.
	ResetAt       int64 `json:"reset_at,omitempty"`
	PeriodMinutes int   `json:"period_minutes"`
}

// quotaSample is what one response said of its account's windows.
type quotaSample struct {
	// At is the request time in Unix milliseconds.
	At       int64         `json:"at"`
	Account  string        `json:"account"`
	Provider string        `json:"provider"`
	Windows  []quotaWindow `json:"windows"`
}

// quotaFromRecord reads the windows a response reported.
func quotaFromRecord(record pluginapi.UsageRecord) (quotaSample, bool) {
	account := strings.TrimSpace(record.AuthIndex)
	provider := strings.ToLower(strings.TrimSpace(record.Provider))
	read := quotaProviders[provider]
	if account == "" || read == nil || len(record.ResponseHeaders) == 0 {
		return quotaSample{}, false
	}
	at := record.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	windows := read(record.ResponseHeaders, at)
	if len(windows) == 0 {
		return quotaSample{}, false
	}
	return quotaSample{At: at.UnixMilli(), Account: account, Provider: provider, Windows: windows}, true
}

// claudeQuotaWindows reads the unified 5-hour and weekly limits every
// subscription has; per-model and overage limits are left out.
func claudeQuotaWindows(headers http.Header, _ time.Time) []quotaWindow {
	var windows []quotaWindow
	for _, window := range []struct {
		id      string
		minutes int
	}{{"5h", 5 * 60}, {"7d", 7 * 24 * 60}} {
		prefix := "Anthropic-Ratelimit-Unified-" + window.id + "-"
		utilization, ok := headerFloat(headers, prefix+"Utilization")
		if !ok {
			continue
		}
		windows = append(windows, quotaWindow{
			ID:            window.id,
			Remaining:     remainingPercent(100 * (1 - utilization)),
			ResetAt:       headerInstant(headers, prefix+"Reset"),
			PeriodMinutes: window.minutes,
		})
	}
	return windows
}

// codexQuotaWindows reads the primary and secondary windows of Codex's main
// limit, whatever their lengths on the account's plan.
func codexQuotaWindows(headers http.Header, at time.Time) []quotaWindow {
	var windows []quotaWindow
	seen := make(map[string]bool)
	for _, slot := range []string{"Primary", "Secondary"} {
		prefix := "X-Codex-" + slot + "-"
		used, okUsed := headerFloat(headers, prefix+"Used-Percent")
		minutes, okMinutes := headerFloat(headers, prefix+"Window-Minutes")
		if !okUsed || !okMinutes || minutes < 1 {
			continue
		}
		window := quotaWindow{
			ID:            windowID(int(minutes)),
			Remaining:     remainingPercent(100 - used),
			ResetAt:       headerInstant(headers, prefix+"Reset-At"),
			PeriodMinutes: int(minutes),
		}
		if window.ResetAt == 0 {
			if after, ok := headerFloat(headers, prefix+"Reset-After-Seconds"); ok && after >= 0 {
				window.ResetAt = at.Add(time.Duration(after * float64(time.Second))).UnixMilli()
			}
		}
		if seen[window.ID] {
			continue
		}
		seen[window.ID] = true
		windows = append(windows, window)
	}
	return windows
}

func windowID(minutes int) string {
	switch {
	case minutes%(24*60) == 0:
		return strconv.Itoa(minutes/(24*60)) + "d"
	case minutes%60 == 0:
		return strconv.Itoa(minutes/60) + "h"
	default:
		return strconv.Itoa(minutes) + "m"
	}
}

func remainingPercent(value float64) float64 {
	return math.Round(min(max(value, 0), 100)*10) / 10
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

// headerInstant reads Unix seconds, or an RFC 3339 time, as Unix
// milliseconds; zero when the header is missing or unreadable.
func headerInstant(headers http.Header, name string) int64 {
	value := headerValue(headers, name)
	if seconds, errParse := strconv.ParseFloat(value, 64); errParse == nil && seconds > 0 {
		return int64(seconds * 1000)
	}
	if parsed, errParse := time.Parse(time.RFC3339, value); errParse == nil {
		return parsed.UnixMilli()
	}
	return 0
}

// quotaSampleDue reports whether next adds to an account's history whose last
// kept sample is prev.
func quotaSampleDue(prev *quotaSample, next quotaSample) bool {
	if prev == nil {
		return true
	}
	// Concurrent requests finish out of order; an earlier reading is stale.
	if next.At <= prev.At {
		return false
	}
	if time.Duration(next.At-prev.At)*time.Millisecond >= quotaHeartbeat {
		return true
	}
	if len(prev.Windows) != len(next.Windows) {
		return true
	}
	for i, window := range next.Windows {
		before := prev.Windows[i]
		if before.ID != window.ID {
			return true
		}
		if time.Duration(absInt64(window.ResetAt-before.ResetAt))*time.Millisecond > quotaResetTolerance {
			return true
		}
		if window.PeriodMinutes >= quotaLongWindowMinutes && math.Abs(window.Remaining-before.Remaining) >= quotaRemainingTolerance {
			return true
		}
	}
	return false
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

// sortQuotaSamples orders samples by time and drops repeats of one account's
// instant, which a spill merged twice would leave.
func sortQuotaSamples(samples []quotaSample) []quotaSample {
	sort.Slice(samples, func(i, j int) bool {
		if samples[i].At != samples[j].At {
			return samples[i].At < samples[j].At
		}
		return samples[i].Account < samples[j].Account
	})
	out := samples[:0]
	for i, sample := range samples {
		if i > 0 && sample.At == samples[i-1].At && sample.Account == samples[i-1].Account {
			continue
		}
		out = append(out, sample)
	}
	return out
}

type quotaReportView struct {
	GeneratedAt time.Time          `json:"generated_at"`
	Accounts    []quotaAccountView `json:"accounts"`
}

type quotaAccountView struct {
	Key      string            `json:"key"`
	Label    string            `json:"label"`
	Provider string            `json:"provider"`
	Samples  []quotaSampleView `json:"samples"`
}

type quotaSampleView struct {
	At      int64         `json:"at"`
	Windows []quotaWindow `json:"windows"`
}

// quotaReport answers the quota route from live state.
func quotaReport(fromParam string) (quotaReportView, error) {
	now := time.Now()
	from := now.Add(-defaultQuotaHistory)
	if value := strings.TrimSpace(fromParam); value != "" {
		parsed, errParse := time.Parse(time.RFC3339, value)
		if errParse != nil {
			return quotaReportView{}, fmt.Errorf("from must be an RFC 3339 time, got %q", value)
		}
		from = parsed
	}
	active := activeStore()
	if active == nil {
		return quotaReportView{}, fmt.Errorf("usage store is not running")
	}
	samples, saved := active.quotaSnapshot(from.UnixMilli())
	entries, errList := hostAuthList()
	return buildQuotaReport(now, samples, saved, entries, errList == nil), nil
}

// buildQuotaReport lists the accounts CLIProxyAPI routes to, each with its
// samples, including those no response has reported on yet. Without the
// host's list it lists every account that has samples.
func buildQuotaReport(now time.Time, samples []quotaSample, saved map[string]accountInfo, entries []pluginapi.HostAuthFileEntry, listed bool) quotaReportView {
	accounts := make(map[string]*quotaAccountView)
	live := make(map[string]accountInfo)
	for _, entry := range entries {
		key := strings.TrimSpace(entry.AuthIndex)
		info := accountInfoFromEntry(entry)
		if key == "" || entry.Disabled || !quotaProviderKnown(info.Provider) {
			continue
		}
		live[key] = info
		accounts[key] = &quotaAccountView{Key: key, Provider: info.Provider, Samples: []quotaSampleView{}}
	}
	for _, sample := range samples {
		account := accounts[sample.Account]
		if account == nil {
			if listed {
				continue
			}
			account = &quotaAccountView{Key: sample.Account, Provider: sample.Provider, Samples: []quotaSampleView{}}
			accounts[sample.Account] = account
		}
		account.Samples = append(account.Samples, quotaSampleView{At: sample.At, Windows: sample.Windows})
	}
	view := quotaReportView{GeneratedAt: now, Accounts: make([]quotaAccountView, 0, len(accounts))}
	for key, account := range accounts {
		account.Label = accountName(key, live, saved).Label
		view.Accounts = append(view.Accounts, *account)
	}
	sort.Slice(view.Accounts, func(i, j int) bool {
		a, b := view.Accounts[i], view.Accounts[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		return a.Key < b.Key
	})
	return view
}
