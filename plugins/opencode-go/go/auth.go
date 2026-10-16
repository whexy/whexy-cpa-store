package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// keyFileName is the source file the plugin page maintains.
const keyFileName = "opencode-go.json"

// keyIDAttribute names the auth attribute that points a runtime auth at its
// key inside the source file.
const keyIDAttribute = "key_id"

// catalogRefreshedField is the time the plugin last rewrote a file to make
// CPA register the catalog again. CPA asks for an auth's models only when
// the auth changes, and a virtual auth of a multi-key file changes only
// through what auth.parse returns, so every auth carries the value as an
// attribute of the same name.
const catalogRefreshedField = "catalog_refreshed_at"

var errNoKey = errors.New("auth file holds no OpenCode Go key")

// keyEntry is one key of an opencode-go auth file.
type keyEntry struct {
	ID       string `json:"id"`
	Label    string `json:"label,omitempty"`
	APIKey   string `json:"api_key"`
	Disabled bool   `json:"disabled,omitempty"`
	AddedAt  string `json:"added_at,omitempty"`
	// Go and Zen say which families the key serves; Go is on unless the file
	// says false, Zen only when it says true.
	Go  *bool `json:"go,omitempty"`
	Zen bool  `json:"zen,omitempty"`
}

// familySet says which of OpenCode Go and OpenCode Zen a credential serves;
// each one it serves is an auth of its own in CPA.
type familySet struct {
	Go, Zen bool
}

func (s familySet) has(family modelFamily) bool {
	switch family {
	case familyGo:
		return s.Go
	case familyZen:
		return s.Zen
	}
	return false
}

func (k keyEntry) families() familySet {
	return familySet{Go: k.Go == nil || *k.Go, Zen: k.Zen}
}

// setFamilies stores which families the key serves, leaving Go unset when
// it is on.
func (k *keyEntry) setFamilies(set familySet) {
	k.Go = nil
	if !set.Go {
		off := false
		k.Go = &off
	}
	k.Zen = set.Zen
}

// keyFile is an opencode-go auth file in either accepted shape: the multi-key
// {"keys":[...]} source file or a single-key {"api_key":...} file.
type keyFile struct {
	Keys []keyEntry
	// extra keeps unrelated top-level fields (prefix, proxy_url, priority, ...)
	// across a rewrite.
	extra map[string]json.RawMessage
}

// keyFields are the top-level fields keyFile owns; marshal rewrites them.
var keyFields = map[string]struct{}{
	"type": {}, "keys": {}, "api_key": {}, "label": {}, "disabled": {}, "id": {}, "key_id": {}, "added_at": {},
	"go": {}, "zen": {},
}

func keyIDFor(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])[:12]
}

// maskKey shows only the last four characters of a key.
func maskKey(apiKey string) string {
	tail := apiKey
	if len(tail) > 4 {
		tail = tail[len(tail)-4:]
	}
	if strings.HasPrefix(apiKey, "oc_sk_") {
		return "oc_sk_…" + tail
	}
	return "…" + tail
}

func (k keyEntry) displayLabel() string {
	if k.Label != "" {
		return k.Label
	}
	return displayName + " " + maskKey(k.APIKey)
}

func normalizeAPIKey(apiKey string) (string, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return "", errors.New("key is empty")
	}
	if strings.ContainsAny(apiKey, " \t\r\n\"") {
		return "", errors.New("key contains whitespace or quotes")
	}
	return apiKey, nil
}

// isKeyFile reports whether raw is an auth file this plugin owns.
func isKeyFile(raw []byte) bool {
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(head.Type), providerKey)
}

