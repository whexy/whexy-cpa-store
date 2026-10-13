package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "embed"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// resourcePagePath is the page Management Center embeds under the plugin's
	// sidebar entry. Resource routes are not authenticated.
	resourcePagePath = "/v0/resource/plugins/" + pluginID + "/index.html"
	// logoPath is Metadata.Logo: Management Center shows it on the sidebar
	// entry, on the Plugins page and in the store listing.
	logoPath = "/v0/resource/plugins/" + pluginID + "/logo.svg"
	// managementBasePath prefixes the authenticated routes. The host matches
	// exact paths, so a group travels in the query or the body.
	managementBasePath  = "/v0/management/" + pluginID
	statePath           = managementBasePath + "/state"
	sessionsPath        = managementBasePath + "/sessions"
	sessionsReleasePath = sessionsPath + "/release"

	maxSessionListLimit = 100
	authLabelCacheTTL   = time.Minute
)

// managementPage holds no data: the page reads everything from the routes
// below and from Management Center's own config API.
//
//go:embed management.html
var managementPage []byte

var managementPageCSP = pageCSP(managementPage)

// logoSVG is the plugin mark; ../logo.svg is the copy the store registry
// links to.
//
//go:embed logo.svg
var logoSVG []byte

type rpcManagementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type managementRegistration struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

// managementRoute is one authenticated route under /v0/management/<plugin id>.
type managementRoute struct {
	Method      string
	Path        string
	Description string
	handle      func(req pluginapi.ManagementRequest) (int, any, error)
}

// resourceRoute is one unauthenticated page or asset under
// /v0/resource/plugins/<plugin id>.
type resourceRoute struct {
	Path        string
	Menu        string
	Description string
}

var managementRoutes = []managementRoute{
	{Method: http.MethodGet, Path: statePath, Description: "Read the groups, their members, live state and session counts.", handle: handleState},
	{Method: http.MethodGet, Path: sessionsPath, Description: "Read the sessions a group holds: member, provider and the credential CPA bound them to: ?group=&limit=.", handle: handleSessions},
	{Method: http.MethodPost, Path: sessionsReleasePath, Description: "Release one session or a whole group so it decides again: {group, session} or {group, all:true}.", handle: handleSessionsRelease},
}

func managementRegister() ([]byte, error) {
	return okEnvelope(managementRegistration{
		Routes: managementRoutes,
		Resources: []resourceRoute{
			{Path: resourcePagePath, Menu: displayName, Description: "Create groups of models CPA serves and pick how a request chooses one of them."},
			{Path: logoPath, Description: displayName + " logo."},
		},
	})
}

func managementHandle(raw []byte) ([]byte, error) {
	var req rpcManagementRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode management request: %w", errUnmarshal)
	}
	return okEnvelope(routeManagement(req.ManagementRequest))
}

func routeManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == http.MethodGet && req.Path == resourcePagePath {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":            {"text/html; charset=utf-8"},
				"Cache-Control":           {"no-cache"},
				"Content-Security-Policy": {managementPageCSP},
				"X-Content-Type-Options":  {"nosniff"},
				"Referrer-Policy":         {"no-referrer"},
			},
			Body: managementPage,
		}
	}
	if method == http.MethodGet && req.Path == logoPath {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":            {"image/svg+xml"},
				"Cache-Control":           {"public, max-age=3600"},
				"Content-Security-Policy": {"default-src 'none'"},
				"X-Content-Type-Options":  {"nosniff"},
			},
			Body: logoSVG,
		}
	}
	for _, route := range managementRoutes {
		if route.Method != method || route.Path != req.Path {
			continue
		}
		status, body, errHandle := route.handle(req)
		if errHandle != nil {
			var failure *apiError
			if errors.As(errHandle, &failure) {
				return managementJSON(failure.status, map[string]string{"error": failure.message})
			}
			return managementJSON(http.StatusInternalServerError, map[string]string{"error": errHandle.Error()})
		}
		return managementJSON(status, body)
	}
	return managementJSON(http.StatusNotFound, map[string]string{"error": "no " + pluginID + " route " + method + " " + req.Path})
}

