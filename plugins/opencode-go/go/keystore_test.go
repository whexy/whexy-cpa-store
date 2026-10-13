package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestKeyFileEdits(t *testing.T) {
	var file keyFile
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.FixedZone("x", 3600))
	added, errAdd := file.addKey("  "+testKeyA+" ", "Work", now)
	if errAdd != nil {
		t.Fatal(errAdd)
	}
	if added.ID != keyIDFor(testKeyA) || added.AddedAt != "2026-10-12T19:00:00Z" || added.Label != "Work" {
		t.Fatalf("added = %+v", added)
	}
	if _, errDup := file.addKey(testKeyA, "again", now); !errors.Is(errDup, errKeyExists) {
		t.Fatalf("duplicate err = %v", errDup)
	}
	if _, errBad := file.addKey("oc_sk bad", "", now); errBad == nil {
		t.Fatal("key with space accepted")
	}
	if _, errAdd := file.addKey(testKeyB, "", now); errAdd != nil {
		t.Fatal(errAdd)
	}

	label, disabled := "Home", true
	updated, errUpdate := file.updateKey(keyIDFor(testKeyB), keyPatch{Label: &label, Disabled: &disabled})
	if errUpdate != nil || updated.Label != "Home" || !updated.Disabled {
		t.Fatalf("updated = %+v, %v", updated, errUpdate)
	}
	if _, errMissing := file.updateKey("nope", keyPatch{}); !errors.Is(errMissing, errKeyNotFound) {
		t.Fatalf("missing update err = %v", errMissing)
	}
	removed, errRemove := file.removeKey(keyIDFor(testKeyA))
	if errRemove != nil || removed.APIKey != testKeyA || len(file.Keys) != 1 {
		t.Fatalf("removed = %+v, keys = %+v, %v", removed, file.Keys, errRemove)
	}
	if _, errMissing := file.removeKey(keyIDFor(testKeyA)); !errors.Is(errMissing, errKeyNotFound) {
		t.Fatalf("missing remove err = %v", errMissing)
	}
}

func TestKeyFileMarshalRoundTrip(t *testing.T) {
	file, errParse := parseKeyFile([]byte(`{"type":"opencode-go","api_key":"` + testKeyA + `","label":"Solo","disabled":true,"prefix":"x","proxy_url":"http://p:1"}`))
	if errParse != nil {
		t.Fatal(errParse)
	}
	if _, errAdd := file.addKey(testKeyB, "New", time.Unix(0, 0)); errAdd != nil {
		t.Fatal(errAdd)
	}
	raw, errMarshal := file.marshal()
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var top map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &top); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	for _, gone := range []string{"api_key", "label", "disabled"} {
		if _, ok := top[gone]; ok {
			t.Fatalf("marshal kept top-level %s: %s", gone, raw)
		}
	}
	if string(top["type"]) != `"opencode-go"` || string(top["prefix"]) != `"x"` || string(top["proxy_url"]) != `"http://p:1"` {
		t.Fatalf("marshal = %s", raw)
	}
	again, errAgain := parseKeyFile(raw)
	if errAgain != nil {
		t.Fatal(errAgain)
	}
	if len(again.Keys) != 2 || !again.Keys[0].Disabled || again.Keys[0].Label != "Solo" || again.Keys[1].Disabled {
		t.Fatalf("round trip = %+v", again.Keys)
	}
	empty, _ := (keyFile{}).marshal()
	if !strings.Contains(string(empty), `"keys": []`) {
		t.Fatalf("empty file = %s", empty)
	}
}

