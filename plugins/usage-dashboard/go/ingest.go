package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// counters are mutually exclusive token buckets: Input excludes cache reads
// and writes, and Output includes Reasoning.
type counters struct {
	Requests   int64 `json:"requests"`
	Failed     int64 `json:"failed,omitempty"`
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning,omitempty"`
}

func (c *counters) add(other counters) {
	c.Requests += other.Requests
	c.Failed += other.Failed
	c.Input += other.Input
	c.CacheRead += other.CacheRead
	c.CacheWrite += other.CacheWrite
	c.Output += other.Output
	c.Reasoning += other.Reasoning
}

// processed counts every token a model handled, the figure the page leads
// with.
func (c counters) processed() int64 {
	return c.Input + c.CacheRead + c.CacheWrite + c.Output
}

type tokenSemantics int

const (
	semanticsUnknown tokenSemantics = iota
	// semanticsSubset: input includes cache tokens, output includes reasoning.
	semanticsSubset
	// semanticsIndependent: input, cache reads, cache writes, output and
	// reasoning are all separate.
	semanticsIndependent
	// semanticsSeparateReasoning: input includes cache tokens, reasoning is
	// separate from output.
	semanticsSeparateReasoning
)

// semanticsFor mirrors tokenAccountingSemanticsFor in CLIProxyAPI v8.0.27
// (sdk/cliproxy/usage/accounting.go), which plugins do not receive.
func semanticsFor(provider, executorType string) tokenSemantics {
	provider = strings.ToLower(strings.TrimSpace(provider))
	executorType = strings.ToLower(strings.TrimSpace(executorType))
	value := strings.TrimSpace(provider + " " + executorType)
	if value == "" || value == "unknown" || value == "unknown unknown" {
		return semanticsUnknown
	}
	if executorType == "openaicompatexecutor" || provider == "openai-compatibility" || strings.HasPrefix(provider, "openai-compatible-") {
		return semanticsSubset
	}
	if strings.Contains(value, "claude") || strings.Contains(value, "anthropic") {
		return semanticsIndependent
	}
	for _, marker := range []string{"gemini", "aistudio", "antigravity", "vertex", "interaction"} {
		if strings.Contains(value, marker) {
			return semanticsSeparateReasoning
		}
	}
	for _, marker := range []string{"openai", "codex", "xai", "grok", "kimi", "qwen", "deepseek", "openrouter"} {
		if strings.Contains(value, marker) {
			return semanticsSubset
		}
	}
	return semanticsUnknown
}

// tokenCounters converts one record's provider-specific counters into
// counters. Providers CLIProxyAPI cannot classify (plugin executors among
// them) are read as independent when their cache tokens could not have been
// part of the input, and as OpenAI-style otherwise.
func tokenCounters(record pluginapi.UsageRecord) counters {
	detail := record.Detail
	semantics := semanticsFor(record.Provider, record.ExecutorType)
	cacheRead := max(detail.CacheReadTokens, 0)
	cacheWrite := max(detail.CacheCreationTokens, 0)
	if cacheRead == 0 && semantics != semanticsIndependent {
		// OpenAI-style parsers may fill only CachedTokens.
		cacheRead = max(detail.CachedTokens, 0)
	}
	input := max(detail.InputTokens, 0)
	output := max(detail.OutputTokens, 0)
	reasoning := max(detail.ReasoningTokens, 0)
	if semantics == semanticsUnknown {
		semantics = semanticsSubset
		if cacheRead+cacheWrite > input {
			semantics = semanticsIndependent
		}
	}
	c := counters{CacheRead: cacheRead, CacheWrite: cacheWrite}
	switch semantics {
	case semanticsIndependent:
		c.Input = input
		c.Output = output + reasoning
		c.Reasoning = reasoning
	case semanticsSeparateReasoning:
		c.Input = max(input-cacheRead-cacheWrite, 0)
		c.Output = output + reasoning
		c.Reasoning = reasoning
	default:
		c.Input = max(input-cacheRead-cacheWrite, 0)
		c.Output = output
		c.Reasoning = min(reasoning, output)
	}
	return c
}

// usageEvent is one record reduced to what the store keeps.
type usageEvent struct {
	At       time.Time
	Account  string
	Identity accountInfo
	Provider string
	Model    string
	Session  string
	Counters counters
}

func handleUsage(raw []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
		return nil, fmt.Errorf("decode usage record: %w", errUnmarshal)
	}
	if active := activeStore(); active != nil {
		if event, ok := eventFromRecord(record, identities.lookup); ok {
			active.add(event)
		}
	}
	return okEnvelope(struct{}{})
}

