package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func multiKeyFile(t *testing.T) []byte {
	t.Helper()
	return []byte(`{
  "type": "opencode-go",
  "priority": 3,
  "keys": [
    {"id": "stale-id", "label": "Work", "api_key": "` + testKeyA + `", "added_at": "2026-10-12T00:00:00Z"},
    {"label": "", "api_key": " ` + testKeyB + ` ", "disabled": true},
    {"label": "dup", "api_key": "` + testKeyA + `"},
    {"label": "empty", "api_key": ""},
    {"label": "spaced", "api_key": "oc_sk_has space"}
  ]
}`)
}

func TestParseKeyFileMultiKey(t *testing.T) {
	file, errParse := parseKeyFile(multiKeyFile(t))
	if errParse != nil {
		t.Fatal(errParse)
	}
	if len(file.Keys) != 2 {
		t.Fatalf("keys = %+v", file.Keys)
	}
	first, second := file.Keys[0], file.Keys[1]
	if first.ID != keyIDFor(testKeyA) || len(first.ID) != 12 || first.Label != "Work" || first.AddedAt != "2026-10-12T00:00:00Z" || first.Disabled {
		t.Fatalf("first = %+v", first)
	}
	if second.APIKey != testKeyB || !second.Disabled || second.displayLabel() != "OpenCode Go oc_sk_…2222" {
		t.Fatalf("second = %+v (%s)", second, second.displayLabel())
	}
	if string(file.extra["priority"]) != "3" {
		t.Fatalf("extra = %v", file.extra)
	}
}

func TestParseKeyFileSingleKeyAndFileDisabled(t *testing.T) {
	file, errParse := parseKeyFile([]byte(`{"type":"opencode-go","api_key":"` + testKeyC + `","label":"Solo","disabled":"true"}`))
	if errParse != nil {
		t.Fatal(errParse)
	}
	if len(file.Keys) != 1 || file.Keys[0].Label != "Solo" || !file.Keys[0].Disabled || file.Keys[0].ID != keyIDFor(testKeyC) {
		t.Fatalf("keys = %+v", file.Keys)
	}
	if _, owned := file.extra["disabled"]; owned {
		t.Fatal("top-level disabled kept as extra")
	}
}

func TestParseAuthsExpandsOneAuthPerKey(t *testing.T) {
	raw := multiKeyFile(t)
	resp := parseAuths(pluginapi.AuthParseRequest{Provider: "opencode-go", Path: "/data/auths/opencode-go.json", FileName: "opencode-go.json", RawJSON: raw})
	if !resp.Handled || len(resp.Auths) != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	idA, idB := keyIDFor(testKeyA), keyIDFor(testKeyB)
	a, b := resp.Auths[0], resp.Auths[1]
	if a.ID != "opencode-go.json#"+idA || b.ID != "opencode-go.json#"+idB {
		t.Fatalf("ids = %q %q", a.ID, b.ID)
	}
	if a.Provider != "opencode-go" || a.Label != "Work" || a.Disabled || a.Attributes["key_id"] != idA {
		t.Fatalf("a = %+v", a)
	}
	if !b.Disabled || b.Label != "OpenCode Go oc_sk_…2222" || b.Attributes["key_id"] != idB {
		t.Fatalf("b = %+v", b)
	}
	if strings.Contains(string(a.StorageJSON), testKeyB) || !strings.Contains(string(a.StorageJSON), testKeyA) {
		t.Fatalf("a storage holds other keys: %s", a.StorageJSON)
	}
	if a.FileName != "" || len(a.Metadata) != 0 {
		t.Fatalf("a sets fields the host derives: %+v", a)
	}
}

func TestParseAuthsSingleKeyKeepsWholeFile(t *testing.T) {
	raw := []byte(`{"type":"opencode-go","keys":[{"label":"Only","api_key":"` + testKeyA + `"}],"proxy_url":"socks5://proxy:1080"}`)
	resp := parseAuths(pluginapi.AuthParseRequest{FileName: "sub/opencode-go.json", RawJSON: raw})
	if !resp.Handled || len(resp.Auths) != 0 {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Auth.ID != "opencode-go.json#"+keyIDFor(testKeyA) || string(resp.Auth.StorageJSON) != string(raw) || resp.Auth.Label != "Only" {
		t.Fatalf("auth = %+v", resp.Auth)
	}
}

func TestParseAuthsWithoutUsableKeyYieldsDisabledAuth(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":   `{"type":"opencode-go","keys":[]}`,
		"invalid": `{"type":"opencode-go","keys":"nope"}`,
	} {
		t.Run(name, func(t *testing.T) {
			resp := parseAuths(pluginapi.AuthParseRequest{Path: "/a/opencode-go.json", RawJSON: []byte(raw)})
			if !resp.Handled || !resp.Auth.Disabled || resp.Auth.ID != "opencode-go.json" || resp.Auth.Provider != "opencode-go" {
				t.Fatalf("resp = %+v", resp)
			}
		})
	}
}

