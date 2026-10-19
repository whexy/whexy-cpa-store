package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestAGroupAnnouncesItsStoredCapabilities(t *testing.T) {
	withPluginState(t, newTestState(t, `
groups:
  - name: fast
    members:
      - model: claude-haiku-5-5
        provider: claude
    capabilities:
      context_length: 200000
      max_completion_tokens: 64000
      input_modalities: [TEXT, text, Image]
      output_modalities: [text]
      supported_parameters: [tools]
      thinking:
        min: 1024
        max: 32000
        zero_allowed: true
        levels: [Low, high]
  - name: plain
    members:
      - model: claude-haiku-5-5
        provider: claude
`, newFakeHost()))
	models := registerModels().Models
	got := models[0]
	got.Created = 0
	want := pluginapi.ModelInfo{
		ID:                        "auto-group/fast",
		Object:                    "model",
		OwnedBy:                   pluginID,
		DisplayName:               "fast",
		ContextLength:             200000,
		MaxCompletionTokens:       64000,
		SupportedInputModalities:  []string{"text", "image"},
		SupportedOutputModalities: []string{"text"},
		SupportedParameters:       []string{"tools"},
		Thinking: &pluginapi.ThinkingSupport{
			Min: 1024, Max: 32000, ZeroAllowed: true, Levels: []string{"low", "high"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("group model\n got %+v\nwant %+v", got, want)
	}
	plain := models[1]
	plain.Created = 0
	if !reflect.DeepEqual(plain, pluginapi.ModelInfo{ID: "auto-group/plain", Object: "model", OwnedBy: pluginID, DisplayName: "plain"}) {
		t.Fatalf("a group without capabilities announces %+v", plain)
	}
}

func TestThinkingWithoutLevelsOrBudgetIsNotAnnounced(t *testing.T) {
	caps, errParse := parseCapabilities(&rawCapabilities{Thinking: &rawThinking{ZeroAllowed: true}})
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if caps.Thinking != nil {
		t.Fatalf("thinking %+v, want none", caps.Thinking)
	}
}

func TestInvalidCapabilitiesAreRejected(t *testing.T) {
	cases := map[string]string{
		"{context_length: -1}":            "context_length must not be negative",
		"{thinking: {min: 10, max: 5}}":   "thinking budget",
		"{thinking: {min: -1, max: 100}}": "thinking budget",
	}
	for capsYAML, want := range cases {
		_, errParse := parseSettings([]byte(`
groups:
  - name: fast
    members:
      - model: m
    capabilities: ` + capsYAML + `
`))
		if errParse == nil || !strings.Contains(errParse.Error(), want) {
			t.Fatalf("%s: error %v, want %q", capsYAML, errParse, want)
		}
	}
}
