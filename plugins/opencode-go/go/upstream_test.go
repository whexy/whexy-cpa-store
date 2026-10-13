package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestParseUpstreamError(t *testing.T) {
	tests := []struct {
		fixture string
		body    string
		status  int
		wantTyp string
		wantMsg string
	}{
		{fixture: "error_permission.json", status: 403, wantTyp: upstreamPermission, wantMsg: "This API key can't call models"},
		{fixture: "error_usage_limit.json", status: 429, wantTyp: upstreamUsageLimit, wantMsg: "Usage limit reached."},
		{fixture: "error_protocol_unsupported.json", status: 400, wantTyp: upstreamProtocolUnsupported, wantMsg: "Model does not support this protocol."},
		{fixture: "error_model_unavailable.json", status: 400, wantTyp: "server_error", wantMsg: "Upstream request failed: Model is unavailable."},
		{body: `{"message":"flat"}`, status: 500, wantMsg: "flat"},
		{body: "<html>bad gateway</html>", status: 502, wantMsg: "<html>bad gateway</html>"},
		{body: "", status: 503, wantMsg: "Service Unavailable"},
	}
	for _, tt := range tests {
		body := []byte(tt.body)
		if tt.fixture != "" {
			body = readFixture(t, tt.fixture)
		}
		got := parseUpstreamError(tt.status, http.Header{"X-Opencode-Log-Id": {"a5f3"}}, body)
		if got.Status != tt.status || got.Type != tt.wantTyp || got.Message != tt.wantMsg || got.LogID != "a5f3" {
			t.Fatalf("%s%s = %+v", tt.fixture, tt.body, got)
		}
	}
}

func TestUpstreamErrorRedactsAndNamesLogID(t *testing.T) {
	err := parseUpstreamError(401, http.Header{"X-Opencode-Log-Id": {"log-9"}}, []byte(`{"type":"error","error":{"type":"AuthError","message":"bad key `+testKeyA+`"}}`))
	text := err.Error()
	if strings.Contains(text, testKeyA) || !strings.Contains(text, "401 AuthError") || !strings.Contains(text, "log id log-9") {
		t.Fatalf("error = %q", text)
	}
}