// managementJSON encodes a route response.
func managementJSON(status int, body any) pluginapi.ManagementResponse {
	raw, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		status = http.StatusInternalServerError
		raw, _ = json.Marshal(map[string]string{"error": "encode response: " + errMarshal.Error()})
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":           {"application/json; charset=utf-8"},
			"Cache-Control":          {"no-store"},
			"X-Content-Type-Options": {"nosniff"},
		},
		Body: raw,
	}
}

// pageCSP admits only the page's own inline scripts and styles and requests to
// its own origin, so injected markup can neither run nor send the management
// key elsewhere. It sets no frame-ancestors because Management Center may be
// served from another origin than CLIProxyAPI.
func pageCSP(page []byte) string {
	return "default-src 'none'; script-src " + inlineHashes(page, "script") +
		"; style-src " + inlineHashes(page, "style") +
		"; connect-src 'self'; base-uri 'none'; form-action 'none'"
}

// inlineHashes lists the hash of every <tag> block of the page.
func inlineHashes(page []byte, tag string) string {
	open, end := []byte("<"+tag+">"), []byte("</"+tag+">")
	var hashes []string
	for {
		start := bytes.Index(page, open)
		if start < 0 {
			break
		}
		page = page[start+len(open):]
		stop := bytes.Index(page, end)
		if stop < 0 {
			break
		}
		sum := sha256.Sum256(page[:stop])
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
		page = page[stop+len(end):]
	}
	if len(hashes) == 0 {
		return "'none'"
	}
	return strings.Join(hashes, " ")
}

// apiError is a route failure shown to the reader as is.
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return e.message }

func failf(status int, format string, args ...any) error {
	return &apiError{status: status, message: fmt.Sprintf(format, args...)}
}

type stateView struct {
	Prefix           string       `json:"prefix"`
	ConfigError      string       `json:"config_error"`
	Affinity         affinityView `json:"affinity"`
	RegisteredModels []string     `json:"registered_models"`
	Providers        []string     `json:"providers"`
	ProviderError    string       `json:"provider_error,omitempty"`
	Groups           []groupView  `json:"groups"`
}

type affinityView struct {
	Enabled         bool `json:"enabled"`
	TTLSeconds      int  `json:"ttl_seconds"`
	RebindOnFailure bool `json:"rebind_on_failure"`
}

type groupView struct {
	Slug       string       `json:"slug"`
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Policy     string       `json:"policy"`
	Enabled    bool         `json:"enabled"`
	Requests1h int          `json:"requests_1h"`
	Sessions   int          `json:"sessions"`
	Members    []memberView `json:"members"`
}

type memberView struct {
	Model       string   `json:"model"`
	Providers   []string `json:"providers"`
	Weight      int      `json:"weight"`
	Requests1h  int      `json:"requests_1h"`
	Share       float64  `json:"share"`
	State       string   `json:"state"`
	StateDetail string   `json:"state_detail"`
	Credentials int      `json:"credentials"`
	Sessions    int      `json:"sessions"`
	LastError   string   `json:"last_error"`
	LastUsed    string   `json:"last_used"`
}

type sessionView struct {
	Group       string      `json:"group"`
	Session     string      `json:"session"`
	SessionFull string      `json:"session_full"`
	Member      string      `json:"member"`
	Provider    string      `json:"provider"`
	BoundAt     string      `json:"bound_at"`
	LastUsed    string      `json:"last_used"`
	Requests    int         `json:"requests"`
	LastOutcome string      `json:"last_outcome"`
	Account     accountView `json:"account"`
}

// Account states the page shows for a session.
const (
	accountBound   = "bound"
	accountUnbound = "unbound"
)

