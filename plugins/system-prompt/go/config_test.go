package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func configureYAML(t *testing.T, configYAML string) {
	t.Helper()
	previous := currentRuleSet.Load()
	t.Cleanup(func() { currentRuleSet.Store(previous) })
	raw, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(configYAML)})
	configure(raw)
}

func interceptRequestFor(format, requested, model, body string) pluginapi.RequestInterceptRequest {
	return pluginapi.RequestInterceptRequest{
		SourceFormat:   format,
		RequestedModel: requested,
		Model:          model,
		Body:           []byte(body),
	}
}

func TestCompileRules(t *testing.T) {
	rules := compileRules([]byte(`
enabled: true
priority: 10
rules:
  - models: [" Claude-* ", "claude-*", ""]
    prompt: "  keep me \n"
  - models: []
    prompt: "no models"
  - models: [gpt-5.5]
    prompt: "   "
`))
	if rules.err != "" {
		t.Fatalf("err = %q", rules.err)
	}
	wantRules := []rule{{patterns: []string{"claude-*"}, prompt: "keep me"}}
	if !reflect.DeepEqual(rules.rules, wantRules) {
		t.Fatalf("rules = %#v, want %#v", rules.rules, wantRules)
	}
	wantProblems := []ruleProblem{{Rule: 2, Error: "lists no models"}, {Rule: 3, Error: "has an empty prompt"}}
	if !reflect.DeepEqual(rules.problems, wantProblems) {
		t.Fatalf("problems = %#v, want %#v", rules.problems, wantProblems)
	}
}

func TestCompileRulesUnreadable(t *testing.T) {
	for _, configYAML := range []string{"rules: {models: x}", "rules: [", "rules: [{models: {a: b}}]"} {
		rules := compileRules([]byte(configYAML))
		if rules.err == "" || len(rules.rules) != 0 {
			t.Fatalf("compileRules(%q) = %+v, want an error and no rules", configYAML, rules)
		}
	}
	if rules := compileRules(nil); rules.err != "" || len(rules.rules) != 0 {
		t.Fatalf("compileRules(nil) = %+v, want no rules and no error", rules)
	}
}

func TestWildcardMatch(t *testing.T) {
	tests := []struct {
		pattern, value string
		want           bool
	}{
		{"gpt-5.5", "gpt-5.5", true},
		{"gpt-5.5", "gpt-5.5-mini", false},
		{"gpt-5.?", "gpt-5.5", false},
		{"claude-*", "claude-opus-5-5", true},
		{"*", "anything/at/all", true},
		{"*/fast", "auto-group/fast", true},
		{"*opus*", "claude-opus-5-5", true},
		{"a*b*c", "abc", true},
		{"a*b*c", "acb", false},
		{"ab*ba", "aba", false},
		{"*-mini", "gpt-mini-x", false},
	}
	for _, tt := range tests {
		if got := wildcardMatch(tt.pattern, tt.value); got != tt.want {
			t.Errorf("wildcardMatch(%q, %q) = %v, want %v", tt.pattern, tt.value, got, tt.want)
		}
	}
}

func TestRegistrationDeclaresInterceptorOnlyWithRules(t *testing.T) {
	configureYAML(t, "rules: []")
	if pluginRegistration().Capabilities.RequestInterceptor {
		t.Fatal("request interceptor declared without rules")
	}
	configureYAML(t, "rules: [{models: [x], prompt: y}]")
	if !pluginRegistration().Capabilities.RequestInterceptor {
		t.Fatal("request interceptor not declared with a rule")
	}
}
