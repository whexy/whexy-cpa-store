package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const catalogTTL = 10 * time.Minute

// modelFamily is which OpenCode catalog a model belongs to: the Go
// subscription gateway, the pay-as-you-go Console (OpenCode Zen) gateway, or
// the free models that gateway serves without a key (OpenCode Free).
type modelFamily string

const (
	familyGo   modelFamily = "go"
	familyZen  modelFamily = "zen"
	familyFree modelFamily = "free"
)

// catalog is the model list of one workspace: the Go subscription models and
// the pay-as-you-go (OpenCode Zen) models, as models.dev describes them with
// the workspace's Console config applied.
type catalog struct {
	// Workspace is the workspace ID the key belongs to, empty before any
	// workspace config has been read.
	Workspace string
	Models    []catalogModel
	// Zen holds the OpenCode Zen models the workspace's Console has not
	// disabled. The plugin serves one only once the page turns it on,
	// because those calls bill Console credits.
	Zen []catalogModel
	// Free holds the models OpenCode serves without a key; it is read from
	// public catalogs, not a workspace's.
	Free []catalogModel
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

// configProvider is one provider of a workspace's Console config. What it
// sets overrides models.dev; a model it does not list is served as
// models.dev describes it.
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
	// Cost replaces models.dev's prices whole: one tier or a list of them.
	Cost json.RawMessage `json:"cost"`
	// Limit replaces only the limits it names.
	Limit struct {
		Context *int64 `json:"context"`
		Input   *int64 `json:"input"`
		Output  *int64 `json:"output"`
	} `json:"limit"`
}

// workspaceConfig is the part of a workspace's Console config the plugin
// applies over models.dev.
type workspaceConfig struct {
	Workspace string
	Go        configProvider
	Zen       configProvider
}

// parseWorkspaceConfig reads providers["opencode-go"] and providers["opencode"]
// of the workspace config. The Go provider must be there, and its org header
// names the workspace; a config without the Zen provider changes nothing of
// models.dev's Zen models.
func parseWorkspaceConfig(raw []byte) (workspaceConfig, error) {
	var doc configDocument
	if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil {
		return workspaceConfig{}, fmt.Errorf("decode workspace config: %w", errUnmarshal)
	}
	provider, ok := doc.Providers[providerKey]
	if !ok {
		return workspaceConfig{}, fmt.Errorf("workspace config has no %s provider", providerKey)
	}
	return workspaceConfig{
		Workspace: strings.TrimSpace(provider.Headers["x-opencode-org-id"]),
		Go:        provider,
		Zen:       doc.Providers[zenProviderKey],
	}, nil
}

// buildCatalog applies a workspace's Console config over models.dev's
// catalog.
func buildCatalog(base *modelsDevCatalog, config workspaceConfig) catalog {
	return catalog{
		Workspace: config.Workspace,
		Models:    buildModels(familyGo, base.Go, config.Go),
		Zen:       buildModels(familyZen, base.Zen, config.Zen),
	}
}