type accountView struct {
	State       string `json:"state"`
	Label       string `json:"label"`
	AuthIndex   string `json:"auth_index"`
	Disabled    bool   `json:"disabled"`
	Unavailable bool   `json:"unavailable"`
}

func handleState(pluginapi.ManagementRequest) (int, any, error) {
	now := time.Now()
	providers := pluginStateRef.catalogSnapshot()
	available, observed := pluginStateRef.availableProviders(now)

	pluginStateRef.mu.Lock()
	cfg := pluginStateRef.cfg
	configError := pluginStateRef.cfgErr
	sessions := make(map[string]int, len(pluginStateRef.groups))
	memberSessions := make(map[string]int, len(pluginStateRef.bindings))
	for _, entry := range pluginStateRef.bindings {
		group, ok := pluginStateRef.byID[entry.Group]
		if !ok {
			continue
		}
		sessions[entry.Group]++
		if index := memberIndex(group.cfg.Members, entry.Model); index >= 0 {
			memberSessions[memberKey(entry.Group, group.cfg.Members[index])]++
		}
	}
	view := stateView{
		Prefix:           cfg.Prefix,
		ConfigError:      configError,
		Affinity:         affinityView{Enabled: cfg.AffinityEnabled, TTLSeconds: int(cfg.AffinityTTL.Seconds()), RebindOnFailure: cfg.RebindOnFailure},
		RegisteredModels: cfg.registeredModelIDs(),
		Providers:        mergeProviderNames(available, providers),
		ProviderError:    providers.Err,
		Groups:           make([]groupView, 0, len(pluginStateRef.groups)),
	}
	for _, group := range pluginStateRef.groups {
		groupEntry := groupView{
			Slug:     group.cfg.Slug,
			ID:       group.cfg.ID,
			Name:     group.cfg.Name,
			Policy:   group.cfg.Policy,
			Enabled:  group.cfg.Enabled,
			Sessions: sessions[group.cfg.ID],
			Members:  make([]memberView, 0, len(group.cfg.Members)),
		}
		for index, member := range group.cfg.Members {
			stats := &group.stats[index]
			requests := stats.requestsSince(now)
			groupEntry.Requests1h += requests
			state, detail := memberState(member, stats, available, observed, now)
			groupEntry.Members = append(groupEntry.Members, memberView{
				Model:       member.Model,
				Providers:   append([]string(nil), member.Providers...),
				Weight:      member.Weight,
				Requests1h:  requests,
				State:       state,
				StateDetail: detail,
				Credentials: memberCredentials(member, providers),
				Sessions:    memberSessions[memberKey(group.cfg.ID, member)],
				LastError:   stats.LastError,
				LastUsed:    formatTime(stats.LastUsed),
			})
		}
		for index := range groupEntry.Members {
			if groupEntry.Requests1h > 0 {
				groupEntry.Members[index].Share = float64(groupEntry.Members[index].Requests1h) / float64(groupEntry.Requests1h)
			}
		}
		view.Groups = append(view.Groups, groupEntry)
	}
	pluginStateRef.mu.Unlock()
	return http.StatusOK, view, nil
}

