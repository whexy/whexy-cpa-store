package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	pricesFileName  = "prices.json"
	pricesMaxAge    = 24 * time.Hour
	pricesRetry     = time.Hour
	pricesCheckTick = time.Minute
)

// price is USD per million tokens. Absent cache rates fall back to Input.
type price struct {
	Input      float64  `json:"input"`
	Output     float64  `json:"output"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
}

func (p price) cacheRead() float64 {
	if p.CacheRead != nil {
		return *p.CacheRead
	}
	return p.Input
}

func (p price) cacheWrite() float64 {
	if p.CacheWrite != nil {
		return *p.CacheWrite
	}
	return p.Input
}

// cost estimates c at p's base rates. models.dev also lists long-context
// tiers; hourly rows no longer know each request's context size, so they are
// not applied.
func (p price) cost(c counters) float64 {
	return (float64(c.Input)*p.Input +
		float64(c.CacheRead)*p.cacheRead() +
		float64(c.CacheWrite)*p.cacheWrite() +
		float64(c.Output)*p.Output) / 1e6
}

// savings is what cache reads saved against paying the input rate for them.
func (p price) savings(c counters) float64 {
	return float64(c.CacheRead) * max(p.Input-p.cacheRead(), 0) / 1e6
}

// priceCatalog is the trimmed models.dev catalog kept in prices.json:
// provider ID -> lower-case model ID -> price.
type priceCatalog struct {
	Source    string                      `json:"source"`
	FetchedAt time.Time                   `json:"fetched_at"`
	Models    map[string]map[string]price `json:"models"`

	// byModel lists, per model ID, the providers that price it, sorted.
	byModel map[string][]string
}

func (c *priceCatalog) index() {
	c.byModel = make(map[string][]string)
	for provider, models := range c.Models {
		for model := range models {
			c.byModel[model] = append(c.byModel[model], provider)
		}
	}
	for model := range c.byModel {
		sort.Strings(c.byModel[model])
	}
}

type modelsDevProvider struct {
	Models map[string]struct {
		ID   string `json:"id"`
		Cost *struct {
			Input      *float64 `json:"input"`
			Output     *float64 `json:"output"`
			CacheRead  *float64 `json:"cache_read"`
			CacheWrite *float64 `json:"cache_write"`
		} `json:"cost"`
	} `json:"models"`
}

func parseModelsDev(raw []byte, source string, fetchedAt time.Time) (*priceCatalog, error) {
	var providers map[string]modelsDevProvider
	if errUnmarshal := json.Unmarshal(raw, &providers); errUnmarshal != nil {
		return nil, fmt.Errorf("decode catalog: %w", errUnmarshal)
	}
	catalog := &priceCatalog{Source: source, FetchedAt: fetchedAt, Models: make(map[string]map[string]price)}
	for providerID, provider := range providers {
		for key, model := range provider.Models {
			if model.Cost == nil || model.Cost.Input == nil || model.Cost.Output == nil {
				continue
			}
			id := strings.ToLower(strings.TrimSpace(model.ID))
			if id == "" {
				id = strings.ToLower(strings.TrimSpace(key))
			}
			if catalog.Models[providerID] == nil {
				catalog.Models[providerID] = make(map[string]price)
			}
			catalog.Models[providerID][id] = price{
				Input:      *model.Cost.Input,
				Output:     *model.Cost.Output,
				CacheRead:  model.Cost.CacheRead,
				CacheWrite: model.Cost.CacheWrite,
			}
		}
	}
	if len(catalog.Models) == 0 {
		return nil, errors.New("catalog lists no priced models")
	}
	catalog.index()
	return catalog, nil
}

// providerCatalogIDs maps CLIProxyAPI provider keys to models.dev provider
// IDs.
var providerCatalogIDs = map[string]string{
	"claude":      "anthropic",
	"anthropic":   "anthropic",
	"codex":       "openai",
	"openai":      "openai",
	"gemini":      "google",
	"gemini-cli":  "google",
	"aistudio":    "google",
	"antigravity": "google",
	"vertex":      "google",
	"xai":         "xai",
	"grok":        "xai",
	"kimi":        "moonshotai",
	"qwen":        "alibaba",
	"deepseek":    "deepseek",
}

// familyCatalogIDs is the vendor that publishes each family's list price.
var familyCatalogIDs = map[string]string{
	"Claude":   "anthropic",
	"GPT":      "openai",
	"Gemini":   "google",
	"Gemma":    "google",
	"Grok":     "xai",
	"DeepSeek": "deepseek",
	"Kimi":     "moonshotai",
	"Qwen":     "alibaba",
	"GLM":      "zhipuai",
	"MiniMax":  "minimax",
	"Mistral":  "mistral",
}

var dateSuffix = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2})$`)

