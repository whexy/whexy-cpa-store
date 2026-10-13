package main

import (
	"sort"
	"strings"
	"sync"
)

// providerState is what the plugin can count for one provider: the credentials
// the host lists for it, and how many of those are disabled. The plugin cannot
// see CPA's model registry (it runs in its own Go runtime), so which models a
// provider serves is resolved in the editor and stored in the group config,
// and whether a provider has any usable credential at all comes from the list
// CPA attaches to every model route request.
type providerState struct {
	Credentials int
	Disabled    int
}

type catalogSnapshot struct {
	Err       string
	Providers map[string]providerState
	Names     []string
}

// catalog caches host.auth.list so routing never blocks on a host call.
// Routing does not depend on it for correctness: the live provider list comes
// from each model route request, and the cache only adds credential counts
// and gives the page something to show.
type catalog struct {
	mu        sync.Mutex
	err       string
	providers map[string]providerState
}

func newCatalog() *catalog {
	return &catalog{providers: make(map[string]providerState)}
}

// refresh rebuilds the provider view from every credential the host has.
func (c *catalog) refresh() {
	entries, errList := hostAuthList()
	if errList != nil {
		c.mu.Lock()
		c.err = errList.Error()
		c.mu.Unlock()
		return
	}
	providers := make(map[string]providerState, len(entries))
	for _, entry := range entries {
		name := providerKeyOf(entry.Provider)
		if name == "" {
			name = providerKeyOf(entry.Type)
		}
		if name == "" {
			continue
		}
		state := providers[name]
		state.Credentials++
		if entry.Disabled || strings.EqualFold(strings.TrimSpace(entry.Status), "disabled") {
			state.Disabled++
		}
		providers[name] = state
	}

	c.mu.Lock()
	c.providers = providers
	c.err = ""
	c.mu.Unlock()
}

func (c *catalog) snapshot() catalogSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	providers := make(map[string]providerState, len(c.providers))
	names := make([]string, 0, len(c.providers))
	for name, state := range c.providers {
		providers[name] = state
		names = append(names, name)
	}
	sort.Strings(names)
	return catalogSnapshot{Err: c.err, Providers: providers, Names: names}
}

// state returns the cached entry for one provider.
func (s catalogSnapshot) state(provider string) providerState {
	return s.Providers[providerKeyOf(provider)]
}
