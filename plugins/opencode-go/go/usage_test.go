package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestParseUsageFixture(t *testing.T) {
	report, errParse := parseUsage(readFixture(t, "usage.json"))
	if errParse != nil {
		t.Fatal(errParse)
	}
	if len(report.Windows) != 3 || report.Windows[0].Name != "rolling" || report.Windows[1].Name != "weekly" || report.Windows[2].Name != "monthly" {
		t.Fatalf("windows = %+v", report.Windows)
	}
	weekly := report.Windows[1]
	if weekly.Percent != 37.5 || weekly.remainingFraction() != 0.625 || !weekly.ResetsAt.Equal(time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("weekly = %+v", weekly)
	}
	if _, _, blocked := report.exhausted(); blocked {
		t.Fatal("fresh usage reported exhausted")
	}
	if _, errParse := parseUsage([]byte(`{"usage":{}}`)); errParse == nil {
		t.Fatal("empty usage accepted")
	}
}

func TestUsageExhaustion(t *testing.T) {
	rollingReset := time.Date(2026, 10, 13, 4, 0, 0, 0, time.UTC)
	weeklyReset := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	report := usageReport{Windows: []usageWindow{
		{Name: "rolling", Status: "ok", Percent: 100, ResetsAt: rollingReset},
		{Name: "weekly", Status: "exceeded", Percent: 80, ResetsAt: weeklyReset},
		{Name: "monthly", Status: "ok", Percent: 40},
	}}
	until, windows, blocked := report.exhausted()
	if !blocked || len(windows) != 2 || !until.Equal(weeklyReset) {
		t.Fatalf("exhausted = %v %+v %v", until, windows, blocked)
	}
	if got := (usageWindow{Percent: 140}).remainingFraction(); got != 0 {
		t.Fatalf("over-limit fraction = %v", got)
	}
	if got := (usageWindow{Percent: -5}).remainingFraction(); got != 1 {
		t.Fatalf("negative percent fraction = %v", got)
	}
}

func TestFetchUsageRequestAndErrors(t *testing.T) {
	status := 200
	doer := &fakeDoer{respond: func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if status != 200 {
			return jsonResponse(status, []byte(`{"type":"error","error":{"type":"AuthError","message":"Invalid key"}}`)), nil
		}
		return jsonResponse(200, readFixture(t, "usage.json")), nil
	}}
	cfg := settings{BaseURL: "https://opencode.ai/inference/go/"}
	if _, errFetch := fetchUsage(context.Background(), doer, cfg, testKeyA); errFetch != nil {
		t.Fatal(errFetch)
	}
	req := doer.requests[0]
	if req.URL != "https://opencode.ai/inference/go/v1/usage" || req.Headers.Get("Authorization") != "Bearer "+testKeyA || req.Headers.Get("x-opencode-session") != "" {
		t.Fatalf("request = %+v", req)
	}
	status = 401
	_, errFetch := fetchUsage(context.Background(), doer, cfg, testKeyA)
	var upstream *upstreamError
	if !errors.As(errFetch, &upstream) || upstream.Status != 401 || quotaErrorStatus(errFetch) != 401 {
		t.Fatalf("err = %v", errFetch)
	}
}

