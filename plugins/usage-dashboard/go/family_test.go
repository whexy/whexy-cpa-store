package main

import "testing"

func TestModelFamily(t *testing.T) {
	cases := map[string]string{
		"claude-opus-5-5":         "Claude",
		"ai-proxy/claude-opus-5":  "Claude",
		"gpt-6.1-sol":             "GPT",
		"o3-mini":                 "GPT",
		"o4-mini-high":            "GPT",
		"codex-mini-latest":       "GPT",
		"gemini-3-pro(high)":      "Gemini",
		"models/gemini-2.5-flash": "Gemini",
		"deepseek-v4-pro":         "DeepSeek",
		"deepseek-chat":           "DeepSeek",
		"opencode-go/kimi-k3":     "Kimi",
		"qwen3.8-max":             "Qwen",
		"glm-5.3":                 "GLM",
		"MiniMax-M3":              "MiniMax",
		"grok-build-0.1":          "Grok",
		"phi-4":                   "Phi",
		"":                        "Unknown",
		"123":                     "Other",
		"openrouter/meta-llama-4": "Llama",
		"oswe-vscode-prime":       "Oswe",
		"o1":                      "GPT",
	}
	for model, want := range cases {
		if got := modelFamily(model, nil); got != want {
			t.Errorf("modelFamily(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestModelFamilyConfiguredRulesWinBySpecificity(t *testing.T) {
	rules, errRules := parseFamilyRules(map[string]string{
		"my-*":       "Mine",
		"my-coder-*": "Qwen",
		"claude-*":   "Anthropic",
	})
	if errRules != nil {
		t.Fatal(errRules)
	}
	cases := map[string]string{
		"my-coder-7b":       "Qwen",
		"my-chat":           "Mine",
		"claude-sonnet-5-5": "Anthropic",
		"gpt-6-astra":       "GPT",
	}
	for model, want := range cases {
		if got := modelFamily(model, rules); got != want {
			t.Errorf("modelFamily(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestParseFamilyRulesRejectsBadPattern(t *testing.T) {
	if _, errRules := parseFamilyRules(map[string]string{"[": "X"}); errRules == nil {
		t.Fatal("want an error for a malformed glob")
	}
}
