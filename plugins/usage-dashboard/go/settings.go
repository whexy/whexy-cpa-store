package main

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/whexy/whexy-cpa-store/pluginkit/config"
	"github.com/whexy/whexy-cpa-store/pluginkit/datadir"
)

const (
	defaultRetentionDays = 30
	maxRetentionDays     = 400
	defaultPricingURL    = "https://models.dev/api.json"
)

// settings are the plugin-owned values under plugins.configs.usage-dashboard.
type settings struct {
	// DataDir is absolute; empty until configure resolves the default.
	DataDir       string
	RetentionDays int
	PricingURL    string
	// QuotaPollInterval is how often the quota of providers that send none
	// in response headers is read; zero turns polling off.
	QuotaPollInterval time.Duration
	// FamilyRules are the configured model_families overrides, matched in
	// pattern order before the built-in rules.
	FamilyRules []familyRule
}

type rawSettings struct {
	DataDir           string            `yaml:"data_dir"`
	RetentionDays     int               `yaml:"retention_days"`
	Pricing           *bool             `yaml:"pricing"`
	PricingURL        string            `yaml:"pricing_url"`
	QuotaPollInterval string            `yaml:"quota_poll_interval"`
	ModelFamilies     map[string]string `yaml:"model_families"`
}

var currentConfig atomic.Pointer[settings]

func defaultSettings() settings {
	return settings{RetentionDays: defaultRetentionDays, PricingURL: defaultPricingURL, QuotaPollInterval: defaultQuotaPollInterval}
}

func currentSettings() settings {
	if cfg := currentConfig.Load(); cfg != nil {
		return *cfg
	}
	return defaultSettings()
}

// configure applies a plugin.register or plugin.reconfigure request.
func configure(request []byte) error {
	cfg, errParse := parseSettings(request)
	if errParse != nil {
		return errParse
	}
	if cfg.DataDir == "" {
		cfg.DataDir = datadir.Default(pluginID)
	}
	currentConfig.Store(&cfg)
	startRuntime(cfg)
	return nil
}

// parseSettings reads the plugin's node of config.yaml from a lifecycle
// request.
func parseSettings(request []byte) (settings, error) {
	var raw rawSettings
	if errDecode := config.Decode(request, &raw); errDecode != nil {
		return settings{}, errDecode
	}
	cfg := defaultSettings()
	if dir := strings.TrimSpace(raw.DataDir); dir != "" {
		abs, errAbs := filepath.Abs(dir)
		if errAbs != nil {
			return settings{}, fmt.Errorf("resolve data_dir %q: %w", dir, errAbs)
		}
		cfg.DataDir = abs
	}
	if raw.RetentionDays != 0 {
		if raw.RetentionDays < 1 || raw.RetentionDays > maxRetentionDays {
			return settings{}, fmt.Errorf("retention_days must be between 1 and %d, got %d", maxRetentionDays, raw.RetentionDays)
		}
		cfg.RetentionDays = raw.RetentionDays
	}
	if raw.Pricing != nil && !*raw.Pricing {
		cfg.PricingURL = ""
	} else {
		pricingURL, errURL := config.URL("pricing_url", raw.PricingURL)
		if errURL != nil {
			return settings{}, errURL
		}
		if pricingURL != "" {
			cfg.PricingURL = pricingURL
		}
	}
	switch value := strings.ToLower(strings.TrimSpace(raw.QuotaPollInterval)); value {
	case "":
	case "off":
		cfg.QuotaPollInterval = 0
	default:
		interval, errParse := time.ParseDuration(value)
		if errParse != nil || interval < minQuotaPollInterval {
			return settings{}, fmt.Errorf("quota_poll_interval must be off or a duration of at least %s, got %q", minQuotaPollInterval, raw.QuotaPollInterval)
		}
		cfg.QuotaPollInterval = interval
	}
	rules, errRules := parseFamilyRules(raw.ModelFamilies)
	if errRules != nil {
		return settings{}, errRules
	}
	cfg.FamilyRules = rules
	return cfg, nil
}

func parseFamilyRules(values map[string]string) ([]familyRule, error) {
	rules := make([]familyRule, 0, len(values))
	for pattern, family := range values {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		family = strings.TrimSpace(family)
		if pattern == "" || family == "" {
			continue
		}
		if _, errMatch := path.Match(pattern, ""); errMatch != nil {
			return nil, fmt.Errorf("model_families pattern %q: %w", pattern, errMatch)
		}
		rules = append(rules, familyRule{Pattern: pattern, Family: family})
	}
	sortFamilyRules(rules)
	return rules, nil
}
