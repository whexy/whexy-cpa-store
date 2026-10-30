package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Grok, Meta and OpenCode Go send no subscription quota in response
// headers, so theirs is read from the same usage endpoints Management
// Center's quota page reads, with the credential in each account's auth
// file. Nothing read from an auth file is kept past the poll that read it,
// and a response body is never logged: Meta's carries the account's API
// key and profile alongside the usage.
const (
	defaultQuotaPollInterval = 15 * time.Minute
	minQuotaPollInterval     = time.Minute
	quotaPollCheckTick       = time.Minute

	metaQuotaURL          = "https://api.meta.ai/muse-code/key"
	xaiBillingWeeklyURL   = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	xaiBillingMonthlyURL  = "https://cli-chat-proxy.grok.com/v1/billing"
	openCodeGoUsageURL    = "https://opencode.ai/inference/go/v1/usage"
	xaiGrokClientVersion  = "1.0.46"
	metaWindowFallbackMin = 5 * 60
)

// pollProviders are the providers read from their usage endpoints.
var pollProviders = map[string]func(context.Context, pluginapi.HostAuthFileEntry) []quotaWindow{
	"meta":        pollMeta,
	"xai":         pollXAI,
	"opencode-go": pollOpenCodeGo,
}

// quotaPoller reads every polled provider's accounts once per interval.
type quotaPoller struct {
	mu   sync.Mutex
	next time.Time
	now  func() time.Time
}

var quotaPolls = &quotaPoller{now: time.Now}

func (p *quotaPoller) run(ctx context.Context) {
	p.pollIfDue(ctx)
	ticker := time.NewTicker(quotaPollCheckTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pollIfDue(ctx)
		}
	}
}

func (p *quotaPoller) pollIfDue(ctx context.Context) {
	interval := currentSettings().QuotaPollInterval
	if interval <= 0 {
		return
	}
	p.mu.Lock()
	if p.now().Before(p.next) {
		p.mu.Unlock()
		return
	}
	// Claim the slot so a slow round is not started twice.
	p.next = p.now().Add(interval)
	p.mu.Unlock()
	p.poll(ctx)
}

func (p *quotaPoller) poll(ctx context.Context) {
	active := activeStore()
	if active == nil {
		return
	}
	entries, errList := hostAuthList()
	if errList != nil {
		return
	}
	now := p.now()
	for _, entry := range entries {
		if entry.Disabled {
			continue
		}
		account := strings.TrimSpace(entry.AuthIndex)
		provider := accountInfoFromEntry(entry).Provider
		fetch := pollProviders[provider]
		if account == "" || fetch == nil {
			continue
		}
		windows := fetch(ctx, entry)
		if len(windows) == 0 {
			continue
		}
		active.addQuota(quotaSample{At: now.UnixMilli(), Account: account, Provider: provider, Windows: windows})
	}
}

// pollAuthFile reads one account's auth file. It refuses an account with
// its own proxy or upstream: host.http.do applies only the global proxy,
// so the poll would reach the provider from somewhere CLIProxyAPI never
// sends that account's traffic.
func pollAuthFile(entry pluginapi.HostAuthFileEntry) (json.RawMessage, bool) {
	raw, errFile := hostAuthFile(strings.TrimSpace(entry.AuthIndex))
	if errFile != nil || len(raw) == 0 {
		return nil, false
	}
	var route struct {
		ProxyURL string `json:"proxy_url"`
		BaseURL  string `json:"base_url"`
	}
	if json.Unmarshal(raw, &route) != nil || strings.TrimSpace(route.ProxyURL) != "" || strings.TrimSpace(route.BaseURL) != "" {
		return nil, false
	}
	return raw, true
}

func pollMeta(ctx context.Context, entry pluginapi.HostAuthFileEntry) []quotaWindow {
	raw, ok := pollAuthFile(entry)
	if !ok {
		return nil
	}
	var file struct {
		DCAToken string `json:"dca_token"`
	}
	if json.Unmarshal(raw, &file) != nil {
		return nil
	}
	token := strings.TrimSpace(file.DCAToken)
	if !strings.HasPrefix(token, "dca:") {
		return nil
	}
	resp, errPost := hostDo(ctx, http.MethodPost, metaQuotaURL, http.Header{
		"Accept":        {"application/json"},
		"Content-Type":  {"application/json"},
		"Authorization": {"Bearer " + token},
		"X-Api-Version": {"1.0.0"},
	}, []byte("{}"))
	if errPost != nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	return metaQuotaFromUsage(resp.Body)
}

