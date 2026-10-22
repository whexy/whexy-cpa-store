package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// modelsDevURL is the public catalog OpenCode's client builds its OpenCode
// providers from; a workspace's Console config only adjusts it.
const modelsDevURL = "https://models.dev/api.json"

// modelsDevSnapshotJSON is models.dev's opencode-go and opencode providers,
// trimmed to the fields the plugin reads, served until models.dev has been
// read.
//
//go:embed catalog_snapshot.json
var modelsDevSnapshotJSON []byte

// modelsDevCatalog is the part of models.dev the plugin reads: the OpenCode Go
// provider, and the OpenCode provider whose models are OpenCode Zen's and,
// priced at zero, OpenCode Free's.
type modelsDevCatalog struct {
	Go  modelsDevProvider `json:"opencode-go"`
	Zen modelsDevProvider `json:"opencode"`
}

type modelsDevProvider struct {
	API    string                    `json:"api"`
	NPM    string                    `json:"npm"`
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	Name   string          `json:"name"`
	Status string          `json:"status"`
	Cost   json.RawMessage `json:"cost"`
	Limit  struct {
		Context int64 `json:"context"`
		Input   int64 `json:"input"`
		Output  int64 `json:"output"`
	} `json:"limit"`
	Modalities struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	// Provider overrides the provider's package and base for one model.
	Provider struct {
		NPM string `json:"npm"`
		API string `json:"api"`
	} `json:"provider"`
}

// parseModelsDev reads models.dev's catalog. One without either OpenCode
// provider would empty that family's list, so it counts as a failed read.
func parseModelsDev(raw []byte) (*modelsDevCatalog, error) {
	var doc modelsDevCatalog
	if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil {
		return nil, fmt.Errorf("decode models.dev catalog: %w", errUnmarshal)
	}
	for key, provider := range map[string]modelsDevProvider{providerKey: doc.Go, zenProviderKey: doc.Zen} {
		if len(provider.Models) == 0 {
			return nil, fmt.Errorf("models.dev lists no %s model", key)
		}
	}
	return &doc, nil
}

func fetchModelsDev(ctx context.Context, doer httpDoer, cfg settings) (*modelsDevCatalog, error) {
	resp, errDo := doer.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     modelsDevURL,
		Headers: http.Header{"Accept": {"application/json"}, "User-Agent": {cfg.userAgent()}},
	})
	if errDo != nil {
		return nil, fmt.Errorf("fetch models.dev catalog: %w", errDo)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev answered %d", resp.StatusCode)
	}
	return parseModelsDev(resp.Body)
}

type modelsDevEntry struct {
	doc       *modelsDevCatalog
	fetchedAt time.Time
}

// modelsDevStore caches models.dev's catalog, which is the same for every
// credential and for OpenCode Free. A failed refresh keeps the last catalog
// read, then the embedded snapshot.
type modelsDevStore struct {
	mu       sync.Mutex
	now      func() time.Time
	entry    *modelsDevEntry
	snapshot *modelsDevCatalog
}

func newModelsDevStore() *modelsDevStore {
	return &modelsDevStore{now: time.Now}
}

var modelsDev = newModelsDevStore()

// get returns models.dev's catalog, refreshing it once it is older than
// catalogTTL. errFetch is the refresh failure when a fallback was served.
func (s *modelsDevStore) get(ctx context.Context, doer httpDoer, cfg settings) (*modelsDevCatalog, catalogSource, error) {
	s.mu.Lock()
	entry := s.entry
	now := s.now()
	s.mu.Unlock()
	if entry != nil && now.Sub(entry.fetchedAt) < catalogTTL {
		return entry.doc, catalogCached, nil
	}
	fetched, errFetch := s.refresh(ctx, doer, cfg)
	if errFetch == nil {
		return fetched, catalogFresh, nil
	}
	doc, source := s.cached()
	if source == catalogCached {
		source = catalogStale
	}
	return doc, source, errFetch
}

// refresh reads models.dev regardless of the cached catalog's age; a failure
// leaves the cache as it was.
func (s *modelsDevStore) refresh(ctx context.Context, doer httpDoer, cfg settings) (*modelsDevCatalog, error) {
	fetched, errFetch := fetchModelsDev(ctx, doer, cfg)
	if errFetch != nil {
		return nil, errFetch
	}
	s.mu.Lock()
	s.entry = &modelsDevEntry{doc: fetched, fetchedAt: s.now()}
	s.mu.Unlock()
	return fetched, nil
}

// cached returns the best models.dev catalog without network access.
func (s *modelsDevStore) cached() (*modelsDevCatalog, catalogSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entry != nil {
		return s.entry.doc, catalogCached
	}
	if s.snapshot == nil {
		// The embedded snapshot is covered by tests; an empty catalog keeps a
		// broken build from taking the host down.
		parsed, errParse := parseModelsDev(modelsDevSnapshotJSON)
		if errParse != nil {
			parsed = &modelsDevCatalog{}
		}
		s.snapshot = parsed
	}
	return s.snapshot, catalogSnapshot
}
