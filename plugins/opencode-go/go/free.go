package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// OpenCode serves its free models without an account: its own client sends
// them the API key "public" when it has no Console connection. The plugin's
// OpenCode Free provider does the same from one auth file of its own, with
// no login and no key.
const (
	freeDisplayName = "OpenCode Free"
	freeFileName    = "opencode-free.json"
	freeModelPrefix = "opencode-free"
	// freeCredentialID keys the free catalog's model registration.
	freeCredentialID = "free"
)

var freeCredential = credential{ID: freeCredentialID, Bearer: "public", Name: freeDisplayName}

// freeSnapshotJSON is the free part of models.dev's OpenCode provider,
// served until the live catalog has been read.
//
//go:embed free_snapshot.json
var freeSnapshotJSON []byte

// freeFile is the OpenCode Free auth file, {"type":"opencode-go","free":true}.
type freeFile struct {
	Disabled bool
	// extra keeps the routing fields and anything else across a rewrite.
	extra map[string]json.RawMessage
}

var freeFields = map[string]struct{}{"type": {}, "free": {}, "disabled": {}}

func isFreeFile(raw []byte) bool {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return false
	}
	var kind string
	_ = json.Unmarshal(top["type"], &kind)
	return strings.EqualFold(strings.TrimSpace(kind), providerKey) && jsonTrue(top["free"])
}

func parseFreeFile(raw []byte) (freeFile, error) {
	var top map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &top); errUnmarshal != nil {
		return freeFile{}, fmt.Errorf("decode free file: %w", errUnmarshal)
	}
	file := freeFile{Disabled: jsonTrue(top["disabled"]), extra: make(map[string]json.RawMessage)}
	for name, value := range top {
		if _, owned := freeFields[name]; !owned {
			file.extra[name] = value
		}
	}
	return file, nil
}

func (f freeFile) marshal() ([]byte, error) {
	out := make(map[string]any, len(f.extra)+3)
	for name, value := range f.extra {
		out[name] = value
	}
	out["type"] = providerKey
	out["free"] = true
	if f.Disabled {
		out["disabled"] = true
	}
	return json.MarshalIndent(out, "", "  ")
}

// freeAuth is the OpenCode Free auth. It holds no credential; its priority
// and weight are the file's top-level fields, which CPA applies itself.
func freeAuth(base string, raw []byte) pluginapi.AuthData {
	file, _ := parseFreeFile(raw)
	attributes := map[string]string{familyAttribute: string(familyFree)}
	if mark := extraString(file.extra, catalogRefreshedField); mark != "" {
		attributes[catalogRefreshedField] = mark
	}
	return pluginapi.AuthData{
		Provider:    providerKey,
		ID:          base,
		Label:       freeDisplayName,
		Disabled:    file.Disabled,
		StorageJSON: bytes.Clone(raw),
		Attributes:  attributes,
	}
}

// parseFreeCatalog reads the free OpenCode models of a models.dev document.
func parseFreeCatalog(raw []byte, live map[string]bool) (catalog, error) {
	var doc modelsDevCatalog
	if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil {
		return catalog{}, fmt.Errorf("decode models.dev catalog: %w", errUnmarshal)
	}
	return doc.freeCatalog(live)
}

// freeCatalog lists the free OpenCode models of models.dev: every cost zero,
// not deprecated, and when live is not nil, served by the gateway now,
// because models.dev keeps retired models listed.
func (d *modelsDevCatalog) freeCatalog(live map[string]bool) (catalog, error) {
	provider := d.Zen
	var out []catalogModel
	for id, model := range provider.Models {
		id = strings.TrimSpace(id)
		if id == "" || deprecated(model) || !zeroCost(model.Cost) {
			continue
		}
		if live != nil && !live[id] {
			continue
		}
		pkg := model.Provider.NPM
		if pkg == "" {
			pkg = provider.NPM
		}
		protocol, ok := protocolForPackage(pkg)
		if !ok {
			continue
		}
		baseURL := model.Provider.API
		if baseURL == "" {
			baseURL = provider.API
		}
		name := strings.TrimSpace(model.Name)
		if name == "" {
			name = id
		}
		out = append(out, catalogModel{
			Family:           familyFree,
			UpstreamID:       id,
			Name:             name,
			Protocol:         protocol,
			BaseURL:          strings.TrimRight(strings.TrimSpace(baseURL), "/"),
			Free:             true,
			ContextLength:    model.Limit.Context,
			InputLimit:       model.Limit.Input,
			OutputLimit:      model.Limit.Output,
			InputModalities:  model.Modalities.Input,
			OutputModalities: model.Modalities.Output,
		})
	}
	if len(out) == 0 {
		return catalog{}, errors.New("models.dev lists no free OpenCode model")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpstreamID < out[j].UpstreamID })
	return catalog{Free: out}, nil
}

