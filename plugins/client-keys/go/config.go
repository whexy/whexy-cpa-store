package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// ruleSet is one compiled configuration; it is never modified once stored.
type ruleSet struct {
	generation uint64
	appliedAt  time.Time
	// err is set when the configuration could not be read at all. Every
	// model request that carries a caller scope is then refused, because
	// which keys are restricted is unknown.
	err string
	// keys holds the entries whose name is a caller scope.
	keys map[string]keyRules
	// entries describes every configured entry for the status route,
	// including names no request can match.
	entries map[string]entryStatus
	// filtersModelLists reports whether any caller's model lists change,
	// which is when the response interceptor is needed.
	filtersModelLists bool
}

// keyRules are the policies of one client key.
type keyRules struct {
	// err marks an entry that could not be read. Its intended limits are
	// unknown, so every model request with the key is refused.
	err      string
	policies []compiledPolicy
}

type compiledPolicy struct {
	name string
	rule any
}

type entryStatus struct {
	Policies []string `json:"policies,omitempty"`
	Error    string   `json:"error,omitempty"`
}

var (
	currentRuleSet atomic.Pointer[ruleSet]
	generations    atomic.Uint64
)

// callerScopePattern is the shape of session.CallerScope output, in either
// case.
var callerScopePattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

func currentRules() *ruleSet {
	if rules := currentRuleSet.Load(); rules != nil {
		return rules
	}
	return &ruleSet{keys: map[string]keyRules{}, entries: map[string]entryStatus{}}
}

// configure applies a plugin.register or plugin.reconfigure request. It never
// fails: CLIProxyAPI skips an interceptor that returns an error and disables
// a plugin that panics, so either would silently stop enforcement. Problems
// in the configuration become refusals instead.
func configure(raw []byte) {
	rules := compileLifecycle(raw)
	rules.generation = generations.Add(1)
	rules.appliedAt = time.Now().UTC()
	currentRuleSet.Store(rules)
}

func compileLifecycle(raw []byte) (rules *ruleSet) {
	defer func() {
		if recovered := recover(); recovered != nil {
			rules = failedRules(fmt.Sprintf("reading the plugin config panicked: %v", recovered))
		}
	}()
	var request lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
			return failedRules("decode lifecycle request: " + errUnmarshal.Error())
		}
	}
	return compileRules(request.ConfigYAML)
}

func failedRules(message string) *ruleSet {
	return &ruleSet{err: message, keys: map[string]keyRules{}, entries: map[string]entryStatus{}, filtersModelLists: true}
}

func compileRules(configYAML []byte) *ruleSet {
	rules := &ruleSet{keys: map[string]keyRules{}, entries: map[string]entryStatus{}}
	var doc yaml.Node
	if errUnmarshal := yaml.Unmarshal(configYAML, &doc); errUnmarshal != nil {
		return failedRules("decode plugin config: " + errUnmarshal.Error())
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = resolved(root.Content[0])
	}
	if root.Kind == 0 || isNull(root) {
		return rules
	}
	if root.Kind != yaml.MappingNode {
		return failedRules("the plugin config is not a mapping")
	}
	// Other top-level fields belong to the host (enabled, priority, store).
	var keysNode *yaml.Node
	for index := 0; index+1 < len(root.Content); index += 2 {
		if name := resolved(root.Content[index]); name.Kind == yaml.ScalarNode && name.Value == "keys" {
			if keysNode != nil {
				return failedRules("keys is set twice")
			}
			keysNode = root.Content[index+1]
		}
	}
	if isNull(keysNode) {
		return rules
	}
	keysNode = resolved(keysNode)
	if keysNode.Kind != yaml.MappingNode {
		return failedRules("keys must be a mapping from caller scope to key entry")
	}
	for index := 0; index+1 < len(keysNode.Content); index += 2 {
		node := resolved(keysNode.Content[index])
		if node.Kind != yaml.ScalarNode {
			return failedRules("keys must be a mapping from caller scope to key entry")
		}
		// Hash tools differ in hex case (PowerShell's Get-FileHash prints
		// uppercase); the host compares lowercase.
		name, isScope := node.Value, callerScopePattern.MatchString(node.Value)
		if isScope {
			name = strings.ToLower(name)
		}
		if _, duplicate := rules.entries[name]; duplicate {
			const message = "the key is listed more than once"
			rules.entries[name] = entryStatus{Error: message}
			if isScope {
				rules.keys[name] = keyRules{err: message}
			}
			continue
		}
		if !isScope {
			rules.entries[name] = entryStatus{Error: "the name is not a caller scope (64 hex characters), so no request matches it"}
			continue
		}
		key := compileKey(keysNode.Content[index+1])
		rules.keys[name] = key
		rules.entries[name] = key.status()
	}
	for _, key := range rules.keys {
		if key.filtersModelLists() {
			rules.filtersModelLists = true
		}
	}
	return rules
}