// metaQuotaFromUsage reads subs_usage only; the reply also carries the
// account's api_key and profile, which are never touched.
func metaQuotaFromUsage(body []byte) []quotaWindow {
	type usageWindow struct {
		UsedPercent  *flexFloat `json:"used_percent"`
		DurationMins *flexFloat `json:"window_duration_mins"`
		ResetsAt     *flexFloat `json:"resets_at"`
	}
	var doc struct {
		SubsUsage *struct {
			Window *usageWindow `json:"window"`
			Weekly *usageWindow `json:"weekly"`
		} `json:"subs_usage"`
	}
	if json.Unmarshal(body, &doc) != nil || doc.SubsUsage == nil {
		return nil
	}
	var windows []quotaWindow
	for _, slot := range []struct {
		raw     *usageWindow
		minutes int
	}{{doc.SubsUsage.Window, metaWindowFallbackMin}, {doc.SubsUsage.Weekly, 7 * 24 * 60}} {
		if slot.raw == nil || slot.raw.UsedPercent == nil {
			continue
		}
		minutes := slot.minutes
		if slot.raw.DurationMins != nil && *slot.raw.DurationMins >= 1 {
			minutes = int(*slot.raw.DurationMins)
		}
		window := quotaWindow{
			ID:            windowID(minutes),
			Remaining:     remainingPercent(100 - float64(*slot.raw.UsedPercent)),
			PeriodMinutes: minutes,
		}
		if slot.raw.ResetsAt != nil && *slot.raw.ResetsAt > 0 {
			window.ResetAt = int64(*slot.raw.ResetsAt * 1000)
		}
		windows = append(windows, window)
	}
	return windows
}

func pollXAI(ctx context.Context, entry pluginapi.HostAuthFileEntry) []quotaWindow {
	raw, ok := pollAuthFile(entry)
	if !ok {
		return nil
	}
	var file struct {
		AccessToken string `json:"access_token"`
		Subject     string `json:"sub"`
	}
	if json.Unmarshal(raw, &file) != nil || strings.TrimSpace(file.AccessToken) == "" {
		return nil
	}
	headers := http.Header{
		"Authorization":         {"Bearer " + strings.TrimSpace(file.AccessToken)},
		"X-Xai-Token-Auth":      {"xai-grok-cli"},
		"X-Grok-Client-Version": {xaiGrokClientVersion},
		"Accept":                {"*/*"},
		"User-Agent":            {"grok-pager/" + xaiGrokClientVersion + " grok-shell/" + xaiGrokClientVersion + " (macos; aarch64)"},
	}
	if subject := strings.TrimSpace(file.Subject); subject != "" {
		headers.Set("X-Userid", subject)
	}
	get := func(url string) []byte {
		resp, errGet := hostDo(ctx, http.MethodGet, url, headers, nil)
		if errGet != nil || resp.StatusCode != http.StatusOK {
			return nil
		}
		return resp.Body
	}
	return xaiQuotaFromBilling(get(xaiBillingWeeklyURL), get(xaiBillingMonthlyURL))
}

// xaiQuotaFromBilling reads Grok's weekly credit use and monthly included
// limit. The two endpoints return one shared config shape; each window is
// read from its own endpoint, and either may be missing (an API account
// has no subscription billing and yields nothing).
func xaiQuotaFromBilling(weeklyBody, monthlyBody []byte) []quotaWindow {
	var windows []quotaWindow
	if config, ok := xaiBillingConfig(weeklyBody); ok && config.creditUsedPercent != nil {
		window := quotaWindow{
			ID:            "7d",
			Remaining:     remainingPercent(100 - *config.creditUsedPercent),
			PeriodMinutes: periodMinutes(config.periodStart, config.periodEnd, 7*24*60),
		}
		if !config.periodEnd.IsZero() {
			window.ResetAt = config.periodEnd.UnixMilli()
		}
		windows = append(windows, window)
	}
	if config, ok := xaiBillingConfig(monthlyBody); ok && config.monthlyLimit != nil && config.used != nil && *config.monthlyLimit > 0 {
		included := min(*config.used, *config.monthlyLimit)
		window := quotaWindow{
			ID:            "30d",
			Remaining:     remainingPercent(100 * (1 - included / *config.monthlyLimit)),
			PeriodMinutes: periodMinutes(config.billingStart, config.billingEnd, 30*24*60),
		}
		if !config.billingEnd.IsZero() {
			window.ResetAt = config.billingEnd.UnixMilli()
		}
		windows = append(windows, window)
	}
	return windows
}

