package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// fakeQuotaHost answers the host callbacks the poller makes.
type fakeQuotaHost struct {
	mu       sync.Mutex
	auths    []pluginapi.HostAuthFileEntry
	files    map[string]string
	answers  map[string]pluginapi.HTTPResponse
	requests []hostHTTPRequest
	opened   int
}

func newFakeQuotaHost() *fakeQuotaHost {
	return &fakeQuotaHost{files: map[string]string{}, answers: map[string]pluginapi.HTTPResponse{}}
}

func (h *fakeQuotaHost) install(t *testing.T) {
	t.Helper()
	callHost = h.call
	t.Cleanup(func() { callHost = nil })
}

func (h *fakeQuotaHost) call(method string, payload []byte) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch method {
	case pluginabi.MethodHostAuthList:
		return okEnvelope(hostAuthListResponse{Files: h.auths})
	case pluginabi.MethodHostAuthGet:
		var req hostAuthIndexRequest
		_ = json.Unmarshal(payload, &req)
		file, ok := h.files[req.AuthIndex]
		if !ok {
			return errorEnvelope("not_found", "no auth "+req.AuthIndex), nil
		}
		return okEnvelope(hostAuthGetResponse{JSON: json.RawMessage(file)})
	case pluginabi.MethodHostHTTPOperationOpen:
		h.opened++
		return okEnvelope(hostOperation{OperationID: fmt.Sprintf("op-%d", h.opened)})
	case pluginabi.MethodHostHTTPCancel, pluginabi.MethodHostLog:
		return okEnvelope(struct{}{})
	case pluginabi.MethodHostHTTPDo:
		var req hostHTTPRequest
		_ = json.Unmarshal(payload, &req)
		h.requests = append(h.requests, req)
		answer, ok := h.answers[req.URL]
		if !ok {
			return errorEnvelope("http_error", "connection refused"), nil
		}
		return okEnvelope(answer)
	}
	return errorEnvelope("unknown_method", method), nil
}

func (h *fakeQuotaHost) answer(url string, status int, body string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answers[url] = pluginapi.HTTPResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
}

func (h *fakeQuotaHost) requestsTo(url string) []hostHTTPRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []hostHTTPRequest
	for _, req := range h.requests {
		if req.URL == url {
			out = append(out, req)
		}
	}
	return out
}

const metaUsageBody = `{
  "api_key": "sk-must-never-be-stored",
  "is_subs_active": true,
  "subs_tier_name": "Muse Code Everyday Usage",
  "subs_usage": {
    "window": {"used_percent": 2, "window_duration_mins": 300, "resets_at": 1789678120},
    "weekly": {"used_percent": 25, "resets_at": 1789948800},
    "tier": "fixture-tier"
  }
}`

func TestMetaQuotaFromUsage(t *testing.T) {
	want := []quotaWindow{
		{ID: "5h", Remaining: 98, ResetAt: 1789678120000, PeriodMinutes: 300},
		{ID: "7d", Remaining: 75, ResetAt: 1789948800000, PeriodMinutes: 7 * 24 * 60},
	}
	if got := metaQuotaFromUsage([]byte(metaUsageBody)); !reflect.DeepEqual(got, want) {
		t.Fatalf("windows = %+v, want %+v", got, want)
	}
	// Before an account's first request the reply has no subs_usage at all;
	// that is an unknown quota, not a full one.
	if got := metaQuotaFromUsage([]byte(`{"is_subs_active":true}`)); len(got) != 0 {
		t.Fatalf("windows without subs_usage = %+v", got)
	}
}

const xaiWeeklyBody = `{"config":{
  "creditUsagePercent": 40,
  "currentPeriod": {"type": "weekly", "start": "2026-10-20T00:00:00Z", "end": "2026-10-27T00:00:00Z"}
}}`

const xaiMonthlyBody = `{"config":{
  "monthlyLimit": {"val": 10000},
  "used": 2500,
  "billingPeriodStart": "2026-10-01T00:00:00Z",
  "billingPeriodEnd": "2026-11-01T00:00:00Z"
}}`

func TestXAIQuotaFromBilling(t *testing.T) {
	weeklyEnd := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	monthlyEnd := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	want := []quotaWindow{
		{ID: "7d", Remaining: 60, ResetAt: weeklyEnd.UnixMilli(), PeriodMinutes: 7 * 24 * 60},
		{ID: "30d", Remaining: 75, ResetAt: monthlyEnd.UnixMilli(), PeriodMinutes: 31 * 24 * 60},
	}
	if got := xaiQuotaFromBilling([]byte(xaiWeeklyBody), []byte(xaiMonthlyBody)); !reflect.DeepEqual(got, want) {
		t.Fatalf("windows = %+v, want %+v", got, want)
	}
	// Use past the included limit is on-demand spend; the included window
	// reads empty, never negative.
	over := xaiQuotaFromBilling(nil, []byte(`{"config":{"monthly_limit":"1000","used":1400}}`))
	if len(over) != 1 || over[0].Remaining != 0 {
		t.Fatalf("over-limit windows = %+v", over)
	}
	if got := xaiQuotaFromBilling([]byte(`{"config":{}}`), nil); len(got) != 0 {
		t.Fatalf("empty config windows = %+v", got)
	}
}

