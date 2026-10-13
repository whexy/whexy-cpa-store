package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// recentDecision remembers which provider served a member last, so a request
// completion that only names the model can still be attributed to a provider.
type recentDecision struct {
	Provider string
	At       time.Time
}

type target struct {
	Member   int
	Model    string
	Provider string
}

// groupRuntime is the live state of one configured group.
type groupRuntime struct {
	cfg    groupConfig
	rrNext int
	wrr    []int
	stats  []memberStats
}

// pluginState owns everything the plugin keeps between calls: the parsed
// config, the per-member counters, the session bindings, the provider cache
// and the persistence of the bindings.
type pluginState struct {
	mu       sync.Mutex
	cfg      settings
	cfgErr   string
	groups   []*groupRuntime
	byID     map[string]*groupRuntime
	bindings map[string]*binding
	recent   map[string]recentDecision
	// accounts records the credential CPA selected for the last request of a
	// session. The plugin cannot ask for it (host.affinity.lookup answers
	// "unsupported" while a plugin scheduler is installed), so it reads it
	// from the post-credential request interceptor instead.
	accounts map[string]accountRecord
	// availableSet is the provider keys CPA reported as having credentials on
	// the last model route request, and availableAt is when that was.
	// host.auth.list only covers file-backed credentials, so this is the
	// plugin's only complete view of who can serve what.
	availableSet map[string]bool
	availableAt  time.Time
	dir          string
	dirty        bool
	catalog      *catalog
}

// accountRecord is the credential CPA used for one session.
type accountRecord struct {
	AuthID    string
	AuthIndex string
	At        time.Time
}

func newPluginState(dir string) *pluginState {
	state := &pluginState{
		cfg:          defaultSettings(),
		byID:         make(map[string]*groupRuntime),
		bindings:     loadBindings(dir),
		recent:       make(map[string]recentDecision),
		accounts:     make(map[string]accountRecord),
		availableSet: make(map[string]bool),
		dir:          dir,
		catalog:      newCatalog(),
	}
	state.mu.Lock()
	state.rebuildLocked()
	state.mu.Unlock()
	return state
}

// applyConfig replaces the configuration. A config this plugin cannot parse
// keeps the previous groups running: a wrong edit in the Plugins page or in
// config.yaml must not take routing down.
func (s *pluginState) applyConfig(configYAML []byte) {
	cfg, errParse := parseSettings(configYAML)
	s.mu.Lock()
	defer s.mu.Unlock()
	if errParse != nil {
		s.cfgErr = errParse.Error()
		hostLog("warn", "model-groups: keeping the previous groups", map[string]any{"error": errParse.Error()})
		return
	}
	s.cfg = cfg
	s.cfgErr = ""
	s.rebuildLocked()
	s.reconcileBindingsLocked()
	s.dirty = true
}

// rebuildLocked rebuilds the runtime groups from the config, carrying the
// counters of members that survived the edit.
func (s *pluginState) rebuildLocked() {
	previous := make(map[string]memberStats)
	for _, group := range s.groups {
		for index, member := range group.cfg.Members {
			previous[memberKey(group.cfg.ID, member)] = group.stats[index]
		}
	}
	groups := make([]*groupRuntime, 0, len(s.cfg.Groups))
	byID := make(map[string]*groupRuntime, len(s.cfg.Groups))
	for _, group := range s.cfg.Groups {
		runtime := &groupRuntime{
			cfg:   group,
			wrr:   make([]int, len(group.Members)),
			stats: make([]memberStats, len(group.Members)),
		}
		for index, member := range group.Members {
			if stats, ok := previous[memberKey(group.ID, member)]; ok {
				runtime.stats[index] = stats
				continue
			}
			runtime.stats[index] = newMemberStats()
		}
		groups = append(groups, runtime)
		byID[group.ID] = runtime
	}
	s.groups = groups
	s.byID = byID
}

// reconcileBindingsLocked drops bindings whose group is gone; a binding whose
// member was removed is re-decided on the session's next request.
func (s *pluginState) reconcileBindingsLocked() {
	for key, entry := range s.bindings {
		if _, ok := s.byID[entry.Group]; !ok {
			delete(s.bindings, key)
			s.dirty = true
		}
	}
}

