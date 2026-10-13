package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// catalogRecheckInterval is how often the plugin re-reads every catalog on
// its own, because CPA asks for models only when an auth changes. OpenCode
// adds and retires Go models days apart, and a check costs one free Console
// request per credential and rewrites files only when a list changed.
const catalogRecheckInterval = 3 * time.Hour

// registrationWait bounds how long a refresh from the page waits for CPA to
// ask for the models of the files it rewrote; CPA does that asynchronously.
var registrationWait = 5 * time.Second

// storedCredential is an enabled credential that can call OpenCode, with
// the file it lives in.
type storedCredential struct {
	cred  credential
	file  string
	login bool
}

// usableCredentials lists enabled, unexpired credentials, logins first.
func usableCredentials(files storedAuthFiles) []storedCredential {
	now := time.Now()
	var out []storedCredential
	for _, stored := range files.Logins {
		login := logins.current(stored.Login)
		if login.Disabled || login.expired(now) {
			continue
		}
		out = append(out, storedCredential{cred: login.credential(), file: stored.Name, login: true})
	}
	for _, file := range files.Keys {
		for _, entry := range file.File.Keys {
			if !entry.Disabled {
				out = append(out, storedCredential{cred: keyCredential(entry), file: file.Name})
			}
		}
	}
	return out
}

// refreshReport says what a catalog refresh found and did. Added and
// Removed compare the new catalogs with the lists CPA had registered.
type refreshReport struct {
	FetchedAt time.Time `json:"fetched_at,omitzero"`
	Added     []string  `json:"added"`
	Removed   []string  `json:"removed"`
	// Files are the auth files rewritten so CPA registers their models again.
	Files []string `json:"files"`
	// Registered reports that CPA asked for the models of every rewritten
	// credential before the refresh answered.
	Registered bool     `json:"registered"`
	Failures   []string `json:"failures,omitempty"`
}

type refreshOptions struct {
	// force rewrites every file whose catalog was read, not only those
	// whose model list changed.
	force bool
	wait  time.Duration
}

// refreshCatalogs fetches every usable credential's catalog and rewrites
// the files of those whose list differs from what CPA registered. Rewriting
// a file changes catalog_refreshed_at on each of its auths, which makes CPA
// call model.for_auth for them; that answers from the catalog just cached.
func refreshCatalogs(ctx context.Context, doer httpDoer, opts refreshOptions) (refreshReport, error) {
	files, errLoad := loadAuthFiles()
	if errLoad != nil {
		return refreshReport{}, errLoad
	}
	cfg := currentSettings()
	report := refreshReport{Added: []string{}, Removed: []string{}, Files: []string{}}
	added, removed := map[string]bool{}, map[string]bool{}
	pending := map[string][]string{}
	loginFiles := map[string]bool{}
	for _, stored := range usableCredentials(files) {
		cat, errFetch := catalogs.refresh(ctx, doer, cfg, stored.cred)
		if errFetch != nil {
			report.Failures = append(report.Failures, stored.cred.Name+": "+errFetch.Error())
			continue
		}
		if at, ok := catalogs.fetchedAt(stored.cred.ID); ok && at.After(report.FetchedAt) {
			report.FetchedAt = at
		}
		reg, known := catalogs.registeredFor(stored.cred.ID)
		if known {
			if reg.fingerprint == cat.fingerprint() && !opts.force {
				continue
			}
			now := cat.exposedIDs(cfg)
			for _, id := range now {
				if !slices.Contains(reg.ids, id) {
					added[id] = true
				}
			}
			for _, id := range reg.ids {
				if !slices.Contains(now, id) {
					removed[id] = true
				}
			}
		}
		pending[stored.file] = append(pending[stored.file], stored.cred.ID)
		loginFiles[stored.file] = stored.login
	}
	report.Added, report.Removed = sortedKeys(added), sortedKeys(removed)
	if len(pending) == 0 {
		report.Registered = len(report.Failures) == 0
		return report, nil
	}
	started := time.Now()
	mark, _ := json.Marshal(started.UTC().Format(time.RFC3339Nano))
	var waitFor []string
	for _, name := range sortedKeys(pending) {
		if errSave := setFileFields(name, loginFiles[name], fileFields{catalogRefreshedField: mark}); errSave != nil {
			report.Failures = append(report.Failures, name+": "+errSave.Error())
			continue
		}
		report.Files = append(report.Files, name)
		waitFor = append(waitFor, pending[name]...)
	}
	report.Registered = len(waitFor) > 0 && awaitRegistration(ctx, waitFor, started, opts.wait)
	return report, nil
}