// zeroCost reports a cost whose every price is zero, in models.dev's shape or
// the Console config's; a model without one is not known to be free. A tier's
// own descriptor holds its size, not a price.
func zeroCost(raw json.RawMessage) bool {
	var cost any
	if isJSONNull(raw) || json.Unmarshal(raw, &cost) != nil {
		return false
	}
	prices := 0
	var walk func(value any) bool
	walk = func(value any) bool {
		switch typed := value.(type) {
		case float64:
			prices++
			return typed == 0
		case map[string]any:
			for key, item := range typed {
				if key != "tier" && !walk(item) {
					return false
				}
			}
		case []any:
			for _, item := range typed {
				if !walk(item) {
					return false
				}
			}
		}
		return true
	}
	return walk(cost) && prices > 0
}

// fetchFreeCatalog reads the gateway's live model list, which is public, and
// lists the free models of doc it serves. A failed live list leaves the
// models.dev list unfiltered.
func fetchFreeCatalog(ctx context.Context, doer httpDoer, cfg settings, doc *modelsDevCatalog) (catalog, error) {
	live, _ := fetchLiveModelIDs(ctx, doer, cfg)
	return doc.freeCatalog(live)
}

// liveModelsURL lists every model the pay-as-you-go gateway serves now; it
// needs no key.
func liveModelsURL(cfg settings) string {
	return cfg.zenBaseURL() + "/v1/models"
}

func fetchLiveModelIDs(ctx context.Context, doer httpDoer, cfg settings) (map[string]bool, error) {
	resp, errDo := doer.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     liveModelsURL(cfg),
		Headers: http.Header{"Accept": {"application/json"}, "User-Agent": {cfg.userAgent()}},
	})
	if errDo != nil {
		return nil, errDo
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("live model list answered %d", resp.StatusCode)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(resp.Body, &list); errUnmarshal != nil {
		return nil, fmt.Errorf("decode live model list: %w", errUnmarshal)
	}
	ids := make(map[string]bool, len(list.Data))
	for _, item := range list.Data {
		ids[strings.TrimSpace(item.ID)] = true
	}
	if len(ids) == 0 {
		return nil, errors.New("live model list is empty")
	}
	return ids, nil
}

// freeCatalogStore caches the free catalog. It is built only from a
// models.dev catalog that was read, never from a fallback; a failed refresh
// keeps the last catalog read, then the embedded snapshot.
type freeCatalogStore struct {
	mu       sync.Mutex
	now      func() time.Time
	entry    *catalogEntry
	snapshot *catalog
}

func newFreeCatalogStore() *freeCatalogStore {
	return &freeCatalogStore{now: time.Now}
}

var freeCatalogs = newFreeCatalogStore()

// get returns the free catalog, refreshing it once it is older than
// catalogTTL. errFetch is the refresh failure when a fallback was served.
func (s *freeCatalogStore) get(ctx context.Context, doer httpDoer, cfg settings) (catalog, catalogSource, error) {
	s.mu.Lock()
	entry := s.entry
	now := s.now()
	s.mu.Unlock()
	if entry != nil && now.Sub(entry.fetchedAt) < catalogTTL {
		return entry.catalog, catalogCached, nil
	}
	doc, _, errFetch := modelsDev.get(ctx, doer, cfg)
	if errFetch == nil {
		fetched, errRefresh := s.refresh(ctx, doer, cfg, doc)
		if errRefresh == nil {
			return fetched, catalogFresh, nil
		}
		errFetch = errRefresh
	}
	cat, source := s.cached()
	if source == catalogCached {
		source = catalogStale
	}
	return cat, source, errFetch
}

func (s *freeCatalogStore) refresh(ctx context.Context, doer httpDoer, cfg settings, doc *modelsDevCatalog) (catalog, error) {
	fetched, errFetch := fetchFreeCatalog(ctx, doer, cfg, doc)
	if errFetch != nil {
		return catalog{}, errFetch
	}
	s.mu.Lock()
	s.entry = &catalogEntry{catalog: fetched, fetchedAt: s.now()}
	s.mu.Unlock()
	return fetched, nil
}

// cached returns the best free catalog without network access.
func (s *freeCatalogStore) cached() (catalog, catalogSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entry != nil {
		return s.entry.catalog, catalogCached
	}
	if s.snapshot == nil {
		// The embedded snapshot is covered by tests; an empty catalog keeps a
		// broken build from taking the host down.
		parsed, _ := parseFreeCatalog(freeSnapshotJSON, nil)
		s.snapshot = &parsed
	}
	return *s.snapshot, catalogSnapshot
}