func handleSessions(req pluginapi.ManagementRequest) (int, any, error) {
	groupID, errGroup := groupIDFromQuery(req.Query)
	if errGroup != nil {
		return 0, nil, errGroup
	}
	limit := queryInt(req.Query, "limit", 25, 1, maxSessionListLimit)

	now := time.Now()
	pluginStateRef.mu.Lock()
	entries := sessionsInGroup(pluginStateRef.bindings, groupID, limit)
	accounts := make(map[string]accountRecord, len(entries))
	for _, entry := range entries {
		if record, ok := pluginStateRef.accounts[bindingKey(groupID, entry.Session)]; ok {
			accounts[entry.Session] = record
		}
	}
	outcomes := map[string]string{}
	if group, ok := pluginStateRef.byID[groupID]; ok {
		for index, member := range group.cfg.Members {
			outcomes[member.Model] = group.stats[index].LastOutcome
		}
	}
	slug := groupID
	if group, ok := pluginStateRef.byID[groupID]; ok {
		slug = group.cfg.Slug
	}
	pluginStateRef.mu.Unlock()

	view := make([]sessionView, 0, len(entries))
	for _, entry := range entries {
		view = append(view, sessionView{
			Group:       slug,
			Session:     shortSession(entry.Session),
			SessionFull: entry.Session,
			Member:      entry.Model,
			Provider:    entry.Provider,
			BoundAt:     formatTime(entry.BoundAt),
			LastUsed:    formatTime(entry.LastUsed),
			Requests:    entry.Requests,
			LastOutcome: outcomes[entry.Model],
			Account:     accountViewFor(entry, accounts[entry.Session], now),
		})
	}
	return http.StatusOK, map[string]any{"sessions": view}, nil
}

func handleSessionsRelease(req pluginapi.ManagementRequest) (int, any, error) {
	var body struct {
		Group   string `json:"group"`
		Session string `json:"session"`
		All     bool   `json:"all"`
	}
	if len(bytes.TrimSpace(req.Body)) > 0 {
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			return 0, nil, failf(http.StatusBadRequest, "decode request body: %v", errUnmarshal)
		}
	}
	if strings.TrimSpace(body.Group) == "" {
		return 0, nil, failf(http.StatusBadRequest, "group is required")
	}
	groupID, errGroup := resolveGroupID(body.Group)
	if errGroup != nil {
		return 0, nil, errGroup
	}
	if !body.All && strings.TrimSpace(body.Session) == "" {
		return 0, nil, failf(http.StatusBadRequest, "session or all is required")
	}
	released := pluginStateRef.release(groupID, strings.TrimSpace(body.Session), body.All)
	if released > 0 {
		pluginStateRef.persist()
	}
	return http.StatusOK, map[string]int{"released": released}, nil
}

// memberState combines what the plugin can see: whether CPA still has
// credentials for the member's providers, and whether a provider is inside its
// breaker window. Whether a provider has credentials comes from the list CPA
// attaches to every model route request, not from host.auth.list, which covers
// only file-backed credentials.
func memberState(member memberConfig, stats *memberStats, available map[string]bool, observed bool, now time.Time) (string, string) {
	if len(member.Providers) == 0 {
		return "unavailable", "no provider recorded"
	}
	if observed {
		reachable := false
		for _, provider := range member.Providers {
			if available[provider] {
				reachable = true
				break
			}
		}
		if !reachable {
			return "unavailable", "no credentials"
		}
	}
	for _, provider := range member.Providers {
		remaining := stats.breakerRemaining(provider, now)
		if remaining <= 0 {
			continue
		}
		detail := fmt.Sprintf("cooling %ds", int(remaining.Seconds()+0.999))
		if stats.LastError != "" {
			detail = detail + " · " + stats.LastError
		}
		return "cooling", detail
	}
	return "ready", ""
}

// memberCredentials counts usable credentials across the member's providers.
func memberCredentials(member memberConfig, providers catalogSnapshot) int {
	total := 0
	for _, provider := range member.Providers {
		state := providers.state(provider)
		if usable := state.Credentials - state.Disabled; usable > 0 {
			total += usable
		}
	}
	return total
}

// accountViewFor describes the credential CPA selected for a session, which
// the post-credential interceptor reported.
func accountViewFor(entry binding, record accountRecord, now time.Time) accountView {
	if record.AuthIndex == "" && record.AuthID == "" {
		return accountView{State: accountUnbound}
	}
	return accountView{
		State:     accountBound,
		Label:     credentialLabel(entry.Provider, record.AuthIndex, record.AuthID, now),
		AuthIndex: record.AuthIndex,
	}
}

var (
	labelCacheMu sync.Mutex
	labelCache   = map[string]labelEntry{}
)

