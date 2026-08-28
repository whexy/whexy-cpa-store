package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
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
	defaultDataFile   = "usage-insights.jsonl"
	defaultRecentSize = 100
	maxRecentSize     = 1000
)

type pluginConfig struct {
	Enabled             bool              `yaml:"enabled"`
	DataFile            string            `yaml:"data_file"`
	RecentRecords       int               `yaml:"recent_records"`
	IncludeAPIKey       bool              `yaml:"include_api_key"`
	PricingEnabled      bool              `yaml:"pricing_enabled"`
	PricingURL          string            `yaml:"pricing_url"`
	PricingRefreshHours int               `yaml:"pricing_refresh_hours"`
	PricingCacheFile    string            `yaml:"pricing_cache_file"`
	PricingProviderMap  map[string]string `yaml:"pricing_provider_map"`
	PricingModelMap     map[string]string `yaml:"pricing_model_map"`
}

type persistedRecord struct {
	RecordedAt        time.Time           `json:"recorded_at"`
	RequestedAt       time.Time           `json:"requested_at"`
	Provider          string              `json:"provider,omitempty"`
	ExecutorType      string              `json:"executor_type,omitempty"`
	Model             string              `json:"model,omitempty"`
	Alias             string              `json:"alias,omitempty"`
	APIKey            string              `json:"api_key,omitempty"`
	CredentialID      string              `json:"credential_id,omitempty"`
	AuthType          string              `json:"auth_type,omitempty"`
	Source            string              `json:"source,omitempty"`
	ReasoningEffort   string              `json:"reasoning_effort,omitempty"`
	ServiceTier       string              `json:"service_tier,omitempty"`
	Generate          bool                `json:"generate"`
	LatencyMS         float64             `json:"latency_ms"`
	TTFTMS            float64             `json:"ttft_ms"`
	Failed            bool                `json:"failed"`
	FailureStatusCode int                 `json:"failure_status_code,omitempty"`
	FailureBody       string              `json:"failure_body,omitempty"`
	InputTokens       int64               `json:"input_tokens"`
	UncachedInput     int64               `json:"uncached_input_tokens"`
	OutputTokens      int64               `json:"output_tokens"`
	ReasoningTokens   int64               `json:"reasoning_tokens"`
	CacheReadTokens   int64               `json:"cache_read_tokens"`
	CacheWriteTokens  int64               `json:"cache_write_tokens"`
	TotalTokens       int64               `json:"total_tokens"`
	CacheReadRatio    float64             `json:"cache_read_ratio"`
	CacheWriteRatio   float64             `json:"cache_write_ratio"`
	CacheRatio        float64             `json:"cache_ratio"`
	QuotaHeaders      map[string][]string `json:"quota_headers,omitempty"`
	APICost           *pricingEstimate    `json:"api_cost,omitempty"`
}

type summary struct {
	StartedAt           time.Time `json:"started_at,omitempty"`
	LastRequestedAt     time.Time `json:"last_requested_at,omitempty"`
	Requests            int64     `json:"requests"`
	SuccessfulRequests  int64     `json:"successful_requests"`
	FailedRequests      int64     `json:"failed_requests"`
	InputTokens         int64     `json:"input_tokens"`
	UncachedInput       int64     `json:"uncached_input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	ReasoningTokens     int64     `json:"reasoning_tokens"`
	CacheReadTokens     int64     `json:"cache_read_tokens"`
	CacheWriteTokens    int64     `json:"cache_write_tokens"`
	TotalTokens         int64     `json:"total_tokens"`
	PricedRequests      int64     `json:"priced_requests"`
	UnpricedRequests    int64     `json:"unpriced_requests"`
	EstimatedAPICostUSD float64   `json:"estimated_api_cost_usd"`
	CacheReadRatio      float64   `json:"cache_read_ratio"`
	CacheWriteRatio     float64   `json:"cache_write_ratio"`
	CacheRatio          float64   `json:"cache_ratio"`
	AverageLatencyMS    float64   `json:"average_latency_ms"`
	AverageTTFTMS       float64   `json:"average_ttft_ms"`
}

type groupSummary struct {
	Key string `json:"key"`
	summary
}

type snapshot struct {
	GeneratedAt  time.Time         `json:"generated_at"`
	DataFile     string            `json:"data_file"`
	Pricing      pricingStatus     `json:"pricing"`
	Overall      summary           `json:"overall"`
	ByProvider   []groupSummary    `json:"by_provider"`
	ByModel      []groupSummary    `json:"by_model"`
	ByCredential []groupSummary    `json:"by_credential"`
	Recent       []persistedRecord `json:"recent"`
}

type modelUsagePeriod struct {
	From         time.Time `json:"from"`
	To           time.Time `json:"to"`
	EndExclusive bool      `json:"end_exclusive"`
}

type modelUsageRow struct {
	CredentialID     string  `json:"credential_id"`
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	APICalls         int64   `json:"api_calls"`
	FailedAPICalls   int64   `json:"failed_api_calls"`
	InputTokens      int64   `json:"input_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CacheHitRate     float64 `json:"cache_hit_rate"`
	CostUSD          float64 `json:"cost_usd"`
	PricedAPICalls   int64   `json:"priced_api_calls"`
	UnpricedAPICalls int64   `json:"unpriced_api_calls"`
}