// route decides which model a group request goes to.
func (s *pluginState) route(req pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
	requested := strings.TrimSpace(req.RequestedModel)
	if requested == "" {
		return pluginapi.ModelRouteResponse{}, false
	}
	base, suffix := splitThinkingSuffix(requested)

	s.mu.Lock()
	defer s.mu.Unlock()
	group, ok := s.byID[base]
	if !ok || !group.cfg.Enabled {
		return pluginapi.ModelRouteResponse{}, false
	}

	now := time.Now()
	available := availableProviderSet(req.AvailableProviders)
	s.observeAvailableLocked(req.AvailableProviders, now)
	sessionID := sessionIdentity(req)
	chosen, ok := s.decideLocked(group, sessionID, available, now)
	if !ok {
		return pluginapi.ModelRouteResponse{}, false
	}

	group.stats[chosen.Member].recordDecision(now)
	s.recent[memberKey(group.cfg.ID, group.cfg.Members[chosen.Member])] = recentDecision{Provider: chosen.Provider, At: now}
	if sessionID != "" {
		s.bindLocked(group, sessionID, chosen, now)
	}
	s.pruneLocked(now)

	return pluginapi.ModelRouteResponse{
		Handled:     true,
		TargetKind:  pluginapi.ModelRouteTargetProvider,
		Target:      chosen.Provider,
		TargetModel: chosen.Model + suffix,
		Reason:      group.cfg.ID + " to " + chosen.Model + "@" + chosen.Provider,
	}, true
}

// observeAvailableLocked records which providers CPA says have credentials.
// Every model route request carries that list, so the newest one replaces the
// previous snapshot: an empty list means no provider can serve anything.
func (s *pluginState) observeAvailableLocked(providers []string, now time.Time) {
	set := make(map[string]bool, len(providers))
	for _, provider := range providers {
		if name := providerKeyOf(provider); name != "" {
			set[name] = true
		}
	}
	s.availableSet = set
	s.availableAt = now
}

// recordAccount remembers the credential CPA selected for a session, reported
// by the post-credential request interceptor.
func (s *pluginState) recordAccount(groupID, sessionID, authID, authIndex string, now time.Time) {
	if groupID == "" || sessionID == "" {
		return
	}
	key := bindingKey(groupID, sessionID)
	if _, ok := s.byID[groupID]; !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[key] = accountRecord{AuthID: authID, AuthIndex: authIndex, At: now}
}

// accountOf returns the recorded credential of one session.
func (s *pluginState) accountOf(groupID, sessionID string) (accountRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.accounts[bindingKey(groupID, sessionID)]
	return record, ok
}

// availableProviders reports the provider keys CPA listed most recently, and
// whether that observation is still fresh enough to draw conclusions from.
func (s *pluginState) availableProviders(now time.Time) (map[string]bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.availableAt.IsZero() || now.Sub(s.availableAt) > providerObservationTTL {
		return nil, false
	}
	set := make(map[string]bool, len(s.availableSet))
	for provider := range s.availableSet {
		set[provider] = true
	}
	return set, true
}

// decideLocked picks the member for one request. A session that is already
// bound keeps its member as long as that member can still be served, because
// CPA's own account binding is keyed by (session, model): moving the session
// to another model would also move it to another credential.
func (s *pluginState) decideLocked(group *groupRuntime, sessionID string, available map[string]bool, now time.Time) (target, bool) {
	members := group.cfg.Members
	if len(members) == 0 {
		return target{}, false
	}

	var bound *binding
	// excluded is the member a session is being moved off, so the decision
	// below cannot put it straight back on the member that just failed.
	excluded := -1
	if sessionID != "" {
		bound = s.bindings[bindingKey(group.cfg.ID, sessionID)]
	}
	if bound != nil {
		if index := memberIndex(members, bound.Model); index >= 0 {
			member := members[index]
			stats := &group.stats[index]
			if !(s.cfg.RebindOnFailure && stats.failedRecently(now)) {
				if provider, ok := usableProvider(member, bound.Provider, available); ok {
					return target{Member: index, Model: member.Model, Provider: provider}, true
				}
			} else {
				excluded = index
			}
		}
	}

	healthy := make([]int, 0, len(members))
	reachable := make([]int, 0, len(members))
	for index, member := range members {
		if index == excluded || len(member.Providers) == 0 {
			continue
		}
		if !servesNow(member, available) {
			continue
		}
		reachable = append(reachable, index)
		if _, healthyNow := healthyProvider(group, index, member, "", available, now); healthyNow {
			healthy = append(healthy, index)
		}
	}

	candidates := healthy
	if len(candidates) == 0 {
		// Nothing is healthy: still route somewhere reachable so CPA reports
		// the real reason instead of the group silently vanishing.
		candidates = reachable
	}
	if len(candidates) == 0 && excluded >= 0 {
		// The member being left is the only one that can serve; keeping the
		// session alive matters more than the move.
		candidates = []int{excluded}
	}
	if len(candidates) == 0 {
		if bound != nil {
			if index := memberIndex(members, bound.Model); index >= 0 && len(members[index].Providers) > 0 {
				return target{Member: index, Model: members[index].Model, Provider: preferredProvider(members[index], bound.Provider)}, true
			}
		}
		if len(members[0].Providers) == 0 {
			return target{}, false
		}
		return target{Member: 0, Model: members[0].Model, Provider: members[0].Providers[0]}, true
	}

	index := s.pickLocked(group, candidates)
	member := members[index]
	preferred := ""
	if bound != nil && bound.Model == member.Model {
		preferred = bound.Provider
	}
	if preferred == "" {
		if recent, ok := s.recent[memberKey(group.cfg.ID, member)]; ok && now.Sub(recent.At) < time.Minute {
			preferred = recent.Provider
		}
	}
	provider, okProvider := healthyProvider(group, index, member, preferred, available, now)
	if !okProvider {
		provider, _ = usableProvider(member, preferred, available)
	}
	return target{Member: index, Model: member.Model, Provider: provider}, true
}