func TestParseAuthsIgnoresOtherProviders(t *testing.T) {
	for _, raw := range []string{`{"type":"claude","api_key":"x"}`, `not json`} {
		if resp := parseAuths(pluginapi.AuthParseRequest{RawJSON: []byte(raw)}); resp.Handled {
			t.Fatalf("%s handled", raw)
		}
	}
}

func TestAuthParseWireShape(t *testing.T) {
	req := mustJSON(t, pluginapi.AuthParseRequest{Provider: "opencode-go", Path: "/a/opencode-go.json", FileName: "opencode-go.json", RawJSON: multiKeyFile(t)})
	raw, errHandle := handleMethod(pluginabi.MethodAuthParse, req)
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	resp := decodeResult[pluginapi.AuthParseResponse](t, raw)
	if !resp.Handled || len(resp.Auths) != 2 {
		t.Fatalf("resp = %+v", resp)
	}
}

// hostStorageJSON mirrors how the host hands an auth's StorageJSON to the
// executor: the plugin's StorageJSON with the auth metadata merged on top.
func hostStorageJSON(t *testing.T, storage []byte, metadata map[string]any) []byte {
	t.Helper()
	out := map[string]any{}
	if errUnmarshal := json.Unmarshal(storage, &out); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	for key, value := range metadata {
		out[key] = value
	}
	out["type"] = "opencode-go"
	return mustJSON(t, out)
}

func TestResolveAuthKey(t *testing.T) {
	physical := multiKeyFile(t)
	auths := parseAuths(pluginapi.AuthParseRequest{FileName: "opencode-go.json", RawJSON: physical}).Auths
	idA, idB := keyIDFor(testKeyA), keyIDFor(testKeyB)

	tests := []struct {
		name       string
		storage    []byte
		attributes map[string]string
		metadata   map[string]any
		wantKey    string
		wantErr    string
	}{
		{name: "executor per-key storage", storage: hostStorageJSON(t, auths[1].StorageJSON, map[string]any{"disabled": true}), attributes: auths[1].Attributes, wantKey: testKeyB},
		{name: "quota physical file", storage: physical, attributes: map[string]string{"key_id": idA, "path": "/a/opencode-go.json"}, wantKey: testKeyA},
		{name: "key id from metadata", storage: physical, metadata: map[string]any{"key_id": idB}, wantKey: testKeyB},
		{name: "single-key file without key id", storage: []byte(`{"type":"opencode-go","api_key":"` + testKeyC + `"}`), wantKey: testKeyC},
		{name: "removed key", storage: physical, attributes: map[string]string{"key_id": keyIDFor(testKeyC)}, wantErr: "no longer in the auth file"},
		{name: "ambiguous", storage: physical, wantErr: "2 keys"},
		{name: "no storage", wantErr: "no OpenCode Go key"},
		{name: "no keys", storage: []byte(`{"type":"opencode-go","keys":[]}`), wantErr: "no OpenCode Go key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, errResolve := resolveAuthKey(tt.storage, tt.attributes, tt.metadata)
			if tt.wantErr != "" {
				if errResolve == nil || !strings.Contains(errResolve.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", errResolve, tt.wantErr)
				}
				return
			}
			if errResolve != nil {
				t.Fatal(errResolve)
			}
			if got.APIKey != tt.wantKey || got.ID != keyIDFor(tt.wantKey) {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestMaskKey(t *testing.T) {
	if got := maskKey(testKeyA); got != "oc_sk_…1111" || strings.Contains(got, "aaaa") {
		t.Fatalf("mask = %q", got)
	}
	if got := maskKey("abc"); got != "…abc" {
		t.Fatalf("mask short = %q", got)
	}
}