const openCodeGoUsageBody = `{"usage":{
  "rolling": {"status": "ok", "percent": 10, "resetsAt": "2026-10-13T04:56:11.000Z"},
  "weekly": {"status": "ok", "percent": 37.5, "resetsAt": "2026-10-19T00:00:00.000Z"},
  "monthly": {"status": "ok", "percent": 12, "resetsAt": "2026-11-12T23:27:39.000Z"}
}}`

func TestOpenCodeGoQuotaFromUsage(t *testing.T) {
	milli := func(text string) int64 {
		parsed, errParse := time.Parse(time.RFC3339Nano, text)
		if errParse != nil {
			t.Fatal(errParse)
		}
		return parsed.UnixMilli()
	}
	want := []quotaWindow{
		{ID: "5h", Remaining: 90, ResetAt: milli("2026-10-13T04:56:11.000Z"), PeriodMinutes: 300},
		{ID: "7d", Remaining: 62.5, ResetAt: milli("2026-10-19T00:00:00.000Z"), PeriodMinutes: 7 * 24 * 60},
		{ID: "30d", Remaining: 88, ResetAt: milli("2026-11-12T23:27:39.000Z"), PeriodMinutes: 30 * 24 * 60},
	}
	if got := openCodeGoQuotaFromUsage([]byte(openCodeGoUsageBody)); !reflect.DeepEqual(got, want) {
		t.Fatalf("windows = %+v, want %+v", got, want)
	}
}

func TestPollRecordsEveryPolledProvider(t *testing.T) {
	host := newFakeQuotaHost()
	host.install(t)
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	host.auths = []pluginapi.HostAuthFileEntry{
		{ID: "meta-a", AuthIndex: "m1", Provider: "meta", Email: "meta@example.com"},
		{ID: "xai-a", AuthIndex: "x1", Provider: "xai", Email: "grok@example.com"},
		{ID: "keys#key-b", AuthIndex: "g1", Provider: "opencode-go", Label: "second key"},
		{ID: "keys#key-b#zen", AuthIndex: "g2", Provider: "opencode-go", Label: "second key · OpenCode Zen"},
		{ID: "login-a", AuthIndex: "g3", Provider: "opencode-go", Label: "workspace"},
		{ID: "claude-a", AuthIndex: "c1", Provider: "claude", Email: "claude@example.com"},
		{ID: "meta-off", AuthIndex: "m2", Provider: "meta", Disabled: true},
		{ID: "xai-proxied", AuthIndex: "x2", Provider: "xai"},
	}
	host.files["m1"] = `{"type":"meta","dca_token":"dca:meta-token"}`
	host.files["x1"] = `{"type":"xai","access_token":"xai-token","sub":"user-9"}`
	host.files["g1"] = `{"type":"opencode-go","keys":[{"id":"key-a","api_key":"oc_sk_first"},{"id":"key-b","api_key":"oc_sk_second"}]}`
	host.files["g3"] = `{"type":"opencode-go","auth_method":"oauth","access_token":"login-token","refresh_token":"refresh","org_id":"org-1"}`
	host.files["x2"] = `{"type":"xai","access_token":"proxied-token","proxy_url":"socks5://10.0.0.1:1080"}`
	host.answer(metaQuotaURL, 200, metaUsageBody)
	host.answer(xaiBillingWeeklyURL, 200, xaiWeeklyBody)
	host.answer(xaiBillingMonthlyURL, 200, xaiMonthlyBody)
	host.answer(openCodeGoUsageURL, 200, openCodeGoUsageBody)

	active := newStore(t.TempDir(), 30)
	active.now = fixedClock(now)
	active.tick()
	previous := current.Load()
	current.Store(active)
	t.Cleanup(func() {
		current.Store(previous)
		active.close()
	})

	poller := &quotaPoller{now: fixedClock(now)}
	poller.poll(context.Background())

	samples, _ := active.quotaSnapshot(0)
	got := make(map[string]quotaSample)
	for _, sample := range samples {
		got[sample.Account] = sample
	}
	if len(got) != 4 {
		t.Fatalf("samples = %+v, want meta, xai and two OpenCode Go accounts", samples)
	}
	if sample := got["m1"]; sample.Provider != "meta" || len(sample.Windows) != 2 || sample.At != now.UnixMilli() {
		t.Errorf("meta sample = %+v", sample)
	}
	if sample := got["x1"]; sample.Provider != "xai" || len(sample.Windows) != 2 {
		t.Errorf("xai sample = %+v", sample)
	}
	if sample := got["g1"]; sample.Provider != "opencode-go" || len(sample.Windows) != 3 {
		t.Errorf("opencode-go key sample = %+v", sample)
	}
	if sample := got["g3"]; sample.Provider != "opencode-go" || len(sample.Windows) != 3 {
		t.Errorf("opencode-go login sample = %+v", sample)
	}

	metaReqs := host.requestsTo(metaQuotaURL)
	if len(metaReqs) != 1 || metaReqs[0].Method != http.MethodPost ||
		metaReqs[0].Headers.Get("Authorization") != "Bearer dca:meta-token" ||
		metaReqs[0].Headers.Get("X-Api-Version") != "1.0.0" || string(metaReqs[0].Body) != "{}" {
		t.Errorf("meta requests = %+v", metaReqs)
	}
	weeklyReqs := host.requestsTo(xaiBillingWeeklyURL)
	if len(weeklyReqs) != 1 || weeklyReqs[0].Headers.Get("Authorization") != "Bearer xai-token" ||
		weeklyReqs[0].Headers.Get("X-Userid") != "user-9" || weeklyReqs[0].Headers.Get("X-Xai-Token-Auth") != "xai-grok-cli" {
		t.Errorf("xai weekly requests = %+v", weeklyReqs)
	}
	goReqs := host.requestsTo(openCodeGoUsageURL)
	var bearers []string
	for _, req := range goReqs {
		bearers = append(bearers, req.Headers.Get("Authorization"))
		if req.OperationID == "" {
			t.Error("poll request carries no operation, so it cannot be cancelled")
		}
	}
	// The key the auth names (key-b), then the login's token. The Zen auth
	// of the same key is not polled.
	if !reflect.DeepEqual(bearers, []string{"Bearer oc_sk_second", "Bearer login-token"}) {
		t.Errorf("opencode-go bearers = %v", bearers)
	}
	// Nothing read from an auth file or a usage endpoint is stored.
	for _, sample := range samples {
		raw, _ := json.Marshal(sample)
		for _, secret := range []string{"dca:meta-token", "xai-token", "oc_sk_second", "login-token", "sk-must-never-be-stored"} {
			if strings.Contains(string(raw), secret) {
				t.Errorf("sample %s holds %q", raw, secret)
			}
		}
	}
}

