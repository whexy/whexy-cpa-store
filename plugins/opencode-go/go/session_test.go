package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestSessionIDChain(t *testing.T) {
	payload := []byte(clientPayload("openai", "m", false))
	all := http.Header{
		"X-Opencode-Session":       {"oc-1"},
		"x-claude-code-session-id": {"cc-1"},
		"Session_id":               {"codex-1"},
		"Session-Id":               {"sid-1"},
		"X-Session-Id":             {"xs-1"},
		"X-Session-Affinity":       {"aff-1"},
	}
	order := []string{"X-Opencode-Session", "x-claude-code-session-id", "Session_id", "Session-Id", "X-Session-Id", "X-Session-Affinity"}
	if got := sessionID(map[string]any{canonicalSessionKey: "canon-1"}, all, sdktranslator.FormatOpenAI, payload); got != "canon-1" {
		t.Fatalf("canonical = %q", got)
	}
	headers := all.Clone()
	for i, name := range order {
		want := headers[name][0]
		if got := sessionID(map[string]any{canonicalSessionKey: "  "}, headers, sdktranslator.FormatOpenAI, payload); got != want {
			t.Fatalf("step %d: %q, want %q", i, got, want)
		}
		delete(headers, name)
	}
	fallback := sessionID(nil, headers, sdktranslator.FormatOpenAI, payload)
	if !strings.HasPrefix(fallback, "cpa-") || len(fallback) != len("cpa-")+32 {
		t.Fatalf("fallback = %q", fallback)
	}
}

func TestSessionIDHashesUnsafeValues(t *testing.T) {
	got := sessionID(map[string]any{canonicalSessionKey: "claude:会话\r\nX-Injected: 1"}, nil, sdktranslator.FormatOpenAI, nil)
	if !strings.HasPrefix(got, "cpa-") || strings.ContainsAny(got, "\r\n: ") {
		t.Fatalf("session = %q", got)
	}
	if got := sessionID(map[string]any{canonicalSessionKey: "claude:7f1c-uuid#hash"}, nil, sdktranslator.FormatOpenAI, nil); got != "claude:7f1c-uuid#hash" {
		t.Fatalf("safe id rewritten: %q", got)
	}
}

// The fallback stays fixed across turns of one conversation, so the gateway
// sees one session, and differs between conversations.
func TestConversationSessionIDIsStableAcrossTurns(t *testing.T) {
	cases := []struct {
		format              sdktranslator.Format
		first, later, other string
	}{
		{
			sdktranslator.FormatOpenAI,
			`{"messages":[{"role":"system","content":"Sys"},{"role":"user","content":"Hello"}]}`,
			`{"messages":[{"role":"system","content":[{"type":"text","text":"Sys"}]},{"role":"user","content":[{"type":"text","text":"Hello"}]},{"role":"assistant","content":"Hi"},{"role":"user","content":"More"}]}`,
			`{"messages":[{"role":"system","content":"Sys"},{"role":"user","content":"Different"}]}`,
		},
		{
			sdktranslator.FormatClaude,
			`{"system":[{"type":"text","text":"Sys","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"Hello","cache_control":{"type":"ephemeral"}}]}]}`,
			`{"system":[{"type":"text","text":"Sys"}],"messages":[{"role":"user","content":[{"type":"text","text":"Hello"}]},{"role":"assistant","content":[{"type":"text","text":"Hi"}]},{"role":"user","content":[{"type":"text","text":"More","cache_control":{"type":"ephemeral"}}]}]}`,
			`{"system":"Other system","messages":[{"role":"user","content":"Hello"}]}`,
		},
		{
			sdktranslator.FormatOpenAIResponse,
			`{"instructions":"Sys","input":"Hello"}`,
			`{"instructions":"Sys","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Hello"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hi"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"More"}]}]}`,
			`{"instructions":"Sys","input":"Different"}`,
		},
	}
	for _, tc := range cases {
		first := conversationSessionID(tc.format, []byte(tc.first))
		if later := conversationSessionID(tc.format, []byte(tc.later)); later != first {
			t.Errorf("%s: turn 1 %q, later turn %q", tc.format, first, later)
		}
		if other := conversationSessionID(tc.format, []byte(tc.other)); other == first {
			t.Errorf("%s: different conversations share %q", tc.format, first)
		}
	}
	if conversationSessionID(sdktranslator.FormatOpenAI, nil) == "" {
		t.Error("empty payload has no session")
	}
}

