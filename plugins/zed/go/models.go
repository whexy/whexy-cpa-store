package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// rpcAuthModelRequest is model.for_auth with the host callback ID.
type rpcAuthModelRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func handleModelsForAuth(raw []byte) ([]byte, error) {
	var req rpcAuthModelRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode model.for_auth request: %w", errUnmarshal)
	}
	cred, errCred := resolveCredential(req.StorageJSON, req.Attributes, req.Metadata)
	if errCred != nil {
		return errorEnvelope("invalid_auth", "Zed auth "+req.AuthID+": "+errCred.Error(), http.StatusUnauthorized), nil
	}
	models, errModels := catalogFor(context.Background(), newHost(req.HostCallbackID), currentSettings(), cred, false)
	if errModels != nil {
		status := http.StatusBadGateway
		var failure *zedFailure
		if errors.As(errModels, &failure) {
			status = failure.Status
		}
		return errorEnvelope("catalog_failed", fmt.Sprintf("list the Zed models of %s: %v", cred.Username, errModels), status), nil
	}
	cfg := currentSettings()
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		if model.IsDisabled || cfg.modelDisabled(model.ID) {
			continue
		}
		out = append(out, modelInfo(cfg, model))
	}
	response := pluginapi.ModelResponse{Provider: providerKey, Models: out}
	return okEnvelope(response)
}

// catalogFor returns the account's models, from cache when it is fresh.
func catalogFor(ctx context.Context, h host, cfg settings, cred credential, refresh bool) ([]zedModel, error) {
	key := llmTokens.key(cred)
	if !refresh {
		if models, ok := modelsByAuth.get(key); ok {
			return models, nil
		}
	}
	models, errFetch := fetchModels(ctx, h, cfg, cred)
	if errFetch != nil {
		var failure *zedFailure
		if errors.As(errFetch, &failure) && failure.Status == http.StatusUnauthorized {
			// The JWT may have been revoked; retry once with a fresh one.
			llmTokens.drop(key)
			models, errFetch = fetchModels(ctx, h, cfg, cred)
		}
		if errFetch != nil {
			return nil, errFetch
		}
	}
	modelsByAuth.put(key, models)
	return models, nil
}

func modelInfo(cfg settings, model zedModel) pluginapi.ModelInfo {
	info := pluginapi.ModelInfo{
		ID:                  cfg.exposedID(model.ID),
		Object:              "model",
		OwnedBy:             model.Provider,
		Type:                model.typeName(),
		DisplayName:         model.DisplayName,
		Name:                model.ID,
		Description:         "Zed-hosted " + model.DisplayName,
		ContextLength:       model.ContextLength,
		InputTokenLimit:     model.ContextLength,
		OutputTokenLimit:    model.OutputTokens,
		MaxCompletionTokens: model.OutputTokens,
	}
	if info.DisplayName == "" {
		info.DisplayName = model.ID
	}
	info.SupportedInputModalities = []string{"text"}
	if model.SupportsImages {
		info.SupportedInputModalities = append(info.SupportedInputModalities, "image")
	}
	info.SupportedOutputModalities = []string{"text"}
	info.SupportedGenerationMethods = []string{"generateContent"}
	info.SupportedParameters = []string{"tools"}
	if model.SupportsThinking {
		info.SupportedParameters = append(info.SupportedParameters, "reasoning")
		info.Thinking = &pluginapi.ThinkingSupport{
			Levels:         model.EffortLevels,
			DynamicAllowed: true,
			ZeroAllowed:    true,
			Min:            0,
			Max:            int(model.OutputTokens),
		}
	}
	return info
}

// modelRoute resolves one requested ID to the upstream model and protocol.
func modelRouteFor(ctx context.Context, h host, cfg settings, cred credential, requested string) (zedModel, error) {
	models, errCatalog := catalogFor(ctx, h, cfg, cred, false)
	if errCatalog != nil {
		return zedModel{}, errCatalog
	}
	upstream := cfg.upstreamID(requested)
	for _, model := range models {
		if strings.EqualFold(model.ID, upstream) {
			if model.IsDisabled {
				return zedModel{}, fmt.Errorf("the Zed account has disabled %s: %s", model.ID, model.DisabledReason)
			}
			if cfg.modelDisabled(model.ID) {
				return zedModel{}, fmt.Errorf("Zed model %s is disabled in the plugin settings", model.ID)
			}
			return model, nil
		}
	}
	// A miss may be a model the account gained since the cache filled.
	models, errRefresh := catalogFor(ctx, h, cfg, cred, true)
	if errRefresh != nil {
		return zedModel{}, errRefresh
	}
	for _, model := range models {
		if strings.EqualFold(model.ID, upstream) && !model.IsDisabled && !cfg.modelDisabled(model.ID) {
			return model, nil
		}
	}
	return zedModel{}, fmt.Errorf("%s is not in this Zed account's model list", requested)
}
