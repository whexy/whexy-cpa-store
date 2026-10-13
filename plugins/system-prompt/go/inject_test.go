package main

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

const prompt = "Answer in English."

func TestAppendSystemPrompt(t *testing.T) {
	tests := []struct {
		name   string
		format string
		body   string
		want   string
	}{
		{
			name:   "claude without system",
			format: formatClaude,
			body:   `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			want:   `{"model":"m","messages":[{"role":"user","content":"hi"}],"system":"Answer in English."}`,
		},
		{
			name:   "claude string system",
			format: formatClaude,
			body:   `{"system":"Be brief.","messages":[]}`,
			want:   `{"system":"Be brief.\n\nAnswer in English.","messages":[]}`,
		},
		{
			name:   "claude blocks extend the last text block and keep its cache control",
			format: formatClaude,
			body:   `{"system":[{"type":"text","text":"a"},{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}],"messages":[]}`,
			want:   `{"system":[{"type":"text","text":"a"},{"type":"text","text":"b\n\nAnswer in English.","cache_control":{"type":"ephemeral"}}],"messages":[]}`,
		},
		{
			name:   "claude empty block list",
			format: formatClaude,
			body:   `{"system":[],"messages":[]}`,
			want:   `{"system":[{"type":"text","text":"Answer in English."}],"messages":[]}`,
		},
		{
			name:   "chat completions without system message",
			format: formatOpenAI,
			body:   `{"messages":[ {"role":"user","content":"hi"} ]}`,
			want:   `{"messages":[{"role":"system","content":"Answer in English."},{"role":"user","content":"hi"}]}`,
		},
		{
			name:   "chat completions extend the last leading system or developer message",
			format: formatOpenAI,
			body:   `{"messages":[{"role":"system","content":"a"},{"role":"developer","content":"b"},{"role":"user","content":"hi"},{"role":"system","content":"later"}]}`,
			want:   `{"messages":[{"role":"system","content":"a"},{"role":"developer","content":"b\n\nAnswer in English."},{"role":"user","content":"hi"},{"role":"system","content":"later"}]}`,
		},
		{
			name:   "chat completions content parts",
			format: formatOpenAI,
			body:   `{"messages":[{"role":"system","content":[{"type":"text","text":"a"}]}]}`,
			want:   `{"messages":[{"role":"system","content":[{"type":"text","text":"a\n\nAnswer in English."}]}]}`,
		},
		{
			name:   "responses without instructions",
			format: formatOpenAIResponses,
			body:   `{"input":"hi"}`,
			want:   `{"input":"hi","instructions":"Answer in English."}`,
		},
		{
			name:   "responses instructions",
			format: formatOpenAIResponses,
			body:   `{"instructions":"You are Codex.","input":[]}`,
			want:   `{"instructions":"You are Codex.\n\nAnswer in English.","input":[]}`,
		},
		{
			name:   "gemini without system instruction",
			format: formatGemini,
			body:   `{"contents":[]}`,
			want:   `{"contents":[],"systemInstruction":{"parts":[{"text":"Answer in English."}]}}`,
		},
		{
			name:   "gemini snake case system instruction",
			format: formatGemini,
			body:   `{"system_instruction":{"parts":[{"text":"a"}]},"contents":[]}`,
			want:   `{"system_instruction":{"parts":[{"text":"a\n\nAnswer in English."}]},"contents":[]}`,
		},
		{
			name:   "gemini system instruction without parts",
			format: formatGemini,
			body:   `{"systemInstruction":{"role":"user"},"contents":[]}`,
			want:   `{"systemInstruction":{"role":"user","parts":[{"text":"Answer in English."}]},"contents":[]}`,
		},
		{
			name:   "gemini last part is not text",
			format: formatGemini,
			body:   `{"systemInstruction":{"parts":[{"inlineData":{}}]},"contents":[]}`,
			want:   `{"systemInstruction":{"parts":[{"inlineData":{}},{"text":"Answer in English."}]},"contents":[]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := appendSystemPrompt(tt.format, []byte(tt.body), prompt)
			if !ok {
				t.Fatal("appendSystemPrompt() reported no change")
			}
			if string(got) != tt.want {
				t.Fatalf("appendSystemPrompt() =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestAppendSystemPromptLeavesOtherRequestsAlone(t *testing.T) {
	tests := []struct {
		name   string
		format string
		body   string
	}{
		{name: "claude without messages", format: formatClaude, body: `{"model":"m"}`},
		{name: "legacy completions", format: formatOpenAI, body: `{"prompt":"hi"}`},
		{name: "gemini embedding", format: formatGemini, body: `{"content":{"parts":[]}}`},
		{name: "unknown format", format: "interactions", body: `{"input":"hi"}`},
		{name: "invalid JSON", format: formatClaude, body: `{"messages":[`},
		{name: "not an object", format: formatOpenAIResponses, body: `[]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, ok := appendSystemPrompt(tt.format, []byte(tt.body), prompt); ok {
				t.Fatalf("appendSystemPrompt() = %s, want no change", got)
			}
		})
	}
}

// CLIProxyAPI recognizes Claude Code's helper requests by exactly three
// system blocks; appending must not add a fourth.
func TestAppendClaudeKeepsBlockCount(t *testing.T) {
	body := `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.234; cch=00000;"},{"type":"text","text":"You are Claude Code"},{"type":"text","text":"Generate a title."}],"messages":[]}`
	got, ok := appendSystemPrompt(formatClaude, []byte(body), prompt)
	if !ok {
		t.Fatal("appendSystemPrompt() reported no change")
	}
	if count := len(gjson.GetBytes(got, "system").Array()); count != 3 {
		t.Fatalf("system has %d blocks, want 3", count)
	}
}

func TestDecide(t *testing.T) {
	configureYAML(t, `
enabled: true
rules:
  - models: ["claude-*"]
    prompt: "first"
  - models: ["auto-group/fast", "gpt-5.5"]
    prompt: "second"
`)
	tests := []struct {
		name      string
		requested string
		model     string
		want      string
	}{
		{name: "pattern", requested: "claude-opus-5-5", model: "claude-opus-5-5", want: "first"},
		{name: "requested alias", requested: "auto-group/fast", model: "glm-5", want: "second"},
		{name: "resolved model", requested: "fast", model: "gpt-5.5", want: "second"},
		{name: "thinking suffix", requested: "gpt-5.5(high)", model: "gpt-5.5(high)", want: "second"},
		{name: "case", requested: "GPT-5.5", model: "GPT-5.5", want: "second"},
		{name: "every matching rule in order", requested: "auto-group/fast", model: "claude-sonnet-5-5", want: "first\n\nsecond"},
		{name: "no match", requested: "gemini-3-pro", model: "gemini-3-pro", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := decide(interceptRequestFor(formatOpenAIResponses, tt.requested, tt.model, `{"input":"hi"}`))
			if got := gjson.GetBytes(resp.Body, "instructions").String(); got != tt.want {
				t.Fatalf("instructions = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInterceptAfterThroughABI(t *testing.T) {
	configureYAML(t, "rules: [{models: [claude-*], prompt: hello}]")
	raw, _ := json.Marshal(interceptRequestFor(formatClaude, "claude-opus-5-5", "claude-opus-5-5", `{"messages":[]}`))
	out, errHandle := handleMethod("request.intercept_after", raw)
	if errHandle != nil {
		t.Fatalf("handleMethod() error = %v", errHandle)
	}
	var result struct {
		OK     bool
		Result struct{ Body []byte }
	}
	if errUnmarshal := json.Unmarshal(out, &result); errUnmarshal != nil || !result.OK {
		t.Fatalf("handleMethod() = %s", out)
	}
	if got := gjson.GetBytes(result.Result.Body, "system").String(); got != "hello" {
		t.Fatalf("system = %q, want hello", got)
	}

	out, _ = handleMethod("request.intercept_before", raw)
	result.Result.Body = nil
	if errUnmarshal := json.Unmarshal(out, &result); errUnmarshal != nil || !result.OK || result.Result.Body != nil {
		t.Fatalf("intercept_before = %s, want an unchanged request", out)
	}
}
