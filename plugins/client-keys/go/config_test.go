package main

import (
	"strings"
	"testing"
)

func keyConfig(scope, entry string) string {
	return "enabled: true\npriority: 0\nkeys:\n  " + scope + ":\n" + entry
}

func patternList(count int) string {
	items := make([]string, count)
	for index := range items {
		items[index] = "m" + strings.Repeat("x", index%5)
	}
	return "[" + strings.Join(items, ", ") + "]"
}

func TestCompileRules(t *testing.T) {
	scope := cursorScope
	models := func(settings string) string {
		return keyConfig(scope, "    policies:\n      models: "+settings+"\n")
	}
	responses := func(settings string) string {
		return keyConfig(scope, "    policies:\n      responses-api: "+settings+"\n")
	}
	tests := []struct {
		name string
		yaml string
		// globalErr is a substring of the global error; empty means none.
		globalErr string
		// keyErr is a substring of scope's error; "-" means scope is absent.
		keyErr   string
		policies string
	}{
		{name: "empty config", yaml: "", keyErr: "-"},
		{name: "host fields only", yaml: "enabled: true\npriority: 0\nstore: {version: 0.1.0}\n", keyErr: "-"},
		{name: "null keys", yaml: "keys: null\n", keyErr: "-"},
		{name: "empty keys", yaml: "keys: {}\n", keyErr: "-"},
		{name: "keys is a list", yaml: "keys: [a, b]\n", globalErr: "keys must be a mapping", keyErr: "-"},
		{name: "keys is a string", yaml: "keys: all\n", globalErr: "keys must be a mapping", keyErr: "-"},
		{name: "keys set twice", yaml: "keys: {}\nkeys: {}\n", globalErr: "keys", keyErr: "-"},
		{name: "keys with a list name", yaml: "keys:\n  ? [a]\n  : {}\n", globalErr: "keys must be a mapping", keyErr: "-"},
		{name: "config is a list", yaml: "- keys\n", globalErr: "not a mapping", keyErr: "-"},
		{name: "undecodable yaml", yaml: "keys: [\n", globalErr: "decode plugin config", keyErr: "-"},
		{name: "entry without value", yaml: keyConfig(scope, ""), policies: ""},
		{name: "name and note only", yaml: keyConfig(scope, "    name: Cursor\n    note: work\n"), policies: ""},
		{name: "null policies", yaml: keyConfig(scope, "    policies: null\n"), policies: ""},
		{name: "both policies", yaml: keyConfig(scope, "    policies:\n      models: {allow: [gpt-5]}\n      responses-api: {enabled: true}\n"), policies: "models,responses-api"},
		{name: "entry is a scalar", yaml: keyConfig(scope, "    5\n"), keyErr: "the entry must be a mapping"},
		{name: "misspelt policies", yaml: keyConfig(scope, "    polices:\n      models: {allow: [gpt-5]}\n"), keyErr: `unknown field "polices"`},
		{name: "policies is a list", yaml: keyConfig(scope, "    policies: [models]\n"), keyErr: "policies must be a mapping"},
		{name: "unknown policy type", yaml: keyConfig(scope, "    policies:\n      budget: {}\n"), keyErr: `unknown policy type "budget"`},
		{name: "allow", yaml: models("{allow: [gpt-5]}"), policies: "models"},
		{name: "deny", yaml: models("{deny: [gpt-5]}"), policies: "models"},
		{name: "allow and deny", yaml: models("{allow: [a], deny: [b]}"), keyErr: "exactly one of allow or deny"},
		{name: "neither allow nor deny", yaml: models("{}"), keyErr: "exactly one of allow or deny"},
		{name: "null models", yaml: models("null"), keyErr: "exactly one of allow or deny"},
		{name: "models is a list", yaml: models("[gpt-5]"), keyErr: "policies.models: must be a mapping"},
		{name: "unknown models field", yaml: models("{allow: [a], except: [b]}"), keyErr: `unknown field "except"`},
		{name: "empty allow list", yaml: models("{allow: []}"), keyErr: "allow must be a non-empty list"},
		{name: "null allow", yaml: models("{allow: null}"), keyErr: "allow must be a non-empty list"},
		{name: "allow is a string", yaml: models("{allow: gpt-5}"), keyErr: "allow must be a non-empty list"},
		{name: "number pattern", yaml: models("{deny: [gpt-5, 4]}"), keyErr: "deny[1] must be a string"},
		{name: "nested list pattern", yaml: models("{deny: [[a]]}"), keyErr: "deny[0] must be a string"},
		{name: "blank pattern", yaml: models(`{allow: ["  "]}`), keyErr: "allow[0] is empty"},
		{name: "control character", yaml: models(`{allow: ["gpt\t5"]}`), keyErr: "allow[0] contains a control character"},
		{name: "200 characters", yaml: models("{allow: [" + strings.Repeat("é", 200) + "]}"), policies: "models"},
		{name: "201 characters", yaml: models("{allow: [" + strings.Repeat("é", 201) + "]}"), keyErr: "longer than 200 characters"},
		{name: "500 patterns", yaml: models("{allow: " + patternList(500) + "}"), policies: "models"},
		{name: "501 patterns", yaml: models("{allow: " + patternList(501) + "}"), keyErr: "the limit is 500"},
		{name: "responses-api enabled", yaml: responses("{enabled: true}"), policies: "responses-api"},
		{name: "responses-api without enabled", yaml: responses("{}"), keyErr: "enabled is required"},
		{name: "null responses-api", yaml: responses("null"), keyErr: "enabled is required"},
		{name: "responses-api quoted bool", yaml: responses(`{enabled: "false"}`), keyErr: "enabled must be true or false"},
		{name: "responses-api YAML 1.1 bool", yaml: responses("{enabled: no}"), keyErr: "enabled must be true or false"},
		{name: "responses-api unknown field", yaml: responses("{enabled: false, mode: x}"), keyErr: `unknown field "mode"`},
		{name: "duplicate policy type", yaml: keyConfig(scope, "    policies:\n      models: {allow: [a]}\n      models: {allow: [b]}\n"), keyErr: "set twice"},
		{name: "duplicate key", yaml: keyConfig(scope, "    name: a\n  "+scope+":\n    name: b\n"), keyErr: "listed more than once"},
		{name: "shared settings through an alias", yaml: "shared: &gpt {allow: [gpt-5]}\nkeys:\n  " + scope + ":\n    policies:\n      models: *gpt\n", policies: "models"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules := compileLifecycle(mustJSON(t, lifecycleRequest{ConfigYAML: []byte(tt.yaml)}))
			if tt.globalErr == "" && rules.err != "" || !strings.Contains(rules.err, tt.globalErr) {
				t.Fatalf("global error = %q, want %q", rules.err, tt.globalErr)
			}
			if tt.globalErr != "" && !rules.filtersModelLists {
				t.Fatal("a global error must filter model lists")
			}
			key, found := rules.keys[scope]
			status, listed := rules.entries[scope]
			if tt.keyErr == "-" {
				if found || listed {
					t.Fatalf("key compiled: %+v", key)
				}
				return
			}
			if !found || !listed {
				t.Fatalf("key missing: keys %v, entries %v", rules.keys, rules.entries)
			}
			if tt.keyErr == "" && key.err != "" || !strings.Contains(key.err, tt.keyErr) || status.Error != key.err {
				t.Fatalf("key error = %q (status %q), want %q", key.err, status.Error, tt.keyErr)
			}
			if got := strings.Join(status.Policies, ","); got != tt.policies {
				t.Fatalf("policies = %q, want %q", got, tt.policies)
			}
			if key.err != "" && (len(key.policies) != 0 || !rules.filtersModelLists) {
				t.Fatalf("an invalid key must enforce nothing but refusal: %+v", rules)
			}
		})
	}
}

