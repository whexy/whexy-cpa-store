package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// configureYAML applies a configuration as plugin.reconfigure does and
// returns the lines the plugin logged.
func configureYAML(t *testing.T, configYAML string) []string {
	t.Helper()
	previous, previousReport := currentSet.Load(), lastReport
	previousCallHost := callHost
	var logged []string
	callHost = func(method string, payload []byte) ([]byte, error) {
		var request hostLogRequest
		if errUnmarshal := json.Unmarshal(payload, &request); errUnmarshal != nil {
			t.Fatalf("decode %s payload: %v", method, errUnmarshal)
		}
		logged = append(logged, request.Level+" "+request.Message)
		return []byte(`{"ok":true}`), nil
	}
	t.Cleanup(func() {
		currentSet.Store(previous)
		lastReport = previousReport
		callHost = previousCallHost
	})
	raw, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(configYAML)})
	configure(raw)
	return logged
}

func TestCatalogEntryBecomesModel(t *testing.T) {
	// The entry from the upstream catalog, native_capabilities included.
	set, problems, err := parseConfig([]byte(`
enabled: true
priority: 0
provider: vertex
models:
  - id: gemini-4-argon
    object: model
    owned_by: google
    type: gemini
    display_name: Gemini 4 Argon
    name: gemini-4-argon
    description: Gemini 4 Argon
    context_length: 1048576
    max_completion_tokens: 1048576
    thinking:
      min: 128
      max: 65535
      dynamic_allowed: true
    native_capabilities:
      web_search: true
`))
	if err != nil || len(problems) != 0 {
		t.Fatalf("parseConfig: err %v, problems %v", err, problems)
	}
	want := pluginapi.ModelInfo{
		ID:                  "gemini-4-argon",
		Object:              "model",
		Created:             pluginStart,
		OwnedBy:             "google",
		Type:                "gemini",
		DisplayName:         "Gemini 4 Argon",
		Name:                "gemini-4-argon",
		Description:         "Gemini 4 Argon",
		ContextLength:       1048576,
		MaxCompletionTokens: 1048576,
		Thinking:            &pluginapi.ThinkingSupport{Min: 128, Max: 65535, DynamicAllowed: true},
	}
	if set.provider != "vertex" || len(set.models) != 1 || !reflect.DeepEqual(set.models[0], want) {
		t.Fatalf("set = %s %+v, want vertex %+v", set.provider, set.models, want)
	}
}

func TestProviderDefaults(t *testing.T) {
	tests := []struct {
		provider, modelType, ownedBy, name string
	}{
		{"codex", "openai", "openai", ""},
		{"claude", "claude", "anthropic", ""},
		{"antigravity", "antigravity", "antigravity", ""},
		{" AIStudio ", "gemini", "google", "models/x-1"},
	}
	for _, tt := range tests {
		set, problems, err := parseConfig([]byte("provider: " + tt.provider + "\nmodels: [{id: ' x-1 '}]"))
		if err != nil || len(problems) != 0 || len(set.models) != 1 {
			t.Fatalf("%s: err %v, problems %v, models %+v", tt.provider, err, problems, set.models)
		}
		model := set.models[0]
		if set.provider != strings.ToLower(strings.TrimSpace(tt.provider)) || model.ID != "x-1" || model.Object != "model" ||
			model.Type != tt.modelType || model.OwnedBy != tt.ownedBy || model.Name != tt.name || model.Created != pluginStart {
			t.Errorf("%s: model = %+v", tt.provider, model)
		}
		// Without thinking, the client's reasoning settings reach the upstream.
		if model.Thinking != nil || !model.UserDefined {
			t.Errorf("%s: thinking %+v, user defined %v", tt.provider, model.Thinking, model.UserDefined)
		}
	}
}

