package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// providerDefaults are the values the upstream catalog gives every model of a
// built-in provider; an entry that leaves them out gets these.
type providerDefaults struct {
	modelType string
	ownedBy   string
	// geminiName makes name models/<id>, which the Gemini model list shows.
	geminiName bool
}

// builtinProviders are CLIProxyAPI's built-in executor keys. Models added under
// any other key would have no credential to serve them.
var builtinProviders = map[string]providerDefaults{
	"aistudio":            {modelType: "gemini", ownedBy: "google", geminiName: true},
	"antigravity":         {modelType: "antigravity", ownedBy: "antigravity"},
	"claude":              {modelType: "claude", ownedBy: "anthropic"},
	"codex":               {modelType: "openai", ownedBy: "openai"},
	"devin":               {modelType: "devin", ownedBy: "devin"},
	"gemini":              {modelType: "gemini", ownedBy: "google", geminiName: true},
	"gemini-interactions": {modelType: "gemini", ownedBy: "google", geminiName: true},
	"kimi":                {modelType: "kimi", ownedBy: "moonshot"},
	"meta":                {modelType: "meta", ownedBy: "meta"},
	"vertex":              {modelType: "gemini", ownedBy: "google", geminiName: true},
	"xai":                 {modelType: "xai", ownedBy: "xai"},
}

func providerKeys() []string {
	keys := make([]string, 0, len(builtinProviders))
	for key := range builtinProviders {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// modelSet is one applied configuration; it is never modified once stored.
type modelSet struct {
	provider string
	models   []pluginapi.ModelInfo
}

// registration answers model.register. CLIProxyAPI appends the models to every
// credential of the provider; where its catalog lists the same ID, the catalog
// entry wins.
func (s *modelSet) registration() pluginapi.ModelRegistrationResponse {
	if s.provider == "" || len(s.models) == 0 {
		return pluginapi.ModelRegistrationResponse{}
	}
	return pluginapi.ModelRegistrationResponse{Provider: s.provider, Models: slices.Clone(s.models)}
}

func (s *modelSet) ids() []string {
	ids := make([]string, 0, len(s.models))
	for _, model := range s.models {
		ids = append(ids, model.ID)
	}
	return ids
}

type configDocument struct {
	Provider string    `yaml:"provider"`
	Models   yaml.Node `yaml:"models"`
}

// modelEntry reads the fields of a model catalog entry that CLIProxyAPI
// accepts from a plugin, under the catalog's own names, so an entry copied
// from the catalog works unchanged. Other catalog fields are ignored.
type modelEntry struct {
	ID                         string         `yaml:"id"`
	Object                     string         `yaml:"object"`
	Created                    int64          `yaml:"created"`
	OwnedBy                    string         `yaml:"owned_by"`
	Type                       string         `yaml:"type"`
	DisplayName                string         `yaml:"display_name"`
	Name                       string         `yaml:"name"`
	Version                    string         `yaml:"version"`
	Description                string         `yaml:"description"`
	InputTokenLimit            int64          `yaml:"inputTokenLimit"`
	OutputTokenLimit           int64          `yaml:"outputTokenLimit"`
	SupportedGenerationMethods []string       `yaml:"supportedGenerationMethods"`
	ContextLength              int64          `yaml:"context_length"`
	MaxCompletionTokens        int64          `yaml:"max_completion_tokens"`
	SupportedParameters        []string       `yaml:"supported_parameters"`
	SupportedInputModalities   []string       `yaml:"supportedInputModalities"`
	SupportedOutputModalities  []string       `yaml:"supportedOutputModalities"`
	Thinking                   *thinkingEntry `yaml:"thinking"`
}

type thinkingEntry struct {
	Min            int      `yaml:"min"`
	Max            int      `yaml:"max"`
	ZeroAllowed    bool     `yaml:"zero_allowed"`
	DynamicAllowed bool     `yaml:"dynamic_allowed"`
	Levels         []string `yaml:"levels"`
}

var (
	currentSet atomic.Pointer[modelSet]
	// pluginStart is the created time of entries that do not set one, so it
	// stays the same across reconfigurations.
	pluginStart = time.Now().Unix()

	configureMu sync.Mutex
	lastReport  []logLine
)

type logLine struct {
	level, message string
}

func currentModels() *modelSet {
	if set := currentSet.Load(); set != nil {
		return set
	}
	return &modelSet{}
}

// configure applies a plugin.register or plugin.reconfigure request. It never
// fails, because the host disables a plugin whose registration fails.
func configure(raw []byte) {
	configureMu.Lock()
	defer configureMu.Unlock()
	report := applyConfig(raw)
	// The host reconfigures every plugin several times at startup and on each
	// config change, so the outcome is logged only when it changes.
	if slices.Equal(report, lastReport) {
		return
	}
	lastReport = report
	for _, line := range report {
		hostLog(line.level, line.message)
	}
}

func applyConfig(raw []byte) []logLine {
	var request lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
			return []logLine{{"warn", "keeping the previous models: decode lifecycle request: " + errUnmarshal.Error()}}
		}
	}
	set, problems, errParse := parseConfig(request.ConfigYAML)
	if errParse != nil {
		return []logLine{{"warn", "keeping the previous models: " + errParse.Error()}}
	}
	currentSet.Store(set)
	var report []logLine
	for _, problem := range problems {
		report = append(report, logLine{"warn", problem})
	}
	if len(set.models) > 0 {
		report = append(report, logLine{"info", fmt.Sprintf("adding %s to %s", strings.Join(set.ids(), ", "), set.provider)})
	}
	return report
}

