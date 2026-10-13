package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// usageWindow is one quota window of GET {base}/v1/usage.
type usageWindow struct {
	// Name is the upstream key: rolling (the 5-hour window), weekly or monthly.
	Name    string
	Status  string
	Percent float64
	// ResetsAt is zero when the gateway omitted it.
	ResetsAt time.Time
}

// usageReport holds the windows in the order rolling, weekly, monthly;
// windows the gateway omits are absent.
type usageReport struct {
	Windows []usageWindow
}

var usageWindowNames = []string{"rolling", "weekly", "monthly"}

func usageURL(cfg settings) string {
	return strings.TrimRight(cfg.BaseURL, "/") + "/v1/usage"
}

// fetchUsage reads the key's subscription usage. The call is free and needs
// no session header.
func fetchUsage(ctx context.Context, doer httpDoer, cfg settings, apiKey string) (usageReport, error) {
	resp, errDo := doer.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     usageURL(cfg),
		Headers: goRequestHeaders(cfg, apiKey),
	})
	if errDo != nil {
		return usageReport{}, fmt.Errorf("fetch OpenCode Go usage: %w", errDo)
	}
	if resp.StatusCode != http.StatusOK {
		return usageReport{}, parseUpstreamError(resp.StatusCode, resp.Headers, resp.Body)
	}
	return parseUsage(resp.Body)
}

func parseUsage(raw []byte) (usageReport, error) {
	var doc struct {
		Usage map[string]struct {
			Status   string  `json:"status"`
			Percent  float64 `json:"percent"`
			ResetsAt string  `json:"resetsAt"`
		} `json:"usage"`
	}
	if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil {
		return usageReport{}, fmt.Errorf("decode OpenCode Go usage: %w", errUnmarshal)
	}
	if len(doc.Usage) == 0 {
		return usageReport{}, fmt.Errorf("decode OpenCode Go usage: no usage windows")
	}
	var report usageReport
	for _, name := range usageWindowNames {
		window, ok := doc.Usage[name]
		if !ok {
			continue
		}
		parsed := usageWindow{Name: name, Status: strings.TrimSpace(window.Status), Percent: window.Percent}
		if window.ResetsAt != "" {
			if resetsAt, errParse := time.Parse(time.RFC3339Nano, window.ResetsAt); errParse == nil {
				parsed.ResetsAt = resetsAt.UTC()
			}
		}
		report.Windows = append(report.Windows, parsed)
	}
	return report, nil
}

// isExhausted treats any status other than "ok" as a block; only "ok" has
// been observed, and OpenCode's client does not name the others.
func (w usageWindow) isExhausted() bool {
	return (w.Status != "" && !strings.EqualFold(w.Status, "ok")) || w.Percent >= 100
}

func (w usageWindow) remainingFraction() float64 {
	fraction := 1 - w.Percent/100
	switch {
	case fraction < 0:
		return 0
	case fraction > 1:
		return 1
	default:
		return fraction
	}
}

// exhausted reports the windows that block the key and the time the last of
// them resets; until is zero when a blocking window has no reset time.
func (r usageReport) exhausted() (until time.Time, windows []usageWindow, ok bool) {
	for _, window := range r.Windows {
		if !window.isExhausted() {
			continue
		}
		windows = append(windows, window)
		if window.ResetsAt.After(until) {
			until = window.ResetsAt
		}
	}
	return until, windows, len(windows) > 0
}
