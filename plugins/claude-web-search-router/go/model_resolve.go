package main

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

const (
	// Default Antigravity model for Claude web_search → native googleSearch (override with
	// antigravity_model). This is the model Google's antigravity fetchAvailableModels reports in
	// webSearchModelIds, and the only model family the host's Claude→Antigravity translator
	// bridges to native googleSearch.
	defaultAntigravityWebSearchModel = "gemini-3.1-flash-lite"
	// Default Codex model for Claude web_search → Codex Responses (override with codex_model).
	defaultCodexWebSearchModel = "gpt-5.6-luna"
	// Default xAI model for server-side web_search per https://docs.x.ai/developers/tools/web-search
	defaultXAIWebSearchModel = "grok-4.3"
)

// resolveAntigravityWebSearchTargetModel picks an Antigravity model that can run native googleSearch.
// Config antigravity_model wins; otherwise registry.AntigravityWebSearchModelFor(requested) or the
// first available antigravity model with SupportsWebSearch, then the static default. The registry
// lookups are best-effort: plugins are dlopen'd with a private registry copy, so runtime-registered
// host models are invisible and the static default is what normally applies.
func resolveAntigravityWebSearchTargetModel(configured, requested string) string {
	if m := strings.TrimSpace(configured); m != "" {
		return m
	}
	if m := registry.AntigravityWebSearchModelFor(strings.TrimSpace(requested)); m != "" {
		return m
	}
	for _, model := range registry.GetGlobalRegistry().GetAvailableModelsByProvider("antigravity") {
		if model == nil || !model.SupportsWebSearch {
			continue
		}
		if id := strings.TrimSpace(model.ID); id != "" {
			return id
		}
	}
	return defaultAntigravityWebSearchModel
}

// resolveCodexWebSearchTargetModel never forwards the client Claude model to Codex.
func resolveCodexWebSearchTargetModel(configured string) string {
	if m := strings.TrimSpace(configured); m != "" {
		return m
	}
	return defaultCodexWebSearchModel
}

// resolveXAIWebSearchTargetModel never forwards the client Claude model to xAI Responses.
func resolveXAIWebSearchTargetModel(configured string) string {
	if m := strings.TrimSpace(configured); m != "" {
		return m
	}
	return defaultXAIWebSearchModel
}