// pickLocked applies the group's policy over the candidate member indexes.
func (s *pluginState) pickLocked(group *groupRuntime, candidates []int) int {
	members := group.cfg.Members
	switch group.cfg.Policy {
	case policyFillFirst:
		return candidates[0]

	case policyRoundRobin:
		for offset := 0; offset < len(members); offset++ {
			index := (group.rrNext + offset) % len(members)
			if containsInt(candidates, index) {
				group.rrNext = (index + 1) % len(members)
				return index
			}
		}
		return candidates[0]

	default:
		// Smooth weighted round robin, the same shape CPA uses for
		// credentials: add each weight, take the largest, subtract the total.
		// Ties go to the earlier member. A member that cannot serve right now
		// is held at zero so its share does not burst when it returns.
		total := 0
		best := -1
		for index, member := range members {
			if !containsInt(candidates, index) {
				group.wrr[index] = 0
				continue
			}
			group.wrr[index] += member.Weight
			total += member.Weight
			if best < 0 || group.wrr[index] > group.wrr[best] {
				best = index
			}
		}
		if best < 0 {
			return candidates[0]
		}
		group.wrr[best] -= total
		return best
	}
}

// bindLocked records the session's member, keeping the member while its
// provider list still contains the provider we routed to.
func (s *pluginState) bindLocked(group *groupRuntime, sessionID string, chosen target, now time.Time) {
	key := bindingKey(group.cfg.ID, sessionID)
	if entry, ok := s.bindings[key]; ok && entry.Model == chosen.Model && entry.Provider == chosen.Provider {
		entry.LastUsed = now
		entry.Requests++
		// The sliding lifetime has to survive a restart, so an updated
		// binding is written again; the persistence loop debounces it.
		s.dirty = true
		return
	}
	s.bindings[key] = &binding{
		Group:    group.cfg.ID,
		Session:  sessionID,
		Model:    chosen.Model,
		Provider: chosen.Provider,
		BoundAt:  now,
		LastUsed: now,
		Requests: 1,
	}
	s.dirty = true
}

// observeCompletion attributes a finished request to the member that served
// it, which is what feeds the member counters and the breaker.
func (s *pluginState) observeCompletion(completion pluginapi.RequestCompletion) {
	requested := strings.TrimSpace(completion.RequestedModel)
	base, _ := splitThinkingSuffix(requested)
	served, _ := splitThinkingSuffix(strings.TrimSpace(completion.Model))

	s.mu.Lock()
	defer s.mu.Unlock()
	group, ok := s.byID[base]
	if !ok {
		return
	}
	index := memberIndex(group.cfg.Members, served)
	if index < 0 {
		return
	}
	member := group.cfg.Members[index]
	stats := &group.stats[index]
	now := time.Now()

	provider := ""
	if recent, ok := s.recent[memberKey(group.cfg.ID, member)]; ok && now.Sub(recent.At) < time.Minute {
		provider = recent.Provider
	}
	if provider == "" && len(member.Providers) == 1 {
		provider = member.Providers[0]
	}
	if provider == "" {
		provider = strings.Join(member.Providers, ",")
	}

	switch completion.Outcome {
	case pluginapi.RequestCompletionSucceeded:
		stats.recordSuccess(provider, now)
	case pluginapi.RequestCompletionCanceled:
		stats.recordCanceled()
	case pluginapi.RequestCompletionRejected:
		// Terminated by another plugin before an upstream attempt; that says
		// nothing about this member.
	default:
		switch {
		case completion.StatusCode == 0:
			// A stream that failed after it started reports no status; for a
			// member several providers serve, an unknown provider is not
			// enough to blame one of them.
			stats.recordFailure(provider, now, completionError(completion))
		case failureStatus(completion.StatusCode):
			stats.recordFailure(provider, now, completionError(completion))
		default:
			stats.recordClientError(completionError(completion))
		}
	}
}

