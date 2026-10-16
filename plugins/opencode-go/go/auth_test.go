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

// Each key is an auth per family it serves: OpenCode Go unless it says
// "go": false, OpenCode Zen only when it says "zen": true.
func TestParseAuthsServesTheFamiliesEachKeyHasOn(t *testing.T) {
	useSettings(t, defaultSettings())
	resp := parseAuths(pluginapi.AuthParseRequest{Provider: "opencode-go", Path: "/data/auths/opencode-go.json", FileName: "opencode-go.json", RawJSON: multiKeyFile(t)})
	if !resp.Handled || len(resp.Auths) != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	idA, idB := keyIDFor(testKeyA), keyIDFor(testKeyB)
	a, b := resp.Auths[0], resp.Auths[1]
	if a.ID != "opencode-go.json#"+idA || b.ID != "opencode-go.json#"+idB {
		t.Fatalf("ids = %q %q", a.ID, b.ID)
	}
	if a.Provider != "opencode-go" || a.Label != "Work" || a.Disabled || a.Attributes["key_id"] != idA || a.Attributes["family"] != "go" {
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
	// The top-level priority is CPA's to apply, to every family.
	if _, ok := a.Attributes["priority"]; ok {
		t.Fatalf("a attributes = %v", a.Attributes)
	}

	mixed := `{"type":"opencode-go","keys":[{"label":"Zen","api_key":"` + testKeyA + `","go":false,"zen":true},{"label":"Both","api_key":"` + testKeyB + `","zen":true},{"label":"Off","api_key":"` + testKeyC + `","go":false}]}`
	auths := parseAuths(pluginapi.AuthParseRequest{FileName: "opencode-go.json", RawJSON: []byte(mixed)}).Auths
	var got []string
	for _, auth := range auths {
		got = append(got, auth.ID+"="+auth.Label+"/"+auth.Attributes["family"])
	}
	want := []string{
		"opencode-go.json#" + idA + "#zen=Zen · OpenCode Zen/zen",
		"opencode-go.json#" + idB + "=Both/go",
		"opencode-go.json#" + idB + "#zen=Both · OpenCode Zen/zen",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("auths = %v", got)
	}
}

// A file that yields one auth is an ordinary file auth CPA may write back,
// so it carries the whole file; one that serves nothing stays known to the
// host as a disabled auth.
func TestParseAuthsSingleAuthAndPlaceholder(t *testing.T) {
	useSettings(t, defaultSettings())
	raw := []byte(`{"type":"opencode-go","keys":[{"label":"Only","api_key":"` + testKeyA + `"}],"proxy_url":"socks5://proxy:1080"}`)
	resp := parseAuths(pluginapi.AuthParseRequest{FileName: "sub/opencode-go.json", RawJSON: raw})
	if !resp.Handled || len(resp.Auths) != 0 || resp.Auth.ID != "opencode-go.json#"+keyIDFor(testKeyA) || resp.Auth.Label != "Only" || string(resp.Auth.StorageJSON) != string(raw) {
		t.Fatalf("resp = %+v", resp)
	}
	zenOnly := []byte(`{"type":"opencode-go","api_key":"` + testKeyA + `","go":"false","zen":true}`)
	resp = parseAuths(pluginapi.AuthParseRequest{FileName: "k.json", RawJSON: zenOnly})
	if resp.Auth.ID != "k.json#"+keyIDFor(testKeyA)+"#zen" || resp.Auth.Attributes["family"] != "zen" || string(resp.Auth.StorageJSON) != string(zenOnly) {
		t.Fatalf("zen only = %+v", resp)
	}
	for name, raw := range map[string]string{
		"key":   `{"type":"opencode-go","keys":[{"api_key":"` + testKeyA + `","go":false}]}`,
		"login": string(loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, map[string]any{"go": false})),
	} {
		resp := parseAuths(pluginapi.AuthParseRequest{FileName: name + ".json", RawJSON: []byte(raw)})
		if !resp.Handled || len(resp.Auths) != 0 || resp.Auth.ID != name+".json" || !resp.Auth.Disabled ||
			!strings.HasSuffix(resp.Auth.Label, "(OpenCode Go and Zen off)") || string(resp.Auth.StorageJSON) != raw {
			t.Fatalf("%s placeholder = %+v", name, resp)
		}
	}
}