func eventFromRecord(record pluginapi.UsageRecord, identify func(string) accountInfo) (usageEvent, bool) {
	// Requests that asked for no generation (token counting) carry no usage.
	if !record.Generate {
		return usageEvent{}, false
	}
	at := record.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	model := strings.TrimSpace(record.Model)
	if model == "" {
		model = strings.TrimSpace(record.Alias)
	}
	c := tokenCounters(record)
	c.Requests = 1
	if record.Failed {
		c.Failed = 1
	}
	event := usageEvent{
		At:       at.UTC(),
		Account:  strings.TrimSpace(record.AuthIndex),
		Provider: strings.ToLower(strings.TrimSpace(record.Provider)),
		Model:    model,
		Session:  sessionDigest(record.SessionID),
		Counters: c,
	}
	if event.Account != "" && identify != nil {
		event.Identity = identify(event.Account)
	}
	if event.Identity.Label == "" {
		event.Identity = recordIdentity(record)
	}
	if event.Identity.Provider == "" {
		event.Identity.Provider = providerName(event.Provider)
	}
	return event, true
}

// recordIdentity names the account from the record alone. CLIProxyAPI's
// host.auth callbacks skip credentials defined in config.yaml (API keys and
// OpenAI-compatible providers), so for those this is the only name. Source
// holds the OAuth email or, for an API key, the key itself.
func recordIdentity(record pluginapi.UsageRecord) accountInfo {
	info := accountInfo{Provider: providerName(record.Provider)}
	source := strings.TrimSpace(record.Source)
	switch strings.ToLower(strings.TrimSpace(record.AuthType)) {
	case "apikey", "api_key":
		info.Label = maskedKeyLabel("", info.Provider, source)
	case "oauth":
		if strings.Contains(source, "@") {
			info.Label = source
		}
	}
	return info
}

// providerName shortens "openai-compatible-<name>" to the configured name.
func providerName(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	return strings.TrimPrefix(provider, "openai-compatible-")
}

// maskedKeyLabel names an API-key credential by its label, unless that is
// CLIProxyAPI's generic "<provider>-apikey", and the key's last four
// characters.
func maskedKeyLabel(label, provider, key string) string {
	label = strings.TrimSpace(label)
	if label == "" || strings.HasSuffix(label, "-apikey") || strings.EqualFold(label, provider) {
		label = "API key"
	}
	if key = strings.TrimSpace(key); len(key) >= 12 {
		return label + " …" + key[len(key)-4:]
	}
	return label
}

// sessionDigest keeps sessions countable without storing client session IDs.
func sessionDigest(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:8])
}

// accountInfo is how an account is shown. It is saved with the usage so an
// account removed from CLIProxyAPI keeps its name.
type accountInfo struct {
	Label    string `json:"label,omitempty"`
	Provider string `json:"provider,omitempty"`
}

// accountLabel names a credential without exposing secrets: an OAuth account
// by its email, an API key by its label and the key's last four characters.
func accountLabel(entry pluginapi.HostAuthFileEntry) string {
	label := strings.TrimSpace(entry.Label)
	if strings.EqualFold(strings.TrimSpace(entry.AccountType), "api_key") {
		return maskedKeyLabel(label, providerName(entry.Provider), entry.Account)
	}
	if email := strings.TrimSpace(entry.Email); email != "" {
		return email
	}
	if label != "" {
		return label
	}
	return strings.TrimSuffix(strings.TrimSpace(entry.Name), ".json")
}

func accountInfoFromEntry(entry pluginapi.HostAuthFileEntry) accountInfo {
	provider := strings.TrimSpace(entry.Provider)
	if provider == "" {
		provider = strings.TrimSpace(entry.Type)
	}
	return accountInfo{Label: accountLabel(entry), Provider: providerName(provider)}
}

const (
	identityTTL        = 10 * time.Minute
	identityFailureTTL = time.Minute
)

// identityCache resolves auth indexes through host.auth.get_runtime at most
// once per TTL, since every model call reports one.
type identityCache struct {
	mu      sync.Mutex
	now     func() time.Time
	resolve func(authIndex string) (pluginapi.HostAuthFileEntry, error)
	entries map[string]cachedIdentity
}

type cachedIdentity struct {
	info    accountInfo
	expires time.Time
}

var identities = &identityCache{now: time.Now, resolve: hostAuthRuntime}

func (c *identityCache) lookup(authIndex string) accountInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if cached, ok := c.entries[authIndex]; ok && now.Before(cached.expires) {
		return cached.info
	}
	entry, errResolve := c.resolve(authIndex)
	cached := cachedIdentity{expires: now.Add(identityTTL)}
	if errResolve != nil {
		cached.expires = now.Add(identityFailureTTL)
	} else {
		cached.info = accountInfoFromEntry(entry)
	}
	if c.entries == nil {
		c.entries = make(map[string]cachedIdentity)
	}
	c.entries[authIndex] = cached
	return cached.info
}