func TestEntriesThatDoNotApply(t *testing.T) {
	set, problems, err := parseConfig([]byte(`
provider: codex
models:
  - id: gpt-6
    thinking: {levels: [" low ", "", high]}
  - display_name: no id
  - id: "gpt 6"
  - id: gpt-6(high)
  - id: GPT-6
  - id: gpt-6-mini
    context_length: -1
  - id: gpt-6-nano
    context_length: lots
  - id: gpt-6-pro
    thinking: {min: 2048, max: 1024}
  - id: gpt-6-max
    thinking: {zero_allowed: true}
  - just-a-string
`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if ids := set.ids(); !reflect.DeepEqual(ids, []string{"gpt-6"}) {
		t.Fatalf("ids = %v", ids)
	}
	if levels := set.models[0].Thinking.Levels; !reflect.DeepEqual(levels, []string{"low", "high"}) || set.models[0].UserDefined {
		t.Fatalf("thinking = %+v, user defined %v", set.models[0].Thinking, set.models[0].UserDefined)
	}
	wantProblems := []string{
		"skipping model 2: it has no id",
		`skipping model 3: id "gpt 6" contains a space or a parenthesis`,
		`skipping model 4: id "gpt-6(high)" contains a space or a parenthesis`,
		"skipping model 5: GPT-6 is listed twice",
		"skipping model 6: context_length of gpt-6-mini is negative",
		"skipping model 7: yaml: unmarshal errors:",
		"skipping model 8: thinking of gpt-6-pro: min 2048 is above max 1024",
		"skipping model 9: thinking of gpt-6-max: it needs levels or a max budget",
		"skipping model 10: yaml: unmarshal errors:",
	}
	if len(problems) != len(wantProblems) {
		t.Fatalf("problems = %q", problems)
	}
	for index, want := range wantProblems {
		if !strings.HasPrefix(problems[index], want) {
			t.Errorf("problem %d = %q, want prefix %q", index+1, problems[index], want)
		}
	}
}

func TestProviderChoice(t *testing.T) {
	tests := []struct {
		config, problem string
	}{
		{"models: [{id: a}]", "no models added: choose a provider"},
		{"provider: openai\nmodels: [{id: a}]", `no models added: "openai" is not a built-in provider; use one of aistudio, antigravity,`},
		{"provider: codex", ""},
		{"provider: codex\nmodels: []", ""},
		{"provider: null\nmodels: null", ""},
		{"", ""},
	}
	for _, tt := range tests {
		set, problems, err := parseConfig([]byte(tt.config))
		if err != nil {
			t.Fatalf("%q: %v", tt.config, err)
		}
		if response := set.registration(); response.Provider != "" || len(response.Models) != 0 {
			t.Errorf("%q: registration = %+v, want none", tt.config, response)
		}
		switch {
		case tt.problem == "" && len(problems) != 0:
			t.Errorf("%q: problems = %q", tt.config, problems)
		case tt.problem != "" && (len(problems) != 1 || !strings.HasPrefix(problems[0], tt.problem)):
			t.Errorf("%q: problems = %q, want %q", tt.config, problems, tt.problem)
		}
	}
}

func TestUnreadableConfigKeepsPreviousModels(t *testing.T) {
	configureYAML(t, "provider: claude\nmodels: [{id: claude-next}]")
	for _, config := range []string{"provider: [", "provider: claude\nmodels: {id: x}"} {
		logged := configureYAML(t, config)
		if ids := currentModels().ids(); !reflect.DeepEqual(ids, []string{"claude-next"}) {
			t.Fatalf("%q: ids = %v", config, ids)
		}
		if len(logged) != 1 || !strings.HasPrefix(logged[0], "warn extra-models: keeping the previous models") {
			t.Fatalf("%q: logged %q", config, logged)
		}
	}
}

func TestClearedModelsRemoveThem(t *testing.T) {
	configureYAML(t, "provider: claude\nmodels: [{id: claude-next}]")
	configureYAML(t, "provider: claude\nmodels: null")
	if ids := currentModels().ids(); len(ids) != 0 {
		t.Fatalf("ids = %v", ids)
	}
}

func TestRepeatedConfigLogsOnce(t *testing.T) {
	config := "provider: claude\nmodels: [{id: claude-next}, {id: ''}]"
	if logged := configureYAML(t, config); len(logged) != 2 {
		t.Fatalf("first configure logged %q", logged)
	}
	if logged := configureYAML(t, config); len(logged) != 0 {
		t.Fatalf("same configure logged %q", logged)
	}
	if logged := configureYAML(t, "provider: claude\nmodels: [{id: claude-next}]"); !reflect.DeepEqual(logged, []string{"info extra-models: adding claude-next to claude"}) {
		t.Fatalf("changed configure logged %q", logged)
	}
}

func TestModelRegister(t *testing.T) {
	logged := configureYAML(t, "provider: claude\nmodels: [{id: claude-next}, {id: ''}]")
	if want := []string{
		"warn extra-models: skipping model 2: it has no id",
		"info extra-models: adding claude-next to claude",
	}; !reflect.DeepEqual(logged, want) {
		t.Fatalf("logged %q, want %q", logged, want)
	}
	raw, errHandle := handleMethod("model.register", nil)
	if errHandle != nil {
		t.Fatalf("model.register: %v", errHandle)
	}
	var reply struct {
		OK     bool
		Result pluginapi.ModelRegistrationResponse
	}
	if errUnmarshal := json.Unmarshal(raw, &reply); errUnmarshal != nil {
		t.Fatalf("decode reply: %v", errUnmarshal)
	}
	if !reply.OK || reply.Result.Provider != "claude" || len(reply.Result.Models) != 1 || reply.Result.Models[0].ID != "claude-next" {
		t.Fatalf("reply = %s", raw)
	}
}

func TestRegistrationDeclaresSettings(t *testing.T) {
	configureYAML(t, "")
	raw, errHandle := handleMethod("plugin.register", nil)
	if errHandle != nil {
		t.Fatalf("plugin.register: %v", errHandle)
	}
	var reply struct {
		OK     bool
		Result registration
	}
	if errUnmarshal := json.Unmarshal(raw, &reply); errUnmarshal != nil {
		t.Fatalf("decode reply: %v", errUnmarshal)
	}
	fields := reply.Result.Metadata.ConfigFields
	if !reply.OK || !reply.Result.Capabilities.ModelRegistrar || reply.Result.Metadata.GitHubRepository == "" || len(fields) != 2 {
		t.Fatalf("reply = %s", raw)
	}
	if fields[0].Name != "provider" || !reflect.DeepEqual(fields[0].EnumValues, providerKeys()) || fields[1].Name != "models" {
		t.Fatalf("fields = %+v", fields)
	}
	for _, field := range fields {
		if strings.ContainsAny(field.Description, `<>&"'`) {
			t.Errorf("description of %s contains a character CPA escapes: %q", field.Name, field.Description)
		}
	}
}