// keyStoreHost serves host.auth.list/get/save over an in-memory auth
// directory, re-parsing saved files the way the host does.
func keyStoreHost(t *testing.T, files map[string]string) *fakeHost {
	t.Helper()
	fake := installFakeHost(t)
	list := func() []pluginapi.HostAuthFileEntry {
		var out []pluginapi.HostAuthFileEntry
		for name, content := range files {
			resp := parseAuths(pluginapi.AuthParseRequest{Path: "/auths/" + name, RawJSON: []byte(content)})
			auths := resp.Auths
			if len(auths) == 0 && resp.Handled {
				auths = []pluginapi.AuthData{resp.Auth}
			}
			if !resp.Handled {
				out = append(out, pluginapi.HostAuthFileEntry{ID: name, AuthIndex: "idx-" + name, Name: name, Provider: "claude", Path: "/auths/" + name})
			}
			for _, auth := range auths {
				out = append(out, pluginapi.HostAuthFileEntry{ID: auth.ID, AuthIndex: "idx-" + auth.ID, Name: name, Provider: "opencode-go", Type: "opencode-go", Path: "/auths/" + name})
			}
		}
		return out
	}
	fake.handle(pluginabi.MethodHostAuthList, func(json.RawMessage) (any, error) {
		return hostAuthListResponse{Files: list()}, nil
	})
	fake.handle(pluginabi.MethodHostAuthGet, func(payload json.RawMessage) (any, error) {
		var req pluginapi.HostAuthGetRequest
		_ = json.Unmarshal(payload, &req)
		for _, entry := range list() {
			if entry.AuthIndex == req.AuthIndex {
				return pluginapi.HostAuthGetResponse{AuthIndex: req.AuthIndex, Name: entry.Name, Path: entry.Path, JSON: json.RawMessage(files[entry.Name])}, nil
			}
		}
		return nil, errors.New("auth not found for auth_index " + req.AuthIndex)
	})
	fake.handle(pluginabi.MethodHostAuthSave, func(payload json.RawMessage) (any, error) {
		var req pluginapi.HostAuthSaveRequest
		_ = json.Unmarshal(payload, &req)
		files[req.Name] = string(req.JSON)
		return pluginapi.HostAuthSaveResponse{Name: req.Name, Path: "/auths/" + req.Name}, nil
	})
	return fake
}

func TestLoadKeyFilesGroupsAuthsByFile(t *testing.T) {
	keyStoreHost(t, map[string]string{
		"a-single.json":    `{"type":"opencode-go","api_key":"` + testKeyC + `"}`,
		"opencode-go.json": string(multiKeyFile(t)),
		"claude.json":      `{"type":"claude"}`,
	})
	files, errLoad := loadKeyFiles()
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if len(files) != 2 || files[0].Name != "opencode-go.json" || files[1].Name != "a-single.json" {
		t.Fatalf("files = %+v", files)
	}
	if len(files[0].File.Keys) != 2 || len(files[0].Auths) != 2 {
		t.Fatalf("source file = %+v", files[0])
	}
	runtime, ok := files[0].runtimeAuth(keyIDFor(testKeyB))
	if !ok || runtime.AuthIndex != "idx-opencode-go.json#"+keyIDFor(testKeyB) {
		t.Fatalf("runtime = %+v, %v", runtime, ok)
	}
	index, entry, found := locateKey(files, keyIDFor(testKeyC))
	if !found || index != 1 || entry.APIKey != testKeyC {
		t.Fatalf("locate = %d %+v %v", index, entry, found)
	}
}

func TestMutateKeyFileCreatesAndRewritesSourceFile(t *testing.T) {
	files := map[string]string{}
	keyStoreHost(t, files)
	errAdd := mutateKeyFile(keyFileName, func(file *keyFile) error {
		_, errAdd := file.addKey(testKeyA, "First", time.Unix(0, 0))
		return errAdd
	})
	if errAdd != nil {
		t.Fatal(errAdd)
	}
	errAdd = mutateKeyFile(keyFileName, func(file *keyFile) error {
		_, errAdd := file.addKey(testKeyB, "Second", time.Unix(0, 0))
		return errAdd
	})
	if errAdd != nil {
		t.Fatal(errAdd)
	}
	saved, errParse := parseKeyFile([]byte(files[keyFileName]))
	if errParse != nil {
		t.Fatal(errParse)
	}
	if len(saved.Keys) != 2 || saved.Keys[0].Label != "First" || saved.Keys[1].Label != "Second" {
		t.Fatalf("saved = %s", files[keyFileName])
	}
	errDup := mutateKeyFile(keyFileName, func(file *keyFile) error {
		_, errAdd := file.addKey(testKeyA, "", time.Unix(0, 0))
		return errAdd
	})
	if !errors.Is(errDup, errKeyExists) {
		t.Fatalf("duplicate err = %v", errDup)
	}
}
