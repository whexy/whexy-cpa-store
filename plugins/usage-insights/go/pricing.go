package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	defaultPricingURL          = "https://models.dev/api.json"
	defaultPricingRefreshHours = 6
	maxPricingResponseBytes    = 32 * 1024 * 1024
	pricingRequestTimeout      = 20 * time.Second
)

type modelsDevProvider struct {
	Name   string                    `json:"name"`
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	LastUpdated string         `json:"last_updated"`
	Cost        *modelsDevCost `json:"cost"`
}

type modelsDevUnitCost struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	Reasoning  *float64 `json:"reasoning"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

type modelsDevCost struct {
	modelsDevUnitCost
	Tiers           []modelsDevCostTier `json:"tiers"`
	ContextOver200K *modelsDevUnitCost  `json:"context_over_200k"`
}

type modelsDevCostTier struct {
	modelsDevUnitCost
	Tier modelsDevTier `json:"tier"`
}

type modelsDevTier struct {
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type pricingCatalog struct {
	FetchedAt time.Time
	Providers map[string]modelsDevProvider
}

type pricingStatus struct {
	Enabled       bool      `json:"enabled"`
	SourceURL     string    `json:"source_url,omitempty"`
	CacheFile     string    `json:"cache_file,omitempty"`
	FetchedAt     time.Time `json:"fetched_at,omitempty"`
	ProviderCount int       `json:"provider_count"`
	ModelCount    int       `json:"model_count"`
	LastError     string    `json:"last_error,omitempty"`
}

type pricingEstimate struct {
	Source                  string    `json:"source"`
	FetchedAt               time.Time `json:"fetched_at"`
	Provider                string    `json:"provider"`
	Model                   string    `json:"model"`
	ModelName               string    `json:"model_name,omitempty"`
	ModelLastUpdated        string    `json:"model_last_updated,omitempty"`
	TierContextTokens       int64     `json:"tier_context_tokens,omitempty"`
	InputPerMillionUSD      float64   `json:"input_per_million_usd"`
	OutputPerMillionUSD     float64   `json:"output_per_million_usd"`
	ReasoningPerMillionUSD  float64   `json:"reasoning_per_million_usd"`
	CacheReadPerMillionUSD  float64   `json:"cache_read_per_million_usd"`
	CacheWritePerMillionUSD float64   `json:"cache_write_per_million_usd"`
	InputCostUSD            float64   `json:"input_cost_usd"`
	OutputCostUSD           float64   `json:"output_cost_usd"`
	ReasoningCostUSD        float64   `json:"reasoning_cost_usd"`
	CacheReadCostUSD        float64   `json:"cache_read_cost_usd"`
	CacheWriteCostUSD       float64   `json:"cache_write_cost_usd"`
	TotalCostUSD            float64   `json:"total_cost_usd"`
}

type pricingManager struct {
	enabled       bool
	url           string
	cacheFile     string
	providerMap   map[string]string
	modelMap      map[string]string
	refreshPeriod time.Duration
	client        *http.Client
	catalog       atomic.Pointer[pricingCatalog]
	mu            sync.RWMutex
	refreshMu     sync.Mutex
	lastError     string
	cancel        context.CancelFunc
	wait          sync.WaitGroup
}

func newPricingManager(cfg pluginConfig, dataFile string) *pricingManager {
	if !cfg.PricingEnabled {
		return &pricingManager{}
	}
	cacheFile := strings.TrimSpace(cfg.PricingCacheFile)
	if cacheFile == "" {
		cacheFile = dataFile + ".models-dev.json"
	}
	if absolute, errPath := filepath.Abs(cacheFile); errPath == nil {
		cacheFile = absolute
	}
	refreshHours := cfg.PricingRefreshHours
	if refreshHours <= 0 {
		refreshHours = defaultPricingRefreshHours
	}
	manager := &pricingManager{
		enabled:       true,
		url:           strings.TrimSpace(cfg.PricingURL),
		cacheFile:     cacheFile,
		providerMap:   normalizedStringMap(cfg.PricingProviderMap),
		modelMap:      normalizedStringMap(cfg.PricingModelMap),
		refreshPeriod: time.Duration(refreshHours) * time.Hour,
		client:        &http.Client{Timeout: pricingRequestTimeout},
	}
	if manager.url == "" {
		manager.url = defaultPricingURL
	}
	if catalog, errLoad := loadPricingCatalog(cacheFile); errLoad == nil {
		manager.catalog.Store(catalog)
	} else if !errors.Is(errLoad, os.ErrNotExist) {
		manager.setError(errLoad)
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager.cancel = cancel
	manager.wait.Add(1)
	go manager.refreshLoop(ctx)
	manager.wait.Add(1)
	go func() {
		defer manager.wait.Done()
		if errRefresh := manager.refresh(ctx); errRefresh != nil && !errors.Is(errRefresh, context.Canceled) {
			manager.setError(errRefresh)
		}
	}()
	return manager
}

func (m *pricingManager) close() {
	if m == nil || m.cancel == nil {
		return
	}
	m.cancel()
	m.wait.Wait()
}

func (m *pricingManager) refreshLoop(ctx context.Context) {
	defer m.wait.Done()
	ticker := time.NewTicker(m.refreshPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if errRefresh := m.refresh(ctx); errRefresh != nil {
				m.setError(errRefresh)
			}
		}
	}
}

func (m *pricingManager) refresh(ctx context.Context) error {
	if m == nil || !m.enabled {
		return nil
	}
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, m.url, nil)
	if errRequest != nil {
		return fmt.Errorf("create models.dev request: %w", errRequest)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "whexy-cpa-store-usage-insights/"+pluginVersion)
	response, errDo := m.client.Do(request)
	if errDo != nil {
		return fmt.Errorf("fetch models.dev pricing: %w", errDo)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("fetch models.dev pricing: HTTP %d", response.StatusCode)
	}
	raw, errRead := io.ReadAll(io.LimitReader(response.Body, maxPricingResponseBytes+1))
	if errRead != nil {
		return fmt.Errorf("read models.dev pricing: %w", errRead)
	}
	if len(raw) > maxPricingResponseBytes {
		return fmt.Errorf("models.dev pricing response exceeds %d bytes", maxPricingResponseBytes)
	}
	catalog, errDecode := decodePricingCatalog(raw, time.Now().UTC())
	if errDecode != nil {
		return errDecode
	}
	if errCache := writePricingCache(m.cacheFile, raw); errCache != nil {
		return errCache
	}
	m.catalog.Store(catalog)
	m.mu.Lock()
	m.lastError = ""
	m.mu.Unlock()
	return nil
}

func decodePricingCatalog(raw []byte, fetchedAt time.Time) (*pricingCatalog, error) {
	var providers map[string]modelsDevProvider
	if errDecode := json.Unmarshal(raw, &providers); errDecode != nil {
		return nil, fmt.Errorf("decode models.dev pricing: %w", errDecode)
	}
	if len(providers) == 0 {
		return nil, errors.New("models.dev pricing catalog is empty")
	}
	modelCount := 0
	for _, provider := range providers {
		modelCount += len(provider.Models)
	}
	if modelCount == 0 {
		return nil, errors.New("models.dev pricing catalog contains no models")
	}
	return &pricingCatalog{FetchedAt: fetchedAt, Providers: providers}, nil
}

func loadPricingCatalog(path string) (*pricingCatalog, error) {
	info, errStat := os.Stat(path)
	if errStat != nil {
		return nil, errStat
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, fmt.Errorf("read pricing cache: %w", errRead)
	}
	return decodePricingCatalog(raw, info.ModTime().UTC())
}

func writePricingCache(path string, raw []byte) error {
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o755); errMkdir != nil {
		return fmt.Errorf("create pricing cache directory: %w", errMkdir)
	}
	temporary := path + ".tmp"
	if errWrite := os.WriteFile(temporary, raw, 0o600); errWrite != nil {
		return fmt.Errorf("write pricing cache: %w", errWrite)
	}
	if errRename := os.Rename(temporary, path); errRename != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("replace pricing cache: %w", errRename)
	}
	return nil
}

func (m *pricingManager) estimate(record pluginapi.UsageRecord, usage persistedRecord) *pricingEstimate {
	if m == nil || !m.enabled {
		return nil
	}
	catalog := m.catalog.Load()
	if catalog == nil {
		return nil
	}
	providerID, modelID, model, okLookup := m.lookup(catalog, record.Provider, record.Model, record.Alias)
	if !okLookup || model.Cost == nil {
		return nil
	}
	unit, tierSize := selectUnitCost(*model.Cost, usage.UncachedInput+usage.CacheReadTokens+usage.CacheWriteTokens)
	inputRate, okInput := unitValue(unit.Input)
	outputRate, okOutput := unitValue(unit.Output)
	if !okInput && !okOutput {
		return nil
	}
	cacheReadRate := inputRate
	if value, okValue := unitValue(unit.CacheRead); okValue {
		cacheReadRate = value
	}
	cacheWriteRate := inputRate
	if value, okValue := unitValue(unit.CacheWrite); okValue {
		cacheWriteRate = value
	}
	reasoningRate := outputRate
	hasReasoningRate := false
	if value, okValue := unitValue(unit.Reasoning); okValue {
		reasoningRate = value
		hasReasoningRate = true
	}
	nonReasoningOutput := usage.OutputTokens
	reasoningTokens := int64(0)
	if hasReasoningRate {
		reasoningTokens = min64(usage.OutputTokens, usage.ReasoningTokens)
		nonReasoningOutput -= reasoningTokens
	}
	estimate := &pricingEstimate{
		Source:                  m.url,
		FetchedAt:               catalog.FetchedAt,
		Provider:                providerID,
		Model:                   modelID,
		ModelName:               model.Name,
		ModelLastUpdated:        model.LastUpdated,
		TierContextTokens:       tierSize,
		InputPerMillionUSD:      inputRate,
		OutputPerMillionUSD:     outputRate,
		ReasoningPerMillionUSD:  reasoningRate,
		CacheReadPerMillionUSD:  cacheReadRate,
		CacheWritePerMillionUSD: cacheWriteRate,
		InputCostUSD:            tokenCost(usage.UncachedInput, inputRate),
		OutputCostUSD:           tokenCost(nonReasoningOutput, outputRate),
		ReasoningCostUSD:        tokenCost(reasoningTokens, reasoningRate),
		CacheReadCostUSD:        tokenCost(usage.CacheReadTokens, cacheReadRate),
		CacheWriteCostUSD:       tokenCost(usage.CacheWriteTokens, cacheWriteRate),
	}
	estimate.TotalCostUSD = estimate.InputCostUSD + estimate.OutputCostUSD + estimate.ReasoningCostUSD + estimate.CacheReadCostUSD + estimate.CacheWriteCostUSD
	return estimate
}

func (m *pricingManager) lookup(catalog *pricingCatalog, provider, model, alias string) (string, string, modelsDevModel, bool) {
	providerID := m.providerID(provider)
	candidates := modelCandidates(model, alias)
	if providerID == "" {
		for _, candidate := range candidates {
			if mappedProvider, mappedModel, okMapped := strings.Cut(candidate, "/"); okMapped {
				mappedProvider = strings.ToLower(strings.TrimSpace(mappedProvider))
				if _, exists := catalog.Providers[mappedProvider]; exists {
					providerID = mappedProvider
					candidates = append([]string{strings.TrimSpace(mappedModel)}, candidates...)
					break
				}
			}
		}
	}
	for _, key := range []string{
		strings.ToLower(strings.TrimSpace(provider) + "/" + strings.TrimSpace(model)),
		strings.ToLower(strings.TrimSpace(providerID) + "/" + strings.TrimSpace(model)),
		strings.ToLower(strings.TrimSpace(model)),
	} {
		mapped := strings.TrimSpace(m.modelMap[key])
		if mapped == "" {
			continue
		}
		if mappedProvider, mappedModel, okMapped := splitMappedModel(catalog, mapped); okMapped {
			providerID = mappedProvider
			candidates = append([]string{mappedModel}, candidates...)
		} else {
			candidates = append([]string{mapped}, candidates...)
		}
		break
	}
	providerEntry, okProvider := catalog.Providers[providerID]
	if !okProvider {
		return "", "", modelsDevModel{}, false
	}
	for _, candidate := range candidates {
		if matchedID, matched, okModel := lookupModel(providerEntry.Models, candidate); okModel {
			return providerID, matchedID, matched, true
		}
	}
	return "", "", modelsDevModel{}, false
}

func (m *pricingManager) providerID(provider string) string {
	normalized := strings.ToLower(strings.TrimSpace(provider))
	for _, suffix := range []string{"executor", "-executor", "_executor"} {
		normalized = strings.TrimSuffix(normalized, suffix)
	}
	normalized = strings.Trim(normalized, "-_")
	if mapped := strings.TrimSpace(m.providerMap[normalized]); mapped != "" {
		return strings.ToLower(mapped)
	}
	switch normalized {
	case "openai", "codex", "openaicompat":
		return "openai"
	case "claude", "anthropic":
		return "anthropic"
	case "gemini", "aistudio", "antigravity", "google":
		return "google"
	case "vertex", "google-vertex":
		return "google-vertex"
	case "xai", "grok":
		return "xai"
	case "bedrock", "amazon-bedrock":
		return "amazon-bedrock"
	default:
		return normalized
	}
}

func splitMappedModel(catalog *pricingCatalog, mapped string) (string, string, bool) {
	mapped = strings.TrimSpace(mapped)
	if catalog == nil || mapped == "" {
		return "", "", false
	}
	providers := make([]string, 0, len(catalog.Providers))
	for provider := range catalog.Providers {
		providers = append(providers, provider)
	}
	sort.Slice(providers, func(i, j int) bool { return len(providers[i]) > len(providers[j]) })
	lower := strings.ToLower(mapped)
	for _, provider := range providers {
		prefix := strings.ToLower(provider) + "/"
		if strings.HasPrefix(lower, prefix) {
			return provider, strings.TrimSpace(mapped[len(prefix):]), true
		}
	}
	return "", "", false
}

func modelCandidates(model, alias string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, 8)
	for _, value := range []string{model, alias} {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		candidates := []string{value, strings.TrimPrefix(value, "models/")}
		if slash := strings.IndexByte(value, '/'); slash >= 0 && slash+1 < len(value) {
			candidates = append(candidates, value[slash+1:])
		}
		for _, candidate := range candidates {
			key := strings.ToLower(strings.TrimSpace(candidate))
			if key == "" {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, candidate)
		}
	}
	return out
}

func lookupModel(models map[string]modelsDevModel, candidate string) (string, modelsDevModel, bool) {
	if model, exists := models[candidate]; exists {
		return candidate, model, true
	}
	candidate = strings.ToLower(strings.TrimSpace(candidate))
	for id, model := range models {
		if strings.ToLower(id) == candidate || strings.ToLower(strings.TrimSpace(model.ID)) == candidate {
			return id, model, true
		}
	}
	return "", modelsDevModel{}, false
}

func selectUnitCost(cost modelsDevCost, inputTokens int64) (modelsDevUnitCost, int64) {
	selected := cost.modelsDevUnitCost
	selectedSize := int64(0)
	tiers := append([]modelsDevCostTier(nil), cost.Tiers...)
	sort.Slice(tiers, func(i, j int) bool { return tiers[i].Tier.Size < tiers[j].Tier.Size })
	for _, tier := range tiers {
		if !strings.EqualFold(strings.TrimSpace(tier.Tier.Type), "context") || tier.Tier.Size <= 0 || inputTokens <= tier.Tier.Size {
			continue
		}
		selected = mergeUnitCost(selected, tier.modelsDevUnitCost)
		selectedSize = tier.Tier.Size
	}
	if selectedSize == 0 && inputTokens > 200_000 && cost.ContextOver200K != nil {
		selected = mergeUnitCost(selected, *cost.ContextOver200K)
		selectedSize = 200_000
	}
	return selected, selectedSize
}

func mergeUnitCost(base, override modelsDevUnitCost) modelsDevUnitCost {
	if override.Input != nil {
		base.Input = override.Input
	}
	if override.Output != nil {
		base.Output = override.Output
	}
	if override.Reasoning != nil {
		base.Reasoning = override.Reasoning
	}
	if override.CacheRead != nil {
		base.CacheRead = override.CacheRead
	}
	if override.CacheWrite != nil {
		base.CacheWrite = override.CacheWrite
	}
	return base
}

func unitValue(value *float64) (float64, bool) {
	if value == nil || *value < 0 {
		return 0, false
	}
	return *value, true
}

func tokenCost(tokens int64, perMillion float64) float64 {
	if tokens <= 0 || perMillion <= 0 {
		return 0
	}
	return float64(tokens) * perMillion / 1_000_000
}

func normalizedStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if key != "" && value != "" {
			out[key] = value
		}
	}
	return out
}

func (m *pricingManager) setError(err error) {
	if m == nil || err == nil {
		return
	}
	m.mu.Lock()
	m.lastError = err.Error()
	m.mu.Unlock()
}

func pricingStatusFor(manager *pricingManager) pricingStatus {
	if manager == nil {
		return pricingStatus{}
	}
	return manager.status()
}

func (m *pricingManager) status() pricingStatus {
	if m == nil || !m.enabled {
		return pricingStatus{}
	}
	status := pricingStatus{Enabled: true, SourceURL: m.url, CacheFile: m.cacheFile}
	if catalog := m.catalog.Load(); catalog != nil {
		status.FetchedAt = catalog.FetchedAt
		status.ProviderCount = len(catalog.Providers)
		for _, provider := range catalog.Providers {
			status.ModelCount += len(provider.Models)
		}
	}
	m.mu.RLock()
	status.LastError = m.lastError
	m.mu.RUnlock()
	return status
}