func TestCompileRulesReportsUnmatchableNames(t *testing.T) {
	notHex := strings.Repeat("g", 64)
	rules := compileRules([]byte("keys:\n  cursor:\n    policies:\n      models: {allow: [a]}\n  " + notHex + ":\n    name: x\n  " + n8nScope + ":\n    policies:\n      budget: {}\n  " + openScope + ": {}\n"))
	if rules.err != "" {
		t.Fatal(rules.err)
	}
	for _, name := range []string{"cursor", notHex} {
		if _, found := rules.keys[name]; found {
			t.Fatalf("%s is enforced", name)
		}
		if !strings.Contains(rules.entries[name].Error, "not a caller scope") {
			t.Fatalf("%s status = %+v", name, rules.entries[name])
		}
	}
	if rules.keys[n8nScope].err == "" || rules.keys[openScope].err != "" {
		t.Fatalf("an invalid entry affected another: %+v", rules.keys)
	}
	if len(rules.entries) != 4 {
		t.Fatalf("entries = %v", rules.entries)
	}
}

func TestCompileRulesLowercasesScopes(t *testing.T) {
	upper := strings.ToUpper(cursorScope)
	mixed := strings.ToUpper(n8nScope[:32]) + n8nScope[32:]
	rules := compileRules([]byte("keys:\n  " + upper + ":\n    policies:\n      models: {allow: [gpt-5*]}\n  " + mixed + ":\n    policies:\n      models: {deny: [o3]}\n  " + n8nScope + ":\n    name: same key in lowercase\n"))
	if rules.err != "" {
		t.Fatal(rules.err)
	}
	if key := rules.keys[cursorScope]; key.err != "" || len(key.policies) != 1 {
		t.Fatalf("uppercase scope = %+v", key)
	}
	if !strings.Contains(rules.keys[n8nScope].err, "listed more than once") || !strings.Contains(rules.entries[n8nScope].Error, "listed more than once") {
		t.Fatalf("scopes colliding after lowercasing = %+v, %+v", rules.keys[n8nScope], rules.entries[n8nScope])
	}
	for name := range rules.entries {
		if name != strings.ToLower(name) {
			t.Fatalf("status lists %s", name)
		}
	}
	if len(rules.entries) != 2 || len(rules.keys) != 2 {
		t.Fatalf("entries = %v", rules.entries)
	}
	useConfig(t, "keys:\n  "+upper+":\n    policies:\n      models: {allow: [gpt-5*]}\n")
	if got := outcome(t, intercept(t, interceptCase{format: "openai", scope: cursorScope, model: "o3"}.request())); got != modelRefused {
		t.Fatalf("request with an uppercase-configured key = %q", got)
	}
}