func compileKey(node *yaml.Node) keyRules {
	entry, errFields := fields(node, "name", "note", "policies")
	if errFields != nil {
		return keyRules{err: "the entry " + errFields.Error()}
	}
	settings := map[string]*yaml.Node{}
	if policies := resolved(entry["policies"]); !isNull(policies) {
		if policies.Kind != yaml.MappingNode {
			return keyRules{err: "policies must be a mapping from policy type to settings"}
		}
		for index := 0; index+1 < len(policies.Content); index += 2 {
			name := resolved(policies.Content[index])
			if name.Kind != yaml.ScalarNode {
				return keyRules{err: "policies must be a mapping from policy type to settings"}
			}
			if !slices.ContainsFunc(policyTypes, func(kind policyType) bool { return kind.name == name.Value }) {
				return keyRules{err: fmt.Sprintf("policies: unknown policy type %q", name.Value)}
			}
			if _, duplicate := settings[name.Value]; duplicate {
				return keyRules{err: fmt.Sprintf("policies: %s is set twice", name.Value)}
			}
			settings[name.Value] = policies.Content[index+1]
		}
	}
	var compiled []compiledPolicy
	for _, kind := range policyTypes {
		node, configured := settings[kind.name]
		if !configured {
			continue
		}
		rule, errParse := kind.parse(node)
		if errParse != nil {
			return keyRules{err: "policies." + kind.name + ": " + errParse.Error()}
		}
		compiled = append(compiled, compiledPolicy{name: kind.name, rule: rule})
	}
	return keyRules{policies: compiled}
}

func (k keyRules) status() entryStatus {
	if k.err != "" {
		return entryStatus{Error: k.err}
	}
	names := make([]string, 0, len(k.policies))
	for _, policy := range k.policies {
		names = append(names, policy.name)
	}
	slices.Sort(names)
	return entryStatus{Policies: names}
}

func (k keyRules) filtersModelLists() bool {
	_, restricted := k.modelList()
	return restricted
}

// resolved follows YAML aliases; it never returns nil for a non-nil node.
func resolved(node *yaml.Node) *yaml.Node {
	for node != nil && node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	return node
}

func isNull(node *yaml.Node) bool {
	node = resolved(node)
	return node == nil || (node.Kind == yaml.ScalarNode && node.ShortTag() == "!!null")
}

// fields reads a mapping limited to the allowed keys. Unknown keys are
// errors, so a misspelt setting is reported instead of ignored. Null reads
// as an empty mapping.
func fields(node *yaml.Node, allowed ...string) (map[string]*yaml.Node, error) {
	out := map[string]*yaml.Node{}
	node = resolved(node)
	if isNull(node) {
		return out, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, errors.New("must be a mapping")
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		name := resolved(node.Content[index])
		if name.Kind != yaml.ScalarNode {
			return nil, errors.New("has a field name that is not a string")
		}
		if !slices.Contains(allowed, name.Value) {
			return nil, fmt.Errorf("has unknown field %q", name.Value)
		}
		if _, duplicate := out[name.Value]; duplicate {
			return nil, fmt.Errorf("sets %s twice", name.Value)
		}
		out[name.Value] = node.Content[index+1]
	}
	return out, nil
}