// parseKeyFile reads either file shape. Keys are identified by the hash of
// the key itself, so a hand-edited id cannot point at another key. A
// top-level "disabled" applies to every key, as the host applies it to
// every auth from the file.
func parseKeyFile(raw []byte) (keyFile, error) {
	var top map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &top); errUnmarshal != nil {
		return keyFile{}, fmt.Errorf("decode auth file: %w", errUnmarshal)
	}
	file := keyFile{extra: make(map[string]json.RawMessage)}
	for name, value := range top {
		if _, owned := keyFields[name]; !owned {
			file.extra[name] = value
		}
	}
	if rawKeys, ok := top["keys"]; ok && !isJSONNull(rawKeys) {
		var entries []keyEntry
		if errUnmarshal := json.Unmarshal(rawKeys, &entries); errUnmarshal != nil {
			return keyFile{}, fmt.Errorf("decode keys: %w", errUnmarshal)
		}
		for _, entry := range entries {
			file.appendEntry(entry)
		}
	}
	if rawKey, ok := top["api_key"]; ok {
		var single keyEntry
		if errUnmarshal := json.Unmarshal(rawKey, &single.APIKey); errUnmarshal != nil {
			return keyFile{}, fmt.Errorf("decode api_key: %w", errUnmarshal)
		}
		if rawLabel, okLabel := top["label"]; okLabel {
			_ = json.Unmarshal(rawLabel, &single.Label)
		}
		if rawAdded, okAdded := top["added_at"]; okAdded {
			_ = json.Unmarshal(rawAdded, &single.AddedAt)
		}
		single.setFamilies(familySet{Go: !jsonFalse(top["go"]), Zen: jsonTrue(top["zen"])})
		file.appendEntry(single)
	}
	if jsonTrue(top["disabled"]) {
		for index := range file.Keys {
			file.Keys[index].Disabled = true
		}
	}
	return file, nil
}

// appendEntry adds a well-formed entry and ignores duplicates of a key
// already present.
func (f *keyFile) appendEntry(entry keyEntry) bool {
	apiKey, errKey := normalizeAPIKey(entry.APIKey)
	if errKey != nil {
		return false
	}
	entry.APIKey = apiKey
	entry.ID = keyIDFor(apiKey)
	entry.Label = strings.TrimSpace(entry.Label)
	entry.setFamilies(entry.families())
	if _, exists := f.find(entry.ID); exists {
		return false
	}
	f.Keys = append(f.Keys, entry)
	return true
}

func (f keyFile) find(id string) (keyEntry, bool) {
	for _, entry := range f.Keys {
		if entry.ID == id {
			return entry, true
		}
	}
	return keyEntry{}, false
}

// marshal writes the multi-key shape. Per-key flags replace any top-level
// "disabled", which would otherwise disable keys added later.
func (f keyFile) marshal() ([]byte, error) {
	out := make(map[string]any, len(f.extra)+2)
	for name, value := range f.extra {
		out[name] = value
	}
	keys := f.Keys
	if keys == nil {
		keys = []keyEntry{}
	}
	out["type"] = providerKey
	out["keys"] = keys
	return json.MarshalIndent(out, "", "  ")
}

func isJSONNull(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null"
}

// jsonFalse reports a field that is explicitly false, so an absent field
// keeps its default.
func jsonFalse(raw json.RawMessage) bool {
	if isJSONNull(raw) {
		return false
	}
	var flag bool
	if json.Unmarshal(raw, &flag) == nil {
		return !flag
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(text))
		return errParse == nil && !parsed
	}
	return false
}

func jsonTrue(raw json.RawMessage) bool {
	if isJSONNull(raw) {
		return false
	}
	var flag bool
	if json.Unmarshal(raw, &flag) == nil {
		return flag
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(text))
		return errParse == nil && parsed
	}
	return false
}

// authFileBase is the file name used in auth IDs. The host passes a path
// relative to the auth directory on save but a base name on load, so only
// the base name is stable.
func authFileBase(path, fileName string) string {
	if strings.TrimSpace(path) != "" {
		return filepath.Base(strings.TrimSpace(path))
	}
	if strings.TrimSpace(fileName) != "" {
		return filepath.Base(strings.TrimSpace(fileName))
	}
	return keyFileName
}

// authIDFor is the runtime auth ID of one key; it is independent of the
// number of keys in the file so cooldowns and stats survive adding a key.
func authIDFor(fileBase, keyID string) string {
	return fileBase + "#" + keyID
}

func handleAuthParse(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode auth parse request: %w", errUnmarshal)
	}
	return okEnvelope(parseAuths(req))
}

