package translate

import (
	"errors"
	"os"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const (
	chat      = sdktranslator.FormatOpenAI
	responses = sdktranslator.FormatOpenAIResponse
	messages  = sdktranslator.FormatClaude
	gemini    = sdktranslator.FormatGemini
	codex     = sdktranslator.FormatCodex
)

func TestNormalizeFormat(t *testing.T) {
	withGoogle := Provider{Aliases: map[string]sdktranslator.Format{"google": gemini}}
	cases := []struct {
		provider Provider
		name     string
		want     sdktranslator.Format
	}{
		{Provider{}, "", chat},
		{Provider{}, "chat-completions", chat},
		{Provider{}, " chat_completions ", chat},
		{Provider{}, "Responses", responses},
		{Provider{}, "openai-response", responses},
		{Provider{}, "openai-responses", responses},
		{Provider{}, "anthropic", messages},
		{Provider{}, "claude", messages},
		{Provider{}, "Gemini", gemini},
		{Provider{}, "codex", codex},
		{Provider{}, "google", "google"},
		{withGoogle, "Google", gemini},
		{withGoogle, "messages", messages},
	}
	for _, tc := range cases {
		if got := tc.provider.NormalizeFormat(tc.name); got != tc.want {
			t.Errorf("NormalizeFormat(%q) with aliases %v = %q, want %q", tc.name, tc.provider.Aliases, got, tc.want)
		}
	}
}

func TestPlanRoute(t *testing.T) {
	cases := []struct {
		client, upstream    sdktranslator.Format
		requestTo, response sdktranslator.Format
	}{
		{chat, chat, "", ""},
		{chat, messages, messages, messages},
		{chat, responses, codex, codex},
		{messages, chat, chat, chat},
		{messages, messages, "", ""},
		{messages, responses, codex, codex},
		{responses, chat, chat, chat},
		{responses, messages, messages, messages},
		{responses, responses, "", ""},
		{gemini, messages, messages, messages},
		{gemini, responses, codex, codex},
		{messages, gemini, gemini, gemini},
		{gemini, gemini, "", ""},
	}
	for _, tc := range cases {
		plan, errPlan := PlanRoute(tc.client, tc.client, tc.upstream)
		if errPlan != nil {
			t.Errorf("%s->%s: %v", tc.client, tc.upstream, errPlan)
			continue
		}
		if plan.RequestTo != tc.requestTo || plan.ResponseFrom != tc.response {
			t.Errorf("%s->%s = request %q response %q", tc.client, tc.upstream, plan.RequestTo, plan.ResponseFrom)
		}
	}
}

func TestPlanRouteRejects(t *testing.T) {
	cases := []struct {
		source, response, upstream sdktranslator.Format
		want                       string
	}{
		{"bogus", "bogus", chat, "no translator from bogus requests to openai"},
		{chat, "bogus", chat, "no translator from openai responses to bogus"},
		{chat, chat, "", errNoUpstream.Error()},
	}
	for _, tc := range cases {
		if _, errPlan := PlanRoute(tc.source, tc.response, tc.upstream); errPlan == nil || errPlan.Error() != tc.want {
			t.Errorf("PlanRoute(%q, %q, %q) = %v, want %q", tc.source, tc.response, tc.upstream, errPlan, tc.want)
		}
	}
}

func TestBuildRequest(t *testing.T) {
	cases := []struct {
		name    string
		source  sdktranslator.Format
		payload string
		stream  bool
		want    map[string]string
		absent  []string
	}{
		{
			name:    "chat limit kept, defaults dropped",
			source:  chat,
			payload: `{"model":"x","messages":[{"role":"user","content":"hi"}],"max_tokens":32}`,
			want:    map[string]string{"max_output_tokens": "32", "model": "gpt-6-luna", "store": "false", "include.0": "reasoning.encrypted_content", "stream": "false"},
			absent:  []string{"reasoning", "parallel_tool_calls"},
		},
		{
			name:    "chat options carried",
			source:  chat,
			payload: `{"model":"x","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":1,"reasoning_effort":"high","parallel_tool_calls":false,"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`,
			stream:  true,
			want:    map[string]string{"max_output_tokens": "16", "reasoning.effort": "high", "parallel_tool_calls": "false", "stream": "true"},
		},
		{
			name:    "claude stream flag settled",
			source:  messages,
			payload: `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`,
			want:    map[string]string{"max_output_tokens": "16", "stream": "false"},
			absent:  []string{"reasoning"},
		},
		{
			name:    "claude thinking kept",
			source:  messages,
			payload: `{"model":"x","max_tokens":4096,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"hi"}]}`,
			stream:  true,
			want:    map[string]string{"max_output_tokens": "4096", "stream": "true"},
		},
		{
			name:    "gemini limits carried",
			source:  gemini,
			payload: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":32,"thinkingConfig":{"thinkingLevel":"high"}}}`,
			stream:  true,
			want:    map[string]string{"max_output_tokens": "32", "reasoning.effort": "high"},
		},
	}
	for _, tc := range cases {
		plan, errPlan := PlanRoute(tc.source, tc.source, responses)
		if errPlan != nil {
			t.Fatalf("%s: %v", tc.name, errPlan)
		}
		body, errBuild := BuildRequest(t.Context(), plan, []byte(tc.payload), "gpt-6-luna", tc.stream)
		if errBuild == nil {
			body, errBuild = SetStream(body, tc.stream)
		}
		if errBuild != nil {
			t.Fatalf("%s: %v", tc.name, errBuild)
		}
		for path, want := range tc.want {
			if got := gjson.GetBytes(body, path).String(); got != want {
				t.Errorf("%s: %s = %q, want %q: %s", tc.name, path, got, want, body)
			}
		}
		for _, path := range tc.absent {
			if gjson.GetBytes(body, path).Exists() {
				t.Errorf("%s: %s survived: %s", tc.name, path, body)
			}
		}
	}
}

func TestBuildRequestPassthrough(t *testing.T) {
	plan, _ := PlanRoute(chat, chat, chat)
	payload := `{"model":"gpt-6-luna","messages":[]}`
	body, errBuild := BuildRequest(t.Context(), plan, []byte(payload), "gpt-6-luna", false)
	if errBuild != nil || string(body) != payload {
		t.Errorf("unchanged body = %s, %v", body, errBuild)
	}
	body, _ = BuildRequest(t.Context(), plan, []byte(payload), "glm-6", false)
	if gjson.GetBytes(body, "model").String() != "glm-6" {
		t.Errorf("model not rewritten: %s", body)
	}
	for _, payload := range []string{``, `[]`, `not json`} {
		if _, errBuild := BuildRequest(t.Context(), plan, []byte(payload), "m", false); !errors.Is(errBuild, errNotObject) {
			t.Errorf("%q: %v", payload, errBuild)
		}
	}
}

func TestSetStream(t *testing.T) {
	cases := []struct {
		body   string
		stream bool
		want   string
	}{
		{`{"a":1}`, false, `{"a":1}`},
		{`{"a":1}`, true, `{"a":1,"stream":true}`},
		{`{"stream":true}`, true, `{"stream":true}`},
		{`{"stream":true}`, false, `{"stream":false}`},
		{`{"stream":false}`, false, `{"stream":false}`},
	}
	for _, tc := range cases {
		got, errSet := SetStream([]byte(tc.body), tc.stream)
		if errSet != nil || string(got) != tc.want {
			t.Errorf("SetStream(%s, %v) = %s, %v, want %s", tc.body, tc.stream, got, errSet, tc.want)
		}
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, errRead := os.ReadFile("testdata/" + name)
	if errRead != nil {
		t.Fatal(errRead)
	}
	return raw
}

func TestCodexTerminalEvent(t *testing.T) {
	errBoom := errors.New("boom")
	provider := Provider{Name: "Demo", Failed: func([]byte) error { return errBoom }}
	cases := []struct {
		name     string
		provider Provider
		body     []byte
		event    string
		err      string
	}{
		{"incomplete", provider, []byte(`{"status":"incomplete","output":[]}`), "response.incomplete", ""},
		{"completed", provider, readFixture(t, "responses_response.json"), "response.completed", ""},
		{"failed", provider, []byte(`{"status":"failed"}`), "", "boom"},
		{"failed without hook", Provider{Name: "Demo"}, []byte(`{"status":"failed"}`), "", "Demo returned a failed response"},
		{"not JSON", provider, []byte(`not json`), "", "Demo returned a Responses body that is not JSON"},
	}
	for _, tc := range cases {
		event, errEvent := tc.provider.CodexTerminalEvent(tc.body)
		if tc.err != "" {
			if errEvent == nil || errEvent.Error() != tc.err {
				t.Errorf("%s: error %v, want %q", tc.name, errEvent, tc.err)
			}
			continue
		}
		if errEvent != nil || gjson.GetBytes(event, "type").String() != tc.event || gjson.GetBytes(event, "response.status").String() != gjson.GetBytes(tc.body, "status").String() {
			t.Errorf("%s: event %s, %v", tc.name, event, errEvent)
		}
	}
}

func TestConvertResponse(t *testing.T) {
	provider := Provider{Name: "Demo"}
	upper := func(body []byte) []byte { return []byte(strings.ToUpper(string(body))) }
	same, _ := PlanRoute(responses, responses, responses)
	cases := []struct {
		name        string
		passthrough func([]byte) []byte
		want        string
	}{
		{"untouched", nil, `{"ok":true}`},
		{"rewritten", upper, `{"OK":TRUE}`},
	}
	for _, tc := range cases {
		out, errConvert := provider.ConvertResponse(t.Context(), same, "m", nil, nil, []byte(`{"ok":true}`), tc.passthrough)
		if errConvert != nil || string(out) != tc.want {
			t.Errorf("%s: %s, %v", tc.name, out, errConvert)
		}
	}

	plan, errPlan := PlanRoute(chat, chat, responses)
	if errPlan != nil {
		t.Fatal(errPlan)
	}
	request := []byte(`{"model":"gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`)
	out, errConvert := provider.ConvertResponse(t.Context(), plan, "gpt-6-luna", request, request, readFixture(t, "responses_response.json"), upper)
	if errConvert != nil {
		t.Fatal(errConvert)
	}
	// The passthrough hook applies only to untranslated responses.
	if gjson.GetBytes(out, "choices.0.message.content").String() != "OK" || gjson.GetBytes(out, "object").String() != "chat.completion" {
		t.Errorf("chat response = %s", out)
	}
	if _, errConvert := provider.ConvertResponse(t.Context(), plan, "m", request, request, []byte(`{"status":"failed"}`), nil); errConvert == nil {
		t.Error("failed Responses object converted")
	}
}