func TestKeyFamiliesRoundTrip(t *testing.T) {
	file, errParse := parseKeyFile([]byte(`{"type":"opencode-go","api_key":"` + testKeyA + `","go":false,"zen":"true"}`))
	if errParse != nil || len(file.Keys) != 1 || file.Keys[0].families() != (familySet{Zen: true}) {
		t.Fatalf("file = %+v, %v", file, errParse)
	}
	file.Keys[0].setFamilies(familySet{Go: true})
	raw, _ := file.marshal()
	if strings.Contains(string(raw), `"go"`) || strings.Contains(string(raw), `"zen"`) {
		t.Fatalf("defaults written: %s", raw)
	}
	file.Keys[0].setFamilies(familySet{Zen: true})
	raw, _ = file.marshal()
	again, _ := parseKeyFile(raw)
	if !strings.Contains(string(raw), `"go": false`) || !strings.Contains(string(raw), `"zen": true`) || again.Keys[0].families() != (familySet{Zen: true}) {
		t.Fatalf("marshal = %s", raw)
	}
}

// Each family's auths take the file's routing fields of that family, and
// only the Zen auths carry the Zen Bare ID aliases.
func TestParseAuthsFamilyRoutingAndAliases(t *testing.T) {
	cfg := defaultSettings()
	cfg.ZenBareModels = []string{"kimi-k3", "gpt-6-luna"}
	useSettings(t, cfg)
	extra := map[string]any{"go_priority": 2, "go_weight": "5", "zen_priority": -2, "zen_weight": 2000000}
	extra["zen"] = true
	key := mustJSON(t, map[string]any{"type": "opencode-go", "api_key": testKeyA, "zen": true, "go_priority": 2, "go_weight": "5", "zen_priority": -2, "zen_weight": 2000000})
	login := loginFileJSON(t, testAccessToken, testRefreshToken, testLoginExpiry, extra)
	for name, raw := range map[string][]byte{"key": key, "login": login} {
		auths := parseAuths(pluginapi.AuthParseRequest{FileName: name + ".json", RawJSON: raw}).Auths
		if len(auths) != 2 {
			t.Fatalf("%s auths = %+v", name, auths)
		}
		goAuth, zenAuth := auths[0], auths[1]
		if goAuth.Attributes["priority"] != "2" || goAuth.Attributes["weight"] != "5" || goAuth.Attributes["model_aliases"] != "" {
			t.Fatalf("%s go attributes = %v", name, goAuth.Attributes)
		}
		// CPA refuses every auth of a file with an invalid weight, so an out
		// of range one is left to CPA's default.
		if _, ok := zenAuth.Attributes["weight"]; ok || zenAuth.Attributes["priority"] != "-2" {
			t.Fatalf("%s zen attributes = %v", name, zenAuth.Attributes)
		}
		want := `[{"name":"opencode/kimi-k3","alias":"kimi-k3","fork":true},{"name":"opencode/gpt-6-luna","alias":"gpt-6-luna","fork":true}]`
		if zenAuth.Attributes["model_aliases"] != want {
			t.Fatalf("%s zen aliases = %s", name, zenAuth.Attributes["model_aliases"])
		}
		if authFamily(goAuth.Attributes) != familyGo || authFamily(zenAuth.Attributes) != familyZen || authFamily(nil) != familyGo {
			t.Fatal("family attribute")
		}
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
	zenAuths := parseAuths(pluginapi.AuthParseRequest{FileName: "opencode-go.json", RawJSON: []byte(`{"type":"opencode-go","keys":[{"api_key":"` + testKeyA + `"},{"api_key":"` + testKeyB + `","go":false,"zen":true}]}`)}).Auths
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
		{name: "zen auth", storage: hostStorageJSON(t, zenAuths[1].StorageJSON, nil), attributes: zenAuths[1].Attributes, wantKey: testKeyB},
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