type modelUsageReport struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Period      modelUsagePeriod `json:"period"`
	Totals      modelUsageRow    `json:"totals"`
	Models      []modelUsageRow  `json:"models"`
}

type modelUsageKey struct {
	CredentialID string
	Provider     string
	Model        string
}

type aggregate struct {
	summary
	latencyTotal float64
	ttftTotal    float64
	ttftCount    int64
}

type usageStore struct {
	mu            sync.RWMutex
	path          string
	includeAPIKey bool
	recentLimit   int
	overall       aggregate
	byProvider    map[string]*aggregate
	byModel       map[string]*aggregate
	byCredential  map[string]*aggregate
	recent        []persistedRecord
	file          *os.File
	writer        *bufio.Writer
	pricing       *pricingManager
}

var activeStore atomic.Pointer[usageStore]

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:             true,
		DataFile:            defaultDataFile,
		RecentRecords:       defaultRecentSize,
		PricingEnabled:      true,
		PricingURL:          defaultPricingURL,
		PricingRefreshHours: defaultPricingRefreshHours,
	}
}

func normalizePluginConfig(cfg pluginConfig) pluginConfig {
	cfg.DataFile = strings.TrimSpace(cfg.DataFile)
	if cfg.DataFile == "" {
		cfg.DataFile = defaultDataFile
	}
	if cfg.RecentRecords < 0 {
		cfg.RecentRecords = 0
	}
	if cfg.RecentRecords > maxRecentSize {
		cfg.RecentRecords = maxRecentSize
	}
	return cfg
}

func newUsageStore(cfg pluginConfig) (*usageStore, error) {
	cfg = normalizePluginConfig(cfg)
	path, errPath := filepath.Abs(cfg.DataFile)
	if errPath != nil {
		return nil, fmt.Errorf("resolve data file: %w", errPath)
	}
	store := &usageStore{
		path:          path,
		includeAPIKey: cfg.IncludeAPIKey,
		recentLimit:   cfg.RecentRecords,
		byProvider:    make(map[string]*aggregate),
		byModel:       make(map[string]*aggregate),
		byCredential:  make(map[string]*aggregate),
	}
	if errLoad := store.load(); errLoad != nil {
		return nil, errLoad
	}
	if errOpen := store.open(); errOpen != nil {
		return nil, errOpen
	}
	store.pricing = newPricingManager(cfg, path)
	return store, nil
}

func (s *usageStore) load() error {
	file, errOpen := os.Open(s.path)
	if errors.Is(errOpen, os.ErrNotExist) {
		return nil
	}
	if errOpen != nil {
		return fmt.Errorf("open existing usage data: %w", errOpen)
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 4*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		var record persistedRecord
		if errDecode := json.Unmarshal(scanner.Bytes(), &record); errDecode != nil {
			return fmt.Errorf("decode usage data line %d: %w", line, errDecode)
		}
		s.addLocked(record)
	}
	if errScan := scanner.Err(); errScan != nil {
		return fmt.Errorf("read usage data: %w", errScan)
	}
	return nil
}

func (s *usageStore) open() error {
	if errMkdir := os.MkdirAll(filepath.Dir(s.path), 0o755); errMkdir != nil {
		return fmt.Errorf("create usage data directory: %w", errMkdir)
	}
	file, errOpen := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if errOpen != nil {
		return fmt.Errorf("open usage data for append: %w", errOpen)
	}
	s.file = file
	s.writer = bufio.NewWriterSize(file, 64*1024)
	return nil
}

