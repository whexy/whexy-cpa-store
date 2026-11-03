package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestCallerSelectors(t *testing.T) {
	scope := strings.Repeat("a", 64)
	other := strings.Repeat("b", 64)
	valid := fmt.Sprintf("callers: [%q]", scope)
	tests := []struct {
		name        string
		selector    string
		scope       string
		metadata    map[string]any
		want        string
		wantProblem string
	}{
		{name: "absent matches any caller", scope: other, want: "personal"},
		{name: "absent matches no caller", want: "personal"},
		{name: "matching scope", selector: valid, scope: scope, want: "personal"},
		{name: "different scope", selector: valid, scope: other},
		{name: "missing scope", selector: valid},
		{name: "metadata wrong type", selector: valid, metadata: map[string]any{"caller_scope": 123}},
		{name: "empty list", selector: "callers: []", scope: scope, wantProblem: "lists no callers"},
		{name: "null", selector: "callers: null", scope: scope, wantProblem: "callers is not a list of caller scopes"},
		{name: "implicit null", selector: "callers:", scope: scope, wantProblem: "callers is not a list of caller scopes"},
		{name: "scalar", selector: "callers: " + scope, scope: scope, wantProblem: "callers is not a list of caller scopes"},
		{name: "mapping", selector: "callers: {scope: value}", scope: scope, wantProblem: "callers is not a list of caller scopes"},
		{name: "malformed scope", selector: "callers: [invalid]", scope: scope, wantProblem: "caller 1 is not a caller scope"},
		{name: "valid followed by malformed", selector: fmt.Sprintf("callers: [%q, invalid]", scope), scope: scope, wantProblem: "caller 2 is not a caller scope"},
		{name: "malformed followed by valid", selector: fmt.Sprintf("callers: [invalid, %q]", scope), scope: scope, wantProblem: "caller 1 is not a caller scope"},
		{name: "short scope", selector: fmt.Sprintf("callers: [%q]", scope[:63]), scope: scope, wantProblem: "caller 1 is not a caller scope"},
		{name: "long scope", selector: fmt.Sprintf("callers: [%q]", scope+"a"), scope: scope, wantProblem: "caller 1 is not a caller scope"},
		{name: "non hex scope", selector: fmt.Sprintf("callers: [%q]", strings.Repeat("g", 64)), scope: scope, wantProblem: "caller 1 is not a caller scope"},
		{name: "empty entry", selector: `callers: [""]`, scope: scope, wantProblem: "caller 1 is not a caller scope"},
		{name: "null entry", selector: "callers: [null]", scope: scope, wantProblem: "caller 1 is not a caller scope"},
		{name: "nested entry", selector: "callers: [[invalid]]", scope: scope, wantProblem: "caller 1 is not a caller scope"},
		{name: "trim and lowercase", selector: fmt.Sprintf("callers: [%q]", "  "+strings.ToUpper(scope)+"  "), scope: scope, want: "personal"},
		{name: "multiple scopes", selector: fmt.Sprintf("callers: [%q, %q]", other, scope), scope: scope, want: "personal"},
		{name: "duplicate scopes apply once", selector: fmt.Sprintf("callers: [%q, %q]", scope, scope), scope: scope, want: "personal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configureYAML(t, "rules:\n  - models: ['*']\n    "+tt.selector+"\n    prompt: personal\n")
			req := interceptRequestFor(formatOpenAIResponses, "gpt-5.5", "gpt-5.5", `{"input":"hi"}`)
			withCallerScope(&req, tt.scope)
			if tt.metadata != nil {
				req.Metadata = tt.metadata
			}
			raw, errMarshal := json.Marshal(req)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			out, errHandle := handleMethod("request.intercept_after", raw)
			if errHandle != nil {
				t.Fatal(errHandle)
			}
			var response struct {
				OK     bool
				Result struct{ Body []byte }
			}
			if errUnmarshal := json.Unmarshal(out, &response); errUnmarshal != nil || !response.OK {
				t.Fatalf("intercept response = %s, error = %v", out, errUnmarshal)
			}
			if got := gjson.GetBytes(response.Result.Body, "instructions").String(); got != tt.want {
				t.Fatalf("instructions = %q, want %q", got, tt.want)
			}
			if tt.want == "" && len(response.Result.Body) != 0 {
				t.Fatalf("nonmatching request changed to %s", response.Result.Body)
			}
			status := routeManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: statusPath})
			var view statusView
			if errUnmarshal := json.Unmarshal(status.Body, &view); errUnmarshal != nil || view.Error != "" {
				t.Fatalf("status = %s, error = %v", status.Body, errUnmarshal)
			}
			if tt.wantProblem == "" {
				if view.Rules != 1 || len(view.Problems) != 0 {
					t.Fatalf("status = %s, want one valid rule", status.Body)
				}
			} else if view.Rules != 0 || len(view.Problems) != 1 || view.Problems[0].Rule != 1 || view.Problems[0].Error != tt.wantProblem {
				t.Fatalf("status = %s, want rule 1 problem %q", status.Body, tt.wantProblem)
			}
		})
	}
}

func TestCallerRuleOrdering(t *testing.T) {
	scope := strings.Repeat("a", 64)
	org := "  - models: ['*']\n    prompt: organization\n"
	personal := fmt.Sprintf("  - models: [gpt-*]\n    callers: [%q]\n    prompt: personal\n", scope)
	tests := []struct {
		name, rules, scope, model, want string
	}{
		{"organization first", org + personal, scope, "gpt-5.5", "organization\n\npersonal"},
		{"personal first", personal + org, scope, "gpt-5.5", "personal\n\norganization"},
		{"different caller", org + personal, strings.Repeat("b", 64), "gpt-5.5", "organization"},
		{"no caller", org + personal, "", "gpt-5.5", "organization"},
		{"caller matches but model does not", org + personal, scope, "claude-opus-5-5", "organization"},
		{"invalid personal rule leaves organization", org + "  - models: ['*']\n    callers: []\n    prompt: personal\n", scope, "gpt-5.5", "organization"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configureYAML(t, "rules:\n"+tt.rules)
			req := interceptRequestFor(formatOpenAIResponses, tt.model, tt.model, `{"instructions":"client","input":"hi"}`)
			withCallerScope(&req, tt.scope)
			resp := decide(req)
			if got := gjson.GetBytes(resp.Body, "instructions").String(); got != "client\n\n"+tt.want {
				t.Fatalf("instructions = %q, want %q", got, "client\n\n"+tt.want)
			}
		})
	}
}

func withCallerScope(req *pluginapi.RequestInterceptRequest, scope string) {
	if scope != "" {
		req.Metadata = map[string]any{"caller_scope": scope}
	}
}