// parseAuths expands one auth file into its runtime auths: one per family
// each login and key serves, so OpenCode Go and OpenCode Zen route as
// separate credentials with their own models, priority and weight, and the
// keyless OpenCode Free auth of the free file.
//
// A file that yields two or more auths makes them plugin-virtual: the host
// never writes them back, and quota calls receive the whole file. A single
// auth is an ordinary file auth the host may persist by writing its
// StorageJSON over the file, so it carries the whole file. Either way a
// login's refreshed tokens are saved by the plugin; see handleAuthRefresh.
//
// A file that serves nothing still yields a disabled auth: declining it
// would make the host register the raw file as an enabled, keyless auth,
// and the plugin finds its files through the host's auths.
func parseAuths(req pluginapi.AuthParseRequest) pluginapi.AuthParseResponse {
	if !isKeyFile(req.RawJSON) {
		return pluginapi.AuthParseResponse{}
	}
	base := authFileBase(req.Path, req.FileName)
	if isFreeFile(req.RawJSON) {
		return pluginapi.AuthParseResponse{Handled: true, Auth: freeAuth(base, req.RawJSON)}
	}
	if isLoginFile(req.RawJSON) {
		login, errParse := parseLoginFile(req.RawJSON)
		if errParse != nil {
			return pluginapi.AuthParseResponse{Handled: true, Auth: placeholderAuth(base, displayName+" (invalid login file)", req.RawJSON)}
		}
		auth := login.authData(base)
		auth.StorageJSON = bytes.Clone(req.RawJSON)
		return fileAuths(base, req.RawJSON, credentialAuths(auth, login.extra, login.families()), login.displayName())
	}
	file, errParse := parseKeyFile(req.RawJSON)
	if errParse != nil || len(file.Keys) == 0 {
		label := displayName + " (no keys)"
		if errParse != nil {
			label = displayName + " (invalid file)"
		}
		return pluginapi.AuthParseResponse{Handled: true, Auth: placeholderAuth(base, label, req.RawJSON)}
	}
	mark := extraString(file.extra, catalogRefreshedField)
	auths := make([]pluginapi.AuthData, 0, 2*len(file.Keys))
	for _, entry := range file.Keys {
		auths = append(auths, credentialAuths(keyAuth(base, entry, mark), file.extra, entry.families())...)
	}
	return fileAuths(base, req.RawJSON, auths, displayName)
}

// fileAuths answers auth.parse for a file's family auths: the whole file
// as the StorageJSON of a single auth, and a disabled auth when the file
// serves no family at all.
func fileAuths(base string, raw []byte, auths []pluginapi.AuthData, name string) pluginapi.AuthParseResponse {
	switch len(auths) {
	case 0:
		return pluginapi.AuthParseResponse{Handled: true, Auth: placeholderAuth(base, name+" (OpenCode Go and Zen off)", raw)}
	case 1:
		auth := auths[0]
		auth.StorageJSON = bytes.Clone(raw)
		return pluginapi.AuthParseResponse{Handled: true, Auth: auth}
	}
	return pluginapi.AuthParseResponse{Handled: true, Auths: auths}
}

// placeholderAuth keeps a file that serves nothing known to the host.
func placeholderAuth(base, label string, raw []byte) pluginapi.AuthData {
	return pluginapi.AuthData{
		Provider:    providerKey,
		ID:          base,
		Label:       label,
		Disabled:    true,
		StorageJSON: bytes.Clone(raw),
		Attributes:  map[string]string{familyAttribute: string(familyGo)},
	}
}

// credentialAuths are a credential's auths for the families it serves.
func credentialAuths(auth pluginapi.AuthData, extra map[string]json.RawMessage, set familySet) []pluginapi.AuthData {
	var out []pluginapi.AuthData
	for _, family := range []modelFamily{familyGo, familyZen} {
		if set.has(family) {
			out = append(out, familyAuth(auth, extra, family))
		}
	}
	return out
}

func keyAuth(base string, entry keyEntry, catalogMark string) pluginapi.AuthData {
	storage, _ := json.Marshal(map[string]any{
		"type":         providerKey,
		keyIDAttribute: entry.ID,
		"api_key":      entry.APIKey,
		"label":        entry.Label,
	})
	attributes := map[string]string{keyIDAttribute: entry.ID}
	if catalogMark != "" {
		attributes[catalogRefreshedField] = catalogMark
	}
	return pluginapi.AuthData{
		Provider:    providerKey,
		ID:          authIDFor(base, entry.ID),
		Label:       entry.displayLabel(),
		Disabled:    entry.Disabled,
		StorageJSON: storage,
		Attributes:  attributes,
	}
}

const (
	// familyAttribute names the catalog an auth serves; an auth without it
	// serves OpenCode Go.
	familyAttribute = "family"
	// zenAuthSuffix turns a credential's Go auth ID into its Zen auth ID,
	// so the Go auth keeps the ID, cooldowns and stats it had before the
	// split.
	zenAuthSuffix  = "#zen"
	zenDisplayName = "OpenCode Zen"
	// modelAliasesAttribute holds per-auth model aliases. The plugin no
	// longer sets it; 0.5.0 listed Zen and Free models under their bare ID
	// through it, so an auth that has it is stale.
	modelAliasesAttribute = "model_aliases"
	// pathAttribute is the auth file CPA loaded an auth from.
	pathAttribute = "path"
)

