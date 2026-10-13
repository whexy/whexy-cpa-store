package main

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultsForAnEmptyConfig(t *testing.T) {
	cfg, errParse := parseSettings(nil)
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if cfg.Prefix != defaultPrefix {
		t.Fatalf("prefix %q, want %q", cfg.Prefix, defaultPrefix)
	}
	if !cfg.AffinityEnabled || cfg.AffinityTTL != defaultAffinityTTL || cfg.RebindOnFailure {
		t.Fatalf("affinity defaults %+v", cfg)
	}
	if len(cfg.Groups) != 0 {
		t.Fatalf("groups %v, want none", cfg.Groups)
	}
}

func TestGroupNameBecomesTheModelIDSuffix(t *testing.T) {
	cfg, errParse := parseSettings([]byte(`
groups:
  - name: Daily driver
    members:
      - model: claude-haiku-5-5
        provider: claude
`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if cfg.Groups[0].ID != "auto-group/daily-driver" {
		t.Fatalf("group ID %q", cfg.Groups[0].ID)
	}
	if cfg.Groups[0].Policy != policyWeightedRR {
		t.Fatalf("default policy %q, want %q", cfg.Groups[0].Policy, policyWeightedRR)
	}
	if got := cfg.Groups[0].Members[0]; got.Weight != 1 || len(got.Providers) != 1 || got.Providers[0] != "claude" {
		t.Fatalf("member %+v", got)
	}
}

func TestPolicyAliases(t *testing.T) {
	cases := map[string]string{
		"":                     policyWeightedRR,
		"weighted":             policyWeightedRR,
		"weighted-round-robin": policyWeightedRR,
		"wrr":                  policyWeightedRR,
		"rr":                   policyRoundRobin,
		"round-robin":          policyRoundRobin,
		"ff":                   policyFillFirst,
		"fill-first":           policyFillFirst,
		"  Round-Robin  ":      policyRoundRobin,
	}
	for input, want := range cases {
		cfg, errParse := parseSettings([]byte(`
groups:
  - slug: g
    policy: "` + input + `"
    members:
      - model: m
        providers: [p]
`))
		if errParse != nil {
			t.Fatalf("policy %q: %v", input, errParse)
		}
		if cfg.Groups[0].Policy != want {
			t.Fatalf("policy %q parsed as %q, want %q", input, cfg.Groups[0].Policy, want)
		}
	}
}

func TestInvalidConfigsAreRejected(t *testing.T) {
	cases := map[string]string{
		"unknown policy": `
groups:
  - slug: g
    policy: random
    members: [{model: m, providers: [p]}]
`,
		"bad slug": `
groups:
  - slug: "No Spaces"
    members: [{model: m, providers: [p]}]
`,
		"no members": `
groups:
  - slug: g
`,
		"weight out of range": `
groups:
  - slug: g
    members: [{model: m, providers: [p], weight: 0}]
`,
		"duplicate slug": `
groups:
  - slug: g
    members: [{model: m, providers: [p]}]
  - slug: g
    members: [{model: n, providers: [p]}]
`,
		"duplicate member": `
groups:
  - slug: g
    members: [{model: m, providers: [p]}, {model: m, providers: [p]}]
`,
		"member of the plugin's own provider": `
groups:
  - slug: g
    members: [{model: m, providers: [model-groups]}]
`,
		"prefix with a space": `
prefix: "auto group"
groups:
  - slug: g
    members: [{model: m, providers: [p]}]
`,
		"affinity ttl as a number": `
affinity:
  ttl: 3600
groups:
  - slug: g
    members: [{model: m, providers: [p]}]
`,
	}
	for name, config := range cases {
		if _, errParse := parseSettings([]byte(config)); errParse == nil {
			t.Fatalf("%s: the config was accepted", name)
		}
	}
}

func TestMemberWithoutProvidersIsKeptButInert(t *testing.T) {
	// The page resolves a member's providers before saving; a hand-written
	// config may name a model only, which stays in the group but cannot be
	// routed until it names a provider.
	cfg, errParse := parseSettings([]byte(`
groups:
  - slug: g
    members: [{model: m}, {model: n, providers: [p]}]
`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if len(cfg.Groups[0].Members) != 2 || len(cfg.Groups[0].Members[0].Providers) != 0 {
		t.Fatalf("members %+v", cfg.Groups[0].Members)
	}
}

func TestSelfReferencingMemberIsRejected(t *testing.T) {
	_, errParse := parseSettings([]byte(`
groups:
  - slug: g
    members: [{model: auto-group/g, providers: [p]}]
`))
	if errParse == nil || !strings.Contains(errParse.Error(), "cannot contain itself") {
		t.Fatalf("error %v, want a self-reference error", errParse)
	}
}

func TestInvalidConfigKeepsThePreviousGroups(t *testing.T) {
	host := newFakeHost()
	host.addProvider("claude", 1, credentialReady)
	state := newTestState(t, multiProviderConfig, host)

	state.applyConfig([]byte("groups:\n  - slug: g\n"))
	if state.cfgErr == "" {
		t.Fatalf("the broken config was not reported")
	}
	if len(state.groups) != 1 {
		t.Fatalf("groups after a broken config: %d, want the previous one", len(state.groups))
	}
	if _, handled := routeFor(state, "auto-group/fast", nil, []string{"claude", "opencode-go"}); !handled {
		t.Fatalf("routing stopped after a broken config")
	}
}

func TestAffinityTTLAndPrefixAreConfigurable(t *testing.T) {
	cfg, errParse := parseSettings([]byte(`
prefix: groups
affinity:
  enabled: false
  ttl: 24h
  rebind_on_failure: true
groups:
  - slug: g
    members: [{model: m, providers: [p]}]
`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if cfg.Prefix != "groups" || cfg.AffinityEnabled || !cfg.RebindOnFailure {
		t.Fatalf("settings %+v", cfg)
	}
	if cfg.AffinityTTL != 24*time.Hour {
		t.Fatalf("ttl %s", cfg.AffinityTTL)
	}
	if cfg.Groups[0].ID != "groups/g" {
		t.Fatalf("group ID %q", cfg.Groups[0].ID)
	}
}

func TestThinkingSuffixSplit(t *testing.T) {
	cases := []struct {
		model  string
		base   string
		suffix string
	}{
		{"auto-group/fast", "auto-group/fast", ""},
		{"auto-group/fast(high)", "auto-group/fast", "(high)"},
		{"auto-group/fast(16384)", "auto-group/fast", "(16384)"},
		{"auto-group/fast(", "auto-group/fast(", ""},
		{"auto-group/fast(high", "auto-group/fast(high", ""},
	}
	for _, entry := range cases {
		base, suffix := splitThinkingSuffix(entry.model)
		if base != entry.base || suffix != entry.suffix {
			t.Fatalf("split %q into %q, %q; want %q, %q", entry.model, base, suffix, entry.base, entry.suffix)
		}
	}
}
