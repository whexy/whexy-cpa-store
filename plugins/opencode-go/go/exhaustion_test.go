package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestUpstreamErrorMapping(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	useExhaustion(t, time.Now())
	logID := http.Header{"X-Opencode-Log-Id": {"log-err-1"}}
	cases := []struct {
		name       string
		status     int
		body       []byte
		wantStatus int
		wantText   string
	}{
		{"auth", 401, []byte(`{"type":"error","error":{"type":"AuthError","message":"Invalid API key ` + testKeyA + `"}}`), 401, "AuthError: Invalid API key oc_sk_[redacted]"},
		{"permission", 403, readFixture(t, "error_permission.json"), 403, "permission_error: This API key can't call models"},
		{"credit", 402, []byte(`{"type":"error","error":{"type":"CreditLimitExceeded","message":"Credit limit exceeded."}}`), 402, "CreditLimitExceeded"},
		{"budget", 429, []byte(`{"error":{"type":"server_error","message":"Upstream request failed: Account budget exceeded"}}`), 429, "Account budget exceeded"},
		{"model unavailable", 400, readFixture(t, "error_model_unavailable.json"), 404, "Model is unavailable"},
		{"protocol", 400, readFixture(t, "error_protocol_unsupported.json"), 400, "ModelProtocolUnsupported"},
		{"missing session", 400, []byte(`{"type":"error","error":{"type":"MissingSessionID","message":"Request is missing x-opencode-session"}}`), 400, "MissingSessionID"},
		{"server", 500, []byte(`internal failure`), 502, "internal failure"},
		{"unavailable", 503, nil, 502, "Service Unavailable"},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			gateway := installFakeGateway(t, func(hostHTTPRequest) gatewayReply {
				return gatewayReply{Status: tc.status, Headers: logID, Body: tc.body}
			})
			call := testCall{Source: "openai", Model: "opencode-go/kimi-k3", Stream: stream, Payload: clientPayload("openai", "opencode-go/kimi-k3", stream)}
			var raw []byte
			if stream {
				raw, _ = executorExecuteStream(call.raw(t))
			} else {
				raw, _ = executorExecute(call.raw(t))
			}
			env := decodeEnvelope(t, raw)
			if env.OK || env.Error.HTTPStatus != tc.wantStatus {
				t.Fatalf("%s stream=%v: %+v", tc.name, stream, env.Error)
			}
			if !strings.Contains(env.Error.Message, tc.wantText) || !strings.Contains(env.Error.Message, "log id log-err-1") || strings.Contains(env.Error.Message, testKeyA) {
				t.Errorf("%s stream=%v message = %q", tc.name, stream, env.Error.Message)
			}
			if len(gateway.fake.callsTo(pluginabi.MethodHostStreamEmit)) != 0 || len(gateway.fake.callsTo(pluginabi.MethodHostStreamClose)) != 0 {
				t.Errorf("%s: error was streamed instead of returned", tc.name)
			}
			if tc.name == "protocol" && len(gateway.fake.callsTo(pluginabi.MethodHostLog)) == 0 {
				t.Error("protocol rejection was not logged")
			}
		}
	}
}

func TestTransportFailure(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	gateway := installFakeGateway(t, fixtureGateway(t))
	gateway.fake.handle(pluginabi.MethodHostHTTPDo, func(json.RawMessage) (any, error) {
		return nil, &hostCallError{Message: "dial tcp: connection refused"}
	})
	raw, _ := executorExecute(testCall{Source: "openai", Model: "opencode-go/kimi-k3", Payload: clientPayload("openai", "m", false)}.raw(t))
	if env := decodeEnvelope(t, raw); env.OK || env.Error.HTTPStatus != http.StatusBadGateway || !strings.Contains(env.Error.Message, "connection refused") {
		t.Fatalf("envelope = %+v", env.Error)
	}
}