func TestCompileLifecycleRejectsUndecodableRequest(t *testing.T) {
	rules := compileLifecycle([]byte(`{"config_yaml": 5}`))
	if !strings.Contains(rules.err, "decode lifecycle request") || !rules.filtersModelLists {
		t.Fatalf("rules = %+v", rules)
	}
}

func TestEveryPolicyTypeImplementsAHook(t *testing.T) {
	settings := map[string]string{
		"models":        "{allow: [a]}",
		"responses-api": "{enabled: false}",
	}
	for _, kind := range policyTypes {
		config, ok := settings[kind.name]
		if !ok {
			t.Fatalf("no test settings for policy type %s", kind.name)
		}
		rules := compileRules([]byte(keyConfig(cursorScope, "    policies:\n      "+kind.name+": "+config+"\n")))
		key := rules.keys[cursorScope]
		if key.err != "" || len(key.policies) != 1 {
			t.Fatalf("%s: %+v", kind.name, key)
		}
		_, checks := key.policies[0].rule.(requestChecker)
		_, checksUpstream := key.policies[0].rule.(upstreamChecker)
		_, lists := key.policies[0].rule.(modelLister)
		if !checks && !checksUpstream && !lists {
			t.Fatalf("%s implements no hook", kind.name)
		}
	}
}