// awaitRegistration waits until model.for_auth has run for every credential
// since started.
func awaitRegistration(ctx context.Context, credIDs []string, started time.Time, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for {
		done := true
		for _, id := range credIDs {
			if reg, ok := catalogs.registeredFor(id); !ok || reg.at.Before(started) {
				done = false
				break
			}
		}
		if done {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func sortedKeys[V any](set map[string]V) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

var catalogWatchOnce sync.Once

// startCatalogWatch re-reads the catalogs every catalogRecheckInterval for
// the plugin's lifetime.
func startCatalogWatch() {
	catalogWatchOnce.Do(func() {
		startBackground(func(ctx context.Context) {
			ticker := time.NewTicker(catalogRecheckInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					recheckCatalogs(ctx, newHost(""))
				}
			}
		})
	})
}

func recheckCatalogs(ctx context.Context, h host) {
	report, errRefresh := refreshCatalogs(ctx, h, refreshOptions{})
	if errRefresh != nil {
		h.Log("warn", "OpenCode Go catalog check failed", map[string]any{"error": errRefresh.Error()})
		return
	}
	for _, failure := range report.Failures {
		h.Log("warn", "OpenCode Go catalog check failed for a credential", map[string]any{"error": failure})
	}
	if len(report.Files) > 0 {
		h.Log("info", "OpenCode Go catalog changed; CPA registers the new model list", map[string]any{
			"added":   strings.Join(report.Added, ","),
			"removed": strings.Join(report.Removed, ","),
			"files":   strings.Join(report.Files, ","),
		})
	}
}

type modelView struct {
	ID         string `json:"id"`
	UpstreamID string `json:"upstream_id"`
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	Context    int64  `json:"context,omitempty"`
	Output     int64  `json:"output,omitempty"`
	Free       bool   `json:"free,omitempty"`
}

// modelsView is the catalog of the first credential that can read it.
// Models lists disabled models too; Unregistered and Unlisted leave them out.
type modelsView struct {
	Credential string `json:"credential,omitempty"`
	FetchedAt  string `json:"fetched_at,omitempty"`
	// DisablePrefix says the model IDs are bare because disable_prefix is on.
	DisablePrefix bool `json:"disable_prefix"`
	// Error says why no live catalog could be read; Models then holds the
	// last list this process read.
	Error  string      `json:"error,omitempty"`
	Models []modelView `json:"models"`
	// Unregistered are catalog models CPA has not registered for the
	// credential yet; Unlisted are registered models the catalog dropped.
	Unregistered []string `json:"unregistered"`
	Unlisted     []string `json:"unlisted"`
}

var protocolLabels = map[sdktranslator.Format]string{
	sdktranslator.FormatOpenAI:         "Chat Completions",
	sdktranslator.FormatOpenAIResponse: "Responses",
	sdktranslator.FormatClaude:         "Messages",
}

func handleListModels(h host, _ pluginapi.ManagementRequest) (int, any, error) {
	files, errLoad := loadAuthFiles()
	if errLoad != nil {
		return 0, nil, failf(http.StatusInternalServerError, "Could not read the OpenCode Go auth files: %v", errLoad)
	}
	cfg := currentSettings()
	view := modelsView{DisablePrefix: cfg.DisablePrefix, Models: []modelView{}, Unregistered: []string{}, Unlisted: []string{}}
	creds := usableCredentials(files)
	if len(creds) == 0 {
		return http.StatusOK, view, nil
	}
	var chosen storedCredential
	var cat catalog
	var failures []string
	for _, stored := range creds {
		got, _, errFetch := catalogs.forKey(context.Background(), h, cfg, stored.cred)
		if errFetch == nil {
			chosen, cat = stored, got
			break
		}
		failures = append(failures, stored.cred.Name+": "+errFetch.Error())
	}
	if chosen.cred.ID == "" {
		chosen = creds[0]
		cat, _ = catalogs.cached(chosen.cred.ID)
		view.Error = strings.Join(failures, "; ")
	}
	view.Credential = chosen.cred.Name
	if at, ok := catalogs.fetchedAt(chosen.cred.ID); ok {
		view.FetchedAt = at.UTC().Format(time.RFC3339)
	}
	for _, model := range cat.Models {
		view.Models = append(view.Models, modelView{
			ID:         exposedModelID(cfg, model.UpstreamID),
			UpstreamID: model.UpstreamID,
			Name:       model.Name,
			Protocol:   protocolLabels[model.Protocol],
			Context:    model.ContextLength,
			Output:     model.OutputLimit,
			Free:       model.Free,
		})
	}
	if reg, ok := catalogs.registeredFor(chosen.cred.ID); ok {
		ids := cat.exposedIDs(cfg)
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
	return http.StatusOK, view, nil
}

func handleRefreshModels(h host, _ pluginapi.ManagementRequest) (int, any, error) {
	report, errRefresh := refreshCatalogs(context.Background(), h, refreshOptions{force: true, wait: registrationWait})
	if errRefresh != nil {
		return 0, nil, failf(http.StatusInternalServerError, "Could not read the OpenCode Go auth files: %v", errRefresh)
	}
	if len(report.Files) == 0 && len(report.Failures) > 0 {
		return 0, nil, failf(http.StatusBadGateway, "Could not read the OpenCode catalog: %s", strings.Join(report.Failures, "; "))
	}
	if len(report.Files) == 0 {
		return 0, nil, failf(http.StatusConflict, "No enabled login or key can read the catalog.")
	}
	return http.StatusOK, report, nil
}