type xaiConfig struct {
	creditUsedPercent *float64
	monthlyLimit      *float64
	used              *float64
	periodStart       time.Time
	periodEnd         time.Time
	billingStart      time.Time
	billingEnd        time.Time
}

// xaiBillingConfig accepts the endpoint's camelCase and snake_case keys,
// and cent amounts as a number, a string or {"val": ...}.
func xaiBillingConfig(body []byte) (xaiConfig, bool) {
	if len(body) == 0 {
		return xaiConfig{}, false
	}
	var doc struct {
		Config *struct {
			CreditUsagePercent  *flexFloat      `json:"creditUsagePercent"`
			CreditUsagePercentS *flexFloat      `json:"credit_usage_percent"`
			CurrentPeriod       json.RawMessage `json:"currentPeriod"`
			CurrentPeriodS      json.RawMessage `json:"current_period"`
			MonthlyLimit        *flexCents      `json:"monthlyLimit"`
			MonthlyLimitS       *flexCents      `json:"monthly_limit"`
			Used                *flexCents      `json:"used"`
			BillingStart        *flexInstant    `json:"billingPeriodStart"`
			BillingStartS       *flexInstant    `json:"billing_period_start"`
			BillingEnd          *flexInstant    `json:"billingPeriodEnd"`
			BillingEndS         *flexInstant    `json:"billing_period_end"`
		} `json:"config"`
	}
	if json.Unmarshal(body, &doc) != nil || doc.Config == nil {
		return xaiConfig{}, false
	}
	raw := doc.Config
	config := xaiConfig{
		creditUsedPercent: flexFloatValue(raw.CreditUsagePercent, raw.CreditUsagePercentS),
		monthlyLimit:      flexCentsValue(raw.MonthlyLimit, raw.MonthlyLimitS),
		used:              flexCentsValue(raw.Used),
		billingStart:      flexInstantValue(raw.BillingStart, raw.BillingStartS),
		billingEnd:        flexInstantValue(raw.BillingEnd, raw.BillingEndS),
	}
	period := raw.CurrentPeriod
	if len(period) == 0 {
		period = raw.CurrentPeriodS
	}
	var span struct {
		Start *flexInstant `json:"start"`
		End   *flexInstant `json:"end"`
	}
	if json.Unmarshal(period, &span) == nil {
		config.periodStart = flexInstantValue(span.Start)
		config.periodEnd = flexInstantValue(span.End)
	}
	return config, true
}

func periodMinutes(start, end time.Time, fallback int) int {
	if !start.IsZero() && end.After(start) {
		if minutes := int(end.Sub(start).Minutes() + 0.5); minutes >= 1 {
			return minutes
		}
	}
	return fallback
}

func pollOpenCodeGo(ctx context.Context, entry pluginapi.HostAuthFileEntry) []quotaWindow {
	// A key or login that also serves OpenCode Zen has a second auth for
	// it; Zen is pay-as-you-go and has no windows.
	if strings.HasSuffix(strings.TrimSpace(entry.ID), "#zen") {
		return nil
	}
	raw, ok := pollAuthFile(entry)
	if !ok {
		return nil
	}
	bearer := openCodeGoBearer(entry, raw)
	if bearer == "" {
		return nil
	}
	resp, errGet := hostDo(ctx, http.MethodGet, openCodeGoUsageURL, http.Header{
		"Authorization": {"Bearer " + bearer},
		"Accept":        {"application/json"},
		"User-Agent":    {"whexy-cpa-usage-dashboard/" + pluginVersion},
	}, nil)
	if errGet != nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	return openCodeGoQuotaFromUsage(resp.Body)
}