// parseConfig reads plugins.configs.extra-models. It fails only when the
// document cannot be read; an entry it cannot use is skipped and described in
// problems.
func parseConfig(configYAML []byte) (*modelSet, []string, error) {
	var doc configDocument
	if errUnmarshal := yaml.Unmarshal(configYAML, &doc); errUnmarshal != nil {
		return nil, nil, fmt.Errorf("decode plugin config: %w", errUnmarshal)
	}
	// Management Center saves a cleared field as null.
	if doc.Models.Kind == yaml.ScalarNode && doc.Models.Tag == "!!null" {
		doc.Models = yaml.Node{}
	}
	if doc.Models.Kind != 0 && doc.Models.Kind != yaml.SequenceNode {
		return nil, nil, fmt.Errorf("models must be a list")
	}

	set := &modelSet{}
	var problems []string
	provider := strings.ToLower(strings.TrimSpace(doc.Provider))
	defaults, known := builtinProviders[provider]
	switch {
	case len(doc.Models.Content) == 0:
		return set, nil, nil
	case provider == "":
		return set, []string{"no models added: choose a provider"}, nil
	case !known:
		return set, []string{fmt.Sprintf("no models added: %q is not a built-in provider; use one of %s", doc.Provider, strings.Join(providerKeys(), ", "))}, nil
	}

	set.provider = provider
	seen := map[string]bool{}
	for index, node := range doc.Models.Content {
		var entry modelEntry
		if errDecode := node.Decode(&entry); errDecode != nil {
			problems = append(problems, fmt.Sprintf("skipping model %d: %v", index+1, errDecode))
			continue
		}
		model, errModel := buildModel(entry, defaults)
		if errModel != nil {
			problems = append(problems, fmt.Sprintf("skipping model %d: %v", index+1, errModel))
			continue
		}
		key := strings.ToLower(model.ID)
		if seen[key] {
			problems = append(problems, fmt.Sprintf("skipping model %d: %s is listed twice", index+1, model.ID))
			continue
		}
		seen[key] = true
		set.models = append(set.models, model)
	}
	return set, problems, nil
}

func buildModel(entry modelEntry, defaults providerDefaults) (pluginapi.ModelInfo, error) {
	id := strings.TrimSpace(entry.ID)
	switch {
	case id == "":
		return pluginapi.ModelInfo{}, fmt.Errorf("it has no id")
	// A parenthesized suffix selects a thinking level, as in model(high).
	case strings.ContainsAny(id, " \t\r\n()"):
		return pluginapi.ModelInfo{}, fmt.Errorf("id %q contains a space or a parenthesis", id)
	}
	for _, limit := range []struct {
		name  string
		value int64
	}{
		{"context_length", entry.ContextLength},
		{"max_completion_tokens", entry.MaxCompletionTokens},
		{"inputTokenLimit", entry.InputTokenLimit},
		{"outputTokenLimit", entry.OutputTokenLimit},
	} {
		if limit.value < 0 {
			return pluginapi.ModelInfo{}, fmt.Errorf("%s of %s is negative", limit.name, id)
		}
	}
	thinking, errThinking := buildThinking(entry.Thinking)
	if errThinking != nil {
		return pluginapi.ModelInfo{}, fmt.Errorf("thinking of %s: %w", id, errThinking)
	}

	model := pluginapi.ModelInfo{
		ID:                         id,
		Object:                     orDefault(entry.Object, "model"),
		Created:                    entry.Created,
		OwnedBy:                    orDefault(entry.OwnedBy, defaults.ownedBy),
		Type:                       orDefault(entry.Type, defaults.modelType),
		DisplayName:                strings.TrimSpace(entry.DisplayName),
		Name:                       strings.TrimSpace(entry.Name),
		Version:                    strings.TrimSpace(entry.Version),
		Description:                strings.TrimSpace(entry.Description),
		InputTokenLimit:            entry.InputTokenLimit,
		OutputTokenLimit:           entry.OutputTokenLimit,
		SupportedGenerationMethods: entry.SupportedGenerationMethods,
		ContextLength:              entry.ContextLength,
		MaxCompletionTokens:        entry.MaxCompletionTokens,
		SupportedParameters:        entry.SupportedParameters,
		SupportedInputModalities:   entry.SupportedInputModalities,
		SupportedOutputModalities:  entry.SupportedOutputModalities,
		Thinking:                   thinking,
		// CLIProxyAPI strips reasoning settings from requests for a model
		// that declares no thinking support, unless the model is user
		// defined; then they reach the upstream, which validates them.
		UserDefined: thinking == nil,
	}
	if model.Created == 0 {
		model.Created = pluginStart
	}
	if model.Name == "" && defaults.geminiName {
		model.Name = "models/" + id
	}
	return model, nil
}

func buildThinking(entry *thinkingEntry) (*pluginapi.ThinkingSupport, error) {
	if entry == nil {
		return nil, nil
	}
	var levels []string
	for _, level := range entry.Levels {
		if level = strings.TrimSpace(level); level != "" {
			levels = append(levels, level)
		}
	}
	switch {
	case entry.Min < 0 || entry.Max < 0:
		return nil, fmt.Errorf("min and max must not be negative")
	case entry.Max > 0 && entry.Min > entry.Max:
		return nil, fmt.Errorf("min %d is above max %d", entry.Min, entry.Max)
	case len(levels) == 0 && entry.Max == 0:
		return nil, fmt.Errorf("it needs levels or a max budget")
	}
	return &pluginapi.ThinkingSupport{
		Min:            entry.Min,
		Max:            entry.Max,
		ZeroAllowed:    entry.ZeroAllowed,
		DynamicAllowed: entry.DynamicAllowed,
		Levels:         levels,
	}, nil
}

func orDefault(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}
