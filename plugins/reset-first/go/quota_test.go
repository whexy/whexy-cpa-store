package main

import (
	"net/http"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func TestCodexQuotaFromHeaders(t *testing.T) {
	// A plan whose weekly window comes in the primary slot: windows are told
	// apart by length, and the keys arrive in any case.
	headers := http.Header{
		"x-codex-primary-used-percent":           {"41"},
		"x-codex-primary-window-minutes":         {"10080"},
		"x-codex-primary-reset-at":               {"1761912000"},
		"X-Codex-Secondary-Used-Percent":         {"100"},
		"X-Codex-Secondary-Window-Minutes":       {"300"},
		"X-Codex-Secondary-Reset-After-Seconds":  {"5400"},
		"X-Codex-Bengalfox-Primary-Used-Percent": {"3"},
	}
	q := codexQuotaFromHeaders(headers, t0)
	if q == nil || len(q.Windows) != 2 {
		t.Fatalf("quota = %+v, want two windows", q)
	}
	five, week := q.Windows[0], q.Windows[1]
	if five.Minutes != 300 || five.Used != 100 || !five.ResetAt.Equal(t0.Add(90*time.Minute)) {
		t.Errorf("5h window = %+v", five)
	}
	if week.Minutes != 10080 || week.Used != 41 || !week.ResetAt.Equal(time.Unix(1761912000, 0)) {
		t.Errorf("weekly window = %+v", week)
	}
	if codexQuotaFromHeaders(http.Header{"Content-Type": {"text/event-stream"}}, t0) != nil {
		t.Error("headers without quota gave a reading")
	}
}

func TestClaudeQuotaFromHeaders(t *testing.T) {
	headers := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.25"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {"1761487200"},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.93"},
		"Anthropic-Ratelimit-Unified-7d-Status":      {"rejected"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {"1761800000"},
	}
	q := claudeQuotaFromHeaders(headers, t0)
	if q == nil || len(q.Windows) != 2 {
		t.Fatalf("quota = %+v, want two windows", q)
	}
	if w := q.Windows[0]; w.Minutes != 300 || w.Used != 25 || !w.ResetAt.Equal(time.Unix(1761487200, 0)) {
		t.Errorf("5h window = %+v", w)
	}
	if w := q.Windows[1]; w.Minutes != 10080 || w.Used != 100 {
		t.Errorf("a rejected weekly window = %+v, want it spent", w)
	}
}

func TestClampPercentLeavesSomeBelow100(t *testing.T) {
	for in, want := range map[float64]float64{29: 29, 99.96: 99.9, 99.94: 99.9, 100: 100, 130: 100, -4: 0, 12.46: 12.5} {
		if got := clampPercent(in); got != want {
			t.Errorf("clampPercent(%v) = %v, want %v", in, got, want)
		}
	}
	// A Claude window at 99.96% still has room, so the account is not spent.
	q := claudeQuotaFromHeaders(http.Header{"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.9996"}}, t0)
	if st := evaluate(q, t0); st.Kind != standingSpare {
		t.Errorf("standing = %+v, want spare", st)
	}
}

func TestCodexQuotaFromUsage(t *testing.T) {
	body := []byte(`{
		"plan_type": "plus",
		"rate_limit": {
			"allowed": true,
			"primary_window": {"used_percent": 12, "limit_window_seconds": 18000, "reset_after_seconds": 3600},
			"secondary_window": {"used_percent": 60.5, "limit_window_seconds": 604800, "reset_after_seconds": 1, "reset_at": 1761900000}
		}
	}`)
	q, plan, ok := codexQuotaFromUsage(body, t0)
	if !ok || plan != "plus" || len(q.Windows) != 2 {
		t.Fatalf("quota = %+v, plan %q, ok %v", q, plan, ok)
	}
	if w := q.Windows[0]; w.Minutes != 300 || w.Used != 12 || !w.ResetAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("5h window = %+v", w)
	}
	// reset_at wins over reset_after_seconds, which drifts with the response.
	if w := q.Windows[1]; w.Minutes != 10080 || w.Used != 60.5 || !w.ResetAt.Equal(time.Unix(1761900000, 0)) {
		t.Errorf("weekly window = %+v", w)
	}
	if _, _, ok := codexQuotaFromUsage([]byte(`{"detail":"Unauthorized"}`), t0); ok {
		t.Error("an answer without rate_limit gave a reading")
	}
}

func TestClaudeQuotaFromUsage(t *testing.T) {
	body := []byte(`{
		"five_hour": {"utilization": 37.0, "resets_at": "2026-10-26T15:00:00.000000+00:00"},
		"seven_day": {"utilization": 8.0, "resets_at": "2026-10-30T09:00:00+00:00"},
		"seven_day_opus": null
	}`)
	q, ok := claudeQuotaFromUsage(body, t0)
	if !ok || len(q.Windows) != 2 {
		t.Fatalf("quota = %+v, ok %v", q, ok)
	}
	if w := q.Windows[0]; w.Used != 37 || !w.ResetAt.Equal(t0.Add(3*time.Hour)) {
		t.Errorf("5h window = %+v", w)
	}
	if w := q.Windows[1]; w.Minutes != 10080 || w.Used != 8 {
		t.Errorf("weekly window = %+v", w)
	}
}

func TestEvaluate(t *testing.T) {
	reading := func(windows ...window) *quota { return &quota{Windows: windows, At: t0.Add(-time.Minute)} }

	if st := evaluate(nil, t0); st.Kind != standingUnknown {
		t.Errorf("no reading: %+v", st)
	}

	spare := evaluate(reading(
		window{Minutes: 300, Used: 40, ResetAt: t0.Add(2 * time.Hour)},
		window{Minutes: 10080, Used: 70, ResetAt: t0.Add(50 * time.Hour)},
	), t0)
	if spare.Kind != standingSpare || spare.Decides != 10080 || !spare.ResetAt.Equal(t0.Add(50*time.Hour)) {
		t.Errorf("spare: %+v, want the weekly window to decide", spare)
	}

	spent := evaluate(reading(
		window{Minutes: 300, Used: 100, ResetAt: t0.Add(2 * time.Hour)},
		window{Minutes: 10080, Used: 70, ResetAt: t0.Add(50 * time.Hour)},
	), t0)
	if spent.Kind != standingSpent || !spent.BackAt.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("spent 5h: %+v, want back when the 5h window resets", spent)
	}

	renewed := evaluate(reading(
		window{Minutes: 300, Used: 100, ResetAt: t0.Add(-time.Minute)},
		window{Minutes: 10080, Used: 70, ResetAt: t0.Add(50 * time.Hour)},
	), t0)
	if renewed.Kind != standingSpare || !renewed.Windows[0].Renewed || renewed.Windows[0].Used != 0 {
		t.Errorf("a passed reset: %+v, want the window renewed", renewed)
	}

	weekRenewed := evaluate(reading(window{Minutes: 10080, Used: 90, ResetAt: t0.Add(-time.Hour)}), t0)
	if !weekRenewed.ResetAt.Equal(t0.Add(7 * 24 * time.Hour)) {
		t.Errorf("a renewed deciding window resets at %v, want one window length away", weekRenewed.ResetAt)
	}

	unknownBack := evaluate(reading(window{Minutes: 300, Used: 100}), t0)
	if unknownBack.Kind != standingSpent || !unknownBack.BackAt.IsZero() {
		t.Errorf("spent without a reset: %+v", unknownBack)
	}
}