// usageLimitGateway answers inference with GoUsageLimitError until
// exhausted is cleared, and /v1/usage with an exhausted weekly window.
func usageLimitGateway(t *testing.T, resetsAt time.Time, exhausted *bool) func(req hostHTTPRequest) gatewayReply {
	fixtures := fixtureGateway(t)
	return func(req hostHTTPRequest) gatewayReply {
		if strings.HasSuffix(req.URL, "/v1/usage") {
			body := `{"usage":{"rolling":{"status":"ok","percent":40,"resetsAt":"` + resetsAt.Add(-time.Hour).Format(time.RFC3339) + `"},` +
				`"weekly":{"status":"exceeded","percent":100,"resetsAt":"` + resetsAt.Format(time.RFC3339) + `"},` +
				`"monthly":{"status":"ok","percent":60,"resetsAt":"` + resetsAt.Add(240*time.Hour).Format(time.RFC3339) + `"}}}`
			return gatewayReply{Status: 200, Body: []byte(body)}
		}
		if *exhausted && !strings.Contains(string(req.Body), "longcat-2.5-preview-free") {
			return gatewayReply{Status: 429, Body: readFixture(t, "error_usage_limit.json")}
		}
		return fixtures(req)
	}
}

func TestUsageLimitBlocksPaidModelsUntilReset(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	resetsAt := now.Add(30 * time.Hour)
	tracker, scheduled, resets := useExhaustion(t, now)
	exhausted := true
	gateway := installFakeGateway(t, usageLimitGateway(t, resetsAt, &exhausted))
	paid := func(apiKey string, stream bool) envelope {
		call := testCall{Source: "openai", Model: "opencode-go/kimi-k3", Stream: stream, APIKey: apiKey, Payload: clientPayload("openai", "opencode-go/kimi-k3", stream)}
		if stream {
			raw, _ := executorExecuteStream(call.raw(t))
			return decodeEnvelope(t, raw)
		}
		raw, _ := executorExecute(call.raw(t))
		return decodeEnvelope(t, raw)
	}

	env := paid(testKeyA, false)
	if env.OK || env.Error.HTTPStatus != http.StatusTooManyRequests || !strings.Contains(env.Error.Message, "weekly limit") || !strings.Contains(env.Error.Message, "2026-10-14T02:00:00Z") || !strings.Contains(env.Error.Message, "GoUsageLimitError") {
		t.Fatalf("first request = %+v", env.Error)
	}
	var usageCalls []hostHTTPRequest
	for _, req := range gateway.requests {
		if strings.HasSuffix(req.URL, "/v1/usage") {
			usageCalls = append(usageCalls, req)
		}
	}
	if len(usageCalls) != 1 || usageCalls[0].Headers.Get("Authorization") != "Bearer "+testKeyA || usageCalls[0].HostCallbackID != "cb-exec" {
		t.Fatalf("usage calls = %+v", usageCalls)
	}
	if len(*scheduled) != 1 {
		t.Fatalf("scheduled expiries = %d", len(*scheduled))
	}

	before := len(gateway.inferenceRequests())
	for _, stream := range []bool{false, true} {
		env = paid(testKeyA, stream)
		if env.OK || env.Error.HTTPStatus != http.StatusTooManyRequests || !strings.Contains(env.Error.Message, "free models stay available") {
			t.Fatalf("blocked request stream=%v = %+v", stream, env.Error)
		}
	}
	if got := len(gateway.inferenceRequests()); got != before {
		t.Fatalf("blocked requests reached the gateway: %d", got-before)
	}

	free := testCall{Source: "openai", Model: "opencode-go/longcat-2.5-preview-free", Payload: clientPayload("openai", "opencode-go/longcat-2.5-preview-free", false)}
	raw, _ := executorExecute(free.raw(t))
	decodeResult[pluginapi.ExecutorResponse](t, raw)

	exhausted = false
	raw, _ = executorExecute(testCall{Source: "openai", Model: "opencode-go/kimi-k3", APIKey: testKeyB, Payload: clientPayload("openai", "m", false)}.raw(t))
	decodeResult[pluginapi.ExecutorResponse](t, raw)

	(*scheduled)[0]()
	if len(*resets) != 1 || len((*resets)[0]) != 1 || (*resets)[0][0] != authIDFor(keyFileName, keyIDFor(testKeyA)) {
		t.Fatalf("cooldown resets = %v", *resets)
	}
	if env := paid(testKeyA, false); !env.OK {
		t.Fatalf("request after reset = %+v", env.Error)
	}
	if _, blocked := tracker.active(keyIDFor(testKeyA), ""); blocked {
		t.Fatal("block survived its expiry")
	}
}