func TestPollIfDueHonoursIntervalAndOff(t *testing.T) {
	host := newFakeQuotaHost()
	host.install(t)
	host.auths = []pluginapi.HostAuthFileEntry{{ID: "meta-a", AuthIndex: "m1", Provider: "meta"}}
	host.files["m1"] = `{"type":"meta","dca_token":"dca:tok"}`
	host.answer(metaQuotaURL, 200, metaUsageBody)
	active := newStore(t.TempDir(), 30)
	previous := current.Load()
	current.Store(active)
	t.Cleanup(func() {
		current.Store(previous)
		active.close()
	})
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	poller := &quotaPoller{now: fixedClock(now)}

	cfg := settings{QuotaPollInterval: 15 * time.Minute}
	currentConfig.Store(&cfg)
	t.Cleanup(func() { currentConfig.Store(nil) })
	poller.pollIfDue(context.Background())
	poller.pollIfDue(context.Background())
	if got := len(host.requestsTo(metaQuotaURL)); got != 1 {
		t.Fatalf("polls within one interval sent %d requests", got)
	}

	off := settings{QuotaPollInterval: 0}
	currentConfig.Store(&off)
	fresh := &quotaPoller{now: fixedClock(now)}
	fresh.pollIfDue(context.Background())
	if got := len(host.requestsTo(metaQuotaURL)); got != 1 {
		t.Fatalf("polling off sent %d more requests", got-1)
	}
}

func TestParseQuotaPollIntervalSetting(t *testing.T) {
	cfg, errParse := parseSettings([]byte("quota_poll_interval: off\n"))
	if errParse != nil || cfg.QuotaPollInterval != 0 {
		t.Fatalf("off: cfg = %+v, err = %v", cfg, errParse)
	}
	cfg, errParse = parseSettings([]byte("quota_poll_interval: 5m\n"))
	if errParse != nil || cfg.QuotaPollInterval != 5*time.Minute {
		t.Fatalf("5m: cfg = %+v, err = %v", cfg, errParse)
	}
	cfg, errParse = parseSettings(nil)
	if errParse != nil || cfg.QuotaPollInterval != defaultQuotaPollInterval {
		t.Fatalf("default: cfg = %+v, err = %v", cfg, errParse)
	}
	if _, errParse := parseSettings([]byte("quota_poll_interval: soon\n")); errParse == nil {
		t.Fatal("an unreadable interval must be rejected")
	}
}
