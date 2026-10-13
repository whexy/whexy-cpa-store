package main

import (
	"time"
)

const (
	// A provider that keeps failing is kept out of fresh decisions for a
	// growing window, capped here. CPA's own per-credential cooldown keeps
	// running underneath; this only stops this plugin from choosing a member
	// that is failing for reasons CPA cannot fix by itself.
	breakerBaseWindow = 30 * time.Second
	breakerMaxWindow  = 10 * time.Minute
	// memberBreakerThreshold is how many consecutive failures a provider needs
	// before fresh decisions stop choosing it. A single transient error must
	// not move established sessions.
	memberBreakerThreshold = 2
	// windowMinutes is how long the per-member decision counters are kept.
	windowMinutes = 60
)

// breakerState is one provider's consecutive failures inside one member.
type breakerState struct {
	Failures int
	Until    time.Time
	Last     time.Time
}

// memberStats tracks one member: how often the policy chose it, how the last
// request ended, and the breaker of each provider it can be served by.
type memberStats struct {
	minutes     map[int64]int
	LastUsed    time.Time
	LastOutcome string
	LastError   string
	lastFailure time.Time
	breakers    map[string]breakerState
}

func newMemberStats() memberStats {
	return memberStats{minutes: make(map[int64]int), breakers: make(map[string]breakerState)}
}

// recordDecision counts one routing decision for this member.
func (m *memberStats) recordDecision(now time.Time) {
	if m.minutes == nil {
		m.minutes = make(map[int64]int)
	}
	m.LastUsed = now
	m.minutes[now.Unix()/60]++
	m.prune(now)
}

// recordSuccess clears the provider's breaker.
func (m *memberStats) recordSuccess(provider string, now time.Time) {
	_ = now
	m.LastOutcome = "ok"
	m.LastError = ""
	delete(m.breakers, providerKeyOf(provider))
}

// recordFailure counts a failed request and widens the provider's breaker.
// An empty provider (a member several providers serve, where the completion
// does not say which one was used) records the outcome without blaming one.
func (m *memberStats) recordFailure(provider string, now time.Time, reason string) {
	m.LastOutcome = "failed"
	m.LastError = reason
	m.lastFailure = now

	provider = providerKeyOf(provider)
	if provider == "" {
		return
	}
	if m.breakers == nil {
		m.breakers = make(map[string]breakerState)
	}
	state := m.breakers[provider]
	if !state.Last.IsZero() && now.Sub(state.Last) > breakerMaxWindow {
		state.Failures = 0
	}
	state.Failures++
	state.Last = now
	if state.Failures >= memberBreakerThreshold {
		window := breakerBaseWindow << (state.Failures - memberBreakerThreshold)
		if window > breakerMaxWindow || window <= 0 {
			window = breakerMaxWindow
		}
		state.Until = now.Add(window)
	}
	m.breakers[provider] = state
}

// recordClientError records a refused request. It is not the member's fault,
// so it neither widens a breaker nor counts as a member failure.
func (m *memberStats) recordClientError(reason string) {
	m.LastOutcome = "rejected"
	m.LastError = reason
}

func (m *memberStats) recordCanceled() {
	m.LastOutcome = "canceled"
}

// blocked reports whether the provider's breaker keeps this member out of
// fresh decisions.
func (m *memberStats) blocked(provider string, now time.Time) bool {
	state, ok := m.breakers[providerKeyOf(provider)]
	return ok && !state.Until.IsZero() && now.Before(state.Until)
}

// breakerRemaining reports how long the provider's breaker still holds.
func (m *memberStats) breakerRemaining(provider string, now time.Time) time.Duration {
	state, ok := m.breakers[providerKeyOf(provider)]
	if !ok || state.Until.IsZero() || !now.Before(state.Until) {
		return 0
	}
	return state.Until.Sub(now)
}

// failedRecently reports whether the member failed within one breaker window,
// which is what the per-group "move the session when that member fails"
// setting reacts to.
func (m *memberStats) failedRecently(now time.Time) bool {
	return !m.lastFailure.IsZero() && now.Sub(m.lastFailure) < breakerBaseWindow
}

// requestsSince counts decisions inside the rolling window.
func (m *memberStats) requestsSince(now time.Time) int {
	if len(m.minutes) == 0 {
		return 0
	}
	oldest := now.Add(-time.Duration(windowMinutes)*time.Minute).Unix() / 60
	total := 0
	for minute, count := range m.minutes {
		if minute >= oldest {
			total += count
		}
	}
	return total
}

func (m *memberStats) prune(now time.Time) {
	if len(m.minutes) == 0 {
		return
	}
	oldest := now.Add(-time.Duration(windowMinutes)*time.Minute).Unix() / 60
	for minute := range m.minutes {
		if minute < oldest {
			delete(m.minutes, minute)
		}
	}
}

// failureStatus are the upstream outcomes that trip a member's breaker.
// Client-side errors (400, 404, 422) are excluded: they say nothing about the
// member's health.
func failureStatus(status int) bool {
	switch {
	case status == 401, status == 402, status == 403, status == 408, status == 409, status == 429:
		return true
	case status >= 500 && status <= 599:
		return true
	default:
		return false
	}
}