// lookup finds the price of model as served by provider: first under the
// provider's own catalog entry, then under the family's vendor, then under
// any provider listing that exact ID.
func (c *priceCatalog) lookup(provider, model, family string) (price, bool) {
	if c == nil {
		return price{}, false
	}
	id := bareModelID(model)
	candidates := []string{id}
	if trimmed := dateSuffix.ReplaceAllString(id, ""); trimmed != id {
		candidates = append(candidates, trimmed)
	}
	if trimmed := strings.TrimSuffix(id, "-latest"); trimmed != id {
		candidates = append(candidates, trimmed)
	}
	preferred := []string{providerCatalogIDs[strings.ToLower(provider)], familyCatalogIDs[family]}
	for _, candidate := range candidates {
		for _, providerID := range preferred {
			if p, ok := c.Models[providerID][candidate]; ok {
				return p, true
			}
		}
	}
	for _, candidate := range candidates {
		if providers := c.byModel[candidate]; len(providers) > 0 {
			return c.Models[providers[0]][candidate], true
		}
	}
	return price{}, false
}

// pricing keeps the current catalog and refreshes it in the background.
type pricing struct {
	mu        sync.Mutex
	url       string
	cachePath string
	catalog   *priceCatalog
	lastError string
	nextFetch time.Time
	fetch     func(url string) ([]byte, error)
	now       func() time.Time
}

var prices = &pricing{fetch: fetchCatalog, now: time.Now}

func fetchCatalog(url string) ([]byte, error) {
	resp, errGet := hostHTTPGet(url)
	if errGet != nil {
		return nil, errGet
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s answered HTTP %d", url, resp.StatusCode)
	}
	return resp.Body, nil
}

// configure applies settings; an empty url disables pricing.
func (p *pricing) configure(url, dataDir string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cachePath := filepath.Join(dataDir, pricesFileName)
	if url != p.url {
		p.nextFetch = time.Time{}
	}
	p.url = url
	if url == "" {
		p.catalog = nil
		p.lastError = ""
		return
	}
	if cachePath != p.cachePath || p.catalog == nil {
		p.cachePath = cachePath
		if raw, errRead := os.ReadFile(cachePath); errRead == nil {
			var cached priceCatalog
			if json.Unmarshal(raw, &cached) == nil && cached.Source == url && len(cached.Models) > 0 {
				cached.index()
				p.catalog = &cached
				p.nextFetch = cached.FetchedAt.Add(pricesMaxAge)
			}
		}
	}
}

func (p *pricing) run(ctx context.Context) {
	p.refreshIfDue()
	ticker := time.NewTicker(pricesCheckTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.refreshIfDue()
		}
	}
}

func (p *pricing) refreshIfDue() {
	p.mu.Lock()
	url, cachePath := p.url, p.cachePath
	due := url != "" && !p.now().Before(p.nextFetch)
	if due {
		// Claim the slot so a slow fetch is not started twice.
		p.nextFetch = p.now().Add(pricesRetry)
	}
	p.mu.Unlock()
	if !due {
		return
	}
	catalog, errRefresh := p.download(url)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.url != url {
		return
	}
	if errRefresh != nil {
		if errRefresh.Error() != p.lastError {
			hostLog("warn", "price catalog: "+errRefresh.Error())
		}
		p.lastError = errRefresh.Error()
		return
	}
	p.catalog = catalog
	p.lastError = ""
	p.nextFetch = catalog.FetchedAt.Add(pricesMaxAge)
	if raw, errMarshal := json.Marshal(catalog); errMarshal == nil {
		if errMkdir := os.MkdirAll(filepath.Dir(cachePath), 0o700); errMkdir == nil {
			_ = writeFileAtomic(cachePath, raw)
		}
	}
}

func (p *pricing) download(url string) (*priceCatalog, error) {
	raw, errFetch := p.fetch(url)
	if errFetch != nil {
		return nil, errFetch
	}
	return parseModelsDev(raw, url, p.now().UTC())
}

type pricingStatus struct {
	Enabled   bool       `json:"enabled"`
	Source    string     `json:"source,omitempty"`
	FetchedAt *time.Time `json:"fetched_at,omitempty"`
	Error     string     `json:"error,omitempty"`
}

func (p *pricing) current() (*priceCatalog, pricingStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	status := pricingStatus{Enabled: p.url != "", Source: p.url, Error: p.lastError}
	if p.catalog != nil {
		fetched := p.catalog.FetchedAt
		status.FetchedAt = &fetched
	}
	return p.catalog, status
}
