package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// snapshotJSON is the opencode-go provider of /console/api/v2/config,
// served until a key has fetched the live catalog.
//
//go:embed catalog_snapshot.json
var snapshotJSON []byte

const catalogTTL = 10 * time.Minute

// modelFamily is which OpenCode catalog a model belongs to: the Go
// subscription gateway or the pay-as-you-go Console (OpenCode Zen) gateway.
type modelFamily string

const (
	familyGo  modelFamily = "go"
	familyZen modelFamily = "zen"
)

// catalog is the model list of one workspace: the Go subscription models and
// the pay-as-you-go (OpenCode Zen) models of the same Console config.
type catalog struct {
	// Workspace is the workspace ID the key belongs to, empty for the snapshot.
	Workspace string
	Models    []catalogModel
	// Zen holds the workspace's OpenCode Zen models, which the workspace's
	// Console has enabled. The plugin serves one only once the page turns it
	// on, because those calls bill Console credits.
	Zen []catalogModel
}

// catalogModel is one enabled model as the catalog describes it.
type catalogModel struct {
	Family     modelFamily
	UpstreamID string
	Name       string
	Protocol   sdktranslator.Format
	// BaseURL is the protocol base the catalog publishes, before rebasing
	// onto the configured base URL.
	BaseURL          string
	Free             bool
	ContextLength    int64
	InputLimit       int64
	OutputLimit      int64
	InputModalities  []string
	OutputModalities []string
}

// modelRoute is what the executor needs to send a request for one model.
type modelRoute struct {
	// ID is the model ID clients use, with the configured prefix.
	ID string
	// UpstreamID is the bare OpenCode model ID sent in the request body.
	UpstreamID string
	Name       string
	// Family says which gateway serves the model, which decides whether the
	// Go subscription quota applies to it.
	Family modelFamily
	// Protocol is the model's native wire format: openai (Chat Completions),
	// openai-response (Responses) or claude (Messages).
	Protocol sdktranslator.Format
	// Endpoint is the full URL of the native endpoint on the configured base.
	Endpoint string
	// Free models have every cost tier at zero and stay usable while the
	// subscription quota is exhausted.
	Free             bool
	ContextLength    int64
	InputLimit       int64
	OutputLimit      int64
	InputModalities  []string
	OutputModalities []string
}

type configDocument struct {
	Providers map[string]configProvider `json:"providers"`
}

type configProvider struct {
	Package  string                 `json:"package"`
	Settings configSettings         `json:"settings"`
	Headers  map[string]string      `json:"headers"`
	Models   map[string]configModel `json:"models"`
}

type configSettings struct {
	BaseURL string `json:"baseURL"`
}

