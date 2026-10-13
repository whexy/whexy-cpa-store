package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// binding pins one session to one member, and for a member that several
// providers serve, to one provider. Keeping the member stable is what keeps
// CPA's own account binding stable: its affinity key is (provider, session,
// model), so a session that changes model loses its credential.
type binding struct {
	Group    string    `json:"group"`
	Session  string    `json:"session"`
	Model    string    `json:"model"`
	Provider string    `json:"provider"`
	BoundAt  time.Time `json:"bound_at"`
	LastUsed time.Time `json:"last_used"`
	Requests int       `json:"requests"`
}

func bindingKey(groupID, sessionID string) string { return groupID + "\x00" + sessionID }

type bindingFile struct {
	Version  int       `json:"version"`
	Bindings []binding `json:"bindings"`
}

const (
	stateFileName  = "state.json"
	stateVersion   = 1
	maxSessionKeys = 4096
)

// loadBindings reads the persisted bindings. A missing, unreadable or
// truncated file is not fatal: the plugin starts without bindings and sessions
// re-decide on their next request.
func loadBindings(dir string) map[string]*binding {
	bindings := make(map[string]*binding)
	if dir == "" {
		return bindings
	}
	raw, errRead := os.ReadFile(filepath.Join(dir, stateFileName))
	if errRead != nil {
		return bindings
	}
	var persisted bindingFile
	if errUnmarshal := json.Unmarshal(raw, &persisted); errUnmarshal != nil {
		hostLog("warn", "model-groups: ignoring unreadable state file", map[string]any{"error": errUnmarshal.Error()})
		return bindings
	}
	for index := range persisted.Bindings {
		entry := persisted.Bindings[index]
		if entry.Group == "" || entry.Session == "" || entry.Model == "" {
			continue
		}
		copied := entry
		bindings[bindingKey(entry.Group, entry.Session)] = &copied
	}
	return bindings
}

// saveBindings writes the bindings through a temporary file so a crash during
// a write cannot truncate the previous state.
func saveBindings(dir string, bindings map[string]*binding) error {
	if dir == "" {
		return nil
	}
	list := make([]binding, 0, len(bindings))
	for _, entry := range bindings {
		list = append(list, *entry)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].LastUsed.Equal(list[j].LastUsed) {
			return list[i].Session < list[j].Session
		}
		return list[i].LastUsed.After(list[j].LastUsed)
	})
	raw, errMarshal := json.MarshalIndent(bindingFile{Version: stateVersion, Bindings: list}, "", "  ")
	if errMarshal != nil {
		return errMarshal
	}
	if errMkdir := os.MkdirAll(dir, 0o755); errMkdir != nil {
		return errMkdir
	}
	target := filepath.Join(dir, stateFileName)
	temp := target + ".tmp"
	if errWrite := os.WriteFile(temp, raw, 0o600); errWrite != nil {
		return errWrite
	}
	return os.Rename(temp, target)
}

// pruneBindings drops expired bindings and, when the map is over its cap, the
// least recently used ones.
func pruneBindings(bindings map[string]*binding, now time.Time, ttl time.Duration, max int) {
	for key, entry := range bindings {
		if now.Sub(entry.LastUsed) > ttl {
			delete(bindings, key)
		}
	}
	if max <= 0 || len(bindings) <= max {
		return
	}
	keys := make([]string, 0, len(bindings))
	for key := range bindings {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return bindings[keys[i]].LastUsed.Before(bindings[keys[j]].LastUsed)
	})
	for _, key := range keys[:len(bindings)-max] {
		delete(bindings, key)
	}
}

// sessionsInGroup lists a group's bindings, most recently used first.
func sessionsInGroup(bindings map[string]*binding, groupID string, limit int) []binding {
	list := make([]binding, 0, len(bindings))
	for _, entry := range bindings {
		if entry.Group == groupID {
			list = append(list, *entry)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].LastUsed.Equal(list[j].LastUsed) {
			return list[i].Session < list[j].Session
		}
		return list[i].LastUsed.After(list[j].LastUsed)
	})
	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}
	return list
}

// shortSession shortens a session ID for display without losing which end it
// was cut from.
func shortSession(session string) string {
	const keep = 14
	if len(session) <= keep {
		return session
	}
	head := keep * 2 / 3
	return fmt.Sprintf("%s…%s", session[:head], session[len(session)-(keep-head):])
}
