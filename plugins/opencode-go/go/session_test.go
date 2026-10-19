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

// Two threads of one client in one folder open on the same system prompt and
// the same first user message — Codex's environment block is both — and the
// hash alone would make them one session. The thread a request names with
// its prompt_cache_key is what separates them, and it holds across turns.
func TestConversationSessionIDNamesAThreadByItsPromptCacheKey(t *testing.T) {
	opening := `"instructions":"You are Codex.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context><cwd>/work</cwd></environment_context>"}]}]`
	first := []byte(`{"prompt_cache_key":"thread-a",` + opening + `}`)
	later := []byte(`{"prompt_cache_key":"thread-a",` + opening + `,"max_output_tokens":32}`)
	other := []byte(`{"prompt_cache_key":"thread-b",` + opening + `}`)

	session := conversationSessionID(sdktranslator.FormatOpenAIResponse, first)
	if got := conversationSessionID(sdktranslator.FormatOpenAIResponse, later); got != session {
		t.Fatalf("one thread got two sessions: %q then %q", session, got)
	}
	if got := conversationSessionID(sdktranslator.FormatOpenAIResponse, other); got == session {
		t.Fatalf("two threads share one session: %q", session)
	}
	// A blank key names no thread, so the conversation's own words do.
	blank := []byte(`{"prompt_cache_key":"  ",` + opening + `}`)
	keyless := []byte(`{` + opening + `}`)
	if got := conversationSessionID(sdktranslator.FormatOpenAIResponse, blank); got == session || got != conversationSessionID(sdktranslator.FormatOpenAIResponse, keyless) {
		t.Fatalf("a blank prompt_cache_key seeded %q", got)
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
	// A caller asking as itself never gains OpenCode's identity headers: the
	// client header it did send is read, and nothing is copied.
	if headers.Get("X-Opencode-Session-Id") != "" || headers.Get("X-Opencode-Request") != "" || headers.Get("X-Opencode-Client") != "" {
		t.Errorf("an uncloaked request carries OpenCode's identity: %v", headers)
	}
}

// An impersonated request carries the client's identity whole, not the least
// of it the gate reads: the full User-Agent, the session in both headers it
// names one in, an identifier for this turn, and its client and project —
// and nothing the client does not send, the client's own User-Agent and
// keys first.
func TestInferenceHeadersImpersonate(t *testing.T) {
	client := http.Header{
		"Authorization": {"Bearer sk-client-cpa-key"},
		"Cookie":        {"a=b"},
		"Referer":       {"https://example.test/"},
		"User-Agent":    {"curl/8.0"},
	}
	headers := inferenceHeaders(defaultSettings(), testKeyA, sdktranslator.FormatOpenAI, client, true, "ses-test", true)
	want := map[string]string{
		"Authorization":         "Bearer " + testKeyA,
		"Content-Type":          "application/json",
		"Accept":                "text/event-stream",
		"User-Agent":            cloakUserAgent,
		"X-Opencode-Session":    "ses-test",
		"X-Opencode-Session-Id": "ses-test",
		"X-Opencode-Client":     "cli",
		"X-Opencode-Project":    "global",
	}
	if len(headers) != len(want)+1 {
		t.Fatalf("headers = %v", headers)
	}
	for name, value := range want {
		if headers.Get(name) != value {
			t.Errorf("%s = %q, want %q", name, headers.Get(name), value)
		}
	}
	request := headers.Get("X-Opencode-Request")
	if !strings.HasPrefix(request, "msg_") || len(request) != len("msg_")+26 {
		t.Errorf("request id = %q", request)
	}
	if headers.Get("Referer") != "" || headers.Get("Cookie") != "" {
		t.Errorf("a client header was forwarded: %v", headers)
	}
	// Every turn carries a turn of its own: two impersonated requests on the
	// same session do not share a request identifier, and their identifiers
	// count up rather than down, which is how the client mints a message's.
	again := inferenceHeaders(defaultSettings(), testKeyA, sdktranslator.FormatOpenAI, client, true, "ses-test", true).Get("X-Opencode-Request")
	if again == request {
		t.Errorf("two turns share one request id %q", request)
	}
	first, second := messageIDValue(t, request), messageIDValue(t, again)
	if second != first+1 {
		t.Errorf("request ids count %d then %d, want consecutive", first, second)
	}
}

// messageIDValue reads the leading twelve hex digits of a msg_ identifier,
// which a message's are not inverted.
func messageIDValue(t *testing.T, id string) uint64 {
	t.Helper()
	if !strings.HasPrefix(id, "msg_") || len(id) != len("msg_")+26 {
		t.Fatalf("id %q is not a message id", id)
	}
	value := uint64(0)
	for _, char := range id[len("msg_") : len("msg_")+12] {
		digit := strings.IndexRune("0123456789abcdef", char)
		if digit < 0 {
			t.Fatalf("id %q has a non-hex leading byte %q", id, char)
		}
		value = value<<4 | uint64(digit)
	}
	return value
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