var families = []modelFamily{familyGo, familyZen, familyFree}

func authFamily(attributes map[string]string) modelFamily {
	switch strings.ToLower(strings.TrimSpace(attributes[familyAttribute])) {
	case string(familyZen):
		return familyZen
	case string(familyFree):
		return familyFree
	}
	return familyGo
}

// familyAuth is the credential's auth for one family, with that family's
// routing fields of the file as its priority and weight.
func familyAuth(auth pluginapi.AuthData, extra map[string]json.RawMessage, family modelFamily) pluginapi.AuthData {
	out := auth
	out.Attributes = make(map[string]string, len(auth.Attributes)+4)
	for name, value := range auth.Attributes {
		out.Attributes[name] = value
	}
	if auth.Metadata != nil {
		out.Metadata = make(map[string]any, len(auth.Metadata))
		for name, value := range auth.Metadata {
			out.Metadata[name] = value
		}
	}
	out.StorageJSON = bytes.Clone(auth.StorageJSON)
	out.Attributes[familyAttribute] = string(family)
	for name, value := range routingAttributes(extra, family) {
		out.Attributes[name] = value
	}
	if family == familyZen {
		out.ID = auth.ID + zenAuthSuffix
		if auth.Label != "" {
			out.Label = auth.Label + " · " + zenDisplayName
		}
	}
	return out
}

// extraString reads a string field kept in a file's extra fields.
func extraString(extra map[string]json.RawMessage, name string) string {
	var value string
	if json.Unmarshal(extra[name], &value) != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

// credential is what one auth sends upstream: a service-account key or a
// login's access token.
type credential struct {
	// ID keys catalog caches and usage blocks. A login's ID is its
	// workspace, so it stays the same when the tokens rotate.
	ID     string
	Bearer string
	// Name labels the credential in quota cards.
	Name string
}

// resolveCredential finds the credential a host call is about. An expired
// login returns its credential together with a *loginExpiredError, so
// callers can still name it and use its cached catalog.
func resolveCredential(storageJSON []byte, attributes map[string]string, metadata map[string]any) (credential, error) {
	if authFamily(attributes) == familyFree || isFreeFile(storageJSON) {
		return freeCredential, nil
	}
	if isLoginFile(storageJSON) {
		login, errParse := parseLoginFile(storageJSON)
		if errParse != nil {
			return credential{}, errParse
		}
		cred := login.credential()
		if login.expired(time.Now()) {
			return cred, &loginExpiredError{name: login.displayName(), at: login.ExpiresAt}
		}
		return cred, nil
	}
	key, errKey := resolveAuthKey(storageJSON, attributes, metadata)
	if errKey != nil {
		return credential{}, errKey
	}
	return keyCredential(key), nil
}

func keyCredential(key keyEntry) credential {
	return credential{ID: key.ID, Bearer: key.APIKey, Name: quotaGroupName(key)}
}

// resolveAuthKey finds the key a host call is about. Executor and
// model.for_auth calls receive the auth's own StorageJSON (one key, or the
// whole file for a file that yields one auth); quota calls receive the whole
// physical file. Every auth carries the key_id attribute, so both resolve
// the same way.
func resolveAuthKey(storageJSON []byte, attributes map[string]string, metadata map[string]any) (keyEntry, error) {
	keyID := strings.TrimSpace(attributes[keyIDAttribute])
	if keyID == "" {
		if value, ok := metadata[keyIDAttribute].(string); ok {
			keyID = strings.TrimSpace(value)
		}
	}
	if len(bytes.TrimSpace(storageJSON)) == 0 {
		return keyEntry{}, errNoKey
	}
	file, errParse := parseKeyFile(storageJSON)
	if errParse != nil {
		return keyEntry{}, errParse
	}
	if keyID != "" {
		if entry, ok := file.find(keyID); ok {
			return entry, nil
		}
		return keyEntry{}, fmt.Errorf("key %s is no longer in the auth file", keyID)
	}
	switch len(file.Keys) {
	case 0:
		return keyEntry{}, errNoKey
	case 1:
		return file.Keys[0], nil
	default:
		return keyEntry{}, fmt.Errorf("auth file holds %d keys and the auth names none", len(file.Keys))
	}
}