func TestInferenceHeaders(t *testing.T) {
	client := http.Header{
		"Authorization":     {"Bearer sk-client-cpa-key"},
		"X-Api-Key":         {"sk-client-cpa-key"},
		"Cookie":            {"a=b"},
		"Connection":        {"keep-alive"},
		"User-Agent":        {"claude-cli/2.1.0 (external, cli)"},
		"Anthropic-Version": {"2023-01-01"},
		"anthropic-beta":    {"interleaved-thinking-2025-05-14", "fine-grained-tool-streaming-2025-05-14"},
	}
	headers := inferenceHeaders(defaultSettings(), testKeyA, sdktranslator.FormatClaude, client, true, "sess-9", false)
	want := map[string]string{
		"Authorization":      "Bearer " + testKeyA,
		"Content-Type":       "application/json",
		"Accept":             "text/event-stream",
		"X-Opencode-Session": "sess-9",
		"User-Agent":         "claude-cli/2.1.0 (external, cli)",
		"Anthropic-Version":  "2023-01-01",
		"Anthropic-Beta":     "interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14",
	}
	if len(headers) != len(want) {
		t.Errorf("headers = %v", headers)
	}
	for name, value := range want {
		if headers.Get(name) != value {
			t.Errorf("%s = %q, want %q", name, headers.Get(name), value)
		}
	}

	headers = inferenceHeaders(defaultSettings(), testKeyA, sdktranslator.FormatClaude, nil, false, "s", false)
	if headers.Get("Anthropic-Version") != defaultAnthropicVersion || headers.Get("Anthropic-Beta") != "" || headers.Get("Accept") != "application/json" {
		t.Errorf("messages defaults = %v", headers)
	}
	if headers.Get("User-Agent") != "whexy-cpa-opencode-go/"+pluginVersion {
		t.Errorf("default user agent = %q", headers.Get("User-Agent"))
	}
	headers = inferenceHeaders(settings{UserAgent: "my-proxy/1"}, testKeyA, sdktranslator.FormatOpenAI, client, false, "s", false)
	if headers.Get("User-Agent") != "claude-cli/2.1.0 (external, cli)" || headers.Get("Anthropic-Version") != "" || headers.Get("Anthropic-Beta") != "" {
		t.Errorf("chat headers = %v", headers)
	}
	if got := inferenceHeaders(settings{UserAgent: "my-proxy/1"}, testKeyA, sdktranslator.FormatOpenAI, nil, false, "s", false).Get("User-Agent"); got != "my-proxy/1" {
		t.Errorf("configured user agent = %q", got)
	}
}

// The executor sends the canonical session and the client's User-Agent, and
// derives a session when the client gives none.
func TestExecutorSendsSessionAndUserAgent(t *testing.T) {
	useSettings(t, defaultSettings())
	useTestCatalog(t)
	gateway := installFakeGateway(t, fixtureGateway(t))
	call := testCall{
		Source:  "claude",
		Model:   "opencode-go/kimi-k3",
		Payload: clientPayload("claude", "opencode-go/kimi-k3", false),
		Headers: http.Header{"User-Agent": {"claude-cli/2.1.0"}, "X-Claude-Code-Session-Id": {"cc-session"}, "X-Api-Key": {"sk-client"}},
		Meta:    map[string]any{canonicalSessionKey: "canonical-7"},
	}
	raw, _ := executorExecute(call.raw(t))
	decodeResult[pluginapi.ExecutorResponse](t, raw)
	call.Meta = nil
	call.Headers = nil
	raw, _ = executorExecute(call.raw(t))
	decodeResult[pluginapi.ExecutorResponse](t, raw)
	sent := gateway.inferenceRequests()
	if sent[0].Headers.Get("X-Opencode-Session") != "canonical-7" || sent[0].Headers.Get("User-Agent") != "claude-cli/2.1.0" || sent[0].Headers.Get("X-Api-Key") != "" {
		t.Errorf("first request headers = %v", sent[0].Headers)
	}
	derived := sent[1].Headers.Get("X-Opencode-Session")
	if derived != conversationSessionID(sdktranslator.FormatClaude, []byte(call.Payload)) || sent[1].Headers.Get("User-Agent") != "whexy-cpa-opencode-go/"+pluginVersion {
		t.Errorf("second request headers = %v", sent[1].Headers)
	}
}