func (s *usageStore) close() error {
	if s == nil {
		return nil
	}
	if s.pricing != nil {
		s.pricing.close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	if s.writer != nil {
		if errFlush := s.writer.Flush(); errFlush != nil {
			errs = append(errs, errFlush)
		}
		s.writer = nil
	}
	if s.file != nil {
		if errClose := s.file.Close(); errClose != nil {
			errs = append(errs, errClose)
		}
		s.file = nil
	}
	return errors.Join(errs...)
}

func (s *usageStore) append(record pluginapi.UsageRecord) error {
	if s == nil {
		return nil
	}
	persisted := normalizeUsageRecord(record, s.includeAPIKey)
	if s.pricing != nil {
		persisted.APICost = s.pricing.estimate(record, persisted)
	}
	raw, errMarshal := json.Marshal(persisted)
	if errMarshal != nil {
		return fmt.Errorf("marshal usage record: %w", errMarshal)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer == nil {
		return errors.New("usage data file is closed")
	}
	if _, errWrite := s.writer.Write(raw); errWrite != nil {
		return fmt.Errorf("write usage record: %w", errWrite)
	}
	if errWriteByte := s.writer.WriteByte('\n'); errWriteByte != nil {
		return fmt.Errorf("terminate usage record: %w", errWriteByte)
	}
	if errFlush := s.writer.Flush(); errFlush != nil {
		return fmt.Errorf("flush usage record: %w", errFlush)
	}
	s.addLocked(persisted)
	return nil
}

func (s *usageStore) addLocked(record persistedRecord) {
	addAggregate(&s.overall, record)
	addAggregate(groupAggregate(s.byProvider, record.Provider), record)
	addAggregate(groupAggregate(s.byModel, record.Model), record)
	addAggregate(groupAggregate(s.byCredential, credentialKey(record)), record)
	if s.recentLimit > 0 {
		s.recent = append(s.recent, record)
		if len(s.recent) > s.recentLimit {
			s.recent = append([]persistedRecord(nil), s.recent[len(s.recent)-s.recentLimit:]...)
		}
	}
}

func groupAggregate(groups map[string]*aggregate, key string) *aggregate {
	key = strings.TrimSpace(key)
	if key == "" {
		key = "unknown"
	}
	item := groups[key]
	if item == nil {
		item = &aggregate{}
		groups[key] = item
	}
	return item
}

func addAggregate(item *aggregate, record persistedRecord) {
	if item == nil {
		return
	}
	item.Requests++
	if record.Failed {
		item.FailedRequests++
	} else {
		item.SuccessfulRequests++
	}
	item.InputTokens += record.InputTokens
	item.UncachedInput += record.UncachedInput
	item.OutputTokens += record.OutputTokens
	item.ReasoningTokens += record.ReasoningTokens
	item.CacheReadTokens += record.CacheReadTokens
	item.CacheWriteTokens += record.CacheWriteTokens
	item.TotalTokens += record.TotalTokens
	if record.APICost != nil {
		item.PricedRequests++
		item.EstimatedAPICostUSD += record.APICost.TotalCostUSD
	} else {
		item.UnpricedRequests++
	}
	item.latencyTotal += record.LatencyMS
	if record.TTFTMS > 0 {
		item.ttftTotal += record.TTFTMS
		item.ttftCount++
	}
	requestedAt := record.RequestedAt
	if requestedAt.IsZero() {
		requestedAt = record.RecordedAt
	}
	if item.StartedAt.IsZero() || requestedAt.Before(item.StartedAt) {
		item.StartedAt = requestedAt
	}
	if requestedAt.After(item.LastRequestedAt) {
		item.LastRequestedAt = requestedAt
	}
}

func (s *usageStore) snapshot() snapshot {
	if s == nil {
		return snapshot{GeneratedAt: time.Now().UTC()}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return snapshot{
		GeneratedAt:  time.Now().UTC(),
		DataFile:     s.path,
		Pricing:      pricingStatusFor(s.pricing),
		Overall:      finalizedSummary(s.overall),
		ByProvider:   groupSummaries(s.byProvider),
		ByModel:      groupSummaries(s.byModel),
		ByCredential: groupSummaries(s.byCredential),
		Recent:       reverseRecords(s.recent),
	}
}

func (s *usageStore) modelUsage(from, to time.Time) (modelUsageReport, error) {
	report := modelUsageReport{
		GeneratedAt: time.Now().UTC(),
		Period: modelUsagePeriod{
			From:         from.UTC(),
			To:           to.UTC(),
			EndExclusive: true,
		},
	}
	if s == nil {
		return report, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	file, errOpen := os.Open(s.path)
	if errOpen != nil {
		return report, fmt.Errorf("open usage data for report: %w", errOpen)
	}
	defer func() { _ = file.Close() }()

	var totals aggregate
	groups := make(map[modelUsageKey]*aggregate)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		var record persistedRecord
		if errDecode := json.Unmarshal(scanner.Bytes(), &record); errDecode != nil {
			return report, fmt.Errorf("decode usage data line %d: %w", line, errDecode)
		}
		requestedAt := recordRequestedAt(record)
		if requestedAt.Before(from) || !requestedAt.Before(to) {
			continue
		}
		addAggregate(&totals, record)
		key := modelUsageKey{
			CredentialID: normalizedGroupKey(record.CredentialID),
			Provider:     normalizedGroupKey(record.Provider),
			Model:        normalizedGroupKey(record.Model),
		}
		item := groups[key]
		if item == nil {
			item = &aggregate{}
			groups[key] = item
		}
		addAggregate(item, record)
	}
	if errScan := scanner.Err(); errScan != nil {
		return report, fmt.Errorf("read usage data for report: %w", errScan)
	}

	report.Totals = modelUsageRowFromAggregate("", "", "", totals)
	report.Models = make([]modelUsageRow, 0, len(groups))
	for key, item := range groups {
		report.Models = append(report.Models, modelUsageRowFromAggregate(key.CredentialID, key.Provider, key.Model, *item))
	}
	sort.Slice(report.Models, func(i, j int) bool {
		if report.Models[i].TotalTokens != report.Models[j].TotalTokens {
			return report.Models[i].TotalTokens > report.Models[j].TotalTokens
		}
		if report.Models[i].APICalls != report.Models[j].APICalls {
			return report.Models[i].APICalls > report.Models[j].APICalls
		}
		if report.Models[i].Provider != report.Models[j].Provider {
			return report.Models[i].Provider < report.Models[j].Provider
		}
		if report.Models[i].CredentialID != report.Models[j].CredentialID {
			return report.Models[i].CredentialID < report.Models[j].CredentialID
		}
		return report.Models[i].Model < report.Models[j].Model
	})
	return report, nil
}

func modelUsageRowFromAggregate(credentialID, provider, model string, item aggregate) modelUsageRow {
	result := finalizedSummary(item)
	return modelUsageRow{
		CredentialID:     credentialID,
		Provider:         provider,
		Model:            model,
		APICalls:         result.Requests,
		FailedAPICalls:   result.FailedRequests,
		InputTokens:      result.UncachedInput,
		CacheReadTokens:  result.CacheReadTokens,
		CacheWriteTokens: result.CacheWriteTokens,
		OutputTokens:     result.OutputTokens,
		TotalTokens:      result.TotalTokens,
		CacheHitRate:     result.CacheReadRatio,
		CostUSD:          result.EstimatedAPICostUSD,
		PricedAPICalls:   result.PricedRequests,
		UnpricedAPICalls: result.UnpricedRequests,
	}
}

func recordRequestedAt(record persistedRecord) time.Time {
	if !record.RequestedAt.IsZero() {
		return record.RequestedAt
	}
	return record.RecordedAt
}

func normalizedGroupKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return value
}

func groupSummaries(groups map[string]*aggregate) []groupSummary {
	out := make([]groupSummary, 0, len(groups))
	for key, item := range groups {
		out = append(out, groupSummary{Key: key, summary: finalizedSummary(*item)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func finalizedSummary(item aggregate) summary {
	out := item.summary
	inputTotal := out.UncachedInput + out.CacheReadTokens + out.CacheWriteTokens
	if inputTotal > 0 {
		out.CacheReadRatio = ratio(out.CacheReadTokens, inputTotal)
		out.CacheWriteRatio = ratio(out.CacheWriteTokens, inputTotal)
		out.CacheRatio = ratio(out.CacheReadTokens+out.CacheWriteTokens, inputTotal)
	}
	if out.Requests > 0 {
		out.AverageLatencyMS = item.latencyTotal / float64(out.Requests)
	}
	if item.ttftCount > 0 {
		out.AverageTTFTMS = item.ttftTotal / float64(item.ttftCount)
	}
	return out
}

func reverseRecords(records []persistedRecord) []persistedRecord {
	out := make([]persistedRecord, len(records))
	for index := range records {
		out[index] = records[len(records)-1-index]
	}
	return out
}

func normalizeUsageRecord(record pluginapi.UsageRecord, includeAPIKey bool) persistedRecord {
	cacheRead := nonNegative(record.Detail.CacheReadTokens)
	if cacheRead == 0 && cachedTokensAreCacheReads(record.Provider, record.ExecutorType) {
		cacheRead = nonNegative(record.Detail.CachedTokens)
	}
	cacheWrite := nonNegative(record.Detail.CacheCreationTokens)
	input := nonNegative(record.Detail.InputTokens)
	uncachedInput := input
	if providerUsesIndependentCache(record.Provider, record.ExecutorType) {
		// Anthropic-style usage reports uncached input separately from cache reads and writes.
	} else {
		uncachedInput = input - min64(input, cacheRead+cacheWrite)
	}
	output := nonNegative(record.Detail.OutputTokens)
	reasoning := nonNegative(record.Detail.ReasoningTokens)
	total := nonNegative(record.Detail.TotalTokens)
	if total == 0 {
		total = uncachedInput + cacheRead + cacheWrite + output
		if reasoning > output {
			total += reasoning - output
		}
	}
	inputTotal := uncachedInput + cacheRead + cacheWrite
	persisted := persistedRecord{
		RecordedAt:        time.Now().UTC(),
		RequestedAt:       record.RequestedAt.UTC(),
		Provider:          strings.TrimSpace(record.Provider),
		ExecutorType:      strings.TrimSpace(record.ExecutorType),
		Model:             strings.TrimSpace(record.Model),
		Alias:             strings.TrimSpace(record.Alias),
		CredentialID:      credentialIdentifier(record),
		AuthType:          strings.TrimSpace(record.AuthType),
		Source:            strings.TrimSpace(record.Source),
		ReasoningEffort:   strings.TrimSpace(record.ReasoningEffort),
		ServiceTier:       strings.TrimSpace(record.ServiceTier),
		Generate:          record.Generate,
		LatencyMS:         durationMilliseconds(record.Latency),
		TTFTMS:            durationMilliseconds(record.TTFT),
		Failed:            record.Failed,
		FailureStatusCode: record.Failure.StatusCode,
		FailureBody:       strings.TrimSpace(record.Failure.Body),
		InputTokens:       input,
		UncachedInput:     uncachedInput,
		OutputTokens:      output,
		ReasoningTokens:   reasoning,
		CacheReadTokens:   cacheRead,
		CacheWriteTokens:  cacheWrite,
		TotalTokens:       total,
		QuotaHeaders:      quotaHeaders(record.ResponseHeaders),
	}
	if includeAPIKey {
		persisted.APIKey = strings.TrimSpace(record.APIKey)
	}
	if inputTotal > 0 {
		persisted.CacheReadRatio = ratio(cacheRead, inputTotal)
		persisted.CacheWriteRatio = ratio(cacheWrite, inputTotal)
		persisted.CacheRatio = ratio(cacheRead+cacheWrite, inputTotal)
	}
	return persisted
}

func providerUsesIndependentCache(provider, executorType string) bool {
	value := strings.ToLower(strings.TrimSpace(provider + " " + executorType))
	return strings.Contains(value, "claude") || strings.Contains(value, "anthropic")
}

func cachedTokensAreCacheReads(provider, executorType string) bool {
	value := strings.ToLower(strings.TrimSpace(provider + " " + executorType))
	for _, marker := range []string{
		"openai", "codex", "xai", "grok", "kimi", "qwen", "deepseek", "openrouter",
		"gemini", "aistudio", "antigravity", "vertex", "interaction", "claude", "anthropic",
	} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func credentialIdentifier(record pluginapi.UsageRecord) string {
	for _, value := range []string{record.AuthIndex, record.AuthID} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func credentialKey(record persistedRecord) string {
	if value := strings.TrimSpace(record.CredentialID); value != "" {
		return value
	}
	if record.APIKey != "" {
		return "api-key:" + record.APIKey
	}
	return "unknown"
}

func durationMilliseconds(value time.Duration) float64 {
	if value <= 0 {
		return 0
	}
	return float64(value) / float64(time.Millisecond)
}

func ratio(part, whole int64) float64 {
	if part <= 0 || whole <= 0 {
		return 0
	}
	return float64(part) / float64(whole)
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func min64(first, second int64) int64 {
	if first < second {
		return first
	}
	return second
}

func quotaHeaders(headers map[string][]string) map[string][]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string][]string)
	for key, values := range headers {
		lower := strings.ToLower(strings.TrimSpace(key))
		if lower != "retry-after" && !strings.HasPrefix(lower, "anthropic-ratelimit-") &&
			!strings.HasPrefix(lower, "x-codex-") && !strings.HasPrefix(lower, "x-ratelimit-") {
			continue
		}
		cloned := make([]string, len(values))
		copy(cloned, values)
		out[key] = cloned
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
