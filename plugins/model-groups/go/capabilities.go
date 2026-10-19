package main

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// capabilities is what a group announces about itself: only what every
// member supports, since a request may land on any of them. The plugin cannot
// read CPA's model registry, so the editor works it out from the members and
// stores it with the group when the group is saved; a group without it
// announces nothing beyond its ID.
type capabilities struct {
	ContextLength       int64
	MaxCompletionTokens int64
	InputTokenLimit     int64
	OutputTokenLimit    int64
	InputModalities     []string
	OutputModalities    []string
	Parameters          []string
	GenerationMethods   []string
	Thinking            *pluginapi.ThinkingSupport
}

type rawCapabilities struct {
	ContextLength       int64        `yaml:"context_length"`
	MaxCompletionTokens int64        `yaml:"max_completion_tokens"`
	InputTokenLimit     int64        `yaml:"input_token_limit"`
	OutputTokenLimit    int64        `yaml:"output_token_limit"`
	InputModalities     []string     `yaml:"input_modalities"`
	OutputModalities    []string     `yaml:"output_modalities"`
	Parameters          []string     `yaml:"supported_parameters"`
	GenerationMethods   []string     `yaml:"generation_methods"`
	Thinking            *rawThinking `yaml:"thinking"`
}

type rawThinking struct {
	Min            int      `yaml:"min"`
	Max            int      `yaml:"max"`
	ZeroAllowed    bool     `yaml:"zero_allowed"`
	DynamicAllowed bool     `yaml:"dynamic_allowed"`
	Levels         []string `yaml:"levels"`
}

func parseCapabilities(raw *rawCapabilities) (capabilities, error) {
	if raw == nil {
		return capabilities{}, nil
	}
	for name, value := range map[string]int64{
		"context_length":        raw.ContextLength,
		"max_completion_tokens": raw.MaxCompletionTokens,
		"input_token_limit":     raw.InputTokenLimit,
		"output_token_limit":    raw.OutputTokenLimit,
	} {
		if value < 0 {
			return capabilities{}, fmt.Errorf("capabilities.%s must not be negative", name)
		}
	}
	caps := capabilities{
		ContextLength:       raw.ContextLength,
		MaxCompletionTokens: raw.MaxCompletionTokens,
		InputTokenLimit:     raw.InputTokenLimit,
		OutputTokenLimit:    raw.OutputTokenLimit,
		InputModalities:     cleanNames(raw.InputModalities),
		OutputModalities:    cleanNames(raw.OutputModalities),
		Parameters:          cleanNames(raw.Parameters),
		GenerationMethods:   cleanNames(raw.GenerationMethods),
	}
	if thinking := raw.Thinking; thinking != nil {
		if thinking.Min < 0 || thinking.Max < 0 || thinking.Min > thinking.Max {
			return capabilities{}, fmt.Errorf("capabilities.thinking budget must satisfy 0 <= min <= max")
		}
		levels := cleanNames(thinking.Levels)
		if len(levels) > 0 || thinking.Max > 0 {
			caps.Thinking = &pluginapi.ThinkingSupport{
				Min:            thinking.Min,
				Max:            thinking.Max,
				ZeroAllowed:    thinking.ZeroAllowed,
				DynamicAllowed: thinking.DynamicAllowed,
				Levels:         levels,
			}
		}
	}
	return caps, nil
}

// cleanNames lowercases and deduplicates, because providers disagree on the
// casing of modality and level names.
func cleanNames(values []string) []string {
	var out []string
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		name := strings.ToLower(strings.TrimSpace(value))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// apply copies the announced capabilities onto the group's model entry.
func (c capabilities) apply(model *pluginapi.ModelInfo) {
	model.ContextLength = c.ContextLength
	model.MaxCompletionTokens = c.MaxCompletionTokens
	model.InputTokenLimit = c.InputTokenLimit
	model.OutputTokenLimit = c.OutputTokenLimit
	model.SupportedInputModalities = append([]string(nil), c.InputModalities...)
	model.SupportedOutputModalities = append([]string(nil), c.OutputModalities...)
	model.SupportedParameters = append([]string(nil), c.Parameters...)
	model.SupportedGenerationMethods = append([]string(nil), c.GenerationMethods...)
	if c.Thinking != nil {
		thinking := *c.Thinking
		thinking.Levels = append([]string(nil), c.Thinking.Levels...)
		model.Thinking = &thinking
	}
}
