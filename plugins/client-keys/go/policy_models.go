package main

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	maxModelPatterns      = 500
	maxModelPatternLength = 200
)

// modelsPolicy is policies.models: {allow: [patterns]} or {deny: [patterns]}.
// Patterns are lowercase globs where * matches any run of characters.
type modelsPolicy struct {
	deny     bool
	patterns []string
}

func parseModelsPolicy(settings *yaml.Node) (any, error) {
	values, errFields := fields(settings, "allow", "deny")
	if errFields != nil {
		return nil, errFields
	}
	allow, hasAllow := values["allow"]
	deny, hasDeny := values["deny"]
	if hasAllow == hasDeny {
		return nil, errors.New("set exactly one of allow or deny")
	}
	listName, list := "allow", allow
	if hasDeny {
		listName, list = "deny", deny
	}
	patterns, errPatterns := parseModelPatterns(listName, list)
	if errPatterns != nil {
		return nil, errPatterns
	}
	return modelsPolicy{deny: hasDeny, patterns: patterns}, nil
}

func parseModelPatterns(listName string, node *yaml.Node) ([]string, error) {
	node = resolved(node)
	if node == nil || node.Kind != yaml.SequenceNode || len(node.Content) == 0 {
		return nil, fmt.Errorf("%s must be a non-empty list of model patterns", listName)
	}
	if len(node.Content) > maxModelPatterns {
		return nil, fmt.Errorf("%s has %d patterns; the limit is %d", listName, len(node.Content), maxModelPatterns)
	}
	patterns := make([]string, 0, len(node.Content))
	for index, item := range node.Content {
		item = resolved(item)
		if item.Kind != yaml.ScalarNode || item.ShortTag() != "!!str" {
			return nil, fmt.Errorf("%s[%d] must be a string", listName, index)
		}
		pattern := strings.TrimSpace(item.Value)
		switch {
		case pattern == "":
			return nil, fmt.Errorf("%s[%d] is empty", listName, index)
		case utf8.RuneCountInString(pattern) > maxModelPatternLength:
			return nil, fmt.Errorf("%s[%d] is longer than %d characters", listName, index, maxModelPatternLength)
		case strings.ContainsFunc(pattern, unicode.IsControl):
			return nil, fmt.Errorf("%s[%d] contains a control character", listName, index)
		}
		patterns = append(patterns, strings.ToLower(pattern))
	}
	return patterns, nil
}

func (p modelsPolicy) check(req modelRequest) (denial, bool) {
	if p.lists(req.model) {
		return denial{}, false
	}
	return modelNotFound(req), true
}

// checkUpstream applies a deny list to the model an alias, pool or route
// resolved to. Allow lists name what clients may send, so a model reached
// through an allowed alias stays allowed.
func (p modelsPolicy) checkUpstream(req modelRequest) (denial, bool) {
	if !p.deny || req.model == "" || p.lists(req.model) {
		return denial{}, false
	}
	return modelNotFound(req), true
}

// lists reports whether the key may use model. A request without a model
// can only pass a deny list.
func (p modelsPolicy) lists(model string) bool {
	if model == "" {
		return p.deny
	}
	model = strings.ToLower(model)
	for _, pattern := range p.patterns {
		if globMatch(pattern, model) {
			return !p.deny
		}
	}
	return p.deny
}

// globMatch reports whether name matches pattern, where * matches any run
// of bytes and every other byte matches itself.
func globMatch(pattern, name string) bool {
	patternIndex, nameIndex := 0, 0
	starIndex, starName := -1, 0
	for nameIndex < len(name) {
		switch {
		case patternIndex < len(pattern) && pattern[patternIndex] == '*':
			starIndex, starName = patternIndex, nameIndex
			patternIndex++
		case patternIndex < len(pattern) && pattern[patternIndex] == name[nameIndex]:
			patternIndex++
			nameIndex++
		case starIndex >= 0:
			starName++
			patternIndex, nameIndex = starIndex+1, starName
		default:
			return false
		}
	}
	for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(pattern)
}

// normalizeModel reduces a model name to what patterns match: the name
// without the Gemini "models/" path prefix and without a thinking suffix
// such as "(high)". "prefix/" segments stay, because clients see and send
// them. The base is trimmed after the suffix is cut, as CLIProxyAPI does
// when routing, so "gpt-5 (high)" is judged as gpt-5.
func normalizeModel(name string) string {
	name = strings.TrimPrefix(strings.TrimSpace(name), "models/")
	if open := strings.LastIndex(name, "("); open >= 0 && strings.HasSuffix(name, ")") {
		name = name[:open]
	}
	return strings.TrimSpace(name)
}
