package main

import (
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// familyRule maps a glob over a bare, lower-case model ID to a family.
type familyRule struct {
	Pattern string
	Family  string
}

// sortFamilyRules orders configured rules so that a more specific pattern
// (fewer wildcards, then longer) wins over a broader one, independent of the
// unordered YAML map they came from.
func sortFamilyRules(rules []familyRule) {
	sort.SliceStable(rules, func(i, j int) bool {
		wi, wj := strings.Count(rules[i].Pattern, "*"), strings.Count(rules[j].Pattern, "*")
		if wi != wj {
			return wi < wj
		}
		if len(rules[i].Pattern) != len(rules[j].Pattern) {
			return len(rules[i].Pattern) > len(rules[j].Pattern)
		}
		return rules[i].Pattern < rules[j].Pattern
	})
}

// builtinFamilies is checked in order; each entry lists ID prefixes.
var builtinFamilies = []struct {
	family   string
	prefixes []string
}{
	{"Claude", []string{"claude", "anthropic"}},
	{"GPT", []string{"gpt", "chatgpt", "codex", "openai", "davinci", "babbage"}},
	{"Gemini", []string{"gemini"}},
	{"Gemma", []string{"gemma"}},
	{"Grok", []string{"grok"}},
	{"DeepSeek", []string{"deepseek"}},
	{"Kimi", []string{"kimi", "moonshot"}},
	{"Qwen", []string{"qwen", "qwq", "qvq"}},
	{"GLM", []string{"glm", "chatglm", "codegeex"}},
	{"MiniMax", []string{"minimax", "abab"}},
	{"Mistral", []string{"mistral", "mixtral", "codestral", "devstral", "magistral", "ministral", "pixtral"}},
	{"Llama", []string{"llama", "meta-llama"}},
	{"Doubao", []string{"doubao", "seed-"}},
	{"ERNIE", []string{"ernie"}},
	{"Hunyuan", []string{"hunyuan"}},
	{"Nova", []string{"amazon.nova", "nova-"}},
	{"Command", []string{"command"}},
}

var (
	// oSeries is OpenAI's o-series (o1, o3-mini, o4-mini-high); a bare prefix
	// match on "o3" would also catch unrelated IDs starting with those letters.
	oSeries          = regexp.MustCompile(`^o[1-9](-|$)`)
	thinkingSuffix   = regexp.MustCompile(`\([^)]*\)$`)
	leadingWordChars = regexp.MustCompile(`^[a-z]+`)
)

// bareModelID strips routing prefixes ("opencode-go/kimi-k3",
// "models/gemini-2.5-pro") and CLIProxyAPI's "(high)" thinking suffix.
func bareModelID(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	model = thinkingSuffix.ReplaceAllString(model, "")
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
		model = model[slash+1:]
	}
	return strings.TrimSpace(model)
}

// modelFamily names the family a model belongs to: configured rules first,
// then the built-in vendor list, then the model ID's leading word.
func modelFamily(model string, rules []familyRule) string {
	id := bareModelID(model)
	if id == "" {
		return "Unknown"
	}
	for _, rule := range rules {
		if matched, _ := path.Match(rule.Pattern, id); matched {
			return rule.Family
		}
	}
	if oSeries.MatchString(id) {
		return "GPT"
	}
	for _, entry := range builtinFamilies {
		for _, prefix := range entry.prefixes {
			if strings.HasPrefix(id, prefix) {
				return entry.family
			}
		}
	}
	word := leadingWordChars.FindString(id)
	if word == "" {
		return "Other"
	}
	runes := []rune(word)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}