type configModel struct {
	Name         string         `json:"name"`
	Package      string         `json:"package"`
	Disabled     bool           `json:"disabled"`
	Settings     configSettings `json:"settings"`
	Capabilities struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"capabilities"`
	Cost []struct {
		Input  float64 `json:"input"`
		Output float64 `json:"output"`
		Cache  struct {
			Read  float64 `json:"read"`
			Write float64 `json:"write"`
		} `json:"cache"`
	} `json:"cost"`
	Limit struct {
		Context int64 `json:"context"`
		Input   int64 `json:"input"`
		Output  int64 `json:"output"`
	} `json:"limit"`
}

// parseCatalog reads providers["opencode-go"] and providers["opencode"] of
// the workspace config. The Go provider decides the workspace: only it
// carries the org header. A workspace without the Zen provider has no
// pay-as-you-go models.
func parseCatalog(raw []byte) (catalog, error) {
	var doc configDocument
	if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil {
		return catalog{}, fmt.Errorf("decode workspace config: %w", errUnmarshal)
	}
	provider, ok := doc.Providers[providerKey]
	if !ok {
		return catalog{}, fmt.Errorf("workspace config has no %s provider", providerKey)
	}
	out := catalog{
		Workspace: strings.TrimSpace(provider.Headers["x-opencode-org-id"]),
		Models:    parseProviderModels(familyGo, provider),
	}
	if len(out.Models) == 0 {
		return catalog{}, fmt.Errorf("workspace config lists no enabled %s model", providerKey)
	}
	if zen, ok := doc.Providers[zenProviderKey]; ok {
		out.Zen = parseProviderModels(familyZen, zen)
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].UpstreamID < out.Models[j].UpstreamID })
	sort.Slice(out.Zen, func(i, j int) bool { return out.Zen[i].UpstreamID < out.Zen[j].UpstreamID })
	return out, nil
}

// parseProviderModels reads one provider's enabled models, dropping the ones
// whose protocol the plugin cannot serve.
func parseProviderModels(family modelFamily, provider configProvider) []catalogModel {
	var out []catalogModel
	for id, model := range provider.Models {
		id = strings.TrimSpace(id)
		if id == "" || model.Disabled {
			continue
		}
		pkg := model.Package
		if pkg == "" {
			pkg = provider.Package
		}
		protocol, ok := protocolForPackage(family, pkg)
		if !ok {
			continue
		}
		baseURL := model.Settings.BaseURL
		if baseURL == "" {
			baseURL = provider.Settings.BaseURL
		}
		free := len(model.Cost) > 0
		for _, tier := range model.Cost {
			if tier.Input != 0 || tier.Output != 0 || tier.Cache.Read != 0 || tier.Cache.Write != 0 {
				free = false
			}
		}
		name := strings.TrimSpace(model.Name)
		if name == "" {
			name = id
		}
		out = append(out, catalogModel{
			Family:           family,
			UpstreamID:       id,
			Name:             name,
			Protocol:         protocol,
			BaseURL:          strings.TrimRight(strings.TrimSpace(baseURL), "/"),
			Free:             free,
			ContextLength:    model.Limit.Context,
			InputLimit:       model.Limit.Input,
			OutputLimit:      model.Limit.Output,
			InputModalities:  model.Capabilities.Input,
			OutputModalities: model.Capabilities.Output,
		})
	}
	return out
}

// protocolForPackage maps the AI SDK package the catalog names to the
// endpoint the gateway accepts for that model. Responses- and
// Messages-family models reject every other endpoint.
//
// The Go gateway serves only packages the plugin knows, so an unknown one
// still maps to Chat Completions there, as the provider default. The Zen
// gateway also serves Gemini and SystemOne models, which this plugin cannot
// speak; it drops those rather than send them to the wrong endpoint.
func protocolForPackage(family modelFamily, pkg string) (sdktranslator.Format, bool) {
	switch strings.TrimPrefix(strings.TrimSpace(pkg), "aisdk:") {
	case "@ai-sdk/anthropic":
		return sdktranslator.FormatClaude, true
	case "@ai-sdk/openai":
		return sdktranslator.FormatOpenAIResponse, true
	case "@ai-sdk/openai-compatible":
		return sdktranslator.FormatOpenAI, true
	}
	if family == familyGo {
		return sdktranslator.FormatOpenAI, true
	}
	return "", false
}

var (
	protocolBasePath = map[sdktranslator.Format]string{
		sdktranslator.FormatOpenAI:         "/openai/v1",
		sdktranslator.FormatOpenAIResponse: "/openai/v1",
		sdktranslator.FormatClaude:         "/anthropic/v1",
	}
	protocolEndpoint = map[sdktranslator.Format]string{
		sdktranslator.FormatOpenAI:         "/chat/completions",
		sdktranslator.FormatOpenAIResponse: "/responses",
		sdktranslator.FormatClaude:         "/messages",
	}
)

// endpointFor rebases the catalog's protocol base onto the configured base
// URL. A catalog base outside the family's gateway is ignored: the other
// opencode.ai inference paths belong to a different gateway, whose traffic a
// model of this family must not draw.
func endpointFor(cfg settings, family modelFamily, protocol sdktranslator.Format, catalogBase string) string {
	root, base := cfg.gatewayBase(family)
	path := protocolBasePath[protocol]
	if rel, ok := strings.CutPrefix(catalogBase, root+"/"); ok && rel != "" && rebasablePath(family, rel) {
		path = "/" + rel
	}
	return strings.TrimRight(base, "/") + path + protocolEndpoint[protocol]
}

// rebasablePath says a catalog base under the family's gateway names a path
// this model's protocol can be sent to. The Go gateway serves every protocol
// under its /go subtree, so any relative path there is the model's own; the
// pay-as-you-go gateway names the protocol family first, so a base that names
// none is dropped in favour of the model's protocol rather than aiming a chat
// request at a path the gateway does not serve it from.
func rebasablePath(family modelFamily, rel string) bool {
	if family != familyZen {
		return true
	}
	return strings.HasPrefix(rel, "openai/") || strings.HasPrefix(rel, "anthropic/")
}

// exposedModelID is the ID clients request. The prefix is part of the ID
// itself rather than AuthData.Prefix, because the host also registers the
// bare ID for a prefixed auth unless force-model-prefix is on, and a bare ID
// that a native provider serves would share its traffic.
func exposedModelID(cfg settings, family modelFamily, upstreamID string) string {
	prefix := cfg.prefix()
	if family == familyZen {
		prefix = cfg.zenPrefix()
	}
	if prefix == "" {
		return upstreamID
	}
	return prefix + "/" + upstreamID
}

func (m catalogModel) route(cfg settings) modelRoute {
	return modelRoute{
		ID:               exposedModelID(cfg, m.Family, m.UpstreamID),
		UpstreamID:       m.UpstreamID,
		Name:             m.Name,
		Family:           m.Family,
		Protocol:         m.Protocol,
		Endpoint:         endpointFor(cfg, m.Family, m.Protocol, m.BaseURL),
		Free:             m.Free,
		ContextLength:    m.ContextLength,
		InputLimit:       m.InputLimit,
		OutputLimit:      m.OutputLimit,
		InputModalities:  m.InputModalities,
		OutputModalities: m.OutputModalities,
	}
}

// servedModels are the catalog models the settings register with CPA: every
// Go model except the disabled ones, and the Zen models the page has turned
// on, Go models first.
func (c catalog) servedModels(cfg settings) []catalogModel {
	out := make([]catalogModel, 0, len(c.Models)+len(c.Zen))
	for _, model := range c.Models {
		if !cfg.modelDisabled(model.UpstreamID) {
			out = append(out, model)
		}
	}
	for _, model := range c.Zen {
		if cfg.zenEnabled(model.UpstreamID) {
			out = append(out, model)
		}
	}
	return out
}

func (c catalog) modelInfos(cfg settings) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(c.Models)+len(c.Zen))
	for _, model := range c.servedModels(cfg) {
		inputLimit := model.InputLimit
		if inputLimit == 0 {
			inputLimit = model.ContextLength
		}
		out = append(out, pluginapi.ModelInfo{
			ID:                        exposedModelID(cfg, model.Family, model.UpstreamID),
			Object:                    "model",
			OwnedBy:                   providerKey,
			Type:                      providerKey,
			DisplayName:               model.Name,
			Name:                      model.UpstreamID,
			ContextLength:             model.ContextLength,
			InputTokenLimit:           inputLimit,
			MaxCompletionTokens:       model.OutputLimit,
			OutputTokenLimit:          model.OutputLimit,
			SupportedInputModalities:  append([]string(nil), model.InputModalities...),
			SupportedOutputModalities: append([]string(nil), model.OutputModalities...),
		})
	}
	return out
}

// find resolves a requested model ID: the prefixed or the bare Go ID, or a
// Zen model the page has turned on, optionally with a CPA thinking suffix
// such as "(high)". The prefixed Go form resolves with disable_prefix on as
// well: CPA then registers bare IDs only, so it never routes a client's
// prefixed request here, but a Bare ID alias kept from the prefixed mode
// still makes CPA hand this executor the alias's prefixed name for a request
// of the bare ID.
func (c catalog) find(cfg settings, model string) (catalogModel, bool) {
	model = strings.TrimSpace(model)
	if open := strings.LastIndex(model, "("); open > 0 && strings.HasSuffix(model, ")") {
		model = model[:open]
	}
	if rest, ok := strings.CutPrefix(model, cfg.zenPrefix()+"/"); ok {
		for _, candidate := range c.Zen {
			if candidate.UpstreamID == rest && cfg.zenEnabled(rest) {
				return candidate, true
			}
		}
		return catalogModel{}, false
	}
	if cfg.ModelPrefix != "" {
		model = strings.TrimPrefix(model, cfg.ModelPrefix+"/")
	}
	for _, candidate := range c.Models {
		if candidate.UpstreamID == model {
			return candidate, true
		}
	}
	return catalogModel{}, false
}

// configURL is the workspace config endpoint on the Console.
func configURL(cfg settings) string {
	return cfg.consoleURL() + "/api/v2/config"
}

// fetchCatalog reads the live catalog with a key or login token; any key
// permission level may read it.
func fetchCatalog(ctx context.Context, doer httpDoer, cfg settings, bearer string) (catalog, error) {
	resp, errDo := doer.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     configURL(cfg),
		Headers: goRequestHeaders(cfg, bearer),
	})
	if errDo != nil {
		return catalog{}, fmt.Errorf("fetch OpenCode Go catalog: %w", errDo)
	}
	if resp.StatusCode != http.StatusOK {
		return catalog{}, parseUpstreamError(resp.StatusCode, resp.Headers, resp.Body)
	}
	return parseCatalog(resp.Body)
}

// catalogSource says where a returned catalog came from.
type catalogSource string

const (
	catalogFresh    catalogSource = "fresh"
	catalogCached   catalogSource = "cached"
	catalogStale    catalogSource = "stale"
	catalogLastGood catalogSource = "last-good"
	catalogSnapshot catalogSource = "snapshot"
)

type catalogEntry struct {
	catalog   catalog
	fetchedAt time.Time
}

// modelRegistration is the model list model.for_auth last returned for a
// credential, which is the list CPA has registered for it.
type modelRegistration struct {
	fingerprint string
	ids         []string
	at          time.Time
}

// catalogStore caches the catalog per credential ID. A failed refresh keeps
// the credential's previous catalog, then the last catalog any credential
// fetched, then the embedded snapshot, so models never vanish because the
// console is down.
type catalogStore struct {
	mu         sync.Mutex
	now        func() time.Time
	byKey      map[string]catalogEntry
	registered map[string]modelRegistration
	lastGood   *catalog
	snapshot   *catalog
}

func newCatalogStore() *catalogStore {
	return &catalogStore{now: time.Now, byKey: make(map[string]catalogEntry), registered: make(map[string]modelRegistration)}
}

var catalogs = newCatalogStore()

// forKey returns the credential's catalog, refreshing it once it is older
// than catalogTTL. errFetch is the refresh failure when a fallback was
// served.
func (s *catalogStore) forKey(ctx context.Context, doer httpDoer, cfg settings, cred credential) (catalog, catalogSource, error) {
	s.mu.Lock()
	entry, cached := s.byKey[cred.ID]
	now := s.now()
	s.mu.Unlock()
	if cached && now.Sub(entry.fetchedAt) < catalogTTL {
		return entry.catalog, catalogCached, nil
	}
	fetched, errFetch := s.refresh(ctx, doer, cfg, cred)
	if errFetch == nil {
		return fetched, catalogFresh, nil
	}
	if cached {
		return entry.catalog, catalogStale, errFetch
	}
	fallback, source := s.cached("")
	return fallback, source, errFetch
}

// refresh fetches the credential's catalog regardless of its age and caches
// it; a failure leaves the cache as it was.
func (s *catalogStore) refresh(ctx context.Context, doer httpDoer, cfg settings, cred credential) (catalog, error) {
	fetched, errFetch := fetchCatalog(ctx, doer, cfg, cred.Bearer)
	if errFetch != nil {
		return catalog{}, errFetch
	}
	s.remember(cred.ID, fetched)
	return fetched, nil
}

// fetchedAt is when the credential's cached catalog was fetched.
func (s *catalogStore) fetchedAt(credID string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.byKey[credID]
	return entry.fetchedAt, ok
}

func (s *catalogStore) markRegistered(credID string, cfg settings, cat catalog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registered[credID] = modelRegistration{fingerprint: cat.fingerprint(cfg), ids: cat.exposedIDs(cfg), at: s.now()}
}

func (s *catalogStore) registeredFor(credID string) (modelRegistration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reg, ok := s.registered[credID]
	return reg, ok
}

// fingerprint covers what decides whether CPA must register a catalog
// again: the models the settings serve, each with its family, protocol and
// token limits. It is settings-dependent so that turning a model on or off
// makes the next refresh rewrite the auth files, which is what makes CPA ask
// for the model list again.
func (c catalog) fingerprint(cfg settings) string {
	sum := sha256.New()
	for _, model := range c.servedModels(cfg) {
		fmt.Fprintf(sum, "%s|%s|%s|%d|%d|%d\n", model.Family, model.UpstreamID, model.Protocol, model.ContextLength, model.InputLimit, model.OutputLimit)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// exposedIDs are the IDs model.for_auth registers for the catalog.
func (c catalog) exposedIDs(cfg settings) []string {
	served := c.servedModels(cfg)
	ids := make([]string, 0, len(served))
	for _, model := range served {
		ids = append(ids, exposedModelID(cfg, model.Family, model.UpstreamID))
	}
	return ids
}

// cached returns the best catalog for keyID without network access.
func (s *catalogStore) cached(keyID string) (catalog, catalogSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.byKey[keyID]; ok {
		return entry.catalog, catalogCached
	}
	if s.lastGood != nil {
		return *s.lastGood, catalogLastGood
	}
	if s.snapshot == nil {
		// The embedded snapshot is covered by tests; an empty catalog keeps a
		// broken build from taking the host down.
		parsed, _ := parseCatalog(snapshotJSON)
		s.snapshot = &parsed
	}
	return *s.snapshot, catalogSnapshot
}

// lookupModel resolves a requested model for the key keyID from the cached
// catalog, without network access. It accepts the exposed ID, the bare Go
// ID and a trailing thinking suffix.
func lookupModel(model, keyID string) (modelRoute, bool) {
	cfg := currentSettings()
	cat, _ := catalogs.cached(keyID)
	found, ok := cat.find(cfg, model)
	if !ok {
		return modelRoute{}, false
	}
	return found.route(cfg), true
}

type rpcAuthModelRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// legacyExclusions are the Go model IDs that exact entries of CPA's
// oauth-excluded-models name with the configured or default prefix, which
// is how 0.2.0 disabled a model. CPA matches entries against registered
// IDs, so these stop applying once disable_prefix drops the prefix; the
// page moves them into disabled_models, and until it has, the plugin
// applies them itself.
func legacyExclusions(cfg settings, excluded map[string][]string) []string {
	if !cfg.DisablePrefix {
		return nil
	}
	var out []string
	for provider, entries := range excluded {
		if !strings.EqualFold(strings.TrimSpace(provider), providerKey) {
			continue
		}
		for _, entry := range entries {
			entry = strings.TrimSpace(entry)
			if strings.Contains(entry, "*") {
				continue
			}
			for _, prefix := range []string{cfg.ModelPrefix, defaultModelPrefix} {
				if head := len(prefix) + 1; len(entry) > head && strings.EqualFold(entry[:head], prefix+"/") {
					out = append(out, entry[head:])
					break
				}
			}
		}
	}
	return out
}

func handleModelsForAuth(raw []byte) ([]byte, error) {
	var req rpcAuthModelRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode model request: %w", errUnmarshal)
	}
	cfg := currentSettings()
	cfg = cfg.withDisabled(legacyExclusions(cfg, req.Host.ExcludedModels))
	cred, errCred := resolveCredential(req.StorageJSON, req.Attributes, req.Metadata)
	var expired *loginExpiredError
	if errors.As(errCred, &expired) {
		// The models stay registered so a request still reaches the auth, and
		// the 401 it gets makes the host refresh the login.
		cat, _ := catalogs.cached(cred.ID)
		catalogs.markRegistered(cred.ID, cfg, cat)
		return okEnvelope(pluginapi.ModelResponse{Provider: providerKey, Models: cat.modelInfos(cfg)})
	}
	if errCred != nil {
		return okEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	h := newHost(req.HostCallbackID)
	cat, source, errFetch := catalogs.forKey(context.Background(), h, cfg, cred)
	if errFetch != nil {
		h.Log("warn", "OpenCode Go catalog refresh failed; serving "+string(source)+" catalog", map[string]any{
			"auth_id": req.AuthID,
			"error":   errFetch.Error(),
		})
	}
	catalogs.markRegistered(cred.ID, cfg, cat)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerKey, Models: cat.modelInfos(cfg)})
}