// buildModels lists one provider's models the way OpenCode's client
// (packages/core/src/plugin/provider/opencode.ts) applies the Console config
// over models.dev: every models.dev model the config does not disable, with
// the fields the config sets for it, and every model only the config lists.
// Deprecated models.dev models are left out, as the client hides them, and so
// are models whose package or modalities the plugin cannot serve.
func buildModels(family modelFamily, base modelsDevProvider, overlay configProvider) []catalogModel {
	listed := make(map[string]configModel, len(overlay.Models))
	for id, model := range overlay.Models {
		listed[strings.TrimSpace(id)] = model
	}
	ids := make(map[string]bool, len(base.Models)+len(listed))
	for id, model := range base.Models {
		if !deprecated(model) {
			ids[id] = true
		}
	}
	for id := range listed {
		ids[id] = true
	}
	var out []catalogModel
	for id := range ids {
		override, isListed := listed[id]
		if id == "" || override.Disabled {
			continue
		}
		source := base.Models[id]
		if deprecated(source) {
			source = modelsDevModel{}
		}
		pkg, baseURL := source.Provider.NPM, source.Provider.API
		// The client lets a package or base URL the config sets for the
		// provider replace a listed model's own from models.dev.
		if isListed && overlay.Package != "" {
			pkg = ""
		}
		if isListed && overlay.Settings.BaseURL != "" {
			baseURL = ""
		}
		protocol, ok := protocolForPackage(cmp.Or(override.Package, pkg, overlay.Package, base.NPM))
		if !ok {
			continue
		}
		input, output := source.Modalities.Input, source.Modalities.Output
		if override.Capabilities.Input != nil {
			input = override.Capabilities.Input
		}
		if override.Capabilities.Output != nil {
			output = override.Capabilities.Output
		}
		if !handlesText(input) || !handlesText(output) {
			continue
		}
		limit := source.Limit
		if override.Limit.Context != nil {
			limit.Context = *override.Limit.Context
		}
		if override.Limit.Input != nil {
			limit.Input = *override.Limit.Input
		}
		if override.Limit.Output != nil {
			limit.Output = *override.Limit.Output
		}
		cost := source.Cost
		if !isJSONNull(override.Cost) {
			cost = override.Cost
		}
		out = append(out, catalogModel{
			Family:           family,
			UpstreamID:       id,
			Name:             cmp.Or(strings.TrimSpace(override.Name), strings.TrimSpace(source.Name), id),
			Protocol:         protocol,
			BaseURL:          strings.TrimRight(strings.TrimSpace(cmp.Or(override.Settings.BaseURL, baseURL, overlay.Settings.BaseURL, base.API)), "/"),
			Free:             zeroCost(cost),
			ContextLength:    limit.Context,
			InputLimit:       limit.Input,
			OutputLimit:      limit.Output,
			InputModalities:  input,
			OutputModalities: output,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpstreamID < out[j].UpstreamID })
	return out
}

func deprecated(model modelsDevModel) bool {
	return strings.EqualFold(model.Status, "deprecated")
}

// handlesText reports modalities that include text, or name none; the
// plugin's endpoints carry text only.
func handlesText(modalities []string) bool {
	if len(modalities) == 0 {
		return true
	}
	for _, modality := range modalities {
		if strings.HasPrefix(modality, "text") {
			return true
		}
	}
	return false
}

// protocolForPackage maps the AI SDK package a model names to the endpoint
// the gateway accepts for that model. Responses- and Messages-family models
// reject every other endpoint. Both gateways also serve packages this plugin
// cannot speak, such as Gemini's, so it drops those models rather than send
// them to the wrong endpoint.
func protocolForPackage(pkg string) (sdktranslator.Format, bool) {
	switch strings.TrimPrefix(strings.TrimSpace(pkg), "aisdk:") {
	case "@ai-sdk/anthropic":
		return sdktranslator.FormatClaude, true
	case "@ai-sdk/openai":
		return sdktranslator.FormatOpenAIResponse, true
	case "@ai-sdk/openai-compatible":
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
	if family == familyGo {
		return true
	}
	return strings.HasPrefix(rel, "openai/") || strings.HasPrefix(rel, "anthropic/")
}

// exposedModelID is the ID clients request. The prefix is part of the ID
// itself rather than AuthData.Prefix, because the host also registers the
// bare ID for a prefixed auth unless force-model-prefix is on, and a bare ID
// that a native provider serves would share its traffic.
func exposedModelID(cfg settings, family modelFamily, upstreamID string) string {
	prefix := cfg.modelPrefix(family)
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

// served are the catalog models of one family the settings register with
// CPA: every Go or Free model except the disabled ones, or the Zen models
// the page has turned on.
func (c catalog) served(cfg settings, family modelFamily) []catalogModel {
	var out []catalogModel
	switch family {
	case familyZen:
		for _, model := range c.Zen {
			if cfg.zenEnabled(model.UpstreamID) {
				out = append(out, model)
			}
		}
	case familyFree:
		for _, model := range c.Free {
			if !cfg.freeDisabled(model.UpstreamID) {
				out = append(out, model)
			}
		}
	default:
		for _, model := range c.Models {
			if !cfg.modelDisabled(model.UpstreamID) {
				out = append(out, model)
			}
		}
	}
	return out
}

// servedCount is how many models a credential serves through the families
// it has on.
func (c catalog) servedCount(cfg settings, set familySet) int {
	count := 0
	for _, family := range []modelFamily{familyGo, familyZen} {
		if set.has(family) {
			count += len(c.served(cfg, family))
		}
	}
	return count
}

func (c catalog) modelInfos(cfg settings, family modelFamily) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(c.Models)+len(c.Zen))
	for _, model := range c.served(cfg, family) {
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

// find resolves a model ID requested of one family's auth, optionally with
// a CPA thinking suffix such as "(high)": the prefixed or the bare ID of a
// Go model, of a Zen model the page has turned on, or of a Free model it has
// not disabled.
func (c catalog) find(cfg settings, model string, family modelFamily) (catalogModel, bool) {
	model = strings.TrimSpace(model)
	if open := strings.LastIndex(model, "("); open > 0 && strings.HasSuffix(model, ")") {
		model = model[:open]
	}
	switch family {
	case familyZen:
		model = strings.TrimPrefix(model, cfg.zenPrefix()+"/")
		for _, candidate := range c.Zen {
			if candidate.UpstreamID == model && cfg.zenEnabled(model) {
				return candidate, true
			}
		}
		return catalogModel{}, false
	case familyFree:
		model = strings.TrimPrefix(model, freeModelPrefix+"/")
		for _, candidate := range c.Free {
			if candidate.UpstreamID == model && !cfg.freeDisabled(model) {
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

// fetchWorkspaceConfig reads the workspace's Console config with a key or
// login token; any key permission level may read it.
func fetchWorkspaceConfig(ctx context.Context, doer httpDoer, cfg settings, bearer string) (workspaceConfig, error) {
	resp, errDo := doer.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     configURL(cfg),
		Headers: goRequestHeaders(cfg, bearer),
	})
	if errDo != nil {
		return workspaceConfig{}, fmt.Errorf("fetch OpenCode workspace config: %w", errDo)
	}
	if resp.StatusCode != http.StatusOK {
		return workspaceConfig{}, parseUpstreamError(resp.StatusCode, resp.Headers, resp.Body)
	}
	return parseWorkspaceConfig(resp.Body)
}

// catalogSource says where a returned catalog came from.
type catalogSource string

const (
	catalogFresh    catalogSource = "fresh"
	catalogCached   catalogSource = "cached"
	catalogStale    catalogSource = "stale"
	catalogLastGood catalogSource = "last-good"
	// catalogDefault is models.dev's catalog without any workspace config.
	catalogDefault  catalogSource = "default"
	catalogSnapshot catalogSource = "snapshot"
)

type catalogEntry struct {
	catalog   catalog
	fetchedAt time.Time
}

// configEntry is one workspace config and the catalog it makes of base, the
// models.dev catalog it was last applied to.
type configEntry struct {
	config    workspaceConfig
	fetchedAt time.Time
	base      *modelsDevCatalog
	catalog   catalog
}

// catalogOf returns the catalog the config makes of base, building it again
// only after models.dev's catalog changed. The caller holds the store's lock.
func (e *configEntry) catalogOf(base *modelsDevCatalog) catalog {
	if e.base != base {
		e.catalog, e.base = buildCatalog(base, e.config), base
	}
	return e.catalog
}

// modelRegistration is the model list model.for_auth last returned for one
// family's auth of a credential, which is the list CPA has registered for
// it.
type modelRegistration struct {
	fingerprint string
	ids         []string
	at          time.Time
}

// catalogStore caches the workspace config per credential ID; a credential's
// catalog is models.dev's current one with that config applied. A failed
// read keeps the credential's previous config, then the last config any
// credential read, then none, so models never vanish because the Console is
// down.
type catalogStore struct {
	mu         sync.Mutex
	now        func() time.Time
	byKey      map[string]*configEntry
	registered map[string]modelRegistration
	lastGood   *configEntry
	// unconfigured holds no workspace config.
	unconfigured *configEntry
}

func newCatalogStore() *catalogStore {
	return &catalogStore{
		now:          time.Now,
		byKey:        make(map[string]*configEntry),
		registered:   make(map[string]modelRegistration),
		unconfigured: &configEntry{},
	}
}

var catalogs = newCatalogStore()

// forKey returns the credential's catalog of base, reading its workspace
// config again once that is older than catalogTTL. errFetch is the read
// failure when a fallback config was applied.
func (s *catalogStore) forKey(ctx context.Context, doer httpDoer, cfg settings, cred credential, base *modelsDevCatalog) (catalog, catalogSource, error) {
	s.mu.Lock()
	entry, cached := s.byKey[cred.ID]
	fresh := cached && s.now().Sub(entry.fetchedAt) < catalogTTL
	s.mu.Unlock()
	if fresh {
		cat, _ := s.cachedOver(cred.ID, base)
		return cat, catalogCached, nil
	}
	fetched, errFetch := s.refresh(ctx, doer, cfg, cred, base)
	if errFetch == nil {
		return fetched, catalogFresh, nil
	}
	cat, source := s.cachedOver(cred.ID, base)
	if source == catalogCached {
		source = catalogStale
	}
	return cat, source, errFetch
}

// refresh reads the credential's workspace config regardless of its age and
// returns the catalog it makes of base; a failure leaves the cache as it was.
func (s *catalogStore) refresh(ctx context.Context, doer httpDoer, cfg settings, cred credential, base *modelsDevCatalog) (catalog, error) {
	config, errFetch := fetchWorkspaceConfig(ctx, doer, cfg, cred.Bearer)
	if errFetch != nil {
		return catalog{}, errFetch
	}
	s.remember(cred.ID, config)
	cat, _ := s.cachedOver(cred.ID, base)
	return cat, nil
}

// fetchedAt is when the credential's cached workspace config was read.
func (s *catalogStore) fetchedAt(credID string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.byKey[credID]
	if !ok {
		return time.Time{}, false
	}
	return entry.fetchedAt, true
}

// registrationKey names one family's auth of a credential.
func registrationKey(credID string, family modelFamily) string {
	return credID + "|" + string(family)
}

func (s *catalogStore) markRegistered(credID string, family modelFamily, cfg settings, cat catalog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registered[registrationKey(credID, family)] = modelRegistration{fingerprint: cat.fingerprint(cfg, family), ids: cat.exposedIDs(cfg, family), at: s.now()}
}

func (s *catalogStore) registeredFor(credID string, family modelFamily) (modelRegistration, bool) {
	return s.registration(registrationKey(credID, family))
}

func (s *catalogStore) registration(key string) (modelRegistration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reg, ok := s.registered[key]
	return reg, ok
}

// fingerprint covers what decides whether CPA must register one family's
// list again: the models the settings serve, each with its protocol and
// token limits. It is settings-dependent so that turning a model on or off
// makes the next refresh rewrite the auth files, which is what makes CPA ask
// for the model list again.
func (c catalog) fingerprint(cfg settings, family modelFamily) string {
	sum := sha256.New()
	for _, model := range c.served(cfg, family) {
		fmt.Fprintf(sum, "%s|%s|%s|%d|%d|%d\n", model.Family, model.UpstreamID, model.Protocol, model.ContextLength, model.InputLimit, model.OutputLimit)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// exposedIDs are the IDs model.for_auth registers for one family's auth.
func (c catalog) exposedIDs(cfg settings, family modelFamily) []string {
	served := c.served(cfg, family)
	ids := make([]string, 0, len(served))
	for _, model := range served {
		ids = append(ids, exposedModelID(cfg, model.Family, model.UpstreamID))
	}
	return ids
}

// catalogFor returns the catalog one family's auth of a credential serves
// from: models.dev's with the workspace's config applied for Go and Zen, the
// public free one for Free. While models.dev cannot be read, the source is
// that of the models.dev catalog used instead.
func catalogFor(ctx context.Context, doer httpDoer, cfg settings, cred credential, family modelFamily) (catalog, catalogSource, error) {
	if family == familyFree {
		return freeCatalogs.get(ctx, doer, cfg)
	}
	base, baseSource, errBase := modelsDev.get(ctx, doer, cfg)
	cat, source, errFetch := catalogs.forKey(ctx, doer, cfg, cred, base)
	if errBase != nil {
		source = baseSource
	}
	return cat, source, errors.Join(errFetch, errBase)
}

func cachedCatalog(credID string, family modelFamily) (catalog, catalogSource) {
	if family == familyFree {
		return freeCatalogs.cached()
	}
	return catalogs.cached(credID)
}

// cached returns the best catalog for keyID without network access.
func (s *catalogStore) cached(keyID string) (catalog, catalogSource) {
	base, _ := modelsDev.cached()
	return s.cachedOver(keyID, base)
}

// cachedOver returns the catalog the best config cached for keyID makes of
// base.
func (s *catalogStore) cachedOver(keyID string, base *modelsDevCatalog) (catalog, catalogSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.byKey[keyID]; ok {
		return entry.catalogOf(base), catalogCached
	}
	if s.lastGood != nil {
		return s.lastGood.catalogOf(base), catalogLastGood
	}
	return s.unconfigured.catalogOf(base), catalogDefault
}

// lookupModel resolves a model requested of one family's auth of the key
// keyID from the cached catalog, without network access. It accepts the
// exposed ID, the bare ID and a trailing thinking suffix.
func lookupModel(model, keyID string, family modelFamily) (modelRoute, bool) {
	cfg := currentSettings()
	cat, _ := cachedCatalog(keyID, family)
	found, ok := cat.find(cfg, model, family)
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

// staleReparsed records that model.for_auth met a file auth an earlier
// plugin version parsed: one without a family attribute, which would serve
// Go models only, or one with a model_aliases attribute, which would list a
// model under its prefixed and its bare ID at once. CPA keeps such auths
// until their file changes, so the plugin has CPA parse every file again,
// once.
var staleReparsed atomic.Bool

// reparseLater runs reparseAuthFiles off the host's model call; tests
// replace it.
var reparseLater = func() {
	startBackground(func(ctx context.Context) { reparseAuthFiles(ctx, newHost("")) })
}

func staleAuth(attributes map[string]string) bool {
	return attributes[pathAttribute] != "" && (attributes[familyAttribute] == "" || attributes[modelAliasesAttribute] != "")
}

func handleModelsForAuth(raw []byte) ([]byte, error) {
	var req rpcAuthModelRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode model request: %w", errUnmarshal)
	}
	if staleAuth(req.Attributes) && staleReparsed.CompareAndSwap(false, true) {
		reparseLater()
	}
	cfg := currentSettings()
	cfg = cfg.withDisabled(legacyExclusions(cfg, req.Host.ExcludedModels))
	family := authFamily(req.Attributes)
	cred, errCred := resolveCredential(req.StorageJSON, req.Attributes, req.Metadata)
	var expired *loginExpiredError
	if errors.As(errCred, &expired) {
		// The models stay registered so a request still reaches the auth, and
		// the 401 it gets makes the host refresh the login.
		cat, _ := cachedCatalog(cred.ID, family)
		catalogs.markRegistered(cred.ID, family, cfg, cat)
		return okEnvelope(pluginapi.ModelResponse{Provider: providerKey, Models: cat.modelInfos(cfg, family)})
	}
	if errCred != nil {
		return okEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	h := newHost(req.HostCallbackID)
	cat, source, errFetch := catalogFor(context.Background(), h, cfg, cred, family)
	if errFetch != nil {
		h.Log("warn", "OpenCode "+string(family)+" catalog refresh failed; serving "+string(source)+" catalog", map[string]any{
			"auth_id": req.AuthID,
			"error":   errFetch.Error(),
		})
	}
	catalogs.markRegistered(cred.ID, family, cfg, cat)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerKey, Models: cat.modelInfos(cfg, family)})
}