// openCodeGoBearer is the credential one opencode-go auth sends upstream:
// a login's access token, or the API key the auth's ID names in its file
// ("<file>#<key id>"). Only credentials serving OpenCode Go have quota.
func openCodeGoBearer(entry pluginapi.HostAuthFileEntry, raw json.RawMessage) string {
	type key struct {
		ID       string `json:"id"`
		APIKey   string `json:"api_key"`
		Disabled bool   `json:"disabled"`
		Go       *bool  `json:"go"`
	}
	var file struct {
		AuthMethod   string `json:"auth_method"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		APIKey       string `json:"api_key"`
		Go           *bool  `json:"go"`
		Keys         []key  `json:"keys"`
	}
	if json.Unmarshal(raw, &file) != nil {
		return ""
	}
	servesGo := func(goFlag *bool, disabled bool) bool {
		return !disabled && (goFlag == nil || *goFlag)
	}
	if strings.EqualFold(strings.TrimSpace(file.AuthMethod), "oauth") || strings.TrimSpace(file.RefreshToken) != "" {
		if !servesGo(file.Go, false) {
			return ""
		}
		return strings.TrimSpace(file.AccessToken)
	}
	keyID := ""
	if at := strings.LastIndex(entry.ID, "#"); at >= 0 {
		keyID = strings.TrimSpace(entry.ID[at+1:])
	}
	for _, candidate := range file.Keys {
		if keyID != "" && strings.TrimSpace(candidate.ID) == keyID && servesGo(candidate.Go, candidate.Disabled) {
			return strings.TrimSpace(candidate.APIKey)
		}
	}
	if keyID == "" && len(file.Keys) == 1 && servesGo(file.Keys[0].Go, file.Keys[0].Disabled) {
		return strings.TrimSpace(file.Keys[0].APIKey)
	}
	if len(file.Keys) == 0 && servesGo(file.Go, false) {
		return strings.TrimSpace(file.APIKey)
	}
	return ""
}

// openCodeGoQuotaFromUsage reads the Go gateway's /v1/usage windows. The
// gateway names no window lengths, so each keeps the length its name
// states on the subscription page.
func openCodeGoQuotaFromUsage(body []byte) []quotaWindow {
	var doc struct {
		Usage map[string]struct {
			Percent  *flexFloat `json:"percent"`
			ResetsAt string     `json:"resetsAt"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return nil
	}
	var windows []quotaWindow
	for _, slot := range []struct {
		name    string
		id      string
		minutes int
	}{{"rolling", "5h", 5 * 60}, {"weekly", "7d", 7 * 24 * 60}, {"monthly", "30d", 30 * 24 * 60}} {
		raw, ok := doc.Usage[slot.name]
		if !ok || raw.Percent == nil {
			continue
		}
		window := quotaWindow{
			ID:            slot.id,
			Remaining:     remainingPercent(100 - float64(*raw.Percent)),
			PeriodMinutes: slot.minutes,
		}
		if resetsAt, errParse := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw.ResetsAt)); errParse == nil {
			window.ResetAt = resetsAt.UnixMilli()
		}
		windows = append(windows, window)
	}
	return windows
}

// flexFloat is a number an endpoint may send as a JSON number or a string.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(raw []byte) error {
	value, errParse := strconv.ParseFloat(strings.Trim(strings.TrimSpace(string(raw)), `"`), 64)
	if errParse != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return strconv.ErrSyntax
	}
	*f = flexFloat(value)
	return nil
}

func flexFloatValue(values ...*flexFloat) *float64 {
	for _, value := range values {
		if value != nil {
			plain := float64(*value)
			return &plain
		}
	}
	return nil
}

// flexCents is a cent amount: a flexFloat, or {"val": ...}.
type flexCents float64

func (f *flexCents) UnmarshalJSON(raw []byte) error {
	var plain flexFloat
	if plain.UnmarshalJSON(raw) == nil {
		*f = flexCents(plain)
		return nil
	}
	var wrapped struct {
		Val *flexFloat `json:"val"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && wrapped.Val != nil {
		*f = flexCents(*wrapped.Val)
		return nil
	}
	return strconv.ErrSyntax
}

func flexCentsValue(values ...*flexCents) *float64 {
	for _, value := range values {
		if value != nil {
			plain := float64(*value)
			return &plain
		}
	}
	return nil
}

// flexInstant is a time sent as RFC 3339 or as Unix seconds.
type flexInstant time.Time

func (f *flexInstant) UnmarshalJSON(raw []byte) error {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if parsed, errParse := time.Parse(time.RFC3339Nano, text); errParse == nil {
		*f = flexInstant(parsed)
		return nil
	}
	if seconds, errParse := strconv.ParseFloat(text, 64); errParse == nil && seconds > 0 {
		*f = flexInstant(time.UnixMilli(int64(seconds * 1000)).UTC())
		return nil
	}
	return strconv.ErrSyntax
}

func flexInstantValue(values ...*flexInstant) time.Time {
	for _, value := range values {
		if value != nil {
			return time.Time(*value)
		}
	}
	return time.Time{}
}