func TestQuotaResponse(t *testing.T) {
	report, _ := parseUsage([]byte(`{"usage":{"rolling":{"status":"limited","percent":100,"resetsAt":"2026-10-13T04:56:11.000Z"},"weekly":{"status":"ok","percent":25,"resetsAt":"2026-10-19T00:00:00.000Z"},"monthly":{"status":"ok","percent":0}}}`))
	resp := quotaResponse(report, keyEntry{Label: "Work", APIKey: "oc_sk_test_abcd1234"})
	if resp.Subscription == nil || resp.Subscription.Plan != "OpenCode Go" || len(resp.Groups) != 1 || resp.Groups[0].DisplayName != "Work · oc_sk_…1234" {
		t.Fatalf("resp = %+v", resp)
	}
	buckets := resp.Groups[0].Buckets
	want := []pluginapi.QuotaBucket{
		{Window: "5h", RemainingFraction: 0, ResetTime: "2026-10-13T04:56:11Z", Description: "5-hour (limited)"},
		{Window: "weekly", RemainingFraction: 0.75, ResetTime: "2026-10-19T00:00:00Z", Description: "Weekly"},
		{Window: "monthly", RemainingFraction: 1, Description: "Monthly"},
	}
	if len(buckets) != len(want) {
		t.Fatalf("buckets = %+v", buckets)
	}
	for index := range want {
		if buckets[index] != want[index] {
			t.Fatalf("bucket %d = %+v, want %+v", index, buckets[index], want[index])
		}
	}
	raw := mustJSON(t, resp)
	if !strings.Contains(string(raw), `"remainingFraction":0,`) {
		t.Fatalf("zero fraction dropped: %s", raw)
	}
}

func TestQuotaFetchUsesPhysicalFileAndKeyID(t *testing.T) {
	useSettings(t, defaultSettings())
	fake := installFakeHost(t)
	fake.handle(pluginabi.MethodHostHTTPDo, func(payload json.RawMessage) (any, error) {
		var req hostHTTPRequest
		_ = json.Unmarshal(payload, &req)
		if req.HostCallbackID != "cb-q" || req.URL != "https://opencode.ai/inference/go/v1/usage" || req.Headers.Get("Authorization") != "Bearer "+testKeyB {
			t.Errorf("request = %+v", req)
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: readFixture(t, "usage.json")}, nil
	})
	req := mustJSON(t, rpcQuotaFetchRequest{
		QuotaFetchRequest: pluginapi.QuotaFetchRequest{
			AuthIndex:   "idx-b",
			AuthID:      "opencode-go.json#" + keyIDFor(testKeyB),
			Provider:    "opencode-go",
			StorageJSON: multiKeyFile(t),
			Attributes:  map[string]string{"key_id": keyIDFor(testKeyB), "plugin_virtual": "true"},
		},
		HostCallbackID: "cb-q",
	})
	if !strings.Contains(string(req), `"storage_json"`) || !strings.Contains(string(req), `"host_callback_id":"cb-q"`) {
		t.Fatalf("wire = %s", req)
	}
	raw, errHandle := handleMethod(pluginabi.MethodQuotaFetch, req)
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	resp := decodeResult[pluginapi.QuotaFetchResponse](t, raw)
	if len(resp.Groups) != 1 || len(resp.Groups[0].Buckets) != 3 || resp.Groups[0].Buckets[1].RemainingFraction != 0.625 {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestQuotaFetchErrors(t *testing.T) {
	useSettings(t, defaultSettings())
	fake := installFakeHost(t)
	fake.handle(pluginabi.MethodHostHTTPDo, func(json.RawMessage) (any, error) {
		return pluginapi.HTTPResponse{StatusCode: 500, Body: []byte("boom")}, nil
	})
	missing := mustJSON(t, rpcQuotaFetchRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{StorageJSON: multiKeyFile(t)}})
	raw, _ := handleMethod(pluginabi.MethodQuotaFetch, missing)
	if env := decodeEnvelope(t, raw); env.OK || env.Error.HTTPStatus != 400 {
		t.Fatalf("ambiguous auth = %s", raw)
	}
	upstream := mustJSON(t, rpcQuotaFetchRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{
		StorageJSON: multiKeyFile(t),
		Attributes:  map[string]string{"key_id": keyIDFor(testKeyA)},
	}})
	raw, _ = handleMethod(pluginabi.MethodQuotaFetch, upstream)
	if env := decodeEnvelope(t, raw); env.OK || env.Error.HTTPStatus != 502 || !strings.Contains(env.Error.Message, "500") {
		t.Fatalf("upstream failure = %s", raw)
	}
}
