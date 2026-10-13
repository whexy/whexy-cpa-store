package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var (
	errKeyExists   = errors.New("key is already stored")
	errKeyNotFound = errors.New("key not found")
)

// addKey appends a new enabled key and returns it.
func (f *keyFile) addKey(apiKey, label string, now time.Time) (keyEntry, error) {
	apiKey, errKey := normalizeAPIKey(apiKey)
	if errKey != nil {
		return keyEntry{}, errKey
	}
	if _, exists := f.find(keyIDFor(apiKey)); exists {
		return keyEntry{}, errKeyExists
	}
	entry := keyEntry{APIKey: apiKey, Label: label, AddedAt: now.UTC().Format(time.RFC3339)}
	f.appendEntry(entry)
	added, _ := f.find(keyIDFor(apiKey))
	return added, nil
}

// keyPatch changes the fields that are non-nil.
type keyPatch struct {
	Label    *string
	Disabled *bool
}

func (f *keyFile) updateKey(id string, patch keyPatch) (keyEntry, error) {
	for index := range f.Keys {
		if f.Keys[index].ID != id {
			continue
		}
		if patch.Label != nil {
			f.Keys[index].Label = strings.TrimSpace(*patch.Label)
		}
		if patch.Disabled != nil {
			f.Keys[index].Disabled = *patch.Disabled
		}
		return f.Keys[index], nil
	}
	return keyEntry{}, errKeyNotFound
}

func (f *keyFile) removeKey(id string) (keyEntry, error) {
	for index, entry := range f.Keys {
		if entry.ID == id {
			f.Keys = append(f.Keys[:index:index], f.Keys[index+1:]...)
			return entry, nil
		}
	}
	return keyEntry{}, errKeyNotFound
}

// storedKeyFile is one opencode-go auth file as the host currently sees it.
type storedKeyFile struct {
	// Name is the file name in the auth directory, as host.auth.save expects.
	Name string
	File keyFile
	// Auths are the runtime auths the host expanded from the file.
	Auths []pluginapi.HostAuthFileEntry
}

// runtimeAuth returns the host's runtime record for one key of the file.
func (s storedKeyFile) runtimeAuth(keyID string) (pluginapi.HostAuthFileEntry, bool) {
	want := authIDFor(s.Name, keyID)
	for _, entry := range s.Auths {
		if entry.ID == want {
			return entry, true
		}
	}
	return pluginapi.HostAuthFileEntry{}, false
}

// keyStoreMu serializes read-modify-write cycles on auth files.
var keyStoreMu sync.Mutex

// loadKeyFiles reads every opencode-go auth file through the host, the
// source file first. Files that fail to read are skipped; the error reports
// them only when nothing could be read.
func loadKeyFiles() ([]storedKeyFile, error) {
	entries, errList := hostAuthList()
	if errList != nil {
		return nil, errList
	}
	return readKeyFiles(entries)
}

func readKeyFiles(entries []pluginapi.HostAuthFileEntry) ([]storedKeyFile, error) {
	byFile := make(map[string][]pluginapi.HostAuthFileEntry)
	var order []string
	for _, entry := range entries {
		if !strings.EqualFold(entry.Provider, providerKey) && !strings.EqualFold(entry.Type, providerKey) {
			continue
		}
		if entry.RuntimeOnly || entry.AuthIndex == "" || entry.Name == "" {
			continue
		}
		file := entry.Path
		if file == "" {
			file = entry.Name
		}
		if _, seen := byFile[file]; !seen {
			order = append(order, file)
		}
		byFile[file] = append(byFile[file], entry)
	}
	files := make([]storedKeyFile, 0, len(order))
	var failures []string
	for _, file := range order {
		auths := byFile[file]
		got, errGet := hostAuthGet(auths[0].AuthIndex)
		if errGet != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", auths[0].Name, errGet))
			continue
		}
		parsed, errParse := parseKeyFile(got.JSON)
		if errParse != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", auths[0].Name, errParse))
			continue
		}
		name := authFileBase(got.Path, got.Name)
		files = append(files, storedKeyFile{Name: name, File: parsed, Auths: auths})
	}
	if len(files) == 0 && len(failures) > 0 {
		return nil, fmt.Errorf("read OpenCode Go auth files: %s", strings.Join(failures, "; "))
	}
	sort.SliceStable(files, func(i, j int) bool {
		if (files[i].Name == keyFileName) != (files[j].Name == keyFileName) {
			return files[i].Name == keyFileName
		}
		return files[i].Name < files[j].Name
	})
	return files, nil
}

// locateKey finds the file holding key id.
func locateKey(files []storedKeyFile, id string) (int, keyEntry, bool) {
	for index, file := range files {
		if entry, ok := file.File.find(id); ok {
			return index, entry, true
		}
	}
	return -1, keyEntry{}, false
}

// saveKeyFile writes a file through host.auth.save; the host re-parses it
// and re-registers its auths before returning.
func saveKeyFile(name string, file keyFile) error {
	raw, errMarshal := file.marshal()
	if errMarshal != nil {
		return errMarshal
	}
	_, errSave := hostAuthSave(name, raw)
	return errSave
}

// mutateKeyFile loads the named file (an empty one if it does not exist
// yet), applies mutate and saves the result, holding keyStoreMu throughout.
func mutateKeyFile(name string, mutate func(*keyFile) error) error {
	keyStoreMu.Lock()
	defer keyStoreMu.Unlock()
	files, errLoad := loadKeyFiles()
	if errLoad != nil {
		return errLoad
	}
	file := keyFile{}
	for _, stored := range files {
		if stored.Name == name {
			file = stored.File
			break
		}
	}
	if errMutate := mutate(&file); errMutate != nil {
		return errMutate
	}
	return saveKeyFile(name, file)
}