func TestUsageLimitWithoutExhaustedWindowBlocksBriefly(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	tracker, _, _ := useExhaustion(t, now)
	installFakeGateway(t, func(req hostHTTPRequest) gatewayReply {
		if strings.HasSuffix(req.URL, "/v1/usage") {
			return gatewayReply{Status: 200, Body: readFixture(t, "usage.json")}
		}
		return gatewayReply{Status: 429, Body: readFixture(t, "error_usage_limit.json")}
	})
	raw, _ := executorExecute(testCall{Source: "openai", Model: "opencode-go/kimi-k3", Payload: clientPayload("openai", "m", false)}.raw(t))
	if env := decodeEnvelope(t, raw); env.OK || env.Error.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("envelope = %+v", env.Error)
	}
	block, blocked := tracker.active(keyIDFor(testKeyA), "")
	if !blocked || !block.Until.Equal(now.Add(unknownResetBlock)) {
		t.Fatalf("block = %+v, %v", block, blocked)
	}
}

func TestExhaustionTrackerExpiry(t *testing.T) {
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	tracker, scheduled, resets := useExhaustion(t, now)
	tracker.block("k1", "auth-1", now.Add(time.Hour), []string{"5-hour"})
	if _, blocked := tracker.active("k1", "auth-2"); !blocked {
		t.Fatal("not blocked")
	}
	tracker.block("k1", "", now.Add(2*time.Hour), nil)
	(*scheduled)[0]()
	if len(*resets) != 0 {
		t.Fatalf("superseded expiry reset cooldowns: %v", *resets)
	}
	if _, blocked := tracker.active("k1", ""); !blocked {
		t.Fatal("superseded expiry lifted the newer block")
	}
	(*scheduled)[1]()
	if len(*resets) != 1 || strings.Join((*resets)[0], ",") != "auth-1,auth-2" {
		t.Fatalf("resets = %v", *resets)
	}
	tracker.now = func() time.Time { return now.Add(3 * time.Hour) }
	tracker.block("k2", "auth-3", now.Add(time.Hour), nil)
	if _, blocked := tracker.active("k2", ""); blocked {
		t.Fatal("a block whose time passed still applies")
	}
}

func TestResetAuthCooldownsLooksUpAuthIndex(t *testing.T) {
	fake := installFakeHost(t)
	fake.handle(pluginabi.MethodHostLog, func(json.RawMessage) (any, error) { return struct{}{}, nil })
	fake.handle(pluginabi.MethodHostAuthList, func(json.RawMessage) (any, error) {
		return hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
			{ID: "opencode-go.json#aaa", AuthIndex: "idx-a"},
			{ID: "opencode-go.json#bbb", AuthIndex: "idx-b"},
			{ID: "other.json", AuthIndex: "idx-c"},
		}}, nil
	})
	fake.handle(pluginabi.MethodHostRoutingResetCooldown, func(json.RawMessage) (any, error) {
		return pluginapi.HostRoutingResetCooldownResponse{}, nil
	})
	resetAuthCooldowns([]string{"opencode-go.json#bbb", "opencode-go.json#gone"})
	calls := fake.callsTo(pluginabi.MethodHostRoutingResetCooldown)
	if len(calls) != 1 || !strings.Contains(string(calls[0]), `"auth_index":"idx-b"`) {
		t.Fatalf("reset calls = %s", calls)
	}
}

func TestScheduleAtRunsUnderPluginLifetime(t *testing.T) {
	done := make(chan struct{})
	scheduleAt(time.Now().Add(10*time.Millisecond), func() { close(done) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled function never ran")
	}
}
