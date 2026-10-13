package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	customPricesFileName    = "custom-prices.json"
	customPricesFileVersion = 1
	maxCustomPrices         = 1000
	maxCustomModelLength    = 256
	// maxCustomRate bounds a rate in USD per million tokens; anything above
	// is a typo, not a price.
	maxCustomRate = 1e6
)

// customPriceInput is one price as the page sends it and the file stores it.
// Pointers tell a missing rate from a zero one.
type customPriceInput struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
}

// customPricesFile is custom-prices.json: model ID as stored with the usage
// -> price.
type customPricesFile struct {
	Version int                         `json:"version"`
	Models  map[string]customPriceInput `json:"models"`
}

// validateCustomPrices checks prices set by hand. Model IDs are matched
// exactly, so only surrounding whitespace is dropped.
func validateCustomPrices(in map[string]customPriceInput) (map[string]price, error) {
	if len(in) > maxCustomPrices {
		return nil, fmt.Errorf("at most %d custom prices", maxCustomPrices)
	}
	out := make(map[string]price, len(in))
	for raw, entry := range in {
		model := strings.TrimSpace(raw)
		switch {
		case model == "":
			return nil, errors.New("a custom price has no model ID")
		case len(model) > maxCustomModelLength:
			return nil, fmt.Errorf("a model ID is longer than %d characters", maxCustomModelLength)
		}
		if _, duplicate := out[model]; duplicate {
			return nil, fmt.Errorf("%s: priced twice", model)
		}
		if entry.Input == nil {
			return nil, fmt.Errorf("%s: input price is required", model)
		}
		if entry.Output == nil {
			return nil, fmt.Errorf("%s: output price is required", model)
		}
		for _, rate := range []struct {
			name  string
			value *float64
		}{{"input", entry.Input}, {"output", entry.Output}, {"cache read", entry.CacheRead}, {"cache write", entry.CacheWrite}} {
			if rate.value != nil && (*rate.value < 0 || *rate.value > maxCustomRate) {
				return nil, fmt.Errorf("%s: %s price must be between 0 and %.0f", model, rate.name, float64(maxCustomRate))
			}
		}
		out[model] = price{Input: *entry.Input, Output: *entry.Output, CacheRead: entry.CacheRead, CacheWrite: entry.CacheWrite}
	}
	return out, nil
}

// customPricing holds the prices set on the page, which win over the
// catalog. models is replaced on save, never changed in place, so callers
// may keep the map current returns.
type customPricing struct {
	mu     sync.Mutex
	path   string
	models map[string]price
	now    func() time.Time
}

var customPrices = &customPricing{now: time.Now}

// configure loads the prices kept in dataDir when the directory changes.
func (c *customPricing) configure(dataDir string) {
	path := filepath.Join(dataDir, customPricesFileName)
	c.mu.Lock()
	defer c.mu.Unlock()
	if path == c.path {
		return
	}
	c.path = path
	c.models = nil
	raw, errRead := os.ReadFile(path)
	if errors.Is(errRead, os.ErrNotExist) {
		return
	}
	models, errLoad := decodeCustomPrices(raw, errRead)
	if errLoad != nil {
		// Keep the file for inspection; saving from the page starts afresh.
		aside := path + ".corrupt-" + c.now().UTC().Format("20060102T150405")
		_ = os.Rename(path, aside)
		hostLog("warn", fmt.Sprintf("custom prices: %s is unreadable (%v), moved to %s", customPricesFileName, errLoad, filepath.Base(aside)))
		return
	}
	c.models = models
}

func decodeCustomPrices(raw []byte, errRead error) (map[string]price, error) {
	if errRead != nil {
		return nil, errRead
	}
	var file customPricesFile
	if errUnmarshal := json.Unmarshal(raw, &file); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if file.Version != customPricesFileVersion {
		return nil, fmt.Errorf("unsupported version %d", file.Version)
	}
	return validateCustomPrices(file.Models)
}

func (c *customPricing) current() map[string]price {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.models
}

// save replaces every custom price with models, which validateCustomPrices
// returned.
func (c *customPricing) save(models map[string]price) error {
	file := customPricesFile{Version: customPricesFileVersion, Models: make(map[string]customPriceInput, len(models))}
	for model, p := range models {
		file.Models[model] = customPriceInput{Input: &p.Input, Output: &p.Output, CacheRead: p.CacheRead, CacheWrite: p.CacheWrite}
	}
	raw, errMarshal := json.MarshalIndent(file, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("encode custom prices: %w", errMarshal)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path == "" {
		return errors.New("usage store is not running")
	}
	if errMkdir := os.MkdirAll(filepath.Dir(c.path), 0o700); errMkdir != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(c.path), errMkdir)
	}
	if errWrite := writeFileAtomic(c.path, raw); errWrite != nil {
		return errWrite
	}
	c.models = models
	return nil
}

// modelPriceView is one model in the custom price editor.
type modelPriceView struct {
	Model  string `json:"model"`
	Family string `json:"family"`
	// Processed counts the model's tokens over the whole retained history.
	Processed int64  `json:"processed"`
	Custom    *price `json:"custom,omitempty"`
	// Catalog is the models.dev price for the provider that served the most
	// of the model's tokens.
	Catalog *price `json:"catalog,omitempty"`
}

type priceListView struct {
	Models []modelPriceView `json:"models"`
}

// buildPriceList lists every model in history, plus custom prices for
// models no longer in it so they stay editable.
func buildPriceList(history []storedRow, catalog *priceCatalog, custom map[string]price, rules []familyRule) priceListView {
	type usage struct {
		processed  int64
		byProvider map[string]int64
	}
	models := make(map[string]*usage)
	for _, r := range history {
		u := models[r.Key.Model]
		if u == nil {
			u = &usage{byProvider: make(map[string]int64)}
			models[r.Key.Model] = u
		}
		u.processed += r.Counters.processed()
		u.byProvider[r.Key.Provider] += r.Counters.processed()
	}
	for model := range custom {
		if models[model] == nil {
			models[model] = &usage{byProvider: map[string]int64{}}
		}
	}
	list := priceListView{Models: make([]modelPriceView, 0, len(models))}
	for model, u := range models {
		family := modelFamily(model, rules)
		view := modelPriceView{Model: model, Family: family, Processed: u.processed}
		if p, ok := custom[model]; ok {
			view.Custom = &p
		}
		provider, top := "", int64(-1)
		for candidate, tokens := range u.byProvider {
			if tokens > top || (tokens == top && candidate < provider) {
				provider, top = candidate, tokens
			}
		}
		if p, ok := catalog.lookup(provider, model, family); ok {
			view.Catalog = &p
		}
		list.Models = append(list.Models, view)
	}
	sort.Slice(list.Models, func(i, j int) bool {
		a, b := list.Models[i], list.Models[j]
		if a.Processed != b.Processed {
			return a.Processed > b.Processed
		}
		return a.Model < b.Model
	})
	return list
}

// priceList assembles the editor's model list from live state.
func priceList() (priceListView, error) {
	active := activeStore()
	if active == nil {
		return priceListView{}, errors.New("usage store is not running")
	}
	all, _ := active.snapshot(time.Time{}, time.Now().Add(48*time.Hour))
	catalog, _ := prices.current()
	return buildPriceList(all, catalog, customPrices.current(), currentSettings().FamilyRules), nil
}
