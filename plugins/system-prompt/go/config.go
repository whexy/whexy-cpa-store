package main

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// ruleSet is one compiled configuration; it is never modified once stored.
type ruleSet struct {
	generation uint64
	appliedAt  time.Time
	// err is set when the configuration could not be read at all; no rule
	// applies then.
	err string
	// rules holds the valid rules in configured order.
	rules []rule
	// problems describes each configured rule that does not apply.
	problems []ruleProblem
}

type rule struct {
	patterns []string
	prompt   string
}

type ruleProblem struct {
	Rule  int    `json:"rule"`
	Error string `json:"error"`
}

type configDocument struct {
	Rules []ruleConfig `yaml:"rules"`
}

type ruleConfig struct {
	Models []string `yaml:"models"`
	Prompt string   `yaml:"prompt"`
}

var (
	currentRuleSet atomic.Pointer[ruleSet]
	generations    atomic.Uint64
)

func currentRules() *ruleSet {
	if rules := currentRuleSet.Load(); rules != nil {
		return rules
	}
	return &ruleSet{}
}

// configure applies a plugin.register or plugin.reconfigure request. It never
// fails, because the host disables a plugin whose registration fails; a
// configuration it cannot read applies no rules and reports why.
func configure(raw []byte) {
	rules := compileLifecycle(raw)
	rules.generation = generations.Add(1)
	rules.appliedAt = time.Now().UTC()
	currentRuleSet.Store(rules)
}

func compileLifecycle(raw []byte) *ruleSet {
	var request lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
			return &ruleSet{err: "decode lifecycle request: " + errUnmarshal.Error()}
		}
	}
	return compileRules(request.ConfigYAML)
}

// compileRules reads plugins.configs.system-prompt. Other top-level fields
// belong to the host (enabled, priority, store).
func compileRules(configYAML []byte) *ruleSet {
	var doc configDocument
	if errUnmarshal := yaml.Unmarshal(configYAML, &doc); errUnmarshal != nil {
		return &ruleSet{err: "decode plugin config: " + errUnmarshal.Error()}
	}
	rules := &ruleSet{}
	for index, config := range doc.Rules {
		compiled, problem := compileRule(config)
		if problem != "" {
			rules.problems = append(rules.problems, ruleProblem{Rule: index + 1, Error: problem})
			continue
		}
		rules.rules = append(rules.rules, compiled)
	}
	return rules
}

func compileRule(config ruleConfig) (rule, string) {
	var compiled rule
	seen := map[string]bool{}
	for _, model := range config.Models {
		pattern := strings.ToLower(strings.TrimSpace(model))
		if pattern == "" || seen[pattern] {
			continue
		}
		seen[pattern] = true
		compiled.patterns = append(compiled.patterns, pattern)
	}
	compiled.prompt = strings.TrimSpace(config.Prompt)
	switch {
	case len(compiled.patterns) == 0:
		return rule{}, "lists no models"
	case compiled.prompt == "":
		return rule{}, "has an empty prompt"
	}
	return compiled, ""
}

// promptFor joins the prompts of every rule that matches one of the model
// IDs, in configured order.
func (s *ruleSet) promptFor(models ...string) string {
	var candidates []string
	for _, model := range models {
		candidates = appendModelCandidates(candidates, model)
	}
	var prompts []string
	for _, rule := range s.rules {
		if rule.matches(candidates) {
			prompts = append(prompts, rule.prompt)
		}
	}
	return strings.Join(prompts, promptSeparator)
}

// appendModelCandidates adds a model ID and, when it carries a thinking
// suffix such as claude-opus-5-5(high), the ID without it, so a rule for the
// model also covers its suffixed variants.
func appendModelCandidates(candidates []string, model string) []string {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return candidates
	}
	candidates = append(candidates, model)
	if open := strings.LastIndex(model, "("); open > 0 && strings.HasSuffix(model, ")") {
		candidates = append(candidates, model[:open])
	}
	return candidates
}

func (r rule) matches(models []string) bool {
	for _, pattern := range r.patterns {
		for _, model := range models {
			if wildcardMatch(pattern, model) {
				return true
			}
		}
	}
	return false
}

// wildcardMatch reports whether value matches pattern, where * stands for any
// run of characters, including slashes. Every other character is literal.
func wildcardMatch(pattern, value string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == value
	}
	if !strings.HasPrefix(value, parts[0]) {
		return false
	}
	value = value[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, part := range parts[1 : len(parts)-1] {
		index := strings.Index(value, part)
		if index < 0 {
			return false
		}
		value = value[index+len(part):]
	}
	return len(value) >= len(last) && strings.HasSuffix(value, last)
}
