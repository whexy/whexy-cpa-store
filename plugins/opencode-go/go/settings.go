package main

import (
	"net/url"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/whexy/whexy-cpa-store/pluginkit/config"
)

// settings are the plugin-owned values under plugins.configs.opencode-go.
type settings struct {
	BaseURL string
	// ConsoleURL is empty unless configured; consoleURL derives it.
	ConsoleURL string
	// ModelPrefix is the configured Go prefix; modelPrefix says whether IDs
	// carry it.
	ModelPrefix string
	// DisablePrefix serves the models of every family under their bare ID
	// only.
	DisablePrefix bool
	// DisabledModels are Go model IDs the plugin keeps out of CPA. The plugin
	// owns this list because CPA's oauth-excluded-models matches registered
	// IDs, whose form depends on DisablePrefix.
	DisabledModels []string
	// ZenModelPrefix is the prefix of OpenCode Zen (pay-as-you-go) model IDs.
	ZenModelPrefix string
	// ZenModels are the bare OpenCode Zen model IDs the plugin serves. The
	// page edits this list; a Zen model stays out of CPA until it is named
	// here, since those calls bill Console credits.
	ZenModels []string
	// FreeDisabledModels are OpenCode Free model IDs kept out of CPA.
	FreeDisabledModels []string
	// CloakFreeModels presents free OpenCode Zen models as the OpenCode
	// client, which is the only caller OpenCode's free tier answers.
	CloakFreeModels bool
	UserAgent       string
}

type rawSettings struct {
	BaseURL        string   `yaml:"base_url"`
	ConsoleURL     string   `yaml:"console_url"`
	ModelPrefix    string   `yaml:"model_prefix"`
	DisablePrefix  bool     `yaml:"disable_prefix"`
	DisabledModels []string `yaml:"disabled_models"`
	ZenModelPrefix string   `yaml:"zen_model_prefix"`
	ZenModels      []string `yaml:"zen_models"`
	// FreeDisabledModels is the OpenCode Free list.
	FreeDisabledModels []string `yaml:"free_disabled_models"`
	CloakFreeModels    *bool    `yaml:"cloak_free_models"`
	UserAgent          string   `yaml:"user_agent"`
}

var currentConfig atomic.Pointer[settings]

func currentSettings() settings {
	if cfg := currentConfig.Load(); cfg != nil {
		return *cfg
	}
	return defaultSettings()
}

func defaultSettings() settings {
	return settings{
		BaseURL:         defaultBaseURL,
		ModelPrefix:     defaultModelPrefix,
		ZenModelPrefix:  defaultZenModelPrefix,
		CloakFreeModels: true,
	}
}

// modelPrefix is the prefix one family's exposed model IDs carry, empty for
// every family while disable_prefix is on.
func (s settings) modelPrefix(family modelFamily) string {
	if s.DisablePrefix {
		return ""
	}
	switch family {
	case familyZen:
		return s.zenPrefix()
	case familyFree:
		return freeModelPrefix
	}
	return s.ModelPrefix
}

// zenPrefix is the configured prefix of OpenCode Zen model IDs, or the
// default.
func (s settings) zenPrefix() string {
	if s.ZenModelPrefix == "" {
		return defaultZenModelPrefix
	}
	return s.ZenModelPrefix
}

// zenBaseURL is the OpenCode Zen gateway root. It is the base URL without the
// Go path, so a base_url pointing at a mirror moves both.
func (s settings) zenBaseURL() string {
	if root, ok := strings.CutSuffix(strings.TrimRight(s.BaseURL, "/"), "/go"); ok && root != "" {
		return root
	}
	return defaultZenBaseURL
}

// gatewayBase is the canonical root the catalog's base URLs are rebased from,
// and the configured base they are rebased onto. Free models are served by
// the pay-as-you-go gateway.
func (s settings) gatewayBase(family modelFamily) (root, base string) {
	if family != familyGo {
		return defaultZenBaseURL, s.zenBaseURL()
	}
	return defaultBaseURL, s.BaseURL
}

func (s settings) modelDisabled(upstreamID string) bool {
	return containsFold(s.DisabledModels, upstreamID)
}

// zenEnabled reports whether the page has turned on the serving of one
// OpenCode Zen model.
func (s settings) zenEnabled(upstreamID string) bool {
	return containsFold(s.ZenModels, upstreamID)
}

func (s settings) freeDisabled(upstreamID string) bool {
	return containsFold(s.FreeDisabledModels, upstreamID)
}

func containsFold(ids []string, id string) bool {
	for _, other := range ids {
		if strings.EqualFold(other, id) {
			return true
		}
	}
	return false
}

// withDisabled returns a copy of s that also disables ids.
func (s settings) withDisabled(ids []string) settings {
	if len(ids) > 0 {
		s.DisabledModels = append(slices.Clip(s.DisabledModels), ids...)
	}
	return s
}

// userAgent is the User-Agent for requests the plugin originates; the
// executor prefers the client's own.
func (s settings) userAgent() string {
	if s.UserAgent != "" {
		return s.UserAgent
	}
	return "whexy-cpa-opencode-go/" + pluginVersion
}

// consoleURL is the OpenCode Console base. By default it is on the
// gateway's host, so a base_url pointing at a mirror moves both.
func (s settings) consoleURL() string {
	if s.ConsoleURL != "" {
		return s.ConsoleURL
	}
	parsed, errParse := url.Parse(s.BaseURL)
	if errParse != nil || parsed.Host == "" {
		return defaultConsoleURL
	}
	return parsed.Scheme + "://" + parsed.Host + "/console"
}

func configure(request []byte) error {
	cfg, errParse := parseSettings(request)
	if errParse != nil {
		return errParse
	}
	currentConfig.Store(&cfg)
	return nil
}

// parseSettings reads plugins.configs.opencode-go from a lifecycle request.
func parseSettings(request []byte) (settings, error) {
	var raw rawSettings
	if errDecode := config.Decode(request, &raw); errDecode != nil {
		return settings{}, errDecode
	}
	cfg := defaultSettings()
	base, errBase := config.URL("base_url", raw.BaseURL)
	if errBase != nil {
		return settings{}, errBase
	}
	if base != "" {
		cfg.BaseURL = base
	}
	console, errConsole := config.URL("console_url", raw.ConsoleURL)
	if errConsole != nil {
		return settings{}, errConsole
	}
	cfg.ConsoleURL = console
	if prefix, errPrefix := config.ModelPrefix("model_prefix", raw.ModelPrefix); errPrefix != nil {
		return settings{}, errPrefix
	} else if prefix != "" {
		cfg.ModelPrefix = prefix
	}
	if prefix, errPrefix := config.ModelPrefix("zen_model_prefix", raw.ZenModelPrefix); errPrefix != nil {
		return settings{}, errPrefix
	} else if prefix != "" {
		cfg.ZenModelPrefix = prefix
	}
	cfg.DisablePrefix = raw.DisablePrefix
	cfg.DisabledModels = modelIDList(raw.DisabledModels)
	cfg.ZenModels = modelIDList(raw.ZenModels)
	cfg.FreeDisabledModels = modelIDList(raw.FreeDisabledModels)
	if raw.CloakFreeModels != nil {
		cfg.CloakFreeModels = *raw.CloakFreeModels
	}
	cfg.UserAgent = strings.TrimSpace(raw.UserAgent)
	return cfg, nil
}

// modelIDList trims ids and drops blank ones and repeats, which match
// case-insensitively like every lookup in these lists.
func modelIDList(ids []string) []string {
	var out []string
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" && !containsFold(out, id) {
			out = append(out, id)
		}
	}
	return out
}