// release drops one session's binding, or every binding of a group.
func (s *pluginState) release(groupID, sessionID string, all bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	released := 0
	for key, entry := range s.bindings {
		if entry.Group != groupID {
			continue
		}
		if !all && entry.Session != sessionID {
			continue
		}
		delete(s.bindings, key)
		released++
	}
	if released > 0 {
		s.dirty = true
	}
	return released
}

// pruneLocked expires old bindings, caps the stored sessions and prunes the
// per-minute counters.
func (s *pluginState) pruneLocked(now time.Time) {
	before := len(s.bindings)
	pruneBindings(s.bindings, now, s.cfg.AffinityTTL, maxSessionKeys)
	if len(s.bindings) != before {
		s.dirty = true
	}
	for _, group := range s.groups {
		for index := range group.stats {
			group.stats[index].prune(now)
		}
	}
	for key, entry := range s.recent {
		if now.Sub(entry.At) > 5*time.Minute {
			delete(s.recent, key)
		}
	}
	for key, entry := range s.accounts {
		if now.Sub(entry.At) > s.cfg.AffinityTTL {
			delete(s.accounts, key)
		}
	}
}

// persist writes the bindings when they changed.
func (s *pluginState) persist() {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	s.dirty = false
	bindings := make(map[string]*binding, len(s.bindings))
	for key, entry := range s.bindings {
		bindings[key] = entry
	}
	dir := s.dir
	s.mu.Unlock()

	if errSave := saveBindings(dir, bindings); errSave != nil {
		hostLog("warn", "model-groups: cannot persist session bindings", map[string]any{"error": errSave.Error()})
	}
}

func memberIndex(members []memberConfig, model string) int {
	for index, member := range members {
		if member.Model == model {
			return index
		}
	}
	return -1
}

func memberKey(groupID string, member memberConfig) string {
	return groupID + "\x00" + member.Model + "\x00" + strings.Join(member.Providers, ",")
}

func availableProviderSet(providers []string) map[string]bool {
	set := make(map[string]bool, len(providers))
	for _, provider := range providers {
		if name := providerKeyOf(provider); name != "" {
			set[name] = true
		}
	}
	return set
}

// usableProvider returns the provider to route this member to, preferring the
// one the session already used. It returns false when the member has no
// provider CPA has credentials for, or when every one of them is inside its
// breaker window.
func usableProvider(member memberConfig, preferred string, available map[string]bool) (string, bool) {
	if len(member.Providers) == 0 {
		return "", false
	}
	if preferred != "" && containsString(member.Providers, preferred) && available[providerKeyOf(preferred)] {
		return providerKeyOf(preferred), true
	}
	for _, provider := range member.Providers {
		if available[provider] {
			return provider, true
		}
	}
	return "", false
}

// healthyProvider returns the provider to route a fresh decision to: the
// preferred one when it can still serve, otherwise the first configured one
// that CPA has credentials for and whose breaker is not tripped.
func healthyProvider(group *groupRuntime, index int, member memberConfig, preferred string, available map[string]bool, now time.Time) (string, bool) {
	stats := &group.stats[index]
	if preferred != "" {
		normalized := providerKeyOf(preferred)
		if containsString(member.Providers, normalized) && available[normalized] && !stats.blocked(normalized, now) {
			return normalized, true
		}
	}
	for _, provider := range member.Providers {
		if available[provider] && !stats.blocked(provider, now) {
			return provider, true
		}
	}
	return "", false
}

// servesNow reports whether the member has any provider with credentials at
// all, ignoring breaker windows.
func servesNow(member memberConfig, available map[string]bool) bool {
	for _, provider := range member.Providers {
		if available[provider] {
			return true
		}
	}
	return false
}

func preferredProvider(member memberConfig, preferred string) string {
	if preferred != "" && containsString(member.Providers, preferred) {
		return preferred
	}
	if len(member.Providers) == 0 {
		return ""
	}
	return member.Providers[0]
}

func containsInt(values []int, wanted int) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func completionError(completion pluginapi.RequestCompletion) string {
	message := strings.TrimSpace(completion.Error)
	if len(message) > 200 {
		message = message[:200]
	}
	if message != "" {
		return message
	}
	if completion.StatusCode > 0 {
		return fmt.Sprintf("HTTP %d", completion.StatusCode)
	}
	return "request failed"
}

// sortedGroupIDs lists the registered model IDs, for the page.
func (s *pluginState) registeredModelIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := s.cfg.registeredModelIDs()
	sort.Strings(ids)
	return ids
}

// settingsSnapshot returns the parsed configuration the router is using.
func (s *pluginState) settingsSnapshot() settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// catalogSnapshot returns the cached provider view.
func (s *pluginState) catalogSnapshot() catalogSnapshot {
	return s.catalog.snapshot()
}
