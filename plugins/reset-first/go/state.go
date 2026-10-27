package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	stateFileName = "state.json"
	stateVersion  = 1

	// history is how far back decision cards and request counts reach.
	history = 24 * time.Hour
)

// account is one CPA credential as this plugin knows it, keyed by the auth ID
// that scheduler candidates, usage records and host.auth.list share.
type account struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Label    string `json:"label,omitempty"`
	Plan     string `json:"plan,omitempty"`
	Quota    *quota `json:"quota,omitempty"`
	// Problem says why the usage endpoint could not be read; empty once it
	// was, or when the account was never polled.
	Problem string `json:"problem,omitempty"`

	authIndex string
	// listed: in the latest host.auth.list.
	listed bool
	// pollable: listed, enabled, and of a provider with a usage endpoint.
	pollable bool
	nextPoll time.Time
	trialAt  time.Time
	// rejected fingerprints the access token the usage endpoint rejected;
	// the endpoint is read again only with another token.
	rejected string
	// limited counts the usage endpoint's consecutive 429 answers, and no
	// read starts before retryAt, whatever responses do to nextPoll.
	limited int
	retryAt time.Time
}

// state is everything the scheduler answers from. scheduler.pick runs inside
// every credential selection, concurrently, so it takes the lock only for
// in-memory work; host calls happen in the background loop.
type state struct {
	mu        sync.Mutex
	accounts  map[string]*account
	decisions map[string]*decision
	dirty     bool
	dir       string
	loaded    bool
}

func newState() *state {
	return &state{accounts: make(map[string]*account), decisions: make(map[string]*decision)}
}

var current = newState()

// accountLocked returns the account with this ID, creating it for a
// credential host.auth.list does not cover, such as an API key from
// config.yaml.
func (s *state) accountLocked(id, provider string) *account {
	a := s.accounts[id]
	if a == nil {
		a = &account{ID: id}
		s.accounts[id] = a
	}
	if a.Provider == "" {
		a.Provider = providerKeyOf(provider)
	}
	return a
}

// syncAccounts applies host.auth.list. An account polled before keeps its
// schedule; a new one is polled at once.
func (s *state) syncAccounts(entries []pluginapi.HostAuthFileEntry, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	listed := make(map[string]bool, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		listed[id] = true
		a := s.accountLocked(id, firstNonEmpty(entry.Provider, entry.Type))
		a.Provider = providerKeyOf(firstNonEmpty(entry.Provider, entry.Type, a.Provider))
		a.authIndex = strings.TrimSpace(entry.AuthIndex)
		if label := firstNonEmpty(entry.Email, entry.Label, entry.Name); label != "" && label != a.Label {
			a.Label = label
			s.dirty = true
		}
		wasPollable := a.pollable
		a.listed = true
		a.pollable = quotaProvider(a.Provider) && a.authIndex != "" && !entry.Disabled
		if a.pollable && !wasPollable {
			a.nextPoll = now
		}
	}
	for id, a := range s.accounts {
		if !listed[id] {
			a.listed, a.pollable = false, false
		}
	}
}

// pollDue lists the accounts whose usage endpoint should be read now.
func (s *state) pollDue(now time.Time) []pollTarget {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []pollTarget
	for _, a := range s.accounts {
		if a.pollable && !now.Before(a.nextPoll) && !now.Before(a.retryAt) {
			due = append(due, pollTarget{id: a.ID, authIndex: a.authIndex, provider: a.Provider, rejected: a.rejected})
		}
	}
	return due
}

// applyReadingLocked stores a quota reading unless a newer one is already held,
// which concurrent requests finishing out of order would otherwise undo.
func (s *state) applyReadingLocked(a *account, q *quota) {
	if q == nil || (a.Quota != nil && q.At.Before(a.Quota.At)) {
		return
	}
	a.Quota = q
	s.dirty = true
}

// persisted is the state file: what a restart or a hot upgrade must not lose.
type persisted struct {
	Version   int                  `json:"version"`
	Accounts  map[string]*account  `json:"accounts"`
	Decisions map[string]*decision `json:"decisions"`
}

// load reads the state file once per process, before the first decision.
func (s *state) load(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dir = dir
	if s.loaded {
		return
	}
	s.loaded = true
	raw, errRead := os.ReadFile(filepath.Join(dir, stateFileName))
	if errRead != nil {
		return
	}
	var file persisted
	if errUnmarshal := json.Unmarshal(raw, &file); errUnmarshal != nil || file.Version != stateVersion {
		hostLog("warn", "reset-first: ignoring unreadable state file", map[string]any{"path": filepath.Join(dir, stateFileName)})
		return
	}
	for id, a := range file.Accounts {
		if a == nil || id == "" {
			continue
		}
		a.ID = id
		s.accounts[id] = a
	}
	for model, d := range file.Decisions {
		if d == nil || model == "" {
			continue
		}
		d.Model = model
		s.decisions[model] = d
	}
}

// persist writes the state through a temporary file, so a crash during a
// write cannot truncate the previous state.
func (s *state) persist(now time.Time) {
	s.mu.Lock()
	s.pruneLocked(now)
	if !s.dirty || s.dir == "" {
		s.mu.Unlock()
		return
	}
	raw, errMarshal := json.Marshal(persisted{Version: stateVersion, Accounts: s.accounts, Decisions: s.decisions})
	dir := s.dir
	s.dirty = false
	s.mu.Unlock()
	if errMarshal != nil {
		hostLog("warn", "reset-first: cannot encode state", map[string]any{"error": errMarshal.Error()})
		return
	}
	target := filepath.Join(dir, stateFileName)
	temp := target + ".tmp"
	errWrite := os.MkdirAll(dir, 0o755)
	if errWrite == nil {
		errWrite = os.WriteFile(temp, raw, 0o600)
	}
	if errWrite == nil {
		errWrite = os.Rename(temp, target)
	}
	if errWrite != nil {
		hostLog("warn", "reset-first: cannot save state", map[string]any{"path": target, "error": errWrite.Error()})
	}
}

// pruneLocked forgets decisions older than the history and accounts nothing
// refers to any more.
func (s *state) pruneLocked(now time.Time) {
	horizon := now.Add(-history)
	referenced := make(map[string]bool)
	for model, d := range s.decisions {
		if d.At.Before(horizon) {
			delete(s.decisions, model)
			s.dirty = true
			continue
		}
		d.prune(horizon)
		for _, snapshot := range d.Accounts {
			referenced[snapshot.ID] = true
		}
	}
	for id, a := range s.accounts {
		if a.listed || referenced[id] || (a.Quota != nil && a.Quota.At.After(horizon)) {
			continue
		}
		delete(s.accounts, id)
		s.dirty = true
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func providerKeyOf(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