type labelEntry struct {
	label   string
	fetched time.Time
}

// credentialLabel names a credential for the reader. The interceptor reports
// only IDs, so the label comes from the credential itself when the host has
// one to give (file-backed credentials) and from the auth ID otherwise; it is
// cached briefly because the sessions table asks once per session.
func credentialLabel(provider, authIndex, authID string, now time.Time) string {
	labelCacheMu.Lock()
	if entry, ok := labelCache[authIndex]; ok && now.Sub(entry.fetched) < authLabelCacheTTL {
		labelCacheMu.Unlock()
		return entry.label
	}
	labelCacheMu.Unlock()

	var resp pluginapi.HostAuthGetRuntimeResponse
	label := fallbackLabel(provider, authIndex, authID)
	if errCall := invokeHost(pluginabi.MethodHostAuthGetRuntime, pluginapi.HostAuthGetRequest{AuthIndex: authIndex}, &resp); errCall == nil {
		entry := resp.Auth
		name := strings.TrimSpace(entry.Email)
		if name == "" {
			name = strings.TrimSpace(entry.Label)
		}
		if name == "" {
			name = strings.TrimSpace(entry.Account)
		}
		if name == "" {
			name = strings.TrimSpace(entry.Name)
		}
		owner := strings.TrimSpace(entry.Provider)
		if owner == "" {
			owner = strings.TrimSpace(entry.Type)
		}
		if name != "" {
			if owner != "" {
				label = owner + " · " + name
			} else {
				label = name
			}
		}
	}
	labelCacheMu.Lock()
	labelCache[authIndex] = labelEntry{label: label, fetched: now}
	labelCacheMu.Unlock()
	return label
}

// fallbackLabel names a credential the host cannot describe, which is the case
// for keys that live in config.yaml rather than in an auth file.
func fallbackLabel(provider, authIndex, authID string) string {
	name := strings.TrimSpace(authID)
	if name == "" {
		name = strings.TrimSpace(authIndex)
	}
	if name == "" {
		name = "unknown"
	}
	if provider = strings.TrimSpace(provider); provider != "" {
		return provider + " · " + name
	}
	return name
}

// mergeProviderNames lists every provider the plugin has seen, from the
// per-request availability list and from the credential cache.
func mergeProviderNames(available map[string]bool, providers catalogSnapshot) []string {
	seen := make(map[string]bool, len(available)+len(providers.Names))
	for provider := range available {
		seen[provider] = true
	}
	for _, provider := range providers.Names {
		seen[provider] = true
	}
	names := make([]string, 0, len(seen))
	for provider := range seen {
		names = append(names, provider)
	}
	sort.Strings(names)
	return names
}

func groupIDFromQuery(values url.Values) (string, error) {
	group := ""
	if values != nil {
		group = strings.TrimSpace(values.Get("group"))
	}
	if group == "" {
		return "", failf(http.StatusBadRequest, "group is required")
	}
	return resolveGroupID(group)
}

// resolveGroupID accepts the group's slug or its full model ID.
func resolveGroupID(value string) (string, error) {
	value = strings.TrimSpace(value)
	pluginStateRef.mu.Lock()
	defer pluginStateRef.mu.Unlock()
	if _, ok := pluginStateRef.byID[value]; ok {
		return value, nil
	}
	for _, group := range pluginStateRef.groups {
		if group.cfg.Slug == value {
			return group.cfg.ID, nil
		}
	}
	return "", failf(http.StatusNotFound, "no group %q in the plugin configuration", value)
}

func queryInt(values url.Values, key string, fallback, min, max int) int {
	if values == nil {
		return fallback
	}
	raw := strings.TrimSpace(values.Get(key))
	if raw == "" {
		return fallback
	}
	parsed, errParse := strconv.Atoi(raw)
	if errParse != nil {
		return fallback
	}
	if parsed < min {
		return min
	}
	if parsed > max {
		return max
	}
	return parsed
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}