func (s *freeCatalogStore) fetchedAt() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entry == nil {
		return time.Time{}, false
	}
	return s.entry.fetchedAt, true
}

// storedFree is the OpenCode Free auth file as the host currently sees it.
type storedFree struct {
	Name string
	File freeFile
	Auth pluginapi.HostAuthFileEntry
}

// editFreeFile rewrites the OpenCode Free file, creating it when create is
// set and it does not exist yet.
func editFreeFile(create bool, edit func(*freeFile)) error {
	keyStoreMu.Lock()
	defer keyStoreMu.Unlock()
	files, errLoad := loadAuthFiles()
	if errLoad != nil {
		return errLoad
	}
	name, file := freeFileName, freeFile{extra: make(map[string]json.RawMessage)}
	if files.Free != nil {
		name, file = files.Free.Name, files.Free.File
	} else if !create {
		return errFileGone
	}
	edit(&file)
	raw, errMarshal := file.marshal()
	if errMarshal != nil {
		return errMarshal
	}
	_, errSave := hostAuthSave(name, raw)
	return errSave
}

// freeView is the OpenCode Free provider as the page shows it. Models are
// listed only while the provider is on, which spares the catalog fetch.
type freeView struct {
	File     string       `json:"file,omitempty"`
	Disabled bool         `json:"disabled"`
	Runtime  *runtimeView `json:"runtime,omitempty"`
	// FetchedAt is when the free catalog was last read; Error says why it
	// could not be read, and Models then holds the last list read.
	FetchedAt    string      `json:"fetched_at,omitempty"`
	Error        string      `json:"error,omitempty"`
	Models       []modelView `json:"models"`
	Unregistered []string    `json:"unregistered"`
	Unlisted     []string    `json:"unlisted"`
}

func handleGetFree(h host, _ pluginapi.ManagementRequest) (int, any, error) {
	files, errLoad := loadAuthFiles()
	if errLoad != nil {
		return 0, nil, failf(http.StatusInternalServerError, "Could not read the OpenCode Go auth files: %v", errLoad)
	}
	return http.StatusOK, newFreeView(h, files.Free), nil
}

func newFreeView(h host, stored *storedFree) freeView {
	view := freeView{Models: []modelView{}, Unregistered: []string{}, Unlisted: []string{}}
	if stored == nil {
		return view
	}
	view.File, view.Disabled = stored.Name, stored.File.Disabled
	if stored.Auth.AuthIndex != "" {
		view.Runtime = newRuntimeView(stored.Auth)
	}
	if view.Disabled {
		return view
	}
	cfg := currentSettings()
	cat, _, errFetch := freeCatalogs.get(context.Background(), h, cfg)
	if errFetch != nil {
		view.Error = errFetch.Error()
	}
	if at, ok := freeCatalogs.fetchedAt(); ok {
		view.FetchedAt = at.UTC().Format(time.RFC3339)
	}
	for _, model := range cat.Free {
		view.Models = append(view.Models, newModelView(cfg, model))
	}
	if reg, ok := catalogs.registeredFor(freeCredentialID, familyFree); ok {
		ids := cat.exposedIDs(cfg, familyFree)
		for _, id := range ids {
			if !slices.Contains(reg.ids, id) {
				view.Unregistered = append(view.Unregistered, id)
			}
		}
		for _, id := range reg.ids {
			if !slices.Contains(ids, id) {
				view.Unlisted = append(view.Unlisted, id)
			}
		}
	}
	return view
}

// handlePutFree turns OpenCode Free on, creating its auth file the first
// time, or off: {"enabled": true}. Turning it off disables the auth; the
// file stays, with its routing, until it is deleted on the Auth Files page.
func handlePutFree(h host, req pluginapi.ManagementRequest) (int, any, error) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if errDecode := decodeBody(req.Body, &body); errDecode != nil {
		return 0, nil, errDecode
	}
	if body.Enabled == nil {
		return 0, nil, failf(http.StatusBadRequest, "Nothing to change: send enabled.")
	}
	errSave := editFreeFile(*body.Enabled, func(file *freeFile) {
		file.Disabled = !*body.Enabled
	})
	if errors.Is(errSave, errFileGone) {
		return handleGetFree(h, req)
	}
	if errSave != nil {
		return 0, nil, failf(http.StatusInternalServerError, "Could not save %s: %v", freeFileName, errSave)
	}
	return handleGetFree(h, req)
}
